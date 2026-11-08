package procedural

import (
	"context"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func expandTransitionRelations(ctx context.Context, raw *zpgparse.RawStmt, parameters []Parameter, trigger *TriggerContext) (*zpgparse.RawStmt, []Parameter, error) {
	if trigger == nil || raw == nil || trigger.OldTransitionName == "" && trigger.NewTransitionName == "" {
		return raw, parameters, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	owned := proto.Clone(raw).(*zpgparse.RawStmt)
	result := append([]Parameter(nil), parameters...)
	err := rewriteTransitionNodes(owned.Stmt, func(name string, alias *zpgparse.Alias) (*zpgparse.RangeSubselect, bool, error) {
		var rows []model.Row
		switch {
		case trigger.OldTransitionName != "" && strings.EqualFold(name, trigger.OldTransitionName):
			rows = trigger.OldTransition
		case trigger.NewTransitionName != "" && strings.EqualFold(name, trigger.NewTransitionName):
			rows = trigger.NewTransition
		default:
			return nil, false, nil
		}
		selectStatement, err := transitionSelect(&result, trigger.Layout, rows)
		if err != nil {
			return nil, false, err
		}
		if alias == nil {
			alias = &zpgparse.Alias{Aliasname: name}
		} else {
			alias = proto.Clone(alias).(*zpgparse.Alias)
		}
		if len(alias.Colnames) == 0 {
			for _, field := range trigger.Layout.FieldNames {
				alias.Colnames = append(alias.Colnames, &zpgparse.Node{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: field}}})
			}
		}
		return &zpgparse.RangeSubselect{Subquery: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: selectStatement}}, Alias: alias}, true, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return owned, result, nil
}

func transitionSelect(parameters *[]Parameter, layout model.TypeLayout, rows []model.Row) (*zpgparse.SelectStmt, error) {
	if len(layout.Children) == 0 || len(layout.Children) != len(layout.FieldNames) {
		return nil, ErrInvalidProgram
	}
	selectStatement := &zpgparse.SelectStmt{}
	if len(rows) == 0 {
		selectStatement.WhereClause = &zpgparse.Node{Node: &zpgparse.Node_Boolean{Boolean: &zpgparse.Boolean{Boolval: false}}}
		for _, column := range layout.Children {
			*parameters = append(*parameters, Parameter{Layout: column, Value: model.NullVal(column.Type.Storage)})
			ref := &zpgparse.Node{Node: &zpgparse.Node_ParamRef{ParamRef: &zpgparse.ParamRef{Number: int32(len(*parameters)), Location: -1}}}
			selectStatement.TargetList = append(selectStatement.TargetList, &zpgparse.Node{Node: &zpgparse.Node_ResTarget{ResTarget: &zpgparse.ResTarget{Val: ref, Location: -1}}})
		}
		return selectStatement, nil
	}
	for _, row := range rows {
		if len(row.Values) != len(layout.Children) {
			return nil, ErrInvalidProgram
		}
		items := make([]*zpgparse.Node, len(row.Values))
		for i, value := range row.Values {
			*parameters = append(*parameters, Parameter{Layout: layout.Children[i], Value: value})
			items[i] = &zpgparse.Node{Node: &zpgparse.Node_ParamRef{ParamRef: &zpgparse.ParamRef{Number: int32(len(*parameters)), Location: -1}}}
		}
		selectStatement.ValuesLists = append(selectStatement.ValuesLists, &zpgparse.Node{Node: &zpgparse.Node_List{List: &zpgparse.List{Items: items}}})
	}
	return selectStatement, nil
}

func rewriteTransitionNodes(node *zpgparse.Node, resolve func(string, *zpgparse.Alias) (*zpgparse.RangeSubselect, bool, error)) error {
	if node == nil {
		return nil
	}
	if relation := node.GetRangeVar(); relation != nil && relation.Catalogname == "" && relation.Schemaname == "" {
		replacement, found, err := resolve(relation.Relname, relation.Alias)
		if err != nil {
			return err
		}
		if found {
			node.Node = &zpgparse.Node_RangeSubselect{RangeSubselect: replacement}
			return nil
		}
	}
	return walkTransitionMessages(node.ProtoReflect(), func(child *zpgparse.Node) error { return rewriteTransitionNodes(child, resolve) })
}

func walkTransitionMessages(message protoreflect.Message, visit func(*zpgparse.Node) error) error {
	fields := message.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if !message.Has(field) || field.IsMap() || (field.Kind() != protoreflect.MessageKind && field.Kind() != protoreflect.GroupKind) {
			continue
		}
		value := message.Get(field)
		if field.IsList() {
			list := value.List()
			for j := 0; j < list.Len(); j++ {
				child := list.Get(j).Message()
				if node, ok := child.Interface().(*zpgparse.Node); ok {
					if err := visit(node); err != nil {
						return err
					}
				} else if err := walkTransitionMessages(child, visit); err != nil {
					return err
				}
			}
			continue
		}
		child := value.Message()
		if node, ok := child.Interface().(*zpgparse.Node); ok {
			if err := visit(node); err != nil {
				return err
			}
		} else if err := walkTransitionMessages(child, visit); err != nil {
			return err
		}
	}
	return nil
}
