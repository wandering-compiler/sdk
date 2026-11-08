package conformance

import (
	"context"
	"encoding/base64"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/wandering-compiler/sdk/go/lib/paging"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// gatewayCtx is what a gateway sends the business tier: the verified
// principal, plus keys that must NOT travel further (a credential, paging,
// locale, a custom header) and one that must not ride an event (w17-tx-id is
// a transport key a client can never set; here it stands for anything the
// relay must leave alone).
func gatewayCtx(t *testing.T, extra ...string) context.Context {
	t.Helper()
	who := set(env.msg("AuthResp"), map[string]any{"user_id": "u-42"})
	raw, err := proto.Marshal(who)
	if err != nil {
		t.Fatal(err)
	}
	pairs := []string{
		"x-w17-user", base64.StdEncoding.EncodeToString(raw),
		"x-w17-scope-org_id", "org-7",
		"w17-label-tenant", "tenant-7",
		"x-w17-device", "device-jkt",
		"x-w17-data-scope", "org",
		"authorization", "Bearer must-not-travel",
		"w17-language", "cs",
		"x-custom", "c",
	}
	pairs = append(pairs, extra...)
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(pairs...))
}

func timeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

func headers(resp *dynamicpb.Message) map[string][]string {
	out := map[string][]string{}
	l := get(resp, "headers").List()
	for i := 0; i < l.Len(); i++ {
		h := l.Get(i).Message()
		vs := get(h, "values").List()
		for j := 0; j < vs.Len(); j++ {
			out[get(h, "key").String()] = append(out[get(h, "key").String()], vs.Get(j).String())
		}
	}
	return out
}

func notes(resp protoreflect.Message) []string {
	var out []string
	l := get(resp, "notes").List()
	for i := 0; i < l.Len(); i++ {
		out = append(out, get(l.Get(i).Message(), "text").String())
	}
	return out
}

func statusOf(t *testing.T, err error) *status.Status {
	t.Helper()
	if err == nil {
		t.Fatal("call succeeded; want an error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status: %v", err)
	}
	return st
}

// waitEvents waits until n events have arrived after mark (or the timeout).
func waitEvents(mark, n int, d time.Duration) []event {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := env.eventsSince(mark); len(got) >= n {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	return env.eventsSince(mark)
}

func decodeEnvelope(t *testing.T, raw []byte) *dynamicpb.Message {
	t.Helper()
	d, err := env.files.FindDescriptorByName("conformance.events.Event")
	if err != nil {
		t.Fatal(err)
	}
	m := dynamicpb.NewMessage(d.(protoreflect.MessageDescriptor))
	if err := proto.Unmarshal(raw, m); err != nil {
		t.Fatalf("envelope does not decode: %v", err)
	}
	return m
}

func stringMap(v protoreflect.Value) map[string]string {
	out := map[string]string{}
	v.Map().Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		out[k.String()] = v.String()
		return true
	})
	return out
}

func noteCreated(t *testing.T, evs []event) []string {
	t.Helper()
	var out []string
	for _, e := range evs {
		if e.topic != "note.created" {
			continue
		}
		env := decodeEnvelope(t, e.raw)
		ev := get(env, "v1_note_created").Message()
		out = append(out, get(ev, "text").String())
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------

func TestHeaderForwarding(t *testing.T) {
	ctx, cancel := timeout(gatewayCtx(t, "x-w17-paging-limit", "3"), 5*time.Second)
	defer cancel()
	resp, err := env.call(ctx, "Inspect", env.msg("InspectRequest"), "InspectResponse")
	if err != nil {
		t.Fatal(err)
	}
	h := headers(resp)
	for k, want := range map[string]string{
		"x-w17-user":         "",
		"x-w17-scope-org_id": "org-7",
		"w17-label-tenant":   "tenant-7",
		"x-w17-device":       "device-jkt",
		"x-w17-data-scope":   "org",
	} {
		got, ok := h[k]
		if !ok || len(got) != 1 || (want != "" && got[0] != want) {
			t.Errorf("storage saw %s = %v, want exactly [%s] relayed", k, got, want)
		}
	}
	for _, k := range []string{"authorization", "w17-language", "x-custom", "x-w17-paging-limit"} {
		if v, ok := h[k]; ok {
			t.Errorf("storage saw %s = %v: only the principal is relayed", k, v)
		}
	}
}

func TestPrincipalReadersAndConfig(t *testing.T) {
	ctx, cancel := timeout(gatewayCtx(t), 5*time.Second)
	defer cancel()
	resp, err := env.call(ctx, "Whoami", env.msg("WhoamiRequest"), "WhoamiResponse")
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"user_id": "u-42", "org_id": "org-7", "label_tenant": "tenant-7", "device_id": "device-jkt",
		"data_scope": "org", "greeting": "hello", "limit": int64(10), "verbose": false,
		"token_repr": "***:" + "11", // the secret prints as ***; its value is there
	} {
		if got := get(resp, k).Interface(); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
}

func TestDeadline(t *testing.T) {
	ctx, cancel := timeout(gatewayCtx(t), 4*time.Second)
	defer cancel()
	resp, err := env.call(ctx, "Inspect", env.msg("InspectRequest"), "InspectResponse")
	if err != nil {
		t.Fatal(err)
	}
	if !get(resp, "has_deadline").Bool() {
		t.Fatal("storage saw no deadline: the caller's did not propagate")
	}
	if ms := get(resp, "deadline_ms").Int(); ms <= 0 || ms > 4000 {
		t.Errorf("storage deadline = %dms, want within the caller's 4000ms", ms)
	}

	short, cancel2 := timeout(gatewayCtx(t), 300*time.Millisecond)
	defer cancel2()
	start := time.Now()
	_, err = env.call(short, "Sleep", set(env.msg("SleepRequest"), map[string]any{"ms": int32(5000)}), "Note")
	if st := statusOf(t, err); st.Code() != codes.DeadlineExceeded {
		t.Errorf("code = %s, want DeadlineExceeded", st.Code())
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("the deadline took %s to end the call", el)
	}
}

func TestErrorDetail(t *testing.T) {
	ctx, cancel := timeout(gatewayCtx(t), 5*time.Second)
	defer cancel()
	_, err := env.call(ctx, "Refuse", env.msg("RefuseRequest"), "Note")
	st := statusOf(t, err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %s", st.Code())
	}
	if want := "NoteService.Refuse: note 7 is locked by an export"; st.Message() != want {
		t.Errorf("status message = %q, want %q (grpcerr.ForUser's method: dev)", st.Message(), want)
	}
	var detail *w17pb.ErrorDetail
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			detail = ed
		}
	}
	if detail == nil {
		t.Fatalf("no w17.ErrorDetail in %v", st.Details())
	}
	if detail.GetCode() != "NOTE_LOCKED" || detail.GetMessage() != "This note is locked." || detail.GetField() != "text" {
		t.Errorf("detail = %v", detail)
	}
}

func TestEventAfterSuccessCarriesTheRequestMetadata(t *testing.T) {
	mark := env.eventCount()
	ctx, cancel := timeout(gatewayCtx(t), 5*time.Second)
	defer cancel()
	note, err := env.call(ctx, "CreateNote", set(env.msg("CreateNoteRequest"), map[string]any{"text": "single"}), "Note")
	if err != nil {
		t.Fatal(err)
	}
	evs := waitEvents(mark, 2, 5*time.Second)
	var created, invalidate *event
	for i := range evs {
		switch evs[i].topic {
		case "note.created":
			created = &evs[i]
		case "w17.invalidate":
			invalidate = &evs[i]
		}
	}
	if created == nil {
		t.Fatalf("no note.created event: got %v", evs)
	}
	e := decodeEnvelope(t, created.raw)
	ev := get(e, "v1_note_created").Message()
	if get(ev, "id").Int() != get(note, "id").Int() || get(ev, "text").String() != "single" || get(ev, "by").String() != "conformance" {
		t.Errorf("payload = %v", ev)
	}
	md := stringMap(get(e, "metadata"))
	for _, k := range []string{"x-w17-user", "x-w17-scope-org_id", "w17-label-tenant", "w17-language", "x-custom"} {
		if _, ok := md[k]; !ok {
			t.Errorf("envelope metadata lacks %s: %v", k, md)
		}
	}
	for k := range md {
		if k == "authorization" || k == "w17-tx-id" || k == "w17-conn-id" {
			t.Errorf("envelope metadata carries %s", k)
		}
	}
	if labels := stringMap(get(e, "labels")); !maps1(labels, map[string]string{"tenant": "tenant-7"}) {
		t.Errorf("labels = %v", labels)
	}
	if invalidate == nil {
		t.Fatal("no w17.invalidate event")
	}
	var inv w17pb.InvalidateEvent
	if err := proto.Unmarshal(invalidate.raw, &inv); err != nil {
		t.Fatal(err)
	}
	if inv.GetGroup() != "conformance.v1" || inv.GetMessage() != "Note" || inv.GetLabels()["tenant"] != "tenant-7" {
		t.Errorf("invalidate = %v", &inv)
	}
}

func maps1(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A nested transaction: CreateNotes runs CreateNote (through the served
// method, which runs its own distx.run) for each text — one transaction,
// committed once; the nested events are HELD until that commit.
func TestNestedTransactionHoldsEventsUntilCommit(t *testing.T) {
	mark := env.eventCount()
	before := len(env.rows(t))
	ctx, cancel := timeout(gatewayCtx(t), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := env.call(ctx, "CreateNotes",
			set(env.msg("CreateNotesRequest"), map[string]any{"texts": []string{"n1", "n2"}, "pause_ms": int32(800)}),
			"CreateNotesResponse")
		done <- err
	}()
	time.Sleep(400 * time.Millisecond)
	if got := noteCreated(t, env.eventsSince(mark)); len(got) != 0 {
		t.Errorf("events %v arrived while the transaction was still open", got)
	}
	if got := len(env.rows(t)); got != before {
		t.Errorf("rows visible before the commit: %d → %d", before, got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows := env.rows(t)
	if !slices.Equal(rows[before:], []string{"n1", "n2"}) {
		t.Errorf("rows = %v", rows[before:])
	}
	if got := noteCreated(t, waitEvents(mark, 4, 5*time.Second)); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Errorf("events after commit = %v, want n1 n2", got)
	}
}

func TestRollbackDropsWritesEventsAndOnCommit(t *testing.T) {
	mark := env.eventCount()
	before := len(env.rows(t))
	ctx, cancel := timeout(gatewayCtx(t), 10*time.Second)
	defer cancel()
	_, err := env.call(ctx, "CreateNotes",
		set(env.msg("CreateNotesRequest"), map[string]any{"texts": []string{"gone"}, "fail": true, "on_commit_text": "after-gone"}),
		"CreateNotesResponse")
	if st := statusOf(t, err); st.Code() != codes.Internal {
		t.Errorf("code = %s (%s), want Internal for an unexpected exception", st.Code(), st.Message())
	}
	time.Sleep(500 * time.Millisecond)
	if rows := env.rows(t); len(rows) != before {
		t.Errorf("rolled-back writes visible: %v", rows[before:])
	}
	if got := noteCreated(t, env.eventsSince(mark)); len(got) != 0 {
		t.Errorf("events of a rolled-back transaction: %v", got)
	}
}

func TestOnCommitRunsAfterCommit(t *testing.T) {
	before := len(env.rows(t))
	ctx, cancel := timeout(gatewayCtx(t), 10*time.Second)
	defer cancel()
	if _, err := env.call(ctx, "CreateNotes",
		set(env.msg("CreateNotesRequest"), map[string]any{"texts": []string{"kept"}, "on_commit_text": "after-kept"}),
		"CreateNotesResponse"); err != nil {
		t.Fatal(err)
	}
	if rows := env.rows(t); !slices.Equal(rows[before:], []string{"kept", "after-kept"}) {
		t.Errorf("rows = %v, want the write then the on_commit write", rows[before:])
	}
}

func TestRollbackOnly(t *testing.T) {
	before := len(env.rows(t))
	ctx, cancel := timeout(gatewayCtx(t), 10*time.Second)
	defer cancel()
	_, err := env.call(ctx, "CreateNotes",
		set(env.msg("CreateNotesRequest"), map[string]any{"texts": []string{"outer"}, "nested_fail_text": "inner"}),
		"CreateNotesResponse")
	st := statusOf(t, err)
	if !strings.Contains(st.Message(), "rolled back instead of committed") {
		t.Errorf("status = %s %q, want the rollback-only refusal", st.Code(), st.Message())
	}
	if rows := env.rows(t); len(rows) != before {
		t.Errorf("a rollback-only transaction committed: %v", rows[before:])
	}

	resp, err := env.call(ctx, "TxErrors", set(env.msg("TxErrorsRequest"), map[string]any{"case": "rollback_only"}), "TxErrorsResponse")
	if err != nil {
		t.Fatal(err)
	}
	if got := get(resp, "error").String(); got != "RollbackOnly" {
		t.Errorf("error = %q (%s), want RollbackOnly", got, get(resp, "message").String())
	}
}

func TestNestedMismatch(t *testing.T) {
	ctx, cancel := timeout(gatewayCtx(t), 10*time.Second)
	defer cancel()
	resp, err := env.call(ctx, "TxErrors", set(env.msg("TxErrorsRequest"), map[string]any{"case": "mismatch"}), "TxErrorsResponse")
	if err != nil {
		t.Fatal(err)
	}
	if got := get(resp, "error").String(); got != "NestedMismatch" {
		t.Errorf("error = %q (%s), want NestedMismatch", got, get(resp, "message").String())
	}
}

func TestHandleFinished(t *testing.T) {
	ctx, cancel := timeout(gatewayCtx(t), 10*time.Second)
	defer cancel()
	resp, err := env.call(ctx, "TxErrors", set(env.msg("TxErrorsRequest"), map[string]any{"case": "finished"}), "TxErrorsResponse")
	if err != nil {
		t.Fatal(err)
	}
	if got := get(resp, "error").String(); got != "HandleFinished" {
		t.Errorf("error = %q (%s), want HandleFinished", got, get(resp, "message").String())
	}
}

// The commit lands and its reply is lost: the probe rollback finds the
// transaction gone, so the outcome is unknown — and the write did land.
func TestCommitOutcomeUnknown(t *testing.T) {
	before := len(env.rows(t))
	env.mu.Lock()
	env.dropCommitReply = true
	env.mu.Unlock()
	ctx, cancel := timeout(gatewayCtx(t), 20*time.Second)
	defer cancel()
	resp, err := env.call(ctx, "TxErrors", set(env.msg("TxErrorsRequest"), map[string]any{"case": "unknown"}), "TxErrorsResponse")
	if err != nil {
		t.Fatal(err)
	}
	if got := get(resp, "error").String(); got != "CommitOutcomeUnknown" {
		t.Errorf("error = %q (%s), want CommitOutcomeUnknown", got, get(resp, "message").String())
	}
	if rows := env.rows(t); !slices.Equal(rows[before:], []string{"outcome-unknown"}) {
		t.Errorf("rows = %v: the commit landed although its reply was lost", rows[before:])
	}
}

// A paged list through a business method: page 1, page 2 and back to page 1,
// as the gateway drives it. The business method forwards the page to storage
// with paging.forward and knows nothing of its sort; storage reports where the
// page ends on its response headers, the business tier relays them (and only
// the forwarded call's, not its sibling's), and the next / previous boundary
// the gateway would put in its cursor comes from there.
func TestPagingThroughABusinessMethod(t *testing.T) {
	if _, err := env.db.Exec(`DELETE FROM notes`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"p1", "p2", "p3", "p4", "p5"} {
		if _, err := env.db.Exec(`INSERT INTO notes (text) VALUES (?)`, s); err != nil {
			t.Fatal(err)
		}
	}
	// page calls ListNotes as the gateway does: the limit and the boundary
	// (a cursor's, decoded) as metadata, the response headers collected.
	page := func(boundary string) (protoreflect.Message, paging.Position) {
		t.Helper()
		pairs := []string{paging.LimitMDKey, "2"}
		if boundary != "" {
			pairs = append(pairs, paging.BoundariesMDKey, boundary)
		}
		ctx, cancel := timeout(gatewayCtx(t, pairs...), 5*time.Second)
		defer cancel()
		var hdr metadata.MD
		resp, err := env.call(ctx, "ListNotes", env.msg("ListNotesRequest"), "ListNotesResponse", grpc.Header(&hdr))
		if err != nil {
			t.Fatal(err)
		}
		pos, ok := paging.PositionFromHeader(hdr)
		if !ok {
			t.Fatalf("the business method answered no page position (headers %v): the forwarded call's was not relayed", hdr)
		}
		return resp, pos
	}
	boundary := func(vals []*w17pb.KeysetValue, dir w17pb.Direction) string {
		t.Helper()
		v, err := paging.EncodeBoundariesMD(vals, dir)
		if err != nil || v == "" {
			t.Fatalf("encode boundary %v: %v", vals, err)
		}
		return v
	}
	p1, pos1 := page("")
	if got := notes(p1); !slices.Equal(got, []string{"p1", "p2"}) {
		t.Fatalf("page 1 = %v", got)
	}
	if pos1.Total != 5 || len(pos1.Next) != 1 || pos1.Next[0].GetIntV() == 999 {
		t.Fatalf("page 1 position = %+v: want the forwarded call's (total 5), not the sibling's (999)", pos1)
	}
	p2, pos2 := page(boundary(pos1.Next, w17pb.Direction_DIRECTION_FORWARD))
	if got := notes(p2); !slices.Equal(got, []string{"p3", "p4"}) {
		t.Errorf("page 2 = %v, want p3 p4 — the position did not reach storage", got)
	}
	// Back: storage runs the page reversed, the gateway turns it round.
	p0, _ := page(boundary(pos2.Prev, w17pb.Direction_DIRECTION_BACKWARD))
	back := notes(p0)
	slices.Reverse(back)
	if !slices.Equal(back, []string{"p1", "p2"}) {
		t.Errorf("previous page = %v, want p1 p2", back)
	}
	env.mu.Lock()
	sibling := env.inspected[len(env.inspected)-1]
	listed := env.listed[len(env.listed)-1]
	env.mu.Unlock()
	for _, k := range []string{paging.BoundariesMDKey, paging.LimitMDKey, paging.TotalMDKey} {
		if len(sibling.Get(k)) != 0 {
			t.Errorf("the sibling storage call got %s: paging bled", k)
		}
	}
	if got := listed.Get(paging.LimitMDKey); !slices.Equal(got, []string{"2"}) {
		t.Errorf("the paged call got limit %v", got)
	}
}
