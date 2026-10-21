package ratelimit

import (
	"strings"
	"testing"
)

func TestRequireTrustedProxies(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"unset in production", map[string]string{"W17_ENV": "production"}, false},
		{"unset without W17_ENV", nil, false},
		{"blank is unset", map[string]string{"GW_PUBLIC_RATELIMIT_TRUSTED_PROXIES": "  "}, false},
		{"another scope's does not count", map[string]string{"GW_LOGIN_RATELIMIT_TRUSTED_PROXIES": "none"}, false},
		{"none", map[string]string{"GW_PUBLIC_RATELIMIT_TRUSTED_PROXIES": "none"}, true},
		{"CIDRs", map[string]string{"GW_PUBLIC_RATELIMIT_TRUSTED_PROXIES": "10.0.0.0/8"}, true},
		{"the bare name it inherits", map[string]string{"GW_RATELIMIT_TRUSTED_PROXIES": "private"}, true},
		{"a development stack", map[string]string{"W17_ENV": "dev"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"W17_ENV", "GW_PUBLIC_RATELIMIT_TRUSTED_PROXIES", "GW_LOGIN_RATELIMIT_TRUSTED_PROXIES", "GW_RATELIMIT_TRUSTED_PROXIES"} {
				t.Setenv(k, c.env[k])
			}
			err := RequireTrustedProxies("GW", "PUBLIC")
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if err != nil && (!strings.Contains(err.Error(), "GW_PUBLIC_RATELIMIT_TRUSTED_PROXIES is not set") || !strings.Contains(err.Error(), "fix:")) {
				t.Fatalf("the refusal does not name the variable and the fix: %v", err)
			}
		})
	}
}
