package stress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

type seenRequest struct {
	method, uri, body, auth, trace string
}

// A load request is the request the e2e runner would make for the same
// step: the same method, path with its parameters, query, JSON body and
// credential. A load test that hit a different route than the suite asserts
// would measure the wrong thing.
func TestFire_SendsTheRequestTheRunnerSends(t *testing.T) {
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.URL.RequestURI(), string(b), r.Header.Get("Authorization"), r.Header.Get("X-Trace")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ep := runner.Endpoint{
		Ref: "shop.Orders.Update", Transport: "rest", AuthRequired: true,
		HTTPMethod: "PATCH", PathTemplate: "/v1/shops/{shop_id}/orders/{id}",
		PathParams: []string{"shop_id", "id"}, QueryParams: []string{"dry_run"},
	}
	input := map[string]any{"shop_id": "s1", "id": "o 7", "dry_run": true, "note": "rush"}
	headers := map[string]string{"X-Trace": "t-1"}

	if _, err := runner.NewRESTCaller(srv.URL, nil).Call(context.Background(), ep, input, "tok", headers, "", nil); err != nil {
		t.Fatalf("runner call: %v", err)
	}
	if code, err := NewLoadCaller(srv.URL, 1, 5*time.Second).Fire(ep, input, "tok", headers); err != nil || code != http.StatusOK {
		t.Fatalf("load call = %d, %v", code, err)
	}
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(seen))
	}
	if seen[0] != seen[1] {
		t.Errorf("load and runner requests differ:\n runner %+v\n load   %+v", seen[0], seen[1])
	}
}
