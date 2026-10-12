package runtime

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// The health service answers probes — a load balancer, an orchestrator's
// liveness check, `w17 healthcheck` — and those carry no credentials. The
// caller's interceptors are the service's request policy (authentication,
// tenant scoping, transaction handling), and an authenticating one refuses
// every probe, so the orchestrator restarts a healthy process in a loop.
//
// So health calls are served from the recovery interceptors directly, ahead
// of the chained interceptors and still inside the recovery net. Only the
// grpc.health.v1 methods are, and only when the registered implementation is
// a health server; every other method takes the full chain. grpc-go has no
// per-service interceptor, and the chain is one opaque handler by the time an
// interceptor sees it, so short-circuiting at the outermost interceptor is the
// one place this can be done for options the caller passed in ServerOptions
// too.

// serveHealthUnary serves a unary health call without the chained
// interceptors. ok is false when the call is not one.
func serveHealthUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo) (resp any, ok bool, err error) {
	hs, isHealth := info.Server.(grpc_health_v1.HealthServer)
	if !isHealth {
		return nil, false, nil
	}
	switch info.FullMethod {
	case grpc_health_v1.Health_Check_FullMethodName:
		in, isReq := req.(*grpc_health_v1.HealthCheckRequest)
		if !isReq {
			return nil, false, nil
		}
		resp, err = hs.Check(ctx, in)
		return resp, true, err
	case grpc_health_v1.Health_List_FullMethodName:
		in, isReq := req.(*grpc_health_v1.HealthListRequest)
		if !isReq {
			return nil, false, nil
		}
		resp, err = hs.List(ctx, in)
		return resp, true, err
	}
	return nil, false, nil
}

// serveHealthStream serves the streaming health call (Watch) without the
// chained interceptors, decoding its request the way the generated handler
// does. ok is false when the call is not one.
func serveHealthStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo) (ok bool, err error) {
	hs, isHealth := srv.(grpc_health_v1.HealthServer)
	if !isHealth || info.FullMethod != grpc_health_v1.Health_Watch_FullMethodName {
		return false, nil
	}
	in := new(grpc_health_v1.HealthCheckRequest)
	if err := ss.RecvMsg(in); err != nil {
		return true, err
	}
	return true, hs.Watch(in, &grpc.GenericServerStream[grpc_health_v1.HealthCheckRequest, grpc_health_v1.HealthCheckResponse]{ServerStream: ss})
}
