package migratecli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestDispatch_HelpDoesNotStartAServer — a generated bundle's binary is both a
// server and a small CLI, and `--help` has to be answered by the CLI half.
//
// A consumer ran `<binary> --help` inside a container and read "starting on
// :50051". They concluded the binary ignored its arguments, went looking for
// another way to apply a schema, and lost an afternoon to it — the help was
// the one place that would have told them what this binary can do.
func TestDispatch_HelpDoesNotStartAServer(t *testing.T) {
	for _, word := range []string{"-h", "--help", "help"} {
		var buf bytes.Buffer
		handled, err := Dispatch(context.Background(), []string{word}, Options{Out: &buf})
		if err != nil {
			t.Fatalf("%s: %v", word, err)
		}
		if !handled {
			t.Errorf("%s: fell through to the server — which is how it printed a listen address instead of help", word)
			continue
		}
		for _, want := range []string{"migrate", "fixtures"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("%s: help does not name the %q command:\n%s", word, want, buf.String())
			}
		}
	}
}

// TestDispatch_FlagsStillFallThrough — the server half keeps every FLAG this
// package does not own. Without this the help arm would be free to swallow a
// bundle's own flags.
//
// It used to cover bare words too ("serve"), which is the behaviour a
// consumer's compose step hung on: an unknown verb started the server instead
// of failing, so a step gating on completion waited forever. Every image runs
// `ENTRYPOINT ["/server"]` with no verb, so nothing passes one on purpose —
// see TestDispatch_UnknownVerbIsRefused.
func TestDispatch_FlagsStillFallThroughFromHelpArm(t *testing.T) {
	for _, word := range []string{"--listen=:9000", "-v"} {
		handled, err := Dispatch(context.Background(), []string{word}, Options{Out: &bytes.Buffer{}})
		if err != nil {
			t.Fatalf("%s: %v", word, err)
		}
		if handled {
			t.Errorf("%s: claimed by the CLI half; the generated main has its own flags and must still see it", word)
		}
	}
}
