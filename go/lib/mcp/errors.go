package mcp

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// Tool errors follow the same two-audience rule as the REST gateway.
//
// mcp-go turns a handler error into a JSON-RPC error whose message is
// err.Error(), verbatim. A backend status message is written for an operator
// (it carries the `<Service>.<Method>` prefix, constraint and table names, and
// whatever prose a handler put there), so it must never be that string. The
// operator's copy goes to observability; the client gets a gRPC status built
// from the code, the catalog sentence (or the backend's request-level detail)
// and the field details — the same parts restgw.ClientError gives the REST
// envelope. The error text keeps the `rpc error: code = X desc = Y` shape, so
// clients that branch on the code keep working, and Y is the same sentence a
// REST client reads for the same failure.

// publicError maps an error from the backend (or from the auth backend) to
// the status a client may see, reporting the original.
func publicError(ctx context.Context, err error) error {
	c, msg, details := restgw.ClientError(ctx, err)
	// Classified like every other surface: a refusal the backend decided is
	// the caller's outcome, not a server fault.
	report(ctx, c, err)
	st := status.New(c, msg)
	if len(details) > 0 {
		pb := make([]*w17pb.ErrorDetail, 0, len(details))
		for _, d := range details {
			pb = append(pb, &w17pb.ErrorDetail{Field: d.Field, Code: d.Code, Message: d.Message})
		}
		if withDetails, derr := withErrorDetails(st, pb); derr == nil {
			st = withDetails
		}
	}
	return st.Err()
}

func withErrorDetails(st *status.Status, details []*w17pb.ErrorDetail) (*status.Status, error) {
	out := st
	for _, d := range details {
		var err error
		if out, err = out.WithDetails(d); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// refusal is a failure the proxy decided itself (permission gate, argument
// decode, response encode). operator is the developer's description; it
// reaches observability only, and the client gets the code's catalog
// sentence.
func refusal(ctx context.Context, c codes.Code, operator error) error {
	report(ctx, c, operator)
	return status.Error(c, restgw.ClientSentence(ctx, c))
}

// report sends the operator's copy down the channel its code belongs on:
// server faults are exceptions, everything else is an expected outcome of a
// request and goes to the non-exception channel.
func report(ctx context.Context, c codes.Code, err error) {
	switch c {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable:
		observx.ReportError(ctx, err)
	default:
		observx.ReportRefusal(ctx, err)
	}
}
