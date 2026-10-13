package principal

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// DeviceMetadataKey carries the verified device id: the RFC 7638 thumbprint of
// the key whose DPoP proof the gateway checked for this request (see
// lib/dpop). It travels next to the `x-w17-user` envelope rather than inside
// it, because the envelope is the auth method's own AuthResp and the device
// is a fact the gateway established, not one the auth method returned.
//
// The key lies in the `x-w17-` namespace, so it is gateway-owned like the
// envelope: [SanitizeGatewayMD] strips a client's copy, [IsGatewayOwnedKey]
// and [IsEdgeReservedKey] refuse it on every edge, and [ForwardToOutgoing]
// relays it with the rest of the verified principal. A gateway that does not
// verify DPoP never stamps it, and [DeviceID] then reports false.
const DeviceMetadataKey = "x-w17-device"

// DeviceID returns the verified device id of the caller: the thumbprint of
// the device key the gateway checked a DPoP proof against. It reports false
// when the request carries none — a gateway without DPoP, a bearer request,
// or a call that bypassed the gateway. Fail closed on false: an absent device
// is never "any device".
//
//	dev, ok := principal.DeviceID(ctx)
//	if !ok {
//	    return nil, status.Error(codes.Unauthenticated, "device-bound credential required")
//	}
func DeviceID(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(DeviceMetadataKey)
	if len(vals) != 1 || vals[0] == "" {
		return "", false
	}
	return vals[0], true
}

// SetDeviceID stamps the verified device id onto md, the gateway's
// sanitized metadata ([SanitizeGatewayMD]) before it goes to
// [WithGatewayMD]. An empty id removes the key, so a request whose proof
// was not verified never carries one. Only a gateway that verified the
// proof may call this; the value is trusted by every tier behind it.
func SetDeviceID(md metadata.MD, id string) {
	if md == nil {
		return
	}
	if id == "" {
		md.Delete(DeviceMetadataKey)
		return
	}
	md.Set(DeviceMetadataKey, id)
}
