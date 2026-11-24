package geoip

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxDatabase caps a download: a country database is about 8 MB, so a
// larger answer is not one.
const maxDatabase = 64 << 20

// Source is where an [Updater] gets the database.
type Source interface {
	// Name is the source's name for the start-up line and the logs.
	Name() string
	// Fetch returns the newest database, or (nil, nil) when it is not newer
	// than the one identified by have (the identity a previous Fetch
	// returned; empty = none).
	Fetch(ctx context.Context, have string) (db []byte, id string, err error)
}

// DBIP is DB-IP's IP-to-Country Lite (CC BY 4.0: an application that shows
// the country must credit DB-IP). It is published monthly, at a URL naming
// the month, so the URL is the identity: a month already held is not
// downloaded again.
type DBIP struct {
	Client *http.Client
	// BaseURL overrides https://download.db-ip.com/free (tests, a mirror).
	BaseURL string
	now     func() time.Time
}

func (s *DBIP) Name() string { return "DB-IP IP-to-Country Lite" }

func (s *DBIP) Fetch(ctx context.Context, have string) ([]byte, string, error) {
	base := s.BaseURL
	if base == "" {
		base = "https://download.db-ip.com/free"
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	t := now().UTC()
	// This month's file appears on its first days; until it does, the
	// previous month's is the newest.
	var lastErr error
	for _, m := range []time.Time{t, t.AddDate(0, -1, 0)} {
		url := fmt.Sprintf("%s/dbip-country-lite-%04d-%02d.mmdb.gz", strings.TrimRight(base, "/"), m.Year(), int(m.Month()))
		if url == have {
			return nil, have, nil
		}
		b, status, err := get(ctx, s.Client, url, nil)
		if status == http.StatusNotFound {
			lastErr = fmt.Errorf("geoip: %s: not published", url)
			continue
		}
		if err != nil {
			return nil, "", err
		}
		db, err := gunzip(b)
		if err != nil {
			return nil, "", fmt.Errorf("geoip: %s: %w", url, err)
		}
		return db, url, nil
	}
	return nil, "", lastErr
}

// MaxMind is MaxMind's GeoLite2 Country, which needs a (free) account: its
// id and a license key. MaxMind's licence asks for a database at most 30
// days old; the updater checks daily. The identity is the response's
// Last-Modified.
type MaxMind struct {
	Client     *http.Client
	AccountID  string
	LicenseKey string
	// BaseURL overrides https://download.maxmind.com (tests, a mirror).
	BaseURL string
}

func (s *MaxMind) Name() string { return "MaxMind GeoLite2 Country" }

func (s *MaxMind) Fetch(ctx context.Context, have string) ([]byte, string, error) {
	base := s.BaseURL
	if base == "" {
		base = "https://download.maxmind.com"
	}
	url := strings.TrimRight(base, "/") + "/geoip/databases/GeoLite2-Country/download?suffix=tar.gz"
	hdr := http.Header{}
	if have != "" {
		hdr.Set("If-Modified-Since", have)
	}
	req := func(r *http.Request) { r.SetBasicAuth(s.AccountID, s.LicenseKey) }
	b, status, lastMod, err := getWith(ctx, s.Client, url, hdr, req)
	if status == http.StatusNotModified {
		return nil, have, nil
	}
	if err != nil {
		return nil, "", err
	}
	tgz, err := gunzip(b)
	if err != nil {
		return nil, "", fmt.Errorf("geoip: GeoLite2 archive: %w", err)
	}
	db, err := fromTar(tgz, ".mmdb")
	if err != nil {
		return nil, "", fmt.Errorf("geoip: GeoLite2 archive: %w", err)
	}
	return db, lastMod, nil
}

// File is a database the deployment provides itself (an air-gapped
// environment, a commercial database). It is re-read when its modification
// time changes.
type File struct{ Path string }

func (s *File) Name() string { return "file " + s.Path }

func (s *File) Fetch(_ context.Context, have string) ([]byte, string, error) {
	st, err := os.Stat(s.Path)
	if err != nil {
		return nil, "", fmt.Errorf("geoip: %w", err)
	}
	id := st.ModTime().UTC().Format(time.RFC3339Nano)
	if id == have {
		return nil, have, nil
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, "", fmt.Errorf("geoip: %w", err)
	}
	return b, id, nil
}

// Updater keeps a [DB] current from a [Source].
type Updater struct {
	DB     *DB
	Source Source
	// CacheDir keeps the last good database across restarts, so a gateway
	// starts with one before any download; empty = no cache.
	CacheDir string
	// Every is how often the source is asked for a newer database. Default
	// 24h.
	Every time.Duration
	// Retry is how soon a failed attempt is repeated while no database is
	// loaded at all. Default 5m.
	Retry time.Duration
	Log   *slog.Logger

	have string
}

const (
	cacheDB = "country.mmdb"
	cacheID = "country.id"
)

// Run loads the cached database, then fetches until ctx ends. It returns
// when ctx is done.
func (u *Updater) Run(ctx context.Context) {
	u.loadCache()
	every, retry := u.Every, u.Retry
	if every <= 0 {
		every = 24 * time.Hour
	}
	if retry <= 0 {
		retry = 5 * time.Minute
	}
	for {
		wait := every
		if err := u.Once(ctx); err != nil {
			u.log().Warn("geoip: update failed; the database in use stays", "source", u.Source.Name(), "error", err)
			if !u.DB.Ready() {
				wait = retry
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Once asks the source for a newer database and loads it.
func (u *Updater) Once(ctx context.Context) error {
	b, id, err := u.Source.Fetch(ctx, u.have)
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	if err := u.DB.Load(b); err != nil {
		return err
	}
	u.have = id
	u.log().Info("geoip: database loaded", "source", u.Source.Name(), "built", u.DB.BuiltAt().Format("2006-01-02"))
	u.saveCache(b, id)
	return nil
}

func (u *Updater) loadCache() {
	if u.CacheDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(u.CacheDir, cacheDB))
	if err != nil {
		return
	}
	id, _ := os.ReadFile(filepath.Join(u.CacheDir, cacheID))
	if err := u.DB.Load(b); err != nil {
		u.log().Warn("geoip: cached database unusable; fetching", "error", err)
		return
	}
	u.have = strings.TrimSpace(string(id))
}

func (u *Updater) saveCache(b []byte, id string) {
	if u.CacheDir == "" {
		return
	}
	if err := os.MkdirAll(u.CacheDir, 0o750); err != nil {
		u.log().Warn("geoip: cache", "error", err)
		return
	}
	// Written beside and renamed over, so a crash mid-write leaves the old
	// file, never half of a new one.
	for name, data := range map[string][]byte{cacheDB: b, cacheID: []byte(id)} {
		tmp := filepath.Join(u.CacheDir, name+".tmp")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			u.log().Warn("geoip: cache", "error", err)
			return
		}
		if err := os.Rename(tmp, filepath.Join(u.CacheDir, name)); err != nil {
			u.log().Warn("geoip: cache", "error", err)
			return
		}
	}
}

func (u *Updater) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.Default()
}

func get(ctx context.Context, c *http.Client, url string, hdr http.Header) ([]byte, int, error) {
	b, status, _, err := getWith(ctx, c, url, hdr, nil)
	return b, status, err
}

func getWith(ctx context.Context, c *http.Client, url string, hdr http.Header, edit func(*http.Request)) ([]byte, int, string, error) {
	if c == nil {
		c = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, "", err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "w17-geoip")
	if edit != nil {
		edit(req)
	}
	resp, err := c.Do(req) // #nosec G704 -- the URL is the configured database source, not caller input
	if err != nil {
		return nil, 0, "", fmt.Errorf("geoip: download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, "", fmt.Errorf("geoip: download: %s answered %s", req.URL.Host, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDatabase+1))
	if err != nil {
		return nil, resp.StatusCode, "", fmt.Errorf("geoip: download: %w", err)
	}
	if len(b) > maxDatabase {
		return nil, resp.StatusCode, "", errors.New("geoip: download: larger than a country database can be")
	}
	return b, resp.StatusCode, resp.Header.Get("Last-Modified"), nil
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, maxDatabase+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxDatabase {
		return nil, errors.New("larger than a country database can be")
	}
	return out, nil
}

func fromTar(b []byte, suffix string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no %s file in the archive", suffix)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && strings.HasSuffix(h.Name, suffix) {
			return io.ReadAll(io.LimitReader(tr, maxDatabase))
		}
	}
}
