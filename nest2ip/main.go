package main

import (
	"context"
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

// findDevices maps camera names to Google device IDs. It retries with backoff on errors.
func (app *App) findDevices(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		devs, err := app.g.ListDevices(ctx)
		if err == nil {
			app.matchDevices(devs)
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
		wait := backoffFor(failures)
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
