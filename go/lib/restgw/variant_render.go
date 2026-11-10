package restgw

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/imagevariant"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// DefaultMaxConcurrentVariantRenders is how many variants this process
// renders at once on behalf of download requests. A render decodes the whole
// original — up to imagevariant's source cap, about 200 MB for a 50 MP RGBA
// image — so the number of renders in flight, not their size alone, is what
// bounds the memory a burst of first requests costs.
const DefaultMaxConcurrentVariantRenders = 2

// defaultVariantRenderWait is how long a download waits for a render slot
// before it is answered 503 with Retry-After.
const defaultVariantRenderWait = 10 * time.Second

// errVariantBusy is the render a download could not get a slot for.
var errVariantBusy = errors.New("restgw: every variant render slot is busy")

var (
	variantRenderSlots atomic.Pointer[chan struct{}]
	variantRenderWait  atomic.Int64 // time.Duration; 0 = the default
	variantFlightsMu   sync.Mutex
	variantFlights     = map[variantFlightKey]*variantFlight{}
)

func init() { SetMaxConcurrentVariantRenders(DefaultMaxConcurrentVariantRenders) }

// SetMaxConcurrentVariantRenders sets how many variants the download handler
// renders at once in this process (n <= 0 restores the default) and returns a
// function restoring the previous bound. Renders already running keep the
// slot they hold. Call it at start-up; the background workers
// ([StartVariantWorkers]) are bounded by their own count instead.
func SetMaxConcurrentVariantRenders(n int) (restore func()) {
	if n <= 0 {
		n = DefaultMaxConcurrentVariantRenders
	}
	ch := make(chan struct{}, n)
	prev := variantRenderSlots.Swap(&ch)
	return func() {
		if prev != nil {
			variantRenderSlots.Store(prev)
		}
	}
}

type variantFlightKey struct {
	driver any // the driver when comparable, else nil (keyed by vkey alone)
	vkey   string
}

// variantFlight is one render in progress; requests for the same variant
// wait on done and share its outcome.
type variantFlight struct {
	done   chan struct{}
	status int
	err    error
}

// renderVariantShared renders vkey once however many requests ask for it at
// the same time: the first becomes the render, the rest wait for it and share
// its outcome. bounded renders take one of the process's render slots first,
// waiting at most the render wait (503, errVariantBusy, after it); the
// background workers pass false, being bounded by their own count. The
// render itself runs without the request's cancellation, so the requests
// waiting on it are not failed by the one that started it going away.
func renderVariantShared(ctx context.Context, d kvfs.Driver, key, vkey string, spec imagevariant.Spec, bounded bool) (int, error) {
	fk := variantFlightKey{vkey: vkey}
	if t := reflect.TypeOf(d); t != nil && t.Comparable() {
		fk.driver = d
	}
	variantFlightsMu.Lock()
	if f, ok := variantFlights[fk]; ok {
		variantFlightsMu.Unlock()
		select {
		case <-f.done:
			return f.status, f.err
		case <-ctx.Done():
			return http.StatusInternalServerError, ctx.Err()
		}
	}
	f := &variantFlight{done: make(chan struct{})}
	variantFlights[fk] = f
	variantFlightsMu.Unlock()
	defer func() {
		variantFlightsMu.Lock()
		delete(variantFlights, fk)
		variantFlightsMu.Unlock()
		close(f.done)
	}()

	if bounded {
		release, err := acquireVariantSlot(ctx)
		if err != nil {
			f.status, f.err = http.StatusServiceUnavailable, err
			return f.status, f.err
		}
		defer release()
	}
	f.status, f.err = renderVariant(context.WithoutCancel(ctx), d, key, vkey, spec)
	return f.status, f.err
}

// acquireVariantSlot takes a render slot, waiting at most the render wait.
func acquireVariantSlot(ctx context.Context) (release func(), err error) {
	slots := *variantRenderSlots.Load()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
	}
	wait := time.Duration(variantRenderWait.Load())
	if wait <= 0 {
		wait = defaultVariantRenderWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-t.C:
		return nil, errVariantBusy
	case <-ctx.Done():
		return nil, errVariantBusy
	}
}
