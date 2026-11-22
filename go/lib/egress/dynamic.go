package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The failure budget of a dynamic client's destinations.
const (
	DefaultBudget   = 5
	DefaultCooldown = 30 * time.Second
	MaxCooldown     = 10 * time.Minute
	// maxTracked bounds the destinations a client remembers; past it the
	// oldest healthy ones are forgotten (a suspended one is kept).
	maxTracked = 10000
)

// refusedAddressError is a dial the address policy refused.
type refusedAddressError struct {
	addr  netip.Addr
	class string
}

func (e refusedAddressError) Error() string {
	return fmt.Sprintf("the destination resolves to a %s address (%s), which this client's DSN does not allow", e.class, e.addr)
}

// addressClass names what kind of address a is, "" for a public one.
func addressClass(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsMulticast() || a.IsInterfaceLocalMulticast() || a.IsLinkLocalMulticast():
		return "multicast"
	case a.IsLoopback():
		return "loopback"
	case a.IsLinkLocalUnicast():
		return "link_local"
	case a.IsPrivate() || cgnat.Contains(a):
		return "private"
	case a.IsUnspecified():
		return "unspecified"
	}
	return ""
}

// cgnat is 100.64.0.0/10 (RFC 6598), which IsPrivate does not cover.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// checkAddress refuses an address whose class the policy does not allow.
// Unspecified and multicast are never allowed.
func checkAddress(a netip.Addr, allow map[string]bool) error {
	c := addressClass(a)
	if c == "" || allow[c] {
		return nil
	}
	return refusedAddressError{addr: a.Unmap(), class: c}
}

// guardedDialer dials only addresses the policy allows. The check runs on
// the address actually dialled, after resolution, so a name that resolves
// (or re-resolves) to an internal address is refused as well.
func guardedDialer(allow map[string]bool) *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			a, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			return checkAddress(a, allow)
		},
	}
}

// checkResolved resolves host and checks every address — for a client
// whose requests go through a proxy, where the dial is the proxy's.
func checkResolved(ctx context.Context, host string, allow map[string]bool) error {
	if a, err := netip.ParseAddr(host); err == nil {
		return checkAddress(a, allow)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if err := checkAddress(a, allow); err != nil {
			return err
		}
	}
	return nil
}

// refusedAddressCallError is the call's answer when the policy refused the
// destination: the caller's data names somewhere the client may not go.
func refusedAddressCallError(client string, err error) error {
	var r refusedAddressError
	if !errors.As(err, &r) {
		return nil
	}
	return callError(codes.PermissionDenied, client, ReasonDestinationRefused, r.Error(), map[string]string{"address_class": r.class})
}

// budgets suspends a dynamic client's destinations that keep failing, so
// one customer's dead endpoint does not spend the client's capacity for all
// the others.
type budgets struct {
	mu       sync.Mutex
	budget   int
	cooldown time.Duration
	now      func() time.Time
	hosts    map[string]*destState
}

type destState struct {
	failures  int
	until     time.Time // suspended until
	nextPause time.Duration
	seen      time.Time
}

func newBudgets(budget int, cooldown time.Duration) *budgets {
	if budget <= 0 {
		budget = DefaultBudget
	}
	if cooldown <= 0 {
		cooldown = DefaultCooldown
	}
	return &budgets{budget: budget, cooldown: cooldown, now: time.Now, hosts: map[string]*destState{}}
}

// admit refuses a call to a suspended destination with UNAVAILABLE, saying
// for how long.
func (b *budgets) admit(client, host string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.hosts[host]
	if s == nil || !b.now().Before(s.until) {
		return nil
	}
	wait := s.until.Sub(b.now()).Round(time.Second)
	st := callError(codes.Unavailable, client, ReasonDestinationSuspended,
		fmt.Sprintf("the destination failed %d calls in a row and is suspended for %s", s.failures, wait),
		map[string]string{"retry_after_s": strconv.Itoa(int(wait.Seconds()))})
	return st
}

// record counts one call's outcome: a success clears the destination, an
// upstream failure (unreachable, 5xx, 429, its deadline) counts against it.
// A refusal of the caller's own making (a bad request, a policy refusal, a
// 4xx) does not.
func (b *budgets) record(host string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.hosts[host]
	if err == nil {
		if s != nil {
			delete(b.hosts, host)
		}
		return
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
	default:
		return
	}
	if s == nil {
		if len(b.hosts) >= maxTracked {
			b.forgetOldestLocked()
		}
		s = &destState{nextPause: b.cooldown}
		b.hosts[host] = s
	}
	s.failures++
	s.seen = b.now()
	if s.failures >= b.budget && !b.now().Before(s.until) {
		s.until = b.now().Add(s.nextPause)
		s.nextPause = min(2*s.nextPause, MaxCooldown)
	}
}

func (b *budgets) forgetOldestLocked() {
	var oldest string
	var at time.Time
	for h, s := range b.hosts {
		if b.now().Before(s.until) {
			continue
		}
		if oldest == "" || s.seen.Before(at) {
			oldest, at = h, s.seen
		}
	}
	if oldest != "" {
		delete(b.hosts, oldest)
	}
}
