package geoip

import (
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// RequireHTTP gates a REST method on the caller's country: it answers 403
// itself (PERMISSION_DENIED, details[].code GEO_REFUSED, the translated
// sentence) and returns false when p refuses the address
// restgw.ClientAddrMiddleware resolved. A nil gate or policy admits.
//
// It lives here, not in restgw, so only a gateway that declares a policy
// links the database reader.
func RequireHTTP(g *Gate, p *Policy, w http.ResponseWriter, r *http.Request) bool {
	return admit(g, p, w, r, restgw.ClientAddrFrom(r.Context()))
}

// Admit is RequireHTTP for a transport that resolves the client address
// itself (the MCP server's admission hook, mcp.Server.SetAdmit): nil when
// there is nothing to check.
func Admit(g *Gate, p *Policy) func(w http.ResponseWriter, r *http.Request, clientAddr string) bool {
	if g == nil || g.Off || p == nil {
		return nil
	}
	return func(w http.ResponseWriter, r *http.Request, clientAddr string) bool {
		return admit(g, p, w, r, clientAddr)
	}
}

func admit(g *Gate, p *Policy, w http.ResponseWriter, r *http.Request, clientAddr string) bool {
	ctx := r.Context()
	v := g.Check(p, ParseAddr(clientAddr))
	if v.Allowed {
		return true
	}
	observx.ReportRefusal(ctx, status.Error(codes.PermissionDenied,
		fmt.Sprintf("geo: refused (%s, country %q, policy %s)", v.Reason, v.Country, p)))
	sentence := i18n.T(ctx, MsgidRefused, nil)
	restgw.WriteErrorWithDetails(w, http.StatusForbidden, restgw.GRPCCodeName(codes.PermissionDenied), sentence,
		[]restgw.FieldError{{Code: CodeRefused, Message: sentence, Msgid: MsgidRefused}})
	return false
}
