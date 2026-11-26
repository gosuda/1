package bind

import (
	"context"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const Profile = "embedded_pg18"
const PostgreSQLVersion = 180004

type FeatureID string

const (
	ServerTransport      FeatureID = "server_transport"
	ClusterAdmin         FeatureID = "cluster_admin"
	Authorization        FeatureID = "authorization"
	ExternalCode         FeatureID = "external_code"
	ForeignData          FeatureID = "foreign_data"
	Replication          FeatureID = "replication"
	PreparedTransactions FeatureID = "prepared_transactions"
	WeakerIsolation      FeatureID = "weaker_isolation"
	Notifications        FeatureID = "notifications"
	ServerIO             FeatureID = "server_io"
	XML                  FeatureID = "xml"
	LocaleProviders      FeatureID = "locale_providers"
	PhysicalStorage      FeatureID = "physical_storage"
	NonBtreeIndexes      FeatureID = "non_btree_indexes"
	AdvancedExclusion    FeatureID = "advanced_exclusion"
	RewriteHooks         FeatureID = "rewrite_hooks"
	PhysicalMaintenance  FeatureID = "physical_maintenance"
	ServerObservability  FeatureID = "server_observability"
	AggregateOrReplace   FeatureID = "aggregate_or_replace"
	SessionState         FeatureID = "session_state"
	// WriteDescriptorExpansion covers write targets whose descriptor needs a
	// constraint, index, trigger, or partition expansion the embedded writer
	// does not implement.
	WriteDescriptorExpansion FeatureID = "write_descriptor_expansion"
	// DatetimeFields covers EXTRACT, date_part, and date_trunc field units the
	// embedded temporal kernels do not implement for the resolved type.
	DatetimeFields FeatureID = "datetime_fields"
	// EncodingConversion covers character-set conversion for encodings outside
	// the builtin conversion kernels (UTF8, LATIN1, WIN1252).
	EncodingConversion FeatureID = "encoding_conversion"
)

// UnsupportedMessage names one profile exclusion in the diagnostic of every
// rejected statement, so the manifest id and the error text cannot drift apart.
func UnsupportedMessage(feature FeatureID) string {
	return "feature is outside " + Profile + ": " + string(feature)
}

func unsupported(feature FeatureID, position int) error {
	return &model.Error{Code: "0A000", Message: UnsupportedMessage(feature), FeatureID: string(feature), Position: position}
}

// ValidateStatementFeatures rejects syntactic exclusions before any evaluation.
// Object-dependent exclusions are checked after resolution, not by spelling a
// user object's name like an excluded pg_catalog object.
func ValidateStatementFeatures(ctx context.Context, statement *zpgparse.Node) error {
	if err := ctx.Err(); err != nil {
		return &model.Error{Code: "57014", Message: "statement canceled", Cause: err}
	}
	if statement == nil {
		return &model.Error{Code: "42601", Message: "missing statement"}
	}
	remaining := 1_000_000
	return validateFeatureTree(ctx, statement.ProtoReflect(), 0, &remaining)
}

func validateFeatureTree(ctx context.Context, message protoreflect.Message, depth int, remaining *int) error {
	if !message.IsValid() {
		return nil
	}
	if depth > 512 || *remaining == 0 {
		return &model.Error{Code: "54000", Message: "statement feature validation limit exceeded"}
	}
	*remaining--
	if *remaining&127 == 0 {
		if err := ctx.Err(); err != nil {
			return &model.Error{Code: "57014", Message: "statement canceled", Cause: err}
		}
	}
	if feature := statementFeature(message); feature != "" {
		position := 0
		if field := message.Descriptor().Fields().ByName("location"); field != nil && field.Kind() == protoreflect.Int32Kind {
			if location := message.Get(field).Int(); location >= 0 {
				position = int(location) + 1
			}
		}
		return unsupported(feature, position)
	}
	fields := message.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if field.Kind() != protoreflect.MessageKind || !message.Has(field) {
			continue
		}
		if field.IsList() {
			list := message.Get(field).List()
			for j := range list.Len() {
				if err := validateFeatureTree(ctx, list.Get(j).Message(), depth+1, remaining); err != nil {
					return err
				}
			}
			continue
		}
		if err := validateFeatureTree(ctx, message.Get(field).Message(), depth+1, remaining); err != nil {
			return err
		}
	}
	return nil
}

func statementFeature(message protoreflect.Message) FeatureID {
	switch node := message.Interface().(type) {
	case *zpgparse.CreatedbStmt, *zpgparse.AlterDatabaseStmt, *zpgparse.AlterDatabaseRefreshCollStmt,
		*zpgparse.AlterDatabaseSetStmt, *zpgparse.DropdbStmt, *zpgparse.AlterSystemStmt,
		*zpgparse.CreateTableSpaceStmt, *zpgparse.DropTableSpaceStmt, *zpgparse.AlterTableSpaceOptionsStmt,
		*zpgparse.AlterTableMoveAllStmt:
		return ClusterAdmin
	case *zpgparse.CreateRoleStmt, *zpgparse.AlterRoleStmt, *zpgparse.AlterRoleSetStmt, *zpgparse.DropRoleStmt,
		*zpgparse.GrantStmt, *zpgparse.GrantRoleStmt, *zpgparse.AlterDefaultPrivilegesStmt,
		*zpgparse.DropOwnedStmt, *zpgparse.ReassignOwnedStmt, *zpgparse.AlterOwnerStmt,
		*zpgparse.CreatePolicyStmt, *zpgparse.AlterPolicyStmt, *zpgparse.SecLabelStmt:
		return Authorization
	case *zpgparse.CreateExtensionStmt, *zpgparse.AlterExtensionStmt, *zpgparse.AlterExtensionContentsStmt,
		*zpgparse.LoadStmt, *zpgparse.CreatePLangStmt, *zpgparse.CreateTransformStmt:
		return ExternalCode
	case *zpgparse.CreateFdwStmt, *zpgparse.AlterFdwStmt, *zpgparse.CreateForeignServerStmt,
		*zpgparse.AlterForeignServerStmt, *zpgparse.CreateUserMappingStmt, *zpgparse.AlterUserMappingStmt,
		*zpgparse.DropUserMappingStmt, *zpgparse.CreateForeignTableStmt, *zpgparse.ImportForeignSchemaStmt:
		return ForeignData
	case *zpgparse.CreatePublicationStmt, *zpgparse.AlterPublicationStmt, *zpgparse.CreateSubscriptionStmt,
		*zpgparse.AlterSubscriptionStmt, *zpgparse.DropSubscriptionStmt:
		return Replication
	case *zpgparse.ListenStmt, *zpgparse.NotifyStmt, *zpgparse.UnlistenStmt:
		return Notifications
	case *zpgparse.XmlExpr, *zpgparse.XmlSerialize, *zpgparse.RangeTableFunc:
		return XML
	case *zpgparse.CreateAmStmt, *zpgparse.CreateOpClassStmt, *zpgparse.CreateOpFamilyStmt, *zpgparse.AlterOpFamilyStmt:
		return NonBtreeIndexes
	case *zpgparse.RuleStmt, *zpgparse.CreateEventTrigStmt, *zpgparse.AlterEventTrigStmt:
		return RewriteHooks
	case *zpgparse.ClusterStmt, *zpgparse.CheckPointStmt:
		return PhysicalMaintenance
	case *zpgparse.VacuumStmt:
		if node.IsVacuumcmd {
			return PhysicalMaintenance
		}
	case *zpgparse.ReindexStmt:
		if node.Kind == zpgparse.ReindexObjectType_REINDEX_OBJECT_SCHEMA || node.Kind == zpgparse.ReindexObjectType_REINDEX_OBJECT_SYSTEM || node.Kind == zpgparse.ReindexObjectType_REINDEX_OBJECT_DATABASE {
			return PhysicalMaintenance
		}
	case *zpgparse.CreateSchemaStmt:
		if node.Authrole != nil {
			return Authorization
		}
	case *zpgparse.CopyStmt:
		if node.IsProgram || node.Filename != "" {
			return ServerIO
		}
	case *zpgparse.RangeVar:
		if node.Relpersistence == "u" {
			return PhysicalStorage
		}
		if node.Schemaname == "pg_catalog" {
			return builtinRelationFeature("pg_catalog." + node.Relname)
		}
	case *zpgparse.CreateStmt:
		if node.Tablespacename != "" {
			return ClusterAdmin
		}
		if node.AccessMethod != "" || len(node.Options) != 0 {
			return PhysicalStorage
		}
	case *zpgparse.IntoClause:
		if node.TableSpaceName != "" {
			return ClusterAdmin
		}
		if node.AccessMethod != "" || len(node.Options) != 0 {
			return PhysicalStorage
		}
	case *zpgparse.ColumnDef:
		if node.Compression != "" || node.StorageName != "" {
			return PhysicalStorage
		}
		if len(node.Fdwoptions) != 0 {
			return ForeignData
		}
	case *zpgparse.IndexStmt:
		if node.Iswithoutoverlaps {
			return AdvancedExclusion
		}
		if node.AccessMethod != "" && node.AccessMethod != "btree" {
			return NonBtreeIndexes
		}
		if node.TableSpace != "" {
			return ClusterAdmin
		}
		if len(node.Options) != 0 {
			return PhysicalStorage
		}
	case *zpgparse.Constraint:
		if node.WithoutOverlaps || node.FkWithPeriod || node.PkWithPeriod {
			return AdvancedExclusion
		}
		if node.Contype == zpgparse.ConstrType_CONSTR_EXCLUSION && node.AccessMethod != "" && node.AccessMethod != "btree" {
			return AdvancedExclusion
		}
		if node.Indexspace != "" {
			return ClusterAdmin
		}
		for _, option := range node.Options {
			if node.Contype != zpgparse.ConstrType_CONSTR_IDENTITY {
				return PhysicalStorage
			}
			definition := option.GetDefElem()
			if definition != nil && (definition.Defname == "logged" || definition.Defname == "unlogged") {
				return PhysicalStorage
			}
		}
	case *zpgparse.IndexElem:
		if len(node.Opclassopts) != 0 {
			return NonBtreeIndexes
		}
	case *zpgparse.AlterTableStmt:
		if feature := objectKindFeature(node.Objtype); feature != "" {
			return feature
		}
		if node.Objtype != zpgparse.ObjectType_OBJECT_VIEW {
			for _, command := range node.Cmds {
				switch command.GetAlterTableCmd().GetSubtype() {
				case zpgparse.AlterTableType_AT_SetRelOptions, zpgparse.AlterTableType_AT_ResetRelOptions, zpgparse.AlterTableType_AT_ReplaceRelOptions:
					return PhysicalStorage
				}
			}
		}
	case *zpgparse.CommentStmt:
		return objectKindFeature(node.Objtype)
	case *zpgparse.AlterObjectDependsStmt:
		return ExternalCode
	case *zpgparse.AlterTypeStmt:
		for _, option := range node.Options {
			switch option.GetDefElem().GetDefname() {
			case "receive", "send", "typmod_in", "typmod_out", "analyze", "subscript":
				return ExternalCode
			case "storage":
				return PhysicalStorage
			}
		}
	case *zpgparse.AlterTableCmd:
		return alterTableFeature(node.Subtype)
	case *zpgparse.TransactionStmt:
		switch node.Kind {
		case zpgparse.TransactionStmtKind_TRANS_STMT_PREPARE, zpgparse.TransactionStmtKind_TRANS_STMT_COMMIT_PREPARED, zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK_PREPARED:
			return PreparedTransactions
		}
	case *zpgparse.DropStmt:
		return objectKindFeature(node.RemoveType)
	case *zpgparse.DefineStmt:
		if node.Kind == zpgparse.ObjectType_OBJECT_TYPE {
			if len(node.Definition) == 0 && len(node.Args) == 0 && !node.Replace {
				return ""
			}
			return ExternalCode
		}
		return objectKindFeature(node.Kind)
	case *zpgparse.AlterObjectSchemaStmt:
		return objectKindFeature(node.ObjectType)
	case *zpgparse.RenameStmt:
		return objectKindFeature(node.RenameType)
	case *zpgparse.CreateFunctionStmt:
		return routineOptionsFeature(node.Options)
	case *zpgparse.AlterFunctionStmt:
		return routineOptionsFeature(node.Actions)
	case *zpgparse.DoStmt:
		return routineOptionsFeature(node.Args)
	case *zpgparse.VariableSetStmt:
		return settingStatementFeature(node)
	case *zpgparse.VariableShowStmt:
		feature, _ := builtinSettingFeature(strings.ToLower(node.Name))
		return feature
	case *zpgparse.DefElem:
		if node.Defname == "transaction_isolation" {
			return isolationValueFeature(node.Arg)
		}
	case *zpgparse.AlterCollationStmt:
		return LocaleProviders
	case *zpgparse.CreateConversionStmt:
		return LocaleProviders
	}
	return ""
}

func objectKindFeature(kind zpgparse.ObjectType) FeatureID {
	switch kind {
	case zpgparse.ObjectType_OBJECT_DATABASE, zpgparse.ObjectType_OBJECT_TABLESPACE:
		return ClusterAdmin
	case zpgparse.ObjectType_OBJECT_ROLE, zpgparse.ObjectType_OBJECT_POLICY, zpgparse.ObjectType_OBJECT_DEFACL, zpgparse.ObjectType_OBJECT_PARAMETER_ACL:
		return Authorization
	case zpgparse.ObjectType_OBJECT_EXTENSION, zpgparse.ObjectType_OBJECT_LANGUAGE, zpgparse.ObjectType_OBJECT_TRANSFORM, zpgparse.ObjectType_OBJECT_TSPARSER, zpgparse.ObjectType_OBJECT_TSTEMPLATE:
		return ExternalCode
	case zpgparse.ObjectType_OBJECT_FDW, zpgparse.ObjectType_OBJECT_FOREIGN_SERVER, zpgparse.ObjectType_OBJECT_FOREIGN_TABLE, zpgparse.ObjectType_OBJECT_USER_MAPPING:
		return ForeignData
	case zpgparse.ObjectType_OBJECT_PUBLICATION, zpgparse.ObjectType_OBJECT_PUBLICATION_NAMESPACE, zpgparse.ObjectType_OBJECT_PUBLICATION_REL, zpgparse.ObjectType_OBJECT_SUBSCRIPTION:
		return Replication
	case zpgparse.ObjectType_OBJECT_LARGEOBJECT:
		return ServerIO
	case zpgparse.ObjectType_OBJECT_COLLATION, zpgparse.ObjectType_OBJECT_CONVERSION:
		return LocaleProviders
	case zpgparse.ObjectType_OBJECT_ACCESS_METHOD, zpgparse.ObjectType_OBJECT_OPCLASS, zpgparse.ObjectType_OBJECT_OPFAMILY, zpgparse.ObjectType_OBJECT_AMOP, zpgparse.ObjectType_OBJECT_AMPROC:
		return NonBtreeIndexes
	case zpgparse.ObjectType_OBJECT_RULE, zpgparse.ObjectType_OBJECT_EVENT_TRIGGER:
		return RewriteHooks
	}
	return ""
}

func alterTableFeature(kind zpgparse.AlterTableType) FeatureID {
	switch kind {
	case zpgparse.AlterTableType_AT_ChangeOwner, zpgparse.AlterTableType_AT_EnableRowSecurity, zpgparse.AlterTableType_AT_DisableRowSecurity, zpgparse.AlterTableType_AT_ForceRowSecurity, zpgparse.AlterTableType_AT_NoForceRowSecurity:
		return Authorization
	case zpgparse.AlterTableType_AT_SetTableSpace:
		return ClusterAdmin
	case zpgparse.AlterTableType_AT_SetStorage, zpgparse.AlterTableType_AT_SetCompression,
		zpgparse.AlterTableType_AT_SetLogged, zpgparse.AlterTableType_AT_SetUnLogged,
		zpgparse.AlterTableType_AT_SetAccessMethod, zpgparse.AlterTableType_AT_SetOptions, zpgparse.AlterTableType_AT_ResetOptions:
		return PhysicalStorage
	case zpgparse.AlterTableType_AT_ClusterOn, zpgparse.AlterTableType_AT_DropCluster:
		return PhysicalMaintenance
	case zpgparse.AlterTableType_AT_EnableRule, zpgparse.AlterTableType_AT_EnableAlwaysRule, zpgparse.AlterTableType_AT_EnableReplicaRule, zpgparse.AlterTableType_AT_DisableRule:
		return RewriteHooks
	case zpgparse.AlterTableType_AT_ReplicaIdentity:
		return Replication
	case zpgparse.AlterTableType_AT_GenericOptions, zpgparse.AlterTableType_AT_AlterColumnGenericOptions:
		return ForeignData
	}
	return ""
}
