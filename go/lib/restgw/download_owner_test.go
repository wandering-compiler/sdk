package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestServeDownloadFor_OwnerGate: an authenticated download route ran
// auth only, so any signed-in principal of ANY organization who learned
// an object key fetched another organization's private file. A file is
// served only to a caller whose data scopes match the ones it was
// uploaded under — the scopes the record it belongs to was stamped with.
func TestServeDownloadFor_OwnerGate(t *testing.T) {
	d := memory.New()
	// Org A uploads through the gateway's upload pipeline.
	up := multipartReq(t)
	handle, err := restgw.ProcessFilePart(context.Background(), up, restgw.FilePartConfig{
		FormName: "file", Driver: d, BucketPath: "/attachments",
		MaxSizeBytes: 1 << 20, AllowedExtensions: []string{"*"},
		Owner: map[string]string{"org_id": "org-a", "user_id": "u-1"},
	})
	if err != nil {
		t.Fatalf("ProcessFilePart: %v", err)
	}
	// An object stored without an owner (before this gate, or by a writer
	// that had no caller): in a scoped bucket it has no owner to match.
	d.PutBytes("attachments/legacy-0000", []byte("LEGACY"))

	scoped := restgw.NewDownloadConfig("/dl", []restgw.DownloadField{
		{BucketPath: "/attachments", Driver: d, Scopes: []string{"org_id"}},
	}, "", false, false, false)
	open := restgw.NewDownloadConfig("/dl", []restgw.DownloadField{
		{BucketPath: "/attachments", Driver: d},
	}, "", false, false, false)

	get := func(cfg restgw.DownloadConfig, key string, caller map[string]string) int {
		req := httptest.NewRequest(http.MethodGet, "/dl/"+strings.TrimPrefix(key, "/"), nil)
		rec := httptest.NewRecorder()
		restgw.ServeDownloadFor(rec, req, cfg, caller)
		return rec.Code
	}
	cases := []struct {
		name   string
		cfg    restgw.DownloadConfig
		key    string
		caller map[string]string
		want   int
	}{
		{"same org, other user (org-axis bucket)", scoped, handle, map[string]string{"org_id": "org-a", "user_id": "u-2"}, http.StatusOK},
		{"another organization", scoped, handle, map[string]string{"org_id": "org-b", "user_id": "u-9"}, http.StatusNotFound},
		{"a caller with no organization", scoped, handle, map[string]string{}, http.StatusNotFound},
		{"an object with no recorded owner, scoped bucket", scoped, "attachments/legacy-0000", map[string]string{"org_id": "org-a"}, http.StatusNotFound},
		{"unscoped bucket serves any caller", open, handle, map[string]string{"org_id": "org-b"}, http.StatusOK},
		{"unscoped bucket serves an ownerless object", open, "attachments/legacy-0000", nil, http.StatusOK},
	}
	for _, c := range cases {
		if got := get(c.cfg, c.key, c.caller); got != c.want {
			t.Errorf("%s: status %d, want %d (key %s)", c.name, got, c.want, c.key)
		}
	}
}
