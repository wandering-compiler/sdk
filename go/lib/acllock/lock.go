// Package acllock owns the acl.lock.json artifact: deterministic
// monotonic allocation of integer IDs to permission strings +
// reservation of removed IDs + checksum-backed drift detection.
//
// Mirrors the compiler's eventbus lock in shape and discipline.
// The eventbus lock allocates oneof field numbers for the
// envelope message; the ACL lock allocates opaque integer IDs
// to permission strings of shape `<module>.<Model>#<action>`
// and `<module>.<Service>.<Method>`. The IDs are the values
// the wire AuthResp carries (`repeated int32 permission_ids`);
// the gateway emit bakes the int literal into per-handler check
// sites from this lock.
//
// Lock file lives in source control at
// `proto/domains/<domain>/acl.lock.json` (parallel to
// events.lock.json). Generators supply the path; this package
// doesn't hardcode it.
//
// Placeholder note: this is the Go-only helper. A future
// per-language w17 toolkit will likely lift these helpers into
// a multi-vendored shape (TS / Python / etc.); until then the
// auth backend reads the JSON directly via embed +
// ReadBytes.
package acllock

// CurrentVersion is the schema version this package reads and
// writes. Bumping requires a parallel migration path; readers
// refuse any unrecognised version at Read time so accidental
// downgrades don't silently produce a corrupted lock.
const CurrentVersion = 1

// Lock is the on-disk acl.lock.json schema:
//
//	{
//	  "version": 1,
//	  "permissions": {
//	    "tasks.Task#add": 1,
//	    "tasks.Task#delete": 2,
//	    "tasks.TaskService.BulkExport": 3
//	  },
//	  "reserved": [4],
//	  "checksum": "sha256:..."
//	}
//
// Permissions is the deterministic ID allocation table. Keys
// are the canonical permission strings the gateway derives from
// the ACL cascade — `<module>.<Model>#<action>` for model perms,
// `<module>.<Service>.<Method>` for endpoint perms. Values are
// opaque integer IDs; new perms get the smallest non-negative
// integer not already in Permissions or Reserved.
//
// Reserved holds IDs that were once allocated but are no longer
// in use (a perm was removed from the cascade). Monotonic — once
// in Reserved, never recycled — so JWTs / DB rows carrying
// stale IDs never collide with a freshly-allocated perm.
//
// Checksum is a tamper-detection digest of the lock contents. NOTE:
// this package only models the lock + the allocator (Allocate / Clone /
// MaxID / the HasPermission bit helpers); it does NOT read, write, or
// verify the checksum. The actual persistence + checksum recompute/
// verify flow lives in the gateway's `gatewayacllock` package (which
// renders/parses the proto lock form). Treat this field as data here.
type Lock struct {
	Version     int            `json:"version"`
	Permissions map[string]int `json:"permissions"`
	Reserved    []int          `json:"reserved"`
	Checksum    string         `json:"checksum"`
}

// Clone returns a deep copy of l so callers can mutate one
// snapshot without affecting another. Allocate uses it
// implicitly (it builds a fresh Lock); tests + caller code
// occasionally need explicit duplication.
func (l *Lock) Clone() *Lock {
	if l == nil {
		return nil
	}
	out := &Lock{
		Version:     l.Version,
		Permissions: make(map[string]int, len(l.Permissions)),
		Reserved:    append([]int(nil), l.Reserved...),
		Checksum:    l.Checksum,
	}
	for k, v := range l.Permissions {
		out.Permissions[k] = v
	}
	return out
}

// MaxID returns the largest allocated permission ID (across
// Permissions and Reserved). Returns 0 when the lock is empty.
// Used by GrantAll to size the bitset.
func (l *Lock) MaxID() int {
	if l == nil {
		return 0
	}
	max := 0
	for _, id := range l.Permissions {
		if id > max {
			max = id
		}
	}
	for _, id := range l.Reserved {
		if id > max {
			max = id
		}
	}
	return max
}
