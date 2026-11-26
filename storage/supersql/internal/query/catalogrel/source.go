package catalogrel

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type Source struct {
	options SourceOptions
	view    ReadView
}

func (s *Source) DatabaseIdentity(ctx context.Context) (model.DatabaseIdentity, error) {
	if err := ctx.Err(); err != nil { return model.DatabaseIdentity{}, err }
	if s == nil { return model.DatabaseIdentity{}, &model.Error{Code: "55000", Message: "catalog source is unavailable"} }
	return model.DatabaseIdentity{Name: strings.Clone(s.options.Database.Name), Encoding: s.options.Database.Encoding}, nil
}

func (s *Source) SequenceParameters(ctx context.Context, oid model.OID) (model.Value, error) {
	sequence, err := s.sequenceMetadata(ctx, oid)
	if err != nil { return model.Value{}, err }
	return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{
		model.Int64Val(sequence.Start), model.Int64Val(sequence.Min), model.Int64Val(sequence.Max),
		model.Int64Val(sequence.Increment), model.BoolVal(sequence.Cycle), model.Int64Val(sequence.Cache),
		model.Int64Val(int64(sequence.Type.ID)),
	}}, nil
}

func (s *Source) SequenceData(ctx context.Context, oid model.OID) (model.Value, error) {
	sequence, err := s.sequenceMetadata(ctx, oid)
	if err != nil { return model.Value{}, err }
	state, err := s.options.Sequences.Get(ctx, s.view, schema.SequenceIncarnation(sequence.Incarnation))
	if err != nil { return model.Value{}, err }
	return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(state.Last), model.BoolVal(state.Called)}}, nil
}

func (s *Source) sequenceMetadata(ctx context.Context, oid model.OID) (_ model.SequenceMetadata, err error) {
	if s == nil || s.view == nil { return model.SequenceMetadata{}, &model.Error{Code: "55000", Message: "catalog source is not bound to the statement read view"} }
	snapshot, err := s.options.Snapshot.OpenCatalogSnapshot(ctx, s.view)
	if err != nil { return model.SequenceMetadata{}, err }
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	var descriptor *model.Descriptor
	err = snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(namespace catalog.PublishedDescriptor) error {
		return snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectSequence, func(published catalog.PublishedDescriptor) error {
			if published.Descriptor.OID() == oid { descriptor = published.Descriptor }
			return nil
		})
	})
	if err != nil { return model.SequenceMetadata{}, err }
	if descriptor == nil { return model.SequenceMetadata{}, &model.Error{Code: "42P01", Message: "sequence does not exist"} }
	sequence, ok := descriptor.Sequence()
	if !ok { return model.SequenceMetadata{}, model.ErrInvalidDescriptor }
	return sequence, nil
}

func NewSource(options SourceOptions) (*Source, error) {
	if options.Snapshot == nil || options.Database.OID == 0 || options.Database.Name == "" || options.Database.Owner == 0 || options.Database.Encoding != 6 || options.Database.RoleOID == 0 || options.Database.RoleName == "" || options.Database.Collation == "" || options.Database.CharacterType == "" || options.Database.CollationOID == 0 {
		return nil, &model.Error{Code: "22023", Message: "incomplete catalog source identity"}
	}
	if !options.Registry.Complete() || options.Statistics == nil || options.Sequences == nil || options.Features == nil || options.Standards == nil {
		return nil, &model.Error{Code: "22023", Message: "incomplete catalog source collaborators"}
	}
	principal := model.Principal{}
	if options.Database.Owner != principal.OID() || options.Database.RoleOID != principal.OID() || options.Database.RoleName != principal.Name() {
		return nil, &model.Error{Code: "22023", Message: "catalog source principal does not match the embedded identity"}
	}
	if options.Database.RoleSuperuser || !options.Database.RoleInherit || options.Database.RoleCreateRole || options.Database.RoleCreateDB ||
		!options.Database.RoleCanLogin || options.Database.RoleReplication || options.Database.RoleConnectionLimit != -1 || options.Database.RoleBypassRLS {
		return nil, &model.Error{Code: "22023", Message: "catalog source principal capabilities do not match the embedded identity"}
	}
	if options.Database.LocaleProvider != 'b' && options.Database.LocaleProvider != 'c' {
		return nil, &model.Error{Code: "22023", Message: "unsupported database locale provider"}
	}
	collation, ok := builtin.LookupCollation(options.Database.CollationOID)
	collate, characterType, locale, version := builtinCollationDetails(collation)
	if !ok || (collation.Provider != 'd' && collation.Provider != options.Database.LocaleProvider) ||
		(collate != "" && collate != options.Database.Collation) || (characterType != "" && characterType != options.Database.CharacterType) ||
		(locale != "" && locale != options.Database.Locale) || (version != "" && version != options.Database.CollationVersion) {
		return nil, &model.Error{Code: "22023", Message: "catalog source database collation identity is inconsistent"}
	}
	if options.Database.LocaleProvider == 'b' {
		if options.Database.Locale == "" || options.Database.CollationVersion == "" {
			return nil, &model.Error{Code: "22023", Message: "incomplete built-in database locale metadata"}
		}
	} else if options.Database.Locale != "" || options.Database.CollationVersion != "" {
		return nil, &model.Error{Code: "22023", Message: "libc database locale metadata is inconsistent"}
	}
	if options.MaxRows == 0 || options.MaxBytes == 0 || options.MaxDescriptors == 0 || options.MaxDescriptorBytes == 0 || options.MaxDescriptorItems <= 0 || options.MaxDescriptorDepth <= 0 {
		return nil, &model.Error{Code: "22023", Message: "catalog source budgets must be positive"}
	}
	return &Source{options: options}, nil
}

func validateRelationIdentities(in map[RelationID]RelationIdentity, informationSchema NamespaceIdentity) (map[RelationID]RelationIdentity, error) {
	if informationSchema.ObjectID == 0 || informationSchema.OID == 0 || informationSchema.OID == 11 {
		return nil, &model.Error{Code: "22023", Message: "invalid information_schema namespace identity"}
	}
	out := cloneRelationIdentities(in)
	if len(out) != len(registry) {
		return nil, &model.Error{Code: "22023", Message: "logical catalog relation identities are incomplete"}
	}
	seen := make(map[model.OID]RelationID, len(registry)*3+2)
	seen[11] = 0
	seen[informationSchema.OID] = 0
	for id := range out {
		if id == 0 || int(id) > len(registry) {
			return nil, &model.Error{Code: "22023", Message: "unknown logical catalog relation identity"}
		}
	}
	for _, relation := range registry {
		identity, ok := out[relation.ID]
		if !ok || identity.RelationOID == 0 || identity.RowTypeOID == 0 || identity.ArrayTypeOID == 0 || identity.RelationOID == identity.RowTypeOID || identity.RelationOID == identity.ArrayTypeOID || identity.RowTypeOID == identity.ArrayTypeOID {
			return nil, &model.Error{Code: "22023", Message: "invalid logical catalog relation identity", Detail: relation.Namespace + "." + relation.Name}
		}
		if (relation.Identity.RelationOID != 0 && relation.Identity.RelationOID != identity.RelationOID) || (relation.Identity.RowTypeOID != 0 && relation.Identity.RowTypeOID != identity.RowTypeOID) || (relation.Identity.ArrayTypeOID != 0 && relation.Identity.ArrayTypeOID != identity.ArrayTypeOID) {
			return nil, &model.Error{Code: "22023", Message: "logical catalog relation identity differs from the pinned PostgreSQL identity", Detail: relation.Namespace + "." + relation.Name}
		}
		if _, exists := builtin.LookupType(model.TypeID(identity.RowTypeOID)); exists && relation.Identity.RowTypeOID != identity.RowTypeOID {
			return nil, &model.Error{Code: "22023", Message: "logical catalog row type identity collides with a built-in type", Detail: relation.Namespace + "." + relation.Name}
		}
		if _, exists := builtin.LookupType(model.TypeID(identity.ArrayTypeOID)); exists && relation.Identity.ArrayTypeOID != identity.ArrayTypeOID {
			return nil, &model.Error{Code: "22023", Message: "logical catalog array type identity collides with a built-in type", Detail: relation.Namespace + "." + relation.Name}
		}
		for _, oid := range []model.OID{identity.RelationOID, identity.RowTypeOID, identity.ArrayTypeOID} {
			if _, exists := seen[oid]; exists {
				return nil, &model.Error{Code: "22023", Message: "duplicate logical catalog SQL identity", Detail: relation.Namespace + "." + relation.Name}
			}
			seen[oid] = relation.ID
		}
	}
	return out, nil
}

func cloneRelationIdentities(in map[RelationID]RelationIdentity) map[RelationID]RelationIdentity {
	if in == nil {
		return nil
	}
	out := make(map[RelationID]RelationIdentity, len(in))
	for id, identity := range in {
		out[id] = identity
	}
	return out
}
func validateInformationSchemaIdentities(config RegistryConfig, relations map[RelationID]RelationIdentity) ([]RegisteredInformationDomain, []builtin.InformationSchemaFunctionBinding, error) {
	domainDefinitions := builtin.InformationDomains()
	functionDefinitions := builtin.InformationSchemaFunctions()
	if len(config.InformationDomains) != len(domainDefinitions) || len(config.InformationSchemaFunctions) != len(functionDefinitions) {
		return nil, nil, &model.Error{Code: "22023", Message: "information schema identities are incomplete"}
	}
	used := map[model.OID]string{11: "pg_catalog", config.InformationSchema.OID: "information_schema"}
	for _, identity := range relations {
		for _, oid := range []model.OID{identity.RelationOID, identity.RowTypeOID, identity.ArrayTypeOID} {
			used[oid] = "logical catalog relation"
		}
	}
	mark := func(oid model.OID, name string) error {
		if oid == 0 {
			return &model.Error{Code: "22023", Message: "information schema identity is zero", Detail: name}
		}
		if prior, exists := used[oid]; exists {
			return &model.Error{Code: "22023", Message: "duplicate information schema SQL identity", Detail: name + " collides with " + prior}
		}
		used[oid] = name
		return nil
	}
	for kind := range config.InformationDomains {
		if _, ok := builtin.InformationDomainByKind(kind); !ok {
			return nil, nil, &model.Error{Code: "22023", Message: "unknown information schema domain identity"}
		}
	}
	domains := make([]RegisteredInformationDomain, len(domainDefinitions))
	for i, definition := range domainDefinitions {
		identity, ok := config.InformationDomains[definition.Kind]
		if !ok || identity.TypeOID == identity.ArrayTypeOID {
			return nil, nil, &model.Error{Code: "22023", Message: "invalid information schema domain identity", Detail: definition.Name}
		}
		if definition.ConstraintName == "" {
			if identity.ConstraintOID != 0 {
				return nil, nil, &model.Error{Code: "22023", Message: "constraint identity supplied for unconstrained information schema domain", Detail: definition.Name}
			}
		} else if identity.ConstraintOID == 0 {
			return nil, nil, &model.Error{Code: "22023", Message: "information schema domain constraint identity is unavailable", Detail: definition.Name}
		}
		if _, exists := builtin.LookupType(model.TypeID(identity.TypeOID)); exists {
			return nil, nil, &model.Error{Code: "22023", Message: "information schema domain identity collides with a built-in type", Detail: definition.Name}
		}
		if _, exists := builtin.LookupType(model.TypeID(identity.ArrayTypeOID)); exists {
			return nil, nil, &model.Error{Code: "22023", Message: "information schema domain array identity collides with a built-in type", Detail: definition.Name}
		}
		if err := mark(identity.TypeOID, "information_schema."+definition.Name); err != nil {
			return nil, nil, err
		}
		if err := mark(identity.ArrayTypeOID, "information_schema._"+definition.Name); err != nil {
			return nil, nil, err
		}
		if identity.ConstraintOID != 0 {
			if err := mark(identity.ConstraintOID, "information_schema."+definition.ConstraintName); err != nil {
				return nil, nil, err
			}
		}
		layout := cloneLayout(definition.Base)
		layout.Type.ID, layout.Type.Modifier = model.TypeID(identity.TypeOID), -1
		array := model.TypeLayout{
			Type:     model.Type{ID: model.TypeID(identity.ArrayTypeOID), Modifier: -1, Storage: model.TypeArray, Collation: layout.Type.Collation},
			Children: []model.TypeLayout{cloneLayout(layout)},
		}
		domains[i] = RegisteredInformationDomain{Definition: definition, Identity: identity, Layout: layout, Array: array}
	}
	for operation := range config.InformationSchemaFunctions {
		if _, ok := builtin.InformationSchemaFunctionByOperation(operation); !ok {
			return nil, nil, &model.Error{Code: "22023", Message: "unknown information schema function identity"}
		}
	}
	functions := make([]builtin.InformationSchemaFunctionBinding, len(functionDefinitions))
	for i, definition := range functionDefinitions {
		oid, ok := config.InformationSchemaFunctions[definition.Operation]
		if !ok {
			return nil, nil, &model.Error{Code: "22023", Message: "information schema function identity is unavailable", Detail: definition.Signature.Name}
		}
		if _, exists := builtin.LookupSignature(model.FunctionID(oid)); exists {
			return nil, nil, &model.Error{Code: "22023", Message: "information schema function identity collides with a built-in function", Detail: definition.Signature.Name}
		}
		if err := mark(oid, "information_schema."+definition.Signature.Name); err != nil {
			return nil, nil, err
		}
		signature := definition.Signature
		signature.ID = model.FunctionID(oid)
		functions[i] = builtin.InformationSchemaFunctionBinding{Operation: definition.Operation, Signature: signature}
	}
	return domains, functions, nil
}


func (s *Source) WithSettings(settings Settings) Scanner {
	if s == nil || settings == nil {
		return s
	}
	copy := *s
	copy.options.Settings = settings
	return &copy
}

func (s *Source) WithCatalogView(view ReadView) Scanner {
	if s == nil || view == nil {
		return s
	}
	copy := *s
	copy.view = view
	return &copy
}

// AuxiliaryScanStore identifies one storage collection catalog emitters read
// through the statement view beyond the catalog store itself.
type AuxiliaryScanStore struct {
	Target    storage.ObjectRef
	KeyLayout []storage.KeyColumn
}

// AuxiliaryScanStoresProvider reports the auxiliary collections a catalog
// scan may read so the transaction's declared read coverage can include them.
type AuxiliaryScanStoresProvider interface {
	AuxiliaryScanStores() []AuxiliaryScanStore
}

// AuxiliaryScanStores reports the statistics and sequence-state collections
// emitters may read through the statement view. WithSettings and
// WithCatalogView return *Source, so the provider survives attachment.
func (s *Source) AuxiliaryScanStores() []AuxiliaryScanStore {
	if s == nil {
		return nil
	}
	var stores []AuxiliaryScanStore
	if factory, ok := s.options.Statistics.(StatisticsStoreFactory); ok && factory.Store != nil {
		for _, layout := range factory.Store.Layouts() {
			stores = append(stores, AuxiliaryScanStore{Target: layout.Target, KeyLayout: factory.Store.KeyLayout()})
		}
	}
	type sequenceTarget interface {
		Target() storage.ObjectRef
		KeyLayout() []storage.KeyColumn
	}
	if sequences, ok := s.options.Sequences.(sequenceTarget); ok && sequences != nil {
		stores = append(stores, AuxiliaryScanStore{Target: sequences.Target(), KeyLayout: sequences.KeyLayout()})
	}
	return stores
}

var _ AuxiliaryScanStoresProvider = (*Source)(nil)

func (s *Source) OpenCatalog(ctx context.Context, request ScanRequest) (storage.BatchCursor, error) {
	if s == nil || s.view == nil {
		return nil, &model.Error{Code: "55000", Message: "catalog source is not bound to the statement read view"}
	}
	if err := ctx.Err(); err != nil {
		return nil, canceled(err)
	}
	relation, ok := s.options.Registry.LookupID(request.Relation)
	if !ok {
		return nil, &model.Error{Code: "42P01", Message: "unknown logical catalog relation"}
	}
	indexes, layouts, err := selectColumns(relation, request.Columns)
	if err != nil {
		return nil, err
	}
	batchRows := request.BatchRows
	if batchRows <= 0 || uint64(batchRows) > s.options.MaxRows {
		batchRows = int(min(s.options.MaxRows, uint64(maxInt())))
	}
	batchBytes := request.BatchBytes
	if batchBytes == 0 || batchBytes > s.options.MaxBytes {
		batchBytes = s.options.MaxBytes
	}
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: batchRows, MaxBytes: batchBytes})
	if err != nil {
		return nil, err
	}
	snapshot, err := s.options.Snapshot.OpenCatalogSnapshot(ctx, s.view)
	if err != nil {
		_ = builder.Close()
		return nil, err
	}
	if snapshot == nil || snapshot.Timestamp() != s.view.Timestamp() {
		if snapshot != nil {
			_ = snapshot.Close()
		}
		_ = builder.Close()
		return nil, &model.Error{Code: "55000", Message: "catalog snapshot timestamp mismatch"}
	}
	producerContext, cancel := context.WithCancel(ctx)
	cursor := &catalogCursor{
		context: producerContext,
		cancel: cancel,
		rows: make(chan rowResult),
		done: make(chan struct{}),
		builder: builder,
		indexes: indexes,
		batchRows: batchRows,
		snapshot: snapshot,
		projected: make([]model.Value, len(indexes)),
		view: s.view,
	}
	go cursor.produce(relation, s.options)
	return cursor, nil
}


func selectColumns(relation Relation, columns []model.ColumnID) ([]int, []model.TypeLayout, error) {
	indexes := make([]int, len(columns))
	layouts := make([]model.TypeLayout, len(columns))
	for i, id := range columns {
		if id == 0 || int(id) > len(relation.Columns) {
			return nil, nil, &model.Error{Code: "42703", Message: "unknown logical catalog column"}
		}
		column := relation.Columns[id-1]
		// Unsupported non-nullable columns are still emitted (as NULL through
		// emitCatalogRow): naming one is rejected during binding, but a scan
		// carries the full column set regardless of what the query projects.
		indexes[i] = int(id - 1)
		layouts[i] = cloneLayout(column.Layout)
	}
	return indexes, layouts, nil
}

type rowResult struct {
	values []model.Value
	err error
}

type catalogCursor struct {
	context context.Context
	cancel context.CancelFunc
	rows chan rowResult
	done chan struct{}
	builder *columnar.Builder
	indexes []int
	projected []model.Value
	batchRows int
	snapshot Snapshot
	view ReadView
	pending []model.Value
	terminal error
	closeOnce sync.Once
	closeErr error
}

func (c *catalogCursor) produce(relation Relation, options SourceOptions) {
	defer close(c.done)
	defer close(c.rows)
	// The scan row bound tracks catalog capacity, not the batch size: every
	// emitted row traces back to a descriptor or descriptor item, so
	// MaxDescriptors*MaxDescriptorItems bounds it. MaxRows stays the
	// per-batch bound.
	budget := rowBudget{remainingRows: options.MaxDescriptors * uint64(options.MaxDescriptorItems), remainingBytes: options.MaxBytes}
	err := generateRows(c.context, c.snapshot, c.view, relation, options, func(row []model.Value) error {
		if err := budget.admit(row); err != nil {
			return err
		}
		select {
		case c.rows <- rowResult{values: row}:
			return nil
		case <-c.context.Done():
			return c.context.Err()
		}
	})
	if err != nil {
		select {
		case c.rows <- rowResult{err: err}:
		case <-c.context.Done():
		}
	}
}

func (c *catalogCursor) Next(ctx context.Context, out *columnar.Batch) error {
	if out == nil {
		return &model.Error{Code: "22023", Message: "nil catalog output batch"}
	}
	if c.terminal != nil {
		_ = c.finish()
		return c.terminal
	}
	if err := ctx.Err(); err != nil {
		c.cancel()
		c.terminal = canceled(err)
		_ = c.finish()
		return c.terminal
	}
	c.builder.Reset()
	limit := c.batchRows
	if out.MaxRows > 0 && out.MaxRows < limit {
		limit = out.MaxRows
	}
scan:
	for c.builder.Rows() < limit {
		row, ok, err := c.nextRow(ctx)
		if err != nil {
			if c.builder.Rows() == 0 {
				c.terminal = err
				_ = c.finish()
				return err
			}
			c.terminal = err
			break
		}
		if !ok {
			if c.builder.Rows() == 0 {
				c.terminal = io.EOF
				_ = c.finish()
				return io.EOF
			}
			c.terminal = io.EOF
			break scan
		}
		for i, index := range c.indexes {
			c.projected[i] = row[index]
		}
		appendErr := c.builder.Append(ctx, c.projected)
		if cause := ctx.Err(); cause != nil {
			appendErr = canceled(cause)
		}
		if appendErr != nil {
			if errors.Is(appendErr, columnar.ErrBatchBudget) && c.builder.Rows() != 0 {
				c.pending = row
				break
			}
			c.terminal = appendErr
			_ = c.finish()
			return appendErr
		}
	}
	return c.builder.Publish(out)
}

func (c *catalogCursor) nextRow(ctx context.Context) ([]model.Value, bool, error) {
	if c.pending != nil {
		row := c.pending
		c.pending = nil
		return row, true, nil
	}
	select {
	case result, ok := <-c.rows:
		if !ok {
			return nil, false, nil
		}
		if result.err != nil {
			return nil, false, result.err
		}
		return result.values, true, nil
	case <-ctx.Done():
		c.cancel()
		return nil, false, canceled(ctx.Err())
	}
}

func (c *catalogCursor) Close() error {
	c.cancel()
	return c.finish()
}

func (c *catalogCursor) finish() error {
	c.closeOnce.Do(func() {
		c.cancel()
		<-c.done
		c.closeErr = errors.Join(c.builder.Close(), c.snapshot.Close())
	})
	return c.closeErr
}

type rowBudget struct {
	remainingRows uint64
	remainingBytes uint64
}

func (b *rowBudget) admit(row []model.Value) error {
	if b.remainingRows == 0 {
		return &model.Error{Code: "54000", Message: "catalog row budget exceeded"}
	}
	bytes := uint64(len(row)) * 64
	for _, value := range row {
		bytes += valueBytes(value)
	}
	if bytes > b.remainingBytes {
		return &model.Error{Code: "54000", Message: "catalog byte budget exceeded"}
	}
	b.remainingRows--
	b.remainingBytes -= bytes
	return nil
}

func valueBytes(value model.Value) uint64 {
	bytes := uint64(len(value.StrVal) + len(value.BytesVal) + len(value.VectorVal)*4 + len(value.Dimensions)*8)
	for _, child := range value.ArrayVal {
		bytes += 64 + valueBytes(child)
	}
	return bytes
}

func canceled(cause error) error {
	return &model.Error{Code: "57014", Message: "catalog scan canceled", Cause: cause}
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

var _ Scanner = (*Source)(nil)
var _ SettingsAttacher = (*Source)(nil)
var _ ViewAttacher = (*Source)(nil)
var _ model.CatalogMetadataIntrospection = (*Source)(nil)
