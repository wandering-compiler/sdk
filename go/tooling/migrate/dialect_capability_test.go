package migrate_test

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

// A caller reading live schemas skips a Schemaless store in silence and must
// refuse a schema-ful store that cannot Observe — an unreported schema-ful
// store would be treated as already converged and never receive its schema.
// So no dialect may be both, and only the stores that hold no SQL schema may
// carry the marker: marking MySQL or SQLite Schemaless would turn that loud
// refusal into a silent freeze.
func TestDialects_SchemalessOnlyWhereThereIsNoSchema(t *testing.T) {
	cases := []struct {
		name       string
		applier    any
		schemaless bool
		observes   bool
	}{
		{"postgres", (*postgres.Applier)(nil), false, true},
		{"mysql", (*mysql.Applier)(nil), false, false},
		{"sqlite", (*sqlite.Applier)(nil), false, false},
		{"redis", (*redis.Applier)(nil), true, false},
		{"nats", (*nats.Applier)(nil), true, false},
		{"s3", (*s3.Applier)(nil), true, false},
	}
	for _, c := range cases {
		_, schemaless := c.applier.(migrate.Schemaless)
		_, observes := c.applier.(migrate.ObserveCapable)
		if schemaless != c.schemaless {
			t.Errorf("%s: Schemaless = %v, want %v", c.name, schemaless, c.schemaless)
		}
		if observes != c.observes {
			t.Errorf("%s: ObserveCapable = %v, want %v", c.name, observes, c.observes)
		}
		if schemaless && observes {
			t.Errorf("%s is both Schemaless and ObserveCapable", c.name)
		}
	}
}
