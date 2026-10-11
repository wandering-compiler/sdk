package dsnredact

import (
	"net/url"
	"strings"
	"testing"
)

func TestString_MasksCredentials(t *testing.T) {
	cases := map[string]string{
		"postgress://app:s3cret@db:5432/main":         "postgress://app:xxxxx@db:5432/main",
		"mysql://root:p@ss@w0rd@db/x":                 "mysql://root:xxxxx@db/x",
		"redis://:s3cret@cache:6379/0":                "redis://:xxxxx@cache:6379/0",
		"host=db user=app password=s3cret dbname=x":   "host=db user=app password=xxxxx dbname=x",
		"host=db password='s 3cret' dbname=x":         "host=db password=xxxxx dbname=x",
		"pg://db/x?sslmode=require&password=s3cret&a": "pg://db/x?sslmode=require&password=xxxxx&a",
		"postgres://db:5432/main?sslmode=disable":     "postgres://db:5432/main?sslmode=disable",
		"sqlite://./data/app.db":                      "sqlite://./data/app.db",
	}
	for in, want := range cases {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErr_RedactsAURLError(t *testing.T) {
	_, err := url.Parse("postgres://app:s3cret@db:bad port/x")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), "s3cret") {
		t.Skip("the standard library no longer quotes the input; nothing to redact")
	}
	if got := Err(err).Error(); strings.Contains(got, "s3cret") {
		t.Fatalf("password survived: %s", got)
	}
}
