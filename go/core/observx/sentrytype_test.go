package observx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type pgError struct{ code string }

func (e *pgError) Error() string { return "ERROR: duplicate key (" + e.code + ")" }

// What an issue is titled by: the type of the error that means something,
// never the wrapper's.
func TestSentryExceptionTypesAreReadable(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want []string // innermost first, as sent
	}{
		"wrapped typed error": {fmt.Errorf("save invoice: %w", fmt.Errorf("insert: %w", &pgError{"23505"})),
			[]string{"observx.pgError", "observx.pgError", "observx.pgError"}},
		"bare errors.New":    {errors.New("boom"), []string{"Error"}},
		"wrapped errors.New": {fmt.Errorf("load: %w", errors.New("no rows")), []string{"Error", "Error"}},
		"grpc status": {fmt.Errorf("call billing: %w", status.Error(codes.Unavailable, "down")),
			[]string{"Unavailable", "Unavailable"}},
	} {
		t.Run(name, func(t *testing.T) {
			events := captureSentry(t)
			ReportError(context.Background(), tc.err)
			got := events()
			if len(got) != 1 {
				t.Fatalf("%d events", len(got))
			}
			var types []string
			for _, x := range got[0].Exception {
				types = append(types, x.Type)
			}
			if fmt.Sprint(types) != fmt.Sprint(tc.want) {
				t.Fatalf("types %v, want %v", types, tc.want)
			}
			if v := got[0].Exception[len(got[0].Exception)-1].Value; v != tc.err.Error() {
				t.Fatalf("the message changed: %q", v)
			}
		})
	}
}
