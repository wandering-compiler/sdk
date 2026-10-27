package principal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// ActAs returns a context that calls a tier of THIS domain as user: the
// envelope (the project's generated AuthResp; its user id is required) and
// the named data scopes (`org_id`, `tenant_id`, …) are stamped on the
// OUTGOING side, in place of any principal ctx carries. The callee reads them
// with [UserID], [Envelope] and [Scope] as it would a gateway's stamps.
//
// It exists for a method another domain's service calls on a person's
// behalf. A private surface admits services, not people: its gateway drops
// any principal a caller sends, so a method that acts for someone takes
// them as request fields — the user, the organization — and the domain,
// having admitted the caller, vouches for them when it calls its own plugin:
//
//	ctx, err := principal.ActAs(ctx, &pb.AuthResp{UserId: req.GetUserId()},
//		map[string]string{"org_id": req.GetOrgId()})
//	if err != nil {
//		return nil, err
//	}
//	res, err := s.clients.AuthService().InviteToOrg(ctx, &authpb.InviteToOrgReq{…})
//
// Only the user id and the scopes travel. Permissions are another domain's
// (permission ids are per domain), so a method acting for a product's user
// does not borrow them: what the user may do here is decided here, from
// this domain's own data.
//
// The principal is replaced, never merged. Every gateway-stamped key on the
// incoming side is dropped too, so a later [ForwardToOutgoing] on the
// returned context cannot put a different principal back. Call the
// downstream tier with the returned context directly.
//
// Trust boundary: as for every principal stamp, the tier believes it because
// only trusted tiers reach it (see the package doc). ActAs is for code that
// has already decided whom it acts for — a method behind a private surface
// that admits its caller by domain — never for a value a person sent.
func ActAs(ctx context.Context, user UserIDMessage, scopes map[string]string) (context.Context, error) {
	if user == nil || user.GetUserId() == "" {
		return ctx, errors.New("principal: ActAs needs a user id")
	}
	raw, err := proto.Marshal(user)
	if err != nil {
		return ctx, fmt.Errorf("principal: ActAs: marshal envelope: %w", err)
	}
	stamps := metadata.MD{userMetadataKey: {base64.StdEncoding.EncodeToString(raw)}}
	for name, v := range scopes {
		if name == "" || v == "" || strings.TrimSpace(name) != name {
			return ctx, fmt.Errorf("principal: ActAs: scope %q=%q is not a scope", name, v)
		}
		stamps[ScopeKey(name)] = []string{v}
	}

	in, _ := metadata.FromIncomingContext(ctx)
	ctx = metadata.NewIncomingContext(ctx, withoutRelayed(in))
	out, _ := metadata.FromOutgoingContext(ctx)
	next := withoutRelayed(out)
	for k, v := range stamps {
		next[k] = v
	}
	return metadata.NewOutgoingContext(ctx, next), nil
}

// withoutRelayed copies md without the principal keys [ForwardToOutgoing]
// relays.
func withoutRelayed(md metadata.MD) metadata.MD {
	next := metadata.MD{}
	for k, v := range md {
		if isRelayedKey(strings.ToLower(k)) {
			continue
		}
		next[k] = append([]string(nil), v...)
	}
	return next
}
