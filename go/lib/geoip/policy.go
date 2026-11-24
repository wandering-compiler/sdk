package geoip

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Policy is which countries a surface or a method admits: an allow-list
// (only these) or a deny-list (all but these), never both.
type Policy struct {
	allow map[string]bool
	deny  map[string]bool
	// RefuseUnknown refuses a public address the database does not place
	// in any country. Admitted by default.
	RefuseUnknown bool
	// RefuseUnavailable refuses every request while no database is loaded
	// (before the first download succeeded). Admitted by default: blocking
	// is a filter, and a gateway that cannot reach the download stays up.
	RefuseUnavailable bool
}

// NewPolicy builds a policy from ISO 3166-1 alpha-2 codes. Exactly one of
// allow and deny is non-empty; the codes are case-insensitive.
func NewPolicy(allow, deny []string) (*Policy, error) {
	if (len(allow) == 0) == (len(deny) == 0) {
		return nil, fmt.Errorf("geoip: a policy names countries to allow or to deny, not both and not neither")
	}
	p := &Policy{}
	var err error
	if p.allow, err = codeSet(allow); err != nil {
		return nil, err
	}
	if p.deny, err = codeSet(deny); err != nil {
		return nil, err
	}
	return p, nil
}

// MustPolicy is NewPolicy for generated code, whose lists the parser has
// already checked.
func MustPolicy(allow, deny []string, refuseUnknown, refuseUnavailable bool) *Policy {
	p, err := NewPolicy(allow, deny)
	if err != nil {
		panic(err)
	}
	p.RefuseUnknown, p.RefuseUnavailable = refuseUnknown, refuseUnavailable
	return p
}

func codeSet(codes []string) (map[string]bool, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	m := make(map[string]bool, len(codes))
	for _, c := range codes {
		u := strings.ToUpper(strings.TrimSpace(c))
		if !ValidCode(u) {
			return nil, fmt.Errorf("geoip: %q is not an ISO 3166-1 alpha-2 country code", c)
		}
		m[u] = true
	}
	return m, nil
}

// ValidCode reports whether c has the shape of an ISO 3166-1 alpha-2 code:
// two ASCII upper-case letters.
func ValidCode(c string) bool {
	return len(c) == 2 && c[0] >= 'A' && c[0] <= 'Z' && c[1] >= 'A' && c[1] <= 'Z'
}

// Verdict is what [Policy.Check] decided and why.
type Verdict struct {
	Allowed bool
	// Country is the address's country, empty when unknown.
	Country string
	// Reason, for observability: "allowed", "country", "unknown",
	// "unavailable", "private".
	Reason string
}

// Check decides one address. A private, loopback, link-local or unspecified
// address is always admitted: it is a caller inside the deployment (or a
// developer's machine), which no database places. A nil policy admits
// everything.
func (p *Policy) Check(db *DB, ip netip.Addr) Verdict {
	if p == nil {
		return Verdict{Allowed: true, Reason: "allowed"}
	}
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return Verdict{Allowed: true, Reason: "private"}
	}
	if !db.Ready() {
		return Verdict{Allowed: !p.RefuseUnavailable, Reason: "unavailable"}
	}
	c, ok := db.Country(ip)
	if !ok {
		return Verdict{Allowed: !p.RefuseUnknown, Reason: "unknown"}
	}
	switch {
	case p.allow != nil && !p.allow[c], p.deny != nil && p.deny[c]:
		return Verdict{Allowed: false, Country: c, Reason: "country"}
	}
	return Verdict{Allowed: true, Country: c, Reason: "allowed"}
}

// String describes the policy for the gateway's start-up line.
func (p *Policy) String() string {
	if p == nil {
		return "any country"
	}
	list := func(m map[string]bool) string {
		s := make([]string, 0, len(m))
		for c := range m {
			s = append(s, c)
		}
		sort.Strings(s)
		return strings.Join(s, ",")
	}
	if p.allow != nil {
		return "only " + list(p.allow)
	}
	return "all but " + list(p.deny)
}
