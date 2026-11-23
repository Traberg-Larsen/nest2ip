package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

type App struct {
	cfg       *Config
	creds     Credentials
	lim       *Limiter
	g         *Google
	cams      []*Camera
	startErr  string
	startedAt time.Time
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	log.Printf("nest2ip %s starter", version)

	cfg, err := loadConfig(env("NEST2IP_OPTIONS", "/data/options.json"))
	if err != nil {
		log.Fatalf("konfiguration: %v", err)
	}
	app := &App{cfg: cfg, startedAt: time.Now()}
	app.lim = NewLimiter(cfg.ProjectPerMinute, cfg.CameraPerMinute, cfg.CameraPerHour)
	log.Printf("kvote: højst %d kald/min for projektet, %d/min og %d/time pr. kamera",
		cfg.ProjectPerMinute, cfg.CameraPerMinute, cfg.CameraPerHour)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	creds, err := resolveCredentials(cfg, env("NEST2IP_HA_CONFIG", "/homeassistant"))
	if err != nil {
		app.startErr = err.Error()
		log.Printf("Google-login: %v", err)
	} else {
		log.Printf("Google-login hentet fra %s (projekt %s)", creds.Source, creds.ProjectID)
	}
	app.creds = creds
	app.g = NewGoogle(creds, app.lim)
	if err == nil && creds.Source != "indstillinger" {
		// Google's refresh token can expire or be revoked. When the Nest integration is
		// signed in again, Home Assistant holds the new one; pick it up without a restart.
		haDir := env("NEST2IP_HA_CONFIG", "/homeassistant")
		app.g.SetReload(func() (Credentials, error) { return resolveCredentials(cfg, haDir) })
	}

	idle := time.Duration(cfg.IdleTimeoutMinutes) * time.Minute
	for i, cc := range cfg.Cameras {
		cam := newCamera(cc, app.g, app.lim, idle)
		out, err := newOutput(cam, cc.Port, 50000+2*i)
		if err != nil {
			log.Fatalf("[%s] %v", cam.slug, err)
		}
		cam.out = out
		app.cams = append(app.cams, cam)
		log.Printf("[%s] RTSP klar på port %d (altid tændt: %v)", cam.slug, cc.Port, cam.alwaysOn)
	}

	go serveHTTP(ctx, app)

	if err == nil {
		app.loadDeviceCache()
		go app.findDevices(ctx)
	}
	for _, cam := range app.cams {
		go cam.run(ctx)
	}

	<-ctx.Done()
	log.Printf("stopper")
	time.Sleep(2 * time.Second) // let cameras send their polite Stop calls
	for _, cam := range app.cams {
		cam.out.close()
	}
}

// deviceCache remembers camera -> Google device ID, so a restart can start streams at once
// even when Google's device list is unavailable (it answered 503 for minutes at a time).
type deviceCache map[string]struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (app *App) cachePath() string { return env("NEST2IP_DEVICE_CACHE", "/data/devices.json") }

func (app *App) loadDeviceCache() {
	b, err := os.ReadFile(app.cachePath())
	if err != nil {
		return
	}
	var dc deviceCache
	if json.Unmarshal(b, &dc) != nil {
		return
	}
	for _, c := range app.cams {
		if d, ok := dc[c.slug]; ok && d.ID != "" && (c.cfg.DeviceID == "" || c.cfg.DeviceID == d.ID) {
			c.mu.Lock()
			c.deviceID, c.deviceName = d.ID, d.Name
			c.mu.Unlock()
			c.logf("bruger gemt kamera-ID fra sidste kørsel (%q)", d.Name)
		}
	}
}

func (app *App) saveDeviceCache() {
	dc := deviceCache{}
	for _, c := range app.cams {
		c.mu.Lock()
		if c.deviceID != "" {
			dc[c.slug] = struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{c.deviceID, c.deviceName}
		}
		c.mu.Unlock()
	}
	b, _ := json.MarshalIndent(dc, "", "  ")
	if err := os.WriteFile(app.cachePath(), b, 0o600); err != nil {
		log.Printf("kunne ikke gemme kamera-ID'er: %v", err)
	}
}

// findDevices maps camera names to Google device IDs. It retries on errors (30 s, 1, 2, 5, 10 min);
// cameras already known from the cache keep running meanwhile.
func (app *App) findDevices(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		devs, err := app.g.ListDevices(ctx)
		if err == nil {
			app.matchDevices(devs)
			app.saveDeviceCache()
			missing := 0
			for _, c := range app.cams {
				c.mu.Lock()
				if c.deviceID == "" {
					missing++
				}
				c.mu.Unlock()
				c.poke()
			}
			if missing == 0 {
				app.startErr = ""
				return
			}
			app.startErr = "et eller flere kameraer blev ikke fundet hos Google; tjek navnene"
			err = errors.New(app.startErr)
		} else {
			app.startErr = "kunne ikke hente kameraer fra Google: " + err.Error()
		}
		failures++
		wait := mediaBackoffFor(failures)
		if failures > len(mediaBackoffLadder) {
			wait = 10 * time.Minute
		}
		log.Printf("kameraliste: %v. Prøver igen om %s", err, wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

func (app *App) matchDevices(devs []Device) {
	var names []string
	for _, d := range devs {
		names = append(names, d.DisplayName()+" ("+strings.TrimPrefix(d.Type, "sdm.devices.types.")+", "+strings.Join(d.Protocols(), "/")+")")
	}
	log.Printf("Google kender %d enheder: %s", len(devs), strings.Join(names, ", "))
	for _, c := range app.cams {
		for _, d := range devs {
			match := (c.cfg.DeviceID != "" && (d.ID() == c.cfg.DeviceID || d.Name == c.cfg.DeviceID)) ||
				(c.cfg.DeviceID == "" && slugify(d.DisplayName()) == c.slug)
			if !match {
				continue
			}
			protos := d.Protocols()
			webrtcOK := false
			for _, p := range protos {
				if p == "WEB_RTC" {
					webrtcOK = true
				}
			}
			c.mu.Lock()
			if webrtcOK {
				c.deviceID = d.ID()
				c.deviceName = d.DisplayName()
			} else {
				c.state = "kameraet understøtter ikke WebRTC (" + strings.Join(protos, "/") + ")"
			}
			c.mu.Unlock()
			if webrtcOK {
				c.logf("fundet hos Google som %q", d.DisplayName())
			} else {
				c.logf("fundet, men understøtter kun %v; kun WebRTC-kameraer er med i denne version", protos)
			}
			break
		}
	}
}
