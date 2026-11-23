// Package geoip answers which country an IP address is in, from a local
// country database (the MaxMind DB format: DB-IP's IP-to-Country Lite or
// MaxMind's GeoLite2 Country), and whether a [Policy] admits it.
//
// A lookup is an in-memory search of the database, well under a
// microsecond, so a gateway checks every request synchronously, before the
// handler: there is no asynchronous mode to opt into. The database keeps
// itself current: an [Updater] downloads it at start and again whenever the
// source has a newer one, verifies it, and swaps it in under the readers
// without a restart. A failed download keeps the database the gateway
// already has.
//
// The whole file is read into memory (about 8 MB for a country database)
// rather than mapped, so a swap only drops the old one to the garbage
// collector — a lookup in flight never reads an unmapped page.
//
// Spec: docs/specs/gateway/geo-blocking.md (platform).
package geoip

import (
	"fmt"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// DB is a country database that an [Updater] replaces under its readers.
// The zero value has no database: every lookup is unknown and Ready is false.
type DB struct {
	cur atomic.Pointer[loaded]
}

type loaded struct {
	r       *maxminddb.Reader
	builtAt time.Time
	kind    string
}

// countryRecord is the part of a GeoIP2-compatible record a country lookup
// reads; DB-IP Lite and GeoLite2 both carry it.
type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	// RegisteredCountry answers for an address whose physical country the
	// database does not know (anycast, satellite): where it is registered.
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

// Load replaces the database with the one in b, after verifying it is a
// well-formed MaxMind DB that answers country lookups. On error the
// database in use stays.
func (d *DB) Load(b []byte) error {
	r, err := maxminddb.OpenBytes(b)
	if err != nil {
		return fmt.Errorf("geoip: open database: %w", err)
	}
	if err := r.Verify(); err != nil {
		return fmt.Errorf("geoip: verify database: %w", err)
	}
	if r.Metadata.NodeCount == 0 {
		return fmt.Errorf("geoip: database %q has no entries", r.Metadata.DatabaseType)
	}
	built := time.Unix(int64(r.Metadata.BuildEpoch), 0).UTC() // #nosec G115 -- a build time in seconds fits int64
	d.cur.Store(&loaded{r: r, builtAt: built, kind: r.Metadata.DatabaseType})
	return nil
}

// Ready reports whether a database is loaded.
func (d *DB) Ready() bool { return d != nil && d.cur.Load() != nil }

// BuiltAt is when the loaded database was built; zero without one.
func (d *DB) BuiltAt() time.Time {
	if l := d.cur.Load(); l != nil {
		return l.builtAt
	}
	return time.Time{}
}

// Country returns the ISO 3166-1 alpha-2 code of ip's country ("CZ"), and
// false when there is no database or it does not know the address.
func (d *DB) Country(ip netip.Addr) (string, bool) {
	if d == nil {
		return "", false
	}
	l := d.cur.Load()
	if l == nil || !ip.IsValid() {
		return "", false
	}
	var rec countryRecord
	if err := l.r.Lookup(ip.Unmap()).Decode(&rec); err != nil {
		return "", false
	}
	if c := rec.Country.ISOCode; c != "" {
		return c, true
	}
	if c := rec.RegisteredCountry.ISOCode; c != "" {
		return c, true
	}
	return "", false
}
