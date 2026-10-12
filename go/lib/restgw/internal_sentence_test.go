package restgw_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// Every 500 the gateway writes for its own failure carries the INTERNAL
// code's catalog sentence — the one a backend Internal gets — never a
// developer string such as "failed to marshal response" or "nil error".
func TestInternalFailures_CarryTheCatalogSentence(t *testing.T) {
	want := restgw.ClientSentence(context.Background(), codes.Internal)
	// Invalid UTF-8 in a proto3 string fails both marshalers.
	bad := wrapperspb.String("\xff\xfe")

	cases := map[string]func(w http.ResponseWriter){
		"JSON marshal failure": func(w http.ResponseWriter) {
			restgw.WriteResponse(w, http.StatusOK, bad, httptest.NewRequest(http.MethodGet, "/", nil))
		},
		"protobuf marshal failure": func(w http.ResponseWriter) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept", restgw.MIMEProtobuf)
			restgw.WriteResponse(w, http.StatusOK, bad, r)
		},
		"marshal failure without a request": func(w http.ResponseWriter) {
			restgw.WriteResponse(w, http.StatusOK, bad)
		},
		"filtered marshal failure": func(w http.ResponseWriter) {
			restgw.WriteResponseFiltered(w, http.StatusOK, bad, []string{"nope"})
		},
		"nil backend error": func(w http.ResponseWriter) {
			restgw.WriteGRPCErrorCtx(context.Background(), w, nil)
		},
		"non-status backend error": func(w http.ResponseWriter) {
			restgw.WriteGRPCErrorCtx(context.Background(), w, errors.New("dial tcp 10.0.0.5:9000"))
		},
		"panic": func(w http.ResponseWriter) {
			restgw.RecoverPanicMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic("boom")
			})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			write(rec)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			var env struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not the envelope: %v: %s", err, rec.Body.String())
			}
			if env.Error.Code != "INTERNAL" || env.Error.Message != want {
				t.Errorf("envelope = %+v, want INTERNAL with %q", env.Error, want)
			}
		})
	}

	for _, err := range []error{nil, errors.New("dial tcp 10.0.0.5:9000")} {
		if c, msg, _ := restgw.ClientError(context.Background(), err); c != codes.Internal || msg != want {
			t.Errorf("ClientError(%v) = %v %q, want Internal %q", err, c, msg, want)
		}
	}
}
