package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// requiredSchemasFromManifest pulls the Postgres namespaces a migration's
// tables are qualified with (`w17.module.schema`) out of its manifest — the
// same `manifest_json` [requiredExtensionsFromManifest] reads, and for the
// same reason as a loose struct: planpb is private to the compiler.
//
// An unreadable manifest yields nothing: it is metadata attached to the
// migration, and a field the console adds later must not refuse an apply.
func requiredSchemasFromManifest(manifestJSON string) []string {
	raw := strings.TrimSpace(manifestJSON)
	if raw == "" {
		return nil
	}
	var doc struct {
		RequiredSchemas []string `json:"required_schemas"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil
	}
	return doc.RequiredSchemas
}

// schemaPrepSQL creates each namespace that does not exist yet. Identifiers
// are double-quote escaped: they come from a proto option an author wrote.
func schemaPrepSQL(schemas []string) string {
	var b strings.Builder
	for _, s := range schemas {
		fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS \"%s\";\n", strings.ReplaceAll(s, `"`, `""`))
	}
	return b.String()
}

// prepareSchemas creates, before the first migration of the run, every
// namespace the pending migrations' tables live in.
//
// The dev apply path has always done this (dev_apply.go: "the prerequisite
// travels with the plan that needs it"); the release path — `migrate apply
// --fetch`, what a deploy runs — did not. Its migrations are qualified
// (`CREATE TABLE "billing"."invoices"`) and carry no CREATE SCHEMA, so the
// first deploy of any project that declares a module schema failed on a fresh
// database with `schema "billing" does not exist`, whichever platform ran it.
// Found by the swarm target's live prover, the first thing to deploy a
// generated project from scratch the way production does.
//
// Unlike an extension, a schema needs no superuser: the role that owns the
// database may create one, so this is the applier's step rather than the
// platform's. Idempotent (`IF NOT EXISTS`), outside the migrations'
// transactions, Postgres only — the manifest carries the field on every
// dialect, and only Postgres has schemas in this sense.
func prepareSchemas(ctx context.Context, ac *runApplierCache, pending []Pending) error {
	byConn := map[string]map[string]bool{}
	for _, p := range pending {
		for _, s := range requiredSchemasFromManifest(p.Migration.GetManifestJson()) {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if byConn[p.Connection] == nil {
				byConn[p.Connection] = map[string]bool{}
			}
			byConn[p.Connection][s] = true
		}
	}
	conns := make([]string, 0, len(byConn))
	for c := range byConn {
		conns = append(conns, c)
	}
	sort.Strings(conns)

	for _, conn := range conns {
		schemas := make([]string, 0, len(byConn[conn]))
		for s := range byConn[conn] {
			schemas = append(schemas, s)
		}
		sort.Strings(schemas)

		applier, err := ac.get(ctx, conn)
		if err != nil {
			return err
		}
		if !isPostgresApplier(applier) {
			continue
		}
		prep := &applyfetchpb.Migration{Id: "schema-prepare", Connection: conn, UpSql: schemaPrepSQL(schemas)}
		if err := applier.Apply(ctx, prep); err != nil {
			return fmt.Errorf("apply %s: creating the schema(s) its tables live in (%s): %w\n"+
				"  The role the DSN connects as needs CREATE on the database. Nothing was migrated.",
				conn, strings.Join(schemas, ", "), err)
		}
	}
	return nil
}
