package session

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type TypeDomainValidationOptions struct {
	Runtime    DeclaredTypeRuntimeOptions
	BatchRows  int
	ValueBytes uint64
	Call       builtin.CallBudget
}

func normalizeDomainValidationOptions(options TypeDomainValidationOptions, descriptors model.DescriptorLimits) TypeDomainValidationOptions {
	if options.Runtime.Descriptors.MaxBytes == 0 {
		options.Runtime.Descriptors = descriptors
	}
	if options.BatchRows == 0 {
		options.BatchRows = options.Runtime.Expressions.Evaluation.MaxRows
	}
	if options.ValueBytes == 0 {
		options.ValueBytes = options.Runtime.Expressions.InputBytes
	}
	if options.Call.MemoryBytes == 0 {
		options.Call.MemoryBytes = options.Runtime.Expressions.Evaluation.MaxStateBytes
	}
	if options.Call.OutputBytes == 0 && options.Runtime.Expressions.ResultBytes <= uint64(^uint64(0)>>1) {
		options.Call.OutputBytes = int64(options.Runtime.Expressions.ResultBytes)
	}
	return options
}

func domainValidationType(ctx context.Context, constraint model.ObjectID, updates []catalog.TypeUpdate, limits model.DescriptorLimits) (model.TypeID, error) {
	var domain model.TypeID
	for _, update := range updates {
		if update.Next == nil || update.Next.Kind() != model.ObjectType {
			continue
		}
		spec, err := update.Next.SpecWithin(ctx, limits)
		if err != nil {
			return 0, err
		}
		if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain {
			continue
		}
		if slices.ContainsFunc(spec.Type.Constraints, func(candidate model.ConstraintDescriptor) bool { return candidate.ID == constraint }) {
			if domain != 0 {
				return 0, model.ErrInvalidDescriptor
			}
			domain = model.TypeID(update.Next.OID())
		}
	}
	if domain == 0 {
		return 0, model.ErrInvalidDescriptor
	}
	return domain, nil
}

func validateDomainTypeChange(ctx context.Context, store *catalog.Store, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, bound *bind.BoundTypeCommand, updates []catalog.TypeUpdate, options TypeDomainValidationOptions, maximum uint32) error {
	constraint := bound.DomainValidationID()
	if constraint == 0 {
		return nil
	}
	if store == nil || snapshot == nil || reader == nil {
		return settingError("22023", "domain validation services are not configured")
	}
	domain, err := domainValidationType(ctx, constraint, updates, options.Runtime.Descriptors)
	if err != nil {
		return err
	}
	references, err := store.DomainColumnReferences(ctx, snapshot, domain, maximum)
	if err != nil {
		return err
	}
	if len(references) == 0 {
		return nil
	}
	if options.BatchRows <= 0 || options.ValueBytes == 0 || options.Call.MemoryBytes == 0 {
		return settingError("22023", "domain validation services are not configured")
	}
	types, err := NewDeclaredTypeRuntimeWithUpdates(ctx, snapshot, options.Runtime, updates)
	if err != nil {
		return err
	}
	for _, reference := range references {
		if err := validateDomainTable(ctx, snapshot, reader, reference, types, domain, constraint, options); err != nil {
			return err
		}
	}
	return nil
}

func validateDomainTable(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, reference catalog.DomainColumnReference, types builtin.TypeResolver, domain model.TypeID, constraint model.ObjectID, options TypeDomainValidationOptions) (err error) {
	spec, err := reference.Table.SpecWithin(ctx, options.Runtime.Descriptors)
	if err != nil || spec.Table == nil {
		return errors.Join(model.ErrInvalidDescriptor, err)
	}
	layout := make([]storage.ColumnLayout, 0, len(spec.Table.Columns))
	for _, column := range spec.Table.Columns {
		physical := storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
		if column.ConstantDefault != nil {
			physical.HasMissingValue = true
			physical.MissingValue = *column.ConstantDefault
		}
		layout = append(layout, physical)
	}
	columns := make([]model.ColumnID, len(reference.Columns))
	columnLayouts := make([]model.TypeLayout, len(reference.Columns))
	for i := range reference.Columns {
		columns[i] = reference.Columns[i].ID
		columnLayouts[i] = reference.Columns[i].Layout
	}
	physical := reference.Table.Physical()
	target := storage.ObjectRef{ObjectID: reference.Table.ID(), Generation: physical.Generation, DescriptorVersion: reference.Table.Version(), EncodingVersion: physical.EncodingVersion}
	request := storage.PushdownRequest{Target: target, Layout: layout, ColumnIDs: columns, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
	plan, err := reader.PlanScan(ctx, request)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reader.ReleasePlan(plan)) }()
	if plan.SnapshotTS != snapshot.Timestamp() || plan.Request.Target != target || !slices.Equal(plan.Projection, columns) || len(plan.Ordering) != 0 || !plan.Accepted.Empty() || !plan.Residual.Empty() || plan.AppliedLimit.Enabled || plan.Binding == ([32]byte{}) {
		return storage.ErrInvalidScanPlan
	}
	expected, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: request, SnapshotTS: snapshot.Timestamp()}, options.ValueBytes)
	if err != nil {
		return err
	}
	actual, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: plan.Request, SnapshotTS: plan.SnapshotTS}, options.ValueBytes)
	if err != nil {
		return err
	}
	if expected != actual {
		return storage.ErrInvalidScanPlan
	}
	cursor, err := reader.VectorizedScan(ctx, plan)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	batch := columnar.Batch{MaxRows: options.BatchRows, Columns: make([]columnar.Vector, 0, len(columns))}
	for {
		batch.Reset()
		batch.MaxRows = options.BatchRows
		if err := cursor.Next(ctx, &batch); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := batch.Validate(options.BatchRows); err != nil {
			return err
		}
		if batch.Rows() > options.BatchRows || len(batch.Columns) != len(columns) {
			return storage.ErrInvalidScanPlan
		}
		for row := range batch.Rows() {
			for column := range columns {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[column], batch.Lane(row), options.ValueBytes)
				if err != nil {
					return err
				}
				if err := builtin.ValidateDomainOccurrences(ctx, types, domain, constraint, columnLayouts[column], value, options.Call, options.Runtime.Descriptors.MaxDepth); err != nil {
					return err
				}
			}
		}
	}
}

func validateTypedTableAdditions(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, updates []catalog.ObjectUpdate, types builtin.TypeResolver, information builtin.InformationSchemaTypeResolver, options TypeDomainValidationOptions) error {
	for _, update := range updates {
		base, err := update.Base.SpecWithin(ctx, options.Runtime.Descriptors)
		if err != nil || base.Table == nil {
			return errors.Join(model.ErrInvalidDescriptor, err)
		}
		next, err := update.Next.SpecWithin(ctx, options.Runtime.Descriptors)
		if err != nil || next.Table == nil {
			return errors.Join(model.ErrInvalidDescriptor, err)
		}
		existing := make(map[model.ColumnID]bool, len(base.Table.Columns))
		for _, column := range base.Table.Columns {
			existing[column.ID] = true
		}
		var added []model.ColumnDescriptor
		for _, column := range next.Table.Columns {
			if !existing[column.ID] {
				added = append(added, column)
			}
		}
		if len(added) == 0 {
			continue
		}
		if len(base.Table.Columns) == 0 {
			return model.ErrInvalidDescriptor
		}
		layout := make([]storage.ColumnLayout, len(base.Table.Columns))
		for i, column := range base.Table.Columns {
			layout[i] = storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
			if column.ConstantDefault != nil {
				layout[i].HasMissingValue = true
				layout[i].MissingValue = *column.ConstantDefault
			}
		}
		physical := update.Base.Physical()
		target := storage.ObjectRef{ObjectID: update.Base.ID(), Generation: physical.Generation, DescriptorVersion: update.Base.Version(), EncodingVersion: physical.EncodingVersion}
		request := storage.PushdownRequest{
			Target: target, Layout: layout, ColumnIDs: []model.ColumnID{base.Table.Columns[0].ID},
			Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		}
		plan, err := reader.PlanScan(ctx, request)
		if err != nil {
			return err
		}
		valid := plan.SnapshotTS == snapshot.Timestamp() && plan.Request.Target == target && slices.Equal(plan.Projection, request.ColumnIDs) && len(plan.Ordering) == 0 && plan.Accepted.Empty() && plan.Residual.Empty() && !plan.AppliedLimit.Enabled && plan.Binding != ([32]byte{})
		if valid {
			expected, hashErr := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: request, SnapshotTS: snapshot.Timestamp()}, options.ValueBytes)
			if hashErr != nil {
				err = hashErr
			} else {
				actual, hashErr := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: plan.Request, SnapshotTS: plan.SnapshotTS}, options.ValueBytes)
				if hashErr != nil {
					err = hashErr
				} else if expected != actual {
					err = storage.ErrInvalidScanPlan
				}
			}
		} else {
			err = storage.ErrInvalidScanPlan
		}
		if err != nil {
			return errors.Join(err, reader.ReleasePlan(plan))
		}
		cursor, err := reader.VectorizedScan(ctx, plan)
		if err != nil {
			return errors.Join(err, reader.ReleasePlan(plan))
		}
		batch := columnar.Batch{MaxRows: options.BatchRows, Columns: make([]columnar.Vector, 0, 1)}
		var scanErr error
	scan:
		for {
			batch.Reset()
			batch.MaxRows = options.BatchRows
			nextErr := cursor.Next(ctx, &batch)
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				scanErr = nextErr
				break
			}
			if err := batch.Validate(options.BatchRows); err != nil {
				scanErr = err
				break
			}
			if batch.Rows() > options.BatchRows || len(batch.Columns) != 1 {
				scanErr = storage.ErrInvalidScanPlan
				break
			}
			if batch.Rows() == 0 {
				continue
			}
			resolvers := builtin.ResolveOptions{Types: types, InformationSchemaTypes: information}
			for _, column := range added {
				value := model.NullVal(column.Layout.Type.Storage)
				if column.ConstantDefault != nil {
					value = *column.ConstantDefault
				}
				if !column.Nullable && value.IsNull {
					scanErr = &model.Error{Code: "23502", Message: "typed table attribute does not allow null values"}
					break scan
				}
				if err := builtin.ValidateResolvedValue(ctx, resolvers, column.Layout, value, options.Call, options.Runtime.Descriptors.MaxDepth); err != nil {
					scanErr = err
					break scan
				}
			}
			break
		}
		scanErr = errors.Join(scanErr, cursor.Close(), reader.ReleasePlan(plan))
		if scanErr != nil {
			return scanErr
		}
	}
	return nil
}
