package geoip

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

var testDB = map[string]string{
	"203.0.113.0/24":  "CZ",
	"198.51.100.0/24": "RU",
	"2001:db8::/32":   "DE",
}

func loadedDB(t *testing.T) *DB {
	t.Helper()
	d := &DB{}
	if err := d.Load(buildMMDB(testDB)); err != nil {
		t.Fatalf("load: %v", err)
	}
	return d
}

func TestCountry(t *testing.T) {
	d := loadedDB(t)
	for ip, want := range map[string]string{
		"203.0.113.7":        "CZ",
		"::ffff:203.0.113.7": "CZ",
		"198.51.100.200":     "RU",
		"2001:db8::1":        "DE",
		"192.0.2.1":          "",
		"2001:db9::1":        "",
	} {
		got, ok := d.Country(netip.MustParseAddr(ip))
		if got != want || ok != (want != "") {
			t.Errorf("Country(%s) = %q, %v; want %q", ip, got, ok, want)
		}
	}
	if d.BuiltAt().IsZero() {
		t.Error("BuiltAt is zero")
	}
}

func TestLoadRefusesGarbageAndKeepsTheOld(t *testing.T) {
	d := loadedDB(t)
	if err := d.Load([]byte("not a database")); err == nil {
		t.Fatal("garbage loaded")
	}
	if c, _ := d.Country(netip.MustParseAddr("203.0.113.7")); c != "CZ" {
		t.Errorf("old database lost: %q", c)
	}
}

func TestPolicy(t *testing.T) {
	d := loadedDB(t)
	deny := MustPolicy(nil, []string{"ru"}, false, false)
	allow := MustPolicy([]string{"CZ", "SK"}, nil, false, false)
	strictAllow := MustPolicy([]string{"CZ"}, nil, true, true)
	cases := []struct {
		p      *Policy
		db     *DB
		ip     string
		ok     bool
		reason string
	}{
		{deny, d, "198.51.100.1", false, "country"},
		{deny, d, "203.0.113.1", true, "allowed"},
		{deny, d, "192.0.2.1", true, "unknown"},
		{allow, d, "203.0.113.1", true, "allowed"},
		{allow, d, "2001:db8::5", false, "country"},
		{allow, d, "192.0.2.1", true, "unknown"},
		{strictAllow, d, "192.0.2.1", false, "unknown"},
		{allow, d, "10.1.2.3", true, "private"},
		{allow, d, "127.0.0.1", true, "private"},
		{strictAllow, d, "::1", true, "private"},
		{allow, &DB{}, "198.51.100.1", true, "unavailable"},
		{strictAllow, &DB{}, "198.51.100.1", false, "unavailable"},
		{nil, d, "198.51.100.1", true, "allowed"},
	}
	for _, c := range cases {
		v := c.p.Check(c.db, netip.MustParseAddr(c.ip))
		if v.Allowed != c.ok || v.Reason != c.reason {
			t.Errorf("%s under %s: %+v; want allowed=%v reason=%s", c.ip, c.p, v, c.ok, c.reason)
		}
	}
}

func TestNewPolicyRejects(t *testing.T) {
	for _, c := range []struct{ allow, deny []string }{
		{nil, nil},
		{[]string{"CZ"}, []string{"RU"}},
		{[]string{"CZE"}, nil},
		{nil, []string{"1A"}},
	} {
		if _, err := NewPolicy(c.allow, c.deny); err == nil {
			t.Errorf("NewPolicy(%v, %v) accepted", c.allow, c.deny)
		}
	}
}

func TestGateOffAdmits(t *testing.T) {
	g := &Gate{DB: loadedDB(t), Off: true}
	p := MustPolicy(nil, []string{"RU"}, false, false)
	if !g.Check(p, netip.MustParseAddr("198.51.100.1")).Allowed {
		t.Error("an off gate refused")
	}
	on := &Gate{DB: g.DB}
	if err := on.Status(context.Background(), p, "198.51.100.1:4433"); err == nil || !strings.Contains(err.Error(), "PermissionDenied") {
		t.Errorf("Status = %v; want PermissionDenied", err)
	}
	if err := on.Status(context.Background(), p, "[2001:db8::1]:443"); err != nil {
		t.Errorf("Status(DE) = %v", err)
	}
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func TestDBIPFallsBackAMonthAndSkipsWhatItHas(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasSuffix(r.URL.Path, "dbip-country-lite-2026-10.mmdb.gz") {
			_, _ = w.Write(gz(buildMMDB(testDB)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	s := &DBIP{BaseURL: srv.URL, now: func() time.Time { return time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC) }}
	dir := t.TempDir()
	u := &Updater{DB: &DB{}, Source: s, CacheDir: dir}
	if err := u.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if c, _ := u.DB.Country(netip.MustParseAddr("203.0.113.9")); c != "CZ" {
		t.Fatalf("country = %q", c)
	}
	before := hits.Load()
	if err := u.Once(context.Background()); err != nil {
		t.Fatalf("second Once: %v", err)
	}
	// November is asked again (it may have appeared); October is held.
	if got := hits.Load() - before; got != 1 {
		t.Errorf("second run made %d requests; want 1", got)
	}
	// A restart starts from the cache, before any download.
	u2 := &Updater{DB: &DB{}, Source: s, CacheDir: dir}
	u2.loadCache()
	if !u2.DB.Ready() || !strings.HasSuffix(u2.have, "2026-10.mmdb.gz") {
		t.Errorf("cache not loaded: ready=%v have=%q", u2.DB.Ready(), u2.have)
	}
}

func TestFailedDownloadKeepsTheDatabase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gz([]byte("corrupt")))
	}))
	defer srv.Close()
	u := &Updater{DB: loadedDB(t), Source: &DBIP{BaseURL: srv.URL}}
	if err := u.Once(context.Background()); err == nil {
		t.Fatal("a corrupt database was accepted")
	}
	if !u.DB.Ready() {
		t.Fatal("the database in use was dropped")
	}
}

func TestFileSourceReloadsOnChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.mmdb")
	if err := os.WriteFile(p, buildMMDB(testDB), 0o600); err != nil {
		t.Fatal(err)
	}
	u := &Updater{DB: &DB{}, Source: &File{Path: p}}
	if err := u.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buildMMDB(map[string]string{"203.0.113.0/24": "SK"}), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, future, future)
	if err := u.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, _ := u.DB.Country(netip.MustParseAddr("203.0.113.1")); c != "SK" {
		t.Errorf("not reloaded: %q", c)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("APP_GEOIP_SOURCE", "off")
	g, u, line, err := FromEnv("APP")
	if err != nil || !g.Off || u != nil || !strings.Contains(line, "OFF") {
		t.Errorf("off: %v %v %v %q", err, g, u, line)
	}
	t.Setenv("APP_GEOIP_SOURCE", "maxmind")
	if _, _, _, err := FromEnv("APP"); err == nil {
		t.Error("maxmind without a key accepted")
	}
	t.Setenv("APP_GEOIP_SOURCE", "")
	t.Setenv("APP_GEOIP_REFRESH", "6h")
	g, u, line, err = FromEnv("APP")
	if err != nil || g.Off || u == nil || u.Every != 6*time.Hour || !strings.Contains(line, "DB-IP") {
		t.Errorf("default: %v %v %q", err, u, line)
	}
	t.Setenv("APP_GEOIP_SOURCE", "ftp://x")
	if _, _, _, err := FromEnv("APP"); err == nil {
		t.Error("unknown source accepted")
	}
}

func BenchmarkCheck(b *testing.B) {
	d := &DB{}
	if err := d.Load(buildMMDB(testDB)); err != nil {
		b.Fatal(err)
	}
	p := MustPolicy(nil, []string{"RU"}, false, false)
	ip := netip.MustParseAddr("203.0.113.7")
	b.ReportAllocs()
	for b.Loop() {
		p.Check(d, ip)
	}
}

func TestRequireHTTP(t *testing.T) {
	g := &Gate{DB: loadedDB(t)}
	p := MustPolicy(nil, []string{"RU"}, false, false)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequireHTTP(g, p, w, r) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	if Admit(g, nil) != nil || Admit(&Gate{Off: true}, p) != nil {
		t.Error("Admit without a check is not nil")
	}
	for addr, want := range map[string]int{"198.51.100.4:1": http.StatusForbidden, "203.0.113.4:1": http.StatusNoContent} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		restgw.ClientAddrMiddleware(nil)(h).ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", addr, rec.Code, want)
		}
		if want == http.StatusForbidden && !strings.Contains(rec.Body.String(), CodeRefused) {
			t.Errorf("%s: body %s lacks %s", addr, rec.Body.String(), CodeRefused)
		}
	}
}
