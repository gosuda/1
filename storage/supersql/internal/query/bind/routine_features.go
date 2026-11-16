package bind

import (
	"strings"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func routineOptionsFeature(options []*zpgparse.Node) FeatureID {
	for _, option := range options {
		definition := option.GetDefElem()
		if definition == nil {
			continue
		}
		switch definition.Defname {
		case "language":
			language := definition.Arg.GetString_().GetSval()
			if !strings.EqualFold(language, "sql") && !strings.EqualFold(language, "plpgsql") {
				return ExternalCode
			}
		case "security":
			if definition.Arg.GetBoolean().GetBoolval() {
				return Authorization
			}
		case "transform":
			return ExternalCode
		}
	}
	return ""
}
