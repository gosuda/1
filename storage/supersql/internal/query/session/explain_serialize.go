package session

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

type explainSerializer struct {
	columns []bind.ResultColumn
	text    []builtin.TypeTextCodec
	binary  []builtin.TypeBinaryCodec
	budget  builtin.CallBudget
	maximum uint64
	bytes   uint64
}

func newExplainSerializer(ctx context.Context, prepared *Prepared, environment exec.Environment, mode bind.ExplainSerialize) (*explainSerializer, error) {
	if mode == bind.ExplainNoSerialization {
		return nil, nil
	}
	columns := prepared.Columns()
	maximum := environment.Limits.BatchBytes
	serializer := &explainSerializer{
		columns: columns,
		budget: builtin.CallBudget{
			OutputBytes: int64(min(maximum, uint64(math.MaxInt64))),
			MemoryBytes: maximum,
			Temporal:    environment.Temporal,
			Relations:   environment.Relations,
			Settings:    environment.Settings,
			TextSearch:  environment.TextSearch,
			Advisory:    environment.Advisory,
			Native:      environment.Native,
		},
		maximum: maximum,
	}
	remaining := environment.Limits.MetadataBytes
	switch mode {
	case bind.ExplainSerializeText:
		serializer.text = make([]builtin.TypeTextCodec, len(columns))
		for index, column := range columns {
			codec, err := builtin.ResolveTypeTextCodec(ctx, column.Layout, environment.Builtin, remaining)
			if err != nil {
				return nil, err
			}
			if codec.OwnedBytes > remaining {
				return nil, settingError("54000", "EXPLAIN serialization metadata limit exceeded")
			}
			remaining -= codec.OwnedBytes
			serializer.text[index] = codec
		}
	case bind.ExplainSerializeBinary:
		serializer.binary = make([]builtin.TypeBinaryCodec, len(columns))
		for index, column := range columns {
			codec, err := builtin.ResolveTypeBinaryCodec(ctx, column.Layout, environment.Builtin, remaining)
			if err != nil {
				return nil, err
			}
			if codec.OwnedBytes > remaining {
				return nil, settingError("54000", "EXPLAIN serialization metadata limit exceeded")
			}
			remaining -= codec.OwnedBytes
			serializer.binary[index] = codec
		}
	default:
		return nil, errPreparedInvocation
	}
	return serializer, nil
}

func (serializer *explainSerializer) consume(ctx context.Context, batch *columnar.Batch) error {
	if serializer == nil {
		return nil
	}
	if batch == nil || len(batch.Columns) != len(serializer.columns) {
		return errPreparedInvocation
	}
	for row := range batch.Rows() {
		lane := batch.Lane(row)
		if serializer.binary != nil {
			if err := serializer.add(2); err != nil {
				return err
			}
		}
		for column := range batch.Columns {
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[column], lane, serializer.maximum)
			if err != nil {
				return err
			}
			if value.Type != serializer.columns[column].Layout.Type.Storage {
				return settingError("42804", "EXPLAIN serialization value differs from prepared column type")
			}
			if serializer.binary != nil {
				if err := serializer.add(4); err != nil {
					return err
				}
				if value.IsNull {
					continue
				}
				result, err := serializer.binary[column].Output(ctx, []model.Value{value}, serializer.budget)
				if err != nil {
					return err
				}
				if result.Type != model.TypeBytes || result.IsNull {
					return settingError("42804", "EXPLAIN binary serialization returned an invalid value")
				}
				if err := serializer.add(uint64(len(result.BytesVal))); err != nil {
					return err
				}
				continue
			}
			if value.IsNull {
				if column+1 != len(batch.Columns) {
					if err := serializer.add(1); err != nil {
						return err
					}
				}
				continue
			}
			result, err := serializer.text[column].Output(ctx, []model.Value{value}, serializer.budget)
			if err != nil {
				return err
			}
			if result.Type != model.TypeString || result.IsNull {
				return settingError("42804", "EXPLAIN text serialization returned an invalid value")
			}
			if err := serializer.add(uint64(len(result.StrVal))); err != nil {
				return err
			}
			if column+1 != len(batch.Columns) {
				if err := serializer.add(1); err != nil {
					return err
				}
			}
		}
		if serializer.text != nil {
			if err := serializer.add(1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (serializer *explainSerializer) add(value uint64) error {
	if value > math.MaxUint64-serializer.bytes {
		return settingError("54000", "EXPLAIN serialized byte count overflow")
	}
	serializer.bytes += value
	return nil
}
