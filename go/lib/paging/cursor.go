package paging

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
	"google.golang.org/protobuf/proto"
)

// CursorKeyFromEnv resolves the HMAC key the gateway signs pagination
// cursors with (see EncodeCursor / DecodeCursor). It reads
// `<prefix>_CURSOR_SECRET`:
//
//   - SET → key = sha256(secret). Stable across restarts and replicas
//     — the production posture; mint on one replica, decode on another.
//   - UNSET → a random per-boot key + a one-line warning via logf.
//     Cursors still work within a single gateway process (localhost /
//     single replica) but won't validate after a restart or across a
//     second replica. This is fail-closed: a stale cursor decodes to
//     ErrCursorMalformed and the client refetches page one — never an
//     insecure accept. Set the env var in any multi-replica or
//     rolling-deploy production gateway.
//
// getenv is injected (os.Getenv in production) so the resolution is
// unit-testable; logf may be nil.
func CursorKeyFromEnv(prefix string, getenv func(string) string, logf func(string, ...any)) []byte {
	if secret := getenv(prefix + "_CURSOR_SECRET"); secret != "" {
		sum := sha256.Sum256([]byte(secret))
		return sum[:]
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// crypto/rand failure is catastrophic and unrecoverable — fail
		// loud at boot rather than sign cursors with a predictable key.
		panic("paging: crypto/rand failed generating cursor key: " + err.Error())
	}
	if logf != nil {
		logf("paging: %s_CURSOR_SECRET unset — signing pagination cursors with a random per-boot key; cursors won't survive a restart or validate across replicas. Set %s_CURSOR_SECRET to a stable shared secret in production.", prefix, prefix)
	}
	return key
}

// cursorMAC computes the HMAC-SHA256 tag binding the cursor's
// serialized PageCursor bytes to the gateway's per-deployment key.
// The tag is appended to the token so a client cannot forge a cursor
// (which DecodeCursor unmarshals OVER the whole request) to smuggle a
// request the server never minted — see DecodeCursor (Q34-gw-1).
func cursorMAC(pcBytes, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(pcBytes)
	return mac.Sum(nil)
}

// ErrCursorExpired indicates the cursor was emitted by a
// server build whose request schema (or KeysetValue shape)
// does not match the current build's `schema_version`.
//
// Gateway handlers map this to gRPC InvalidArgument with a
// reason code clients can recognize.
var ErrCursorExpired = errors.New("paging: cursor_expired")

// ErrCursorMalformed indicates the cursor bytes do not decode
// as a base64-url-safe payload or do not parse as a PageCursor
// proto. Distinct from ErrCursorExpired because the
// remediation is the same (refetch page one) but the cause is
// different — corrupted transport vs. server upgrade.
var ErrCursorMalformed = errors.New("paging: cursor_malformed")

// EncodeCursor builds an opaque cursor token from a storage
// request + the boundary values captured from one edge of the
// returned page + the cached total + direction + the active
// sort selector + the current build's request-schema version.
//
// The request proto is serialized opaquely; callers do not
// need to know its concrete type. Boundaries are ordered
// positionally — index i corresponds to the i-th column of
// the ORDER BY the ACTIVE sort variant runs under (the
// method's DQL ORDER BY at `sortBy == 0`; the sorted column
// followed by that same ORDER BY otherwise). `sortBy` rides
// along so the resume reads them back under the identical
// variant — see [w17pb.PageCursor].SortBy. Unsorted callers
// (every non-admin paged endpoint) pass 0.
//
// Returns the empty string (nil error) when `boundaries` is
// empty, i.e. the cursor would be meaningless: a forward cursor
// on a page that did not fill (no further rows), or a backward
// cursor on the first page (no prior rows). Callers signal "no
// more pages in this direction" by passing an empty `boundaries`
// slice and get back "" — the generated gateway handlers already
// treat a "" token as "omit the next/prev link".
// key is the gateway's per-deployment HMAC key (see
// CursorKeyFromEnv). The returned token is
// `base64url(PageCursor) "." base64url(HMAC)` so DecodeCursor can
// reject any client-forged token before it overwrites the request.
func EncodeCursor(
	req proto.Message,
	boundaries []*w17pb.KeysetValue,
	total uint64,
	direction w17pb.Direction,
	schemaVersion uint32,
	sortBy uint32,
	key []byte,
) (string, error) {
	// No boundary values → there is no page edge to encode; emit the
	// empty token the godoc promises rather than a meaningless cursor.
	if len(boundaries) == 0 {
		return "", nil
	}

	reqBytes, err := proto.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("paging: marshal request: %w", err)
	}

	pc := &w17pb.PageCursor{
		Request:       reqBytes,
		Boundaries:    boundaries,
		Total:         total,
		Direction:     direction,
		SchemaVersion: schemaVersion,
		SortBy:        sortBy,
	}

	pcBytes, err := proto.Marshal(pc)
	if err != nil {
		return "", fmt.Errorf("paging: marshal cursor: %w", err)
	}

	payload := base64.RawURLEncoding.EncodeToString(pcBytes)
	tag := base64.RawURLEncoding.EncodeToString(cursorMAC(pcBytes, key))
	return payload + "." + tag, nil
}

// DecodeCursor reverses EncodeCursor. The caller supplies a
// concrete `req` proto.Message of the storage method's request
// type; DecodeCursor unmarshals the cursor's request bytes
// into it. Boundary values, total, direction, and the active
// sort selector are returned as separate values.
//
// `sortBy` is authoritative over any `?sort_by=` the client
// re-sent: the boundaries in this cursor were captured under
// that variant's ORDER BY, so resuming under a different one
// would seek the wrong column. A client changing the sort
// drops the cursor (the admin SPA resets it on sort change)
// and starts a fresh session at page one.
//
// Returns ErrCursorExpired if the cursor's `schema_version`
// does not match `expectedSchemaVersion`. Returns
// ErrCursorMalformed if the cursor is not valid base64-url or
// not a parseable PageCursor proto.
// key is the gateway's per-deployment HMAC key (see
// CursorKeyFromEnv). The token's appended HMAC tag is verified
// against it in constant time BEFORE the request bytes are
// unmarshalled — a tampered or forged cursor returns
// ErrCursorMalformed and never reaches `req`. This is what stops a
// client from crafting a cursor that overwrites the request with
// fields the server never validated/minted (Q34-gw-1).
func DecodeCursor(
	token string,
	req proto.Message,
	expectedSchemaVersion uint32,
	key []byte,
) (boundaries []*w17pb.KeysetValue, total uint64, direction w17pb.Direction, sortBy uint32, err error) {
	if token == "" {
		err = ErrCursorMalformed
		return
	}

	// Split `payload "." tag`. base64url (RawURLEncoding) never emits
	// a '.', so the last dot is the unambiguous separator; a token
	// with no dot is the legacy unsigned shape and is rejected.
	dot := strings.LastIndexByte(token, '.')
	if dot < 0 {
		err = ErrCursorMalformed
		return
	}
	payload, tag := token[:dot], token[dot+1:]

	pcBytes, decErr := base64.RawURLEncoding.DecodeString(payload)
	if decErr != nil {
		err = ErrCursorMalformed
		return
	}
	gotMAC, macErr := base64.RawURLEncoding.DecodeString(tag)
	if macErr != nil {
		err = ErrCursorMalformed
		return
	}
	if !hmac.Equal(gotMAC, cursorMAC(pcBytes, key)) {
		err = ErrCursorMalformed
		return
	}

	pc := &w17pb.PageCursor{}
	if unmarshalErr := proto.Unmarshal(pcBytes, pc); unmarshalErr != nil {
		err = ErrCursorMalformed
		return
	}

	if pc.GetSchemaVersion() != expectedSchemaVersion {
		err = ErrCursorExpired
		return
	}

	if unmarshalErr := proto.Unmarshal(pc.GetRequest(), req); unmarshalErr != nil {
		err = ErrCursorMalformed
		return
	}

	// Every boundary must carry a set oneof. A crafted cursor whose
	// schema_version matches but whose KeysetValue oneof is empty
	// (valid proto wire) would otherwise reach ScalarOf and panic —
	// reject it here as malformed so it surfaces as a clean client
	// error, not a recovered 500.
	for _, b := range pc.GetBoundaries() {
		if b.GetValue() == nil {
			err = ErrCursorMalformed
			return
		}
	}

	boundaries = pc.GetBoundaries()
	total = pc.GetTotal()
	direction = pc.GetDirection()
	sortBy = pc.GetSortBy()
	return
}
