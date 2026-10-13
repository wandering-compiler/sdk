package dpopgw

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// The issuer principal checks the verification was BOUND, not only present.
// Through the gate an unbound verification never carries an Authorization
// header, so this is reachable only by a caller that put an unbound verdict
// on the context next to a credential; the principal still refuses it.
func TestTicketPrincipal_RefusesAnUnboundVerification(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/w17-ticket", nil)
	r.Header.Set("Authorization", "DPoP tok-1")
	r = r.WithContext(withVerified(r.Context(), Verified{Thumbprint: "jkt-1"}))
	if _, err := TicketPrincipal(r); !errors.Is(err, ErrTicketUnverified) {
		t.Fatalf("unbound verification: %v, want ErrTicketUnverified", err)
	}
	r = r.WithContext(withVerified(r.Context(), Verified{Thumbprint: "jkt-1", AccessToken: "tok-1"}))
	labels, err := TicketPrincipal(r)
	if err != nil || labels[TicketThumbprintLabel] != "jkt-1" || labels[restgw.WSAuthTicketHeaderLabel] != "DPoP tok-1" {
		t.Fatalf("bound verification: %v %v", labels, err)
	}
}
