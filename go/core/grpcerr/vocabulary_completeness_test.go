package grpcerr

import (
	"testing"

	"google.golang.org/grpc/codes"
)

// Vocabulary() is a list of CODES, and several codes share one sentence — so
// it was complete by COINCIDENCE, not by construction. A consumer found the
// shared arm that mattered: `FailedPrecondition` was answered with "The
// request could not be accepted as sent.", which is true for InvalidArgument
// and false here — their handler refused because a payment had too little left
// to allocate, and the caller was told to fix a request that was fine.
//
// Splitting that arm added a sentence the harvest list did not reach. A msgid
// outside it falls back to itself, so the new sentence would have rendered
// English in every declared language with nothing saying so — the exact drift
// restgw's clientFacing warns about, one layer down.
//
// Hence this test rather than a comment: the next split cannot forget.

// everyCode is every gRPC code UserMsgid switches on, plus the default arm's
// input. Listed explicitly because the point is to catch a sentence the
// PRODUCTION list misses — deriving both from one source would make the two
// agree by construction and prove nothing.
var everyCode = []codes.Code{
	codes.OK, codes.Canceled, codes.Unknown, codes.InvalidArgument,
	codes.DeadlineExceeded, codes.NotFound, codes.AlreadyExists,
	codes.PermissionDenied, codes.ResourceExhausted, codes.FailedPrecondition,
	codes.Aborted, codes.OutOfRange, codes.Unimplemented, codes.Internal,
	codes.Unavailable, codes.DataLoss, codes.Unauthenticated,
}

func TestVocabularyCoversEveryUserMsgid(t *testing.T) {
	inVocab := map[string]bool{}
	for _, m := range Vocabulary() {
		inVocab[m] = true
	}
	if len(inVocab) == 0 {
		t.Fatal("Vocabulary() is empty — nothing would be harvested at all")
	}
	for _, c := range everyCode {
		msg := UserMsgid(c)
		if msg == "" {
			t.Errorf("UserMsgid(%s) is empty — a caller would get a blank sentence", c)
			continue
		}
		if !inVocab[msg] {
			t.Errorf("UserMsgid(%s) = %q, which Vocabulary() does not carry — it will never be "+
				"harvested into a project's .po and renders English in every declared language, "+
				"silently. Add %s to Vocabulary()'s code list.", c, msg, c)
		}
	}
}

// The sentence for FailedPrecondition must not be the request-shaped one. This
// is the consumer's finding pinned directly: a shared arm here is not a style
// question, it tells the reader to fix the wrong thing.
func TestFailedPreconditionDoesNotBlameTheRequest(t *testing.T) {
	fp := UserMsgid(codes.FailedPrecondition)
	ia := UserMsgid(codes.InvalidArgument)
	if fp == ia {
		t.Fatalf("FailedPrecondition and InvalidArgument share %q. FailedPrecondition means the "+
			"request was understood and the STATE does not allow it, so a sentence about how the "+
			"request was sent sends the caller to fix something that is not broken", fp)
	}
	for _, blames := range []string{"as sent", "request could not be accepted"} {
		if containsFold(fp, blames) {
			t.Errorf("FailedPrecondition's sentence %q still blames the request (%q)", fp, blames)
		}
	}
}

func containsFold(hay, needle string) bool {
	if len(needle) > len(hay) {
		return false
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a, b := hay[i+j], needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
