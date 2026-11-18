// Package sqlitedriver links the SQLite migrator into a binary: importing it
// for its side effect registers SQLite with tooling/migrate/factory.
//
//	import _ "github.com/wandering-compiler/sdk/go/tooling/migrate/sqlitedriver"
//
// The factory does not import it itself, so that a binary migrating only
// PostgreSQL, MySQL or the KV stores does not download and compile
// modernc.org/sqlite (SQLite in pure Go, the largest module the SDK depends
// on). A generated server imports this package when its project declares a
// SQLite connection; w17ctl always does.
package sqlitedriver

import (
	"context"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

func init() {
	factory.RegisterSQLite(
		func(ctx context.Context, dsn string) (migrate.Applier, error) {
			a, err := sqlite.New(ctx, dsn)
			if err != nil {
				return nil, err
			}
			return a, nil
		},
		func(dsn string) (migrate.Snapshotter, error) {
			s, err := sqlite.NewSnapshotter(dsn)
			if err != nil {
				return nil, err
			}
			return s, nil
		},
	)
}
