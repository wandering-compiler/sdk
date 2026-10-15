package restgw

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/wandering-compiler/sdk/go/lib/imagevariant"
	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// VariantJob renders the declared variants of one stored image.
type VariantJob struct {
	Driver   kvfs.Driver
	Key      string
	Variants []imagevariant.Spec
}

// VariantQueue takes variant work off the upload request. Enqueue never
// blocks: a full queue drops the job and reports false, because nothing
// depends on it — a variant that is not there when it is asked for is rendered
// on the spot by the download handler. The queue only moves that cost from the
// first viewer to the background.
type VariantQueue interface {
	Enqueue(VariantJob) bool
}

// DefaultVariantWorkers is how many images a gateway resizes at once when
// <PREFIX>_VARIANT_WORKERS is unset.
const DefaultVariantWorkers = 2

// variantQueueDepth bounds the jobs waiting for a worker; a burst beyond it
// (a mass import) is dropped and rendered lazily instead of growing memory.
const variantQueueDepth = 1024

// VariantWorkersFromEnv reads <prefix>_VARIANT_WORKERS: how many images this
// gateway resizes at once. 0 turns background rendering off (every variant
// is then rendered on its first request); unset is DefaultVariantWorkers.
func VariantWorkersFromEnv(prefix string, getenv func(string) string) (int, error) {
	v := getenv(prefix + "_VARIANT_WORKERS")
	if v == "" {
		return DefaultVariantWorkers, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, errors.New(prefix + "_VARIANT_WORKERS is not a number of workers (0 or more)")
	}
	return n, nil
}

// VariantWorkers is the in-process VariantQueue: a bounded queue drained by a
// fixed number of goroutines. In-process means a job dies with the process;
// the download handler's on-the-spot render covers it.
type VariantWorkers struct {
	jobs    chan VariantJob
	wg      sync.WaitGroup
	dropped atomic.Int64
	report  func(error)
}

// StartVariantWorkers starts n workers until ctx ends; n == 0 returns nil
// (a nil *VariantWorkers enqueues nothing). report receives each failed
// render — an operator's concern, never a caller's.
func StartVariantWorkers(ctx context.Context, n int, report func(error)) *VariantWorkers {
	if n <= 0 {
		return nil
	}
	q := &VariantWorkers{jobs: make(chan VariantJob, variantQueueDepth), report: report}
	for range n {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-q.jobs:
					q.render(ctx, j)
				}
			}
		}()
	}
	return q
}

// Enqueue adds a job, or drops it when the queue is full.
func (q *VariantWorkers) Enqueue(j VariantJob) bool {
	if q == nil || len(j.Variants) == 0 {
		return false
	}
	select {
	case q.jobs <- j:
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

// Dropped is how many jobs a full queue has turned away.
func (q *VariantWorkers) Dropped() int64 {
	if q == nil {
		return 0
	}
	return q.dropped.Load()
}

// Wait returns once every worker has stopped (after ctx ended).
func (q *VariantWorkers) Wait() {
	if q != nil {
		q.wg.Wait()
	}
}

func (q *VariantWorkers) render(ctx context.Context, j VariantJob) {
	for _, spec := range j.Variants {
		vkey := VariantKey(j.Key, spec.Name)
		if _, err := j.Driver.Stat(ctx, vkey); err == nil {
			continue // a download rendered it first
		}
		status, err := renderVariant(ctx, j.Driver, j.Key, vkey, spec)
		if err != nil && q.report != nil {
			if status == http.StatusUnsupportedMediaType {
				// Not an image this server resizes: every variant would fail
				// the same way, and the download answers 415 for each.
				q.report(err)
				return
			}
			q.report(err)
		}
	}
}
