package restgw

import "time"

// SetStreamReauthRetryMinForTest shortens the first retry delay after an
// unanswered re-auth probe and returns a function restoring the default.
func SetStreamReauthRetryMinForTest(d time.Duration) func() {
	prev := streamReauthRetryMin.Swap(int64(d))
	return func() { streamReauthRetryMin.Store(prev) }
}

// SetW17EventsWriteTimeoutForTest shortens the per-write deadline on the
// reserved w17-events stream and returns a function restoring the default.
func SetW17EventsWriteTimeoutForTest(d time.Duration) func() {
	prev := w17EventsWriteTimeoutOverride.Swap(int64(d))
	return func() { w17EventsWriteTimeoutOverride.Store(prev) }
}
