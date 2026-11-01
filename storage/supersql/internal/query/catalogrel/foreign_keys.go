package catalogrel

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type catalogForeignKey struct {
	foreign, primary RelationID
	foreignColumns   []string
	primaryColumns   []string
	array, optional  bool
}

func catalogFK(foreign RelationID, foreignColumns []string, primary RelationID, primaryColumns []string, array, optional bool) catalogForeignKey {
	return catalogForeignKey{foreign: foreign, foreignColumns: foreignColumns, primary: primary, primaryColumns: primaryColumns, array: array, optional: optional}
}

var catalogForeignKeys = []catalogForeignKey{
	catalogFK(IDPgAggregate, []string{"aggfnoid"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgAggregate, []string{"aggtransfn"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgAggregate, []string{"aggfinalfn"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggcombinefn"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggserialfn"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggdeserialfn"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggmtransfn"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggminvtransfn"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggmfinalfn"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggsortop"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgAggregate, []string{"aggtranstype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgAggregate, []string{"aggmtranstype"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgAm, []string{"amhandler"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgAmop, []string{"amopfamily"}, IDPgOpfamily, []string{"oid"}, false, false),
	catalogFK(IDPgAmop, []string{"amoplefttype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgAmop, []string{"amoprighttype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgAmop, []string{"amopopr"}, IDPgOperator, []string{"oid"}, false, false),
	catalogFK(IDPgAmop, []string{"amopmethod"}, IDPgAm, []string{"oid"}, false, false),
	catalogFK(IDPgAmop, []string{"amopsortfamily"}, IDPgOpfamily, []string{"oid"}, false, true),
	catalogFK(IDPgAmproc, []string{"amprocfamily"}, IDPgOpfamily, []string{"oid"}, false, false),
	catalogFK(IDPgAmproc, []string{"amproclefttype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgAmproc, []string{"amprocrighttype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgAmproc, []string{"amproc"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgAttrdef, []string{"adrelid", "adnum"}, IDPgAttribute, []string{"attrelid", "attnum"}, false, false),
	catalogFK(IDPgAttribute, []string{"attrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgAttribute, []string{"atttypid"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgAttribute, []string{"attcollation"}, IDPgCollation, []string{"oid"}, false, true),
	catalogFK(IDPgCast, []string{"castsource"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgCast, []string{"casttarget"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgCast, []string{"castfunc"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgClass, []string{"relnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgClass, []string{"reltype"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgClass, []string{"reloftype"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgClass, []string{"relam"}, IDPgAm, []string{"oid"}, false, true),
	catalogFK(IDPgCollation, []string{"collnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgConstraint, []string{"connamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgConstraint, []string{"conrelid"}, IDPgClass, []string{"oid"}, false, true),
	catalogFK(IDPgConstraint, []string{"contypid"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgConstraint, []string{"conindid"}, IDPgClass, []string{"oid"}, false, true),
	catalogFK(IDPgConstraint, []string{"conparentid"}, IDPgConstraint, []string{"oid"}, false, true),
	catalogFK(IDPgConstraint, []string{"confrelid"}, IDPgClass, []string{"oid"}, false, true),
	catalogFK(IDPgConstraint, []string{"conrelid", "conkey"}, IDPgAttribute, []string{"attrelid", "attnum"}, true, true),
	catalogFK(IDPgConstraint, []string{"confrelid", "confkey"}, IDPgAttribute, []string{"attrelid", "attnum"}, true, false),
	catalogFK(IDPgConstraint, []string{"conpfeqop"}, IDPgOperator, []string{"oid"}, true, false),
	catalogFK(IDPgConstraint, []string{"conppeqop"}, IDPgOperator, []string{"oid"}, true, false),
	catalogFK(IDPgConstraint, []string{"conffeqop"}, IDPgOperator, []string{"oid"}, true, false),
	catalogFK(IDPgConstraint, []string{"conexclop"}, IDPgOperator, []string{"oid"}, true, false),
	catalogFK(IDPgEnum, []string{"enumtypid"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgIndex, []string{"indexrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgIndex, []string{"indrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgIndex, []string{"indcollation"}, IDPgCollation, []string{"oid"}, true, true),
	catalogFK(IDPgIndex, []string{"indclass"}, IDPgOpclass, []string{"oid"}, true, false),
	catalogFK(IDPgIndex, []string{"indrelid", "indkey"}, IDPgAttribute, []string{"attrelid", "attnum"}, true, true),
	catalogFK(IDPgInherits, []string{"inhrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgInherits, []string{"inhparent"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgLanguage, []string{"lanplcallfoid"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgLanguage, []string{"laninline"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgLanguage, []string{"lanvalidator"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgOpclass, []string{"opcmethod"}, IDPgAm, []string{"oid"}, false, false),
	catalogFK(IDPgOpclass, []string{"opcnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgOpclass, []string{"opcfamily"}, IDPgOpfamily, []string{"oid"}, false, false),
	catalogFK(IDPgOpclass, []string{"opcintype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgOpclass, []string{"opckeytype"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgOperator, []string{"oprnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgOperator, []string{"oprleft"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgOperator, []string{"oprright"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgOperator, []string{"oprresult"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgOperator, []string{"oprcom"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgOperator, []string{"oprnegate"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgOperator, []string{"oprcode"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgOperator, []string{"oprrest"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgOperator, []string{"oprjoin"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgOpfamily, []string{"opfmethod"}, IDPgAm, []string{"oid"}, false, false),
	catalogFK(IDPgOpfamily, []string{"opfnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgPartitionedTable, []string{"partrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgPartitionedTable, []string{"partdefid"}, IDPgClass, []string{"oid"}, false, true),
	catalogFK(IDPgPartitionedTable, []string{"partrelid", "partattrs"}, IDPgAttribute, []string{"attrelid", "attnum"}, true, true),
	catalogFK(IDPgPartitionedTable, []string{"partclass"}, IDPgOpclass, []string{"oid"}, true, false),
	catalogFK(IDPgPartitionedTable, []string{"partcollation"}, IDPgCollation, []string{"oid"}, true, true),
	catalogFK(IDPgProc, []string{"pronamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgProc, []string{"prolang"}, IDPgLanguage, []string{"oid"}, false, false),
	catalogFK(IDPgProc, []string{"provariadic"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgProc, []string{"prosupport"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgProc, []string{"prorettype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgProc, []string{"proargtypes"}, IDPgType, []string{"oid"}, true, false),
	catalogFK(IDPgProc, []string{"proallargtypes"}, IDPgType, []string{"oid"}, true, false),
	catalogFK(IDPgProc, []string{"protrftypes"}, IDPgType, []string{"oid"}, true, false),
	catalogFK(IDPgRange, []string{"rngtypid"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgRange, []string{"rngsubtype"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgRange, []string{"rngmultitypid"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgRange, []string{"rngcollation"}, IDPgCollation, []string{"oid"}, false, true),
	catalogFK(IDPgRange, []string{"rngsubopc"}, IDPgOpclass, []string{"oid"}, false, false),
	catalogFK(IDPgRange, []string{"rngcanonical"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgRange, []string{"rngsubdiff"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgSequence, []string{"seqrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgSequence, []string{"seqtypid"}, IDPgType, []string{"oid"}, false, false),
	catalogFK(IDPgStatistic, []string{"starelid", "staattnum"}, IDPgAttribute, []string{"attrelid", "attnum"}, false, false),
	catalogFK(IDPgStatistic, []string{"staop1"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"staop2"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"staop3"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"staop4"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"staop5"}, IDPgOperator, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"stacoll1"}, IDPgCollation, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"stacoll2"}, IDPgCollation, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"stacoll3"}, IDPgCollation, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"stacoll4"}, IDPgCollation, []string{"oid"}, false, true),
	catalogFK(IDPgStatistic, []string{"stacoll5"}, IDPgCollation, []string{"oid"}, false, true),
	catalogFK(IDPgStatisticExt, []string{"stxrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgStatisticExt, []string{"stxnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgStatisticExt, []string{"stxrelid", "stxkeys"}, IDPgAttribute, []string{"attrelid", "attnum"}, true, false),
	catalogFK(IDPgStatisticExtData, []string{"stxoid"}, IDPgStatisticExt, []string{"oid"}, false, false),
	catalogFK(IDPgTrigger, []string{"tgrelid"}, IDPgClass, []string{"oid"}, false, false),
	catalogFK(IDPgTrigger, []string{"tgparentid"}, IDPgTrigger, []string{"oid"}, false, true),
	catalogFK(IDPgTrigger, []string{"tgfoid"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgTrigger, []string{"tgconstrrelid"}, IDPgClass, []string{"oid"}, false, true),
	catalogFK(IDPgTrigger, []string{"tgconstrindid"}, IDPgClass, []string{"oid"}, false, true),
	catalogFK(IDPgTrigger, []string{"tgconstraint"}, IDPgConstraint, []string{"oid"}, false, true),
	catalogFK(IDPgTrigger, []string{"tgrelid", "tgattr"}, IDPgAttribute, []string{"attrelid", "attnum"}, true, false),
	catalogFK(IDPgTSConfig, []string{"cfgnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgTSConfig, []string{"cfgparser"}, IDPgTSParser, []string{"oid"}, false, false),
	catalogFK(IDPgTSConfigMap, []string{"mapcfg"}, IDPgTSConfig, []string{"oid"}, false, false),
	catalogFK(IDPgTSConfigMap, []string{"mapdict"}, IDPgTSDict, []string{"oid"}, false, false),
	catalogFK(IDPgTSDict, []string{"dictnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgTSDict, []string{"dicttemplate"}, IDPgTSTemplate, []string{"oid"}, false, false),
	catalogFK(IDPgTSParser, []string{"prsnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgTSParser, []string{"prsstart"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgTSParser, []string{"prstoken"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgTSParser, []string{"prsend"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgTSParser, []string{"prsheadline"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgTSParser, []string{"prslextype"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgTSTemplate, []string{"tmplnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgTSTemplate, []string{"tmplinit"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgTSTemplate, []string{"tmpllexize"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgType, []string{"typnamespace"}, IDPgNamespace, []string{"oid"}, false, false),
	catalogFK(IDPgType, []string{"typrelid"}, IDPgClass, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typsubscript"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typelem"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typarray"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typinput"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgType, []string{"typoutput"}, IDPgProc, []string{"oid"}, false, false),
	catalogFK(IDPgType, []string{"typreceive"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typsend"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typmodin"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typmodout"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typanalyze"}, IDPgProc, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typbasetype"}, IDPgType, []string{"oid"}, false, true),
	catalogFK(IDPgType, []string{"typcollation"}, IDPgCollation, []string{"oid"}, false, true),
}

func (s *Source) VisitCatalogForeignKeys(ctx context.Context, visit func(model.CatalogForeignKeySummary) error) error {
	if s == nil || visit == nil {
		return &model.Error{Code: "22023", Message: "invalid catalog foreign-key visitor"}
	}
	for _, key := range catalogForeignKeys {
		if err := ctx.Err(); err != nil {
			return err
		}
		foreign, ok := s.options.Registry.LookupID(key.foreign)
		if !ok || !catalogColumnsExist(foreign, key.foreignColumns) {
			return &model.Error{Code: "XX000", Message: "invalid logical catalog foreign-key metadata"}
		}
		primary, ok := s.options.Registry.LookupID(key.primary)
		if !ok || !catalogColumnsExist(primary, key.primaryColumns) || len(key.foreignColumns) != len(key.primaryColumns) {
			return &model.Error{Code: "XX000", Message: "invalid logical catalog foreign-key metadata"}
		}
		foreignOID, primaryOID := relationOID(foreign, s.options), relationOID(primary, s.options)
		if foreignOID == 0 || primaryOID == 0 {
			return &model.Error{Code: "55000", Message: "logical catalog relation OID is unavailable"}
		}
		if err := visit(model.CatalogForeignKeySummary{ForeignTable: foreignOID, ForeignColumns: key.foreignColumns, PrimaryTable: primaryOID, PrimaryColumns: key.primaryColumns, ForeignIsArray: key.array, ForeignOptional: key.optional}); err != nil {
			return err
		}
	}
	return nil
}

func catalogColumnsExist(relation Relation, columns []string) bool {
	if len(columns) == 0 {
		return false
	}
	for _, name := range columns {
		found := false
		for _, column := range relation.Columns {
			if column.Name == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
