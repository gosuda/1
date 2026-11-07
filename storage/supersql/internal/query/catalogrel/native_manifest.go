package catalogrel

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync"
	"unsafe"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// NativeCatalogManifestVersion identifies the durable encoding of the native
// catalog identity inventory. A reader rejects any other version instead of
// reallocating identities.
const NativeCatalogManifestVersion uint32 = 1

const nativeCatalogManifestHeader = "NCMF0001"

// NativeCatalogManifest is the durable identity inventory of one bootstrapped
// database. Config carries the pinned relation, information-domain, and
// helper-function SQL identities plus every bound native view definition;
// PgCatalogObjectID records the durable descriptor identity of the fixed
// pg_catalog schema (SQL OID 11), which the logical registry does not carry.
// Decoding never invents identities: an incomplete or incompatible record is
// rejected so a reopened database cannot silently diverge from its bootstrap.
type NativeCatalogManifest struct {
	InformationSchema NamespaceIdentity
	PgCatalogObjectID model.ObjectID
	Config            RegistryConfig
}

// RegistryConfig returns the configuration that reproduces the sealed registry.
func (m NativeCatalogManifest) RegistryConfig() RegistryConfig {
	config := m.Config
	config.InformationSchema = m.InformationSchema
	return config
}

// BoundViewDefinitions returns the sealed registry's definition for every
// canonical native view. A registry that is not Complete is refused.
func BoundViewDefinitions(registry *Registry) (map[RelationID]BoundViewDefinition, error) {
	if !registry.Complete() {
		return nil, &model.Error{Code: "55000", Message: "sealed catalog registry is unavailable"}
	}
	sources, err := NativeViewSources()
	if err != nil {
		return nil, err
	}
	definitions := make(map[RelationID]BoundViewDefinition, len(sources))
	for _, source := range sources {
		definition, ok := registry.ViewDefinition(source.Namespace, source.Name)
		if !ok {
			return nil, &model.Error{Code: "XX000", Message: "sealed native view definition is unavailable", Detail: source.Namespace + "." + source.Name}
		}
		definitions[source.Relation] = definition.Bound
	}
	return definitions, nil
}

type manifestWriter struct {
	out   []byte
	limit uint64
	fail  error
}

func (w *manifestWriter) bytes(count uint64) {
	if w.fail != nil {
		return
	}
	if count > w.limit || uint64(len(w.out)) > w.limit-count {
		w.fail = manifestLimitError()
		return
	}
	w.out = append(w.out, make([]byte, count)...)
}

func (w *manifestWriter) u8(value uint8) {
	w.bytes(1)
	if w.fail != nil {
		return
	}
	w.out[len(w.out)-1] = value
}

func (w *manifestWriter) u32(value uint32) {
	w.bytes(4)
	if w.fail != nil {
		return
	}
	binary.BigEndian.PutUint32(w.out[len(w.out)-4:], value)
}

func (w *manifestWriter) u64(value uint64) {
	w.bytes(8)
	if w.fail != nil {
		return
	}
	binary.BigEndian.PutUint64(w.out[len(w.out)-8:], value)
}

func (w *manifestWriter) raw(value []byte) {
	w.bytes(uint64(len(value)))
	if w.fail != nil {
		return
	}
	copy(w.out[len(w.out)-len(value):], value)
}

type manifestReader struct {
	in    []byte
	at    uint64
	limit uint64
	fail  error
}

func (r *manifestReader) take(count uint64) []byte {
	if r.fail != nil {
		return nil
	}
	if count > r.limit || r.at > uint64(len(r.in)) || count > uint64(len(r.in))-r.at {
		r.fail = manifestTruncatedError()
		return nil
	}
	value := r.in[r.at : r.at+count]
	r.at += count
	return value
}

func (r *manifestReader) u8() uint8 {
	value := r.take(1)
	if value == nil {
		return 0
	}
	return value[0]
}

func (r *manifestReader) u32() uint32 {
	value := r.take(4)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint32(value)
}

func (r *manifestReader) u64() uint64 {
	value := r.take(8)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}

func (r *manifestReader) raw(count uint64) []byte {
	value := r.take(count)
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

func manifestLimitError() error {
	return &model.Error{Code: "54000", Message: "native catalog manifest limit exceeded"}
}

func manifestTruncatedError() error {
	return &model.Error{Code: "XX001", Message: "native catalog manifest is truncated or malformed"}
}

func manifestContentError(detail string) error {
	return &model.Error{Code: "XX001", Message: "native catalog manifest is inconsistent", Detail: detail}
}

var (
	canonicalNativeViewEncodingsOnce sync.Once
	canonicalNativeViewEncodings     map[RelationID][]byte
	canonicalNativeViewEncodingsErr  error
)

func canonicalNativeViewSemanticEncoding(id RelationID) ([]byte, error) {
	canonicalNativeViewEncodingsOnce.Do(func() {
		sources, err := NativeViewSources()
		if err != nil {
			canonicalNativeViewEncodingsErr = err
			return
		}
		canonicalNativeViewEncodings = make(map[RelationID][]byte, len(sources))
		for _, source := range sources {
			parsed, err := zpgparse.Parse(source.SQL)
			if err != nil || parsed == nil || len(parsed.Stmts) != 1 || parsed.Stmts[0] == nil ||
				parsed.Stmts[0].Stmt == nil || parsed.Stmts[0].Stmt.GetSelectStmt() == nil {
				canonicalNativeViewEncodingsErr = &model.Error{Code: "XX000", Message: "canonical native view definition is invalid"}
				return
			}
			encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&zpgparse.RawStmt{Stmt: parsed.Stmts[0].Stmt})
			if err != nil {
				canonicalNativeViewEncodingsErr = &model.Error{Code: "XX000", Message: "canonical native view definition cannot be encoded"}
				return
			}
			canonicalNativeViewEncodings[source.Relation] = encoded
		}
	})
	if canonicalNativeViewEncodingsErr != nil {
		return nil, canonicalNativeViewEncodingsErr
	}
	encoded, ok := canonicalNativeViewEncodings[id]
	if !ok {
		return nil, &model.Error{Code: "XX000", Message: "canonical native view definition is unavailable"}
	}
	return encoded, nil
}

func validateNativeViewSemanticEncoding(id RelationID, encoded []byte) error {
	canonical, err := canonicalNativeViewSemanticEncoding(id)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, canonical) {
		return manifestContentError("native view semantic encoding does not match this build")
	}
	return nil
}

// EncodeNativeCatalogManifest writes the inventory in one deterministic record
// bounded by maxBytes. It never partially encodes: a limit breach returns no
// payload.
func EncodeNativeCatalogManifest(manifest NativeCatalogManifest, maxBytes uint64) ([]byte, error) {
	if maxBytes == 0 {
		return nil, manifestLimitError()
	}
	domainDefinitions := builtin.InformationDomains()
	functionDefinitions := builtin.InformationSchemaFunctions()
	relations := All()
	if len(manifest.Config.Relations) != len(relations) ||
		len(manifest.Config.InformationDomains) != len(domainDefinitions) ||
		len(manifest.Config.InformationSchemaFunctions) != len(functionDefinitions) {
		return nil, manifestContentError("inventory is incomplete")
	}
	if manifest.InformationSchema.ObjectID == 0 || manifest.InformationSchema.OID == 0 || manifest.InformationSchema.OID == 11 || manifest.PgCatalogObjectID == 0 {
		return nil, manifestContentError("namespace identity is unavailable")
	}
	views := make(map[RelationID]BoundViewDefinition, len(manifest.Config.ViewDefinitions))
	for id, definition := range manifest.Config.ViewDefinitions {
		views[id] = definition
	}
	writer := &manifestWriter{limit: maxBytes}
	writer.raw([]byte(nativeCatalogManifestHeader))
	writer.u32(NativeCatalogManifestVersion)
	writer.u64(uint64(manifest.InformationSchema.ObjectID))
	writer.u32(uint32(manifest.InformationSchema.OID))
	writer.u64(uint64(manifest.PgCatalogObjectID))
	writer.u32(uint32(len(relations)))
	for _, relation := range relations {
		identity, ok := manifest.Config.Relations[relation.ID]
		if !ok {
			return nil, manifestContentError("relation identity is unavailable")
		}
		writer.u32(uint32(relation.ID))
		writer.u32(uint32(identity.RelationOID))
		writer.u32(uint32(identity.RowTypeOID))
		writer.u32(uint32(identity.ArrayTypeOID))
	}
	writer.u32(uint32(len(domainDefinitions)))
	for _, definition := range domainDefinitions {
		identity, ok := manifest.Config.InformationDomains[definition.Kind]
		if !ok {
			return nil, manifestContentError("information domain identity is unavailable")
		}
		writer.u8(uint8(definition.Kind))
		writer.u32(uint32(identity.TypeOID))
		writer.u32(uint32(identity.ArrayTypeOID))
		writer.u32(uint32(identity.ConstraintOID))
	}
	writer.u32(uint32(len(functionDefinitions)))
	for _, definition := range functionDefinitions {
		oid, ok := manifest.Config.InformationSchemaFunctions[definition.Operation]
		if !ok {
			return nil, manifestContentError("information schema function identity is unavailable")
		}
		writer.u8(uint8(definition.Operation))
		writer.u32(uint32(oid))
	}
	viewCount := 0
	for _, relation := range relations {
		if logicalRelationDeclaration(relation) == DeclarationView {
			viewCount++
		}
	}
	writer.u32(uint32(viewCount))
	for _, relation := range relations {
		if logicalRelationDeclaration(relation) != DeclarationView {
			continue
		}
		definition, ok := views[relation.ID]
		if !ok {
			return nil, manifestContentError("native view definition is unavailable")
		}
		if err := validateNativeViewSemanticEncoding(relation.ID, definition.Semantic.Encoding); err != nil {
			return nil, err
		}
		writer.u32(uint32(relation.ID))
		writer.raw(definition.RegistryVersion[:])
		writer.u32(definition.Semantic.SemanticVersion)
		writer.u32(uint32(len(definition.Semantic.Encoding)))
		writer.raw(definition.Semantic.Encoding)
		if uint64(len(definition.Semantic.Dependencies)) > uint64(math.MaxUint32) {
			return nil, manifestContentError("native view dependency limit exceeded")
		}
		writer.u32(uint32(len(definition.Semantic.Dependencies)))
		for _, dependency := range definition.Semantic.Dependencies {
			writer.u64(uint64(dependency.Object))
			writer.u64(dependency.Version)
			writer.u32(uint32(dependency.Column))
		}
	}
	if writer.fail != nil {
		return nil, writer.fail
	}
	return writer.out, nil
}

// DecodeNativeCatalogManifest parses one durable record. Every identity must be
// present, distinct, and consistent with the pinned logical inventory; missing
// or unknown entries are errors, never defaults.
func DecodeNativeCatalogManifest(payload []byte, maxBytes uint64) (NativeCatalogManifest, error) {
	if maxBytes == 0 || uint64(len(payload)) > maxBytes {
		return NativeCatalogManifest{}, manifestLimitError()
	}
	reader := &manifestReader{in: payload, limit: maxBytes}
	header := reader.take(uint64(len(nativeCatalogManifestHeader)))
	if reader.fail != nil || len(header) != len(nativeCatalogManifestHeader) || string(header) != nativeCatalogManifestHeader {
		return NativeCatalogManifest{}, manifestTruncatedError()
	}
	if version := reader.u32(); reader.fail != nil || version != NativeCatalogManifestVersion {
		return NativeCatalogManifest{}, &model.Error{Code: "XX001", Message: "unsupported native catalog manifest version", Detail: "record is not readable by this build"}
	}
	manifest := NativeCatalogManifest{}
	manifest.InformationSchema.ObjectID = model.ObjectID(reader.u64())
	manifest.InformationSchema.OID = model.OID(reader.u32())
	manifest.PgCatalogObjectID = model.ObjectID(reader.u64())
	if reader.fail != nil {
		return NativeCatalogManifest{}, reader.fail
	}
	if manifest.InformationSchema.ObjectID == 0 || manifest.InformationSchema.OID == 0 || manifest.InformationSchema.OID == 11 || manifest.PgCatalogObjectID == 0 {
		return NativeCatalogManifest{}, manifestContentError("namespace identity is unavailable")
	}
	relations := All()
	if count := reader.u32(); reader.fail != nil || uint64(count) != uint64(len(relations)) {
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		return NativeCatalogManifest{}, manifestContentError("relation inventory does not match this build")
	}
	manifest.Config.Relations = make(map[RelationID]RelationIdentity, len(relations))
	for index := range relations {
		rawID := reader.u32()
		identity := RelationIdentity{
			RelationOID:  model.OID(reader.u32()),
			RowTypeOID:   model.OID(reader.u32()),
			ArrayTypeOID: model.OID(reader.u32()),
		}
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		if rawID > uint32(^RelationID(0)) || rawID != uint32(relations[index].ID) {
			return NativeCatalogManifest{}, manifestContentError("relation inventory is out of order")
		}
		id := RelationID(rawID)
		manifest.Config.Relations[id] = identity
	}
	domainDefinitions := builtin.InformationDomains()
	if count := reader.u32(); reader.fail != nil || uint64(count) != uint64(len(domainDefinitions)) {
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		return NativeCatalogManifest{}, manifestContentError("information domain inventory does not match this build")
	}
	manifest.Config.InformationDomains = make(map[builtin.InformationDomainKind]InformationDomainIdentity, len(domainDefinitions))
	for index := range domainDefinitions {
		kind := builtin.InformationDomainKind(reader.u8())
		identity := InformationDomainIdentity{
			TypeOID:       model.OID(reader.u32()),
			ArrayTypeOID:  model.OID(reader.u32()),
			ConstraintOID: model.OID(reader.u32()),
		}
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		if kind != domainDefinitions[index].Kind {
			return NativeCatalogManifest{}, manifestContentError("information domain inventory is out of order")
		}
		manifest.Config.InformationDomains[kind] = identity
	}
	functionDefinitions := builtin.InformationSchemaFunctions()
	if count := reader.u32(); reader.fail != nil || uint64(count) != uint64(len(functionDefinitions)) {
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		return NativeCatalogManifest{}, manifestContentError("information schema function inventory does not match this build")
	}
	manifest.Config.InformationSchemaFunctions = make(map[builtin.InformationSchemaFunction]model.OID, len(functionDefinitions))
	for index := range functionDefinitions {
		operation := builtin.InformationSchemaFunction(reader.u8())
		oid := model.OID(reader.u32())
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		if operation != functionDefinitions[index].Operation {
			return NativeCatalogManifest{}, manifestContentError("information schema function inventory is out of order")
		}
		manifest.Config.InformationSchemaFunctions[operation] = oid
	}
	viewCount := 0
	for _, relation := range relations {
		if logicalRelationDeclaration(relation) == DeclarationView {
			viewCount++
		}
	}
	if count := reader.u32(); reader.fail != nil || uint64(count) != uint64(viewCount) {
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		return NativeCatalogManifest{}, manifestContentError("native view inventory is incomplete")
	}
	manifest.Config.ViewDefinitions = make(map[RelationID]BoundViewDefinition, viewCount)
	for _, relation := range relations {
		if logicalRelationDeclaration(relation) != DeclarationView {
			continue
		}
		rawID := reader.u32()
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		if rawID > uint32(^RelationID(0)) || rawID != uint32(relation.ID) {
			return NativeCatalogManifest{}, manifestContentError("native view inventory is out of order")
		}
		id := RelationID(rawID)
		definition := BoundViewDefinition{Relation: id}
		copy(definition.RegistryVersion[:], reader.raw(uint64(len(definition.RegistryVersion))))
		definition.Semantic.SemanticVersion = reader.u32()
		encoding := reader.raw(uint64(reader.u32()))
		definition.Semantic.Encoding = encoding
		dependencies := reader.u32()
		if reader.fail != nil {
			return NativeCatalogManifest{}, reader.fail
		}
		if definition.Semantic.SemanticVersion != NativeViewSemanticVersion || len(encoding) == 0 {
			return NativeCatalogManifest{}, manifestContentError("native view definition is invalid")
		}
		if err := validateNativeViewSemanticEncoding(id, encoding); err != nil {
			return NativeCatalogManifest{}, err
		}
		const dependencyWireBytes = uint64(8 + 8 + 4)
		dependencyCount := uint64(dependencies)
		remaining := uint64(len(reader.in)) - reader.at
		if dependencyCount > remaining/dependencyWireBytes {
			return NativeCatalogManifest{}, manifestTruncatedError()
		}
		dependencyBytes := uint64(unsafe.Sizeof(model.Dependency{}))
		if dependencyCount > reader.limit/dependencyBytes {
			return NativeCatalogManifest{}, manifestLimitError()
		}
		definition.Semantic.Dependencies = make([]model.Dependency, int(dependencies))
		for index := range definition.Semantic.Dependencies {
			object := model.ObjectID(reader.u64())
			version := reader.u64()
			column := model.ColumnID(reader.u32())
			if reader.fail != nil {
				return NativeCatalogManifest{}, reader.fail
			}
			if object == 0 || version == 0 {
				return NativeCatalogManifest{}, manifestContentError("native view dependency identity is invalid")
			}
			definition.Semantic.Dependencies[index] = model.Dependency{Object: object, Version: version, Column: column}
		}
		manifest.Config.ViewDefinitions[id] = definition
	}
	if reader.fail != nil {
		return NativeCatalogManifest{}, reader.fail
	}
	if reader.at != uint64(len(payload)) {
		return NativeCatalogManifest{}, manifestContentError("record has trailing bytes")
	}
	return manifest, nil
}
