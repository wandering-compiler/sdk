package restgw

import (
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/captcha"
	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// The detail codes a captcha refusal carries in the envelope's details[],
// for a client to branch on: the first two are fixed by solving the captcha
// again, the last by trying later. The envelope's own `code` stays the
// canonical gRPC name (PERMISSION_DENIED, UNAVAILABLE), as on every other
// refusal.
const (
	CodeCaptchaRequired    = "CAPTCHA_REQUIRED"
	CodeCaptchaInvalid     = "CAPTCHA_INVALID"
	CodeCaptchaUnavailable = "CAPTCHA_UNAVAILABLE"
)

// Sentences for the captcha refusals, translated with the request locale
// and harvested through [Vocabulary].
const (
	MsgidCaptchaRequired    = "This request needs a solved captcha."
	MsgidCaptchaInvalid     = "The captcha was not accepted. Solve it again."
	MsgidCaptchaUnavailable = "The captcha could not be checked. Try again later."
)

// RequireCaptcha verifies the request's solved captcha (the
// [captcha.Header] header) with v, for an endpoint that requires one
// (RestEndpoint.captcha). It answers the refusal itself and reports false
// when the handler must stop: 403 PERMISSION_DENIED with detail
// CAPTCHA_REQUIRED without a token, 403 PERMISSION_DENIED with detail
// CAPTCHA_INVALID for one the provider rejects, 503 UNAVAILABLE with detail
// CAPTCHA_UNAVAILABLE when the provider cannot be asked — the gateway fails
// closed. The sentence is the catalog's, in the request's language; the
// cause goes to observability (a refusal on the non-exception channel, an
// unreachable provider on the exception channel). A nil v (no captcha
// secret configured) passes every request.
//
// The client address is the one [ClientAddrMiddleware] recorded, which the
// provider weighs with the token.
func RequireCaptcha(v *captcha.Verifier, w http.ResponseWriter, r *http.Request) bool {
	if !v.Enabled() {
		return true
	}
	ctx := r.Context()
	err := v.Verify(ctx, r.Header.Get(captcha.Header), ClientAddrFrom(ctx))
	var (
		code   codes.Code
		detail string
		msgid  string
	)
	switch {
	case err == nil:
		return true
	case errors.Is(err, captcha.ErrMissing):
		code, detail, msgid = codes.PermissionDenied, CodeCaptchaRequired, MsgidCaptchaRequired
	case errors.Is(err, captcha.ErrRejected):
		code, detail, msgid = codes.PermissionDenied, CodeCaptchaInvalid, MsgidCaptchaInvalid
	default:
		code, detail, msgid = codes.Unavailable, CodeCaptchaUnavailable, MsgidCaptchaUnavailable
	}
	reportByCode(ctx, code, status.Error(code, fmt.Sprintf("captcha (%s): %v", detail, err)))
	sentence := i18n.T(ctx, msgid, nil)
	WriteErrorWithDetails(w, HTTPStatusFromGRPCCode(code), GRPCCodeName(code), sentence,
		[]FieldError{{Code: detail, Message: sentence}})
	return false
}
