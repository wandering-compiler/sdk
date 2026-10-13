package dpop

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

// NonceSource issues and checks server nonces. A nonce is bound to the key
// thumbprint it was issued for.
type NonceSource interface {
	// Issue returns a nonce for the key with this thumbprint.
	Issue(thumbprint string) (string, error)
	// Valid reports whether nonce is current for this thumbprint.
	Valid(nonce, thumbprint string) bool
	// Lifetime is the longest a nonce stays valid after it is issued. The
	// verifier keeps a jti at least this long.
	Lifetime() time.Duration
}

// DefaultNonceBucket is the default time bucket of [HMACNonces]: a nonce is
// valid for one to two buckets.
const DefaultNonceBucket = 5 * time.Minute

// MinNonceKeyBytes is the shortest key [NewHMACNonces] accepts.
const MinNonceKeyBytes = 32

// nonceMACBytes is the length of the HMAC-SHA-256 tag a nonce carries.
const nonceMACBytes = sha256.Size

// HMACNonces is the stateless [NonceSource]: a nonce is the bucket number and
// HMAC-SHA-256(key, label ‖ bucket ‖ thumbprint). Every replica configured
// with the same key and bucket issues and accepts the same nonces, with no
// shared state. Nonces of the current and the previous bucket are valid.
type HMACNonces struct {
	key    []byte
	bucket time.Duration
	now    func() time.Time
}

// HMACNoncesOption configures [NewHMACNonces].
type HMACNoncesOption func(*HMACNonces)

// WithNonceBucket sets the bucket length; d <= 0 keeps [DefaultNonceBucket].
// A bucket shorter than a second is rounded up to one second.
func WithNonceBucket(d time.Duration) HMACNoncesOption {
	return func(n *HMACNonces) {
		if d > 0 {
			n.bucket = max(d, time.Second)
		}
	}
}

// WithNonceClock sets the clock; nil keeps time.Now.
func WithNonceClock(now func() time.Time) HMACNoncesOption {
	return func(n *HMACNonces) {
		if now != nil {
			n.now = now
		}
	}
}

// NewHMACNonces returns a nonce source keyed with key, which must be at least
// [MinNonceKeyBytes] long and the same on every replica. The key is copied.
func NewHMACNonces(key []byte, opts ...HMACNoncesOption) (*HMACNonces, error) {
	if len(key) < MinNonceKeyBytes {
		return nil, errors.New("dpop: nonce key shorter than 32 bytes")
	}
	n := &HMACNonces{key: append([]byte(nil), key...), bucket: DefaultNonceBucket, now: time.Now}
	for _, o := range opts {
		o(n)
	}
	return n, nil
}

func (n *HMACNonces) current() uint64 {
	return uint64(n.now().UnixNano() / int64(n.bucket))
}

func (n *HMACNonces) mac(bucket uint64, thumbprint string) []byte {
	m := hmac.New(sha256.New, n.key)
	m.Write([]byte("w17-dpop-nonce\x00"))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], bucket)
	m.Write(b[:])
	m.Write([]byte(thumbprint))
	return m.Sum(nil)
}

// Issue returns the nonce of the current bucket for thumbprint.
func (n *HMACNonces) Issue(thumbprint string) (string, error) {
	bucket := n.current()
	out := make([]byte, 8, 8+nonceMACBytes)
	binary.BigEndian.PutUint64(out, bucket)
	out = append(out, n.mac(bucket, thumbprint)...)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// Valid reports whether nonce was issued for thumbprint in the current or the
// previous bucket.
func (n *HMACNonces) Valid(nonce, thumbprint string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(raw) != 8+nonceMACBytes {
		return false
	}
	bucket := binary.BigEndian.Uint64(raw[:8])
	cur := n.current()
	if bucket != cur && bucket+1 != cur {
		return false
	}
	return hmac.Equal(raw[8:], n.mac(bucket, thumbprint))
}

// Lifetime is two buckets: a nonce issued at the start of a bucket stays
// valid through the next one.
func (n *HMACNonces) Lifetime() time.Duration { return 2 * n.bucket }
