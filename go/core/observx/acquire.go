package observx

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

// ErrHeld is returned (wrapped) when an init or shutdown would replace
// or tear down observability state that a running supervisor holds
// through [Acquire].
var ErrHeld = errors.New("observx: observability is held by a running supervisor")

// holders counts the live [Acquire] holds; heldConfig is the
// configuration they share. Both are guarded by mu.
var (
	holders    int
	heldConfig Config
)

func heldError(op string) error {
	return fmt.Errorf("%w (%d hold(s), service %q); %s would replace telemetry under running components — release every hold first",
		ErrHeld, holders, heldConfig.ServiceName, op)
}

// Acquire is the reference-counted form of [MustInit] for code that may
// run more than once in one process — the component supervisor
// (bootstrap.Run) uses it, so several supervisors in one test binary or
// one composed binary share one observability setup instead of each
// replacing the one the others are running on.
//
// The observability state is process-global: one tracer provider, one
// meter provider, one Sentry hub. The first Acquire initialises it from
// c exactly as MustInit does. A later Acquire while any hold is live
// shares that state when c is identical to the configuration it was
// initialised from, and is refused with [ErrHeld] when it differs —
// there is no way to give two configurations to one process, and
// silently keeping either would mislabel the other's telemetry. While
// any hold is live, [MustInit] and [Shutdown] are refused the same way.
//
// release drops the hold; the last release shuts observability down
// (flushing, bounded by ctx) and returns Shutdown's error. Calling
// release again is a no-op that returns nil.
//
// When the state was initialised directly by MustInit and nothing holds
// it, the first Acquire re-initialises from c, as a second MustInit
// would.
func Acquire(c Config) (release func(context.Context) error, err error) {
	mu.Lock()
	defer mu.Unlock()
	if holders > 0 {
		if !reflect.DeepEqual(c, heldConfig) {
			return nil, heldError("an Acquire with a different Config")
		}
		holders++
		return newRelease(), nil
	}
	if err := initLocked(c); err != nil {
		return nil, err
	}
	holders = 1
	heldConfig = c
	return newRelease(), nil
}

func newRelease() func(context.Context) error {
	var once sync.Once
	return func(ctx context.Context) error {
		var err error
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			holders--
			if holders > 0 {
				return
			}
			heldConfig = Config{}
			err = shutdownLocked(ctx)
		})
		return err
	}
}
