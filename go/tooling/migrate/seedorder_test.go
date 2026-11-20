package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeSeedTx models just enough: a statement "need:X put:Y" fails with
// errMissing unless X was put, and puts Y; SAVEPOINT / ROLLBACK TO undo the
// puts since the savepoint.
type fakeSeedTx struct {
	have  map[string]bool
	since []string
	log   []string
}

var errMissing = errors.New("missing parent")

func (f *fakeSeedTx) Exec(_ context.Context, sql string, _ ...any) error {
	switch sql {
	case "SAVEPOINT w17_seed_fixture":
		f.since = nil
		return nil
	case "ROLLBACK TO SAVEPOINT w17_seed_fixture":
		for _, k := range f.since {
			delete(f.have, k)
		}
		f.since = nil
		return nil
	case "RELEASE SAVEPOINT w17_seed_fixture":
		return nil
	}
	f.log = append(f.log, sql)
	need, put, _ := strings.Cut(sql, " ")
	if need = strings.TrimPrefix(need, "need:"); need != "-" && !f.have[need] {
		return errMissing
	}
	f.have[strings.TrimPrefix(put, "put:")] = true
	f.since = append(f.since, strings.TrimPrefix(put, "put:"))
	return nil
}

func TestExecSeedFiles_RetriesAFixtureReferencingALaterOne(t *testing.T) {
	deferrable := func(err error) bool { return errors.Is(err, errMissing) }
	tx := &fakeSeedTx{have: map[string]bool{}}
	stmts := []SeedStmt{
		{SQL: "need:org put:host1", File: "a/org-hosts"},
		{SQL: "need:org put:host2", File: "a/org-hosts"},
		{SQL: "need:- put:org", File: "a/acme-org"},
	}
	if err := ExecSeedFiles(context.Background(), tx, stmts, deferrable); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"org", "host1", "host2"} {
		if !tx.have[k] {
			t.Errorf("%s not applied", k)
		}
	}

	// No fixture creates the parent: the deferred one's error, named.
	tx = &fakeSeedTx{have: map[string]bool{}}
	err := ExecSeedFiles(context.Background(), tx, []SeedStmt{{SQL: "need:ghost put:x", File: "a/child"}, {SQL: "need:- put:y", File: "a/other"}}, deferrable)
	if err == nil || !strings.Contains(err.Error(), "fixture a/child, statement 1 of 2") || !errors.Is(err, errMissing) {
		t.Fatalf("err = %v", err)
	}

	// Not deferrable: fails at once, no retry.
	tx = &fakeSeedTx{have: map[string]bool{}}
	err = ExecSeedFiles(context.Background(), tx, stmts, func(error) bool { return false })
	if err == nil || len(tx.log) != 1 {
		t.Fatalf("a non-deferrable error was retried: err = %v, statements run %v", err, tx.log)
	}

	// No File (an old caller): one set, in order, as before.
	tx = &fakeSeedTx{have: map[string]bool{}}
	err = ExecSeedFiles(context.Background(), tx, []SeedStmt{{SQL: "need:- put:a"}, {SQL: "need:a put:b"}}, deferrable)
	if err != nil {
		t.Fatal(err)
	}
}
