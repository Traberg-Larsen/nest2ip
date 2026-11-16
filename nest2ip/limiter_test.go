package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func newTestLimiter() (*Limiter, *fakeClock) {
	l := NewLimiter(8, 4, 60)
	c := &fakeClock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	l.now = c.now
	return l, c
}

func TestCameraMinuteLimit(t *testing.T) {
	l, c := newTestLimiter()
	for i := 0; i < 4; i++ {
		if w := l.tryLocked("have", PrioExtend); w != 0 {
			t.Fatalf("call %d should pass, wait %v", i, w)
		}
		c.t = c.t.Add(time.Second)
	}
	if w := l.tryLocked("have", PrioExtend); w == 0 {
		t.Fatal("5th call within a minute must be refused")
	}
	// another camera still has room
	if w := l.tryLocked("stue", PrioGenerate); w != 0 {
		t.Fatal("other camera should pass")
	}
	c.t = c.t.Add(57 * time.Second)
	if w := l.tryLocked("have", PrioExtend); w != 0 {
		t.Fatalf("after a minute the oldest call has expired, wait %v", w)
	}
}

func TestProjectReserveForExtends(t *testing.T) {
	l, _ := newTestLimiter()
	cams := []string{"a", "b", "c"}
	for i := 0; i < 6; i++ {
		if w := l.tryLocked(cams[i%3], PrioGenerate); w != 0 {
			t.Fatalf("generate %d should pass", i)
		}
	}
	if w := l.tryLocked("d", PrioGenerate); w == 0 {
		t.Fatal("7th generate must wait: 2 slots are reserved for extends")
	}
	if w := l.tryLocked("d", PrioExtend); w != 0 {
		t.Fatal("extend may use the reserved slots")
	}
	if w := l.tryLocked("e", PrioExtend); w != 0 {
		t.Fatal("8th call (extend) should pass")
	}
	if w := l.tryLocked("f", PrioExtend); w == 0 {
		t.Fatal("9th call must be refused")
	}
}

func TestCameraHourLimit(t *testing.T) {
	l, c := newTestLimiter()
	for i := 0; i < 60; i++ {
		if w := l.tryLocked("have", PrioExtend); w != 0 {
			t.Fatalf("call %d should pass", i)
		}
		c.t = c.t.Add(59 * time.Second)
	}
	w := l.tryLocked("have", PrioExtend)
	if w == 0 {
		t.Fatal("61st call within the hour must be refused")
	}
}

func TestStopNeverWaits(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 4; i++ {
		_ = l.tryLocked("have", PrioExtend)
	}
	if err := l.Acquire(context.Background(), "have", PrioStop); err != ErrNoBudget {
		t.Fatalf("stop without budget must fail fast, got %v", err)
	}
}

func TestPauseAfter429(t *testing.T) {
	l, c := newTestLimiter()
	l.Pause(c.t.Add(5 * time.Minute))
	if w := l.tryLocked("have", PrioExtend); w < 4*time.Minute {
		t.Fatalf("paused project must wait ~5 min, got %v", w)
	}
}

func TestBackoffLadder(t *testing.T) {
	want := []time.Duration{0, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := backoffFor(i); got != w {
			t.Fatalf("failures=%d: got %v want %v", i, got, w)
		}
	}
}

func TestSlugify(t *testing.T) {
	if s := slugify("Indkørsel"); s != "indkorsel" {
		t.Fatal(s)
	}
	if s := slugify(" Have "); s != "have" {
		t.Fatal(s)
	}
}

func TestHACredentials(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".storage"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".storage", "core.config_entries"), []byte(`{"data":{"entries":[
	 {"domain":"hue","data":{"token":"a-string-token"}},
	 {"domain":"nest","disabled_by":null,"data":{"auth_implementation":"nest_x","project_id":"proj-1","token":{"refresh_token":"rt"}}}]}}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".storage", "application_credentials"), []byte(`{"data":{"items":[
	 {"id":"other","domain":"nest","auth_domain":"other","client_id":"wrong","client_secret":"wrong"},
	 {"id":"nest_x","domain":"nest","auth_domain":"nest_x","client_id":"cid","client_secret":"sec"}]}}`), 0o600)
	cr, err := loadHACredentials(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if cr.ProjectID != "proj-1" || cr.RefreshToken != "rt" || cr.ClientID != "cid" || cr.ClientSecret != "sec" {
		t.Fatalf("unexpected %+v", cr)
	}
	if _, err := loadHACredentials(dir, "another-project"); err == nil {
		t.Fatal("must not use a Nest entry from another project")
	}
}

func TestMediaBackoffLadder(t *testing.T) {
	want := []time.Duration{0, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := mediaBackoffFor(i); got != w {
			t.Fatalf("failures=%d: got %v want %v", i, got, w)
		}
	}
	if !isMediaErr(errStalled) || isMediaErr(&APIError{Status: 429}) {
		t.Fatal("isMediaErr classifies wrongly")
	}
}
