package schema

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type SchemaJobOperation uint8

const (
	SchemaAddColumn SchemaJobOperation = iota + 1
	SchemaDropColumn
	SchemaAlterColumnType
	SchemaRewritePrimaryKey
	SchemaDropIndex
	SchemaAlterColumnExpression
	SchemaCreateIndexTransactional
	SchemaReindex
)

type GenerationTransform struct {
	Column     model.ColumnID
	Expression *model.ExpressionDefinition
}

// JobDefinition is the immutable transformation contract committed with a job
// intent. Source descriptors remain the decoding contract for the protected
// snapshot; target descriptors are published together only after validation.
type JobDefinition struct {
	JobID         JobID
	Operation     SchemaJobOperation
	SourceTable   *model.Descriptor
	TargetTable   *model.Descriptor
	SourceIndexes []*model.Descriptor
	TargetIndexes []*model.Descriptor
	Transforms    []GenerationTransform
}

const jobDefinitionEncoding = "SJDF0001"
const jobDefinitionHeaderBytes = 32

var errJobDefinition = errors.New("invalid durable schema job definition")

type jobTransformWire struct {
	Column     model.ColumnID              `json:"column"`
	Expression *model.ExpressionDefinition `json:"expression"`
}

func definitionDescriptorLimits(maximum uint64) model.DescriptorLimits {
	return model.DescriptorLimits{MaxBytes: maximum, MaxItems: int(min(maximum, uint64(math.MaxInt))), MaxDepth: 64}
}

func definitionCatalogLimits(maximum, keyBytes uint64, batchRows int) catalog.Limits {
	return catalog.Limits{
		Descriptor:      definitionDescriptorLimits(maximum),
		MaxEncodedBytes: maximum,
		MaxKeyBytes:     keyBytes,
		ScanBatchRows:   batchRows,
	}
}

func validateJobDefinition(ctx context.Context, job Job, definition JobDefinition, maximum uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if definition.JobID != job.ID || definition.SourceTable == nil || definition.TargetTable == nil {
		return errJobDefinition
	}
	if definition.SourceTable.ID() != job.TableID || definition.TargetTable.ID() != job.TableID || !tableShapedKind(definition.SourceTable.Kind()) || !tableShapedKind(definition.TargetTable.Kind()) {
		return errJobDefinition
	}
	if definition.SourceTable.Version() != job.SourceVersion || definition.TargetTable.Version() != job.TargetVersion || definition.SourceTable.State() != model.StatePublic || definition.TargetTable.State() != model.StatePublic {
		return errJobDefinition
	}
	if definition.SourceTable.Name() != definition.TargetTable.Name() || definition.SourceTable.OID() != definition.TargetTable.OID() {
		return errJobDefinition
	}
	sourcePhysical, targetPhysical := definition.SourceTable.Physical(), definition.TargetTable.Physical()
	switch definition.Operation {
	case SchemaAddColumn:
		if job.Kind != AddColumnJob || targetPhysical != sourcePhysical {
			return errJobDefinition
		}
	case SchemaDropColumn, SchemaAlterColumnType, SchemaRewritePrimaryKey, SchemaReindex:
		if job.Kind != RewriteGenerationJob || targetPhysical.Generation <= sourcePhysical.Generation || targetPhysical.EncodingVersion == 0 {
			return errJobDefinition
		}
	case SchemaDropIndex:
		if job.Kind != DropIndexJob || targetPhysical != sourcePhysical || len(definition.SourceIndexes) != 1 || len(definition.TargetIndexes) != 0 {
			return errJobDefinition
		}
	default:
		return errJobDefinition
	}
	if definition.Operation == SchemaAddColumn && (len(definition.SourceIndexes) != 0 || len(definition.TargetIndexes) != 0) {
		return errJobDefinition
	}
	if definition.Operation == SchemaDropIndex && (len(definition.SourceIndexes) != 1 || len(definition.TargetIndexes) != 0 || definition.SourceIndexes[0].ID() != job.MutationID) {
		return errJobDefinition
	}
	sourceSpec, err := definition.SourceTable.SpecWithin(ctx, definitionDescriptorLimits(maximum))
	if err != nil || sourceSpec.Table == nil {
		return errors.Join(errJobDefinition, err)
	}
	if definition.Operation == SchemaDropColumn || definition.Operation == SchemaAlterColumnType || definition.Operation == SchemaRewritePrimaryKey || definition.Operation == SchemaReindex {
		if len(definition.SourceIndexes) != len(definition.TargetIndexes) {
			return errJobDefinition
		}
		for i, source := range definition.SourceIndexes {
			target := definition.TargetIndexes[i]
			generationAdvanced := target.Physical().Generation > source.Physical().Generation
			generationPreserved := definition.Operation == SchemaReindex && target.Physical() == source.Physical() && source.ID() != job.MutationID
			if source.ID() != target.ID() || target.Version() != source.Version()+1 || !generationAdvanced && !generationPreserved {
				return errJobDefinition
			}
			if err := source.ValidateSuccessor(ctx, target); err != nil {
				return errors.Join(errJobDefinition, err)
			}
		}
	}
	if definition.Operation == SchemaReindex {
		for i, target := range definition.TargetIndexes {
			targetMetadata, targetOK := target.Index()
			sourceMetadata, sourceOK := definition.SourceIndexes[i].Index()
			if !targetOK || !sourceOK {
				return errJobDefinition
			}
			if target.Physical().Generation > definition.SourceIndexes[i].Physical().Generation {
				if !targetMetadata.Valid() || !targetMetadata.Ready() || !targetMetadata.Live() {
					return errJobDefinition
				}
			} else if targetMetadata.Invalid != sourceMetadata.Invalid || targetMetadata.NotReady != sourceMetadata.NotReady || targetMetadata.NotLive != sourceMetadata.NotLive {
				return errJobDefinition
			}
		}
	}
	sourceSpec.Version = definition.TargetTable.Version() - 1
	prior, err := model.NewDescriptor(ctx, sourceSpec, definitionDescriptorLimits(maximum))
	if err != nil {
		return errors.Join(errJobDefinition, err)
	}
	if err := prior.ValidateSuccessor(ctx, definition.TargetTable); err != nil {
		return errors.Join(errJobDefinition, err)
	}
	if uint64(len(definition.SourceIndexes))+uint64(len(definition.TargetIndexes))+uint64(len(definition.Transforms)) > maximum {
		return ErrJobLimit
	}
	if err := validateDefinitionIndexes(ctx, job.TableID, definition.SourceIndexes, definition.TargetIndexes); err != nil {
		return err
	}
	targetSpec, err := definition.TargetTable.SpecWithin(ctx, definitionDescriptorLimits(maximum))
	if err != nil || targetSpec.Table == nil {
		return errors.Join(errJobDefinition, err)
	}
	seen := make(map[model.ColumnID]struct{}, len(definition.Transforms))
	for _, transform := range definition.Transforms {
		if transform.Column == 0 || transform.Expression == nil || transform.Expression.SemanticVersion == 0 || transform.Expression.SQL == "" && len(transform.Expression.Encoding) == 0 {
			return errJobDefinition
		}
		if uint64(len(transform.Expression.SQL)) > maximum || uint64(len(transform.Expression.Encoding)) > maximum || uint64(len(transform.Expression.Dependencies)) > maximum/uint64(unsafe.Sizeof(model.Dependency{})) {
			return ErrJobLimit
		}
		if _, exists := seen[transform.Column]; exists {
			return errJobDefinition
		}
		seen[transform.Column] = struct{}{}
		if !slices.ContainsFunc(targetSpec.Table.Columns, func(column model.ColumnDescriptor) bool { return column.ID == transform.Column }) {
			return errJobDefinition
		}
		for i, dependency := range transform.Expression.Dependencies {
			if dependency.Object == 0 || dependency.Version == 0 || i > 0 && compareDefinitionDependency(transform.Expression.Dependencies[i-1], dependency) >= 0 {
				return errJobDefinition
			}
		}
	}
	if definition.Operation != SchemaAlterColumnType && len(definition.Transforms) != 0 {
		return errJobDefinition
	}
	return nil
}

func compareDefinitionDependency(left, right model.Dependency) int {
	if left.Object < right.Object {
		return -1
	}
	if left.Object > right.Object {
		return 1
	}
	if left.Version < right.Version {
		return -1
	}
	if left.Version > right.Version {
		return 1
	}
	if left.Column < right.Column {
		return -1
	}
	if left.Column > right.Column {
		return 1
	}
	return 0
}

func validateDefinitionIndexes(ctx context.Context, table model.ObjectID, source, target []*model.Descriptor) error {
	validate := func(descriptors []*model.Descriptor) error {
		var previous model.ObjectID
		for _, descriptor := range descriptors {
			if err := ctx.Err(); err != nil {
				return err
			}
			if descriptor == nil || descriptor.Kind() != model.ObjectIndex || descriptor.State() != model.StatePublic || descriptor.ID() <= previous {
				return errJobDefinition
			}
			spec, err := descriptor.Spec(ctx)
			if err != nil || spec.Index == nil || spec.Index.Table != table {
				return errors.Join(errJobDefinition, err)
			}
			previous = descriptor.ID()
		}
		return nil
	}
	if err := validate(source); err != nil {
		return err
	}
	if err := validate(target); err != nil {
		return err
	}
	return nil
}

func EncodeJobDefinition(ctx context.Context, job Job, definition JobDefinition, maximum, keyBytes uint64, batchRows int) ([]byte, error) {
	if err := validateJobDefinition(ctx, job, definition, maximum); err != nil {
		return nil, err
	}
	limits := definitionCatalogLimits(maximum, keyBytes, batchRows)
	descriptors := make([]*model.Descriptor, 0, 2+len(definition.SourceIndexes)+len(definition.TargetIndexes))
	descriptors = append(descriptors, definition.SourceTable, definition.TargetTable)
	descriptors = append(descriptors, definition.SourceIndexes...)
	descriptors = append(descriptors, definition.TargetIndexes...)
	encoded := make([][]byte, len(descriptors))
	total := uint64(jobDefinitionHeaderBytes) + uint64(len(encoded))*4
	for i, descriptor := range descriptors {
		data, err := catalog.EncodeDescriptor(ctx, descriptor, limits)
		if err != nil {
			return nil, err
		}
		encoded[i] = data
		if uint64(len(data)) > math.MaxUint32 || uint64(len(data)) > maximum || total > maximum-uint64(len(data)) {
			return nil, ErrJobLimit
		}
		total += uint64(len(data))
	}
	transforms := make([]jobTransformWire, len(definition.Transforms))
	for i, transform := range definition.Transforms {
		transforms[i] = jobTransformWire{Column: transform.Column, Expression: transform.Expression}
	}
	transformData, err := json.Marshal(transforms)
	if err != nil {
		return nil, err
	}
	if uint64(len(transformData)) > math.MaxUint32 || uint64(len(transformData)) > maximum || total > maximum-uint64(len(transformData)) {
		return nil, ErrJobLimit
	}
	total += uint64(len(transformData))
	if total > maximum || total > uint64(math.MaxInt) || len(definition.SourceIndexes) > math.MaxUint16 || len(definition.TargetIndexes) > math.MaxUint16 {
		return nil, ErrJobLimit
	}
	data := make([]byte, int(total))
	copy(data, jobDefinitionEncoding)
	binary.LittleEndian.PutUint64(data[8:16], uint64(definition.JobID))
	data[16] = byte(definition.Operation)
	binary.LittleEndian.PutUint16(data[18:20], uint16(len(definition.SourceIndexes)))
	binary.LittleEndian.PutUint16(data[20:22], uint16(len(definition.TargetIndexes)))
	binary.LittleEndian.PutUint32(data[24:28], uint32(len(transformData)))
	offset := jobDefinitionHeaderBytes
	for _, descriptor := range encoded {
		binary.LittleEndian.PutUint32(data[offset:offset+4], uint32(len(descriptor)))
		offset += 4
		copy(data[offset:], descriptor)
		offset += len(descriptor)
	}
	copy(data[offset:], transformData)
	return data, nil
}

func DecodeJobDefinition(ctx context.Context, job Job, data []byte, maximum, keyBytes uint64, batchRows int) (JobDefinition, error) {
	if err := ctx.Err(); err != nil {
		return JobDefinition{}, err
	}
	if len(data) < jobDefinitionHeaderBytes || uint64(len(data)) > maximum || string(data[:8]) != jobDefinitionEncoding || data[17] != 0 || data[22] != 0 || data[23] != 0 || data[28] != 0 || data[29] != 0 || data[30] != 0 || data[31] != 0 {
		return JobDefinition{}, errJobDefinition
	}
	definition := JobDefinition{JobID: JobID(binary.LittleEndian.Uint64(data[8:16])), Operation: SchemaJobOperation(data[16])}
	sourceCount := int(binary.LittleEndian.Uint16(data[18:20]))
	targetCount := int(binary.LittleEndian.Uint16(data[20:22]))
	transformBytes := int(binary.LittleEndian.Uint32(data[24:28]))
	count := 2 + sourceCount + targetCount
	if count < 2 || count > int(maximum/4) || transformBytes > len(data)-jobDefinitionHeaderBytes-count*4 {
		return JobDefinition{}, errJobDefinition
	}
	limits := definitionCatalogLimits(maximum, keyBytes, batchRows)
	descriptors := make([]*model.Descriptor, count)
	offset := jobDefinitionHeaderBytes
	for i := range descriptors {
		if offset > len(data)-4 {
			return JobDefinition{}, errJobDefinition
		}
		size := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if size <= 0 || size > len(data)-offset-transformBytes {
			return JobDefinition{}, errJobDefinition
		}
		descriptor, err := catalog.DecodeDescriptor(ctx, data[offset:offset+size], limits)
		if err != nil {
			return JobDefinition{}, errors.Join(errJobDefinition, err)
		}
		descriptors[i] = descriptor
		offset += size
	}
	if offset+transformBytes != len(data) {
		return JobDefinition{}, errJobDefinition
	}
	var transforms []jobTransformWire
	decoder := json.NewDecoder(bytes.NewReader(data[offset:]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&transforms); err != nil {
		return JobDefinition{}, errors.Join(errJobDefinition, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return JobDefinition{}, errJobDefinition
	}
	definition.SourceTable, definition.TargetTable = descriptors[0], descriptors[1]
	definition.SourceIndexes = descriptors[2 : 2+sourceCount]
	definition.TargetIndexes = descriptors[2+sourceCount:]
	definition.Transforms = make([]GenerationTransform, len(transforms))
	for i, transform := range transforms {
		if transform.Expression != nil {
			transform.Expression.SQL = strings.Clone(transform.Expression.SQL)
			transform.Expression.Encoding = bytes.Clone(transform.Expression.Encoding)
			transform.Expression.Dependencies = slices.Clone(transform.Expression.Dependencies)
		}
		definition.Transforms[i] = GenerationTransform{Column: transform.Column, Expression: transform.Expression}
	}
	if err := validateJobDefinition(ctx, job, definition, maximum); err != nil {
		return JobDefinition{}, err
	}
	return definition, nil
}

func (s *JobStore) Definition(ctx context.Context, reader JobReader, job Job) (JobDefinition, error) {
	if reader == nil || job.ID == 0 || job.Kind == AddIndexJob {
		return JobDefinition{}, errJobDefinition
	}
	data, err := s.read(ctx, reader, jobLogicalKey(3, uint64(job.ID)))
	if err != nil {
		return JobDefinition{}, err
	}
	return DecodeJobDefinition(ctx, job, data, s.options.RecordBytes, s.options.KeyBytes, s.options.BatchRows)
}

// CreateDefined commits the active-table guard, job record, and immutable target
// definition in one caller-owned statement.
func (s *JobStore) CreateDefined(ctx context.Context, transaction txn.MutationApplier, job Job, definition JobDefinition) error {
	if transaction == nil || job.Kind == AddIndexJob {
		return errJobDefinition
	}
	definitionData, err := EncodeJobDefinition(ctx, job, definition, s.options.RecordBytes, s.options.KeyBytes, s.options.BatchRows)
	if err != nil {
		return err
	}
	if err := s.create(ctx, transaction, job, definitionData); err != nil {
		return err
	}
	return nil
}

func (s *JobStore) create(ctx context.Context, transaction txn.MutationApplier, job Job, definition []byte) error {
	wrongState := job.State != JobPending || job.Revision != 1
	protected := job.SnapshotTS != 0 || job.ProtectedTimestamp != 0
	progressed := len(job.ResumeKey) != 0 || len(job.FailureKey) != 0
	if transaction == nil || wrongState || protected || progressed {
		return ErrJobRecord
	}
	payload, err := EncodeJob(ctx, job, s.options.RecordBytes)
	if err != nil {
		return err
	}
	_, err = s.Active(ctx, transaction, job.TableID)
	if err == nil {
		return ErrJobConflict
	}
	if errors.Is(err, ErrJobRecord) || !errors.Is(err, storage.ErrRowNotFound) {
		return err
	}
	guard := make([]byte, 8)
	binary.BigEndian.PutUint64(guard, uint64(job.ID))
	mutations := make([]storage.Mutation, 0, 3)
	for _, item := range []struct {
		key     []byte
		payload []byte
	}{{jobLogicalKey(1, uint64(job.TableID)), guard}, {jobLogicalKey(2, uint64(job.ID)), payload}, {jobLogicalKey(3, uint64(job.ID)), definition}} {
		if item.payload == nil {
			continue
		}
		mutation, err := s.mutation(ctx, storage.MutationInsert, item.key, item.payload)
		if err != nil {
			return err
		}
		mutations = append(mutations, mutation)
	}
	return transaction.ApplyMutations(ctx, mutations)
}
