package restgw_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The schema stays on the operator's side: whatever a constraint failure
// becomes — a registry miss, a mapped violation with the default sentence —
// the client-facing reading (the REST envelope, and ClientError, which the
// MCP proxy uses) names neither the constraint nor the table.
func TestConstraintFailure_NamesNoConstraintOrTableToTheClient(t *testing.T) {
	const constraint, table = "orders_tenant_sku_key", "billing_orders"
	registry := &grpcerr.ConstraintRegistry{ByName: map[string]grpcerr.ConstraintInfo{
		"orders_tenant_sku_key_mapped": {Field: "sku", Code: "UNIQUE_VIOLATION"},
	}}
	for name, pgErr := range map[string]*pgconn.PgError{
		"registry miss":    {Code: "23505", ConstraintName: constraint, TableName: table},
		"mapped":           {Code: "23505", ConstraintName: constraint + "_mapped", TableName: table},
		"check, no field":  {Code: "23514", ConstraintName: constraint, TableName: table},
		"foreign key miss": {Code: "23503", ConstraintName: constraint, TableName: table},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			err := grpcerr.Wrap(ctx, "OrderMutation.Create", pgErr, registry, grpcerr.DialectPostgres)

			rec := httptest.NewRecorder()
			restgw.WriteGRPCErrorCtx(ctx, rec, err)
			_, msg, details := restgw.ClientError(ctx, err)
			seen := rec.Body.String() + "\n" + msg
			for _, d := range details {
				seen += "\n" + d.Field + " " + d.Code + " " + d.Message
			}
			for _, secret := range []string{constraint, table, "billing"} {
				if strings.Contains(seen, secret) {
					t.Errorf("client-facing reading names %q:\n%s", secret, seen)
				}
			}
		})
	}
}
