# Data Types & Pure-Go ICU Collations

SuperSQL supports PostgreSQL 18 scalar, structured, JSONB, array, range, full-text search, and user-defined types, plus **pure-Go ICU/locale collations** powered by `golang.org/x/text/collate` with `CGO_ENABLED=0`.

---

## 1. Supported Data Types

| Family | Types |
| :--- | :--- |
| **Integers & Floats** | `smallint` (`int2`), `integer` (`int4`), `bigint` (`int8`), `real` (`float4`), `double precision` (`float8`), `numeric(p, s)` / `decimal` |
| **Text & Binary** | `text`, `varchar(n)`, `char(n)` / `bpchar`, `name`, `bytea`, `bit(n)`, `varbit(n)` |
| **Boolean & Identity** | `boolean`, `uuid`, `oid`, `regclass`, `regtype`, `regproc`, `regprocedure`, `regnamespace`, `regrole` |
| **Date & Time** | `date`, `time`, `timetz`, `timestamp`, `timestamptz`, `interval` |
| **Structured & Semi-Structured** | `json`, `jsonb`, multi-dimensional arrays `T[]`, `tsvector`, `tsquery`, `int4range`, `int8range`, `numrange`, `tsrange`, `tstzrange`, `daterange` (plus multiranges) |
| **Network & Geometry** | `inet`, `cidr`, `macaddr`, `macaddr8`, `point`, `line`, `lseg`, `box`, `path`, `polygon`, `circle` |
| **User-Defined** | `CREATE TYPE ... AS ENUM (...)`, `CREATE TYPE ... AS (field type, ...)`, `CREATE TYPE ... AS RANGE (...)`, `CREATE DOMAIN ... [CHECK (...)]` |
| **Excluded (`0A000`)** | `xml`, `tid`, `xid`, `xid8`, `cid`, `aclitem` |

<!-- doctest -->
```sql
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');
CREATE DOMAIN positive_cents AS BIGINT CHECK (VALUE > 0);

CREATE TABLE shipments (
    id UUID PRIMARY KEY,
    status order_status NOT NULL,
    amount positive_cents NOT NULL,
    window_slot DATERANGE NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);

INSERT INTO shipments VALUES (
    '550e8400-e29b-41d4-a716-446655440000',
    'paid',
    4500,
    '[2026-05-01, 2026-05-10)'::daterange,
    '{"carrier":"fastship"}'::jsonb
);

SELECT status, amount, window_slot @> DATE '2026-05-05' AS in_window
FROM shipments;
```

---

## 2. Pure-Go ICU & Locale Collations

SuperSQL includes deterministic binary collations (`"C"`, `"POSIX"`, `"default"`) plus **pure-Go Unicode/ICU collations** (UCA sort keys, case-insensitive matching, numeric ordering, and `hashtext` support) with zero CGo or external `libicu` dependencies.

### Built-In Predefined ICU Collations

| Collation Name | Locale / Options | Deterministic? | Behavior |
| :--- | :--- | :--- | :--- |
| `"unicode"` / `"pg_unicode_fast"` | `und` | Yes | Standard Unicode collation order |
| `"und-x-icu"` | `und` | Yes | Root UCA collation |
| `"en-US-x-icu"` | `en-US` | Yes | English linguistic order (`'a' < 'A' < 'b' < 'B'`) |
| `"ko-KR-x-icu"` | `ko-KR` | Yes | Korean Hangul collation (`가나다 < 나비 < 바다 < 하늘`) |
| `"ja-JP-x-icu"` | `ja-JP` | Yes | Japanese collation |
| `"de-DE-x-icu"`, `"fr-FR-x-icu"`, `"es-ES-x-icu"`, `"zh-CN-x-icu"` | BCP-47 | Yes | Locale-aware linguistic collation |
| `"und-u-ks-level2"`, `"en-US-u-ks-level2"`, `"ko-KR-u-ks-level2"` | `ks-level2` | **No** | Case-insensitive equality & index lookups (`'Alice' = 'alice'`) |
| `"und-u-kn-true"`, `"en-US-u-kn-true"` | `kn-true` | Yes | Natural numeric ordering (`'item2' < 'item10'`) |

### `CREATE COLLATION` & `DROP COLLATION`

```sql
CREATE COLLATION [ IF NOT EXISTS ] [ schema. ] name (
    PROVIDER = icu,
    LOCALE = '<bcp47-locale-with-optional-u-extensions>'
    [, DETERMINISTIC = { true | false } ]
)

CREATE COLLATION [ IF NOT EXISTS ] [ schema. ] name FROM existing_collation

DROP COLLATION [ IF EXISTS ] [ schema. ] name [, ...] [ CASCADE | RESTRICT ]
```

Supported BCP-47 `-u-` extension tags:
- `-u-ks-level1` (ignore case & accents), `-u-ks-level2` (case-insensitive), `-u-ks-level3` / `level4` / `identic`
- `-u-kn-true` (numeric collation)
- `-u-kc-false` (ignore case)

Excluded (`0A000`): `PROVIDER = libc`, custom `RULES`, `ALTER COLLATION`, `CREATE CONVERSION`.

### Example: Linguistic Ordering, Numeric Sort & Case-Insensitive Columns/Indexes

<!-- doctest -->
```sql
-- Predefined linguistic and natural numeric ordering
SELECT v FROM (VALUES ('b'), ('A'), ('a'), ('B')) AS t(v)
ORDER BY v COLLATE "en-US-x-icu";

SELECT v FROM (VALUES ('item10'), ('item2'), ('item1')) AS t(v)
ORDER BY v COLLATE "en-US-u-kn-true";

-- Custom case-insensitive collation on a column + B-tree index
CREATE COLLATION ci_en (
    provider = icu,
    locale = 'en-US-u-ks-level2',
    deterministic = false
);

CREATE TABLE users_ci (
    id INT PRIMARY KEY,
    email TEXT COLLATE ci_en NOT NULL
);
CREATE INDEX idx_users_ci_email ON users_ci (email);

INSERT INTO users_ci VALUES
    (1, 'Alice@Example.com'),
    (2, 'bob@example.com'),
    (3, 'ALICE@example.com');

SELECT count(*) FROM users_ci WHERE email = 'alice@example.com';

-- Non-deterministic ICU hashing is consistent with equality
SELECT hashtext('Alice' COLLATE ci_en) = hashtext('alice' COLLATE ci_en) AS hash_eq;

DROP COLLATION IF EXISTS unused_coll;
```
