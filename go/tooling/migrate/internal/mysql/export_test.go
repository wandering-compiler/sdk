package mysql

import "time"

// SetLockSessionTimingForTest replaces the lock session's idle limit and the
// heartbeat interval until the returned func restores them.
func SetLockSessionTimingForTest(idle, beat time.Duration) func() {
	i, b := lockSessionIdle, lockHeartbeatEvery
	lockSessionIdle, lockHeartbeatEvery = idle, beat
	return func() { lockSessionIdle, lockHeartbeatEvery = i, b }
}

// SilenceForTest stops a run lock's heartbeat without releasing it: the
// holder is still connected but says nothing, as a holder whose host lost
// the network looks to the server.
func SilenceForTest(rl interface{}) {
	if l, ok := rl.(*runLock); ok {
		l.stopHeartbeat()
	}
}

// SetHeartbeatForTest replaces the heartbeat's statement and its per-beat
// bound until the returned func restores them.
func SetHeartbeatForTest(stmt string, bound time.Duration) func() {
	s, b := heartbeatStmt, lockHeartbeatTimeout
	heartbeatStmt, lockHeartbeatTimeout = stmt, bound
	return func() { heartbeatStmt, lockHeartbeatTimeout = s, b }
}
