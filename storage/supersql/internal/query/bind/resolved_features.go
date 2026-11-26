package bind

import (
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

//go:generate go run ./internal/genfeatures -manifest ../../../testdata/postgres/embedded_features.json -bind-output builtin_features_generated.go -builtin-output ../builtin/signatures_generated.go -catalog-output ../builtin/catalog_generated.go -opclass-output ../builtin/opclass_generated.go -row-comparison-output ../builtin/row_comparison_generated.go -parameter-output ../builtin/function_parameters_generated.go -range-output ../builtin/range_catalog_generated.go

// ValidateBuiltinFunction is called only after resolution identifies a builtin.
// User-defined functions with the same name are not subject to this inventory.
func ValidateBuiltinFunction(id model.FunctionID, position int) error {
	if feature := builtinFunctionFeature(id); feature != "" {
		return unsupported(feature, position)
	}
	return nil
}

func ValidateBuiltinType(id model.TypeID, position int) error {
	if feature := builtinTypeFeature(id); feature != "" {
		return unsupported(feature, position)
	}
	return nil
}

func ValidateBuiltinCollation(id model.CollationID, position int) error {
	if feature := builtinCollationFeature(id); feature != "" {
		return unsupported(feature, position)
	}
	return nil
}

// ValidateBuiltinRelation checks a resolved system relation's qualified identity,
// never an unqualified source spelling or a user-owned namespace.
func ValidateBuiltinRelation(qualifiedName string, position int) error {
	if feature := builtinRelationFeature(qualifiedName); feature != "" {
		return unsupported(feature, position)
	}
	return nil
}

// IsCustomSettingName reports whether name is a valid PostgreSQL custom GUC
// parameter name: two or more simple ASCII identifiers separated by dots,
// within the 63-byte identifier limit.
func IsCustomSettingName(name string) bool {
	if len(name) == 0 || len(name) > 63 || strings.IndexByte(name, '.') < 0 {
		return false
	}
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 {
			return false
		}
		for i := range len(part) {
			ch := part[i]
			letter := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
			digit := ch >= '0' && ch <= '9'
			if i == 0 && !letter {
				return false
			}
			if !letter && !digit {
				return false
			}
		}
	}
	return true
}

// ValidateSetting distinguishes recognized but excluded PostgreSQL settings from
// unknown names and custom qualified GUC names. Value and transaction-state
// checks belong to the session.
func ValidateSetting(name string, position int) error {
	feature, known := builtinSettingFeature(strings.ToLower(name))
	if !known {
		if IsCustomSettingName(name) {
			return nil
		}
		if strings.IndexByte(name, '.') >= 0 {
			return &model.Error{
				Code:     "42602",
				Message:  "invalid configuration parameter name \"" + name + "\"",
				Detail:   "Custom parameter names must be two or more simple identifiers separated by dots.",
				Position: position,
			}
		}
		return &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: name, Position: position}
	}
	if feature != "" {
		return unsupported(feature, position)
	}
	return nil
}

func ValidateIsolation(value string, position int) error {
	switch strings.ToLower(value) {
	case "serializable":
		return nil
	case "repeatable read", "read committed", "read uncommitted":
		return unsupported(WeakerIsolation, position)
	default:
		return &model.Error{Code: "22023", Message: "invalid transaction isolation level", Detail: value, Position: position}
	}
}
