package sqlitecollate

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Compare runs on every connection's goroutine at once; under contention it
// returns exactly what it returns alone.
func TestCompare_SafeForConcurrentUse(t *testing.T) {
	words := []string{"café", "cafe", "Café", "CAFÉ", "naïve", "naive", "Ångström", "angstrom", "zebra", "Zebra", "ab", "a\u200bb", "résumé", "resume"}
	want := make(map[[2]string]int)
	for _, a := range words {
		for _, b := range words {
			want[[2]string{a, b}] = Compare(a, b)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan string, 32)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				for i, a := range words {
					b := words[(i+g+round)%len(words)]
					if got := Compare(a, b); got != want[[2]string{a, b}] {
						select {
						case errs <- a + " vs " + b:
						default:
						}
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent Compare(%s) disagreed with the serial result", e)
	}
}

// The read side accepts a database that carries no stamp — none yet, or a
// stamp table with no row — and refuses one built by different Unicode
// tables, naming both fingerprints and the fix.
func TestVerifyStamp_UnstampedAcceptedForeignTablesReported(t *testing.T) {
	Register()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "stamp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := VerifyStamp(ctx, db); err != nil {
		t.Fatalf("no stamp table: %v, want accepted", err)
	}
	if _, err := db.Exec(`CREATE TABLE ` + StampTable + ` (id INTEGER PRIMARY KEY CHECK (id = 1), fingerprint TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStamp(ctx, db); err != nil {
		t.Fatalf("empty stamp table: %v, want accepted", err)
	}

	const foreign = "w17c1:9.0.0:9.0.0:deadbeefdeadbeef"
	if _, err := db.Exec(`INSERT INTO `+StampTable+` VALUES (1, ?)`, foreign); err != nil {
		t.Fatal(err)
	}
	err = VerifyStamp(ctx, db)
	if err == nil {
		t.Fatal("a database built by different Unicode tables was accepted")
	}
	for _, want := range []string{foreign, Fingerprint(), "fix:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}
}

// Two appliers stamping one fresh database at once both succeed: the stamp
// is written with INSERT OR IGNORE and read back, so the one that loses the
// race reads the winner's stamp instead of failing on the primary key.
func TestEnsureStamp_TwoWritersOnAFreshDatabaseBothSucceed(t *testing.T) {
	Register()
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "stamp.db")
		const writers = 6
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		start := make(chan struct{})
		for w := 0; w < writers; w++ {
			db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.PingContext(ctx); err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- EnsureStamp(ctx, db)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: a concurrent EnsureStamp failed: %v", round, err)
			}
		}
	}
}
