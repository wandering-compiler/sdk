package dpop

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Refusal causes. Every error [Verifier.Verify] returns for a bad request
// matches exactly one of these with errors.Is; any other error (a failing
// [JTIStore], for instance) is a server fault, not a verdict on the proof.
var (
	// ErrMissingProof: the request has no DPoP header.
	ErrMissingProof = errors.New("dpop: missing proof")
	// ErrMalformed: more than one DPoP header, an oversized proof, a value
	// that is not a compact JWS, or claims that are missing or mistyped.
	ErrMalformed = errors.New("dpop: malformed proof")
	// ErrBadAlg: alg is not ES256 or EdDSA.
	ErrBadAlg = errors.New("dpop: proof algorithm not allowed")
	// ErrBadType: typ is not dpop+jwt.
	ErrBadType = errors.New("dpop: proof typ is not dpop+jwt")
	// ErrBadKey: jwk is missing, carries private members, or does not fit
	// alg.
	ErrBadKey = errors.New("dpop: proof key not acceptable")
	// ErrBadSignature: the signature does not verify with the embedded key.
	ErrBadSignature = errors.New("dpop: proof signature invalid")
	// ErrUseNonce: the nonce is missing or no longer valid. The error is a
	// [*NonceError] holding the fresh nonce to send back.
	ErrUseNonce = errors.New("dpop: proof needs a fresh nonce")
	// ErrStaleProof: with nonces off, iat is outside the accepted window.
	ErrStaleProof = errors.New("dpop: proof iat outside the accepted window")
	// ErrReplay: this jti was already used by a different proof, or the same
	// proof's reuse window has closed.
	ErrReplay = errors.New("dpop: proof replayed")
	// ErrHTMMismatch: htm is not the request method.
	ErrHTMMismatch = errors.New("dpop: proof htm does not match the request")
	// ErrHTUMismatch: htu is not the configured origin plus the request path.
	ErrHTUMismatch = errors.New("dpop: proof htu does not match the request")
	// ErrATHMismatch: ath is not the hash of the presented access token, is
	// missing while a token is presented, or is present while none is.
	ErrATHMismatch = errors.New("dpop: proof ath does not match the access token")
	// ErrBindingMismatch: the proof's key is not the key the credential is
	// bound to.
	ErrBindingMismatch = errors.New("dpop: proof key does not match the credential binding")
)

// ErrUnavailable is not a verdict on the proof: the replay store could not
// record it — it is full ([ErrJTIStoreFull], also matched) or failing. The
// request is refused, never accepted unrecorded (the one opt-in exception is
// an unbound proof meeting a full mint store, [Config.MintFull]), and the
// caller answers
// 503 Service Unavailable with Retry-After (gRPC: codes.Unavailable), which
// [HTTPStatus], [GRPCCode] and [SetChallenge] do. Entries expire within a
// nonce lifetime, so the condition clears by itself.
var ErrUnavailable = errors.New("dpop: replay store unavailable; retry later")

// NonceError is the [ErrUseNonce] refusal. The caller answers it with
// `use_dpop_nonce` and Nonce ([SetChallenge], [ChallengeMetadata]).
type NonceError struct {
	// Nonce is a fresh nonce for the proof's key.
	Nonce string
	// Missing is true when the proof carried no nonce at all.
	Missing bool
}

func (e *NonceError) Error() string {
	if e.Missing {
		return ErrUseNonce.Error() + ": nonce missing"
	}
	return ErrUseNonce.Error() + ": nonce invalid or expired"
}

// Is makes errors.Is(err, ErrUseNonce) hold.
func (e *NonceError) Is(target error) bool { return target == ErrUseNonce }

// Limits and defaults of the verifier.
const (
	// MaxProofBytes bounds a DPoP header value; a longer one is malformed
	// before any parsing.
	MaxProofBytes = 8 << 10
	// MaxJTIBytes bounds the jti claim.
	MaxJTIBytes = 256
	// DefaultReuseWindow is how long the same proof may be presented again.
	DefaultReuseWindow = 30 * time.Second
	// MaxReuseWindow is the longest reuse window [NewVerifier] accepts.
	MaxReuseWindow = 300 * time.Second
	// DefaultIATWindow is the accepted |now - iat| when nonces are off.
	DefaultIATWindow = 60 * time.Second
	// DefaultSignatureCacheEntries bounds the reuse cache.
	DefaultSignatureCacheEntries = 10_000
	// DefaultMaxMintJTIs bounds the default store of unbound (mint) proofs.
	DefaultMaxMintJTIs = 50_000
)

// AllowedAlgs are the proof algorithms the verifier accepts.
var AllowedAlgs = []jose.SignatureAlgorithm{jose.ES256, jose.EdDSA}

// Config configures a [Verifier].
type Config struct {
	// Origin is the public origin clients address, scheme://host[:port]
	// (e.g. https://api.example.com). Required. htu is compared against
	// Origin + Request.Path, never against a Host header.
	Origin string
	// Nonces issues and checks server nonces. nil turns nonces off: then
	// iat must lie within IATWindow of the verifier's clock.
	Nonces NonceSource
	// JTIs records accepted bound proofs (Request.Binding set). nil means a
	// [MemoryJTIStore] with the default limits, correct only for a single
	// replica.
	JTIs JTIStore
	// MintJTIs records accepted unbound proofs (a mint endpoint: no token,
	// no binding). It is kept apart from JTIs because anyone can sign an
	// unbound proof with a throwaway key: a mint flood can fill MintJTIs and
	// stall minting, but never the store authenticated requests need. nil
	// means a [MemoryJTIStore] of [DefaultMaxMintJTIs] when JTIs is nil too;
	// setting JTIs without MintJTIs, or both to the same store, is an error.
	MintJTIs JTIStore
	// MintFull is what an unbound (mint) proof meets when MintJTIs is full
	// ([ErrJTIStoreFull]). The default, [MintFullRefuse], refuses it with
	// [ErrUnavailable] like any other unrecordable proof.
	// [MintFullAcceptUnrecorded] accepts it without recording its jti and
	// reports that in [Result.MintUnrecorded], so the caller can log it:
	// replay protection for minting becomes best-effort, which a deployment
	// may prefer to a mint outage, since a replayed unbound proof only yields
	// a token bound to a key the replayer does not hold. It never applies to
	// bound proofs, nor to a store that fails rather than being full.
	MintFull MintFullPolicy
	// ReuseWindow is how long a proof may be presented again byte for byte.
	// 0 means [DefaultReuseWindow]; a negative value turns reuse off (every
	// repeat is a replay); more than [MaxReuseWindow] is an error.
	ReuseWindow time.Duration
	// IATWindow is the accepted clock difference when nonces are off. 0
	// means [DefaultIATWindow]; negative is an error. Ignored with nonces.
	IATWindow time.Duration
	// SignatureCacheEntries bounds the reuse cache; 0 means
	// [DefaultSignatureCacheEntries]. A full cache stops caching; it never
	// refuses a request.
	SignatureCacheEntries int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// MintFullPolicy is [Config.MintFull].
type MintFullPolicy int

const (
	// MintFullRefuse refuses an unbound proof the full mint store cannot
	// record ([ErrUnavailable] wrapping [ErrJTIStoreFull]). The default.
	MintFullRefuse MintFullPolicy = iota
	// MintFullAcceptUnrecorded accepts an unbound proof the full mint store
	// cannot record, without recording it, and sets [Result.MintUnrecorded].
	MintFullAcceptUnrecorded
)

// Result is what [Verifier.VerifyResult] established for one request.
type Result struct {
	// Thumbprint is the RFC 7638 thumbprint of the key that signed the
	// proof.
	Thumbprint string
	// MintUnrecorded is true when an unbound proof was accepted without its
	// jti being recorded, because the mint store was full and
	// [Config.MintFull] is [MintFullAcceptUnrecorded]. The proof could be
	// presented again until the store has room; log it.
	MintUnrecorded bool
}

// Request is what the verifier needs from one request.
type Request struct {
	// Method is the HTTP method; POST for gRPC.
	Method string
	// Path is the request path as sent (r.URL.EscapedPath()), or the full
	// gRPC method name ("/package.Service/Method"). It must start with "/";
	// a query is ignored.
	Path string
	// Proofs are all values of the DPoP header (gRPC: the `dpop` metadata).
	// Exactly one is required.
	Proofs []string
	// AccessToken is the token from `Authorization: DPoP <token>`, or ""
	// when the request carries none (a mint endpoint); then ath must be
	// absent.
	AccessToken string
	// Binding is the thumbprint the presented credential is bound to, or ""
	// on a mint endpoint, where no binding exists yet. A request with an
	// AccessToken and no Binding is refused (fail closed): a caller that
	// verifies a token must say which key it is bound to.
	Binding string
}

// Verifier checks DPoP proofs. It is safe for concurrent use.
type Verifier struct {
	origin   string
	nonces   NonceSource
	jtis     JTIStore
	mintJTIs JTIStore
	mintFull MintFullPolicy
	reuse    time.Duration
	iat      time.Duration
	jtiTTL   time.Duration
	now      func() time.Time
	cacheMax int

	mu    sync.Mutex
	cache map[string]time.Time // proof hash → end of its reuse window
}

// NewVerifier validates cfg and returns a verifier.
func NewVerifier(cfg Config) (*Verifier, error) {
	origin, err := NormalizeHTU(cfg.Origin)
	if err != nil || !strings.HasSuffix(origin, "/") || strings.Count(origin, "/") != 3 {
		return nil, fmt.Errorf("dpop: Origin %q: want scheme://host[:port] with no path", cfg.Origin)
	}
	v := &Verifier{
		origin:   strings.TrimSuffix(origin, "/"),
		nonces:   cfg.Nonces,
		jtis:     cfg.JTIs,
		mintJTIs: cfg.MintJTIs,
		mintFull: cfg.MintFull,
		reuse:    cfg.ReuseWindow,
		iat:      cfg.IATWindow,
		now:      cfg.Now,
		cacheMax: cfg.SignatureCacheEntries,
		cache:    make(map[string]time.Time),
	}
	switch {
	case v.reuse == 0:
		v.reuse = DefaultReuseWindow
	case v.reuse < 0:
		v.reuse = 0
	case v.reuse > MaxReuseWindow:
		return nil, fmt.Errorf("dpop: ReuseWindow %s exceeds %s", cfg.ReuseWindow, MaxReuseWindow)
	}
	switch {
	case v.iat == 0:
		v.iat = DefaultIATWindow
	case v.iat < 0:
		return nil, fmt.Errorf("dpop: IATWindow %s is negative", cfg.IATWindow)
	}
	switch cfg.MintFull {
	case MintFullRefuse, MintFullAcceptUnrecorded:
	default:
		return nil, fmt.Errorf("dpop: unknown MintFull policy %d", cfg.MintFull)
	}
	switch {
	case cfg.JTIs != nil && cfg.MintJTIs == nil:
		return nil, errors.New("dpop: JTIs set without MintJTIs: unbound proofs need their own store")
	case cfg.JTIs != nil && cfg.MintJTIs == cfg.JTIs:
		return nil, errors.New("dpop: MintJTIs is the same store as JTIs: a mint flood would fill both")
	}
	if v.jtis == nil {
		v.jtis = NewMemoryJTIStore(0)
	}
	if v.mintJTIs == nil {
		v.mintJTIs = NewMemoryJTIStore(DefaultMaxMintJTIs)
	}
	if v.now == nil {
		v.now = time.Now
	}
	if v.cacheMax <= 0 {
		v.cacheMax = DefaultSignatureCacheEntries
	}
	// A jti must be remembered for as long as its proof could be accepted.
	if v.nonces != nil {
		v.jtiTTL = v.nonces.Lifetime()
	} else {
		v.jtiTTL = 2 * v.iat
	}
	v.jtiTTL = max(v.jtiTTL, v.reuse)
	return v, nil
}

// Origin returns the normalised configured origin.
func (v *Verifier) Origin() string { return v.origin }

// Nonce returns a fresh nonce for a key, for a response that rotates the
// nonce (DPoP-Nonce on a success) or a mint endpoint that hands one out
// first. It fails when nonces are off.
func (v *Verifier) Nonce(thumbprint string) (string, error) {
	if v.nonces == nil {
		return "", errors.New("dpop: nonces are off")
	}
	return v.nonces.Issue(thumbprint)
}

// claimsIn mirrors Claims with pointers, to tell absent from empty. It is
// filled member by member from a strictly decoded object (exact key case, no
// duplicates), never by encoding/json's case-insensitive field matching.
type claimsIn struct {
	JTI, HTM, HTU, ATH, Nonce *string
	IAT                       *int64
}

// privateJWKMembers are the JWK members that only a private or symmetric key
// has (RFC 7518 §6.2.2, §6.3.2, §6.4.1; RFC 8037 §2).
var privateJWKMembers = []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"}

// Verify checks one request's proof and returns the RFC 7638 thumbprint of
// the key that signed it. See the package documentation for the order of the
// checks and [ErrMissingProof] and its siblings for the refusals.
//
// With [Config.MintFull] set to [MintFullAcceptUnrecorded], Verify may accept
// an unbound proof it did not record; [Verifier.VerifyResult] says when.
func (v *Verifier) Verify(ctx context.Context, req Request) (string, error) {
	r, err := v.VerifyResult(ctx, req)
	return r.Thumbprint, err
}

// VerifyResult is [Verifier.Verify] returning the whole [Result].
func (v *Verifier) VerifyResult(ctx context.Context, req Request) (Result, error) {
	thumb, unrecorded, err := v.verify(ctx, req)
	if err != nil {
		return Result{}, err
	}
	return Result{Thumbprint: thumb, MintUnrecorded: unrecorded}, nil
}

func (v *Verifier) verify(ctx context.Context, req Request) (string, bool, error) {
	now := v.now()

	// 1. Header.
	switch {
	case len(req.Proofs) == 0 || (len(req.Proofs) == 1 && req.Proofs[0] == ""):
		return "", false, ErrMissingProof
	case len(req.Proofs) > 1:
		return "", false, fmt.Errorf("%w: %d DPoP headers", ErrMalformed, len(req.Proofs))
	}
	proof := req.Proofs[0]
	if len(proof) > MaxProofBytes {
		return "", false, fmt.Errorf("%w: proof longer than %d bytes", ErrMalformed, MaxProofBytes)
	}
	alg, err := checkHeader(proof)
	if err != nil {
		return "", false, err
	}
	jws, err := jose.ParseSignedCompact(proof, AllowedAlgs)
	if err != nil {
		var unexpected *jose.ErrUnexpectedSignatureAlgorithm
		if errors.As(err, &unexpected) {
			return "", false, fmt.Errorf("%w: %w", ErrBadAlg, err)
		}
		return "", false, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if len(jws.Signatures) != 1 {
		return "", false, fmt.Errorf("%w: want one signature", ErrMalformed)
	}
	jwk := jws.Signatures[0].Header.JSONWebKey
	if jwk == nil {
		return "", false, fmt.Errorf("%w: no jwk", ErrBadKey)
	}
	if !keyFitsAlg(jwk.Key, alg) {
		return "", false, fmt.Errorf("%w: %T does not fit %s", ErrBadKey, jwk.Key, alg)
	}
	thumb, err := Thumbprint(jwk.Key)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrBadKey, err)
	}
	sum := sha256.Sum256([]byte(proof))
	hash := hex.EncodeToString(sum[:])

	// 2. Signature: a cached verdict for these exact bytes, or the check.
	var payload []byte
	verified := v.cached(hash, now)
	if verified {
		payload = jws.UnsafePayloadWithoutVerification()
	} else {
		payload, err = jws.Verify(jwk)
		if err != nil {
			return "", false, fmt.Errorf("%w: %w", ErrBadSignature, err)
		}
	}
	c, err := parseClaims(payload)
	if err != nil {
		return "", false, err
	}

	// 3. Nonce, or iat when nonces are off.
	if v.nonces != nil {
		if c.Nonce == nil || *c.Nonce == "" || !v.nonces.Valid(*c.Nonce, thumb) {
			fresh, err := v.nonces.Issue(thumb)
			if err != nil {
				return "", false, fmt.Errorf("dpop: issue nonce: %w", err)
			}
			return "", false, &NonceError{Nonce: fresh, Missing: c.Nonce == nil || *c.Nonce == ""}
		}
	} else {
		iat := time.Unix(*c.IAT, 0)
		if d := now.Sub(iat); d > v.iat || d < -v.iat {
			return "", false, fmt.Errorf("%w: iat %s from now", ErrStaleProof, -d)
		}
	}

	// 4. htm and htu.
	if !strings.HasPrefix(req.Path, "/") {
		return "", false, fmt.Errorf("%w: request path %q does not start with /", ErrHTUMismatch, req.Path)
	}
	if *c.HTM != req.Method {
		return "", false, fmt.Errorf("%w: htm %q, request %q", ErrHTMMismatch, *c.HTM, req.Method)
	}
	path, _, _ := strings.Cut(req.Path, "?")
	want, err := NormalizeHTU(v.origin + path)
	if err != nil {
		return "", false, fmt.Errorf("%w: request path %q: %w", ErrHTUMismatch, req.Path, err)
	}
	if gotHTU, err := NormalizeHTU(*c.HTU); err != nil || gotHTU != want {
		return "", false, fmt.Errorf("%w: htu %q, want %q", ErrHTUMismatch, *c.HTU, want)
	}

	// 5. ath.
	if req.AccessToken != "" {
		if c.ATH == nil || subtle.ConstantTimeCompare([]byte(*c.ATH), []byte(AccessTokenHash(req.AccessToken))) != 1 {
			return "", false, ErrATHMismatch
		}
	} else if c.ATH != nil {
		return "", false, fmt.Errorf("%w: ath present without an access token", ErrATHMismatch)
	}

	// 6. Binding.
	switch {
	case req.Binding != "":
		if subtle.ConstantTimeCompare([]byte(req.Binding), []byte(thumb)) != 1 {
			return "", false, ErrBindingMismatch
		}
	case req.AccessToken != "":
		return "", false, fmt.Errorf("%w: access token presented with no binding to check", ErrBindingMismatch)
	}

	// 7. jti, last: only a proof that passed every other check is recorded,
	// so refused proofs never take room in the store. The insert is atomic;
	// a jti already there is a replay unless it is this very proof inside
	// its reuse window.
	store, mint := v.jtis, req.Binding == ""
	if mint {
		store = v.mintJTIs // unbound: kept apart, see Config.MintJTIs
	}
	rec := JTIRecord{Thumbprint: thumb, ProofHash: hash, FirstSeen: now}
	got, created, err := store.Record(ctx, thumb+"\x00"+*c.JTI, rec, v.jtiTTL)
	unrecorded := false
	switch {
	case err != nil && mint && v.mintFull == MintFullAcceptUnrecorded && errors.Is(err, ErrJTIStoreFull):
		// Best-effort replay protection for minting (Config.MintFull):
		// accepted, not recorded, and reported to the caller.
		unrecorded, created = true, true
	case err != nil:
		return "", false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !created {
		if got.ProofHash != hash || v.reuse <= 0 || now.Sub(got.FirstSeen) >= v.reuse {
			return "", false, ErrReplay
		}
	}

	if !verified {
		v.remember(hash, now)
	}
	return thumb, unrecorded, nil
}

// checkHeader reads the protected header before go-jose parses the JWS, to
// tell the header refusals apart: alg outside the allowlist, a typ other than
// dpop+jwt, and a jwk with private members. The JWS itself is parsed and
// verified by go-jose.
func checkHeader(proof string) (jose.SignatureAlgorithm, error) {
	seg, _, ok := strings.Cut(proof, ".")
	if !ok {
		return "", fmt.Errorf("%w: not a compact JWS", ErrMalformed)
	}
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return "", fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	h, err := strictObject(raw)
	if err != nil {
		return "", fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	algName, err := optString(h, "alg")
	if err != nil {
		return "", fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	alg := jose.SignatureAlgorithm(algName)
	if alg != jose.ES256 && alg != jose.EdDSA {
		return "", fmt.Errorf("%w: %q", ErrBadAlg, algName)
	}
	typ, err := optString(h, "typ")
	if err != nil {
		return "", fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	if !strings.EqualFold(typ, ProofType) {
		return "", fmt.Errorf("%w: %q", ErrBadType, typ)
	}
	rawJWK, ok := h["jwk"]
	if !ok {
		return "", fmt.Errorf("%w: no jwk", ErrBadKey)
	}
	jwk, err := strictObject(rawJWK)
	if err != nil {
		return "", fmt.Errorf("%w: jwk: %w", ErrBadKey, err)
	}
	for member := range jwk {
		for _, m := range privateJWKMembers {
			if strings.EqualFold(member, m) {
				return "", fmt.Errorf("%w: jwk carries private member %q", ErrBadKey, member)
			}
		}
	}
	return alg, nil
}

// strictObject decodes one JSON object with exact, case-sensitive keys. It
// refuses duplicate keys (which encoding/json would resolve silently, last
// one winning) and anything after the object.
func strictObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	out := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		out[key] = val
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return out, nil
}

// optString reads an optional string member; absent gives "", any other JSON
// type an error.
func optString(m map[string]json.RawMessage, key string) (string, error) {
	p, err := optStringPtr(m, key)
	if err != nil || p == nil {
		return "", err
	}
	return *p, nil
}

func optStringPtr(m map[string]json.RawMessage, key string) (*string, error) {
	raw, ok := m[key]
	if !ok {
		return nil, nil
	}
	var s string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &s) != nil {
		return nil, fmt.Errorf("%s is not a string", key)
	}
	return &s, nil
}

func keyFitsAlg(key any, alg jose.SignatureAlgorithm) bool {
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		return alg == jose.ES256 && k.Curve == elliptic.P256()
	case ed25519.PublicKey:
		return alg == jose.EdDSA && len(k) == ed25519.PublicKeySize
	}
	return false
}

func parseClaims(payload []byte) (*claimsIn, error) {
	m, err := strictObject(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: claims: %w", ErrMalformed, err)
	}
	var c claimsIn
	for key, dst := range map[string]**string{"jti": &c.JTI, "htm": &c.HTM, "htu": &c.HTU, "ath": &c.ATH, "nonce": &c.Nonce} {
		if *dst, err = optStringPtr(m, key); err != nil {
			return nil, fmt.Errorf("%w: claims: %w", ErrMalformed, err)
		}
	}
	if raw, ok := m["iat"]; ok {
		var iat int64
		if err := json.Unmarshal(raw, &iat); err != nil {
			return nil, fmt.Errorf("%w: claims: iat is not an integer", ErrMalformed)
		}
		c.IAT = &iat
	}
	switch {
	case c.JTI == nil || *c.JTI == "":
		return nil, fmt.Errorf("%w: no jti", ErrMalformed)
	case len(*c.JTI) > MaxJTIBytes:
		return nil, fmt.Errorf("%w: jti longer than %d bytes", ErrMalformed, MaxJTIBytes)
	case c.HTM == nil || *c.HTM == "":
		return nil, fmt.Errorf("%w: no htm", ErrMalformed)
	case c.HTU == nil || *c.HTU == "":
		return nil, fmt.Errorf("%w: no htu", ErrMalformed)
	case c.IAT == nil:
		return nil, fmt.Errorf("%w: no iat", ErrMalformed)
	}
	return &c, nil
}

// cached reports whether a proof with this hash verified inside its reuse
// window.
func (v *Verifier) cached(hash string, now time.Time) bool {
	if v.reuse <= 0 {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	until, ok := v.cache[hash]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(v.cache, hash)
		return false
	}
	return true
}

// remember caches a verified signature until the end of the reuse window.
func (v *Verifier) remember(hash string, now time.Time) {
	if v.reuse <= 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= v.cacheMax {
		for h, until := range v.cache {
			if !now.Before(until) {
				delete(v.cache, h)
			}
		}
		if len(v.cache) >= v.cacheMax {
			return
		}
	}
	v.cache[hash] = now.Add(v.reuse)
}
