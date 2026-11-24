package postgres

import "testing"

// The resume reads the index name, whether it is schema-qualified, and the
// table it is built on, so it can look for the index in the table's schema.
func TestCreateIndexConcurrentlyRe_ReadsIndexQualifierAndTable(t *testing.T) {
	cases := []struct{ stmt, index, qual, table string }{
		{`CREATE INDEX CONCURRENTLY books_title ON books (title)`, "books_title", "", "books"},
		{`CREATE UNIQUE INDEX CONCURRENTLY books_title ON app.books(title)`, "books_title", "", "app.books"},
		{`create index concurrently app.books_title on only app.books (title)`, "app.books_title", ".books_title", "app.books"},
		{`CREATE INDEX CONCURRENTLY "Odd.Name" ON "My Schema"."Books" (title)`, `"Odd.Name"`, "", `"My Schema"."Books"`},
	}
	for _, c := range cases {
		mm := createIndexConcurrentlyRe.FindStringSubmatch(c.stmt)
		if mm == nil {
			t.Fatalf("%s: no match", c.stmt)
		}
		if mm[1] != c.index || mm[2] != c.qual || mm[3] != c.table {
			t.Errorf("%s: index=%q qual=%q table=%q; want %q %q %q", c.stmt, mm[1], mm[2], mm[3], c.index, c.qual, c.table)
		}
	}
	if createIndexConcurrentlyRe.MatchString(`CREATE INDEX CONCURRENTLY IF NOT EXISTS x ON t (a)`) {
		t.Error("IF NOT EXISTS re-runs cleanly and must not be settled")
	}
}
