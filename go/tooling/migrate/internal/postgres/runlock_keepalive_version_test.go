package postgres

import (
	"strings"
	"testing"
)

// tcp_user_timeout does not exist before PostgreSQL 12, and a SET of it there
// fails "unrecognized configuration parameter" — which would refuse every
// apply. The lock session leaves it out below 12 and keeps the keepalives,
// which every supported server has. (No PG < 12 server runs in the live
// lanes; the live keepalive test covers 12 and up.)
func TestLockSessionKeepalive_TCPUserTimeoutOnlyFromPG12(t *testing.T) {
	for _, c := range []struct {
		version int
		want    bool
	}{{90600, false}, {110022, false}, {119999, false}, {120000, true}, {160016, true}, {180007, true}} {
		set := lockSessionKeepalive(c.version)
		if got := strings.Contains(set, "tcp_user_timeout"); got != c.want {
			t.Errorf("server_version_num %d: tcp_user_timeout set = %v, want %v (%s)", c.version, got, c.want, set)
		}
		for _, k := range []string{"tcp_keepalives_idle = 30", "tcp_keepalives_interval = 10", "tcp_keepalives_count = 3"} {
			if !strings.Contains(set, k) {
				t.Errorf("server_version_num %d: %s missing (%s)", c.version, k, set)
			}
		}
	}
}
