package lifecycle

import "time"

// clock abstracts time so IdleShutdown's fire-vs-Touch race can be
// driven deterministically from tests (constitution/Constitution.md
// §11.4.115 — a sub-millisecond real-clock race is not reproducible
// with wall-clock sleeps). Production code always uses realClock;
// this seam stays unexported so NewIdleShutdown's public signature
// and behavior are unchanged for every existing caller.
type clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) timerHandle
}

// timerHandle abstracts the subset of *time.Timer that IdleShutdown
// needs, so a test double can capture the scheduled callback without
// letting it fire on its own.
type timerHandle interface {
	Stop() bool
	Reset(d time.Duration) bool
}

// realClock is the production clock implementation, delegating to
// time.Now and time.AfterFunc.
type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

func (realClock) AfterFunc(d time.Duration, f func()) timerHandle {
	return &realTimer{t: time.AfterFunc(d, f)}
}

// realTimer wraps *time.Timer to satisfy timerHandle.
type realTimer struct {
	t *time.Timer
}

func (r *realTimer) Stop() bool {
	return r.t.Stop()
}

func (r *realTimer) Reset(d time.Duration) bool {
	return r.t.Reset(d)
}
