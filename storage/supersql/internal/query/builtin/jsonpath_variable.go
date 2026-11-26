package builtin

import (
	"context"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// JSONPathVariable is borrowed for one ExecuteJSONPathTyped call. Layout carries
// the SQL identity that is intentionally not recoverable from Value alone.
type JSONPathVariable struct {
	Name   string
	Layout model.TypeLayout
	Value  model.Value
}

type jsonPathVariableItem struct {
	name     string
	item     jsonPathItem
	document jsonbDocument
}

func resolveJSONPathVariables(ctx context.Context, variables []JSONPathVariable, budget CallBudget, options ResolveOptions) ([]jsonPathVariableItem, []columnar.Allocation, CallBudget, error) {
	items := make([]jsonPathVariableItem, len(variables))
	claims := make([]columnar.Allocation, 0, len(variables))
	release := func() {
		for _, claim := range claims {
			if claim != nil {
				claim.Release()
			}
		}
	}
	for i, variable := range variables {
		if err := ctx.Err(); err != nil {
			release()
			return nil, nil, CallBudget{}, err
		}
		if variable.Name == "" {
			release()
			return nil, nil, CallBudget{}, sqlError("42601", "SQL/JSON PASSING variable name is empty")
		}
		for j := range i {
			if variables[j].Name == variable.Name {
				release()
				return nil, nil, CallBudget{}, sqlError("42710", "duplicate SQL/JSON PASSING variable name")
			}
		}
		if err := validateConcreteLayout(ctx, variable.Layout); err != nil {
			release()
			return nil, nil, CallBudget{}, err
		}
		if err := validateJSONPathVariableValue(ctx, variable.Value, variable.Layout, 0); err != nil {
			release()
			return nil, nil, CallBudget{}, err
		}
		items[i].name = variable.Name
		if variable.Value.IsNull {
			items[i].item = jsonPathItem{kind: jsonTextNull, text: "null"}
			continue
		}
		switch variable.Layout.Type.ID {
		case 1082, 1083, 1114, 1184, 1266:
			items[i].item = jsonPathItem{kind: jsonTextString, temporal: variable.Value, typeID: variable.Layout.Type.ID}
			continue
		}
		switch variable.Value.Type {
		case model.TypeBoolean:
			text := "false"
			if variable.Value.IntVal != 0 {
				text = "true"
			}
			items[i].item = jsonPathItem{kind: jsonTextBoolean, text: text, boolean: variable.Value.IntVal != 0}
			continue
		case model.TypeInt64:
			items[i].item = jsonPathItem{kind: jsonTextNumber, text: strconv.FormatInt(variable.Value.IntVal, 10)}
			continue
		case model.TypeString:
			items[i].item = jsonPathItem{kind: jsonTextString, text: variable.Value.StrVal}
			continue
		}
		encoder, err := ResolveJSONValue(ctx, variable.Layout, options, budget.MemoryBytes)
		if err != nil {
			release()
			return nil, nil, CallBudget{}, err
		}
		encoded, err := encoder.EncodeValue(ctx, variable.Value, JSONCompact, budget)
		if err != nil {
			release()
			return nil, nil, CallBudget{}, err
		}
		if int64(len(encoded.StrVal)) > budget.OutputBytes || uint64(len(encoded.StrVal)) > budget.MemoryBytes {
			release()
			return nil, nil, CallBudget{}, sqlError("54000", "SQL/JSON PASSING value exceeds budget")
		}
		document, claim, remaining, err := claimJSONBDocument(ctx, encoded.StrVal, budget)
		if err != nil {
			release()
			return nil, nil, CallBudget{}, err
		}
		claims = append(claims, claim)
		items[i].document = document
		items[i].item = jsonPathNodeItem(&items[i].document, 0)
		budget = remaining
	}
	return items, claims, budget, nil
}

func validateJSONPathVariableValue(ctx context.Context, value model.Value, layout model.TypeLayout, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 || value.Type != layout.Type.Storage {
		return sqlError("42804", "SQL/JSON PASSING value differs from its declared type")
	}
	if value.IsNull {
		return nil
	}
	switch value.Type {
	case model.TypeBoolean:
		if value.IntVal != 0 && value.IntVal != 1 {
			return sqlError("42804", "invalid boolean SQL/JSON PASSING value")
		}
	case model.TypeInt64:
		if layout.Type.ID == 21 && (value.IntVal < math.MinInt16 || value.IntVal > math.MaxInt16) || layout.Type.ID == 23 && (value.IntVal < math.MinInt32 || value.IntVal > math.MaxInt32) {
			return sqlError("22003", "SQL/JSON PASSING integer is out of range")
		}
	case model.TypeArray:
		if len(layout.Children) != 1 {
			return sqlError("42804", "SQL/JSON PASSING array layout is invalid")
		}
		if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
			return err
		}
		for _, element := range value.ArrayVal {
			if err := validateJSONPathVariableValue(ctx, element, layout.Children[0], depth+1); err != nil {
				return err
			}
		}
	case model.TypeComposite:
		if len(value.ArrayVal) != len(layout.Children) {
			return sqlError("42804", "SQL/JSON PASSING row width differs from its declared type")
		}
		for i, field := range value.ArrayVal {
			if err := validateJSONPathVariableValue(ctx, field, layout.Children[i], depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
