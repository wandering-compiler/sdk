package s3_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

// fakeS3 is a minimal in-process, path-style S3 endpoint backing
// the SDK over HTTP. It implements just the verbs the Applier
// uses: ListObjectsV2, GetObject, PutObject, DeleteObject and the
// batch DeleteObjects. Pagination is forced to one key per page
// so the continuation-token loop is exercised with few objects.
type fakeS3 struct {
	mu          sync.Mutex
	objects     map[string][]byte            // object key (no bucket) -> body
	contentType map[string]string            // key -> Content-Type
	metadata    map[string]map[string]string // key -> user metadata (x-amz-meta-*)
	failGet     map[string]int               // key -> HTTP status to return on GET
	failPut     map[string]int               // key -> HTTP status to return on PUT
	failDelete  map[string]int               // key -> HTTP status to return on DELETE
	failList    bool                         // make every ListObjectsV2 return 500

	// Batch-delete (POST ?delete) fault hooks:
	failBatchDelete int      // >0 → return this HTTP status for the whole batch
	batchDeleteErrs []string // keys to report as per-object <Error> in the 200 body
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		objects:     map[string][]byte{},
		contentType: map[string]string{},
		metadata:    map[string]map[string]string{},
		failGet:     map[string]int{},
		failPut:     map[string]int{},
		failDelete:  map[string]int{},
	}
}

type lbrContents struct {
	Key  string `xml:"Key"`
	Size int64  `xml:"Size"`
}

type listResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	KeyCount              int           `xml:"KeyCount"`
	MaxKeys               int           `xml:"MaxKeys"`
	IsTruncated           bool          `xml:"IsTruncated"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	Contents              []lbrContents `xml:"Contents"`
}

type s3ErrBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(s3ErrBody{Code: code, Message: code})
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path-style: /{bucket}/{key...}. Strip the leading bucket
	// segment to recover the object key.
	p := strings.TrimPrefix(r.URL.Path, "/")
	slash := strings.IndexByte(p, '/')
	key := ""
	if slash >= 0 {
		key = p[slash+1:]
	}
	q := r.URL.Query()

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && q.Has("list-type"):
		f.handleList(w, q)
	case r.Method == http.MethodGet:
		f.handleGet(w, key)
	case r.Method == http.MethodPut:
		f.handlePut(w, r, key)
	case r.Method == http.MethodPost && q.Has("delete"):
		f.handleBatchDelete(w, r)
	case r.Method == http.MethodDelete:
		f.handleDelete(w, key)
	default:
		writeS3Error(w, http.StatusBadRequest, "BadRequest")
	}
}

func (f *fakeS3) handleList(w http.ResponseWriter, q url.Values) {
	if f.failList {
		writeS3Error(w, http.StatusInternalServerError, "InternalError")
		return
	}
	prefix := q.Get("prefix")
	token := q.Get("continuation-token")

	var keys []string
	for k := range f.objects {
		// Key-based cursor: emit keys strictly greater than the
		// continuation-token (the last key handed out). Stable
		// against deletion of already-emitted keys — matches how
		// real S3 tokens survive a delete-during-listing loop.
		if strings.HasPrefix(k, prefix) && k > token {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	// One key per page so the continuation-token loop is
	// exercised even with a handful of objects.
	res := listResult{Name: "bucket", Prefix: prefix, MaxKeys: 1}
	if len(keys) > 0 {
		k := keys[0]
		res.Contents = []lbrContents{{Key: k, Size: int64(len(f.objects[k]))}}
		res.KeyCount = 1
		if len(keys) > 1 {
			res.IsTruncated = true
			res.NextContinuationToken = k
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func (f *fakeS3) handleGet(w http.ResponseWriter, key string) {
	if status, ok := f.failGet[key]; ok {
		code := "InternalError"
		if status == http.StatusNotFound {
			code = "NoSuchKey"
		}
		writeS3Error(w, status, code)
		return
	}
	body, ok := f.objects[key]
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	if ct := f.contentType[key]; ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	for mk, mv := range f.metadata[key] {
		w.Header().Set("X-Amz-Meta-"+mk, mv)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (f *fakeS3) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	if status, ok := f.failPut[key]; ok {
		writeS3Error(w, status, "InternalError")
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.objects[key] = body
	// Record the metadata the SDK serialised so a test can assert a
	// rewrite carried it across. Overwrite per-PUT: a plain rewrite
	// (no ContentType / metadata set) therefore clears it.
	f.contentType[key] = r.Header.Get("Content-Type")
	meta := map[string]string{}
	for hk, hv := range r.Header {
		const prefix = "X-Amz-Meta-"
		if strings.HasPrefix(hk, prefix) && len(hv) > 0 {
			meta[strings.ToLower(strings.TrimPrefix(hk, prefix))] = hv[0]
		}
	}
	f.metadata[key] = meta
	w.Header().Set("ETag", `"deadbeef"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) handleDelete(w http.ResponseWriter, key string) {
	if status, ok := f.failDelete[key]; ok {
		code := "InternalError"
		if status == http.StatusNotFound {
			code = "NoSuchKey"
		}
		writeS3Error(w, status, code)
		return
	}
	delete(f.objects, key)
	w.WriteHeader(http.StatusNoContent)
}

type batchDeleteReq struct {
	XMLName xml.Name `xml:"Delete"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

func (f *fakeS3) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	if f.failBatchDelete > 0 {
		writeS3Error(w, f.failBatchDelete, "InternalError")
		return
	}
	var req batchDeleteReq
	raw, _ := io.ReadAll(r.Body)
	_ = xml.Unmarshal(raw, &req)
	errSet := map[string]bool{}
	for _, k := range f.batchDeleteErrs {
		errSet[k] = true
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><DeleteResult>`)
	for _, o := range req.Objects {
		if errSet[o.Key] {
			fmt.Fprintf(&sb, `<Error><Key>%s</Key><Code>AccessDenied</Code><Message>denied</Message></Error>`, o.Key)
			continue
		}
		delete(f.objects, o.Key)
	}
	sb.WriteString(`</DeleteResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, sb.String())
}

// newApplier stands up the fake server, wires the AWS env so the
// SDK uses static creds (no IMDS) + single-attempt retries (so
// 500 branches return fast), and returns an Applier pointed at it.
func newApplier(t *testing.T, f *fakeS3) *s3.Applier {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	dsn := "s3://bucket?endpoint=" + url.QueryEscape(srv.URL) + "&region=us-east-1"
	a, err := s3.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// TestAppliedHead_Empty pins that an empty tracker prefix yields
// the empty head ("no migrations applied yet").
func TestAppliedHead_Empty(t *testing.T) {
	a := newApplier(t, newFakeS3())
	head, err := a.AppliedHead(ctx(t))
	if err != nil {
		t.Fatalf("AppliedHead: %v", err)
	}
	if head != "" {
		t.Errorf("head = %q, want empty", head)
	}
}

// TestAppliedHead_MaxLex pins that AppliedHead returns the
// max-by-lex timestamp across a paginated listing, with the
// prefix + .json suffix stripped.
func TestAppliedHead_MaxLex(t *testing.T) {
	f := newFakeS3()
	f.objects["wc-migrations/20260101T000000Z.json"] = []byte("{}")
	f.objects["wc-migrations/20260301T000000Z.json"] = []byte("{}")
	f.objects["wc-migrations/20260201T000000Z.json"] = []byte("{}")
	a := newApplier(t, f)
	head, err := a.AppliedHead(ctx(t))
	if err != nil {
		t.Fatalf("AppliedHead: %v", err)
	}
	if head != "20260301T000000Z" {
		t.Errorf("head = %q, want 20260301T000000Z", head)
	}
}

// TestAppliedHead_ListError pins that a failed listing surfaces
// as an error rather than an empty head.
func TestAppliedHead_ListError(t *testing.T) {
	f := newFakeS3()
	f.failList = true
	a := newApplier(t, f)
	if _, err := a.AppliedHead(ctx(t)); err == nil {
		t.Error("expected list error")
	}
}

// TestApply_CommandPutAndDelete pins the command-script path:
// `S3 PUT` writes an object and `S3 DELETE` removes it, with
// comment / non-S3 marker lines skipped.
func TestApply_CommandPutAndDelete(t *testing.T) {
	f := newFakeS3()
	a := newApplier(t, f)
	body := "# wc: audit marker\n" +
		"aws s3 rm --recursive s3://x (user-DDL marker, ignored)\n" +
		"S3 PUT wc-migrations/ts-1.json {\"v\":1}\n"
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-1", UpSql: body}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(f.objects["wc-migrations/ts-1.json"]) != `{"v":1}` {
		t.Errorf("object not written: %q", f.objects["wc-migrations/ts-1.json"])
	}

	// up_post_tx is honoured too.
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{
		Id:       "ts-1b",
		UpSql:    "S3 PUT k/a x",
		UpPostTx: "S3 DELETE wc-migrations/ts-1.json",
	}); err != nil {
		t.Fatalf("Apply post_tx: %v", err)
	}
	if _, ok := f.objects["wc-migrations/ts-1.json"]; ok {
		t.Error("DELETE in up_post_tx did not remove object")
	}
}

// TestApply_DeletePrefix pins the recursive prefix delete across
// a paginated listing.
func TestApply_DeletePrefix(t *testing.T) {
	f := newFakeS3()
	f.objects["data/a"] = []byte("1")
	f.objects["data/b"] = []byte("2")
	f.objects["data/c"] = []byte("3")
	f.objects["keep/x"] = []byte("9")
	a := newApplier(t, f)
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{
		Id:    "ts-dp",
		UpSql: "S3 DELETE_PREFIX data/",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, k := range []string{"data/a", "data/b", "data/c"} {
		if _, ok := f.objects[k]; ok {
			t.Errorf("%s should have been deleted", k)
		}
	}
	if _, ok := f.objects["keep/x"]; !ok {
		t.Error("keep/x should survive a data/ prefix delete")
	}
}

// TestApply_CommandErrors pins the malformed-command branches of
// runOne reachable through the line-trimming run() loop. (The
// empty-key DELETE / DELETE_PREFIX guards are unreachable here —
// run() trims trailing whitespace — and are pinned white-box.)
func TestApply_CommandErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		frag string
	}{
		{"put missing body", "S3 PUT onlykey", "PUT missing body"},
		{"unsupported verb", "S3 FROBNICATE x", "unsupported S3 verb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newApplier(t, newFakeS3())
			err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "e", UpSql: tc.body})
			if err == nil || !strings.Contains(err.Error(), tc.frag) {
				t.Errorf("Apply(%q) err = %v, want fragment %q", tc.body, err, tc.frag)
			}
		})
	}
}

// TestApply_PostTxError pins the up_post_tx error-wrap branch.
func TestApply_PostTxError(t *testing.T) {
	a := newApplier(t, newFakeS3())
	err := a.Apply(ctx(t), &applyfetchpb.Migration{
		Id:       "ts-pt",
		UpSql:    "S3 PUT k/a x",
		UpPostTx: "S3 NOPE x",
	})
	if err == nil || !strings.Contains(err.Error(), "up_post_tx") {
		t.Errorf("expected up_post_tx error, got %v", err)
	}
}

// TestApply_DeleteObjectError pins the runOne DELETE error branch
// for a real (non-NoSuchKey) DeleteObject failure.
func TestApply_DeleteObjectError(t *testing.T) {
	f := newFakeS3()
	f.objects["doomed"] = []byte("x")
	f.failDelete["doomed"] = http.StatusInternalServerError
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-de", UpSql: "S3 DELETE doomed"})
	if err == nil || !strings.Contains(err.Error(), "DeleteObject") {
		t.Errorf("expected DeleteObject error, got %v", err)
	}
}

// TestApply_DeletePrefixListError pins the ListObjectsV2 error
// branch of deletePrefix.
func TestApply_DeletePrefixListError(t *testing.T) {
	f := newFakeS3()
	f.failList = true
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-dple", UpSql: "S3 DELETE_PREFIX data/"})
	if err == nil || !strings.Contains(err.Error(), "ListObjectsV2") {
		t.Errorf("expected ListObjectsV2 error, got %v", err)
	}
}

// TestApply_DeletePrefixBatchError pins the DeleteObjects top-level error
// branch of deletePrefix (the batch DELETE returns 500).
func TestApply_DeletePrefixBatchError(t *testing.T) {
	f := newFakeS3()
	f.objects["data/a"] = []byte("1")
	f.failBatchDelete = http.StatusInternalServerError
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-dpbe", UpSql: "S3 DELETE_PREFIX data/"})
	if err == nil || !strings.Contains(err.Error(), "DeleteObjects") {
		t.Errorf("expected DeleteObjects error, got %v", err)
	}
}

// TestApply_DeletePrefixPerObjectError pins the dout.Errors branch: the
// batch DELETE returns HTTP 200 but reports a per-object failure, which
// deletePrefix must surface rather than reporting success.
func TestApply_DeletePrefixPerObjectError(t *testing.T) {
	f := newFakeS3()
	f.objects["data/a"] = []byte("1")
	f.objects["data/b"] = []byte("2")
	f.batchDeleteErrs = []string{"data/a"}
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-dppoe", UpSql: "S3 DELETE_PREFIX data/"})
	if err == nil || !strings.Contains(err.Error(), "object(s) failed") {
		t.Errorf("expected per-object failure error, got %v", err)
	}
	if !strings.Contains(err.Error(), "data/a") {
		t.Errorf("error should name the failed key, got %v", err)
	}
}

// TestRollback_CommandErrors pins the down_pre_tx and down_sql
// error-wrap branches of the command Rollback path.
func TestRollback_CommandErrors(t *testing.T) {
	a := newApplier(t, newFakeS3())
	if err := a.Rollback(ctx(t), &applyfetchpb.Migration{
		Id:        "ts-rpe",
		DownPreTx: "S3 NOPE x",
	}); err == nil || !strings.Contains(err.Error(), "down_pre_tx") {
		t.Errorf("expected down_pre_tx error, got %v", err)
	}
	if err := a.Rollback(ctx(t), &applyfetchpb.Migration{
		Id:      "ts-rse",
		DownSql: "S3 NOPE x",
	}); err == nil || !strings.Contains(err.Error(), "down_sql") {
		t.Errorf("expected down_sql error, got %v", err)
	}
}

// TestApply_PutServerError pins that a PutObject failure surfaces
// as an Apply error.
func TestApply_PutServerError(t *testing.T) {
	f := newFakeS3()
	f.failPut["boom/k"] = http.StatusInternalServerError
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "p", UpSql: "S3 PUT boom/k payload"})
	if err == nil || !strings.Contains(err.Error(), "PutObject") {
		t.Errorf("expected PutObject error, got %v", err)
	}
}

// TestRollback_CommandPath pins the down command path: down_pre_tx
// then down_sql, with DELETE tolerating a missing key.
func TestRollback_CommandPath(t *testing.T) {
	f := newFakeS3()
	f.objects["wc-migrations/ts-r.json"] = []byte("{}")
	a := newApplier(t, f)
	err := a.Rollback(ctx(t), &applyfetchpb.Migration{
		Id:        "ts-r",
		DownPreTx: "# pre marker",
		DownSql:   "S3 DELETE wc-migrations/ts-r.json\nS3 DELETE wc-migrations/already-gone.json",
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, ok := f.objects["wc-migrations/ts-r.json"]; ok {
		t.Error("object should have been deleted")
	}
}

const yamlAdd = `version: 1
encoding: json
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: users/
    field: active
    value: true
`

// TestApply_YAMLDataMigration pins the full YAML data-migration
// apply path end to end: list keyspace, read each object, mutate
// JSON, write back, persist + clear the cursor, and record the
// bookkeeping object.
func TestApply_YAMLDataMigration(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.objects["users/2"] = []byte(`{"id":2}`)
	a := newApplier(t, f)

	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-yaml", UpSql: yamlAdd}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, k := range []string{"users/1", "users/2"} {
		if !strings.Contains(string(f.objects[k]), `"active":true`) {
			t.Errorf("%s missing added field: %s", k, f.objects[k])
		}
	}
	// Bookkeeping object written, cursor cleared (no residue).
	if _, ok := f.objects["wc-migrations/ts-yaml.json"]; !ok {
		t.Error("bookkeeping object not written")
	}
	for k := range f.objects {
		if strings.HasPrefix(k, "wc-data-migrations/") {
			t.Errorf("cursor object %s should have been deleted", k)
		}
	}
}

// TestApply_YAMLPreservesObjectMetadata — INVARIANT: a per-object
// rewrite carries the original object's Content-Type and user
// Metadata across. A PutObject populating only Bucket/Key/Body
// would reset both to S3 defaults, silently corrupting served
// objects; the rewrite replays them off the GetObject output.
func TestApply_YAMLPreservesObjectMetadata(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.contentType["users/1"] = "application/json"
	f.metadata["users/1"] = map[string]string{"owner": "alice"}
	a := newApplier(t, f)

	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-meta", UpSql: yamlAdd}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(string(f.objects["users/1"]), `"active":true`) {
		t.Fatalf("object not rewritten: %s", f.objects["users/1"])
	}
	if ct := f.contentType["users/1"]; ct != "application/json" {
		t.Errorf("Content-Type lost on rewrite: got %q, want application/json", ct)
	}
	if owner := f.metadata["users/1"]["owner"]; owner != "alice" {
		t.Errorf("user metadata lost on rewrite: owner = %q, want alice", owner)
	}
}

// TestApply_YAMLResumesFromCursor pins the resumability path: a
// pre-existing cursor marking op 0 complete makes the apply skip
// op 0 (so the object stays untouched) yet still finish cleanly.
func TestApply_YAMLResumesFromCursor(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.objects["wc-data-migrations/ts-resume.cursor.json"] = []byte(`{"completed_ops":[0]}`)
	a := newApplier(t, f)

	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-resume", UpSql: yamlAdd}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(string(f.objects["users/1"]), "active") {
		t.Error("op 0 should have been skipped (cursor marked complete)")
	}
	if _, ok := f.objects["wc-migrations/ts-resume.json"]; !ok {
		t.Error("bookkeeping object not written")
	}
}

// TestApply_YAMLCursorDecodeError pins that a corrupt cursor
// object surfaces as a cursor-read error.
func TestApply_YAMLCursorDecodeError(t *testing.T) {
	f := newFakeS3()
	f.objects["wc-data-migrations/ts-badcur.cursor.json"] = []byte(`{not json`)
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-badcur", UpSql: yamlAdd})
	if err == nil || !strings.Contains(err.Error(), "cursor read") {
		t.Errorf("expected cursor read error, got %v", err)
	}
}

// TestApply_YAMLListError pins that a failure while listing the
// keyspace surfaces through the op error.
func TestApply_YAMLListError(t *testing.T) {
	f := newFakeS3()
	f.failList = true
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-le", UpSql: yamlAdd})
	if err == nil || !strings.Contains(err.Error(), "ListObjectsV2") {
		t.Errorf("expected ListObjectsV2 error, got %v", err)
	}
}

// TestApply_YAMLParseError pins the YAML parse-refusal branch.
func TestApply_YAMLParseError(t *testing.T) {
	a := newApplier(t, newFakeS3())
	err := a.Apply(ctx(t), &applyfetchpb.Migration{
		Id:    "ts-badyaml",
		UpSql: "version: 1\noperations: [unterminated\n",
	})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error, got %v", err)
	}
}

// TestRollback_YAMLDataMigration pins the YAML rollback path: a
// valid down body runs the inverse op + erases the bookkeeping
// object.
func TestRollback_YAMLDataMigration(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1,"active":true}`)
	f.objects["wc-migrations/ts-rb.json"] = []byte("{}")
	a := newApplier(t, f)
	down := `version: 1
encoding: json
operations:
  - op: REMOVE_FIELD
    keyspace: users/
    field: active
`
	err := a.Rollback(ctx(t), &applyfetchpb.Migration{Id: "ts-rb", UpSql: yamlAdd, DownSql: down})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if strings.Contains(string(f.objects["users/1"]), "active") {
		t.Errorf("active should have been removed: %s", f.objects["users/1"])
	}
	if _, ok := f.objects["wc-migrations/ts-rb.json"]; ok {
		t.Error("bookkeeping object should have been erased")
	}
}

// TestRollback_YAMLIrreversibleRefused pins the irreversible
// refusal (no S3 work).
func TestRollback_YAMLIrreversibleRefused(t *testing.T) {
	a := newApplier(t, newFakeS3())
	err := a.Rollback(ctx(t), &applyfetchpb.Migration{
		Id:      "ts-irr",
		UpSql:   yamlAdd,
		DownSql: "# wc:irreversible: REMOVE_FIELD has no inverse",
	})
	if err == nil || !strings.Contains(err.Error(), "irreversible") {
		t.Errorf("expected irreversible refusal, got %v", err)
	}
}

// TestApply_YAMLTransform pins the TRANSFORM_FIELD path through
// the Starlark VM (encoding-agnostic per-object transform).
func TestApply_YAMLTransform(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"n":1}`)
	a := newApplier(t, f)
	body := `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: users/
    script_lang: starlark
    script: |
      def transform(value):
          doc = json.decode(str(value))
          doc["tagged"] = True
          return bytes(json.encode(doc))
`
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-tf", UpSql: body}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(string(f.objects["users/1"]), "tagged") {
		t.Errorf("transform not applied: %s", f.objects["users/1"])
	}
}

// TestApply_YAMLTransformResumeRefused — INVARIANT (V-11): a
// TRANSFORM_FIELD op whose cursor shows it started but never
// completed (a mid-op crash) is REFUSED on resume rather than
// silently re-run. The user's Starlark transform may be non-
// idempotent (here it increments `n` every run), so the object the
// op already touched must stay byte-identical on the refused resume
// — proof no double-apply happens.
func TestApply_YAMLTransformResumeRefused(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"n":1}`)
	// Simulate a crash mid-op: op 0 is marked started but never completed.
	f.objects["wc-data-migrations/ts-tfresume.cursor.json"] = []byte(`{"completed_ops":[],"started_ops":[0]}`)
	a := newApplier(t, f)
	body := `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: users/
    script_lang: starlark
    script: |
      def transform(value):
          doc = json.decode(str(value))
          doc["n"] = doc["n"] + 1
          return bytes(json.encode(doc))
`
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-tfresume", UpSql: body})
	if err == nil || !strings.Contains(err.Error(), "TRANSFORM_FIELD") {
		t.Fatalf("expected refusal naming TRANSFORM_FIELD, got %v", err)
	}
	if string(f.objects["users/1"]) != `{"n":1}` {
		t.Errorf("refused resume must not re-transform; users/1 = %s", f.objects["users/1"])
	}
}

// TestApply_YAMLNoOpAndRace pins two per-object branches: an
// object that already has the field is a no-op (no PutObject, so
// it is left byte-identical) and an object that vanishes between
// list and get (NoSuchKey on GET) is silently skipped.
func TestApply_YAMLNoOpAndRace(t *testing.T) {
	f := newFakeS3()
	f.objects["users/has"] = []byte(`{"id":1,"active":true}`) // already set → no-op
	f.objects["users/race"] = []byte(`{"id":2}`)              // present...
	f.failGet["users/race"] = http.StatusNotFound             // ...but vanishes on GET
	a := newApplier(t, f)

	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-noop", UpSql: yamlAdd}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(f.objects["users/has"]) != `{"id":1,"active":true}` {
		t.Errorf("no-op object was rewritten: %s", f.objects["users/has"])
	}
}

// TestApply_YAMLPutObjectError pins that a PutObject failure
// while writing a mutated object surfaces as an op error.
func TestApply_YAMLPutObjectError(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.failPut["users/1"] = http.StatusInternalServerError
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-pe", UpSql: yamlAdd})
	if err == nil || !strings.Contains(err.Error(), "PutObject") {
		t.Errorf("expected PutObject error, got %v", err)
	}
}

// TestApply_YAMLEmptyCursorObject pins the empty-body branch of
// loadCursor: a present-but-empty cursor object decodes to a
// fresh cursor (no completed ops) rather than a JSON error.
func TestApply_YAMLEmptyCursorObject(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.objects["wc-data-migrations/ts-empty.cursor.json"] = []byte("")
	a := newApplier(t, f)
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-empty", UpSql: yamlAdd}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(string(f.objects["users/1"]), "active") {
		t.Error("op should still run with an empty cursor")
	}
}

// TestApply_YAMLSaveCursorError pins the cursor-write error
// branch: a failing PUT on the cursor key aborts the migration.
func TestApply_YAMLSaveCursorError(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.failPut["wc-data-migrations/ts-sce.cursor.json"] = http.StatusInternalServerError
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-sce", UpSql: yamlAdd})
	if err == nil || !strings.Contains(err.Error(), "cursor write") {
		t.Errorf("expected cursor write error, got %v", err)
	}
}

// TestApply_YAMLDeleteCursorError pins the final cursor-delete
// error branch (bookkeeping already written, cursor delete fails).
func TestApply_YAMLDeleteCursorError(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.failDelete["wc-data-migrations/ts-dce.cursor.json"] = http.StatusInternalServerError
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-dce", UpSql: yamlAdd})
	if err == nil || !strings.Contains(err.Error(), "cursor delete") {
		t.Errorf("expected cursor delete error, got %v", err)
	}
}

// TestApply_YAMLBookkeepingError pins that a failure writing the
// bookkeeping object surfaces as a bookkeeping error.
func TestApply_YAMLBookkeepingError(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.failPut["wc-migrations/ts-bk.json"] = http.StatusInternalServerError
	a := newApplier(t, f)
	err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-bk", UpSql: yamlAdd})
	if err == nil || !strings.Contains(err.Error(), "bookkeeping") {
		t.Errorf("expected bookkeeping error, got %v", err)
	}
}

// TestRollback_YAMLParseError pins the down-body parse-refusal
// branch of rollbackYAMLDataMigration.
func TestRollback_YAMLParseError(t *testing.T) {
	a := newApplier(t, newFakeS3())
	err := a.Rollback(ctx(t), &applyfetchpb.Migration{
		Id:      "ts-rbpe",
		UpSql:   yamlAdd,
		DownSql: "version: 1\noperations: [unterminated\n",
	})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected rollback parse error, got %v", err)
	}
}

// TestRollback_YAMLCodecError pins the protobuf-codec refusal
// branch on the rollback path.
func TestRollback_YAMLCodecError(t *testing.T) {
	a := newApplier(t, newFakeS3())
	down := `version: 1
encoding: protobuf
proto_descriptor: aGVsbG8=
proto_message: pkg.User
operations:
  - op: REMOVE_FIELD
    keyspace: users/
    field: active
`
	err := a.Rollback(ctx(t), &applyfetchpb.Migration{Id: "ts-rbce", UpSql: yamlAdd, DownSql: down})
	if err == nil || strings.Contains(err.Error(), "cursor") {
		t.Errorf("expected codec error before network, got %v", err)
	}
}

// TestSetParallelOverride pins the operator override is honoured
// by the YAML path (here just that it does not break a 2-object
// migration).
func TestSetParallelOverride(t *testing.T) {
	f := newFakeS3()
	f.objects["users/1"] = []byte(`{"id":1}`)
	f.objects["users/2"] = []byte(`{"id":2}`)
	a := newApplier(t, f)
	a.SetParallelOverride(2)
	if err := a.Apply(ctx(t), &applyfetchpb.Migration{Id: "ts-par", UpSql: yamlAdd}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}
