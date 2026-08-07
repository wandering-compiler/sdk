package factory_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

func TestParseTargets_Happy(t *testing.T) {
	got, err := factory.ParseTargets([]string{
		"main=postgres://localhost/orders",
		"cache=redis://localhost:6379/0",
	})
	if err != nil {
		t.Fatalf("ParseTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Connection != "main" || got[0].DSN != "postgres://localhost/orders" {
		t.Errorf("[0] = %+v", got[0])
	}
	if got[1].Connection != "cache" || got[1].DSN != "redis://localhost:6379/0" {
		t.Errorf("[1] = %+v", got[1])
	}
}

func TestParseTargets_TrimsWhitespace(t *testing.T) {
	got, err := factory.ParseTargets([]string{"  main  =  postgres://x  "})
	if err != nil {
		t.Fatalf("ParseTargets: %v", err)
	}
	if got[0].Connection != "main" || got[0].DSN != "postgres://x" {
		t.Errorf("trim mismatch: %+v", got[0])
	}
}

func TestParseTargets_RejectsMissingEquals(t *testing.T) {
	_, err := factory.ParseTargets([]string{"main"})
	if err == nil || !strings.Contains(err.Error(), "expected <connection>=<dsn>") {
		t.Errorf("expected format error, got %v", err)
	}
}

func TestParseTargets_RejectsEmptyName(t *testing.T) {
	_, err := factory.ParseTargets([]string{"=postgres://x"})
	if err == nil || !strings.Contains(err.Error(), "connection name is empty") {
		t.Errorf("expected empty-name error, got %v", err)
	}
}

func TestParseTargets_RejectsEmptyDSN(t *testing.T) {
	_, err := factory.ParseTargets([]string{"main="})
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

func TestParseTargets_RejectsDuplicateConnection(t *testing.T) {
	_, err := factory.ParseTargets([]string{
		"main=postgres://a",
		"main=postgres://b",
	})
	if err == nil || !strings.Contains(err.Error(), "already specified") {
		t.Errorf("expected duplicate-connection error, got %v", err)
	}
}

// TestFromTargets_UnknownConnection — applier factory rejects
// connections not declared via --target so the orchestrator
// doesn't silently skip an unconfigured backend.
func TestFromTargets_UnknownConnection(t *testing.T) {
	af := factory.FromTargets(nil)
	_, err := af("missing")
	if err == nil || !strings.Contains(err.Error(), `no --target configured`) {
		t.Errorf("expected unknown-connection error, got %v", err)
	}
}

// TestFromTargets_UnknownDSNScheme — DSN whose prefix matches no
// registered scheme errors loud (avoids silently routing to a
// random Applier).
func TestFromTargets_UnknownDSNScheme(t *testing.T) {
	af := factory.FromTargets([]factory.TargetSpec{{Connection: "main", DSN: "weird://target"}})
	_, err := af("main")
	if err == nil || !strings.Contains(err.Error(), "unrecognised DSN scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

// TestFromTargets_AllDialectsRoute — every supported scheme
// reaches its dialect-specific package. Pings (PG/MySQL/SQLite)
// or DSN parse (Redis/NATS/S3) may surface an error against
// unreachable / minimal DSNs; we just confirm the route lands.
func TestFromTargets_AllDialectsRoute(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]bool{ // dsn → expectErr
		"postgres://nobody@127.0.0.1:1/x?connect_timeout=1": true,  // unreachable PG
		"mysql://nobody@127.0.0.1:1/x?timeout=1s":           true,  // unreachable MySQL
		"sqlite://" + dir + "/test.db":                      false, // creates file
		"redis://localhost:6379":                            false, // parse-only at New time
		"rediss://localhost:6380":                           false,
		"nats://nats.local:4222":                            false,
		"s3://my-bucket":                                    false,
	}
	for dsn, expectErr := range cases {
		t.Run(dsn, func(t *testing.T) {
			af := factory.FromTargets([]factory.TargetSpec{{Connection: "c", DSN: dsn}})
			a, err := af("c")
			if expectErr {
				if err == nil {
					t.Fatalf("expected error for %s", dsn)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", dsn, err)
			}
			_ = a.Close()
		})
	}
}

// TestFromTargets_MySQL_ConnectError — mysql:// route reaches
// mysql.New which converts URL → driver DSN + dials. A bogus
// host fails fast.
func TestFromTargets_MySQL_ConnectError(t *testing.T) {
	af := factory.FromTargets([]factory.TargetSpec{
		{Connection: "main", DSN: "mysql://nobody:nopass@127.0.0.1:1/x?timeout=1s"},
	})
	_, err := af("main")
	if err == nil {
		t.Fatal("expected connect error for unreachable mysql")
	}
	if !strings.Contains(err.Error(), "mysql.New") {
		t.Errorf("err %q missing mysql.New prefix", err.Error())
	}
}

// TestFromTargets_SQLite_OpensFile — sqlite:// / file: routes
// open the database file (sqlite creates it on first connect
// when the parent dir exists). Use a temp dir so no on-disk
// pollution.
func TestFromTargets_SQLite_OpensFile(t *testing.T) {
	dir := t.TempDir()
	af := factory.FromTargets([]factory.TargetSpec{
		{Connection: "main", DSN: "sqlite://" + dir + "/test.db"},
	})
	a, err := af("main")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestFromTargets_PostgresDSN_ConnectError — postgres:// route
// reaches postgres.New, which tries to dial. A bogus DSN errors;
// the wrapping says "postgres" so the caller can see the
// dialect.
func TestFromTargets_PostgresDSN_ConnectError(t *testing.T) {
	af := factory.FromTargets([]factory.TargetSpec{
		{Connection: "main", DSN: "postgres://nobody:nopass@127.0.0.1:1/no_such_db?connect_timeout=1"},
	})
	_, err := af("main")
	if err == nil {
		t.Fatal("expected connect error against an unreachable PG")
	}
	if !strings.Contains(err.Error(), "postgres.New") {
		t.Errorf("err %q should carry postgres.New prefix", err.Error())
	}
}

// TestFromTargets_WithParallelOption — WithParallel option is
// threaded through; the resulting Applier accepts the
// per-CLI parallel override (D-iter3-18). For dialects that
// don't implement SetParallelOverride (e.g. PG), the option
// is silently ignored — tests just confirm the factory
// doesn't error when the option is supplied.
func TestFromTargets_WithParallelOption(t *testing.T) {
	// Redis applier accepts the option (it has SetParallelOverride).
	af := factory.FromTargets([]factory.TargetSpec{
		{Connection: "kv", DSN: "redis://127.0.0.1:1/0"},
	}, factory.WithParallel(8))
	a, err := af("kv")
	if err != nil {
		t.Fatalf("FromTargets with WithParallel(redis): %v", err)
	}
	_ = a.Close()

	// S3 applier also accepts.
	af = factory.FromTargets([]factory.TargetSpec{
		{Connection: "blob", DSN: "s3://bucket?endpoint=http://127.0.0.1:1"},
	}, factory.WithParallel(8))
	a, err = af("blob")
	if err != nil {
		t.Fatalf("FromTargets with WithParallel(s3): %v", err)
	}
	_ = a.Close()
}

// TestSnapshotterFromTargets_UnknownConnection — snapshot factory
// rejects connections not declared via --target, mirroring the
// applier factory's loud-refusal posture.
func TestSnapshotterFromTargets_UnknownConnection(t *testing.T) {
	sf := factory.SnapshotterFromTargets(nil)
	_, err := sf("missing")
	if err == nil || !strings.Contains(err.Error(), `no --target configured`) {
		t.Errorf("expected unknown-connection error, got %v", err)
	}
}

// TestSnapshotterFromTargets_Postgres — postgres:// routes to the PG
// Snapshotter, which constructs without dialing (pg_dump / psql
// connect lazily), so even an unreachable DSN yields a usable adapter.
func TestSnapshotterFromTargets_Postgres(t *testing.T) {
	sf := factory.SnapshotterFromTargets([]factory.TargetSpec{
		{Connection: "main", DSN: "postgres://nobody@127.0.0.1:1/x"},
	})
	s, err := sf("main")
	if err != nil {
		t.Fatalf("SnapshotterFromTargets(postgres): %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Snapshotter")
	}
}

// TestSnapshotterFromTargets_AllDialectsRoute — every supported scheme
// now reaches its dialect Snapshotter. Construction is lazy for the
// network stores (redis/nats/s3 parse the DSN but don't dial) and the
// file store (sqlite), so each routes without a live backend. mysql
// parses the URL eagerly; sqlite refuses in-memory — both given valid
// shapes here.
func TestSnapshotterFromTargets_AllDialectsRoute(t *testing.T) {
	dir := t.TempDir()
	for _, dsn := range []string{
		"postgres://nobody@127.0.0.1:1/x",
		"mysql://u:p@127.0.0.1:3306/x",
		"sqlite://" + dir + "/x.db",
		"redis://127.0.0.1:6379",
		"nats://127.0.0.1:4222",
		"s3://bucket",
	} {
		t.Run(dsn, func(t *testing.T) {
			sf := factory.SnapshotterFromTargets([]factory.TargetSpec{{Connection: "c", DSN: dsn}})
			s, err := sf("c")
			if err != nil {
				t.Fatalf("route %s: %v", dsn, err)
			}
			if s == nil {
				t.Fatalf("route %s: nil Snapshotter", dsn)
			}
		})
	}
}

// TestSnapshotExt — dialect → dump file extension mapping used to
// name on-disk snapshots. SQL dialects → sql; sqlite → sqlite;
// schemaless stores → gob; unknown → dump.
func TestSnapshotExt(t *testing.T) {
	cases := map[string]string{
		"postgres://h/d": "sql",
		"mysql://h/d":    "sql",
		"sqlite:///x.db": "sqlite",
		"redis://h":      "gob",
		"nats://h":       "gob",
		"s3://b":         "gob",
		"weird://x":      "dump",
	}
	for dsn, want := range cases {
		if got := factory.SnapshotExt(dsn); got != want {
			t.Errorf("SnapshotExt(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// TestSnapshotterFromTargets_UnknownDSNScheme — a scheme matching no
// dialect errors loud rather than routing to a random adapter.
func TestSnapshotterFromTargets_UnknownDSNScheme(t *testing.T) {
	sf := factory.SnapshotterFromTargets([]factory.TargetSpec{{Connection: "main", DSN: "weird://target"}})
	_, err := sf("main")
	if err == nil || !strings.Contains(err.Error(), "unrecognised DSN scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

// TestFromTargets_NATSAndRedisAndS3 — the remaining DSN
// schemes the factory dispatches on (Phase E.C). Each one
// successfully constructs an Applier (parsed DSN; lazy
// connect happens at first call, not at New).
func TestFromTargets_NATSAndRedisAndS3(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{"redis", "redis://127.0.0.1:6379/0"},
		{"nats", "nats://127.0.0.1:4222"},
		{"s3", "s3://bucket?region=us-east-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			af := factory.FromTargets([]factory.TargetSpec{{Connection: "x", DSN: c.dsn}})
			a, err := af("x")
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			_ = a.Close()
		})
	}
}
