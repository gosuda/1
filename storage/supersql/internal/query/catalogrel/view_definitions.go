package catalogrel

import (
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type NativeViewSource struct {
	Relation        RelationID
	Namespace       string
	Name            string
	SemanticVersion uint32
	SQL             string
}

type nativeViewSource struct {
	relation RelationID
	sql      string
}

// Definitions follow PostgreSQL REL_18_4 system_views.sql and
// information_schema.sql. Authorization predicates fold under the immutable
// embedded principal; excluded physical/rewrite catalogs and trigger firing
// modes use equivalent embedded runtime projections.
var nativeViewSources = [...]nativeViewSource{
	{relation: IDPgIndexes, sql: `SELECT
        N.nspname AS schemaname,
        C.relname AS tablename,
        I.relname AS indexname,
        CAST(NULL AS name) AS tablespace,
        pg_get_indexdef(I.oid) AS indexdef
    FROM pg_index X JOIN pg_class C ON (C.oid = X.indrelid)
         JOIN pg_class I ON (I.oid = X.indexrelid)
         LEFT JOIN pg_namespace N ON (N.oid = C.relnamespace)
    WHERE C.relkind IN ('r', 'm', 'p') AND I.relkind IN ('i', 'I')`},
	{relation: IDPgMatviews, sql: `SELECT
        N.nspname AS schemaname,
        C.relname AS matviewname,
        pg_get_userbyid(C.relowner) AS matviewowner,
        CAST(NULL AS name) AS tablespace,
        C.relhasindex AS hasindexes,
        C.relispopulated AS ispopulated,
        pg_get_viewdef(C.oid) AS definition
    FROM pg_class C LEFT JOIN pg_namespace N ON (N.oid = C.relnamespace)
    WHERE C.relkind = 'm'`},
	{relation: IDPgRoles, sql: `SELECT 'embedded'::name AS rolname, false AS rolsuper, true AS rolinherit,
       false AS rolcreaterole, false AS rolcreatedb, true AS rolcanlogin,
       false AS rolreplication, -1::integer AS rolconnlimit,
       NULL::text AS rolpassword, NULL::timestamp with time zone AS rolvaliduntil,
       false AS rolbypassrls, NULL::text[] AS rolconfig, 10::oid AS oid`},
	{relation: IDPgSequences, sql: `SELECT
        N.nspname AS schemaname,
        C.relname AS sequencename,
        pg_get_userbyid(C.relowner) AS sequenceowner,
        S.seqtypid::regtype AS data_type,
        S.seqstart AS start_value,
        S.seqmin AS min_value,
        S.seqmax AS max_value,
        S.seqincrement AS increment_by,
        S.seqcycle AS cycle,
        S.seqcache AS cache_size,
        pg_sequence_last_value(C.oid) AS last_value
    FROM pg_sequence S JOIN pg_class C ON (C.oid = S.seqrelid)
         LEFT JOIN pg_namespace N ON (N.oid = C.relnamespace)
    WHERE NOT FALSE
          AND relkind = 'S'`},
	{relation: IDPgSettings, sql: `SELECT * FROM pg_show_all_settings() AS A`},
	{relation: IDPgStats, sql: `SELECT
        nspname AS schemaname,
        relname AS tablename,
        attname AS attname,
        stainherit AS inherited,
        stanullfrac AS null_frac,
        stawidth AS avg_width,
        stadistinct AS n_distinct,
        CASE
            WHEN stakind1 = 1 THEN stavalues1
            WHEN stakind2 = 1 THEN stavalues2
            WHEN stakind3 = 1 THEN stavalues3
            WHEN stakind4 = 1 THEN stavalues4
            WHEN stakind5 = 1 THEN stavalues5
        END AS most_common_vals,
        CASE
            WHEN stakind1 = 1 THEN stanumbers1
            WHEN stakind2 = 1 THEN stanumbers2
            WHEN stakind3 = 1 THEN stanumbers3
            WHEN stakind4 = 1 THEN stanumbers4
            WHEN stakind5 = 1 THEN stanumbers5
        END AS most_common_freqs,
        CASE
            WHEN stakind1 = 2 THEN stavalues1
            WHEN stakind2 = 2 THEN stavalues2
            WHEN stakind3 = 2 THEN stavalues3
            WHEN stakind4 = 2 THEN stavalues4
            WHEN stakind5 = 2 THEN stavalues5
        END AS histogram_bounds,
        CASE
            WHEN stakind1 = 3 THEN stanumbers1[1]
            WHEN stakind2 = 3 THEN stanumbers2[1]
            WHEN stakind3 = 3 THEN stanumbers3[1]
            WHEN stakind4 = 3 THEN stanumbers4[1]
            WHEN stakind5 = 3 THEN stanumbers5[1]
        END AS correlation,
        CASE
            WHEN stakind1 = 4 THEN stavalues1
            WHEN stakind2 = 4 THEN stavalues2
            WHEN stakind3 = 4 THEN stavalues3
            WHEN stakind4 = 4 THEN stavalues4
            WHEN stakind5 = 4 THEN stavalues5
        END AS most_common_elems,
        CASE
            WHEN stakind1 = 4 THEN stanumbers1
            WHEN stakind2 = 4 THEN stanumbers2
            WHEN stakind3 = 4 THEN stanumbers3
            WHEN stakind4 = 4 THEN stanumbers4
            WHEN stakind5 = 4 THEN stanumbers5
        END AS most_common_elem_freqs,
        CASE
            WHEN stakind1 = 5 THEN stanumbers1
            WHEN stakind2 = 5 THEN stanumbers2
            WHEN stakind3 = 5 THEN stanumbers3
            WHEN stakind4 = 5 THEN stanumbers4
            WHEN stakind5 = 5 THEN stanumbers5
        END AS elem_count_histogram,
        CASE
            WHEN stakind1 = 6 THEN stavalues1
            WHEN stakind2 = 6 THEN stavalues2
            WHEN stakind3 = 6 THEN stavalues3
            WHEN stakind4 = 6 THEN stavalues4
            WHEN stakind5 = 6 THEN stavalues5
        END AS range_length_histogram,
        CASE
            WHEN stakind1 = 6 THEN stanumbers1[1]
            WHEN stakind2 = 6 THEN stanumbers2[1]
            WHEN stakind3 = 6 THEN stanumbers3[1]
            WHEN stakind4 = 6 THEN stanumbers4[1]
            WHEN stakind5 = 6 THEN stanumbers5[1]
        END AS range_empty_frac,
        CASE
            WHEN stakind1 = 7 THEN stavalues1
            WHEN stakind2 = 7 THEN stavalues2
            WHEN stakind3 = 7 THEN stavalues3
            WHEN stakind4 = 7 THEN stavalues4
            WHEN stakind5 = 7 THEN stavalues5
            END AS range_bounds_histogram
    FROM pg_statistic s JOIN pg_class c ON (c.oid = s.starelid)
         JOIN pg_attribute a ON (c.oid = attrelid AND attnum = s.staattnum)
         LEFT JOIN pg_namespace n ON (n.oid = c.relnamespace)
    WHERE NOT attisdropped
    AND TRUE
    AND (c.relrowsecurity = false OR NOT FALSE)`},
	{relation: IDPgTables, sql: `SELECT
        N.nspname AS schemaname,
        C.relname AS tablename,
        pg_get_userbyid(C.relowner) AS tableowner,
        CAST(NULL AS name) AS tablespace,
        C.relhasindex AS hasindexes,
        C.relhasrules AS hasrules,
        C.relhastriggers AS hastriggers,
        C.relrowsecurity AS rowsecurity
    FROM pg_class C LEFT JOIN pg_namespace N ON (N.oid = C.relnamespace)
    WHERE C.relkind IN ('r', 'p')`},
	{relation: IDPgUser, sql: `SELECT 'embedded'::name AS usename, 10::oid AS usesysid,
       false AS usecreatedb, false AS usesuper, false AS userepl,
       false AS usebypassrls, NULL::text AS passwd,
       NULL::timestamp with time zone AS valuntil, NULL::text[] AS useconfig`},
	{relation: IDPgViews, sql: `SELECT
        N.nspname AS schemaname,
        C.relname AS viewname,
        pg_get_userbyid(C.relowner) AS viewowner,
        pg_get_viewdef(C.oid) AS definition
    FROM pg_class C LEFT JOIN pg_namespace N ON (N.oid = C.relnamespace)
    WHERE C.relkind = 'v'`},
	{relation: IDAttributes, sql: `SELECT CAST(current_database() AS sql_identifier) AS udt_catalog,
           CAST(nc.nspname AS sql_identifier) AS udt_schema,
           CAST(c.relname AS sql_identifier) AS udt_name,
           CAST(a.attname AS sql_identifier) AS attribute_name,
           CAST(a.attnum AS cardinal_number) AS ordinal_position,
           CAST(pg_get_expr(ad.adbin, ad.adrelid) AS character_data) AS attribute_default,
           CAST(CASE WHEN a.attnotnull OR (t.typtype = 'd' AND t.typnotnull) THEN 'NO' ELSE 'YES' END
             AS yes_or_no)
             AS is_nullable, -- This column was apparently removed between SQL:2003 and SQL:2008.

           CAST(
             CASE WHEN t.typelem <> 0 AND t.typarray = 0 THEN 'ARRAY'
                  WHEN nt.nspname = 'pg_catalog' THEN format_type(a.atttypid, null)
                  ELSE 'USER-DEFINED' END
             AS character_data)
             AS data_type,

           CAST(
             _pg_char_max_length(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                 CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)

             AS cardinal_number)
             AS character_maximum_length,

           CAST(
             _pg_char_octet_length(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                   CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS character_octet_length,

           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(null AS sql_identifier) AS character_set_name,

           CAST(CASE WHEN nco.nspname IS NOT NULL THEN current_database() END AS sql_identifier) AS collation_catalog,
           CAST(nco.nspname AS sql_identifier) AS collation_schema,
           CAST(co.collname AS sql_identifier) AS collation_name,

           CAST(
             _pg_numeric_precision(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                   CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS numeric_precision,

           CAST(
             _pg_numeric_precision_radix(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                         CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS numeric_precision_radix,

           CAST(
             _pg_numeric_scale(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                               CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS numeric_scale,

           CAST(
             _pg_datetime_precision(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                    CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS datetime_precision,

           CAST(
             _pg_interval_type(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                               CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS character_data)
             AS interval_type,
           CAST(null AS cardinal_number) AS interval_precision,

           CAST(current_database() AS sql_identifier) AS attribute_udt_catalog,
           CAST(nt.nspname AS sql_identifier) AS attribute_udt_schema,
           CAST(t.typname AS sql_identifier) AS attribute_udt_name,

           CAST(null AS sql_identifier) AS scope_catalog,
           CAST(null AS sql_identifier) AS scope_schema,
           CAST(null AS sql_identifier) AS scope_name,

           CAST(null AS cardinal_number) AS maximum_cardinality,
           CAST(a.attnum AS sql_identifier) AS dtd_identifier,
           CAST('NO' AS yes_or_no) AS is_derived_reference_attribute

    FROM (pg_attribute a LEFT JOIN pg_attrdef ad ON attrelid = adrelid AND attnum = adnum)
         JOIN (pg_class c JOIN pg_namespace nc ON (c.relnamespace = nc.oid)) ON a.attrelid = c.oid
         JOIN (pg_type t JOIN pg_namespace nt ON (t.typnamespace = nt.oid)) ON a.atttypid = t.oid
         LEFT JOIN (pg_collation co JOIN pg_namespace nco ON (co.collnamespace = nco.oid))
           ON a.attcollation = co.oid AND (nco.nspname, co.collname) <> ('pg_catalog', 'default')

    WHERE a.attnum > 0 AND NOT a.attisdropped
          AND c.relkind IN ('c')
          AND (TRUE
               OR TRUE)`},
	{relation: IDCharacterSets, sql: `SELECT CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(getdatabaseencoding() AS sql_identifier) AS character_set_name,
           CAST(CASE WHEN getdatabaseencoding() = 'UTF8' THEN 'UCS' ELSE getdatabaseencoding() END AS sql_identifier) AS character_repertoire,
           CAST(getdatabaseencoding() AS sql_identifier) AS form_of_use,
           CAST(current_database() AS sql_identifier) AS default_collate_catalog,
           CAST(nc.nspname AS sql_identifier) AS default_collate_schema,
           CAST(c.collname AS sql_identifier) AS default_collate_name
    FROM pg_database d
         LEFT JOIN (pg_collation c JOIN pg_namespace nc ON (c.collnamespace = nc.oid))
             ON (datcollate = collcollate AND datctype = collctype)
    WHERE d.datname = current_database()
    ORDER BY char_length(c.collname) DESC, c.collname ASC -- prefer full/canonical name
    LIMIT 1`},
	{relation: IDCheckConstraintRoutineUsage, sql: `SELECT DISTINCT
           CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(nc.nspname AS sql_identifier) AS constraint_schema,
           CAST(c.conname AS sql_identifier) AS constraint_name,
           CAST(current_database() AS sql_identifier) AS specific_catalog,
           CAST(np.nspname AS sql_identifier) AS specific_schema,
           CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier) AS specific_name
    FROM pg_namespace nc, pg_constraint c, pg_depend d, pg_proc p, pg_namespace np
    WHERE nc.oid = c.connamespace
      AND c.contype = 'c'
      AND c.oid = d.objid
      AND d.classid = 'pg_catalog.pg_constraint'::regclass
      AND d.refobjid = p.oid
      AND d.refclassid = 'pg_catalog.pg_proc'::regclass
      AND p.pronamespace = np.oid
      AND TRUE`},
	{relation: IDCheckConstraints, sql: `SELECT CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(rs.nspname AS sql_identifier) AS constraint_schema,
           CAST(con.conname AS sql_identifier) AS constraint_name,
           CAST(pg_get_expr(con.conbin, coalesce(c.oid, 0)) AS character_data) AS check_clause
    FROM pg_constraint con
           LEFT OUTER JOIN pg_namespace rs ON (rs.oid = con.connamespace)
           LEFT OUTER JOIN pg_class c ON (c.oid = con.conrelid)
           LEFT OUTER JOIN pg_type t ON (t.oid = con.contypid)
    WHERE TRUE
      AND con.contype = 'c'

    UNION ALL
    -- not-null constraints
    SELECT current_database()::information_schema.sql_identifier AS constraint_catalog,
           rs.nspname::information_schema.sql_identifier AS constraint_schema,
           con.conname::information_schema.sql_identifier AS constraint_name,
           pg_catalog.format('%s IS NOT NULL', coalesce(at.attname, 'VALUE'))::information_schema.character_data AS check_clause
     FROM pg_constraint con
            LEFT JOIN pg_namespace rs ON rs.oid = con.connamespace
            LEFT JOIN pg_class c ON c.oid = con.conrelid
            LEFT JOIN pg_type t ON t.oid = con.contypid
            LEFT JOIN pg_attribute at ON (con.conrelid = at.attrelid AND con.conkey[1] = at.attnum)
     WHERE TRUE
       AND con.contype = 'n'`},
	{relation: IDCollationCharacterSetApplicability, sql: `SELECT CAST(current_database() AS sql_identifier) AS collation_catalog,
           CAST(nc.nspname AS sql_identifier) AS collation_schema,
           CAST(c.collname AS sql_identifier) AS collation_name,
           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(getdatabaseencoding() AS sql_identifier) AS character_set_name
    FROM pg_collation c, pg_namespace nc
    WHERE c.collnamespace = nc.oid
          AND collencoding IN (-1, (SELECT encoding FROM pg_database WHERE datname = current_database()))`},
	{relation: IDCollations, sql: `SELECT CAST(current_database() AS sql_identifier) AS collation_catalog,
           CAST(nc.nspname AS sql_identifier) AS collation_schema,
           CAST(c.collname AS sql_identifier) AS collation_name,
           CAST('NO PAD' AS character_data) AS pad_attribute
    FROM pg_collation c, pg_namespace nc
    WHERE c.collnamespace = nc.oid
          AND collencoding IN (-1, (SELECT encoding FROM pg_database WHERE datname = current_database()))`},
	{relation: IDColumnColumnUsage, sql: `SELECT DISTINCT
           CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(n.nspname AS sql_identifier) AS table_schema,
           CAST(c.relname AS sql_identifier) AS table_name,
           CAST(ac.attname AS sql_identifier) AS column_name,
           CAST(ad.attname AS sql_identifier) AS dependent_column

    FROM pg_namespace n, pg_class c, pg_depend d,
         pg_attribute ac, pg_attribute ad, pg_attrdef atd

    WHERE n.oid = c.relnamespace
          AND c.oid = ac.attrelid
          AND c.oid = ad.attrelid
          AND ac.attnum <> ad.attnum
          AND ad.attrelid = atd.adrelid
          AND ad.attnum = atd.adnum
          AND d.classid = 'pg_catalog.pg_attrdef'::regclass
          AND d.refclassid = 'pg_catalog.pg_class'::regclass
          AND d.objid = atd.oid
          AND d.refobjid = ac.attrelid
          AND d.refobjsubid = ac.attnum
          AND ad.attgenerated <> ''
          AND TRUE`},
	{relation: IDColumnDomainUsage, sql: `SELECT CAST(current_database() AS sql_identifier) AS domain_catalog,
           CAST(nt.nspname AS sql_identifier) AS domain_schema,
           CAST(t.typname AS sql_identifier) AS domain_name,
           CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nc.nspname AS sql_identifier) AS table_schema,
           CAST(c.relname AS sql_identifier) AS table_name,
           CAST(a.attname AS sql_identifier) AS column_name

    FROM pg_type t, pg_namespace nt, pg_class c, pg_namespace nc,
         pg_attribute a

    WHERE t.typnamespace = nt.oid
          AND c.relnamespace = nc.oid
          AND a.attrelid = c.oid
          AND a.atttypid = t.oid
          AND t.typtype = 'd'
          AND c.relkind IN ('r', 'v', 'f', 'p')
          AND a.attnum > 0
          AND NOT a.attisdropped
          AND TRUE`},
	{relation: IDColumnUdtUsage, sql: `SELECT CAST(current_database() AS sql_identifier) AS udt_catalog,
           CAST(coalesce(nbt.nspname, nt.nspname) AS sql_identifier) AS udt_schema,
           CAST(coalesce(bt.typname, t.typname) AS sql_identifier) AS udt_name,
           CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nc.nspname AS sql_identifier) AS table_schema,
           CAST(c.relname AS sql_identifier) AS table_name,
           CAST(a.attname AS sql_identifier) AS column_name

    FROM pg_attribute a, pg_class c, pg_namespace nc,
         (pg_type t JOIN pg_namespace nt ON (t.typnamespace = nt.oid))
           LEFT JOIN (pg_type bt JOIN pg_namespace nbt ON (bt.typnamespace = nbt.oid))
           ON (t.typtype = 'd' AND t.typbasetype = bt.oid)

    WHERE a.attrelid = c.oid
          AND a.atttypid = t.oid
          AND nc.oid = c.relnamespace
          AND a.attnum > 0 AND NOT a.attisdropped
          AND c.relkind in ('r', 'v', 'f', 'p')
          AND TRUE`},
	{relation: IDColumns, sql: `SELECT CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nc.nspname AS sql_identifier) AS table_schema,
           CAST(c.relname AS sql_identifier) AS table_name,
           CAST(a.attname AS sql_identifier) AS column_name,
           CAST(a.attnum AS cardinal_number) AS ordinal_position,
           CAST(CASE WHEN a.attgenerated = '' THEN pg_get_expr(ad.adbin, ad.adrelid) END AS character_data) AS column_default,
           CAST(CASE WHEN a.attnotnull OR (t.typtype = 'd' AND t.typnotnull) THEN 'NO' ELSE 'YES' END
             AS yes_or_no)
             AS is_nullable,

           CAST(
             CASE WHEN t.typtype = 'd' THEN
               CASE WHEN bt.typelem <> 0 AND bt.typarray = 0 THEN 'ARRAY'
                    WHEN nbt.nspname = 'pg_catalog' THEN format_type(t.typbasetype, null)
                    ELSE 'USER-DEFINED' END
             ELSE
               CASE WHEN t.typelem <> 0 AND t.typarray = 0 THEN 'ARRAY'
                    WHEN nt.nspname = 'pg_catalog' THEN format_type(a.atttypid, null)
                    ELSE 'USER-DEFINED' END
             END
             AS character_data)
             AS data_type,

           CAST(
             _pg_char_max_length(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                 CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS character_maximum_length,

           CAST(
             _pg_char_octet_length(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                   CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS character_octet_length,

           CAST(
             _pg_numeric_precision(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                   CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS numeric_precision,

           CAST(
             _pg_numeric_precision_radix(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                         CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS numeric_precision_radix,

           CAST(
             _pg_numeric_scale(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                               CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS numeric_scale,

           CAST(
             _pg_datetime_precision(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                                    CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS cardinal_number)
             AS datetime_precision,

           CAST(
             _pg_interval_type(CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE a.atttypid END,
                               CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
             AS character_data)
             AS interval_type,
           CAST(null AS cardinal_number) AS interval_precision,

           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(null AS sql_identifier) AS character_set_name,

           CAST(CASE WHEN nco.nspname IS NOT NULL THEN current_database() END AS sql_identifier) AS collation_catalog,
           CAST(nco.nspname AS sql_identifier) AS collation_schema,
           CAST(co.collname AS sql_identifier) AS collation_name,

           CAST(CASE WHEN t.typtype = 'd' THEN current_database() ELSE null END
             AS sql_identifier) AS domain_catalog,
           CAST(CASE WHEN t.typtype = 'd' THEN nt.nspname ELSE null END
             AS sql_identifier) AS domain_schema,
           CAST(CASE WHEN t.typtype = 'd' THEN t.typname ELSE null END
             AS sql_identifier) AS domain_name,

           CAST(current_database() AS sql_identifier) AS udt_catalog,
           CAST(coalesce(nbt.nspname, nt.nspname) AS sql_identifier) AS udt_schema,
           CAST(coalesce(bt.typname, t.typname) AS sql_identifier) AS udt_name,

           CAST(null AS sql_identifier) AS scope_catalog,
           CAST(null AS sql_identifier) AS scope_schema,
           CAST(null AS sql_identifier) AS scope_name,

           CAST(null AS cardinal_number) AS maximum_cardinality,
           CAST(a.attnum AS sql_identifier) AS dtd_identifier,
           CAST('NO' AS yes_or_no) AS is_self_referencing,

           CAST(CASE WHEN a.attidentity IN ('a', 'd') THEN 'YES' ELSE 'NO' END AS yes_or_no) AS is_identity,
           CAST(CASE a.attidentity WHEN 'a' THEN 'ALWAYS' WHEN 'd' THEN 'BY DEFAULT' END AS character_data) AS identity_generation,
           CAST(seq.seqstart AS character_data) AS identity_start,
           CAST(seq.seqincrement AS character_data) AS identity_increment,
           CAST(seq.seqmax AS character_data) AS identity_maximum,
           CAST(seq.seqmin AS character_data) AS identity_minimum,
           CAST(CASE WHEN seq.seqcycle THEN 'YES' ELSE 'NO' END AS yes_or_no) AS identity_cycle,

           CAST(CASE WHEN a.attgenerated <> '' THEN 'ALWAYS' ELSE 'NEVER' END AS character_data) AS is_generated,
           CAST(CASE WHEN a.attgenerated <> '' THEN pg_get_expr(ad.adbin, ad.adrelid) END AS character_data) AS generation_expression,

           CAST(CASE WHEN c.relkind IN ('r', 'p') OR
                          (c.relkind IN ('v', 'f') AND
                           pg_column_is_updatable(c.oid, a.attnum, false))
                THEN 'YES' ELSE 'NO' END AS yes_or_no) AS is_updatable

    FROM (pg_attribute a LEFT JOIN pg_attrdef ad ON attrelid = adrelid AND attnum = adnum)
         JOIN (pg_class c JOIN pg_namespace nc ON (c.relnamespace = nc.oid)) ON a.attrelid = c.oid
         JOIN (pg_type t JOIN pg_namespace nt ON (t.typnamespace = nt.oid)) ON a.atttypid = t.oid
         LEFT JOIN (pg_type bt JOIN pg_namespace nbt ON (bt.typnamespace = nbt.oid))
           ON (t.typtype = 'd' AND t.typbasetype = bt.oid)
         LEFT JOIN (pg_collation co JOIN pg_namespace nco ON (co.collnamespace = nco.oid))
           ON a.attcollation = co.oid AND (nco.nspname, co.collname) <> ('pg_catalog', 'default')
         LEFT JOIN (pg_depend dep JOIN pg_sequence seq ON (dep.classid = 'pg_class'::regclass AND dep.objid = seq.seqrelid AND dep.deptype = 'i'))
           ON (dep.refclassid = 'pg_class'::regclass AND dep.refobjid = c.oid AND dep.refobjsubid = a.attnum)

    WHERE (NOT FALSE)

          AND a.attnum > 0 AND NOT a.attisdropped
          AND c.relkind IN ('r', 'v', 'f', 'p')

          AND (TRUE
               OR TRUE)`},
	{relation: IDConstraintColumnUsage, sql: `SELECT CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(tblschema AS sql_identifier) AS table_schema,
           CAST(tblname AS sql_identifier) AS table_name,
           CAST(colname AS sql_identifier) AS column_name,
           CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(cstrschema AS sql_identifier) AS constraint_schema,
           CAST(cstrname AS sql_identifier) AS constraint_name

    FROM (
        /* check constraints */
        SELECT DISTINCT nr.nspname, r.relname, r.relowner, a.attname, nc.nspname, c.conname
          FROM pg_namespace nr, pg_class r, pg_attribute a, pg_depend d, pg_namespace nc, pg_constraint c
          WHERE nr.oid = r.relnamespace
            AND r.oid = a.attrelid
            AND d.refclassid = 'pg_catalog.pg_class'::regclass
            AND d.refobjid = r.oid
            AND d.refobjsubid = a.attnum
            AND d.classid = 'pg_catalog.pg_constraint'::regclass
            AND d.objid = c.oid
            AND c.connamespace = nc.oid
            AND c.contype = 'c'
            AND r.relkind IN ('r', 'p')
            AND NOT a.attisdropped

        UNION ALL

        /* not-null constraints */
        SELECT DISTINCT nr.nspname, r.relname, r.relowner, a.attname, nc.nspname, c.conname
          FROM pg_namespace nr, pg_class r, pg_attribute a, pg_namespace nc, pg_constraint c
          WHERE nr.oid = r.relnamespace
            AND r.oid = a.attrelid
            AND r.oid = c.conrelid
            AND a.attnum = c.conkey[1]
            AND c.connamespace = nc.oid
            AND c.contype = 'n'
            AND r.relkind in ('r', 'p')
            AND not a.attisdropped

        UNION ALL

        /* unique/primary key/foreign key constraints */
        SELECT nr.nspname, r.relname, r.relowner, a.attname, nc.nspname, c.conname
          FROM pg_namespace nr, pg_class r, pg_attribute a, pg_namespace nc,
               pg_constraint c
          WHERE nr.oid = r.relnamespace
            AND r.oid = a.attrelid
            AND nc.oid = c.connamespace
            AND r.oid = CASE c.contype WHEN 'f' THEN c.confrelid ELSE c.conrelid END
            AND a.attnum = ANY (CASE c.contype WHEN 'f' THEN c.confkey ELSE c.conkey END)
            AND NOT a.attisdropped
            AND c.contype IN ('p', 'u', 'f')
            AND r.relkind IN ('r', 'p')

      ) AS x (tblschema, tblname, tblowner, colname, cstrschema, cstrname)

    WHERE TRUE`},
	{relation: IDConstraintTableUsage, sql: `SELECT CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nr.nspname AS sql_identifier) AS table_schema,
           CAST(r.relname AS sql_identifier) AS table_name,
           CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(nc.nspname AS sql_identifier) AS constraint_schema,
           CAST(c.conname AS sql_identifier) AS constraint_name

    FROM pg_constraint c, pg_namespace nc,
         pg_class r, pg_namespace nr

    WHERE c.connamespace = nc.oid AND r.relnamespace = nr.oid
          AND ( (c.contype = 'f' AND c.confrelid = r.oid)
             OR (c.contype IN ('p', 'u') AND c.conrelid = r.oid) )
          AND r.relkind IN ('r', 'p')
          AND TRUE`},
	{relation: IDDomainConstraints, sql: `SELECT CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(rs.nspname AS sql_identifier) AS constraint_schema,
           CAST(con.conname AS sql_identifier) AS constraint_name,
           CAST(current_database() AS sql_identifier) AS domain_catalog,
           CAST(n.nspname AS sql_identifier) AS domain_schema,
           CAST(t.typname AS sql_identifier) AS domain_name,
           CAST(CASE WHEN condeferrable THEN 'YES' ELSE 'NO' END
             AS yes_or_no) AS is_deferrable,
           CAST(CASE WHEN condeferred THEN 'YES' ELSE 'NO' END

             AS yes_or_no) AS initially_deferred
    FROM pg_namespace rs, pg_namespace n, pg_constraint con, pg_type t
    WHERE rs.oid = con.connamespace
          AND n.oid = t.typnamespace
          AND t.oid = con.contypid
          AND (TRUE
               OR TRUE)`},
	{relation: IDDomainUdtUsage, sql: `SELECT CAST(current_database() AS sql_identifier) AS udt_catalog,
           CAST(nbt.nspname AS sql_identifier) AS udt_schema,
           CAST(bt.typname AS sql_identifier) AS udt_name,
           CAST(current_database() AS sql_identifier) AS domain_catalog,
           CAST(nt.nspname AS sql_identifier) AS domain_schema,
           CAST(t.typname AS sql_identifier) AS domain_name

    FROM pg_type t, pg_namespace nt,
         pg_type bt, pg_namespace nbt

    WHERE t.typnamespace = nt.oid
          AND t.typbasetype = bt.oid
          AND bt.typnamespace = nbt.oid
          AND t.typtype = 'd'
          AND TRUE`},
	{relation: IDDomains, sql: `SELECT CAST(current_database() AS sql_identifier) AS domain_catalog,
           CAST(nt.nspname AS sql_identifier) AS domain_schema,
           CAST(t.typname AS sql_identifier) AS domain_name,

           CAST(
             CASE WHEN t.typelem <> 0 AND t.typarray = 0 THEN 'ARRAY'
                  WHEN nbt.nspname = 'pg_catalog' THEN format_type(t.typbasetype, null)
                  ELSE 'USER-DEFINED' END
             AS character_data)
             AS data_type,

           CAST(
             _pg_char_max_length(t.typbasetype, t.typtypmod)
             AS cardinal_number)
             AS character_maximum_length,

           CAST(
             _pg_char_octet_length(t.typbasetype, t.typtypmod)
             AS cardinal_number)
             AS character_octet_length,

           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(null AS sql_identifier) AS character_set_name,

           CAST(CASE WHEN nco.nspname IS NOT NULL THEN current_database() END AS sql_identifier) AS collation_catalog,
           CAST(nco.nspname AS sql_identifier) AS collation_schema,
           CAST(co.collname AS sql_identifier) AS collation_name,

           CAST(
             _pg_numeric_precision(t.typbasetype, t.typtypmod)
             AS cardinal_number)
             AS numeric_precision,

           CAST(
             _pg_numeric_precision_radix(t.typbasetype, t.typtypmod)
             AS cardinal_number)
             AS numeric_precision_radix,

           CAST(
             _pg_numeric_scale(t.typbasetype, t.typtypmod)
             AS cardinal_number)
             AS numeric_scale,

           CAST(
             _pg_datetime_precision(t.typbasetype, t.typtypmod)
             AS cardinal_number)
             AS datetime_precision,

           CAST(
             _pg_interval_type(t.typbasetype, t.typtypmod)
             AS character_data)
             AS interval_type,
           CAST(null AS cardinal_number) AS interval_precision,

           CAST(t.typdefault AS character_data) AS domain_default,

           CAST(current_database() AS sql_identifier) AS udt_catalog,
           CAST(nbt.nspname AS sql_identifier) AS udt_schema,
           CAST(bt.typname AS sql_identifier) AS udt_name,

           CAST(null AS sql_identifier) AS scope_catalog,
           CAST(null AS sql_identifier) AS scope_schema,
           CAST(null AS sql_identifier) AS scope_name,

           CAST(null AS cardinal_number) AS maximum_cardinality,
           CAST(1 AS sql_identifier) AS dtd_identifier

    FROM (pg_type t JOIN pg_namespace nt ON t.typnamespace = nt.oid)
         JOIN (pg_type bt JOIN pg_namespace nbt ON bt.typnamespace = nbt.oid)
           ON (t.typbasetype = bt.oid AND t.typtype = 'd')
         LEFT JOIN (pg_collation co JOIN pg_namespace nco ON (co.collnamespace = nco.oid))
           ON t.typcollation = co.oid AND (nco.nspname, co.collname) <> ('pg_catalog', 'default')

    WHERE (TRUE
           OR TRUE)`},
	{relation: IDElementTypes, sql: `SELECT CAST(current_database() AS sql_identifier) AS object_catalog,
           CAST(n.nspname AS sql_identifier) AS object_schema,
           CAST(x.objname AS sql_identifier) AS object_name,
           CAST(x.objtype AS character_data) AS object_type,
           CAST(x.objdtdid AS sql_identifier) AS collection_type_identifier,
           CAST(
             CASE WHEN nbt.nspname = 'pg_catalog' THEN format_type(bt.oid, null)
                  ELSE 'USER-DEFINED' END AS character_data) AS data_type,

           CAST(null AS cardinal_number) AS character_maximum_length,
           CAST(null AS cardinal_number) AS character_octet_length,
           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(null AS sql_identifier) AS character_set_name,
           CAST(CASE WHEN nco.nspname IS NOT NULL THEN current_database() END AS sql_identifier) AS collation_catalog,
           CAST(nco.nspname AS sql_identifier) AS collation_schema,
           CAST(co.collname AS sql_identifier) AS collation_name,
           CAST(null AS cardinal_number) AS numeric_precision,
           CAST(null AS cardinal_number) AS numeric_precision_radix,
           CAST(null AS cardinal_number) AS numeric_scale,
           CAST(null AS cardinal_number) AS datetime_precision,
           CAST(null AS character_data) AS interval_type,
           CAST(null AS cardinal_number) AS interval_precision,

           CAST(current_database() AS sql_identifier) AS udt_catalog,
           CAST(nbt.nspname AS sql_identifier) AS udt_schema,
           CAST(bt.typname AS sql_identifier) AS udt_name,

           CAST(null AS sql_identifier) AS scope_catalog,
           CAST(null AS sql_identifier) AS scope_schema,
           CAST(null AS sql_identifier) AS scope_name,

           CAST(null AS cardinal_number) AS maximum_cardinality,
           CAST('a' || CAST(x.objdtdid AS text) AS sql_identifier) AS dtd_identifier

    FROM pg_namespace n, pg_type at, pg_namespace nbt, pg_type bt,
         (
           /* columns, attributes */
           SELECT c.relnamespace, CAST(c.relname AS sql_identifier),
                  CASE WHEN c.relkind = 'c' THEN 'USER-DEFINED TYPE'::text ELSE 'TABLE'::text END,
                  a.attnum, a.atttypid, a.attcollation
           FROM pg_class c, pg_attribute a
           WHERE c.oid = a.attrelid
                 AND c.relkind IN ('r', 'v', 'f', 'c', 'p')
                 AND attnum > 0 AND NOT attisdropped

           UNION ALL

           /* domains */
           SELECT t.typnamespace, CAST(t.typname AS sql_identifier),
                  'DOMAIN'::text, 1, t.typbasetype, t.typcollation
           FROM pg_type t
           WHERE t.typtype = 'd'

           UNION ALL

           /* parameters */
           SELECT pronamespace,
                  CAST(nameconcatoid(proname, oid) AS sql_identifier),
                  'ROUTINE'::text, (ss.x).n, (ss.x).x, 0
           FROM (SELECT p.pronamespace, p.proname, p.oid,
                        _pg_expandarray(coalesce(p.proallargtypes, p.proargtypes::oid[])) AS x
                 FROM pg_proc p) AS ss

           UNION ALL

           /* result types */
           SELECT p.pronamespace,
                  CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier),
                  'ROUTINE'::text, 0, p.prorettype, 0
           FROM pg_proc p

         ) AS x (objschema, objname, objtype, objdtdid, objtypeid, objcollation)
         LEFT JOIN (pg_collation co JOIN pg_namespace nco ON (co.collnamespace = nco.oid))
           ON x.objcollation = co.oid AND (nco.nspname, co.collname) <> ('pg_catalog', 'default')

    WHERE n.oid = x.objschema
          AND at.oid = x.objtypeid
          AND (at.typelem <> 0 AND at.typarray = 0)
          AND at.typelem = bt.oid
          AND nbt.oid = bt.typnamespace`},
	{relation: IDInformationSchemaCatalogName, sql: `SELECT CAST(current_database() AS sql_identifier) AS catalog_name`},
	{relation: IDKeyColumnUsage, sql: `SELECT CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(nc_nspname AS sql_identifier) AS constraint_schema,
           CAST(conname AS sql_identifier) AS constraint_name,
           CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nr_nspname AS sql_identifier) AS table_schema,
           CAST(relname AS sql_identifier) AS table_name,
           CAST(a.attname AS sql_identifier) AS column_name,
           CAST((ss.x).n AS cardinal_number) AS ordinal_position,
           CAST(CASE WHEN contype = 'f' THEN
                       _pg_index_position(ss.conindid, ss.confkey[(ss.x).n])
                     ELSE NULL
                END AS cardinal_number)
             AS position_in_unique_constraint
    FROM pg_attribute a,
         (SELECT r.oid AS roid, r.relname, r.relowner,
                 nc.nspname AS nc_nspname, nr.nspname AS nr_nspname,
                 c.oid AS coid, c.conname, c.contype, c.conindid,
                 c.confkey, c.confrelid,
                 _pg_expandarray(c.conkey) AS x
          FROM pg_namespace nr, pg_class r, pg_namespace nc,
               pg_constraint c
          WHERE nr.oid = r.relnamespace
                AND r.oid = c.conrelid
                AND nc.oid = c.connamespace
                AND c.contype IN ('p', 'u', 'f')
                AND r.relkind IN ('r', 'p')
                AND (NOT FALSE) ) AS ss
    WHERE ss.roid = a.attrelid
          AND a.attnum = (ss.x).x
          AND NOT a.attisdropped
          AND (TRUE
               OR TRUE)`},
	{relation: IDParameters, sql: `SELECT CAST(current_database() AS sql_identifier) AS specific_catalog,
           CAST(n_nspname AS sql_identifier) AS specific_schema,
           CAST(nameconcatoid(proname, p_oid) AS sql_identifier) AS specific_name,
           CAST((ss.x).n AS cardinal_number) AS ordinal_position,
           CAST(
             CASE WHEN proargmodes IS NULL THEN 'IN'
                WHEN proargmodes[(ss.x).n] = 'i' THEN 'IN'
                WHEN proargmodes[(ss.x).n] = 'o' THEN 'OUT'
                WHEN proargmodes[(ss.x).n] = 'b' THEN 'INOUT'
                WHEN proargmodes[(ss.x).n] = 'v' THEN 'IN'
                WHEN proargmodes[(ss.x).n] = 't' THEN 'OUT'
             END AS character_data) AS parameter_mode,
           CAST('NO' AS yes_or_no) AS is_result,
           CAST('NO' AS yes_or_no) AS as_locator,
           CAST(NULLIF(proargnames[(ss.x).n], '') AS sql_identifier) AS parameter_name,
           CAST(
             CASE WHEN t.typelem <> 0 AND t.typarray = 0 THEN 'ARRAY'
                  WHEN nt.nspname = 'pg_catalog' THEN format_type(t.oid, null)
                  ELSE 'USER-DEFINED' END AS character_data)
             AS data_type,
           CAST(null AS cardinal_number) AS character_maximum_length,
           CAST(null AS cardinal_number) AS character_octet_length,
           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(null AS sql_identifier) AS character_set_name,
           CAST(null AS sql_identifier) AS collation_catalog,
           CAST(null AS sql_identifier) AS collation_schema,
           CAST(null AS sql_identifier) AS collation_name,
           CAST(null AS cardinal_number) AS numeric_precision,
           CAST(null AS cardinal_number) AS numeric_precision_radix,
           CAST(null AS cardinal_number) AS numeric_scale,
           CAST(null AS cardinal_number) AS datetime_precision,
           CAST(null AS character_data) AS interval_type,
           CAST(null AS cardinal_number) AS interval_precision,
           CAST(current_database() AS sql_identifier) AS udt_catalog,
           CAST(nt.nspname AS sql_identifier) AS udt_schema,
           CAST(t.typname AS sql_identifier) AS udt_name,
           CAST(null AS sql_identifier) AS scope_catalog,
           CAST(null AS sql_identifier) AS scope_schema,
           CAST(null AS sql_identifier) AS scope_name,
           CAST(null AS cardinal_number) AS maximum_cardinality,
           CAST((ss.x).n AS sql_identifier) AS dtd_identifier,
           CAST(
             CASE WHEN TRUE
                  THEN pg_get_function_arg_default(p_oid, (ss.x).n)
                  ELSE NULL END
             AS character_data) AS parameter_default

    FROM pg_type t, pg_namespace nt,
         (SELECT n.nspname AS n_nspname, p.proname, p.oid AS p_oid, p.proowner,
                 p.proargnames, p.proargmodes,
                 _pg_expandarray(coalesce(p.proallargtypes, p.proargtypes::oid[])) AS x
          FROM pg_namespace n, pg_proc p
          WHERE n.oid = p.pronamespace
                AND (TRUE OR
                     TRUE)) AS ss
    WHERE t.oid = (ss.x).x AND t.typnamespace = nt.oid`},
	{relation: IDReferentialConstraints, sql: `SELECT CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(ncon.nspname AS sql_identifier) AS constraint_schema,
           CAST(con.conname AS sql_identifier) AS constraint_name,
           CAST(

             CASE WHEN npkc.nspname IS NULL THEN NULL
                  ELSE current_database() END
             AS sql_identifier) AS unique_constraint_catalog,
           CAST(npkc.nspname AS sql_identifier) AS unique_constraint_schema,
           CAST(pkc.conname AS sql_identifier) AS unique_constraint_name,

           CAST(
             CASE con.confmatchtype WHEN 'f' THEN 'FULL'
                                    WHEN 'p' THEN 'PARTIAL'
                                    WHEN 's' THEN 'NONE' END
             AS character_data) AS match_option,

           CAST(
             CASE con.confupdtype WHEN 'c' THEN 'CASCADE'
                                  WHEN 'n' THEN 'SET NULL'
                                  WHEN 'd' THEN 'SET DEFAULT'
                                  WHEN 'r' THEN 'RESTRICT'
                                  WHEN 'a' THEN 'NO ACTION' END
             AS character_data) AS update_rule,

           CAST(
             CASE con.confdeltype WHEN 'c' THEN 'CASCADE'
                                  WHEN 'n' THEN 'SET NULL'
                                  WHEN 'd' THEN 'SET DEFAULT'
                                  WHEN 'r' THEN 'RESTRICT'
                                  WHEN 'a' THEN 'NO ACTION' END
             AS character_data) AS delete_rule

    FROM (pg_namespace ncon
          INNER JOIN pg_constraint con ON ncon.oid = con.connamespace
          INNER JOIN pg_class c ON con.conrelid = c.oid AND con.contype = 'f')
         LEFT JOIN pg_depend d1  -- find constraint's dependency on an index
          ON d1.objid = con.oid AND d1.classid = 'pg_constraint'::regclass
             AND d1.refclassid = 'pg_class'::regclass AND d1.refobjsubid = 0
         LEFT JOIN pg_depend d2  -- find pkey/unique constraint for that index
          ON d2.refclassid = 'pg_constraint'::regclass
             AND d2.classid = 'pg_class'::regclass
             AND d2.objid = d1.refobjid AND d2.objsubid = 0
             AND d2.deptype = 'i'
         LEFT JOIN pg_constraint pkc ON pkc.oid = d2.refobjid
            AND pkc.contype IN ('p', 'u')
            AND pkc.conrelid = con.confrelid
         LEFT JOIN pg_namespace npkc ON pkc.connamespace = npkc.oid

    WHERE TRUE
          -- SELECT privilege omitted, per SQL standard
          OR TRUE
          OR TRUE`},
	{relation: IDRoutineColumnUsage, sql: `SELECT DISTINCT
           CAST(current_database() AS sql_identifier) AS specific_catalog,
           CAST(np.nspname AS sql_identifier) AS specific_schema,
           CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier) AS specific_name,
           CAST(current_database() AS sql_identifier) AS routine_catalog,
           CAST(np.nspname AS sql_identifier) AS routine_schema,
           CAST(p.proname AS sql_identifier) AS routine_name,
           CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nt.nspname AS sql_identifier) AS table_schema,
           CAST(t.relname AS sql_identifier) AS table_name,
           CAST(a.attname AS sql_identifier) AS column_name

    FROM pg_namespace np, pg_proc p, pg_depend d,
         pg_class t, pg_namespace nt, pg_attribute a

    WHERE np.oid = p.pronamespace
          AND p.oid = d.objid
          AND d.classid = 'pg_catalog.pg_proc'::regclass
          AND d.refobjid = t.oid
          AND d.refclassid = 'pg_catalog.pg_class'::regclass
          AND t.relnamespace = nt.oid
          AND t.relkind IN ('r', 'v', 'f', 'p')
          AND t.oid = a.attrelid
          AND d.refobjsubid = a.attnum
          AND TRUE`},
	{relation: IDRoutineRoutineUsage, sql: `SELECT DISTINCT
           CAST(current_database() AS sql_identifier) AS specific_catalog,
           CAST(np.nspname AS sql_identifier) AS specific_schema,
           CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier) AS specific_name,
           CAST(current_database() AS sql_identifier) AS routine_catalog,
           CAST(np1.nspname AS sql_identifier) AS routine_schema,
           CAST(nameconcatoid(p1.proname, p1.oid) AS sql_identifier) AS routine_name

    FROM pg_namespace np, pg_proc p, pg_depend d,
         pg_proc p1, pg_namespace np1

    WHERE np.oid = p.pronamespace
          AND p.oid = d.objid
          AND d.classid = 'pg_catalog.pg_proc'::regclass
          AND d.refobjid = p1.oid
          AND d.refclassid = 'pg_catalog.pg_proc'::regclass
          AND p1.pronamespace = np1.oid
          AND p.prokind IN ('f', 'p') AND p1.prokind IN ('f', 'p')
          AND TRUE`},
	{relation: IDRoutineSequenceUsage, sql: `SELECT DISTINCT
           CAST(current_database() AS sql_identifier) AS specific_catalog,
           CAST(np.nspname AS sql_identifier) AS specific_schema,
           CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier) AS specific_name,
           CAST(current_database() AS sql_identifier) AS routine_catalog,
           CAST(np.nspname AS sql_identifier) AS routine_schema,
           CAST(p.proname AS sql_identifier) AS routine_name,
           CAST(current_database() AS sql_identifier) AS sequence_catalog,
           CAST(ns.nspname AS sql_identifier) AS sequence_schema,
           CAST(s.relname AS sql_identifier) AS sequence_name

    FROM pg_namespace np, pg_proc p, pg_depend d,
         pg_class s, pg_namespace ns

    WHERE np.oid = p.pronamespace
          AND p.oid = d.objid
          AND d.classid = 'pg_catalog.pg_proc'::regclass
          AND d.refobjid = s.oid
          AND d.refclassid = 'pg_catalog.pg_class'::regclass
          AND s.relnamespace = ns.oid
          AND s.relkind = 'S'
          AND TRUE`},
	{relation: IDRoutineTableUsage, sql: `SELECT DISTINCT
           CAST(current_database() AS sql_identifier) AS specific_catalog,
           CAST(np.nspname AS sql_identifier) AS specific_schema,
           CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier) AS specific_name,
           CAST(current_database() AS sql_identifier) AS routine_catalog,
           CAST(np.nspname AS sql_identifier) AS routine_schema,
           CAST(p.proname AS sql_identifier) AS routine_name,
           CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nt.nspname AS sql_identifier) AS table_schema,
           CAST(t.relname AS sql_identifier) AS table_name

    FROM pg_namespace np, pg_proc p, pg_depend d,
         pg_class t, pg_namespace nt

    WHERE np.oid = p.pronamespace
          AND p.oid = d.objid
          AND d.classid = 'pg_catalog.pg_proc'::regclass
          AND d.refobjid = t.oid
          AND d.refclassid = 'pg_catalog.pg_class'::regclass
          AND t.relnamespace = nt.oid
          AND t.relkind IN ('r', 'v', 'f', 'p')
          AND TRUE`},
	{relation: IDRoutines, sql: `SELECT CAST(current_database() AS sql_identifier) AS specific_catalog,
           CAST(n.nspname AS sql_identifier) AS specific_schema,
           CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier) AS specific_name,
           CAST(current_database() AS sql_identifier) AS routine_catalog,
           CAST(n.nspname AS sql_identifier) AS routine_schema,
           CAST(p.proname AS sql_identifier) AS routine_name,
           CAST(CASE p.prokind WHEN 'f' THEN 'FUNCTION' WHEN 'p' THEN 'PROCEDURE' END
             AS character_data) AS routine_type,
           CAST(null AS sql_identifier) AS module_catalog,
           CAST(null AS sql_identifier) AS module_schema,
           CAST(null AS sql_identifier) AS module_name,
           CAST(null AS sql_identifier) AS udt_catalog,

           CAST(null AS sql_identifier) AS udt_schema,
           CAST(null AS sql_identifier) AS udt_name,

           CAST(
             CASE WHEN p.prokind = 'p' THEN NULL
                  WHEN t.typelem <> 0 AND t.typarray = 0 THEN 'ARRAY'
                  WHEN nt.nspname = 'pg_catalog' THEN format_type(t.oid, null)
                  ELSE 'USER-DEFINED' END AS character_data)
             AS data_type,
           CAST(null AS cardinal_number) AS character_maximum_length,
           CAST(null AS cardinal_number) AS character_octet_length,
           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(null AS sql_identifier) AS character_set_name,
           CAST(null AS sql_identifier) AS collation_catalog,
           CAST(null AS sql_identifier) AS collation_schema,
           CAST(null AS sql_identifier) AS collation_name,
           CAST(null AS cardinal_number) AS numeric_precision,
           CAST(null AS cardinal_number) AS numeric_precision_radix,
           CAST(null AS cardinal_number) AS numeric_scale,
           CAST(null AS cardinal_number) AS datetime_precision,
           CAST(null AS character_data) AS interval_type,
           CAST(null AS cardinal_number) AS interval_precision,
           CAST(CASE WHEN nt.nspname IS NOT NULL THEN current_database() END AS sql_identifier) AS type_udt_catalog,
           CAST(nt.nspname AS sql_identifier) AS type_udt_schema,
           CAST(t.typname AS sql_identifier) AS type_udt_name,
           CAST(null AS sql_identifier) AS scope_catalog,
           CAST(null AS sql_identifier) AS scope_schema,
           CAST(null AS sql_identifier) AS scope_name,
           CAST(null AS cardinal_number) AS maximum_cardinality,
           CAST(CASE WHEN p.prokind <> 'p' THEN 0 END AS sql_identifier) AS dtd_identifier,

           CAST(CASE WHEN l.lanname = 'sql' THEN 'SQL' ELSE 'EXTERNAL' END AS character_data)
             AS routine_body,
           CAST(
             CASE WHEN TRUE THEN p.prosrc ELSE null END
             AS character_data) AS routine_definition,
           CAST(
             CASE WHEN l.lanname = 'c' THEN p.prosrc ELSE null END
             AS character_data) AS external_name,
           CAST(upper(l.lanname) AS character_data) AS external_language,

           CAST('GENERAL' AS character_data) AS parameter_style,
           CAST(CASE WHEN p.provolatile = 'i' THEN 'YES' ELSE 'NO' END AS yes_or_no) AS is_deterministic,
           CAST('MODIFIES' AS character_data) AS sql_data_access,
           CAST(CASE WHEN p.prokind <> 'p' THEN
             CASE WHEN p.proisstrict THEN 'YES' ELSE 'NO' END END AS yes_or_no) AS is_null_call,
           CAST(null AS character_data) AS sql_path,
           CAST('YES' AS yes_or_no) AS schema_level_routine,
           CAST(0 AS cardinal_number) AS max_dynamic_result_sets,
           CAST(null AS yes_or_no) AS is_user_defined_cast,
           CAST(null AS yes_or_no) AS is_implicitly_invocable,
           CAST(CASE WHEN p.prosecdef THEN 'DEFINER' ELSE 'INVOKER' END AS character_data) AS security_type,
           CAST(null AS sql_identifier) AS to_sql_specific_catalog,
           CAST(null AS sql_identifier) AS to_sql_specific_schema,
           CAST(null AS sql_identifier) AS to_sql_specific_name,
           CAST('NO' AS yes_or_no) AS as_locator,
           CAST(null AS time_stamp) AS created,
           CAST(null AS time_stamp) AS last_altered,
           CAST(null AS yes_or_no) AS new_savepoint_level,
           CAST('NO' AS yes_or_no) AS is_udt_dependent,

           CAST(null AS character_data) AS result_cast_from_data_type,
           CAST(null AS yes_or_no) AS result_cast_as_locator,
           CAST(null AS cardinal_number) AS result_cast_char_max_length,
           CAST(null AS cardinal_number) AS result_cast_char_octet_length,
           CAST(null AS sql_identifier) AS result_cast_char_set_catalog,
           CAST(null AS sql_identifier) AS result_cast_char_set_schema,
           CAST(null AS sql_identifier) AS result_cast_char_set_name,
           CAST(null AS sql_identifier) AS result_cast_collation_catalog,
           CAST(null AS sql_identifier) AS result_cast_collation_schema,
           CAST(null AS sql_identifier) AS result_cast_collation_name,
           CAST(null AS cardinal_number) AS result_cast_numeric_precision,
           CAST(null AS cardinal_number) AS result_cast_numeric_precision_radix,
           CAST(null AS cardinal_number) AS result_cast_numeric_scale,
           CAST(null AS cardinal_number) AS result_cast_datetime_precision,
           CAST(null AS character_data) AS result_cast_interval_type,
           CAST(null AS cardinal_number) AS result_cast_interval_precision,
           CAST(null AS sql_identifier) AS result_cast_type_udt_catalog,
           CAST(null AS sql_identifier) AS result_cast_type_udt_schema,
           CAST(null AS sql_identifier) AS result_cast_type_udt_name,
           CAST(null AS sql_identifier) AS result_cast_scope_catalog,
           CAST(null AS sql_identifier) AS result_cast_scope_schema,
           CAST(null AS sql_identifier) AS result_cast_scope_name,
           CAST(null AS cardinal_number) AS result_cast_maximum_cardinality,
           CAST(null AS sql_identifier) AS result_cast_dtd_identifier

    FROM (pg_namespace n
          JOIN pg_proc p ON n.oid = p.pronamespace
          JOIN pg_language l ON p.prolang = l.oid)
         LEFT JOIN
         (pg_type t JOIN pg_namespace nt ON t.typnamespace = nt.oid)
         ON p.prorettype = t.oid AND p.prokind <> 'p'

    WHERE (TRUE
           OR TRUE)`},
	{relation: IDSchemata, sql: `SELECT CAST(current_database() AS sql_identifier) AS catalog_name,
       CAST(n.nspname AS sql_identifier) AS schema_name,
       CAST(pg_get_userbyid(n.nspowner) AS sql_identifier) AS schema_owner,
       CAST(NULL AS sql_identifier) AS default_character_set_catalog,
       CAST(NULL AS sql_identifier) AS default_character_set_schema,
       CAST(NULL AS sql_identifier) AS default_character_set_name,
       CAST(NULL AS character_data) AS sql_path
FROM pg_namespace n`},
	{relation: IDSequences, sql: `SELECT CAST(current_database() AS sql_identifier) AS sequence_catalog,
           CAST(nc.nspname AS sql_identifier) AS sequence_schema,
           CAST(c.relname AS sql_identifier) AS sequence_name,
           CAST(format_type(s.seqtypid, null) AS character_data) AS data_type,
           CAST(_pg_numeric_precision(s.seqtypid, -1) AS cardinal_number) AS numeric_precision,
           CAST(2 AS cardinal_number) AS numeric_precision_radix,
           CAST(0 AS cardinal_number) AS numeric_scale,
           CAST(s.seqstart AS character_data) AS start_value,
           CAST(s.seqmin AS character_data) AS minimum_value,
           CAST(s.seqmax AS character_data) AS maximum_value,
           CAST(s.seqincrement AS character_data) AS increment,
           CAST(CASE WHEN s.seqcycle THEN 'YES' ELSE 'NO' END AS yes_or_no) AS cycle_option
    FROM pg_namespace nc, pg_class c, pg_sequence s
    WHERE c.relnamespace = nc.oid
          AND c.relkind = 'S'
          AND NOT EXISTS (SELECT 1 FROM pg_depend WHERE classid = 'pg_class'::regclass AND objid = c.oid AND deptype = 'i')
          AND (NOT FALSE)
          AND c.oid = s.seqrelid
          AND (TRUE
               OR TRUE )`},
	{relation: IDTableConstraints, sql: `SELECT CAST(current_database() AS sql_identifier) AS constraint_catalog,
           CAST(nc.nspname AS sql_identifier) AS constraint_schema,
           CAST(c.conname AS sql_identifier) AS constraint_name,
           CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nr.nspname AS sql_identifier) AS table_schema,
           CAST(r.relname AS sql_identifier) AS table_name,
           CAST(
             CASE c.contype WHEN 'c' THEN 'CHECK'
                            WHEN 'n' THEN 'CHECK'
                            WHEN 'f' THEN 'FOREIGN KEY'
                            WHEN 'p' THEN 'PRIMARY KEY'
                            WHEN 'u' THEN 'UNIQUE' END
             AS character_data) AS constraint_type,
           CAST(CASE WHEN c.condeferrable THEN 'YES' ELSE 'NO' END AS yes_or_no)
             AS is_deferrable,
           CAST(CASE WHEN c.condeferred THEN 'YES' ELSE 'NO' END AS yes_or_no)
             AS initially_deferred,
           CAST(CASE WHEN c.conenforced THEN 'YES' ELSE 'NO' END AS yes_or_no) AS enforced,
           CAST(CASE WHEN c.contype = 'u'
                     THEN CASE WHEN (SELECT NOT indnullsnotdistinct FROM pg_index WHERE indexrelid = conindid) THEN 'YES' ELSE 'NO' END
                     END
                AS yes_or_no) AS nulls_distinct

    FROM pg_namespace nc,
         pg_namespace nr,
         pg_constraint c,
         pg_class r

    WHERE nc.oid = c.connamespace AND nr.oid = r.relnamespace
          AND c.conrelid = r.oid
          AND c.contype NOT IN ('t', 'x')  -- ignore nonstandard constraints

          AND r.relkind IN ('r', 'p')
          AND (NOT FALSE)
          AND (TRUE
               -- SELECT privilege omitted, per SQL standard
               OR TRUE
               OR TRUE )`},
	{relation: IDTables, sql: `SELECT CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nc.nspname AS sql_identifier) AS table_schema,
           CAST(c.relname AS sql_identifier) AS table_name,

           CAST(
             CASE WHEN nc.oid = pg_my_temp_schema() THEN 'LOCAL TEMPORARY'
                  WHEN c.relkind IN ('r', 'p') THEN 'BASE TABLE'
                  WHEN c.relkind = 'v' THEN 'VIEW'
                  WHEN c.relkind = 'f' THEN 'FOREIGN'
                  ELSE null END
             AS character_data) AS table_type,

           CAST(null AS sql_identifier) AS self_referencing_column_name,
           CAST(null AS character_data) AS reference_generation,

           CAST(CASE WHEN t.typname IS NOT NULL THEN current_database() ELSE null END AS sql_identifier) AS user_defined_type_catalog,
           CAST(nt.nspname AS sql_identifier) AS user_defined_type_schema,
           CAST(t.typname AS sql_identifier) AS user_defined_type_name,

           CAST(CASE WHEN c.relkind IN ('r', 'p') OR
                          (c.relkind IN ('v', 'f') AND
                           -- 1 << CMD_INSERT
                           pg_relation_is_updatable(c.oid, false) & 8 = 8)
                THEN 'YES' ELSE 'NO' END AS yes_or_no) AS is_insertable_into,

           CAST(CASE WHEN t.typname IS NOT NULL THEN 'YES' ELSE 'NO' END AS yes_or_no) AS is_typed,
           CAST(null AS character_data) AS commit_action

    FROM pg_namespace nc JOIN pg_class c ON (nc.oid = c.relnamespace)
           LEFT JOIN (pg_type t JOIN pg_namespace nt ON (t.typnamespace = nt.oid)) ON (c.reloftype = t.oid)

    WHERE c.relkind IN ('r', 'v', 'f', 'p')
          AND (NOT FALSE)
          AND (TRUE
               OR TRUE
               OR TRUE )`},
	{relation: IDTriggeredUpdateColumns, sql: `SELECT CAST(current_database() AS sql_identifier) AS trigger_catalog,
           CAST(n.nspname AS sql_identifier) AS trigger_schema,
           CAST(t.tgname AS sql_identifier) AS trigger_name,
           CAST(current_database() AS sql_identifier) AS event_object_catalog,
           CAST(n.nspname AS sql_identifier) AS event_object_schema,
           CAST(c.relname AS sql_identifier) AS event_object_table,
           CAST(a.attname AS sql_identifier) AS event_object_column

    FROM pg_namespace n, pg_class c, pg_trigger t,
         (SELECT tgoid, (ta0.tgat).x AS tgattnum, (ta0.tgat).n AS tgattpos
          FROM (SELECT oid AS tgoid, information_schema._pg_expandarray(tgattr) AS tgat FROM pg_trigger) AS ta0) AS ta,
         pg_attribute a

    WHERE n.oid = c.relnamespace
          AND c.oid = t.tgrelid
          AND t.oid = ta.tgoid
          AND (a.attrelid, a.attnum) = (t.tgrelid, ta.tgattnum)
          AND NOT t.tgisinternal
          AND (NOT FALSE)
          AND (TRUE
               -- SELECT privilege omitted, per SQL standard
               OR TRUE )`},
	{relation: IDTriggers, sql: `SELECT CAST(current_database() AS sql_identifier) AS trigger_catalog,
           CAST(n.nspname AS sql_identifier) AS trigger_schema,
           CAST(t.tgname AS sql_identifier) AS trigger_name,
           CAST(em.text AS character_data) AS event_manipulation,
           CAST(current_database() AS sql_identifier) AS event_object_catalog,
           CAST(n.nspname AS sql_identifier) AS event_object_schema,
           CAST(c.relname AS sql_identifier) AS event_object_table,
           CAST(
             -- To determine action order, partition by schema, table,
             -- event_manipulation (INSERT/DELETE/UPDATE), ROW/STATEMENT (1),
             -- BEFORE/AFTER (66), then order by trigger name.  It's preferable
             -- to partition by view output columns, so that query constraints
             -- can be pushed down below the window function.
             rank() OVER (PARTITION BY CAST(n.nspname AS sql_identifier),
                                       CAST(c.relname AS sql_identifier),
                                       em.num,
                                       t.tgtype & 1,
                                       t.tgtype & 66
                                       ORDER BY t.tgname)
             AS cardinal_number) AS action_order,
           CAST(
             CASE WHEN TRUE
               THEN (regexp_match(pg_get_triggerdef(t.oid), E'.{35,} WHEN \\((.+)\\) EXECUTE FUNCTION'))[1]
               ELSE null END
             AS character_data) AS action_condition,
           CAST(
             substring(pg_get_triggerdef(t.oid) from
                       position('EXECUTE FUNCTION' in substring(pg_get_triggerdef(t.oid) from 48)) + 47)
             AS character_data) AS action_statement,
           CAST(
             -- hard-wired reference to TRIGGER_TYPE_ROW
             CASE t.tgtype & 1 WHEN 1 THEN 'ROW' ELSE 'STATEMENT' END
             AS character_data) AS action_orientation,
           CAST(
             -- hard-wired refs to TRIGGER_TYPE_BEFORE, TRIGGER_TYPE_INSTEAD
             CASE t.tgtype & 66 WHEN 2 THEN 'BEFORE' WHEN 64 THEN 'INSTEAD OF' ELSE 'AFTER' END
             AS character_data) AS action_timing,
           CAST(tgoldtable AS sql_identifier) AS action_reference_old_table,
           CAST(tgnewtable AS sql_identifier) AS action_reference_new_table,
           CAST(null AS sql_identifier) AS action_reference_old_row,
           CAST(null AS sql_identifier) AS action_reference_new_row,
           CAST(null AS time_stamp) AS created

    FROM pg_namespace n, pg_class c, pg_trigger t,
         -- hard-wired refs to TRIGGER_TYPE_INSERT, TRIGGER_TYPE_DELETE,
         -- TRIGGER_TYPE_UPDATE; we intentionally omit TRIGGER_TYPE_TRUNCATE
         (VALUES (4, 'INSERT'),
                 (8, 'DELETE'),
                 (16, 'UPDATE')) AS em (num, text)

    WHERE n.oid = c.relnamespace
          AND c.oid = t.tgrelid
          AND t.tgtype & em.num <> 0
          AND NOT t.tgisinternal
          AND (NOT FALSE)
          AND (TRUE
               -- SELECT privilege omitted, per SQL standard
               OR TRUE
               OR TRUE )`},
	{relation: IDUserDefinedTypes, sql: `SELECT CAST(current_database() AS sql_identifier) AS user_defined_type_catalog,
           CAST(n.nspname AS sql_identifier) AS user_defined_type_schema,
           CAST(c.relname AS sql_identifier) AS user_defined_type_name,
           CAST('STRUCTURED' AS character_data) AS user_defined_type_category,
           CAST('YES' AS yes_or_no) AS is_instantiable,
           CAST(null AS yes_or_no) AS is_final,
           CAST(null AS character_data) AS ordering_form,
           CAST(null AS character_data) AS ordering_category,
           CAST(null AS sql_identifier) AS ordering_routine_catalog,
           CAST(null AS sql_identifier) AS ordering_routine_schema,
           CAST(null AS sql_identifier) AS ordering_routine_name,
           CAST(null AS character_data) AS reference_type,
           CAST(null AS character_data) AS data_type,
           CAST(null AS cardinal_number) AS character_maximum_length,
           CAST(null AS cardinal_number) AS character_octet_length,
           CAST(null AS sql_identifier) AS character_set_catalog,
           CAST(null AS sql_identifier) AS character_set_schema,
           CAST(null AS sql_identifier) AS character_set_name,
           CAST(null AS sql_identifier) AS collation_catalog,
           CAST(null AS sql_identifier) AS collation_schema,
           CAST(null AS sql_identifier) AS collation_name,
           CAST(null AS cardinal_number) AS numeric_precision,
           CAST(null AS cardinal_number) AS numeric_precision_radix,
           CAST(null AS cardinal_number) AS numeric_scale,
           CAST(null AS cardinal_number) AS datetime_precision,
           CAST(null AS character_data) AS interval_type,
           CAST(null AS cardinal_number) AS interval_precision,
           CAST(null AS sql_identifier) AS source_dtd_identifier,
           CAST(null AS sql_identifier) AS ref_dtd_identifier

    FROM pg_namespace n, pg_class c, pg_type t

    WHERE n.oid = c.relnamespace
          AND t.typrelid = c.oid
          AND c.relkind = 'c'
          AND (TRUE
               OR TRUE)`},
	{relation: IDViewColumnUsage, sql: `SELECT DISTINCT CAST(current_database() AS sql_identifier) AS view_catalog,
       CAST(nv.nspname AS sql_identifier) AS view_schema,
       CAST(v.relname AS sql_identifier) AS view_name,
       CAST(current_database() AS sql_identifier) AS table_catalog,
       CAST(nt.nspname AS sql_identifier) AS table_schema,
       CAST(t.relname AS sql_identifier) AS table_name,
       CAST(a.attname AS sql_identifier) AS column_name
FROM pg_namespace nv JOIN pg_class v ON nv.oid = v.relnamespace
JOIN pg_depend d ON d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = v.oid
JOIN pg_class t ON d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjid = t.oid
JOIN pg_namespace nt ON t.relnamespace = nt.oid
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
WHERE v.relkind = 'v' AND t.relkind IN ('r', 'v', 'p') AND d.refobjsubid > 0`},
	{relation: IDViewRoutineUsage, sql: `SELECT DISTINCT CAST(current_database() AS sql_identifier) AS table_catalog,
       CAST(nv.nspname AS sql_identifier) AS table_schema,
       CAST(v.relname AS sql_identifier) AS table_name,
       CAST(current_database() AS sql_identifier) AS specific_catalog,
       CAST(np.nspname AS sql_identifier) AS specific_schema,
       CAST(nameconcatoid(p.proname, p.oid) AS sql_identifier) AS specific_name
FROM pg_namespace nv JOIN pg_class v ON nv.oid = v.relnamespace
JOIN pg_depend d ON d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = v.oid
JOIN pg_proc p ON d.refclassid = 'pg_catalog.pg_proc'::regclass AND d.refobjid = p.oid
JOIN pg_namespace np ON p.pronamespace = np.oid
WHERE v.relkind = 'v'`},
	{relation: IDViewTableUsage, sql: `SELECT DISTINCT CAST(current_database() AS sql_identifier) AS view_catalog,
       CAST(nv.nspname AS sql_identifier) AS view_schema,
       CAST(v.relname AS sql_identifier) AS view_name,
       CAST(current_database() AS sql_identifier) AS table_catalog,
       CAST(nt.nspname AS sql_identifier) AS table_schema,
       CAST(t.relname AS sql_identifier) AS table_name
FROM pg_namespace nv JOIN pg_class v ON nv.oid = v.relnamespace
JOIN pg_depend d ON d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = v.oid
JOIN pg_class t ON d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjid = t.oid
JOIN pg_namespace nt ON t.relnamespace = nt.oid
WHERE v.relkind = 'v' AND t.relkind IN ('r', 'v', 'p')`},
	{relation: IDViews, sql: `SELECT CAST(current_database() AS sql_identifier) AS table_catalog,
           CAST(nc.nspname AS sql_identifier) AS table_schema,
           CAST(c.relname AS sql_identifier) AS table_name,

           CAST(
             CASE WHEN TRUE
                  THEN pg_get_viewdef(c.oid)
                  ELSE null END
             AS character_data) AS view_definition,

           CAST(
             CASE WHEN 'check_option=cascaded' = ANY (c.reloptions)
                  THEN 'CASCADED'
                  WHEN 'check_option=local' = ANY (c.reloptions)
                  THEN 'LOCAL'
                  ELSE 'NONE' END
             AS character_data) AS check_option,

           CAST(
             -- (1 << CMD_UPDATE) + (1 << CMD_DELETE)
             CASE WHEN pg_relation_is_updatable(c.oid, false) & 20 = 20
                  THEN 'YES' ELSE 'NO' END
             AS yes_or_no) AS is_updatable,

           CAST(
             -- 1 << CMD_INSERT
             CASE WHEN pg_relation_is_updatable(c.oid, false) & 8 = 8
                  THEN 'YES' ELSE 'NO' END
             AS yes_or_no) AS is_insertable_into,

           CAST(
             -- TRIGGER_TYPE_ROW + TRIGGER_TYPE_INSTEAD + TRIGGER_TYPE_UPDATE
             CASE WHEN EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = c.oid AND tgenabled IN ('O', 'A') AND tgtype & 81 = 81)
                  THEN 'YES' ELSE 'NO' END
           AS yes_or_no) AS is_trigger_updatable,

           CAST(
             -- TRIGGER_TYPE_ROW + TRIGGER_TYPE_INSTEAD + TRIGGER_TYPE_DELETE
             CASE WHEN EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = c.oid AND tgenabled IN ('O', 'A') AND tgtype & 73 = 73)
                  THEN 'YES' ELSE 'NO' END
           AS yes_or_no) AS is_trigger_deletable,

           CAST(
             -- TRIGGER_TYPE_ROW + TRIGGER_TYPE_INSTEAD + TRIGGER_TYPE_INSERT
             CASE WHEN EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = c.oid AND tgenabled IN ('O', 'A') AND tgtype & 69 = 69)
                  THEN 'YES' ELSE 'NO' END
           AS yes_or_no) AS is_trigger_insertable_into

    FROM pg_namespace nc, pg_class c

    WHERE c.relnamespace = nc.oid
          AND c.relkind = 'v'
          AND (NOT FALSE)
          AND (TRUE
               OR TRUE
               OR TRUE )`},
}

// NativeViewSources returns the complete canonical source inventory.
func NativeViewSources() ([]NativeViewSource, error) {
	expected := 0
	for _, relation := range registry {
		if logicalRelationDeclaration(relation) == DeclarationView {
			expected++
		}
	}
	if len(nativeViewSources) != expected {
		return nil, &model.Error{Code: "XX000", Message: "native view source inventory is incomplete"}
	}
	result := make([]NativeViewSource, 0, expected)
	seen := make(map[RelationID]struct{}, expected)
	for _, source := range nativeViewSources {
		if source.relation == 0 || source.sql == "" {
			return nil, &model.Error{Code: "XX000", Message: "native view source inventory is invalid"}
		}
		if _, duplicate := seen[source.relation]; duplicate {
			return nil, &model.Error{Code: "XX000", Message: "native view source inventory contains duplicate identities"}
		}
		seen[source.relation] = struct{}{}
		relation, ok := LookupID(source.relation)
		if !ok || logicalRelationDeclaration(relation) != DeclarationView {
			return nil, &model.Error{Code: "XX000", Message: "native view source relation is unavailable"}
		}
		result = append(result, NativeViewSource{
			Relation: source.relation, Namespace: relation.Namespace, Name: relation.Name,
			SemanticVersion: NativeViewSemanticVersion, SQL: strings.Clone(source.sql),
		})
	}
	return result, nil
}

func canonicalViewSQL(id RelationID) (string, bool) {
	for _, source := range nativeViewSources {
		if source.relation == id {
			return source.sql, true
		}
	}
	return "", false
}
