package restgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestServeDownloadGated_RecordGate: an object of a bucket holding a scoped
// model's files is served only when the record gate (the storage tier's
// read of the referencing row, through the caller's scopes) admits it.
// Every "no" — and every refusal of the caller — is the same 404; a gate
// that could not be asked is a 500, not a "no such file".
func TestServeDownloadGated_RecordGate(t *testing.T) {
	d := memory.New()
	d.PutBytes("attachments/aa-0001", []byte("BODY"))
	gated := restgw.NewDownloadConfig("/dl", []restgw.DownloadField{
		{BucketPath: "/attachments", Driver: d, Gated: true},
	}, "", false, false, false)
	open := restgw.NewDownloadConfig("/dl", []restgw.DownloadField{
		{BucketPath: "/attachments", Driver: d},
	}, "", false, false, false)

	var asked []string
	gateOf := func(ok bool, err error) restgw.RecordGate {
		return func(_ context.Context, key string) (bool, error) {
			asked = append(asked, key)
			return ok, err
		}
	}
	cases := []struct {
		name string
		cfg  restgw.DownloadConfig
		gate restgw.RecordGate
		want int
	}{
		{"a record the caller may read references it", gated, gateOf(true, nil), http.StatusOK},
		{"no record the caller may read references it", gated, gateOf(false, nil), http.StatusNotFound},
		{"no gate on a gated bucket", gated, nil, http.StatusNotFound},
		{"storage refuses the caller's scopes", gated, gateOf(false, status.Error(codes.PermissionDenied, "missing required scope: org_id")), http.StatusNotFound},
		{"storage cannot be asked", gated, gateOf(false, status.Error(codes.Unavailable, "down")), http.StatusInternalServerError},
		{"a plain error asking", gated, gateOf(true, errors.New("boom")), http.StatusInternalServerError},
		{"an ungated bucket needs no gate", open, nil, http.StatusOK},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		restgw.ServeDownloadGated(rec, httptest.NewRequest(http.MethodGet, "/dl/attachments/aa-0001", nil), c.cfg, c.gate)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
	for _, k := range asked {
		if k != "attachments/aa-0001" {
			t.Errorf("gate asked about %q, want the stored key attachments/aa-0001", k)
		}
	}
	// ServeDownload is the no-gate form: a gated bucket is refused there.
	rec := httptest.NewRecorder()
	restgw.ServeDownload(rec, httptest.NewRequest(http.MethodGet, "/dl/attachments/aa-0001", nil), gated)
	if rec.Code != http.StatusNotFound {
		t.Errorf("ServeDownload over a gated bucket: %d, want 404", rec.Code)
	}
}
