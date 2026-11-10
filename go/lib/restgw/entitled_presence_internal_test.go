package restgw

import "testing"

// The audience rule is "every event label is PRESENT with the same value in
// the principal's labels". A map lookup reads a missing key as "", so an
// event label with an empty value was possessed by every principal that
// lacks the label.
func TestEntitled_ALabelMustBePresentNotMerelyEqualToTheZeroValue(t *testing.T) {
	cases := []struct {
		name      string
		event     map[string]string
		principal map[string]string
		want      bool
	}{
		{"empty event value, principal lacks the label", map[string]string{"org": ""}, map[string]string{"account_kind": "person"}, false},
		{"empty event value, nil principal labels", map[string]string{"org": ""}, nil, false},
		{"empty event value, principal has it empty", map[string]string{"org": ""}, map[string]string{"org": ""}, true},
		{"matching value", map[string]string{"org": "a"}, map[string]string{"org": "a", "x": "y"}, true},
		{"different value", map[string]string{"org": "a"}, map[string]string{"org": "b"}, false},
	}
	for _, c := range cases {
		if got := entitled(true, c.event, c.principal); got != c.want {
			t.Errorf("%s: entitled = %v, want %v", c.name, got, c.want)
		}
	}
}
