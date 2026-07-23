package observx

import "testing"

// The chokepoint that guarantees a secret cannot become a resource
// attribute: secret attrs and empty values are dropped; non-secret
// values are exported under w17.config.<lower(key)>.
func TestConfigResourceAttrsExcludeSecrets(t *testing.T) {
	attrs := configResourceAttrs([]ConfigAttr{
		{Key: "APP_FEATURE_FLAG", Value: "true"},
		{Key: "APP_DB_PASSWORD", Value: "hunter2", Secret: true},
		{Key: "APP_STRIPE_KEY", Value: "sk_live_x", Secret: true},
		{Key: "APP_EMPTY", Value: ""},
		{Key: "APP_REGION", Value: "eu-west-1"},
	})

	got := map[string]string{}
	for _, a := range attrs {
		got[string(a.Key)] = a.Value.AsString()
	}

	// Non-secret, non-empty present under the namespaced key.
	if got["w17.config.app_feature_flag"] != "true" {
		t.Errorf("feature flag not exported: %v", got)
	}
	if got["w17.config.app_region"] != "eu-west-1" {
		t.Errorf("region not exported: %v", got)
	}

	// Secrets absent entirely — neither key nor value.
	for k, v := range got {
		if v == "hunter2" || v == "sk_live_x" {
			t.Fatalf("secret value leaked into resource attr %s=%s", k, v)
		}
	}
	if _, ok := got["w17.config.app_db_password"]; ok {
		t.Errorf("secret key leaked into resource attrs: %v", got)
	}
	if _, ok := got["w17.config.app_stripe_key"]; ok {
		t.Errorf("secret key leaked into resource attrs: %v", got)
	}

	// Empty value dropped.
	if _, ok := got["w17.config.app_empty"]; ok {
		t.Errorf("empty value should be dropped: %v", got)
	}

	if len(attrs) != 2 {
		t.Errorf("expected 2 exported attrs, got %d", len(attrs))
	}
}
