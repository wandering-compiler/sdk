// Package pluginhook is how a plugin's code calls the project back: a hook
// (`plugin.yaml` `hooks:`) the activation binds to business methods of its
// domain (PluginActivation.hooks). The generated bundle hands the plugin one
// function per bound method (gen.ClientSet.<Name>Hooks()), made here.
//
// A plugin runs in its domain's storage bundle and a hook's method in the
// business tier, which itself calls the storage tier — so the connection is
// late: dialled on the first call in a standalone bundle, set once the
// business tier is up in a composed binary. Neither waits for the business
// tier at boot, so the two tiers never depend on each other's start.
package pluginhook

import (
	"context"
	"errors"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// ErrNotConnected is a call made before the connection exists: a composed
// binary that never set it.
var ErrNotConnected = errors.New("pluginhook: the business tier is not connected")

// Conn is the connection hook calls go over, bound late.
type Conn struct {
	mu   sync.Mutex
	cc   grpc.ClientConnInterface
	dial func() (grpc.ClientConnInterface, error)
	err  error
}

// Lazy is a Conn that dials on its first use (a standalone storage bundle:
// the business tier's address from its environment). A failed dial is
// retried on the next call.
func Lazy(dial func() (grpc.ClientConnInterface, error)) *Conn {
	return &Conn{dial: dial}
}

// Late is a Conn that is Set once the connection exists (a composed binary:
// the in-process business tier is registered after the storage tier).
func Late() *Conn { return &Conn{} }

// Set binds c to cc. Later calls go over it.
func (c *Conn) Set(cc grpc.ClientConnInterface) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cc, c.err = cc, nil
}

func (c *Conn) get() (grpc.ClientConnInterface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cc != nil {
		return c.cc, nil
	}
	if c.dial == nil {
		return nil, ErrNotConnected
	}
	cc, err := c.dial()
	if err != nil {
		return nil, err
	}
	c.cc = cc
	return cc, nil
}

// Unary is a call of the unary method (a full gRPC name,
// `/<package>.<Service>/<Method>`) over c. The verified caller rides along
// (principal.ForwardToOutgoing): the project method sees the same principal
// the plugin's handler was called with.
func Unary[Req, Resp any, PReq interface {
	*Req
	proto.Message
}, PResp interface {
	*Resp
	proto.Message
}](c *Conn, method string) func(context.Context, PReq) (PResp, error) {
	return func(ctx context.Context, in PReq) (PResp, error) {
		cc, err := c.get()
		if err != nil {
			return nil, err
		}
		out := PResp(new(Resp))
		if err := cc.Invoke(principal.ForwardToOutgoing(ctx), method, in, out); err != nil {
			return nil, err
		}
		return out, nil
	}
}
