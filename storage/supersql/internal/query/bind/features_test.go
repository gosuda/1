package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestStatementFeatureBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		sql     string
		feature bind.FeatureID
	}{
		{"database", "CREATE DATABASE db", bind.ClusterAdmin},
		{"tablespace", "CREATE TABLE t(i int) TABLESPACE other", bind.ClusterAdmin},
		{"role", "CREATE ROLE r", bind.Authorization},
		{"schema authorization", "CREATE SCHEMA s AUTHORIZATION r", bind.Authorization},
		{"security definer", "CREATE FUNCTION f() RETURNS int LANGUAGE SQL SECURITY DEFINER AS 'SELECT 1'", bind.Authorization},
		{"row security", "ALTER TABLE t ENABLE ROW LEVEL SECURITY", bind.Authorization},
		{"extension", "CREATE EXTENSION hstore", bind.ExternalCode},
		{"native routine", "CREATE FUNCTION f() RETURNS int LANGUAGE C AS 'library', 'f'", bind.ExternalCode},
		{"external anonymous block", "DO LANGUAGE plpython3u 'pass'", bind.ExternalCode},
		{"foreign server", "CREATE SERVER s FOREIGN DATA WRAPPER f", bind.ForeignData},
		{"publication", "CREATE PUBLICATION p FOR ALL TABLES", bind.Replication},
		{"prepared transaction", "PREPARE TRANSACTION 'x'", bind.PreparedTransactions},
		{"imported snapshot", "SET TRANSACTION SNAPSHOT '1-2-3'", bind.PreparedTransactions},
		{"begin weaker isolation", "BEGIN ISOLATION LEVEL REPEATABLE READ", bind.WeakerIsolation},
		{"set weaker isolation", "SET TRANSACTION ISOLATION LEVEL READ COMMITTED", bind.WeakerIsolation},
		{"session weaker isolation", "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ UNCOMMITTED", bind.WeakerIsolation},
		{"default weaker isolation", "SET default_transaction_isolation = 'repeatable read'", bind.WeakerIsolation},
		{"unsupported setting", "SET shared_buffers = '1GB'", bind.ServerObservability},
		{"notification", "NOTIFY n, 'payload'", bind.Notifications},
		{"server copy", "COPY t TO '/tmp/out'", bind.ServerIO},
		{"copy program", "COPY t FROM PROGRAM 'cat /tmp/in'", bind.ServerIO},
		{"xml before effect", "SELECT nextval('s'), XMLPARSE(DOCUMENT '<a/>')", bind.XML},
		{"unused xml CTE", "WITH unused AS (SELECT XMLPARSE(DOCUMENT '<a/>')) SELECT 1", bind.XML},
		{"xml table", "SELECT * FROM XMLTABLE('/a' PASSING XMLPARSE(DOCUMENT '<a/>') COLUMNS x int PATH '.')", bind.XML},
		{"xml serialize null", "SELECT XMLSERIALIZE(DOCUMENT NULL AS text INDENT)", bind.XML},
		{"xml serialize text", "SELECT XMLSERIALIZE(CONTENT '<a/>' AS text)", bind.XML},
		{"logical view check option", "ALTER VIEW v SET (check_option=local)", ""},
		{"logical view reset option", "ALTER VIEW v RESET (check_option)", ""},
		{"logical view security barrier", "ALTER VIEW v SET (security_barrier=true)", ""},
		{"physical table option", "ALTER TABLE t SET (fillfactor=70)", bind.PhysicalStorage},
		{"physical index option", "ALTER INDEX i RESET (fillfactor)", bind.PhysicalStorage},
		{"collation", "CREATE COLLATION c (provider = icu, locale = 'en')", bind.LocaleProviders},
		{"unlogged", "CREATE UNLOGGED TABLE t(i int)", bind.PhysicalStorage},
		{"physical column", "CREATE TABLE t(a text COMPRESSION pglz)", bind.PhysicalStorage},
		{"table method", "CREATE TABLE t(a int) USING heap", bind.PhysicalStorage},
		{"gin index", "CREATE INDEX i ON t USING gin(a)", bind.NonBtreeIndexes},
		{"temporal unique", "CREATE TABLE t(i int, r int4range, PRIMARY KEY(i, r WITHOUT OVERLAPS))", bind.AdvancedExclusion},
		{"gist exclusion", "CREATE TABLE t(r int4range, EXCLUDE USING gist (r WITH &&))", bind.AdvancedExclusion},
		{"rewrite rule", "CREATE RULE r AS ON DELETE TO t DO INSTEAD NOTHING", bind.RewriteHooks},
		{"vacuum", "VACUUM t", bind.PhysicalMaintenance},
		{"schema reindex", "REINDEX SCHEMA public", bind.PhysicalMaintenance},
		{"logical schema", "CREATE SCHEMA s", ""},
		{"btree neighbor", "CREATE UNIQUE INDEX i ON t USING btree(a DESC NULLS FIRST) INCLUDE(b) WHERE a > 0", ""},
		{"range query", "SELECT '[1,2)'::int4range && '[2,3)'::int4range", ""},
		{"deferred FK", "CREATE TABLE t(i int REFERENCES p(i) DEFERRABLE INITIALLY DEFERRED)", ""},
		{"equality exclusion", "CREATE TABLE t(i int, EXCLUDE USING btree(i WITH =))", ""},
		{"caller copy", "COPY t TO STDOUT WITH (FORMAT binary)", ""},
		{"analyze", "ANALYZE t", ""},
		{"table reindex", "REINDEX TABLE CONCURRENTLY t", ""},
		{"SQL prepare", "PREPARE q(int) AS SELECT $1", ""},
		{"SQL routine", "CREATE FUNCTION f() RETURNS int LANGUAGE SQL SECURITY INVOKER AS 'SELECT 1'", ""},
		{"SQL composite type", "CREATE TYPE t AS (i int)", ""},
		{"SQL enum", "CREATE TYPE e AS ENUM ('a', 'b')", ""},
		{"user names", "SELECT public.pg_notify('x', 'y') FROM public.pg_stat_activity", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := zpgparse.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sql, err)
			}
			err = bind.ValidateStatementFeatures(t.Context(), parsed.Stmts[0].Stmt)
			if tc.feature == "" {
				if err != nil {
					t.Fatalf("required syntax rejected: %v", err)
				}
				return
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.SQLState() != "0A000" || sqlErr.FeatureID != string(tc.feature) {
				t.Fatalf("got %v; want 0A000 feature %s", err, tc.feature)
			}
		})
	}
}
