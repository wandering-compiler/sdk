package geoip

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// MsgidRefused is the sentence a caller from a refused country reads.
// Harvested into each project's catalogs with the gateway's other refusals.
const MsgidRefused = "This service is not available in your location."

// CodeRefused is the refusal's details[].code, for a client to dispatch on.
const CodeRefused = "GEO_REFUSED"

// Vocabulary is every msgid this package puts in front of a person.
func Vocabulary() []string { return []string{MsgidRefused} }

// Gate is a gateway's geo check: its database and whether checking is on.
// A nil Gate, or one turned off, admits everything.
type Gate struct {
	DB  *DB
	Off bool
}

// Check decides ip under p.
func (g *Gate) Check(p *Policy, ip netip.Addr) Verdict {
	if g == nil || g.Off || p == nil {
		return Verdict{Allowed: true, Reason: "allowed"}
	}
	return p.Check(g.DB, ip)
}

// Status is Check as a gRPC answer: nil when admitted, PERMISSION_DENIED
// with the translated sentence when not. addr is the caller's address as
// the gateway resolved it ("203.0.113.7" or "203.0.113.7:51234").
func (g *Gate) Status(ctx context.Context, p *Policy, addr string) error {
	v := g.Check(p, ParseAddr(addr))
	if v.Allowed {
		return nil
	}
	return status.Error(codes.PermissionDenied, i18n.T(ctx, MsgidRefused, nil))
}

// ParseAddr reads a client address with or without a port; the zero Addr
// when it is neither.
func ParseAddr(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr()
	}
	a, _ := netip.ParseAddr(strings.Trim(s, "[]"))
	return a
}

// FromEnv builds a gateway's Gate and the Updater that keeps it current:
//
//	<PREFIX>_GEOIP_SOURCE   dbip (default) | maxmind | file:<path> | off
//	<PREFIX>_GEOIP_MAXMIND_ACCOUNT_ID, <PREFIX>_GEOIP_MAXMIND_LICENSE_KEY
//	                        for maxmind
//	<PREFIX>_GEOIP_CACHE_DIR  where the last good database is kept across
//	                        restarts (default: <tmp>/w17-geoip-<prefix>)
//	<PREFIX>_GEOIP_REFRESH  how often a newer database is looked for
//	                        (default 24h)
//
// With SOURCE=off the gate admits everything and the updater is nil. The
// returned line says which, for the gateway to log once at start.
func FromEnv(prefix string) (*Gate, *Updater, string, error) {
	env := func(k string) string { return strings.TrimSpace(os.Getenv(prefix + "_GEOIP_" + k)) }
	src := env("SOURCE")
	gate := &Gate{DB: &DB{}}
	var s Source
	switch {
	case strings.EqualFold(src, "off"):
		gate.Off = true
		return gate, nil, fmt.Sprintf("geo blocking OFF (%s_GEOIP_SOURCE=off): every country is admitted", prefix), nil
	case src == "" || strings.EqualFold(src, "dbip"):
		s = &DBIP{}
	case strings.EqualFold(src, "maxmind"):
		id, key := env("MAXMIND_ACCOUNT_ID"), env("MAXMIND_LICENSE_KEY")
		if id == "" || key == "" {
			return nil, nil, "", fmt.Errorf("%s_GEOIP_SOURCE=maxmind needs %s_GEOIP_MAXMIND_ACCOUNT_ID and %s_GEOIP_MAXMIND_LICENSE_KEY", prefix, prefix, prefix)
		}
		s = &MaxMind{AccountID: id, LicenseKey: key}
	case strings.HasPrefix(strings.ToLower(src), "file:"):
		s = &File{Path: src[len("file:"):]}
	default:
		return nil, nil, "", fmt.Errorf("%s_GEOIP_SOURCE=%q: want dbip, maxmind, file:<path> or off", prefix, src)
	}
	every := 24 * time.Hour
	if r := env("REFRESH"); r != "" {
		d, err := time.ParseDuration(r)
		if err != nil || d < time.Minute {
			return nil, nil, "", fmt.Errorf("%s_GEOIP_REFRESH=%q: want a duration of at least 1m", prefix, r)
		}
		every = d
	}
	dir := env("CACHE_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "w17-geoip-"+strings.ToLower(prefix))
	}
	u := &Updater{DB: gate.DB, Source: s, CacheDir: dir, Every: every, Log: slog.Default()}
	return gate, u, fmt.Sprintf("geo blocking ON: database from %s, refreshed every %s", s.Name(), every), nil
}

var shared struct {
	mu    sync.Mutex
	gates map[string]*Gate
}

// Shared is FromEnv once per process and prefix, with its Updater running
// for the life of the process: every surface of one gateway binary (REST,
// gRPC, MCP) checks against one database, downloaded once. The start-up
// line is logged on the first call; a configuration error is fatal to the
// caller, as any other bad env.
func Shared(prefix string) (*Gate, error) {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if g, ok := shared.gates[prefix]; ok {
		return g, nil
	}
	g, u, line, err := FromEnv(prefix)
	if err != nil {
		return nil, err
	}
	slog.Info(line)
	if u != nil {
		go u.Run(context.Background())
	}
	if shared.gates == nil {
		shared.gates = map[string]*Gate{}
	}
	shared.gates[prefix] = g
	return g, nil
}
