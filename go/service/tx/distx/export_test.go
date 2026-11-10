package distx

import "time"

// SetRollbackWait shortens the bound on the client's detached rollbacks for
// a test and returns the restore.
func SetRollbackWait(d time.Duration) (restore func()) {
	prev := rollbackWait
	rollbackWait = d
	return func() { rollbackWait = prev }
}

// RollbackWait is the client's bound on its detached rollbacks.
func RollbackWait() time.Duration { return rollbackWait }
