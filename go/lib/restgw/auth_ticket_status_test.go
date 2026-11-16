package restgw

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteAuthError_TicketSentinelsAre401: the generated per-RPC stream
// handlers and the authenticated download routes answer an auth failure
// through WriteAuthError, and accept ?ticket=. A browser EventSource that
// reconnects with its consumed one-shot ticket got 500 plus a server-fault
// report per reconnect, because the two ticket sentinels are plain errors
// and only RequireAuth (the /w17-events hub) mapped them. Wrapped or not,
// they are an authentication failure.
func TestWriteAuthError_TicketSentinelsAre401(t *testing.T) {
	for _, err := range []error{
		ErrTicketInvalid,
		ErrWSAuthNoTicket,
		fmt.Errorf("auth: %w", ErrTicketInvalid),
	} {
		rec := httptest.NewRecorder()
		WriteAuthError(rec, err)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("WriteAuthError(%v) = %d, want 401", err, rec.Code)
		}
	}
	// A genuine failure keeps its server-side status.
	rec := httptest.NewRecorder()
	WriteAuthError(rec, fmt.Errorf("ticket store unreachable"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a non-auth failure = %d, want 500", rec.Code)
	}
}
