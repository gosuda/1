package catalogrel

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	defaultTextSearchParser       model.OID = 3722
	simpleTextSearchTemplate      model.OID = 3727
	simpleTextSearchDictionary    model.OID = 3765
	simpleTextSearchConfiguration model.OID = 3748
)

func emitTextSearchCatalog(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	switch relation.ID {
	case IDPgTSParser:
		return emitCatalogRow(relation, emit,
			fv("oid", oidValue(defaultTextSearchParser)), fv("prsname", stringValue("default")), fv("prsnamespace", oidValue(11)),
			fv("prsstart", oidValue(3717)), fv("prstoken", oidValue(3718)), fv("prsend", oidValue(3719)), fv("prsheadline", oidValue(3720)), fv("prslextype", oidValue(3721)))
	case IDPgTSTemplate:
		for _, template := range []struct {
			oid     model.OID
			name    string
			init    model.OID
			lexize  model.OID
		}{
			{3727, "simple", 3725, 3726},
			{3730, "synonym", 3728, 3729},
			{3733, "ispell", 3731, 3732},
			{3742, "thesaurus", 3740, 3741},
		} {
			if err := emitCatalogRow(relation, emit,
				fv("oid", oidValue(template.oid)),
				fv("tmplname", stringValue(template.name)),
				fv("tmplnamespace", oidValue(11)),
				fv("tmplinit", oidValue(template.init)),
				fv("tmpllexize", oidValue(template.lexize)),
			); err != nil {
				return err
			}
		}
		return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTextSearchTemplate}, func(object objectDescriptor) error {
			metadata := object.spec.TextSearchTemplate
			if metadata == nil {
				return nil
			}
			return emitCatalogRow(relation, emit,
				fv("oid", oidValue(object.spec.OID)),
				fv("tmplname", stringValue(object.spec.Name.Name)),
				fv("tmplnamespace", oidValue(object.schema.oid)),
				fv("tmplinit", oidValue(metadata.Init)),
				fv("tmpllexize", oidValue(metadata.Lexize)),
			)
		})
	case IDPgTSDict:
		if err := emitCatalogRow(relation, emit, fv("oid", oidValue(simpleTextSearchDictionary)), fv("dictname", stringValue("simple")), fv("dictnamespace", oidValue(11)), fv("dictowner", oidValue(10)), fv("dicttemplate", oidValue(simpleTextSearchTemplate))); err != nil {
			return err
		}
		return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTextSearchDictionary}, func(object objectDescriptor) error {
			metadata := object.spec.TextSearchDictionary
			if metadata == nil {
				return nil
			}
			fields := []fieldValue{fv("oid", oidValue(object.spec.OID)), fv("dictname", stringValue(object.spec.Name.Name)), fv("dictnamespace", oidValue(object.schema.oid)), fv("dictowner", oidValue(metadata.Owner)), fv("dicttemplate", oidValue(metadata.Template))}
			if metadata.InitOptions != "" {
				fields = append(fields, fv("dictinitoption", stringValue(metadata.InitOptions)))
			}
			return emitCatalogRow(relation, emit, fields...)
		})
	case IDPgTSConfig:
		if err := emitCatalogRow(relation, emit, fv("oid", oidValue(simpleTextSearchConfiguration)), fv("cfgname", stringValue("simple")), fv("cfgnamespace", oidValue(11)), fv("cfgowner", oidValue(10)), fv("cfgparser", oidValue(defaultTextSearchParser))); err != nil {
			return err
		}
		return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTextSearchConfiguration}, func(object objectDescriptor) error {
			metadata := object.spec.TextSearchConfiguration
			if metadata == nil {
				return nil
			}
			return emitCatalogRow(relation, emit, fv("oid", oidValue(object.spec.OID)), fv("cfgname", stringValue(object.spec.Name.Name)), fv("cfgnamespace", oidValue(object.schema.oid)), fv("cfgowner", oidValue(metadata.Owner)), fv("cfgparser", oidValue(metadata.Parser)))
		})
	case IDPgTSConfigMap:
		if err := emitBuiltinTextSearchMappings(relation, simpleTextSearchConfiguration, simpleTextSearchDictionary, emit); err != nil {
			return err
		}
		return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTextSearchConfiguration}, func(object objectDescriptor) error {
			metadata := object.spec.TextSearchConfiguration
			if metadata == nil {
				return nil
			}
			for _, mapping := range metadata.Mappings {
				for sequence, dictionary := range mapping.Dictionaries {
					if err := emitCatalogRow(relation, emit, fv("mapcfg", oidValue(object.spec.OID)), fv("maptokentype", intValue(int64(mapping.TokenType))), fv("mapseqno", intValue(int64(sequence+1))), fv("mapdict", oidValue(dictionary))); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}
	return &model.Error{Code: "XX000", Message: "unknown text search catalog relation"}
}

func emitBuiltinTextSearchMappings(relation Relation, configuration, dictionary model.OID, emit rowEmitter) error {
	for token := uint16(1); token <= 23; token++ {
		if token == 12 || token == 13 || token == 14 || token == 23 {
			continue
		}
		selected := dictionary
		if err := emitCatalogRow(relation, emit, fv("mapcfg", oidValue(configuration)), fv("maptokentype", intValue(int64(token))), fv("mapseqno", intValue(1)), fv("mapdict", oidValue(selected))); err != nil {
			return err
		}
	}
	return nil
}
