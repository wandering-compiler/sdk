package restgw

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// invalidArgStatus is a backend refusal with the OPERATOR's message on it — the
// shape clientFacing has to demote. Named locally because the package's tests
// already have an unrelated `statusOf`.
func invalidArgStatus() *status.Status {
	return status.New(codes.InvalidArgument, "SomeService/Method: validation failed")
}

// One sentence per code on the whole surface, which is what surfaces.md promises
// and what a consumer measured to be false on 2026-09-30.
//
// The two halves of INVALID_ARGUMENT reach a client by different routes: a backend
// status gets demoted by clientFacing, while the gateway's own pre-flight
// validation has no status to demote and used to write the literal
// `"validation failed"` into the envelope. From outside, one code answered two
// sentences — and the spec's "an assertion on it cannot fail" was advice built on
// the false half.

func TestPreflightSentence_IsTheCatalogueSentenceForInvalidArgument(t *testing.T) {
	ctx := context.Background()
	got := PreflightSentence(ctx)

	want := ClientSentence(ctx, codes.InvalidArgument)
	if got != want {
		t.Errorf("PreflightSentence = %q, want the same sentence clientFacing falls back to (%q)", got, want)
	}
	// NOT compared against grpcerr.UserMsgid directly, which an earlier draft did.
	// That equality holds only because a background ctx carries no locale and
	// `i18n.T` falls back to the msgid — so it pins the FALLBACK while claiming to
	// pin the table, and the first translated catalogue would fail it for the wrong
	// reason. The invariant worth having is that both ROUTES agree, in any locale;
	// the table is what makes that true rather than what is asserted.
	if got == "" {
		t.Error("PreflightSentence returned an empty sentence — the envelope would carry no message at all")
	}
}

// The same agreement under a locale, which is where the msgid comparison an earlier
// draft used would have gone wrong: a translated catalogue moves both routes or
// neither, and "neither" is the bug this file exists for.
func TestPreflightSentence_AgreesWithTheDemotionUnderALocale(t *testing.T) {
	for _, lang := range []string{"", "en", "cs"} {
		// The locale rides on gRPC metadata, and a GATEWAY's own request locale is
		// on the OUTGOING side — it is the one attaching metadata for the hop it is
		// about to make, which is exactly the path the pre-flight validation runs on.
		ctx := metadata.AppendToOutgoingContext(context.Background(), i18n.LanguageMetadataKey, lang)
		_, viaDemotion := clientFacing(ctx, invalidArgStatus(), nil)
		if got := PreflightSentence(ctx); got != viaDemotion {
			t.Errorf("lang %q: pre-flight says %q, the demotion says %q — one code must answer one sentence in every language",
				lang, got, viaDemotion)
		}
	}
}

// The specific string that was leaking, named so the regression is unmistakable.
func TestPreflightSentence_IsNotTheOperatorsPhrasing(t *testing.T) {
	if got := PreflightSentence(context.Background()); got == "validation failed" {
		t.Error("the envelope is carrying the OPERATOR's phrasing again: lower case, no full stop, developer vocabulary")
	}
}

// The other half of the promise: the demotion path and the pre-flight path must
// agree for the SAME code. Asserted through clientFacing so the comparison runs
// over the code a client actually branches on.
func TestClientFacing_AgreesWithPreflightWhenNothingWasAttached(t *testing.T) {
	ctx := context.Background()
	_, viaDemotion := clientFacing(ctx, invalidArgStatus(), nil)
	if viaDemotion != PreflightSentence(ctx) {
		t.Errorf("a backend refusal says %q while the gateway's own says %q — one code, two sentences, which is the finding",
			viaDemotion, PreflightSentence(ctx))
	}
}

// A request-level detail still wins over both. That is step 1 of the demotion and
// the whole reason a handler can phrase its own refusal; a fix that flattened it
// would trade one bug for a worse one.
func TestClientFacing_RequestLevelDetailStillWins(t *testing.T) {
	ctx := context.Background()
	details := []FieldError{{Field: "", Code: "NOT_ENOUGH_CREDIT", Message: "Your balance is too low for this."}}
	_, msg := clientFacing(ctx, invalidArgStatus(), details)
	if msg != "Your balance is too low for this." {
		t.Errorf("clientFacing = %q, want the request-level detail's own sentence", msg)
	}
	if msg == PreflightSentence(ctx) {
		t.Error("the generic sentence overrode a sentence somebody wrote for this failure")
	}
}
