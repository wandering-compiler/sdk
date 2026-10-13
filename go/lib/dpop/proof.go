package dpop

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/cryptosigner"
)

// ProofType is the `typ` header of every DPoP proof.
const ProofType = "dpop+jwt"

// Claims is the payload of a DPoP proof.
type Claims struct {
	JTI   string `json:"jti"`
	HTM   string `json:"htm"`
	HTU   string `json:"htu"`
	IAT   int64  `json:"iat"`
	ATH   string `json:"ath,omitempty"`
	Nonce string `json:"nonce,omitempty"`
}

// NewProof builds and signs a proof for one request: htm is the HTTP method
// (POST for gRPC), htu the target URI (its query and fragment are dropped),
// accessToken the token the request carries ("" on a request without one,
// which omits `ath`), nonce the last nonce the server sent ("" for none). Each
// call draws a fresh random jti.
func NewProof(s Signer, htm, htu, accessToken, nonce string) (string, error) {
	return newProof(s, htm, htu, accessToken, nonce, time.Now())
}

func newProof(s Signer, htm, htu, accessToken, nonce string, now time.Time) (string, error) {
	if htm == "" {
		return "", errors.New("dpop: proof needs an HTTP method")
	}
	target, err := NormalizeHTU(htu)
	if err != nil {
		return "", err
	}
	jti, err := newJTI()
	if err != nil {
		return "", err
	}
	c := Claims{JTI: jti, HTM: htm, HTU: target, IAT: now.Unix(), Nonce: nonce}
	if accessToken != "" {
		c.ATH = AccessTokenHash(accessToken)
	}
	return SignClaims(s, c)
}

// SignClaims signs arbitrary proof claims. [NewProof] is the normal entry
// point; this one exists for callers that set iat or jti themselves.
func SignClaims(s Signer, c Claims) (string, error) {
	alg, err := algFor(s.Public())
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("dpop: encode claims: %w", err)
	}
	opts := (&jose.SignerOptions{EmbedJWK: true}).WithType(ProofType)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: cryptosigner.Opaque(s)}, opts)
	if err != nil {
		return "", fmt.Errorf("dpop: signer: %w", err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("dpop: sign proof: %w", err)
	}
	return jws.CompactSerialize()
}

// newJTI returns 128 random bits, base64url.
func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("dpop: jti: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// NormalizeHTU puts a target URI in the form proofs carry and the verifier
// compares: an absolute http or https URI, lower-case scheme and host, no
// default port, no user info, an empty path written as "/", and no query or
// fragment (RFC 9449 §4.2, compared per §4.3 with the syntax- and
// scheme-based normalisation of RFC 3986 §6.2.2 and §6.2.3).
func NormalizeHTU(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("dpop: htu: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("dpop: htu %q: want an absolute http or https URI", raw)
	}
	if u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return "", fmt.Errorf("dpop: htu %q: want scheme://host[:port]/path", raw)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return scheme + "://" + host + path, nil
}
