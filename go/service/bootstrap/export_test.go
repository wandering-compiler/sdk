package bootstrap

import "os"

// SetNotifySignalsForTest replaces how the supervisor registers for signals
// and returns the restore.
func SetNotifySignalsForTest(f func(chan<- os.Signal)) func() {
	prev := notifySignals
	notifySignals = f
	return func() { notifySignals = prev }
}

// ShutdownDeadlineForTest is the deadline the supervisor reads from lookup.
var ShutdownDeadlineForTest = shutdownDeadline
