package dqlbind

import (
	"bytes"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Every element type, read through the pgx database/sql driver from a real
// server: the text form the driver hands over is the one ScanArray parses,
// and a slice bound as an argument round-trips into it.
func TestScanArray_PostgresLive(t *testing.T) {
	dsn := os.Getenv("W17_PG_DSN")
	if dsn == "" {
		t.Skip("W17_PG_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var (
		s  []string
		i8 []int64
		i4 []int32
		u4 []uint32
		b  []bool
		f8 []float64
		f4 []float32
		by [][]byte
		e  []testEnum
	)
	err = db.QueryRow(`SELECT $1::text[], $2::int8[], $3::int4[], $4::int8[], $5::bool[], $6::float8[], $7::float4[], $8::bytea[], $9::int4[]`,
		[]string{"a", "b,c", `d"e`, `f\g`, "", "NULL"}, []int64{-1, 1 << 40}, []int32{7}, []int64{4294967295},
		[]bool{true, false}, []float64{1.5, -2}, []float32{0.25}, [][]byte{{0, 1, 0xff}, {}}, []int32{2, 3},
	).Scan(ScanArray(&s), ScanArray(&i8), ScanArray(&i4), ScanArray(&u4), ScanArray(&b), ScanArray(&f8), ScanArray(&f4), ScanArray(&by), ScanArray(&e))
	if err != nil {
		t.Fatal(err)
	}
	wantS := []string{"a", "b,c", `d"e`, `f\g`, "", "NULL"}
	for k := range wantS {
		if s[k] != wantS[k] {
			t.Fatalf("text[] element %d = %q, want %q", k, s[k], wantS[k])
		}
	}
	if i8[1] != 1<<40 || i4[0] != 7 || u4[0] != 4294967295 || !b[0] || b[1] || f8[0] != 1.5 || f4[0] != 0.25 || e[1] != 3 {
		t.Fatalf("scalars: %v %v %v %v %v %v %v", i8, i4, u4, b, f8, f4, e)
	}
	if !bytes.Equal(by[0], []byte{0, 1, 0xff}) || len(by[1]) != 0 {
		t.Fatalf("bytea[] = %v", by)
	}

	var empty, null []string
	null = []string{"stale"}
	if err := db.QueryRow(`SELECT '{}'::text[], NULL::text[]`).Scan(ScanArray(&empty), ScanArray(&null)); err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 || null != nil {
		t.Fatalf("empty %#v null %#v", empty, null)
	}
}
