package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// requiredExtensions pulls the declared PG extensions out of a migration's
// manifest.
//
// The manifest rides every migration as `manifest_json` — the console marshals
// planpb.Manifest with protojson and UseProtoNames, so the key is the proto
// field name. It is read as a loose struct rather than the typed message
// because planpb is private: the client is not allowed to know the compiler's
// types, and one repeated string does not need them.
func requiredExtensions(m *applyfetchpb.Migration) []string {
	return requiredExtensionsFromManifest(m.GetManifestJson())
}

// requiredExtensionsFromManifest is the projection ContentHash binds.
//
// Split out so the digest and the check read the manifest through the SAME
// parser. Two readers of one blob is how a value gets enforced under one
// interpretation and hashed under another, and then the pin and the refusal
// disagree about what the migration said.
func requiredExtensionsFromManifest(manifestJSON string) []string {
	raw := strings.TrimSpace(manifestJSON)
	if raw == "" {
		return nil
	}
	var doc struct {
		RequiredExtensions []string `json:"required_extensions"`
	}
	// A manifest this cannot parse is NOT an error. It is metadata attached to
	// the migration, not the migration; refusing an apply because a field the
	// console added later did not decode would turn an additive change into an
	// outage. The check simply has nothing to say.
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil
	}
	return doc.RequiredExtensions
}

// extensionPreflightSQL builds the probe that refuses when a declared
// extension is absent.
//
// It is PG-specific and that is safe: `required_extensions` comes from
// `(w17.pg.*)`, so a manifest that lists any has already chosen Postgres. A
// MySQL or SQLite connection never carries them and never reaches this.
//
// Built HERE rather than emitted by the console, unlike adopt_preflight_sql.
// The line the public split draws is around COMPILER knowledge — schema
// shapes, dialect rendering, anything that encodes what the generator decided.
// This encodes none: it asks a system catalogue whether a name is present. Its
// content does not vary with the schema, the dialect renderer or the plan, so
// putting it behind a new proto field, a regen and a deploy would buy the
// architecture nothing and cost the check a release cycle.
//
// Names are single-quote escaped rather than parameterised because a DO block
// takes no parameters. They arrive from a proto field an author wrote, so the
// escape is what stands between a stray quote and a broken probe.
func extensionPreflightSQL(exts []string) string {
	if len(exts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("DO $w17_ext$\nBEGIN\n")
	for _, e := range exts {
		q := strings.ReplaceAll(e, "'", "''")
		fmt.Fprintf(&b,
			"  IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = '%s') THEN\n"+
				"    RAISE EXCEPTION 'required extension %% is not installed in this database', '%s';\n"+
				"  END IF;\n", q, q)
	}
	b.WriteString("END\n$w17_ext$;")
	return b.String()
}

// preflightExtensions refuses the whole run when any migration about to be
// applied declares an extension the target database does not have.
//
// # Why this exists
//
// `required_extensions` was a constraint that did nothing. An author declared
// it, the compiler aggregated it into the manifest, the console stored the
// manifest and shipped it to the client on every migration — and NOTHING read
// it. Not here, not anywhere: the only mentions in the tree were the write and
// a test asserting the write happened. The whole apparatus was in place except
// the question.
//
// What that cost: migrations deliberately carry no CREATE EXTENSION, because
// provisioning is the deploying platform's job. So an unprovisioned target
// failed in the MIDDLE of a migration, on a raw Postgres error, with part of
// the schema already written.
//
// # Why before the first apply, and per connection
//
// Every connection is probed before any of them is written to. A run that
// checked lazily would leave a database changed by the migrations that ran
// before the missing extension was noticed — the same partial state the adopt
// path splits its two passes to avoid.
// PostgresDialect is the optional interface an [Applier] implements to say
// which SQL dialect it speaks.
//
// The extension preflight needs it because its probe is Postgres syntax
// (`DO $$ … pg_extension`) while the manifest field it reads is carried on
// EVERY dialect. That is deliberate — `plan.go` flows
// `(w17.pg.field).required_extensions` into non-PG buckets for manifest
// TRACKING, and the MySQL emitter stamps its own "manifest tracking only"
// marker saying so. The preflight bucketed by connection NAME alone, so a
// MySQL or SQLite connection whose manifest carried the annotation got the
// Postgres probe fired at it, failed on syntax, and was refused with "this
// database is missing an extension the schema declares" — a healthy database
// permanently unable to apply, told something untrue about why
// (T2-6 pass #10, D10-1 ≡ B10-4).
//
// An optional interface rather than a new method on [Applier]: every
// applier in this tree is a per-dialect package and knows the answer
// trivially, but the interface is public and a consumer's own applier must
// not stop compiling. An applier that does not implement it is not probed —
// the preflight is a Postgres-specific check, and running it against
// something that has not said it is Postgres is what caused this.
type PostgresDialect interface {
	// IsPostgres reports whether this applier speaks PostgreSQL.
	IsPostgres() bool
}

// WrappedApplier is the optional interface a decorator implements to expose
// the applier it wraps, so an OPTIONAL interface survives the wrapping.
//
// This is not hypothetical tidiness — it is the hole the first version of
// this gate shipped with. Embedding a `migrate.Applier` INTERFACE in a
// decorator promotes only that interface's methods, so `IsPostgres` becomes
// invisible the moment anything wraps the applier, the type assertion below
// fails, and the preflight goes silently dark against a real Postgres. A
// check that disables itself when a decorator appears is the same fail-open
// this gate was written to remove, one layer out. The live lane caught it
// where a unit test over a bare applier could not (T2-6 pass #10, D10-1).
type WrappedApplier interface {
	// Unwrap returns the applier this one decorates.
	Unwrap() Applier
}

// isPostgresApplier reports whether the preflight's Postgres probe is
// meaningful against this applier, following [WrappedApplier] decorators the
// way errors.As follows Unwrap.
func isPostgresApplier(a Applier) bool {
	for range 16 { // a decorator chain deeper than this is a cycle
		if d, ok := a.(PostgresDialect); ok {
			return d.IsPostgres()
		}
		w, ok := a.(WrappedApplier)
		if !ok {
			return false
		}
		inner := w.Unwrap()
		if inner == nil || inner == a {
			return false
		}
		a = inner
	}
	return false
}

func preflightExtensions(ctx context.Context, ac *runApplierCache, pending []Pending) error {
	byConn := map[string]map[string]bool{}
	for _, p := range pending {
		for _, e := range requiredExtensions(p.Migration) {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if byConn[p.Connection] == nil {
				byConn[p.Connection] = map[string]bool{}
			}
			byConn[p.Connection][e] = true
		}
	}
	if len(byConn) == 0 {
		return nil
	}

	conns := make([]string, 0, len(byConn))
	for c := range byConn {
		conns = append(conns, c)
	}
	sort.Strings(conns)

	for _, conn := range conns {
		exts := make([]string, 0, len(byConn[conn]))
		for e := range byConn[conn] {
			exts = append(exts, e)
		}
		sort.Strings(exts)

		applier, err := ac.get(ctx, conn)
		if err != nil {
			return err
		}
		if !isPostgresApplier(applier) {
			// The manifest carries the annotation on every dialect for
			// tracking; only Postgres can be asked the question.
			continue
		}
		probe := &applyfetchpb.Migration{
			Id:         "extension-preflight",
			Connection: conn,
			UpSql:      extensionPreflightSQL(exts),
		}
		if err := applier.Apply(ctx, probe); err != nil {
			return fmt.Errorf(
				"apply %s: this database is missing an extension the schema declares (%s): %w\n"+
					"  Migrations deliberately do not CREATE EXTENSION — provisioning is the deploying\n"+
					"  platform's step, and it has to happen before apply. Nothing was written.",
				conn, strings.Join(exts, ", "), err)
		}
	}
	return nil
}
