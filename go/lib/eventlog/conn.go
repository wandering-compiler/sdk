package eventlog

import (
	"context"
	"net/http"

	"google.golang.org/grpc"
)

// Conn records the unary calls an HTTP surface makes through cc into the
// request's record: an admin handler calls one backend method per request,
// and the call is where its method, request, caller (the metadata the
// surface stamped) and outcome all are. The first recorded call of a
// request begins its record in the slot [Surface.HTTPMiddleware] gave the
// request; a sync audit method's record is finished right after a
// successful call, before the handler answers; later calls of other
// recorded methods in the same request are not recorded, and calls of methods not in methods (the auth lookup every
// request makes) pass straight through. A nil Surface returns cc.
func (s *Surface) Conn(cc grpc.ClientConnInterface, methods map[string]*Method) grpc.ClientConnInterface {
	if s == nil {
		return cc
	}
	return &recordingConn{ClientConnInterface: cc, s: s, methods: methods}
}

type recordingConn struct {
	grpc.ClientConnInterface
	s       *Surface
	methods map[string]*Method
}

func (c *recordingConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	m := c.methods[method]
	sl, _ := ctx.Value(slotKey{}).(*slot)
	if m == nil || sl == nil || sl.rec != nil {
		return c.ClientConnInterface.Invoke(ctx, method, args, reply, opts...)
	}
	rec := c.s.Start(m)
	sl.rec = rec
	rec.SetRequestAny(args)
	rec.SetCallerOutgoing(ctx)
	err := c.ClientConnInterface.Invoke(ctx, method, args, reply, opts...)
	rec.setResultAny(reply, err)
	if m.Sync && err == nil {
		// A sync audit record holds the request until it is stored — here,
		// before the handler answers. The status is the one a successful
		// call answers with.
		rec.Finish(ctx, http.StatusOK)
	}
	return err
}
