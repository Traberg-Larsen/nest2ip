package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"
)

// Camera owns the one Google session for a camera and decides when to call Google.
type Camera struct {
	cfg      CameraConfig
	slug     string
	g        *Google
	lim      *Limiter
	out      *Output
	alwaysOn bool
	idle     time.Duration

	mu                sync.Mutex
	deviceID          string
	deviceName        string
	state             string
	lastErr           string
	lastErrAt         time.Time
	failures          int
	nextAllowed       time.Time
	lastDemand        time.Time
	sessions          int
	extendsOK         int
	extendsFailed     int
	extend400InRow    int
	extendUnsupported bool
	expiresAt         time.Time
	runningSince      time.Time
	sessionSince      time.Time // when the session now on air started
	lastQuickRetry    time.Time

	wake chan struct{}
}

func newCamera(cfg CameraConfig, g *Google, lim *Limiter, idle time.Duration) *Camera {
	always := true
	if cfg.AlwaysOn != nil {
		always = *cfg.AlwaysOn
	}
	return &Camera{
		cfg: cfg, slug: slugify(cfg.Name), g: g, lim: lim, alwaysOn: always, idle: idle,
		state: "venter", wake: make(chan struct{}, 1),
	}
}

func (c *Camera) logf(format string, a ...any) {
	log.Printf("[%s] %s", c.slug, fmt.Sprintf(format, a...))
}

func (c *Camera) setState(s string) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

func (c *Camera) countSession() {
	c.mu.Lock()
	c.sessions++
	c.mu.Unlock()
}

func (c *Camera) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// demand is called when an RTSP client asks for the stream.
func (c *Camera) demand() {
	c.mu.Lock()
	c.lastDemand = time.Now()
	c.mu.Unlock()
	c.poke()
}

func (c *Camera) clientsChanged() {
	c.demand()
}

// wanted says whether a Google session should be open now.
// Always-on cameras: always. On-demand: while someone watches, and for a grace period after.
func (c *Camera) wanted() bool {
	if c.alwaysOn {
		return true
	}
	_, clients, _ := c.out.status()
	if clients > 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.lastDemand) < c.idle
}

// isMediaErr is true when Google accepted the call but the WebRTC media failed or was dropped.
// That costs no extra quota pressure, so it gets a gentler ladder than quota/API errors.
func isMediaErr(err error) bool {
	return errors.Is(err, errICE) || errors.Is(err, errNoMedia) || errors.Is(err, errStalled)
}

// fail records a failure and schedules the next attempt:
// quota/API errors: 1, 2, 5, 10, 30 min. Media drops: 30 s, 1, 2, 5 min.
// A session that ran fine for 3 minutes or more resets the count first. Jitter up to +10 %.
func (c *Camera) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sessionSince.IsZero() && time.Since(c.sessionSince) >= 3*time.Minute {
		c.failures = 0
	}
	c.sessionSince = time.Time{}
	c.failures++
	var wait time.Duration
	if isMediaErr(err) {
		wait = mediaBackoffFor(c.failures)
	} else {
		wait = backoffFor(c.failures)
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > wait {
		wait = ae.RetryAfter
	}
	wait += time.Duration(rand.Int64N(int64(wait / 10))) // jitter, up to +10 %
	c.nextAllowed = time.Now().Add(wait)
	c.lastErr = err.Error()
	c.lastErrAt = time.Now()
	c.state = "pause"
	c.runningSince = time.Time{}
	c.expiresAt = time.Time{}
	log.Printf("[%s] fejl nr. %d i træk: %v. Næste forsøg kl. %s", c.slug, c.failures, err, c.nextAllowed.Local().Format("15:04:05"))
}

func (c *Camera) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-c.wake:
	}
}

func (c *Camera) run(ctx context.Context) {
	for ctx.Err() == nil {
		c.mu.Lock()
		next, dev := c.nextAllowed, c.deviceID
		c.mu.Unlock()
		if dev == "" {
			c.setState("kamera ikke fundet hos Google")
			c.sleep(ctx, time.Minute)
			continue
		}
		if !c.wanted() {
			c.setState("hviler (ingen seere)")
			c.sleep(ctx, 5*time.Second)
			continue
		}
		if wait := time.Until(next); wait > 0 {
			c.setState("pause")
			c.sleep(ctx, wait)
			continue
		}
		s, err := c.openSession(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.fail(err)
			continue
		}
		err = c.keepAlive(ctx, s)
		if err != nil && ctx.Err() == nil {
			if c.quickRetryOK() {
				continue
			}
			c.fail(err)
		}
	}
}

// quickRetryOK allows one immediate reconnect when Google drops a session that had run fine
// for at least a minute, at most once every 2 minutes. Anything else goes through the backoff ladder.
func (c *Camera) quickRetryOK() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ran := time.Since(c.sessionSince)
	if c.sessionSince.IsZero() || ran < time.Minute || time.Since(c.lastQuickRetry) < 2*time.Minute {
		return false
	}
	c.lastQuickRetry = time.Now()
	c.state = "forbinder igen"
	log.Printf("[%s] Google lukkede sessionen efter %s: forbinder igen med det samme", c.slug, ran.Round(time.Second))
	return true
}

// keepAlive serves one session (and its replacements) until it can no longer be kept running.
func (c *Camera) keepAlive(ctx context.Context, s *Session) error {
	c.out.activate(s)
	s.requestKeyframe()
	c.mu.Lock()
	c.state = "kører"
	c.runningSince = time.Now()
	c.sessionSince = time.Now()
	c.expiresAt = s.ExpiresAt
	c.mu.Unlock()
	c.logf("strøm kører (session %d, udløber %s)", s.ID, s.ExpiresAt.Local().Format("15:04:05"))

	for {
		renewAt := time.Until(s.ExpiresAt.Add(-60 * time.Second))
		if renewAt < 0 {
			renewAt = 0
		}
		timer := time.NewTimer(renewAt)
		check := time.NewTicker(5 * time.Second)
		var reason string
	wait:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				check.Stop()
				c.stopSession(s)
				return nil
			case <-s.dead:
				timer.Stop()
				check.Stop()
				c.stopSession(s)
				return s.deadErr
			case <-check.C:
				c.mu.Lock()
				healthy := !c.runningSince.IsZero() && time.Since(c.runningSince) > 10*time.Minute
				if healthy && c.failures > 0 {
					c.failures = 0 // ran fine for 10 minutes: back to the short backoff
				}
				c.mu.Unlock()
				if !c.wanted() {
					reason = "ingen seere"
					timer.Stop()
					break wait
				}
			case <-timer.C:
				reason = "renew"
				break wait
			}
		}
		check.Stop()
		if reason == "ingen seere" {
			c.logf("ingen seere i %s: lukker strømmen", c.idle)
			c.stopSession(s)
			c.setState("hviler (ingen seere)")
			return nil
		}

		// Time to renew. First try extend (cheapest: one call, no gap).
		c.mu.Lock()
		unsupported := c.extendUnsupported
		c.mu.Unlock()
		if !unsupported {
			ectx, cancel := context.WithDeadline(ctx, s.ExpiresAt.Add(-10*time.Second))
			err := c.extendSession(ectx, s)
			cancel()
			if err == nil {
				c.mu.Lock()
				c.extendsOK++
				c.extend400InRow = 0
				c.expiresAt = s.ExpiresAt
				c.mu.Unlock()
				continue
			}
			c.mu.Lock()
			c.extendsFailed++
			if isStatus(err, 400) {
				c.extend400InRow++
				if c.extend400InRow >= 2 && !c.extendUnsupported {
					c.extendUnsupported = true
					c.logf("forlængelse afvist to gange i træk (400): bruger ny session hvert 5. minut i stedet")
				}
			}
			c.mu.Unlock()
			if isStatus(err, 429) {
				// Out of quota: let this session run out, then pause. No new calls now.
				select {
				case <-s.dead:
				case <-time.After(time.Until(s.ExpiresAt)):
				case <-ctx.Done():
				}
				c.stopSession(s)
				return err
			}
		}

		// Extend failed or is not supported: open a new session while the old one still runs,
		// switch the output to it, then stop the old one. RTSP clients stay connected.
		nctx, cancel := context.WithDeadline(ctx, s.ExpiresAt.Add(30*time.Second))
		ns, err := c.openSession(nctx)
		cancel()
		if err != nil {
			c.stopSession(s)
			return err
		}
		c.out.activate(ns)
		ns.requestKeyframe()
		c.stopSession(s)
		s = ns
		c.mu.Lock()
		c.state = "kører"
		c.sessionSince = time.Now()
		c.expiresAt = s.ExpiresAt
		c.mu.Unlock()
		c.logf("skiftet til ny session %d (udløber %s)", s.ID, s.ExpiresAt.Local().Format("15:04:05"))
	}
}

// CameraStatus is what /health shows for one camera.
type CameraStatus struct {
	Name              string    `json:"name"`
	Slug              string    `json:"slug"`
	State             string    `json:"state"`
	GoogleName        string    `json:"google_name,omitempty"`
	DeviceFound       bool      `json:"device_found"`
	AlwaysOn          bool      `json:"always_on"`
	RTSPPort          int       `json:"rtsp_port"`
	Clients           int       `json:"clients"`
	LastFrame         time.Time `json:"last_frame,omitzero"`
	LastFrameAgeSec   *float64  `json:"last_frame_age_s,omitempty"`
	SessionExpires    time.Time `json:"session_expires,omitzero"`
	NextAllowedCall   time.Time `json:"next_allowed_call,omitzero"`
	BackoffLevel      int       `json:"backoff_level"`
	CallsLastMinute   int       `json:"calls_last_minute"`
	CallsLastHour     int       `json:"calls_last_hour"`
	SessionsStarted   int       `json:"sessions_started"`
	ExtendsOK         int       `json:"extends_ok"`
	ExtendsFailed     int       `json:"extends_failed"`
	ExtendUnsupported bool      `json:"extend_unsupported"`
	LastError         string    `json:"last_error,omitempty"`
	LastErrorAt       time.Time `json:"last_error_at,omitzero"`
}

func (c *Camera) status() CameraStatus {
	lastFrame, clients, _ := c.out.status()
	m, h := c.lim.CameraCounts(c.slug)
	c.mu.Lock()
	defer c.mu.Unlock()
	st := CameraStatus{
		Name: c.cfg.Name, Slug: c.slug, State: c.state, GoogleName: c.deviceName,
		DeviceFound: c.deviceID != "", AlwaysOn: c.alwaysOn, RTSPPort: c.cfg.Port,
		Clients: clients, LastFrame: lastFrame, SessionExpires: c.expiresAt,
		BackoffLevel: c.failures, CallsLastMinute: m, CallsLastHour: h,
		SessionsStarted: c.sessions, ExtendsOK: c.extendsOK, ExtendsFailed: c.extendsFailed,
		ExtendUnsupported: c.extendUnsupported, LastError: c.lastErr, LastErrorAt: c.lastErrAt,
	}
	if time.Now().Before(c.nextAllowed) {
		st.NextAllowedCall = c.nextAllowed
	}
	if !lastFrame.IsZero() {
		age := time.Since(lastFrame).Seconds()
		st.LastFrameAgeSec = &age
	}
	return st
}
