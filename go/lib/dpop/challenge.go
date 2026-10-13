package dpop

import (
	"errors"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// Header and metadata names. HTTP header names are case-insensitive; gRPC
// metadata keys are lower case.
const (
	// AuthScheme is the Authorization scheme of a DPoP-bound token.
	AuthScheme = "DPoP"
	// HeaderProof carries the proof.
	HeaderProof = "DPoP"
	// HeaderNonce carries a server nonce on a response.
	HeaderNonce = "DPoP-Nonce"
	// HeaderWWWAuthenticate carries the challenge on a refusal.
	HeaderWWWAuthenticate = "WWW-Authenticate"

	// MetadataAuthorization is the gRPC request key of the token.
	MetadataAuthorization = "authorization"
	// MetadataProof is the gRPC request key of the proof.
	MetadataProof = "dpop"
	// MetadataNonce is the gRPC response key (header or trailer) of a nonce.
	MetadataNonce = "dpop-nonce"
	// MetadataWWWAuthenticate is the gRPC trailer key of the challenge.
	MetadataWWWAuthenticate = "www-authenticate"
)

// Challenge error codes (RFC 9449 §7.1, §8; RFC 6750 §3.1).
const (
	ChallengeUseNonce     = "use_dpop_nonce"
	ChallengeInvalidProof = "invalid_dpop_proof"
	ChallengeInvalidToken = "invalid_token"
)

// algsParam advertises the accepted algorithms in every challenge.
const algsParam = `algs="ES256 EdDSA"`

// AccessToken extracts the token from an `Authorization: DPoP <token>`
// value. The scheme is matched case-insensitively; any other scheme, or an
// empty token, gives ("", false).
func AccessToken(authorization string) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, AuthScheme) {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// WWWAuthenticate is the challenge for a refusal from [Verifier.Verify]:
// `use_dpop_nonce` for [ErrUseNonce], `invalid_token` for a token that does
// not fit its proof ([ErrATHMismatch], [ErrBindingMismatch]), a bare
// challenge for [ErrMissingProof], and `invalid_dpop_proof` for every other
// refusal.
func WWWAuthenticate(err error) string {
	code := ""
	switch {
	case errors.Is(err, ErrMissingProof):
	case errors.Is(err, ErrUseNonce):
		code = ChallengeUseNonce
	case errors.Is(err, ErrATHMismatch), errors.Is(err, ErrBindingMismatch):
		code = ChallengeInvalidToken
	default:
		code = ChallengeInvalidProof
	}
	if code == "" {
		return AuthScheme + " " + algsParam
	}
	return AuthScheme + ` error="` + code + `", ` + algsParam
}

// HTTPStatus is the status answering an error from [Verifier.Verify]: 503
// for [ErrUnavailable] (the replay store is full or failing; retry), 401 for
// every refusal of the proof, 500 for anything else.
func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	case isRefusal(err):
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

// GRPCCode is [HTTPStatus] for gRPC: Unavailable, Unauthenticated or
// Internal.
func GRPCCode(err error) codes.Code {
	switch HTTPStatus(err) {
	case http.StatusOK:
		return codes.OK
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	}
	return codes.Internal
}

func isRefusal(err error) bool {
	for _, r := range []error{ErrMissingProof, ErrMalformed, ErrBadAlg, ErrBadType, ErrBadKey, ErrBadSignature,
		ErrUseNonce, ErrStaleProof, ErrReplay, ErrHTMMismatch, ErrHTUMismatch, ErrATHMismatch, ErrBindingMismatch} {
		if errors.Is(err, r) {
			return true
		}
	}
	return false
}

// SetChallenge sets the response headers answering err. For a refusal (a
// 401): WWW-Authenticate, DPoP-Nonce when err is a [*NonceError], and
// Cache-Control: no-store. For [ErrUnavailable] (a 503): Retry-After: 1 and
// no challenge, since the proof itself was fine. The caller writes the
// status ([HTTPStatus]) and body.
func SetChallenge(h http.Header, err error) {
	h.Set("Cache-Control", "no-store")
	if errors.Is(err, ErrUnavailable) {
		h.Set("Retry-After", "1")
		return
	}
	h.Set(HeaderWWWAuthenticate, WWWAuthenticate(err))
	var ne *NonceError
	if errors.As(err, &ne) {
		h.Set(HeaderNonce, ne.Nonce)
	}
}

// ChallengeMetadata is the gRPC trailer answering err: the challenge under
// `www-authenticate` and, for a [*NonceError], the nonce under `dpop-nonce`.
// The caller returns codes.Unauthenticated with it.
func ChallengeMetadata(err error) metadata.MD {
	md := metadata.Pairs(MetadataWWWAuthenticate, WWWAuthenticate(err))
	var ne *NonceError
	if errors.As(err, &ne) {
		md.Set(MetadataNonce, ne.Nonce)
	}
	return md
}

// IsUseNonceChallenge reports whether any of the WWW-Authenticate values
// holds a DPoP challenge with error="use_dpop_nonce".
func IsUseNonceChallenge(values []string) bool {
	for _, v := range values {
		l := strings.ToLower(v)
		i := strings.Index(l, "dpop ")
		if i < 0 || (i > 0 && !strings.HasSuffix(strings.TrimRight(l[:i], " "), ",")) {
			continue
		}
		rest := strings.ReplaceAll(l[i:], " ", "")
		if strings.Contains(rest, `error="`+ChallengeUseNonce+`"`) || strings.Contains(rest, "error="+ChallengeUseNonce) {
			return true
		}
	}
	return false
}
