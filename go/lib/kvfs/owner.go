package kvfs

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"

	"google.golang.org/grpc/metadata"
)

// ownerMark separates an object key from the owner record [OwnedObjectKey]
// appends: `<hash>-<nonce>.o<base64url(owner)>`. ObjectKey's own part is
// hex and '-', so the first ".o" in the last path segment is the mark.
const ownerMark = ".o"

// OwnedObjectKey is [ObjectKey] carrying the data scopes of the caller
// that stored the object — `org_id`, an owner `user_id`, … — so a download
// can serve it only to a caller who shares them (restgw.ServeDownloadFor).
// An empty owner yields a plain ObjectKey.
//
// The record is part of the NAME, not a sidecar: it costs no extra write
// or read on any driver, survives every copy of the key, and cannot drift
// from the object it describes. It is not secret (the key is already
// handed to the owner's organization in API responses) and needs no
// signature: a caller who rewrites it names an object that does not exist.
func OwnedObjectKey(hash string, owner map[string]string) (string, error) {
	key, err := ObjectKey(hash)
	if err != nil || len(owner) == 0 {
		return key, err
	}
	v := url.Values{}
	for name, val := range owner {
		if val != "" {
			v.Set(name, val)
		}
	}
	if len(v) == 0 {
		return key, nil
	}
	return key + ownerMark + base64.RawURLEncoding.EncodeToString([]byte(v.Encode())), nil
}

// KeyOwner returns the owner record of a stored object's key (or handle),
// and whether it has one. A key stored before owner records existed, or by
// a writer with no caller, has none.
func KeyOwner(key string) (map[string]string, bool) {
	last := key[strings.LastIndex(key, "/")+1:]
	i := strings.Index(last, ownerMark)
	if i < 0 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(last[i+len(ownerMark):])
	if err != nil {
		return nil, false
	}
	v, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, false
	}
	out := make(map[string]string, len(v))
	for name := range v {
		out[name] = v.Get(name)
	}
	return out, true
}

// scopeKeyPrefix mirrors restgw.ScopeKey's prefix (kvfs sits below restgw).
const scopeKeyPrefix = "x-w17-scope-"

// incomingOwner reads the data scopes a request carries into this tier —
// the `x-w17-scope-<name>` metadata the gateway threads — so a file the
// business tier stores while serving a caller is owned like an upload of
// that caller. Nil outside a request.
func incomingOwner(ctx context.Context) map[string]string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	var out map[string]string
	for k, vals := range md {
		if !strings.HasPrefix(k, scopeKeyPrefix) || len(vals) != 1 || vals[0] == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[strings.TrimPrefix(k, scopeKeyPrefix)] = vals[0]
	}
	return out
}
