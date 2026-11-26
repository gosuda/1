# Security, Object RBAC, `SECURITY DEFINER` & Row-Level Security (RLS)

SuperSQL implements PostgreSQL-compatible MVCC roles, object-level `GRANT`/`REVOKE`, ownership transfer (`ALTER ... OWNER TO`), `SECURITY DEFINER` routines, native Row-Level Security (RLS), and custom session GUCs.

The default built-in superuser role is `supersql` (active on fresh in-process connections until `SET ROLE` or `pgwire` role authentication is used). All role definitions, ACLs, owners, and RLS policies are stored transactionally in the MVCC catalog and included in hot/incremental backups.

---

## 1. Roles & Membership

### Synopsis
```sql
CREATE ROLE name [ [ WITH ] { SUPERUSER | NOSUPERUSER | BYPASSRLS | NOBYPASSRLS | IN ROLE role [, ...] | ROLE role [, ...] } ... ]
ALTER ROLE name [ [ WITH ] { SUPERUSER | NOSUPERUSER | BYPASSRLS | NOBYPASSRLS } ... ]
DROP ROLE [ IF EXISTS ] name [, ...]

GRANT role_name [, ...] TO role_name [, ...]
REVOKE role_name [, ...] FROM role_name [, ...]

SET [ LOCAL ] ROLE { role_name | NONE }
RESET ROLE
```

- Role memberships are evaluated **transitively** for both object privileges (`GRANT`) and RLS policy applicability.
- Active roles and policies can be inspected via `pg_catalog.pg_roles` and `pg_catalog.pg_policies`.

---

## 2. Object-Level `GRANT` / `REVOKE` & `ALTER ... OWNER TO`

### Synopsis
```sql
GRANT { SELECT | INSERT | UPDATE | DELETE | TRUNCATE | REFERENCES | TRIGGER | ALL [ PRIVILEGES ] } [, ...]
    ON { [ TABLE ] table_name [, ...] | ALL TABLES IN SCHEMA schema_name [, ...] }
    TO { role_name | PUBLIC | CURRENT_USER | CURRENT_ROLE } [, ...]
    [ WITH GRANT OPTION ]

GRANT { USAGE | SELECT | UPDATE | ALL [ PRIVILEGES ] } [, ...]
    ON { SEQUENCE seq_name [, ...] | ALL SEQUENCES IN SCHEMA schema_name [, ...] }
    TO grantee [, ...] [ WITH GRANT OPTION ]

GRANT { USAGE | CREATE | ALL [ PRIVILEGES ] } [, ...]
    ON SCHEMA schema_name [, ...]
    TO grantee [, ...] [ WITH GRANT OPTION ]

GRANT { EXECUTE | ALL [ PRIVILEGES ] } [, ...]
    ON { { FUNCTION | PROCEDURE | ROUTINE } name ( [ argtype [, ...] ] ) [, ...]
       | ALL { FUNCTIONS | PROCEDURES | ROUTINES } IN SCHEMA schema_name [, ...] }
    TO grantee [, ...] [ WITH GRANT OPTION ]

REVOKE [ GRANT OPTION FOR ] privileges ON target FROM grantee [, ...] [ CASCADE | RESTRICT ]

ALTER { TABLE | VIEW | MATERIALIZED VIEW | SEQUENCE | SCHEMA | FUNCTION | PROCEDURE | ROUTINE } name
    OWNER TO new_owner

CREATE SCHEMA [ IF NOT EXISTS ] schema_name AUTHORIZATION role_name
```

### Security Rules Enforced
- **Default Privileges**: `public` schema grants `USAGE` to `PUBLIC`; newly created functions/procedures grant `EXECUTE` to `PUBLIC` unless revoked; tables, views, and sequences deny non-owners until explicitly granted (`42501`).
- **Grantor-Scoped `REVOKE`**: Non-owners holding `WITH GRANT OPTION` can revoke only grants they themselves issued; they cannot revoke grants made by the object owner or superuser.
- **DDL Ownership**: `CREATE INDEX`, `DROP INDEX`, `ALTER INDEX`, `DROP TABLE/VIEW/SEQUENCE/TYPE/COLLATION`, and `ALTER ... OWNER TO` enforce fail-closed ownership checks (`42501`).
- **Excluded (`0A000`)**: `ALTER DEFAULT PRIVILEGES`, `DROP OWNED`, `REASSIGN OWNED`, `SECURITY LABEL`, `SET SESSION AUTHORIZATION`.

### Example: Object Grants, Ownership & Grantor Isolation

<!-- doctest -->
```sql
CREATE ROLE app_reader;
CREATE ROLE app_writer;
CREATE ROLE alice IN ROLE app_reader;

CREATE TABLE secrets (
    id INT PRIMARY KEY,
    body TEXT NOT NULL
);
INSERT INTO secrets VALUES (1, 'confidential');

GRANT SELECT ON secrets TO app_reader;
GRANT SELECT, INSERT, UPDATE ON secrets TO app_writer;

BEGIN;
SET LOCAL ROLE alice;
-- Succeeds via transitive membership in app_reader
SELECT body FROM secrets WHERE id = 1;
COMMIT;

-- Ownership transfer
ALTER TABLE secrets OWNER TO app_writer;
```

---

## 3. `SECURITY DEFINER` Routines

Routines declared with `SECURITY DEFINER` execute with the privileges of the routine's **owner** rather than the caller, and unconditionally restore the caller's role when the routine returns (even on errors or nested calls).

- Inside a `SECURITY DEFINER` frame, `SET ROLE`, `RESET ROLE`, and `RESET ALL` are rejected with SQLSTATE `0A000` (matching PostgreSQL 18).
- Visible in `pg_catalog.pg_proc.prosecdef` and `information_schema.routines.security_type`.

<!-- doctest -->
```sql
CREATE ROLE svc_caller;

CREATE TABLE audit_log (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor TEXT NOT NULL,
    action TEXT NOT NULL
);

-- svc_caller has NO direct table privileges on audit_log
CREATE FUNCTION record_audit(p_action TEXT) RETURNS BIGINT
LANGUAGE sql
SECURITY DEFINER
AS $$
    INSERT INTO audit_log (actor, action)
    VALUES ('svc', p_action)
    RETURNING id;
$$;

BEGIN;
SET LOCAL ROLE svc_caller;
SELECT record_audit('login');
COMMIT;
```

---

## 4. Native MVCC Row-Level Security (RLS) & Multi-Tenant GUCs

### Synopsis
```sql
ALTER TABLE table_name { ENABLE | DISABLE | FORCE | NO FORCE } ROW LEVEL SECURITY

CREATE POLICY policy_name ON table_name
    [ AS { PERMISSIVE | RESTRICTIVE } ]
    [ FOR { ALL | SELECT | INSERT | UPDATE | DELETE } ]
    [ TO { role_name | PUBLIC | CURRENT_USER | CURRENT_ROLE } [, ...] ]
    [ USING ( using_expr ) ]
    [ WITH CHECK ( check_expr ) ]

ALTER POLICY policy_name ON table_name [ TO ... ] [ USING (...) ] [ WITH CHECK (...) ]
DROP POLICY [ IF EXISTS ] policy_name ON table_name
```

- Policies are compiled inside a **`SecurityBarrier`** so user-supplied leaky operators cannot bypass `USING` filters via predicate pushdown.
- Table owners bypass RLS unless `FORCE ROW LEVEL SECURITY` is enabled; `SUPERUSER` and `BYPASSRLS` roles bypass RLS.
- Pair RLS with transaction-scoped custom GUCs (`SET LOCAL app.tenant_id = '...'` + `current_setting('app.tenant_id', true)`).

<!-- doctest -->
```sql
CREATE ROLE tenant_app;

CREATE TABLE notes (
    id INT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    body TEXT NOT NULL
);

INSERT INTO notes VALUES
    (1, 'acme', 'Acme roadmap'),
    (2, 'globex', 'Globex roadmap');

GRANT SELECT, INSERT, UPDATE, DELETE ON notes TO tenant_app;

ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE notes FORCE ROW LEVEL SECURITY;

CREATE POLICY notes_tenant_isolation ON notes
    FOR ALL
    TO tenant_app
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

BEGIN;
SET LOCAL ROLE tenant_app;
SET LOCAL app.tenant_id = 'acme';
SELECT id, body FROM notes;
COMMIT;
```
