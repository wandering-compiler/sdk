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
