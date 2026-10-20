package restgw

import (
	"errors"
	"log"
	"net/http"

	"github.com/wandering-compiler/sdk/go/lib/captcha"
)

// The error codes a captcha refusal answers with, for a client to branch
// on: the first two are fixed by solving the captcha again, the last by
// trying later.
const (
	CodeCaptchaRequired    = "CAPTCHA_REQUIRED"
	CodeCaptchaInvalid     = "CAPTCHA_INVALID"
	CodeCaptchaUnavailable = "CAPTCHA_UNAVAILABLE"
)

// RequireCaptcha verifies the request's solved captcha (the
// [captcha.Header] header) with v, for an endpoint that requires one
// (RestEndpoint.captcha). It answers the refusal itself and reports false
// when the handler must stop: 403 CAPTCHA_REQUIRED without a token, 403
// CAPTCHA_INVALID for one the provider rejects, 503 CAPTCHA_UNAVAILABLE when
// the provider cannot be asked — the gateway fails closed. A nil v (no
// captcha secret configured) passes every request.
//
// The client address is the one [ClientAddrMiddleware] recorded, which the
// provider weighs with the token.
func RequireCaptcha(v *captcha.Verifier, w http.ResponseWriter, r *http.Request) bool {
	if !v.Enabled() {
		return true
	}
	err := v.Verify(r.Context(), r.Header.Get(captcha.Header), ClientAddrFrom(r.Context()))
	switch {
	case err == nil:
		return true
	case errors.Is(err, captcha.ErrMissing):
		WriteError(w, http.StatusForbidden, CodeCaptchaRequired, "this request needs a solved captcha in the "+captcha.Header+" header")
	case errors.Is(err, captcha.ErrRejected):
		WriteError(w, http.StatusForbidden, CodeCaptchaInvalid, "the captcha was not accepted: solve it again")
	default:
		log.Printf("restgw: captcha verification failed: %v", err)
		WriteError(w, http.StatusServiceUnavailable, CodeCaptchaUnavailable, "the captcha could not be verified: try again later")
	}
	return false
}
