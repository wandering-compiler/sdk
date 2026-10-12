package restgw_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The context-free writers have no request locale to read: they answer in
// the source language even when a catalog for the caller's language is
// loaded, while their Ctx twins answer from that catalog.
func TestContextFreeWriters_AnswerInTheSourceLocale(t *testing.T) {
	ctx := czech(t)
	source := func(c codes.Code) string { return restgw.ClientSentence(context.Background(), c) }

	rec := httptest.NewRecorder()
	restgw.WriteTooManyRequests(rec)
	if got := decodeEnvelope(t, rec).Error.Message; got != source(codes.ResourceExhausted) {
		t.Errorf("WriteTooManyRequests = %q, want the source sentence %q", got, source(codes.ResourceExhausted))
	}
	rec = httptest.NewRecorder()
	restgw.WriteTooManyRequestsCtx(ctx, rec)
	if got := decodeEnvelope(t, rec).Error.Message; got != "Příliš mnoho požadavků." {
		t.Errorf("WriteTooManyRequestsCtx = %q, want the catalog's sentence", got)
	}

	denied := status.Error(codes.PermissionDenied, "acl: missing perm 11")
	rec = httptest.NewRecorder()
	restgw.WriteGRPCError(rec, denied)
	if got := decodeEnvelope(t, rec).Error.Message; got != source(codes.PermissionDenied) {
		t.Errorf("WriteGRPCError = %q, want the source sentence %q", got, source(codes.PermissionDenied))
	}
	rec = httptest.NewRecorder()
	restgw.WriteGRPCErrorCtx(ctx, rec, denied)
	if got := decodeEnvelope(t, rec).Error.Message; got != "K tomuto nemáte přístup." {
		t.Errorf("WriteGRPCErrorCtx = %q, want the catalog's sentence", got)
	}
}
