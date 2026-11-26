package supersql

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

// newStackMutationTargets resolves one physical mutation target from the current
// catalog. It is the production replacement for the per-fixture target maps:
// the descriptor comes from a certified current read and the expansion from the
// same index policy provider that owns every published index.
type newStackMutationTargets struct {
	descriptors *catalog.CurrentDescriptors
	policies    *schema.IndexPolicyProvider
}

func (targets newStackMutationTargets) ResolveMutationTarget(ctx context.Context, ref storage.ObjectRef) (exec.MutationTarget, error) {
	if err := ctx.Err(); err != nil {
		return exec.MutationTarget{}, err
	}
	if targets.descriptors == nil || targets.policies == nil {
		return exec.MutationTarget{}, storage.ErrInvalidTarget
	}
	descriptor, err := targets.descriptors.Descriptor(ctx, ref.ObjectID)
	if err != nil {
		return exec.MutationTarget{}, err
	}
	policy, err := targets.policies.TablePolicy(ctx, ref)
	if err != nil {
		return exec.MutationTarget{}, err
	}
	return exec.MutationTarget{Descriptor: descriptor, Expansion: policy.Indexes}, nil
}

// newStackSQLMetadata reports the SQL feature/standards inventory of the
// embedded_pg18 profile for information_schema.sql_features, sql_parts, and
// sql_sizing.
type newStackSQLMetadata struct{}

func int32Value(v int32) *int32 { return &v }

var (
	embeddedSQLFeatures = []catalogrel.SQLFeature{
		{ID: "B011", Name: "Embedded Ada", Supported: false},
		{ID: "B012", Name: "Embedded C", Supported: false},
		{ID: "B013", Name: "Embedded COBOL", Supported: false},
		{ID: "B014", Name: "Embedded Fortran", Supported: false},
		{ID: "B015", Name: "Embedded MUMPS", Supported: false},
		{ID: "B016", Name: "Embedded Pascal", Supported: false},
		{ID: "B017", Name: "Embedded PL/I", Supported: false},
		{ID: "E011", Name: "Numeric data types", Supported: true},
		{ID: "E011", Name: "Numeric data types", SubfeatureID: "01", SubfeatureName: "INTEGER and SMALLINT data types", Supported: true},
		{ID: "E011", Name: "Numeric data types", SubfeatureID: "02", SubfeatureName: "REAL, DOUBLE PRECISION, and FLOAT data types", Supported: true},
		{ID: "E011", Name: "Numeric data types", SubfeatureID: "03", SubfeatureName: "DECIMAL and NUMERIC data types", Supported: true},
		{ID: "E011", Name: "Numeric data types", SubfeatureID: "04", SubfeatureName: "Arithmetic operators", Supported: true},
		{ID: "E011", Name: "Numeric data types", SubfeatureID: "05", SubfeatureName: "Numeric comparison", Supported: true},
		{ID: "E011", Name: "Numeric data types", SubfeatureID: "06", SubfeatureName: "Implicit casting among the numeric data types", Supported: true},
		{ID: "E021", Name: "Character string types", Supported: true},
		{ID: "E031", Name: "Identifiers", Supported: true},
		{ID: "E051", Name: "Basic query specification", Supported: true},
		{ID: "E061", Name: "Basic predicates and search conditions", Supported: true},
		{ID: "E071", Name: "Basic query expressions", Supported: true},
		{ID: "E081", Name: "Basic Privileges", Supported: true},
		{ID: "E091", Name: "Set functions", Supported: true},
		{ID: "E101", Name: "Basic data manipulation", Supported: true},
		{ID: "E111", Name: "Single row SELECT statement", Supported: true},
		{ID: "E121", Name: "Basic cursor support", Supported: true},
		{ID: "E131", Name: "Null value support (nulls in lieu of values)", Supported: true},
		{ID: "E141", Name: "Basic integrity constraints", Supported: true},
		{ID: "E151", Name: "Transaction support", Supported: true},
		{ID: "E152", Name: "Basic SET TRANSACTION statement", Supported: true},
		{ID: "E153", Name: "Updatable queries with subqueries", Supported: true},
		{ID: "E161", Name: "SQL comments using leading double minus", Supported: true},
		{ID: "E171", Name: "SQLSTATE support", Supported: true},
		{ID: "E182", Name: "Host language binding", Supported: false},
		{ID: "F021", Name: "Basic information schema", Supported: true},
		{ID: "F031", Name: "Basic schema manipulation", Supported: true},
		{ID: "F041", Name: "Basic joined table", Supported: true},
		{ID: "F051", Name: "Basic date and time", Supported: true},
		{ID: "F081", Name: "UNION and EXCEPT in views", Supported: true},
		{ID: "F131", Name: "Grouped operations", Supported: true},
		{ID: "F201", Name: "CAST function", Supported: true},
		{ID: "F221", Name: "Explicit defaults", Supported: true},
		{ID: "F261", Name: "CASE expression", Supported: true},
		{ID: "F302", Name: "INTERSECT table operator", Supported: true},
		{ID: "F311", Name: "Schema definition statement", Supported: true},
		{ID: "F401", Name: "Extended joined table", Supported: true},
		{ID: "F471", Name: "Scalar subquery values", Supported: true},
		{ID: "F491", Name: "Constraint management", Supported: true},
		{ID: "F501", Name: "Features and conformance views", Supported: true},
		{ID: "F591", Name: "Derived tables", Supported: true},
		{ID: "F850", Name: "Top-level <order by clause> in <query expression>", Supported: true},
		{ID: "T121", Name: "WITH (excluding RECURSIVE) in query expression", Supported: true},
		{ID: "T131", Name: "Recursive query", Supported: true},
		{ID: "T321", Name: "Basic SQL-invoked routines", Supported: true},
		{ID: "T611", Name: "Elementary OLAP operations", Supported: true},
		{ID: "T631", Name: "IN predicate with one list element", Supported: true},
	}

	embeddedSQLParts = []catalogrel.SQLPart{
		{ID: "1", Name: "Framework (SQL/Framework)", Supported: false},
		{ID: "2", Name: "Foundation (SQL/Foundation)", Supported: false},
		{ID: "3", Name: "Call-Level Interface (SQL/CLI)", Supported: false},
		{ID: "4", Name: "Persistent Stored Modules (SQL/PSM)", Supported: false},
		{ID: "9", Name: "Management of External Data (SQL/MED)", Supported: false},
		{ID: "10", Name: "Object Language Bindings (SQL/OLB)", Supported: false},
		{ID: "11", Name: "Information and Definition Schemas (SQL/Schemata)", Supported: false},
		{ID: "13", Name: "Java Routines and Types (SQL/JRT)", Supported: false},
		{ID: "14", Name: "XML-Related Specifications (SQL/XML)", Supported: false},
	}

	embeddedSQLSizing = []catalogrel.SQLSizing{
		{ID: 34, Name: "MAXIMUM CATALOG NAME LENGTH", SupportedValue: int32Value(63)},
		{ID: 30, Name: "MAXIMUM COLUMN NAME LENGTH", SupportedValue: int32Value(63)},
		{ID: 97, Name: "MAXIMUM COLUMNS IN GROUP BY", SupportedValue: int32Value(0)},
		{ID: 99, Name: "MAXIMUM COLUMNS IN ORDER BY", SupportedValue: int32Value(0)},
		{ID: 100, Name: "MAXIMUM COLUMNS IN SELECT", SupportedValue: int32Value(1664)},
		{ID: 101, Name: "MAXIMUM COLUMNS IN TABLE", SupportedValue: int32Value(1600)},
		{ID: 1, Name: "MAXIMUM CONCURRENT ACTIVITIES", SupportedValue: int32Value(0)},
		{ID: 31, Name: "MAXIMUM CURSOR NAME LENGTH", SupportedValue: int32Value(63)},
		{ID: 0, Name: "MAXIMUM DRIVER CONNECTIONS", SupportedValue: nil},
		{ID: 10005, Name: "MAXIMUM IDENTIFIER LENGTH", SupportedValue: int32Value(63)},
		{ID: 32, Name: "MAXIMUM SCHEMA NAME LENGTH", SupportedValue: int32Value(63)},
		{ID: 20000, Name: "MAXIMUM STATEMENT OCTETS", SupportedValue: int32Value(0)},
		{ID: 20001, Name: "MAXIMUM STATEMENT OCTETS DATA", SupportedValue: int32Value(0)},
		{ID: 20002, Name: "MAXIMUM STATEMENT OCTETS SCHEMA", SupportedValue: int32Value(0)},
		{ID: 35, Name: "MAXIMUM TABLE NAME LENGTH", SupportedValue: int32Value(63)},
		{ID: 106, Name: "MAXIMUM TABLES IN SELECT", SupportedValue: int32Value(0)},
		{ID: 107, Name: "MAXIMUM USER NAME LENGTH", SupportedValue: int32Value(63)},
		{ID: 25000, Name: "MAXIMUM CURRENT DEFAULT TRANSFORM GROUP LENGTH", SupportedValue: nil},
		{ID: 25001, Name: "MAXIMUM CURRENT TRANSFORM GROUP LENGTH", SupportedValue: nil},
		{ID: 25002, Name: "MAXIMUM CURRENT PATH LENGTH", SupportedValue: int32Value(0)},
		{ID: 25003, Name: "MAXIMUM CURRENT ROLE LENGTH", SupportedValue: nil},
		{ID: 25004, Name: "MAXIMUM SESSION USER LENGTH", SupportedValue: int32Value(63)},
		{ID: 25005, Name: "MAXIMUM SYSTEM USER LENGTH", SupportedValue: int32Value(63)},
	}
)

func (newStackSQLMetadata) CatalogFeatures(ctx context.Context) ([]catalogrel.SQLFeature, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return embeddedSQLFeatures, nil
}

func (newStackSQLMetadata) CatalogSQLParts(ctx context.Context) ([]catalogrel.SQLPart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return embeddedSQLParts, nil
}

func (newStackSQLMetadata) CatalogSQLSizing(ctx context.Context) ([]catalogrel.SQLSizing, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return embeddedSQLSizing, nil
}

var (
	_ exec.MutationTargets       = newStackMutationTargets{}
	_ catalogrel.FeatureSource   = newStackSQLMetadata{}
	_ catalogrel.StandardsSource = newStackSQLMetadata{}
)
