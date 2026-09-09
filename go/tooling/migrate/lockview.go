package migrate

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	gopath "path"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// The deploy-side view of `w17/lock.yaml`, and the reason it lives HERE.
//
// Applying migrations is moving out of `w17ctl` and into the generated binary
// that owns the database — the only process that can reach it. Both need the
// same three fields off the lock (which connections exist, and what each one
// is pinned to), and if each reads the file for itself there are two YAML
// views of one artefact that nothing keeps in step. They would agree today
// and diverge the first time a field is added, silently, because the half
// that stops reading a pin just applies less than it was asked to.
//
// So the view is defined once, in the public SDK both callers already import.
// It carries no compiler knowledge to a place that should not have it: these
// are routing pins the console writes for the deploy path to read, and the
// bodies they point at are native SQL the console lowered long before.
//
// It does NOT verify the lock's signature — no client-side crypto (D4). It
// reads routing fields off a file the deployment already trusts enough to
// hand its database credentials to.

// LockView is the subset of the lock the deploy path reads. Every other field
// (generated_code, secrets, signature, …) is ignored by the decoder; the
// struct is deliberately partial so a lock that grows a field does not need
// this to grow with it.
type LockView struct {
	Project     string           `yaml:"project"`
	ProjectID   string           `yaml:"project_id"`
	Connections []LockConnection `yaml:"connections"`
}

// LockConnection mirrors a lock connection's deploy pins. Dialect and version
// live in proto, not the lock — the lock tracks only what to deploy TO.
type LockConnection struct {
	Name                string `yaml:"name"`
	TargetMigrationID   string `yaml:"target_migration_id"`
	TargetContentSha256 string `yaml:"target_content_sha256"`
}

// LoadLockView reads and decodes the lock at path into the partial view.
func LoadLockView(path string) (*LockView, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lock: read %s: %w", path, err)
	}
	var lk LockView
	if err := yaml.Unmarshal(data, &lk); err != nil {
		return nil, fmt.Errorf("lock: parse %s: %w", path, err)
	}
	return &lk, nil
}

// ConnTargets turns a lock view into the orchestrator's per-connection deploy
// ceilings.
//
// Connections with no pinned target are carried through rather than dropped:
// Plan skips them with its own notice, and dropping them here would make
// "this connection has never been pushed" indistinguishable from "this
// connection is not in the lock at all".
func (l *LockView) ConnTargets() []ConnTarget {
	if l == nil {
		return nil
	}
	tgts := make([]ConnTarget, 0, len(l.Connections))
	for _, c := range l.Connections {
		tgts = append(tgts, ConnTarget{
			Connection:          c.Name,
			TargetMigrationID:   c.TargetMigrationID,
			TargetContentSha256: c.TargetContentSha256,
		})
	}
	return tgts
}

// TargetEnvVar is the env var a connection's DSN is read from:
// `W17_TARGET_<CONN_UPPER>`, hyphens collapsed to underscores so
// `read-replica` resolves via `W17_TARGET_READ_REPLICA`.
//
// DSNs are env-only and never a flag, so a credential does not land in shell
// history or a process listing. Defined here because the generated binary and
// w17ctl must agree on the name — an operator who sets the variable one of
// them reads and the other does not gets "no DSN for connection", pointing at
// a variable they can see is set.
func TargetEnvVar(connection string) string {
	out := make([]byte, 0, len(connection))
	for i := 0; i < len(connection); i++ {
		c := connection[i]
		switch {
		case c == '-':
			out = append(out, '_')
		case c >= 'a' && c <= 'z':
			out = append(out, c-('a'-'A'))
		default:
			out = append(out, c)
		}
	}
	return "W17_TARGET_" + string(out)
}

// MigrationSetFS lays a freshly fetched migration set out as a filesystem, in
// the SAME layout WriteMigration produces on disk, so `apply --fetch` can hand
// it straight to the orchestrator without touching a disk.
//
// Why marshal it back to JSON instead of passing the messages through: the
// loader is where a migration's content hash is verified, and that check is
// the only thing between "these are the bytes the console signed" and "these
// are whatever arrived". A fetched set that bypassed the loader would be the
// one path into the applier with no hash check on it — and it is the path a
// deploy will use every time, which is the worst possible place for that gap.
// The round-trip costs microseconds and buys the guarantee outright.
//
// Only the `<id>.json` is materialised. The `.up.sql` / `.down.sql` siblings
// WriteMigration also writes are an operator-audit convenience for a tree
// someone reads; nothing loads them, and an in-memory tree has no reader.
func MigrationSetFS(migs []*applyfetchpb.Migration) (fs.FS, error) {
	set := memFS{}
	for _, m := range migs {
		if m.GetId() == "" {
			return nil, fmt.Errorf("MigrationSetFS: migration with empty id")
		}
		if m.GetConnection() == "" {
			return nil, fmt.Errorf("MigrationSetFS: empty connection on %s", m.GetId())
		}
		buf, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: true, Indent: "  "}.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("MigrationSetFS: marshal %s: %w", m.GetId(), err)
		}
		set[gopath.Join(m.GetConnection(), m.GetId()+".json")] = buf
	}
	return set, nil
}

// memFS is a read-only fs.FS over an in-memory file map. Written here rather
// than reached for in testing/fstest because this is production code on the
// deploy path, and only the two operations the loader performs are needed.
type memFS map[string][]byte

func (m memFS) Open(name string) (fs.File, error) {
	buf, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memFile{name: gopath.Base(name), Reader: bytes.NewReader(buf), size: int64(len(buf))}, nil
}

func (m memFS) ReadDir(name string) ([]fs.DirEntry, error) {
	var out []fs.DirEntry
	prefix := name + "/"
	for p := range m {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if strings.Contains(rest, "/") {
			continue // no nesting below <connection>/ in this layout
		}
		out = append(out, memEntry(rest))
	}
	if len(out) == 0 {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type memFile struct {
	*bytes.Reader
	name string
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) { return memInfo{name: f.name, size: f.size}, nil }
func (f *memFile) Close() error               { return nil }

type memEntry string

func (e memEntry) Name() string               { return string(e) }
func (e memEntry) IsDir() bool                { return false }
func (e memEntry) Type() fs.FileMode          { return 0 }
func (e memEntry) Info() (fs.FileInfo, error) { return memInfo{name: string(e)}, nil }

type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
