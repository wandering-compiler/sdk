package mysql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

func TestNew_EmptyDSNRefuses(t *testing.T) {
	_, err := mysql.New(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

func TestNew_BadSchemeRefuses(t *testing.T) {
	_, err := mysql.New(context.Background(), "postgres://x")
	if err == nil || !strings.Contains(err.Error(), "expected mysql:// scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestNew_BogusDSNFails(t *testing.T) {
	_, err := mysql.New(context.Background(),
		"mysql://nobody:nopass@127.0.0.1:1/x?timeout=1s")
	if err == nil {
		t.Fatal("expected connect error against unreachable mysql")
	}
	if !strings.Contains(err.Error(), "mysql.New") {
		t.Errorf("err %q missing mysql.New prefix", err.Error())
	}
}

func TestURLToDriverDSN_FullURL(t *testing.T) {
	got, err := mysql.URLToDriverDSN("mysql://user:pass@localhost:3306/db?charset=utf8mb4")
	if err != nil {
		t.Fatalf("URLToDriverDSN: %v", err)
	}
	// go-sql-driver expects user:pass@tcp(host:port)/db?…
	if !strings.HasPrefix(got, "user:pass@tcp(localhost:3306)/db?") {
		t.Errorf("DSN structure wrong: %q", got)
	}
	// multiStatements=true must be force-set.
	if !strings.Contains(got, "multiStatements=true") {
		t.Errorf("DSN missing multiStatements=true: %q", got)
	}
	// Original param preserved.
	if !strings.Contains(got, "charset=utf8mb4") {
		t.Errorf("DSN dropped user param: %q", got)
	}
}

func TestURLToDriverDSN_ForcesMultiStatementsOverFalse(t *testing.T) {
	got, err := mysql.URLToDriverDSN("mysql://localhost:3306/db?multiStatements=false")
	if err != nil {
		t.Fatalf("URLToDriverDSN: %v", err)
	}
	if !strings.Contains(got, "multiStatements=true") {
		t.Errorf("multiStatements=false not overridden: %q", got)
	}
	if strings.Contains(got, "multiStatements=false") {
		t.Errorf("DSN still carries multiStatements=false: %q", got)
	}
}

func TestURLToDriverDSN_NoUser(t *testing.T) {
	got, err := mysql.URLToDriverDSN("mysql://localhost:3306/db")
	if err != nil {
		t.Fatalf("URLToDriverDSN: %v", err)
	}
	if !strings.HasPrefix(got, "tcp(localhost:3306)/db") {
		t.Errorf("DSN should omit user@ when no userinfo; got %q", got)
	}
}

func TestURLToDriverDSN_NoDatabase(t *testing.T) {
	got, err := mysql.URLToDriverDSN("mysql://user@localhost:3306")
	if err != nil {
		t.Fatalf("URLToDriverDSN: %v", err)
	}
	// Server-level connection (no DB) — accepts trailing /
	if !strings.Contains(got, "/?") && !strings.HasSuffix(got, "multiStatements=true") {
		// fine either way; just sanity-check the path bit
		if !strings.Contains(got, "tcp(localhost:3306)") {
			t.Errorf("missing tcp(host); got %q", got)
		}
	}
}

func TestURLToDriverDSN_BadScheme(t *testing.T) {
	_, err := mysql.URLToDriverDSN("postgres://x")
	if err == nil || !strings.Contains(err.Error(), "expected mysql:// scheme") {
		t.Errorf("expected scheme error, got %v", err)
	}
}

func TestURLToDriverDSN_NoHost(t *testing.T) {
	_, err := mysql.URLToDriverDSN("mysql:///db")
	if err == nil || !strings.Contains(err.Error(), "missing host") {
		t.Errorf("expected missing-host error, got %v", err)
	}
}

func TestURLToDriverDSN_MalformedURL(t *testing.T) {
	_, err := mysql.URLToDriverDSN("://broken")
	if err == nil || !strings.Contains(err.Error(), "parse url") {
		t.Errorf("expected parse error, got %v", err)
	}
}
