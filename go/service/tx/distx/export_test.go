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

// SetRegisterWait shortens how long RemoteCommits waits for the coordinator
// to confirm a wait, and returns the restore.
func SetRegisterWait(d time.Duration) (restore func()) {
	prev := registerWait
	registerWait = d
	return func() { registerWait = prev }
}
