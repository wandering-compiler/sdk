package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// The business companion: the tier reaches the echo egress client and the
// notes-files store through the companion the harness runs (bizcompanion
// over the suite's manifest), with Go's semantics end to end.

func errorInfoOf(t *testing.T, err error) *errdetails.ErrorInfo {
	t.Helper()
	for _, d := range statusOf(t, err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info
		}
	}
	t.Fatalf("%v carries no google.rpc.ErrorInfo", err)
	return nil
}

// The tier's companion calls carry the gateway's principal and never the
// storage transaction or paging headers, though every one of them ran
// inside a distx.run.
func assertCompanionHeaders(t *testing.T, calls []companionCall) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("the companion saw no call")
	}
	user := gatewayCtxUser(t)
	for _, c := range calls {
		if got := c.md.Get("x-w17-user"); len(got) != 1 || got[0] != user {
			t.Errorf("%s: x-w17-user %v, want the gateway's", c.method, got)
		}
		if got := c.md.Get("w17-label-tenant"); len(got) != 1 || got[0] != "tenant-7" {
			t.Errorf("%s: w17-label-tenant %v", c.method, got)
		}
		for k := range c.md {
			if k == "w17-tx-id" || k == "w17-conn-id" || strings.HasPrefix(k, "x-w17-paging-") {
				t.Errorf("%s carried %s: a companion call is not part of a storage transaction", c.method, k)
			}
			if k == "authorization" {
				t.Errorf("%s carried the gateway's credential", c.method)
			}
		}
	}
}

func gatewayCtxUser(t *testing.T) string {
	t.Helper()
	md, _ := metadata.FromOutgoingContext(gatewayCtx(t))
	return md.Get("x-w17-user")[0]
}

func TestCompanionEgressCall(t *testing.T) {
	mark := env.companionCount()
	ctx, cancel := timeout(gatewayCtx(t, "x-w17-paging-limit", "3"), 10*time.Second)
	defer cancel()
	resp, err := env.call(ctx, "Echo", set(env.msg("EchoRequest"), map[string]any{"id": "42"}), "EchoResponse")
	if err != nil {
		t.Fatal(err)
	}
	// The upstream's body, reached with the DSN's credential.
	if got := get(resp, "message").String(); got != "echo 42 auth=Bearer conformance" {
		t.Fatalf("message %q", got)
	}
	calls := env.companionSince(mark)
	if len(calls) != 1 || calls[0].method != "/clients.echo.EchoApi/GetEcho" {
		t.Fatalf("companion calls %v", calls)
	}
	assertCompanionHeaders(t, calls)
}

func TestCompanionEgressErrorPassesThrough(t *testing.T) {
	_, err := env.call(gatewayCtx(t), "Echo", set(env.msg("EchoRequest"), map[string]any{"id": "missing"}), "EchoResponse")
	st := statusOf(t, err)
	if st.Code() != codes.NotFound || st.Message() != "echo: upstream answered HTTP 404" {
		t.Fatalf("got %v %q, want NotFound with the egress runtime's message", st.Code(), st.Message())
	}
	info := errorInfoOf(t, err)
	if info.GetDomain() != "echo" || info.GetReason() != "UPSTREAM_HTTP_404" || info.GetMetadata()["http_status"] != "404" {
		t.Fatalf("ErrorInfo %v", info)
	}
}

func TestCompanionEgressDeadline(t *testing.T) {
	ctx, cancel := timeout(gatewayCtx(t), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := env.call(ctx, "Echo", set(env.msg("EchoRequest"), map[string]any{"id": "slow"}), "EchoResponse")
	if code := statusOf(t, err).Code(); code != codes.DeadlineExceeded {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("the call took %s: the deadline did not reach the upstream call", took)
	}
}

var noteFileHandle = regexp.MustCompile(`^note-files/(?:[0-9a-f]{2}/)*([0-9a-f]{64})-[0-9a-f]{32}$`)

func TestCompanionFileRoundTrip(t *testing.T) {
	const size = 200 << 10
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251)
	}
	sum := sha256.Sum256(body)
	for _, part := range []int64{0, 50_000} { // one bytes value; an iterable of parts
		mark := env.companionCount()
		resp, err := env.call(gatewayCtx(t), "AttachFile",
			set(env.msg("AttachFileRequest"), map[string]any{"size": int64(size), "part": part}), "AttachFileResponse")
		if err != nil {
			t.Fatalf("part %d: %v", part, err)
		}
		handle := get(resp, "handle").String()
		m := noteFileHandle.FindStringSubmatch(handle)
		if m == nil || m[1] != hex.EncodeToString(sum[:]) {
			t.Fatalf("handle %q is not note-files/<sub-buckets>/<sha256 of the body>-<nonce>", handle)
		}
		// Byte-identical to what a Go tier's kvfs.Put would name it.
		objectKey := handle[strings.LastIndex(handle, "/")+1:]
		if want := kvfs.BuildKey("/note-files", objectKey, 100000, 100); handle != want {
			t.Fatalf("handle %q, kvfs.BuildKey says %q", handle, want)
		}
		if get(resp, "stat_size").Int() != size || !get(resp, "read_back_equal").Bool() {
			t.Fatalf("stat %d, read back equal %v", get(resp, "stat_size").Int(), get(resp, "read_back_equal").Bool())
		}
		if ms := get(resp, "mod_time_ms").Int(); time.Since(time.UnixMilli(ms)) > time.Minute || time.Until(time.UnixMilli(ms)) > time.Minute {
			t.Fatalf("mod time %v", time.UnixMilli(ms))
		}
		if !get(resp, "not_found_after_delete").Bool() {
			t.Fatal("opening the deleted handle was not what files.is_not_found recognises")
		}
		if _, err := os.Stat(filepath.Join(env.filesRoot, filepath.FromSlash(handle))); !os.IsNotExist(err) {
			t.Fatalf("the deleted file is still under the store root: %v", err)
		}
		calls := env.companionSince(mark)
		var methods []string
		for _, c := range calls {
			methods = append(methods, c.method[strings.LastIndex(c.method, "/")+1:])
			switch {
			case strings.HasSuffix(c.method, "/Put") && c.messages != 5:
				t.Errorf("part %d: Put sent %d messages, want the header and four 64 KiB chunks", part, c.messages)
			case strings.HasSuffix(c.method, "/Delete") && !c.existed:
				t.Errorf("part %d: the file was not at its handle under the store root before the delete", part)
			}
		}
		if got := strings.Join(methods, ","); got != "Put,Stat,Open,Delete,Open" {
			t.Fatalf("part %d: companion calls %s", part, got)
		}
		assertCompanionHeaders(t, calls)
	}
}

func TestCompanionFileRefusals(t *testing.T) {
	_, err := env.call(gatewayCtx(t), "AttachFile",
		set(env.msg("AttachFileRequest"), map[string]any{"size": int64(262144 + 1)}), "AttachFileResponse")
	if code := statusOf(t, err).Code(); code != codes.InvalidArgument {
		t.Fatalf("too large: %v", err)
	}
	if info := errorInfoOf(t, err); info.GetDomain() != "w17.files" || info.GetReason() != "FILE_TOO_LARGE" ||
		info.GetMetadata()["limit_bytes"] != "262144" || info.GetMetadata()["field"] != "conformance.v1.NoteFile.file" {
		t.Fatalf("too large: ErrorInfo %v", info)
	}

	mark := env.companionCount()
	resp, err := env.call(gatewayCtx(t), "AttachFile",
		set(env.msg("AttachFileRequest"), map[string]any{"case": "read_only_delete"}), "AttachFileResponse")
	if err != nil {
		t.Fatal(err)
	}
	if refused := get(resp, "refused").String(); !strings.Contains(refused, "notes-media") {
		t.Fatalf("a delete on the read-only connection: refused %q", refused)
	}
	if calls := env.companionSince(mark); len(calls) != 0 {
		t.Fatalf("the SDK sent the refused delete: %v", calls)
	}
}
