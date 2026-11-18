package migratecli

// The tests here migrate SQLite stores; the migrator is linked in the way a
// generated server of a SQLite project links it.
import _ "github.com/wandering-compiler/sdk/go/tooling/migrate/sqlitedriver"
