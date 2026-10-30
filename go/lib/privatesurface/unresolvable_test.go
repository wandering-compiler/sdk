package privatesurface

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A private surface whose host does not resolve is said at boot, naming the
// host and the private proxy, instead of surfacing only as UNAVAILABLE on the
// first call; a host that resolves, and an IP address, say nothing.
func TestWarnUnresolvable(t *testing.T) {
	prevResolve, prevLog := resolveHost, warnLog
	t.Cleanup(func() { resolveHost, warnLog = prevResolve, prevLog })
	var logged []string
	warnLog = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }

	resolveHost = func(context.Context, string) error { return errors.New("no such host") }
	warnUnresolvable("authplatform-private", "authplatform-private:50061")
	if len(logged) != 1 || !strings.Contains(logged[0], `"authplatform-private" does not resolve`) || !strings.Contains(logged[0], "private-proxy") {
		t.Fatalf("logged = %q, want one warning naming the host and the private proxy", logged)
	}

	logged = nil
	warnUnresolvable("10.0.0.7", "10.0.0.7:50061")
	resolveHost = func(context.Context, string) error { return nil }
	warnUnresolvable("identity-private", "identity-private:50061")
	if len(logged) != 0 {
		t.Fatalf("an IP address or a resolving host warned: %q", logged)
	}
}
