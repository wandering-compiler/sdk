package principal

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// DataScopeMetadataKey carries the range of rows a request may see when it
// came in through an audience gateway (w17/audience.proto): one of the
// DataScope* values — the audience's
// default, or what the called method widens it to with `(w17.widen_scope)`.
//
// Gateway-owned like the rest of the `x-w17-` namespace: [SanitizeGatewayMD]
// strips a client's copy, and [ForwardToOutgoing] relays it, so a facade's
// calls into storage carry the range of the request that made them. Storage
// reads it to pick which of a model's data scopes apply. A request without it
// — every gateway that is not an audience's, every internal job — keeps
// today's behaviour: all of them apply.
const DataScopeMetadataKey = "x-w17-data-scope"

// The DataScopeMetadataKey values.
const (
	// DataScopeOwner applies every data scope of a model, the owner axis
	// included: the caller's own rows.
	DataScopeOwner = "owner"
	// DataScopeTenant applies the tenant-axis scopes only: the caller's
	// tenant.
	DataScopeTenant = "tenant"
	// DataScopeAll applies none: every tenant. Only the admin surface's
	// gateway stamps it.
	DataScopeAll = "all"
)

// DataScope returns the data range the incoming request carries, "" when it
// carries none (the request did not come through an audience gateway).
func DataScope(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(DataScopeMetadataKey); len(v) == 1 {
		switch v[0] {
		case DataScopeOwner, DataScopeTenant, DataScopeAll:
			return v[0]
		}
		// An unknown value is read as the narrowest range rather than
		// ignored: ignoring it would be "no audience", which applies every
		// scope too, but a typo should not be indistinguishable from intent.
		return DataScopeOwner
	}
	return ""
}
