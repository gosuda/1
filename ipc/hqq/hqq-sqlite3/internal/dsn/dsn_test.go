package dsn

import (
	"fmt"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	o, err := Parse("hqq://svc?db=/p/q.db&mode=ro&timeout=2s&fetch=10&busy=-1s&txlock=EXCLUSIVE&pragma.synchronous=NORMAL&pragma.journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	want := Options{
		Listener: "svc", Database: "/p/q.db", Mode: ModeReadOnly, Timeout: 2 * time.Second, Fetch: 10,
		Busy: -time.Second, TxLock: TxExclusive,
		Pragmas: []Pragma{{"journal_mode", "WAL"}, {"synchronous", "NORMAL"}},
	}
	if fmt.Sprint(o) != fmt.Sprint(want) {
		t.Fatalf("got %+v, want %+v", o, want)
	}
	o, err = Parse("hqq://svc?db=main")
	if err != nil || o.Database != "main" || o.Mode != ModeReadWriteCreate || o.TxLock != TxDeferred {
		t.Fatalf("defaults: %+v, %v", o, err)
	}
}

func TestParseControlKnobs(t *testing.T) {
	raw := "hqq://svc?db=main&_busy_timeout=2500&_txlock=immediate&_loc=UTC&_fk=true&_defer_fk=on" +
		"&_journal=wal&_sync=normal&_vacuum=incremental&_cache_size=-4000&_cslike=1" +
		"&_ignore_check_constraints=false&_locking=exclusive&_query_only=no&_rt=yes" +
		"&_secure_delete=fast&_temp_store=memory&_writable_schema=0&_mmap_size=65536" +
		"&_page_size=4096&_wal_autocheckpoint=1000"
	o, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if o.Busy != 2500*time.Millisecond || o.TxLock != TxImmediate || o.Location != time.UTC {
		t.Fatalf("unexpected header options: %+v", o)
	}
	wantPragmas := []Pragma{
		{"auto_vacuum", "INCREMENTAL"},
		{"cache_size", "-4000"},
		{"case_sensitive_like", "1"},
		{"defer_foreign_keys", "1"},
		{"foreign_keys", "1"},
		{"ignore_check_constraints", "0"},
		{"journal_mode", "WAL"},
		{"locking_mode", "EXCLUSIVE"},
		{"mmap_size", "65536"},
		{"page_size", "4096"},
		{"query_only", "0"},
		{"recursive_triggers", "1"},
		{"secure_delete", "FAST"},
		{"synchronous", "NORMAL"},
		{"temp_store", "MEMORY"},
		{"wal_autocheckpoint", "1000"},
		{"writable_schema", "0"},
	}
	if fmt.Sprint(o.Pragmas) != fmt.Sprint(wantPragmas) {
		t.Fatalf("pragmas:\ngot  %+v\nwant %+v", o.Pragmas, wantPragmas)
	}

	oZero, err := Parse("hqq://svc?db=main&_timeout=0&_loc=auto")
	if err != nil {
		t.Fatal(err)
	}
	if oZero.Busy >= 0 || oZero.Location != time.Local {
		t.Fatalf("expected disabled busy (<0) and Local location, got busy=%v loc=%v", oZero.Busy, oZero.Location)
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"postgres://svc?db=x", "hqq://?db=x", "hqq://svc", "hqq://svc?db=x&bogus=1", "hqq://svc?db=x&fetch=many",
		"hqq://svc?db=x&mode=nope", "hqq://svc?db=x&timeout=soon", "hqq://svc?db=x&txlock=sometimes",
		"hqq://svc/path?db=x", "hqq://svc?db=a&db=b", "hqq://u@svc?db=x", "hqq://svc?db=x&pragma.a=1&pragma.a=2",
		"hqq://svc?db=x&_fk=1&_foreign_keys=1", "hqq://svc?db=x&_fk=1&pragma.foreign_keys=1",
		"hqq://svc?db=x&busy=1s&_busy_timeout=1000", "hqq://svc?db=x&txlock=deferred&_txlock=immediate",
		"hqq://svc?db=x&loc=UTC&_loc=UTC", "hqq://svc?db=x&_loc=NoSuch/Zone", "hqq://svc?db=x&_loc=",
		"hqq://svc?db=x&_journal=bogus", "hqq://svc?db=x&_fk=maybe", "hqq://svc?db=x&_page_size=1000",
		"hqq://svc?db=x&_mmap_size=-1", "hqq://svc?db=x&_cache_size=abc", "hqq://svc?db=x&_busy_timeout=nope",
		"::bad",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
}

