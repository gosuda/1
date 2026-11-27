package bind

import (
	"strconv"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) expandCompositeStar(node *zpgparse.Node, s *scope) ([]expression, []string, bool, error) {
	indirection := node.GetAIndirection()
	if indirection == nil || len(indirection.Indirection) == 0 || indirection.Indirection[len(indirection.Indirection)-1].GetAStar() == nil {
		return nil, nil, false, nil
	}
	parent := indirection.Arg
	if len(indirection.Indirection) > 1 {
		parent = &zpgparse.Node{Node: &zpgparse.Node_AIndirection{AIndirection: &zpgparse.A_Indirection{
			Arg: parent, Indirection: indirection.Indirection[:len(indirection.Indirection)-1],
		}}}
	}
	if parent == nil {
		return nil, nil, true, bindError("42601", "invalid indirection", 0)
	}
	aggregates, aggregateColumns := len(s.aggregates), len(s.aggregateColumns)
	groupings, groupingColumns, windows := len(s.groupings), len(s.groupingColumns), len(s.windows)
	source, err := b.scalar(parent, s)
	if err != nil {
		return nil, nil, true, err
	}
	if source.typ.Type.Storage != model.TypeComposite {
		return nil, nil, true, bindError("42809", "type is not composite", source.position)
	}
	if source.typ.Type.ID == 2249 && source.record != recordConstructor && source.record != recordDeclared {
		return nil, nil, true, bindError("42809", "record type has not been registered", source.position)
	}
	count := len(source.typ.Children)
	if err := b.checkStarExpansion(count, source.position); err != nil {
		return nil, nil, true, err
	}
	if count == 0 {
		clear(s.aggregates[aggregates:])
		s.aggregates = s.aggregates[:aggregates]
		clear(s.aggregateColumns[aggregateColumns:])
		s.aggregateColumns = s.aggregateColumns[:aggregateColumns]
		clear(s.groupings[groupings:])
		s.groupings = s.groupings[:groupings]
		clear(s.groupingColumns[groupingColumns:])
		s.groupingColumns = s.groupingColumns[:groupingColumns]
		clear(s.windows[windows:])
		s.windows = s.windows[:windows]
	}
	var fields []model.ColumnDescriptor
	if source.typ.Type.ID != 2249 && source.recordFields == nil {
		fields, err = b.namedCompositeFields(source.typ, source.position)
		if err != nil {
			return nil, nil, true, err
		}
	}
	values := make([]expression, count)
	names := make([]string, count)
	for i := range values {
		if err := b.ctx.Err(); err != nil {
			return nil, nil, true, err
		}
		name := ""
		if source.recordFields != nil {
			name = source.recordFields[i].Name
		} else if fields != nil {
			name = fields[i].Name
		} else {
			name = "f" + strconv.Itoa(i+1)
		}
		// PostgreSQL expands f().* into independent calls, not one shared record.
		parentValue := source
		if i != 0 {
			parentValue, err = b.scalar(parent, s)
			if err != nil {
				return nil, nil, true, err
			}
		}
		values[i], err = b.compositeFieldAt(s, parentValue, i, source.position)
		if err != nil {
			return nil, nil, true, err
		}
		names[i] = name
	}
	return values, names, true, nil
}

func (b *binder) checkStarExpansion(count int, pos int) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	limits := b.options.Limits
	// Include append growth, target metadata, ROW child layouts and generated names.
	const fieldBytes = uint64(3*unsafe.Sizeof(expression{}) + 2*unsafe.Sizeof(column{}) + 2*unsafe.Sizeof(model.TypeLayout{}) + unsafe.Sizeof((*zpgparse.Node)(nil)) + unsafe.Sizeof("") + 11)
	if count < 0 || uint64(count) > uint64(limits.MaxNodes) || uint64(count) > uint64(limits.MaxOperands) || uint64(count) > uint64(limits.MaxSlots) || uint64(count) > limits.MaxBytes/fieldBytes {
		return bindError("54000", "star expansion limit exceeded", pos)
	}
	return nil
}

func (b *binder) rowArguments(nodes []*zpgparse.Node, s *scope, pos int) ([]expression, error) {
	if err := b.checkStarExpansion(len(nodes), pos); err != nil {
		return nil, err
	}
	args := make([]expression, 0, len(nodes))
	for _, node := range nodes {
		if ref := starReference(node); ref != nil {
			var err error
			args, err = b.appendRowStar(args, ref, s)
			if err != nil {
				return nil, err
			}
			continue
		}
		expanded, _, star, err := b.expandCompositeStar(node, s)
		if err != nil {
			return nil, err
		}
		if star {
			if err := b.checkStarExpansion(len(args)+len(expanded), pos); err != nil {
				return nil, err
			}
			args = append(args, expanded...)
			continue
		}
		if err := b.checkStarExpansion(len(args)+1, pos); err != nil {
			return nil, err
		}
		value, err := b.scalar(node, s)
		if err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	return args, nil
}

func (b *binder) appendRowStar(args []expression, ref *zpgparse.ColumnRef, s *scope) ([]expression, error) {
	pos := int(ref.Location) + 1
	if len(ref.Fields) == 1 {
		return nil, bindError("42601", "row star requires a relation name", pos)
	}
	qualifier, err := starQualifier(ref, true)
	if err != nil {
		return nil, err
	}
	owner, err := s.starScope(qualifier, pos)
	if err != nil {
		return nil, err
	}
	for _, c := range owner.columns {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		if b.nodes >= b.options.Limits.MaxNodes {
			return nil, bindError("54000", "star expansion work limit exceeded", pos)
		}
		b.nodes++
		if !c.visibleToStar(qualifier) {
			continue
		}
		if err := b.checkStarExpansion(len(args)+1, pos); err != nil {
			return nil, err
		}
		value, err := b.reference(s, owner.starColumn(s, c), pos)
		if err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	return args, nil
}
