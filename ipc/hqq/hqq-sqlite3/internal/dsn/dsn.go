// Package dsn parses the DSN of the hqq-sqlite drivers:
//
//	hqq://<listener>?db=...&mode=...&timeout=...&fetch=...&busy=...&txlock=...&pragma.<name>=<value>
//
// Both drivers accept the same options. The package returns the raw values;
// each driver applies its own defaults and checks its own limits.
package dsn

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mode is the open mode of the database. The zero value is read-write-create.
type Mode uint8

// Open modes.
const (
	ModeReadWriteCreate Mode = iota // mode=rwc
	ModeReadWrite                   // mode=rw
	ModeReadOnly                    // mode=ro
)

// TxLock is the BEGIN mode of transactions. The zero value is deferred.
type TxLock uint8

// Transaction modes.
const (
	TxDeferred TxLock = iota
	TxImmediate
	TxExclusive
)

// Pragma is a PRAGMA to apply when a session opens.
type Pragma struct {
	Name  string
	Value string
}

// Options are the values of a DSN. A field that the DSN does not set is zero.
type Options struct {
	Listener string
	Database string
	Mode     Mode
	Timeout  time.Duration
	Busy     time.Duration
	Fetch    int
	TxLock   TxLock
	Location *time.Location
	Pragmas  []Pragma // sorted by name
}

// Parse parses a DSN of the form hqq://<listener>?<options>. Unknown options
// are errors, so misspelled options are not ignored. Every option and target
// PRAGMA may appear at most once.
func Parse(dsn string) (Options, error) {
	var o Options
	u, err := url.Parse(dsn)
	if err != nil {
		return o, fmt.Errorf("sqlite: invalid DSN: %w", err)
	}
	if u.Scheme != "hqq" {
		return o, fmt.Errorf("sqlite: DSN scheme must be hqq, got %q", u.Scheme)
	}
	if u.Host == "" || u.Path != "" || u.User != nil {
		return o, errors.New("sqlite: DSN must be hqq://<listener>?<options>")
	}
	if err := applyQuery(&o, u.Query(), true); err != nil {
		return o, err
	}
	if o.Database == "" {
		return o, errors.New("sqlite: DSN option db is required")
	}
	o.Listener = u.Host
	return o, nil
}

// ParseFile parses a file-based or :memory: DSN compatible with
// github.com/mattn/go-sqlite3 (such as "app.db?_journal=WAL",
// "file:app.db?_fk=1", or ":memory:").
func ParseFile(dsn string) (Options, error) {
	var o Options
	rawPath, rawQuery, _ := strings.Cut(dsn, "?")
	if idx := strings.Index(rawPath, "://"); idx >= 0 {
		scheme := rawPath[:idx]
		if !strings.EqualFold(scheme, "file") {
			return o, fmt.Errorf("sqlite: unsupported DSN scheme %q", scheme)
		}
		rawPath = rawPath[idx+3:]
		if p, err := url.PathUnescape(rawPath); err == nil {
			rawPath = p
		}
	} else if len(rawPath) >= 5 && strings.EqualFold(rawPath[:5], "file:") {
		rawPath = rawPath[5:]
		if p, err := url.PathUnescape(rawPath); err == nil {
			rawPath = p
		}
	}
	if rawPath == "" || strings.HasPrefix(rawPath, "::") || strings.ContainsRune(rawPath, 0) {
		return o, fmt.Errorf("sqlite: invalid database path in DSN %q", dsn)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return o, fmt.Errorf("sqlite: invalid DSN query: %w", err)
	}
	if err := applyQuery(&o, q, false); err != nil {
		return o, err
	}
	o.Database = rawPath
	return o, nil
}

func applyQuery(o *Options, q url.Values, allowDBKey bool) error {
	var (
		pragmas    []Pragma
		seenPragma = make(map[string]bool)
		seenBusy   bool
		seenTxLock bool
		seenLoc    bool
	)
	addPragma := func(name, val string) error {
		if seenPragma[name] {
			return fmt.Errorf("sqlite: pragma %s specified more than once", name)
		}
		seenPragma[name] = true
		pragmas = append(pragmas, Pragma{Name: name, Value: val})
		return nil
	}

	for key, vals := range q {
		if len(vals) != 1 {
			return fmt.Errorf("sqlite: option %s must be given once", key)
		}
		v := vals[0]
		if strings.HasPrefix(key, "pragma.") {
			if err := addPragma(strings.TrimPrefix(key, "pragma."), v); err != nil {
				return err
			}
			continue
		}
		var err error
		switch key {
		case "db":
			if !allowDBKey {
				return errors.New("sqlite: option db is not allowed in a file DSN")
			}
			o.Database = v
		case "cache":
			switch strings.ToLower(v) {
			case "shared", "private":
			default:
				return fmt.Errorf("sqlite: invalid cache %q", v)
			}
		case "mode":
			switch v {
			case "rwc", "":
				o.Mode = ModeReadWriteCreate
			case "rw":
				o.Mode = ModeReadWrite
			case "ro":
				o.Mode = ModeReadOnly
			default:
				return fmt.Errorf("sqlite: invalid mode %q", v)
			}
		case "timeout":
			o.Timeout, err = parseDuration(key, v)
		case "busy":
			if seenBusy {
				return errors.New("sqlite: busy timeout specified more than once")
			}
			seenBusy = true
			o.Busy, err = parseDuration(key, v)
		case "_busy_timeout", "_timeout":
			if seenBusy {
				return errors.New("sqlite: busy timeout specified more than once")
			}
			seenBusy = true
			o.Busy, err = parseBusyTimeout(key, v)
		case "fetch":
			o.Fetch, err = strconv.Atoi(v)
			if err != nil {
				err = fmt.Errorf("sqlite: invalid fetch %q", v)
			}
		case "txlock", "_txlock":
			if seenTxLock {
				return errors.New("sqlite: txlock specified more than once")
			}
			seenTxLock = true
			switch strings.ToLower(v) {
			case "deferred", "":
				o.TxLock = TxDeferred
			case "immediate":
				o.TxLock = TxImmediate
			case "exclusive":
				o.TxLock = TxExclusive
			default:
				return fmt.Errorf("sqlite: invalid %s %q", key, v)
			}
		case "loc", "_loc":
			if seenLoc {
				return errors.New("sqlite: loc specified more than once")
			}
			seenLoc = true
			o.Location, err = parseLocation(key, v)
		case "_foreign_keys", "_fk":
			err = addBoolPragma(addPragma, key, "foreign_keys", v)
		case "_defer_foreign_keys", "_defer_fk":
			err = addBoolPragma(addPragma, key, "defer_foreign_keys", v)
		case "_case_sensitive_like", "_cslike":
			err = addBoolPragma(addPragma, key, "case_sensitive_like", v)
		case "_ignore_check_constraints":
			err = addBoolPragma(addPragma, key, "ignore_check_constraints", v)
		case "_query_only":
			err = addBoolPragma(addPragma, key, "query_only", v)
		case "_recursive_triggers", "_rt":
			err = addBoolPragma(addPragma, key, "recursive_triggers", v)
		case "_writable_schema":
			err = addBoolPragma(addPragma, key, "writable_schema", v)
		case "_journal_mode", "_journal":
			err = addEnumPragma(addPragma, key, "journal_mode", v, "DELETE", "TRUNCATE", "PERSIST", "MEMORY", "WAL", "OFF")
		case "_synchronous", "_sync":
			err = addEnumPragma(addPragma, key, "synchronous", v, "0", "OFF", "1", "NORMAL", "2", "FULL", "3", "EXTRA")
		case "_auto_vacuum", "_vacuum":
			err = addEnumPragma(addPragma, key, "auto_vacuum", v, "0", "NONE", "1", "FULL", "2", "INCREMENTAL")
		case "_locking_mode", "_locking":
			err = addEnumPragma(addPragma, key, "locking_mode", v, "NORMAL", "EXCLUSIVE")
		case "_temp_store":
			err = addEnumPragma(addPragma, key, "temp_store", v, "0", "DEFAULT", "1", "FILE", "2", "MEMORY")
		case "_secure_delete":
			if strings.EqualFold(v, "fast") {
				err = addPragma("secure_delete", "FAST")
			} else {
				err = addBoolPragma(addPragma, key, "secure_delete", v)
			}
		case "_cache_size":
			if _, perr := strconv.ParseInt(v, 10, 64); perr != nil {
				return fmt.Errorf("sqlite: invalid %s %q", key, v)
			}
			err = addPragma("cache_size", v)
		case "_mmap_size":
			if n, perr := strconv.ParseInt(v, 10, 64); perr != nil || n < 0 {
				return fmt.Errorf("sqlite: invalid %s %q", key, v)
			}
			err = addPragma("mmap_size", v)
		case "_page_size":
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 512 || n > 65536 || n&(n-1) != 0 {
				return fmt.Errorf("sqlite: invalid %s %q", key, v)
			}
			err = addPragma("page_size", v)
		case "_wal_autocheckpoint":
			if _, perr := strconv.ParseInt(v, 10, 32); perr != nil {
				return fmt.Errorf("sqlite: invalid %s %q", key, v)
			}
			err = addPragma("wal_autocheckpoint", v)
		default:
			return fmt.Errorf("sqlite: unknown DSN option %q", key)
		}
		if err != nil {
			return err
		}
	}
	sort.Slice(pragmas, func(i, j int) bool { return pragmas[i].Name < pragmas[j].Name })
	o.Pragmas = pragmas
	return nil
}

func parseDuration(key, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: invalid %s %q: %w", key, v, err)
	}
	return d, nil
}

// parseBusyTimeout parses _busy_timeout or _timeout as integer milliseconds or
// as a Go duration string. A zero or negative millisecond count returns
// -time.Millisecond so Config.BusyTimeout disables waiting rather than selecting
// the default 5s timeout.
func parseBusyTimeout(key, v string) (time.Duration, error) {
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		if ms <= 0 {
			return -time.Millisecond, nil
		}
		return time.Duration(ms) * time.Millisecond, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("sqlite: invalid %s %q", key, v)
	}
	if d <= 0 {
		return -time.Millisecond, nil
	}
	return d, nil
}

func parseLocation(key, v string) (*time.Location, error) {
	if v == "" {
		return nil, fmt.Errorf("sqlite: invalid %s %q", key, v)
	}
	if strings.EqualFold(v, "auto") {
		return time.Local, nil
	}
	if strings.EqualFold(v, "utc") {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(v)
	if err != nil {
		return nil, fmt.Errorf("sqlite: invalid %s %q: %w", key, v, err)
	}
	return loc, nil
}

func addBoolPragma(add func(string, string) error, key, pragma, v string) error {
	switch strings.ToLower(v) {
	case "1", "true", "on", "yes":
		return add(pragma, "1")
	case "0", "false", "off", "no":
		return add(pragma, "0")
	default:
		return fmt.Errorf("sqlite: invalid %s %q", key, v)
	}
}

func addEnumPragma(add func(string, string) error, key, pragma, v string, allowed ...string) error {
	up := strings.ToUpper(v)
	for _, a := range allowed {
		if up == a {
			return add(pragma, up)
		}
	}
	return fmt.Errorf("sqlite: invalid %s %q", key, v)
}

