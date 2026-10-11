package factory

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// SeedCandidate nominates exactly the stores whose applier can seed. A store
// nominated without seed support is picked as the seed target and then
// refused, or counted as a second candidate and demands a --connection that
// can never work.
func TestSeedCandidate_MatchesSeedCapable(t *testing.T) {
	cases := []struct {
		dsn     string
		applier migrate.Applier
	}{
		{"postgres://u@h/db", (*postgres.Applier)(nil)},
		{"postgresql://u@h/db", (*postgres.Applier)(nil)},
		{"mysql://u@h/db", (*mysql.Applier)(nil)},
		{"sqlite:///tmp/x.db", (*sqlite.Applier)(nil)},
		{"file:x.db", (*sqlite.Applier)(nil)},
		{"redis://h:6379", (*redis.Applier)(nil)},
		{"nats://h:4222", (*nats.Applier)(nil)},
		{"s3://bucket", (*s3.Applier)(nil)},
	}
	for _, c := range cases {
		_, canSeed := c.applier.(migrate.SeedCapable)
		if got := SeedCandidate(c.dsn); got != canSeed {
			t.Errorf("SeedCandidate(%q) = %v, but its applier %T can seed: %v", c.dsn, got, c.applier, canSeed)
		}
	}
}
