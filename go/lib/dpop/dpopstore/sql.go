package dpopstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// Dialect names the SQL dialect of a [SQLJTIStore].
type Dialect string

// The dialects [SQLJTIStore] speaks.
const (
	Postgres Dialect = "postgres"
	MySQL    Dialect = "mysql"
	SQLite   Dialect = "sqlite"
)

// DefaultSQLTable is the table of a [SQLJTIStore].
const DefaultSQLTable = "w17_dpop_jti"

// DefaultSQLPruneEvery is how many recorded proofs pass between two sweeps of
// expired rows.
const DefaultSQLPruneEvery = 256

// SQLRecountEvery is the longest a [SQLJTIStore] trusts its count of live
// rows (by rec.FirstSeen) before counting again for the total cap.
const SQLRecountEvery = time.Second

var sqlIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// SQLJTIStore is a [dpop.JTIStore] in a SQL table shared by every replica
// that uses the same database: one row per accepted proof, unique on the
// hash of its key, with an expiry. Record inserts if absent (`ON CONFLICT DO
// NOTHING` on PostgreSQL and SQLite, `ON DUPLICATE KEY UPDATE` as a no-op on
// MySQL) and then reads the row back, so whichever replica's insert won, both
// see the same stored record.
//
// Expiry is judged by rec.FirstSeen, the verifier's clock: a row whose expiry
// has passed is ignored, replaced by the next Record of its key, and deleted
// by a sweep every [DefaultSQLPruneEvery] records ([SQLJTIStore.Prune] runs
// one on demand). The per-key share is counted before the insert, so
// concurrent Records of one key may overshoot it by the number in flight;
// the share bounds a key, it is not a quota.
//
// The total cap ([WithSQLMaxEntries]) is judged against a count of live rows
// each replica keeps: counted again every [SQLRecountEvery] or every
// pruneEvery records, whichever comes first, and raised by each of its own
// inserts in between. Counting the table on every Record would cost a scan
// per proof. Other replicas' inserts since the last count are not seen, so
// the table may overshoot the cap by what they insert in that interval; it
// stays bounded, which is what keeps a mint flood from filling the database.
type SQLJTIStore struct {
	db         *sql.DB
	dialect    Dialect
	table      string
	maxKey     int
	maxTotal   int
	pruneEvery int64
	n          atomic.Int64

	tmu       sync.Mutex
	total     int       // live rows at countedAt, plus our inserts since
	countedAt time.Time // zero: never counted
	since     int64     // our inserts since countedAt
	counting  bool

	qDeleteExpiredKey, qCount, qCountAll, qInsert, qSelect, qPrune string
}

// SQLOption configures [NewSQLJTIStore].
type SQLOption func(*SQLJTIStore)

// WithSQLTable sets the table name (default [DefaultSQLTable]): lower case
// letters, digits and underscores. Use another table for the mint store.
func WithSQLTable(name string) SQLOption {
	return func(s *SQLJTIStore) { s.table = name }
}

// WithSQLMaxPerKey bounds the live rows one key thumbprint may hold (default
// [dpop.DefaultMaxJTIsPerKey]); n <= 0 keeps the default.
func WithSQLMaxPerKey(n int) SQLOption {
	return func(s *SQLJTIStore) {
		if n > 0 {
			s.maxKey = n
		}
	}
}

// WithSQLMaxEntries bounds the live rows of the whole table (default
// [dpop.DefaultMaxJTIs]); n <= 0 keeps the default. Size it for the peak
// rate of proofs times the jti ttl; a full table refuses with
// [dpop.ErrJTIStoreFull]. See [SQLJTIStore] for how closely it holds.
func WithSQLMaxEntries(n int) SQLOption {
	return func(s *SQLJTIStore) {
		if n > 0 {
			s.maxTotal = n
		}
	}
}

// WithSQLPruneEvery sets how many Records pass between two sweeps of expired
// rows; n <= 0 keeps [DefaultSQLPruneEvery].
func WithSQLPruneEvery(n int) SQLOption {
	return func(s *SQLJTIStore) {
		if n > 0 {
			s.pruneEvery = int64(n)
		}
	}
}

// NewSQLJTIStore returns a store over db in the given dialect. It does not
// create the table; call [SQLJTIStore.EnsureSchema] or create it with your
// migrations (the DDL is [SQLJTIStore.Schema]).
func NewSQLJTIStore(db *sql.DB, dialect Dialect, opts ...SQLOption) (*SQLJTIStore, error) {
	if db == nil {
		return nil, errors.New("dpopstore: NewSQLJTIStore needs a database")
	}
	switch dialect {
	case Postgres, MySQL, SQLite:
	default:
		return nil, fmt.Errorf("dpopstore: unknown dialect %q", dialect)
	}
	s := &SQLJTIStore{db: db, dialect: dialect, table: DefaultSQLTable, maxKey: dpop.DefaultMaxJTIsPerKey, maxTotal: dpop.DefaultMaxJTIs, pruneEvery: DefaultSQLPruneEvery}
	for _, o := range opts {
		o(s)
	}
	if !sqlIdent.MatchString(s.table) {
		return nil, fmt.Errorf("dpopstore: table name %q: want [a-z_][a-z0-9_]*", s.table)
	}
	t := s.table
	s.qDeleteExpiredKey = s.bind("DELETE FROM " + t + " WHERE key_hash = ? AND expires_at <= ?")
	s.qCount = s.bind("SELECT COUNT(*) FROM " + t + " WHERE thumbprint = ? AND expires_at > ?")
	s.qCountAll = s.bind("SELECT COUNT(*) FROM " + t + " WHERE expires_at > ?")
	insert := "INSERT INTO " + t + " (key_hash, thumbprint, proof_hash, first_seen, expires_at) VALUES (?, ?, ?, ?, ?)"
	if dialect == MySQL {
		insert += " ON DUPLICATE KEY UPDATE key_hash = key_hash"
	} else {
		insert += " ON CONFLICT (key_hash) DO NOTHING"
	}
	s.qInsert = s.bind(insert)
	s.qSelect = s.bind("SELECT thumbprint, proof_hash, first_seen, expires_at FROM " + t + " WHERE key_hash = ?")
	s.qPrune = s.bind("DELETE FROM " + t + " WHERE expires_at <= ?")
	return s, nil
}

// bind rewrites ? placeholders to $n for PostgreSQL.
func (s *SQLJTIStore) bind(q string) string {
	if s.dialect != Postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, c := range q {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Schema returns the statements that create the table and its indexes.
func (s *SQLJTIStore) Schema() []string {
	t := s.table
	cols := "key_hash CHAR(64) NOT NULL PRIMARY KEY, thumbprint VARCHAR(255) NOT NULL, proof_hash VARCHAR(255) NOT NULL, first_seen BIGINT NOT NULL, expires_at BIGINT NOT NULL"
	if s.dialect == MySQL {
		return []string{"CREATE TABLE IF NOT EXISTS " + t + " (" + cols +
			", INDEX " + t + "_tp (thumbprint, expires_at), INDEX " + t + "_exp (expires_at))"}
	}
	return []string{
		"CREATE TABLE IF NOT EXISTS " + t + " (" + cols + ")",
		"CREATE INDEX IF NOT EXISTS " + t + "_tp ON " + t + " (thumbprint, expires_at)",
		"CREATE INDEX IF NOT EXISTS " + t + "_exp ON " + t + " (expires_at)",
	}
}

// EnsureSchema creates the table and its indexes when they do not exist.
func (s *SQLJTIStore) EnsureSchema(ctx context.Context) error {
	for _, q := range s.Schema() {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("dpopstore: ensure schema: %w", err)
		}
	}
	return nil
}

// Prune deletes every row expired at now.
func (s *SQLJTIStore) Prune(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, s.qPrune, now.UnixNano()); err != nil {
		return fmt.Errorf("dpopstore: prune: %w", err)
	}
	return nil
}

// recordAttempts bounds the read-back loop: a row seen expired or gone
// between the insert and the read (another replica's sweep, a clock behind
// ours) is retried, a bounded number of times.
const recordAttempts = 3

// Record implements [dpop.JTIStore].
func (s *SQLJTIStore) Record(ctx context.Context, key string, rec dpop.JTIRecord, ttl time.Duration) (dpop.JTIRecord, bool, error) {
	sum := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(sum[:])
	now := rec.FirstSeen.UnixNano()
	exp := rec.FirstSeen.Add(ttl).UnixNano()

	for range recordAttempts {
		if _, err := s.db.ExecContext(ctx, s.qDeleteExpiredKey, keyHash, now); err != nil {
			return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: record: %w", err)
		}
		got, found, err := s.read(ctx, keyHash, now)
		if err != nil {
			return dpop.JTIRecord{}, false, err
		}
		if found {
			return got, false, nil
		}
		var live int
		if err := s.db.QueryRowContext(ctx, s.qCount, rec.Thumbprint, now).Scan(&live); err != nil {
			return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: record: count: %w", err)
		}
		if live >= s.maxKey {
			return dpop.JTIRecord{}, false, dpop.ErrJTIStoreFull
		}
		switch full, err := s.totalFull(ctx, rec.FirstSeen); {
		case err != nil:
			return dpop.JTIRecord{}, false, err
		case full:
			return dpop.JTIRecord{}, false, dpop.ErrJTIStoreFull
		}
		res, err := s.db.ExecContext(ctx, s.qInsert, keyHash, rec.Thumbprint, rec.ProofHash, now, exp)
		if err != nil {
			return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: record: insert: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			s.inserted()
		}
		// Read back: the row is ours exactly when it holds what we wrote.
		// Two presentations of one proof at the same nanosecond both read
		// "created", which the verifier's reuse window allows anyway.
		got, found, err = s.read(ctx, keyHash, now)
		if err != nil {
			return dpop.JTIRecord{}, false, err
		}
		if !found {
			continue
		}
		if got.ProofHash == rec.ProofHash && got.FirstSeen.UnixNano() == now && got.Thumbprint == rec.Thumbprint {
			s.maybePrune(ctx, rec.FirstSeen)
			return rec, true, nil
		}
		return got, false, nil
	}
	return dpop.JTIRecord{}, false, errors.New("dpopstore: record: row kept changing under concurrent writers")
}

// totalFull reports whether the table is at its total cap at now, by the
// kept count (see [SQLJTIStore]), counting again when that is stale. One
// Record counts at a time; the others use the count they find.
func (s *SQLJTIStore) totalFull(ctx context.Context, now time.Time) (bool, error) {
	s.tmu.Lock()
	stale := !s.counting && (s.countedAt.IsZero() || now.Sub(s.countedAt) >= SQLRecountEvery ||
		now.Before(s.countedAt) || s.since >= s.pruneEvery)
	if stale {
		s.counting = true
	}
	total := s.total
	s.tmu.Unlock()
	if !stale {
		return total >= s.maxTotal, nil
	}
	var live int
	err := s.db.QueryRowContext(ctx, s.qCountAll, now.UnixNano()).Scan(&live)
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.counting = false
	if err != nil {
		return false, fmt.Errorf("dpopstore: record: count all: %w", err)
	}
	s.total, s.countedAt, s.since = live, now, 0
	return live >= s.maxTotal, nil
}

// inserted adds one of our inserts to the kept count.
func (s *SQLJTIStore) inserted() {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.total++
	s.since++
}

// read returns the live row of keyHash at now.
func (s *SQLJTIStore) read(ctx context.Context, keyHash string, now int64) (dpop.JTIRecord, bool, error) {
	var r dpop.JTIRecord
	var first, exp int64
	err := s.db.QueryRowContext(ctx, s.qSelect, keyHash).Scan(&r.Thumbprint, &r.ProofHash, &first, &exp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return dpop.JTIRecord{}, false, nil
	case err != nil:
		return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: record: read: %w", err)
	case exp <= now:
		return dpop.JTIRecord{}, false, nil
	}
	r.FirstSeen = time.Unix(0, first)
	return r, true, nil
}

// maybePrune sweeps expired rows every pruneEvery records. A failed sweep is
// not the request's failure: the rows are retried at the next sweep.
func (s *SQLJTIStore) maybePrune(ctx context.Context, now time.Time) {
	if s.n.Add(1)%s.pruneEvery != 0 {
		return
	}
	_ = s.Prune(ctx, now)
}
