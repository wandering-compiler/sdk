package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func TestMetadataPropagationMiddleware_ForwardsConfiguredHeaders(t *testing.T) {
	var captured metadata.MD
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		md, _ := metadata.FromOutgoingContext(r.Context())
		captured = md
	})
	mw := restgw.MetadataPropagationMiddleware([]string{"X-Request-Id", "Traceparent", "Baggage"}, inner)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "rid-42")
	req.Header.Set("Traceparent", "00-abc-def-01")
	// Baggage left unset on purpose.
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if got := captured.Get("x-request-id"); len(got) != 1 || got[0] != "rid-42" {
		t.Errorf("x-request-id: want [rid-42], got %v", got)
	}
	if got := captured.Get("traceparent"); len(got) != 1 || got[0] != "00-abc-def-01" {
		t.Errorf("traceparent: want [00-abc-def-01], got %v", got)
	}
	if got := captured.Get("baggage"); len(got) != 0 {
		t.Errorf("baggage: want absent, got %v", got)
	}
}

func TestMetadataPropagationMiddleware_EmptyHeaders_IsPassthrough(t *testing.T) {
	var saw bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = true
		if _, ok := metadata.FromOutgoingContext(r.Context()); ok {
			t.Error("pass-through wrap should not seed outgoing metadata")
		}
	})
	mw := restgw.MetadataPropagationMiddleware(nil, inner)
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !saw {
		t.Error("inner handler not invoked")
	}
}

func TestMetadataPropagationMiddleware_NoHeadersPresent_NoMetadataAttached(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		md, ok := metadata.FromOutgoingContext(r.Context())
		if ok && md.Len() > 0 {
			t.Errorf("no headers present → no outgoing metadata; got %v", md)
		}
	})
	mw := restgw.MetadataPropagationMiddleware([]string{"X-Request-Id"}, inner)
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

func TestMetadataPropagationMiddleware_PreservesIncomingMetadata(t *testing.T) {
	// Simulate an upstream layer that already attached
	// outgoing metadata via context — propagation must
	// append on top, not overwrite.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		md, _ := metadata.FromOutgoingContext(r.Context())
		if got := md.Get("upstream-key"); len(got) != 1 || got[0] != "upstream-val" {
			t.Errorf("upstream-key: want [upstream-val], got %v", got)
		}
		if got := md.Get("x-request-id"); len(got) != 1 || got[0] != "rid-99" {
			t.Errorf("x-request-id: want [rid-99], got %v", got)
		}
	})
	mw := restgw.MetadataPropagationMiddleware([]string{"X-Request-Id"}, inner)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "rid-99")
	ctx := metadata.AppendToOutgoingContext(context.Background(), "upstream-key", "upstream-val")
	mw.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
}
