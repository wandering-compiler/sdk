package factory

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

const secret = "s3cretPW"

// A DSN that fails to route or parse still has to be named in the error so
// the operator can find it — but never with its password.
func TestErrors_NeverPrintTheDSNPassword(t *testing.T) {
	typo := "postgress://app:" + secret + "@db:5432/main"
	specs := []TargetSpec{{Connection: "main", DSN: typo}}

	_, err := FromTargets(specs)("main")
	assertRedacted(t, "FromTargets", err, "postgress://app:")
	_, err = SnapshotterFromTargets(specs)("main")
	assertRedacted(t, "SnapshotterFromTargets", err, "postgress://app:")

	_, err = ParseTargets([]string{"main=" + typo, "main=" + typo})
	assertRedacted(t, "ParseTargets duplicate", err, "already specified")
	_, err = ParseTargets([]string{"=" + typo})
	assertRedacted(t, "ParseTargets empty name", err, "connection name is empty")

	// Malformed URLs: the standard library quotes the whole input.
	bad := func(scheme string) string { return scheme + "://app:" + secret + "@db:bad port/x" }
	_, err = mysql.URLToDriverDSN(bad("mysql"))
	assertRedacted(t, "mysql.URLToDriverDSN", err, "parse url")
	_, err = mysql.NewSnapshotter(bad("mysql"))
	assertRedacted(t, "mysql.NewSnapshotter", err, "parse url")
	_, err = s3.ParseDSN(bad("s3"))
	assertRedacted(t, "s3.ParseDSN", err, "parse url")
	_, err = nats.DSNToServer(bad("nats"))
	assertRedacted(t, "nats.DSNToServer", err, "parse url")
	_, err = redis.ParseDSN(bad("redis"))
	assertRedacted(t, "redis.ParseDSN", err, "redis")
}

func assertRedacted(t *testing.T, what string, err error, mustMention string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected an error", what)
		return
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Errorf("%s: error prints the password: %s", what, msg)
	}
	if !strings.Contains(msg, mustMention) {
		t.Errorf("%s: error lost its context %q: %s", what, mustMention, msg)
	}
}
