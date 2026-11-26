package bind

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"reflect"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// Version 3 binds timezone context and the pinned timezone-data identity.
const indexExpressionSemanticVersion = 3

type indexExpressionEncoding struct {
	Format           string
	Version          uint32
	PostgreSQL       int
	Table            model.ObjectID
	TimeZoneData     string
	TableVersion     uint64
	SearchPath       []string
	Collation        model.CollationID
	DefaultCollation model.CollationID
	Builtin          builtin.ResolveOptions
	Predicate        bool
	Columns          []IndexExpressionParameter
	Fingerprint      [sha256.Size]byte
	AST              []byte
	Target           *model.TypeLayout        `json:",omitempty"`
	TargetColumn     model.ColumnID           `json:",omitempty"`
	ConstraintID     model.ObjectID           `json:",omitempty"`
	Identities       []routineDefaultIdentity `json:",omitempty"`
}

func encodeIndexExpression(ctx context.Context, encoding indexExpressionEncoding, limits ir.Limits) ([]byte, error) {
	encoding.TimeZoneData = builtin.TimeZoneDataSHA256
	if err := checkIndexProto(ctx, encoding.AST, limits); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(encoding)
	if err != nil {
		return nil, err
	}
	if err := checkIndexJSON(ctx, encoded, limits); err != nil {
		return nil, err
	}
	return encoded, nil
}

func decodeRowExpression(ctx context.Context, encoded []byte, limits ir.Limits, format string, version uint32) (indexExpressionEncoding, *zpgparse.Node, error) {
	var result indexExpressionEncoding
	if _, err := ir.NewBuilder(limits); err != nil {
		return result, nil, err
	}
	if err := checkIndexJSON(ctx, encoded, limits); err != nil {
		return result, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, nil, indexEncodingError(err)
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return result, nil, indexEncodingError(err)
	}
	if result.Format != format || result.Version != version || result.PostgreSQL != PostgreSQLVersion {
		return result, nil, missing("index expression semantic encoding version", 0)
	}
	if result.TimeZoneData != builtin.TimeZoneDataSHA256 {
		return result, nil, bindError("0A000", "index expression uses a different timezone data revision", 0)
	}
	if result.Table == 0 || result.TableVersion == 0 || uint64(len(result.Columns)) > uint64(limits.MaxOperands) || uint64(len(result.Identities)) > uint64(limits.MaxOperands) {
		return result, nil, indexEncodingError(nil)
	}
	if err := checkIndexProto(ctx, result.AST, limits); err != nil {
		return result, nil, err
	}
	ast := &zpgparse.Node{}
	if err := (proto.UnmarshalOptions{RecursionLimit: int(limits.MaxDepth) + 1}).Unmarshal(result.AST, ast); err != nil {
		return result, nil, indexEncodingError(err)
	}
	return result, ast, nil
}

// Token admission precedes decoding typed slices. Each token reserves a generous
// metadata header allowance; source bytes and decoded metadata have separate caps.
func checkIndexJSON(ctx context.Context, encoded []byte, limits ir.Limits) error {
	if len(encoded) == 0 || uint64(len(encoded)) > limits.MaxBytes {
		return bindError("54000", "index expression encoding byte limit exceeded", 0)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	depth, roots := 0, 0
	remaining := limits.MaxBytes
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			if depth != 0 || roots != 1 {
				return indexEncodingError(nil)
			}
			return nil
		}
		if err != nil {
			return indexEncodingError(err)
		}
		cost := uint64(256)
		if text, ok := token.(string); ok {
			cost += uint64(len(text)) * 3
		}
		if cost > remaining {
			return bindError("54000", "index expression decoded metadata limit exceeded", 0)
		}
		remaining -= cost
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			if depth == 0 {
				return indexEncodingError(nil)
			}
			continue
		}
		switch delimiter {
		case '{', '[':
			if depth == 0 && delimiter != '{' {
				return indexEncodingError(nil)
			}
			if depth == 0 {
				roots++
			}
			depth++
			if depth > int(limits.MaxDepth)+8 {
				return bindError("54000", "index expression metadata depth limit exceeded", 0)
			}
		case '}', ']':
			depth--
		}
	}
}

// The protobuf wire walk admits generated message headers, repeated-field growth
// and copied payloads before Unmarshal can allocate them. Unknown fields fail shut.
func checkIndexProto(ctx context.Context, encoded []byte, limits ir.Limits) error {
	if len(encoded) == 0 || uint64(len(encoded)) > limits.MaxBytes {
		return bindError("54000", "index expression AST byte limit exceeded", 0)
	}
	remaining, nodes := limits.MaxBytes, limits.MaxNodes
	return admitIndexProto(ctx, encoded, (&zpgparse.Node{}).ProtoReflect().Descriptor(), 0, limits, &remaining, &nodes)
}

func admitIndexProto(ctx context.Context, encoded []byte, descriptor protoreflect.MessageDescriptor, depth uint32, limits ir.Limits, remaining *uint64, nodes *uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > limits.MaxDepth || *nodes == 0 {
		return bindError("54000", "index expression AST limit exceeded", 0)
	}
	*nodes--
	messageType, err := protoregistry.GlobalTypes.FindMessageByName(descriptor.FullName())
	if err != nil {
		return indexEncodingError(err)
	}
	cost := uint64(reflect.TypeOf(messageType.Zero().Interface()).Elem().Size()) * 3
	if cost > *remaining {
		return bindError("54000", "index expression decoded AST limit exceeded", 0)
	}
	*remaining -= cost
	for len(encoded) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		number, kind, n := protowire.ConsumeTag(encoded)
		if n < 0 {
			return indexEncodingError(protowire.ParseError(n))
		}
		encoded = encoded[n:]
		field := descriptor.Fields().ByNumber(protoreflect.FieldNumber(number))
		if field == nil || kind == protowire.StartGroupType || kind == protowire.EndGroupType {
			return indexEncodingError(nil)
		}
		if *remaining < 64 {
			return bindError("54000", "index expression decoded AST limit exceeded", 0)
		}
		*remaining -= 64
		if kind == protowire.BytesType {
			value, n := protowire.ConsumeBytes(encoded)
			if n < 0 {
				return indexEncodingError(protowire.ParseError(n))
			}
			if field.Kind() == protoreflect.MessageKind {
				if err := admitIndexProto(ctx, value, field.Message(), depth+1, limits, remaining, nodes); err != nil {
					return err
				}
			} else {
				multiplier := uint64(3)
				if field.IsPacked() {
					multiplier = 24
				}
				if uint64(len(value)) > *remaining/multiplier {
					return bindError("54000", "index expression AST payload limit exceeded", 0)
				}
				cost := uint64(len(value)) * multiplier
				*remaining -= cost
			}
			encoded = encoded[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(number, kind, encoded)
		if n < 0 {
			return indexEncodingError(protowire.ParseError(n))
		}
		encoded = encoded[n:]
	}
	return nil
}

func indexEncodingError(cause error) error {
	return &model.Error{Code: "22023", Message: "invalid index expression semantic encoding", Cause: cause}
}

func indexBindingFingerprint(ctx context.Context, graph *ir.Graph) ([sha256.Size]byte, error) {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for id := ir.LayoutID(1); uint32(id) <= graph.LayoutCount(); id++ {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		typ, _ := graph.Type(id)
		if err := encoder.Encode(typ); err != nil {
			return [sha256.Size]byte{}, err
		}
		for i := uint32(0); i < graph.LayoutChildCount(id); i++ {
			child, _ := graph.LayoutChild(id, i)
			if err := encoder.Encode(child); err != nil {
				return [sha256.Size]byte{}, err
			}
		}
	}
	for id := ir.SignatureID(1); uint32(id) <= graph.SignatureCount(); id++ {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		signature, _ := graph.Signature(id)
		if err := encoder.Encode(signature.Metadata()); err != nil {
			return [sha256.Size]byte{}, err
		}
		for i := uint32(0); i < signature.ParameterCount(); i++ {
			parameter, _ := graph.SignatureParameter(id, i)
			if err := encoder.Encode(parameter); err != nil {
				return [sha256.Size]byte{}, err
			}
		}
	}
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		scalar, _ := graph.Scalar(id)
		metadata := scalar.Metadata()
		metadata.Span = ir.Span{}
		if err := encoder.Encode(metadata); err != nil {
			return [sha256.Size]byte{}, err
		}
		for i := uint32(0); i < scalar.OperandCount(); i++ {
			operand, _ := graph.Operand(id, i)
			if err := encoder.Encode(operand); err != nil {
				return [sha256.Size]byte{}, err
			}
		}
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}
