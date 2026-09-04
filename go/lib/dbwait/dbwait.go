// Package dbwait waits for a database to accept connections instead of
// exiting the moment it refuses.
//
// A dependency that has not finished starting is a NORMAL state, not a
// fault. The generated bundle used to ping once and return an error, which
// ends the process; under docker compose that is a restart, and a container
// whose published port is EPHEMERAL gets a different host port each time it
// starts. Anything that discovered the port before the restart then dials a
// port nothing is listening on.
//
// deinvo traced their "flaky discover host port" — open since 2026-07-28,
// blamed in turn on IPv6 and on a race between listeners, both wrong — to
// exactly that chain. A 358-statement role seed made initdb slow enough
// that the server lost the race every run, exited, restarted, and
// renumbered; `w17ctl test` then waited 15s on a port that belonged to
// nothing while the stack sat there healthy, answering 401 on the port it
// had actually been given.
//
// Retrying here removes the restart, and with it the renumbering. It is the
// deeper of the two fixes they proposed, and it is deeper because the
// restart is harmful everywhere — not only where something happened to be
// holding a port number.
package dbwait

import (
	"context"
	"fmt"
	"time"
)

// Pinger is the one method this package needs; *sql.DB satisfies it.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Logf receives one line when waiting starts and one when it ends. Nil is
// allowed and silences both.
//
// It reports rather than counts: a boot that waits eight seconds for
// Postgres should say so once, because an operator watching the logs needs
// to tell "waiting for a dependency" from "hung". Logging every attempt
// would bury that in a backoff transcript.
type Logf func(format string, args ...any)

const (
	firstDelay = 100 * time.Millisecond
	maxDelay   = 2 * time.Second
	growth     = 1.5
)

// Ping blocks until p answers, budget elapses, or ctx is done.
//
// The first attempt is immediate, so a database that is already up costs
// nothing. Failure still fails: an unreachable database is a real fault and
// this returns one, naming how long it waited and how many attempts it
// made — the two facts that separate "misconfigured" from "still starting".
func Ping(ctx context.Context, p Pinger, label string, budget time.Duration, logf Logf) error {
	if p == nil {
		return fmt.Errorf("dbwait: nil pinger for %s", label)
	}
	if budget <= 0 {
		budget = 60 * time.Second
	}
	deadline := time.Now().Add(budget)
	delay := firstDelay
	attempts := 0
	var last error
	announced := false

	for {
		attempts++
		// Each attempt gets what is left of the budget, so a hanging dial
		// cannot outlive the wait it belongs to.
		attemptCtx, cancel := context.WithDeadline(ctx, deadline)
		last = p.PingContext(attemptCtx)
		cancel()
		if last == nil {
			if announced && logf != nil {
				logf("%s: accepted connections after %s (%d attempts)", label, roundish(time.Since(deadline.Add(-budget))), attempts)
			}
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w (waiting for the database; %d attempts, last error: %w)", label, ctx.Err(), attempts, last)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%s: not accepting connections after %s (%d attempts): %w",
				label, roundish(budget), attempts, last)
		}
		if !announced && logf != nil {
			// Once, on the first refusal. A dependency still starting is
			// the expected case; the line exists so a slow one is visible
			// rather than looking like a hang.
			logf("%s: not ready yet (%v) — retrying for up to %s", label, last, roundish(budget))
			announced = true
		}
		if delay > remaining {
			delay = remaining
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return fmt.Errorf("%s: %w (waiting for the database; %d attempts, last error: %w)", label, ctx.Err(), attempts, last)
		}
		if next := time.Duration(float64(delay) * growth); next < maxDelay {
			delay = next
		} else {
			delay = maxDelay
		}
	}
}

// roundish trims sub-second noise out of durations that appear in messages
// a human reads.
func roundish(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(10 * time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}
