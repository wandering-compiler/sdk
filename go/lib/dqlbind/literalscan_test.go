package dqlbind

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// T1-4 pass #11 — the quote-blind-scanner family.
//
// Three helpers in this package scan emitted SQL byte by byte:
// findValuesTuple, nthPlaceholder (ExpandIn) and renumberPG. Only the first
// ever learned that a single-quoted string literal is not code. Since the
// walker started INLINING DQL string literals verbatim into the emitted SQL
// (`emitLiteral`, LITERAL_KIND_STRING), the other two mis-target on any
// statement whose SQL carries a literal containing `?` (MySQL / SQLite) or
// `$<digit>` (PG) — silently, with the driver satisfied.
//
// These tests pin the RESULT, not the emitted text alone: which row a real
// engine returns (C-F6) and which bytes the statement carries into every
// cloned VALUES tuple (VC-N1).

// TestExpandIn_LiteralQuestionMark_C_F6 replays the exact codegen sequence
// from buildExpandInPreamble (body.go): one ExpandIn call per array binding,
// in DESCENDING binding order, over SQL that a MySQL / SQLite walker emitted
// for
//
//	WHERE u.note = 'why?' AND u.b IN (:l1) AND u.c IN (:l2)
//
// The literal is inlined, so it produces NO binding: `b` is binding 1 and
// `c` is binding 2. Counting `?` bytes blindly makes the literal's `?` the
// first occurrence, so both expansions land one placeholder too early.
func TestExpandIn_LiteralQuestionMark_C_F6(t *testing.T) {
	const base = "SELECT id FROM t WHERE note = 'why?' AND b IN (?) AND c IN (?)"

	cases := []struct {
		name   string
		n1, n2 int
		want   string
	}{
		{
			// The silent window the verifier sharpened: len(first)==1 with
			// ANY len(second)>=2. The blind scan produces
			// `b IN (?, ?) AND c IN (?)` — three placeholders for three
			// args, so no driver error, and the two lists swap members.
			name: "1 and 2 — the silent inversion",
			n1:   1, n2: 2,
			want: "SELECT id FROM t WHERE note = 'why?' AND b IN (?) AND c IN (?, ?)",
		},
		{
			name: "1 and 3",
			n1:   1, n2: 3,
			want: "SELECT id FROM t WHERE note = 'why?' AND b IN (?) AND c IN (?, ?, ?)",
		},
		{
			name: "2 and 2",
			n1:   2, n2: 2,
			want: "SELECT id FROM t WHERE note = 'why?' AND b IN (?, ?) AND c IN (?, ?)",
		},
		{
			// n==0 is the empty-filter semantic. Blindly, occurrence 1 is
			// the literal's `?` and `'why?'` becomes `'whyNULL'` — the
			// stored predicate silently changes meaning.
			name: "first list empty — literal must survive",
			n1:   0, n2: 2,
			want: "SELECT id FROM t WHERE note = 'why?' AND b IN (NULL) AND c IN (?, ?)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql := base
			// Descending binding order, exactly as the codegen preamble emits.
			sql = ExpandIn(sql, 2, tc.n2)
			sql = ExpandIn(sql, 1, tc.n1)
			if sql != tc.want {
				t.Errorf("expansion chain:\n got: %q\nwant: %q", sql, tc.want)
			}
		})
	}
}

// TestExpandIn_LiteralOnlyPlaceholders_Unchanged is the ERROR path: when
// every `?` in the SQL sits inside a string literal there is no placeholder
// to expand, so ExpandIn must report "not found" by returning the SQL
// UNCHANGED (and leave the driver to raise the arg-count mismatch) rather
// than rewriting the literal.
func TestExpandIn_LiteralOnlyPlaceholders_Unchanged(t *testing.T) {
	const sql = "SELECT id FROM t WHERE note = 'why?' AND tag = 'huh?'"
	for _, n := range []int{0, 1, 3} {
		if got := ExpandIn(sql, 1, n); got != sql {
			t.Errorf("ExpandIn(%q, 1, %d) = %q, want unchanged", sql, n, got)
		}
	}
}

// TestExpandIn_LateMaterializeFetch_LiteralInProjection pins the OTHER
// ExpandIn caller: codegen/latemat.go emits
// `dqlbind.ExpandIn(<fetchSQL>, 1, len(ids))` with the occurrence hardcoded
// to 1, over a fetch SELECT whose projection is the child half of the
// original query's target list — i.e. arbitrary computed expressions, which
// may inline a string literal. Blindly, occurrence 1 was the literal's `?`:
// the stored predicate text got rewritten and the real IN placeholder was
// left with len(ids) args (loud for len>=2, accidentally right for len==1).
func TestExpandIn_LateMaterializeFetch_LiteralInProjection(t *testing.T) {
	const fetchSQL = "SELECT c.id, COALESCE(c.body, 'n/a?') AS c_body FROM comments c WHERE c.id IN (?)"
	const want = "SELECT c.id, COALESCE(c.body, 'n/a?') AS c_body FROM comments c WHERE c.id IN (?, ?, ?)"
	if got := ExpandIn(fetchSQL, 1, 3); got != want {
		t.Errorf("late-materialize fetch expansion:\n got: %q\nwant: %q", got, want)
	}
}

// TestNthPlaceholder_SkipsLiterals pins the shared scan directly, including
// SQL's doubled-quote escape (the emitter always doubles an embedded quote —
// see walker.emitLiteral — so a doubled quote must NOT end the literal).
func TestNthPlaceholder_SkipsLiterals(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want int
	}{
		{name: "literal ? not counted", s: "a = 'x?' AND b = ?", n: 1, want: 17},
		{name: "second real placeholder", s: "'?' ? '?' ?", n: 2, want: 10},
		{name: "only literal ? — none found", s: "note = 'why?'", n: 1, want: -1},
		{name: "doubled quote keeps the literal open", s: "note = 'it''s ?' AND b = ?", n: 1, want: 25},
		{name: "n<1 guard", s: "a?b?", n: 0, want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nthPlaceholder(tc.s, tc.n); got != tc.want {
				t.Errorf("nthPlaceholder(%q, %d) = %d, want %d", tc.s, tc.n, got, tc.want)
			}
		})
	}
}

// TestExpandValuesPG_LiteralDollarDigit_VC_N1 is the stored-data corruption:
// a mixed multi-row splat (`INSERT … SET :items[*], note = 'fee $2 extra'`)
// inlines the constant into the VALUES tuple. renumberPG shifted EVERY
// `$<digits>` byte-run in the cloned tuple, including the one inside the
// literal, so rows 2..n stored a DIFFERENT string than row 1 — with matching
// arg counts and no driver error.
func TestExpandValuesPG_LiteralDollarDigit_VC_N1(t *testing.T) {
	const in = "INSERT INTO tasks (owner, price, note) VALUES ($1, $2, 'fee $2 extra')"
	const want = "INSERT INTO tasks (owner, price, note) VALUES ($1, $2, 'fee $2 extra'), " +
		"($3, $4, 'fee $2 extra'), ($5, $6, 'fee $2 extra')"
	if got := ExpandValuesPG(in, 2, 3); got != want {
		t.Errorf("ExpandValuesPG:\n got: %q\nwant: %q", got, want)
	}
}

// TestExpandValuesPG_LiteralDollarDigit_Escaped covers the same shape with a
// doubled quote inside the literal — the escape must not end the literal and
// re-expose the `$2` to renumbering.
func TestExpandValuesPG_LiteralDollarDigit_Escaped(t *testing.T) {
	const in = "INSERT INTO t (a, note) VALUES ($1, 'it''s $1, not $2')"
	const want = "INSERT INTO t (a, note) VALUES ($1, 'it''s $1, not $2'), ($2, 'it''s $1, not $2')"
	if got := ExpandValuesPG(in, 1, 2); got != want {
		t.Errorf("ExpandValuesPG:\n got: %q\nwant: %q", got, want)
	}
}

// TestFindValuesTuple_MarkerInsideLiteral pins the third scanner's share of
// the same rule: `VALUES (` written inside a string literal is data, not the
// clause. The emitted INSERT shape puts the real clause first, so this is
// the primitive's contract rather than a reachable defect today — it is
// pinned so the shared scan cannot regress into the byte-blind form.
func TestFindValuesTuple_MarkerInsideLiteral(t *testing.T) {
	const sql = "INSERT INTO t (note, a) SELECT 'VALUES (x)', 1 FROM d WHERE k = 'v' UNION SELECT 'y', 2 FROM e"
	if _, _, ok := findValuesTuple(sql); ok {
		t.Error("a `VALUES (` inside a string literal must not be taken for the tuple")
	}
}

// TestExpandIn_LiveSQLite_C_F6 is the live half: a real SQLite engine, the
// shipped expansion chain, and an assertion on WHICH ROW comes back — the
// only assertion that catches this class, because the emitted SQL is
// well-formed and the arg count matches either way.
//
// Fixture (verifier C's): rows (1,'why?',10,20) and (2,'why?',20,99), args
// [10 20 99] for l1=[10], l2=[20,99].
//
//	correct : b IN (10)    AND c IN (20, 99) → id 1
//	blind   : b IN (10, 20) AND c IN (99)    → id 2
//
// Engine is modernc.org/sqlite (pure Go, in-process) — a real SQLite, no
// container needed; the same driver the generated SQLite bundles link.
func TestExpandIn_LiveSQLite_C_F6(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "c_f6.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, stmt := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, note TEXT NOT NULL, b INTEGER NOT NULL, c INTEGER NOT NULL)",
		"INSERT INTO t (id, note, b, c) VALUES (1, 'why?', 10, 20), (2, 'why?', 20, 99)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	l1 := []any{10}
	l2 := []any{20, 99}

	query := "SELECT id FROM t WHERE note = 'why?' AND b IN (?) AND c IN (?)"
	query = ExpandIn(query, 2, len(l2))
	query = ExpandIn(query, 1, len(l1))

	args := append(append([]any{}, l1...), l2...)
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var got []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("SQLite returned %v, want [1] — the IN lists were expanded onto the wrong placeholders (sql: %q)", got, query)
	}
}
