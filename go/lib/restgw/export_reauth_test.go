package restgw

import "time"

// SetStreamReauthRetryMinForTest shortens the first retry delay after an
// unanswered re-auth probe and returns a function restoring the default.
func SetStreamReauthRetryMinForTest(d time.Duration) func() {
	prev := streamReauthRetryMin.Swap(int64(d))
	return func() { streamReauthRetryMin.Store(prev) }
}
