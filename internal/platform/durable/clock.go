package durable

import "time"

type Clock interface {
	Now() time.Time
	Timer(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) Timer(d time.Duration) <-chan time.Time {
	if d <= 0 {
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	timer := time.NewTimer(d)
	return timer.C
}

func RealClock() Clock { return realClock{} }
