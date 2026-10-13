package restgw

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The issuer principal checks the verification was BOUND, not only present.
// Through the gate an unbound verification never carries an Authorization
// header, so this is reachable only by a caller that put an unbound verdict
// on the context next to a credential; the principal still refuses it.
func TestDPoPTicketPrincipal_RefusesAnUnboundVerification(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/w17-ticket", nil)
	r.Header.Set("Authorization", "DPoP tok-1")
	r = r.WithContext(withDPoPVerified(r.Context(), DPoPVerified{Thumbprint: "jkt-1"}))
	if _, err := DPoPTicketPrincipal(r); !errors.Is(err, ErrDPoPTicketUnverified) {
		t.Fatalf("unbound verification: %v, want ErrDPoPTicketUnverified", err)
	}
	r = r.WithContext(withDPoPVerified(r.Context(), DPoPVerified{Thumbprint: "jkt-1", AccessToken: "tok-1"}))
	labels, err := DPoPTicketPrincipal(r)
	if err != nil || labels[WSAuthTicketThumbprintLabel] != "jkt-1" || labels[WSAuthTicketHeaderLabel] != "DPoP tok-1" {
		t.Fatalf("bound verification: %v %v", labels, err)
	}
}
