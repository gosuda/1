package bind

import (
	"strings"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func settingStatementFeature(statement *zpgparse.VariableSetStmt) FeatureID {
	switch statement.Name {
	case "TRANSACTION SNAPSHOT":
		return PreparedTransactions
	case "transaction_isolation", "default_transaction_isolation":
		if len(statement.Args) == 1 {
			return isolationValueFeature(statement.Args[0])
		}
	}
	feature, _ := builtinSettingFeature(strings.ToLower(statement.Name))
	return feature
}

func isolationValueFeature(value *zpgparse.Node) FeatureID {
	literal := value.GetAConst()
	if literal == nil || literal.Isnull {
		return ""
	}
	switch strings.ToLower(literal.GetSval().GetSval()) {
	case "repeatable read", "read committed", "read uncommitted":
		return WeakerIsolation
	}
	return ""
}
