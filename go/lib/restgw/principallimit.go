package restgw

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// AdmitPrincipal charges one authenticated request to its caller's
// per-principal budget (`principal_rate`, [ratelimit.NewPrincipal]): cost
// tokens, or nothing for an exempt method (cost <= 0). authResp is what the
// auth method answered, marshalled — the caller is its `user_id`, field 2 of
// every auth response by contract. It reports false after writing the 429,
// with Retry-After the seconds until the bucket can pay the cost.
//
// A nil limiter (the limit is off) admits everything, as does an answer
// without an id.
func AdmitPrincipal(l *ratelimit.Limiter, w http.ResponseWriter, r *http.Request, authResp []byte, cost int) bool {
	if l == nil || cost <= 0 {
		return true
	}
	ok, wait := l.AllowPrincipal(PrincipalOf(authResp), cost)
	if ok {
		return true
	}
	WriteTooManyRequestsAfter(r.Context(), w, wait)
	return false
}

// PrincipalOf is the user_id (field 2) of a marshalled auth response, "" when
// it has none or does not parse.
func PrincipalOf(authResp []byte) string {
	b := authResp
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ""
		}
		b = b[n:]
		if num == 2 && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return ""
			}
			return string(v)
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return ""
		}
		b = b[m:]
	}
	return ""
}

// WriteTooManyRequestsAfter is the 429 with the wait the limiter computed,
// in whole seconds, at least one.
func WriteTooManyRequestsAfter(ctx context.Context, w http.ResponseWriter, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	WriteError(w, http.StatusTooManyRequests, GRPCCodeName(codes.ResourceExhausted),
		ClientSentence(ctx, codes.ResourceExhausted))
}
