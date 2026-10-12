package dbsession

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Every applier, applied twice, adds nothing the second time.
func TestApply_IsIdempotent(t *testing.T) {
	for name, tc := range map[string]struct {
		apply func(string) string
		dsn   string
	}{
		"sqlite bare":       {ApplySQLitePragmas, "file:app.db"},
		"sqlite query":      {ApplySQLitePragmas, "file:app.db?_pragma=busy_timeout(5000)"},
		"mysql bare":        {ApplyMySQLDSNParams, "user:pw@tcp(db:3306)/app"},
		"mysql query":       {ApplyMySQLDSNParams, "user:pw@tcp(db:3306)/app?tls=true"},
		"postgres url":      {ApplyPGDSNParams, "postgres://u:pw@db:5432/app"},
		"postgres url+q":    {ApplyPGDSNParams, "postgres://u:pw@db:5432/app?sslmode=disable"},
		"postgres keywords": {ApplyPGDSNParams, "host=db user=u dbname=app"},
	} {
		once := tc.apply(tc.dsn)
		if twice := tc.apply(once); twice != once {
			t.Errorf("%s: second apply changed the DSN\n once:  %s\n twice: %s", name, once, twice)
		}
	}
}

// A setting is recognised by its NAME in the parameter list. The same text
// inside a password is credentials, and must not suppress the pin.
func TestApply_NameInsideAPasswordDoesNotSuppressThePin(t *testing.T) {
	got := ApplyMySQLDSNParams("app:pw_collation=x@tcp(db:3306)/app")
	if !strings.Contains(got, "/app?") || !strings.Contains(got[strings.LastIndex(got, "/"):], "collation=utf8mb4_0900_as_cs") {
		t.Errorf("MySQL: a password containing `collation=` suppressed the pin: %q", got)
	}

	got = ApplyPGDSNParams("host=db user=u password=search_path=x dbname=app")
	named, ok := pgKeywords(got)
	if !ok || named["search_path"] != "public" || named["password"] != "search_path=x" {
		t.Errorf("PostgreSQL keyword form: a password containing `search_path=` suppressed the pin: %q", got)
	}

	got = ApplyPGDSNParams("postgres://u:" + url.QueryEscape("search_path=x") + "@db/app")
	u, err := url.Parse(got)
	if err != nil || u.Query().Get("search_path") != "public" {
		t.Errorf("PostgreSQL URL form: a password containing `search_path=` suppressed the pin: %q", got)
	}
}

// A `?` in a MySQL password is not the start of the parameter list: the pins
// must go after the database name, or they become part of it.
func TestApplyMySQLDSNParams_QuestionMarkInPasswordDoesNotStartTheQuery(t *testing.T) {
	got := ApplyMySQLDSNParams("app:p?w@tcp(db:3306)/app")
	if !strings.HasPrefix(got, "app:p?w@tcp(db:3306)/app?") {
		t.Fatalf("the parameter list was not opened after the database name: %q", got)
	}
	if named, _ := mysqlParams(got); named["parseTime"] != "true" {
		t.Errorf("parseTime not in the parameter list: %q", got)
	}
}

// A search path set through `options` — URL-encoded, as a URL carries it —
// is the operator's, and no second `search_path` startup parameter goes next
// to it.
func TestApplyPGDSNParams_SearchPathInOptionsIsKept(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:pw@db/app?options=-c%20search_path%3Dtenant",
		"postgres://u:pw@db/app?options=-csearch_path%3Dtenant",
		"postgres://u:pw@db/app?options=--search-path%3Dtenant",
		"host=db dbname=app options='-c search_path=tenant'",
	} {
		got := ApplyPGDSNParams(dsn)
		if strings.Contains(got, "search_path=public") {
			t.Errorf("%q: a second search_path was appended: %q", dsn, got)
		}
		if !strings.Contains(got, "standard_conforming_strings=on") {
			t.Errorf("%q: the other pin was lost: %q", dsn, got)
		}
	}
}

// PostgreSQL: an operator's parameter keeps its value in both forms.
func TestApplyPGDSNParams_KeepsAnOperatorParameter(t *testing.T) {
	got := ApplyPGDSNParams("postgres://u:pw@db/app?search_path=tenant#frag")
	if strings.Count(got, "search_path=") != 1 || !strings.Contains(got, "search_path=tenant") {
		t.Errorf("URL form: %q", got)
	}
	if !strings.HasSuffix(got, "#frag") {
		t.Errorf("URL form: the fragment no longer ends the DSN: %q", got)
	}
	got = ApplyPGDSNParams("host=db search_path = 'tenant' dbname=app")
	if strings.Count(got, "search_path") != 1 {
		t.Errorf("keyword form: %q", got)
	}
}

// SQLite: an operator's explicit pragma wins, as a chosen setting does on
// the other two dialects, and the difference is logged — the parameter, never
// the DSN.
func TestApplySQLitePragmas_OperatorPragmaWinsAndIsLogged(t *testing.T) {
	logs := captureLogs(t)
	for _, dsn := range []string{
		"file:secret-name.db?_pragma=foreign_keys(0)",
		"file:secret-name.db?_pragma=foreign_keys%3D0",
		"file:secret-name.db?_pragma=main.foreign_keys%20%3D%20off",
	} {
		got := ApplySQLitePragmas(dsn)
		if strings.Contains(got, "foreign_keys(1)") {
			t.Errorf("%q: the pin was appended after the operator's pragma: %q", dsn, got)
		}
		if !strings.Contains(got, "case_sensitive_like(1)") || !strings.Contains(got, "_time_format=sqlite") {
			t.Errorf("%q: the other pins were lost: %q", dsn, got)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "pragma:foreign_keys") || !strings.Contains(out, "sqlite") {
		t.Errorf("the kept operator pragma was not logged: %q", out)
	}
	if strings.Contains(out, "secret-name") {
		t.Errorf("the log carries the DSN: %q", out)
	}
	if n := strings.Count(out, "pragma:foreign_keys"); n != 1 {
		t.Errorf("logged %d times, want once per process", n)
	}

	// The same value spelled differently is not a difference.
	if got := ApplySQLitePragmas("file:a.db?_pragma=foreign_keys%3DON"); strings.Contains(got, "foreign_keys(1)") {
		t.Errorf("an equal operator value was overridden: %q", got)
	}
}

// SQLite pins: foreign keys on, LIKE case-sensitive, time format set — on a
// bare DSN and next to an operator's unrelated pragma.
func TestApplySQLitePragmas_PinsABareDSN(t *testing.T) {
	for _, dsn := range []string{"file:a.db", "file:a.db?_pragma=busy_timeout(5000)"} {
		got := ApplySQLitePragmas(dsn)
		for _, p := range SQLitePragmas() {
			if !strings.Contains(got, p) {
				t.Errorf("%q: missing %s in %q", dsn, p, got)
			}
		}
		if strings.Count(got, "?") != 1 {
			t.Errorf("%q: malformed query string %q", dsn, got)
		}
	}
}

var logMu sync.Mutex

// captureLogs routes slog's default logger into a buffer for the test and
// clears the once-per-process memory so the test sees its own lines.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logMu.Lock()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	reported.Clear()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		reported.Clear()
		logMu.Unlock()
	})
	return &buf
}
