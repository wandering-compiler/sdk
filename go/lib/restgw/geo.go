package restgw

import (
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/geoip"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// RequireGeo gates a REST method on the caller's country: it answers 403
// itself (PERMISSION_DENIED, details[].code GEO_REFUSED, the translated
// sentence) and returns false when the policy refuses the address
// [ClientAddrMiddleware] resolved. A nil gate or policy admits.
func RequireGeo(g *geoip.Gate, p *geoip.Policy, w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	v := g.Check(p, geoip.ParseAddr(ClientAddrFrom(ctx)))
	if v.Allowed {
		return true
	}
	reportByCode(ctx, codes.PermissionDenied, status.Error(codes.PermissionDenied,
		fmt.Sprintf("geo: refused (%s, country %q, policy %s)", v.Reason, v.Country, p)))
	sentence := i18n.T(ctx, geoip.MsgidRefused, nil)
	WriteErrorWithDetails(w, http.StatusForbidden, GRPCCodeName(codes.PermissionDenied), sentence,
		[]FieldError{{Code: geoip.CodeRefused, Message: sentence}})
	return false
}
