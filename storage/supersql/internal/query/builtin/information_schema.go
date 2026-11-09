package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

type InformationDomainKind uint8

const (
	InformationCardinalNumber InformationDomainKind = iota + 1
	InformationCharacterData
	InformationSQLIdentifier
	InformationTimeStamp
	InformationYesOrNo
)
const InformationDomainCount = int(InformationYesOrNo)


type InformationDomainInfo struct {
	Kind           InformationDomainKind
	Name           string
	Layout         model.TypeLayout
	Base           model.TypeLayout
	NotNull        bool
	CheckSQL       string
	ConstraintName string
	DefaultSQL     string
}

func InformationDomain(name string) (InformationDomainInfo, bool) {
	for _, domain := range InformationDomains() {
		if domain.Name == name {
			return domain, true
		}
	}
	return InformationDomainInfo{}, false
}

func InformationDomainByKind(kind InformationDomainKind) (InformationDomainInfo, bool) {
	varchar := func(modifier int32) model.TypeLayout {
		return model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: modifier, Collation: 100}}
	}
	var info InformationDomainInfo
	switch kind {
	case InformationCardinalNumber:
		info = InformationDomainInfo{Kind: kind, Name: "cardinal_number", Base: model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}, CheckSQL: "VALUE >= 0", ConstraintName: "cardinal_number_domain_check"}
	case InformationCharacterData:
		info = InformationDomainInfo{Kind: kind, Name: "character_data", Base: varchar(-1)}
	case InformationSQLIdentifier:
		info = InformationDomainInfo{Kind: kind, Name: "sql_identifier", Base: model.TypeLayout{Type: model.Type{ID: 19, Storage: model.TypeString, Modifier: -1, Collation: 950}}}
	case InformationTimeStamp:
		info = InformationDomainInfo{Kind: kind, Name: "time_stamp", Base: model.TypeLayout{Type: model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: 2}}, DefaultSQL: "CURRENT_TIMESTAMP(2)"}
	case InformationYesOrNo:
		info = InformationDomainInfo{Kind: kind, Name: "yes_or_no", Base: varchar(7), CheckSQL: "VALUE IN ('YES', 'NO')", ConstraintName: "yes_or_no_check"}
	default:
		return InformationDomainInfo{}, false
	}
	info.Layout = info.Base
	info.Layout.Type.Modifier = -1
	return info, true
}

func InformationDomains() []InformationDomainInfo {
	kinds := [...]InformationDomainKind{InformationCardinalNumber, InformationCharacterData, InformationSQLIdentifier, InformationTimeStamp, InformationYesOrNo}
	result := make([]InformationDomainInfo, len(kinds))
	for i, kind := range kinds {
		result[i], _ = InformationDomainByKind(kind)
	}
	return result
}
