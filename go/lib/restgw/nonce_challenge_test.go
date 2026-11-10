package restgw

import (
	"net/http"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// NonceChallenge reads WWW-Authenticate itself (lib/dpop stays out of this
// package); it must answer as dpop.IsUseNonceChallenge does on a 401.
func TestNonceChallenge_AgreesWithDPoP(t *testing.T) {
	for _, v := range []string{
		`DPoP error="use_dpop_nonce", algs="ES256 EdDSA"`,
		`dpop error=use_dpop_nonce`,
		`Bearer realm="x", DPoP error="use_dpop_nonce"`,
		`DPoP error="invalid_token"`,
		`Bearer error="use_dpop_nonce"`,
		`xdpop error="use_dpop_nonce"`,
		``,
	} {
		h := http.Header{}
		h.Set("WWW-Authenticate", v)
		if got, want := NonceChallenge(http.StatusUnauthorized, h), dpop.IsUseNonceChallenge([]string{v}); got != want {
			t.Errorf("%q: NonceChallenge %v, dpop.IsUseNonceChallenge %v", v, got, want)
		}
	}
}
