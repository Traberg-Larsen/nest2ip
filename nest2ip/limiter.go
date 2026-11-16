package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Priority decides who gets the last calls in the budget.
type Priority int

const (
	PrioExtend   Priority = iota // keep a running stream alive: may use the whole budget
	PrioGenerate                 // start a new stream: leaves room for extends
	PrioStop                     // polite cleanup: only if there is room right now
)

var ErrNoBudget = errors.New("ingen kvote lige nu")

// Limiter is the one place every SDM executeCommand call has to pass.
// It counts calls in sliding windows per project and per camera and never
// lets a call through that would break a limit.
type Limiter struct {
	mu sync.Mutex

	projectPerMin int
	camPerMin     int
	camPerHour    int
	reserve       int // project slots per minute kept free for extends

	project []time.Time            // calls in the last minute (project)
	cams    map[string][]time.Time // calls in the last hour (per camera)

	pausedUntil time.Time // project-wide pause after a 429
	total429    int

	now func() time.Time
}

func NewLimiter(projectPerMin, camPerMin, camPerHour int) *Limiter {
	reserve := 2
	if projectPerMin <= 3 {
		reserve = 0
	}
	return &Limiter{
		projectPerMin: projectPerMin,
		camPerMin:     camPerMin,
		camPerHour:    camPerHour,
		reserve:       reserve,
		cams:          map[string][]time.Time{},
		now:           time.Now,
	}
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

func countAfter(ts []time.Time, cutoff time.Time) int {
	n := 0
	for _, t := range ts {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// tryLocked returns 0 if the call may go now (and records it), or how long to wait.
func (l *Limiter) tryLocked(cam string, prio Priority) time.Duration {
	now := l.now()
	l.project = prune(l.project, now.Add(-time.Minute))
	l.cams[cam] = prune(l.cams[cam], now.Add(-time.Hour))
	camTs := l.cams[cam]

	var wait time.Duration
	if now.Before(l.pausedUntil) {
		wait = l.pausedUntil.Sub(now)
	}

	projLimit := l.projectPerMin
	if prio != PrioExtend {
		projLimit -= l.reserve
	}
	if len(l.project) >= projLimit {
		// wait until enough old calls drop out of the minute window
		idx := len(l.project) - projLimit
		if w := l.project[idx].Add(time.Minute).Sub(now); w > wait {
			wait = w
		}
	}
	camMin := countAfter(camTs, now.Add(-time.Minute))
	if camMin >= l.camPerMin {
		first := len(camTs) - camMin
		idx := first + camMin - l.camPerMin
		if w := camTs[idx].Add(time.Minute).Sub(now); w > wait {
			wait = w
		}
	}
	if len(camTs) >= l.camPerHour {
		idx := len(camTs) - l.camPerHour
		if w := camTs[idx].Add(time.Hour).Sub(now); w > wait {
			wait = w
		}
	}
	if wait > 0 {
		return wait + 50*time.Millisecond
	}
	l.project = append(l.project, now)
	l.cams[cam] = append(camTs, now)
	return 0
}

// Acquire blocks until a call for cam is allowed (or ctx ends). PrioStop never waits.
func (l *Limiter) Acquire(ctx context.Context, cam string, prio Priority) error {
	for {
		l.mu.Lock()
		wait := l.tryLocked(cam, prio)
		l.mu.Unlock()
		if wait == 0 {
			return nil
		}
		if prio == PrioStop {
			return ErrNoBudget
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Pause stops all calls for the project until t (used after a 429).
func (l *Limiter) Pause(until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total429++
	if until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}

type LimiterStats struct {
	ProjectLastMinute int       `json:"calls_last_minute"`
	ProjectLastHour   int       `json:"calls_last_hour"`
	PausedUntil       time.Time `json:"paused_until,omitzero"`
	Total429          int       `json:"total_429"`
	ProjectPerMinute  int       `json:"limit_per_minute"`
	CameraPerMinute   int       `json:"limit_camera_per_minute"`
	CameraPerHour     int       `json:"limit_camera_per_hour"`
}

func (l *Limiter) Stats() LimiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	s := LimiterStats{
		ProjectLastMinute: countAfter(l.project, now.Add(-time.Minute)),
		Total429:          l.total429,
		ProjectPerMinute:  l.projectPerMin,
		CameraPerMinute:   l.camPerMin,
		CameraPerHour:     l.camPerHour,
	}
	for _, ts := range l.cams {
		s.ProjectLastHour += countAfter(ts, now.Add(-time.Hour))
	}
	if now.Before(l.pausedUntil) {
		s.PausedUntil = l.pausedUntil
	}
	return s
}

func (l *Limiter) CameraCounts(cam string) (lastMinute, lastHour int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ts := l.cams[cam]
	return countAfter(ts, now.Add(-time.Minute)), countAfter(ts, now.Add(-time.Hour))
}

// backoffLadder is the wait after the n-th failure in a row (capped at the last step).
var backoffLadder = []time.Duration{
	1 * time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
}

func backoffFor(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures > len(backoffLadder) {
		failures = len(backoffLadder)
	}
	return backoffLadder[failures-1]
}
