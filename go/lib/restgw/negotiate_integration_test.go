package restgw_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// echoHandler mirrors the generated unary handler shape: decode the
// request (format from Content-Type), then write it straight back
// (format negotiated from Accept). Exercises the real server path
// end-to-end over HTTP.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	msg := &wrapperspb.StringValue{}
	if err := restgw.DecodeRequest(r, msg); err != nil {
		restgw.WriteError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	restgw.WriteResponse(w, http.StatusOK, msg, r)
}

// TestNegotiation_HTTPRoundTrip drives a real httptest server through
// every negotiation combination: PB↔PB, JSON↔JSON, and the mixed
// directions (JSON request / PB response and vice versa).
func TestNegotiation_HTTPRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(echoHandler))
	defer srv.Close()

	pbBody, _ := proto.Marshal(wrapperspb.String("hello"))
	jsonBody, _ := protojson.Marshal(wrapperspb.String("hello"))

	cases := []struct {
		name        string
		body        []byte
		contentType string
		accept      string
		wantRespPB  bool
	}{
		{"pb→pb", pbBody, "application/protobuf", "application/protobuf", true},
		{"pb→pb (x-alias)", pbBody, "application/x-protobuf", "application/protobuf", true},
		{"json→json", jsonBody, "application/json", "application/json", false},
		{"json→json (no headers)", jsonBody, "", "", false},
		{"json req → pb resp", jsonBody, "application/json", "application/protobuf", true},
		{"pb req → json resp", pbBody, "application/protobuf", "application/json", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(c.body))
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			if c.accept != "" {
				req.Header.Set("Accept", c.accept)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			body, _ := io.ReadAll(resp.Body)
			ct := resp.Header.Get("Content-Type")

			got := &wrapperspb.StringValue{}
			if c.wantRespPB {
				if ct != "application/protobuf" {
					t.Fatalf("Content-Type = %q, want application/protobuf", ct)
				}
				if err := proto.Unmarshal(body, got); err != nil {
					t.Fatalf("response not valid protobuf: %v", err)
				}
			} else {
				if ct != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", ct)
				}
				if err := protojson.Unmarshal(body, got); err != nil {
					t.Fatalf("response not valid JSON: %v", err)
				}
			}
			if got.GetValue() != "hello" {
				t.Errorf("round-trip value = %q, want hello", got.GetValue())
			}
		})
	}
}
