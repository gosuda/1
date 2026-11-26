# PL/pgSQL, SQL Routines & Triggers

SuperSQL supports stored functions, procedures (`CALL`), anonymous `DO` blocks, and row/statement triggers written in **`SQL`** or **`PL/pgSQL`** with full cancellation support (`statement_timeout` and Go `context.Context`).

---

## 1. Functions, Procedures & `DO` Blocks

### Synopsis
```sql
CREATE [ OR REPLACE ] FUNCTION name ( [ [ IN | OUT | INOUT | VARIADIC ] [ argname ] argtype [ DEFAULT expr ] [, ...] ] )
    [ RETURNS { rettype | SETOF rettype | TABLE ( col type [, ...] ) } ]
    LANGUAGE { SQL | PLPGSQL }
    [ IMMUTABLE | STABLE | VOLATILE ]
    [ STRICT | CALLED ON NULL INPUT ]
    [ SECURITY { INVOKER | DEFINER } ]
    AS $$ body $$

CREATE [ OR REPLACE ] PROCEDURE name ( ... )
    LANGUAGE { SQL | PLPGSQL }
    [ SECURITY { INVOKER | DEFINER } ]
    AS $$ body $$

CALL procedure_name ( [ arg [, ...] ] )

DO [ LANGUAGE plpgsql ] $$ body $$
```

### Supported PL/pgSQL Constructs
- `DECLARE` variables (including `table.col%TYPE` and `table%ROWTYPE`), constants, defaults
- Control flow: `IF / ELSIF / ELSE`, `CASE`, `LOOP`, `WHILE`, `FOR i IN a..b`, `FOR rec IN SELECT ...`, `EXIT`, `CONTINUE`
- `SELECT ... INTO [STRICT]`, `PERFORM`, `EXECUTE` dynamic SQL (`USING`), `RETURN`, `RETURN NEXT`, `RETURN QUERY`
- `BEGIN ... EXCEPTION WHEN <sqlstate_or_condition> THEN ... END;` **subtransactions** (automatically roll back uncommitted DML, deferred constraints, advisory locks, session settings, and `pg_notify()` emissions inside the failed block)
- `RAISE NOTICE / WARNING / EXCEPTION [USING ERRCODE = ..., DETAIL = ..., HINT = ...]`
- **SuperSQL Superset**: `%TYPE` column references are supported directly inside routine parameter and return signatures.
- **Excluded (`0A000`)**: Untrusted/external languages (`C`, `plpython3u`, `plperl`, etc.), `TRANSFORM`, and `CREATE EVENT TRIGGER`.

### Example: PL/pgSQL Function, `EXCEPTION` Subtransaction & Procedure `CALL`

<!-- doctest -->
```sql
CREATE TABLE ledger (
    id INT PRIMARY KEY,
    balance INT NOT NULL CHECK (balance >= 0)
);

INSERT INTO ledger VALUES (1, 100), (2, 50);

CREATE PROCEDURE transfer(p_from INT, p_to INT, p_amt INT)
LANGUAGE plpgsql
AS $$
BEGIN
    UPDATE ledger SET balance = balance - p_amt WHERE id = p_from;
    UPDATE ledger SET balance = balance + p_amt WHERE id = p_to;
END;
$$;

CALL transfer(1, 2, 30);
SELECT id, balance FROM ledger ORDER BY id;

-- Subtransaction rollback on caught CHECK violation
CREATE FUNCTION safe_withdraw(p_id INT, p_amt INT) RETURNS BOOLEAN
LANGUAGE plpgsql
AS $$
BEGIN
    BEGIN
        UPDATE ledger SET balance = balance - p_amt WHERE id = p_id;
        RETURN true;
    EXCEPTION WHEN check_violation THEN
        RETURN false;
    END;
END;
$$;

SELECT safe_withdraw(1, 1000) AS overdraw_ok,
       safe_withdraw(1, 20)   AS valid_ok;
```

---

## 2. Triggers

### Synopsis
```sql
CREATE [ OR REPLACE ] TRIGGER name
    { BEFORE | AFTER | INSTEAD OF } { INSERT | UPDATE [ OF col [, ...] ] | DELETE | TRUNCATE } [ OR ... ]
    ON table_or_view
    [ FOR [ EACH ] { ROW | STATEMENT } ]
    [ WHEN ( condition ) ]
    EXECUTE { FUNCTION | PROCEDURE } trigger_func ( [ args ] )

ALTER TABLE table_name { ENABLE | DISABLE } TRIGGER { trigger_name | ALL | USER }
DROP TRIGGER [ IF EXISTS ] name ON table_name
```

<!-- doctest -->
```sql
CREATE TABLE items (
    id INT PRIMARY KEY,
    name TEXT NOT NULL,
    updated_count INT NOT NULL DEFAULT 0
);

CREATE FUNCTION bump_updated_count() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_count := OLD.updated_count + 1;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_items_bump
    BEFORE UPDATE ON items
    FOR EACH ROW
    EXECUTE FUNCTION bump_updated_count();

INSERT INTO items (id, name) VALUES (1, 'widget');
UPDATE items SET name = 'widget-v2' WHERE id = 1;
SELECT name, updated_count FROM items WHERE id = 1;
```
