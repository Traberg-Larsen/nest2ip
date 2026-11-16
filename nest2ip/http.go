package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed status.html
var statusHTML []byte

type health struct {
	Version   string         `json:"version"`
	Uptime    string         `json:"uptime"`
	OK        bool           `json:"ok"`
	Problem   string         `json:"problem,omitempty"`
	ProjectID string         `json:"project_id,omitempty"`
	Login     string         `json:"login_source,omitempty"`
	Quota     LimiterStats   `json:"quota"`
	Cameras   []CameraStatus `json:"cameras"`
}

func (app *App) health() health {
	h := health{
		Version: version, Uptime: time.Since(app.startedAt).Round(time.Second).String(),
		Problem: app.startErr, ProjectID: app.creds.ProjectID, Login: app.creds.Source,
		Quota: app.lim.Stats(),
	}
	h.OK = h.Problem == ""
	for _, c := range app.cams {
		h.Cameras = append(h.Cameras, c.status())
	}
	return h
}

// fromIngress is true for requests that came through Home Assistant's ingress (already authenticated).
func fromIngress(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host == "172.30.32.2" && r.Header.Get("X-Ingress-Path") != ""
}

func serveHTTP(ctx context.Context, app *App) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(statusHTML)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(app.health())
	})
	mux.HandleFunc("GET /snapshot/{file}", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimSuffix(r.PathValue("file"), ".jpg")
		var cam *Camera
		for _, c := range app.cams {
			if c.slug == slug {
				cam = c
			}
		}
		if cam == nil {
			http.NotFound(w, r)
			return
		}
		if *app.cfg.SnapshotKeyRequired && !fromIngress(r) {
			key := r.URL.Query().Get("key")
			if subtle.ConstantTimeCompare([]byte(key), []byte(cam.cfg.Path)) != 1 {
				http.Error(w, "kræver ?key=<kameraets RTSP-sti>", http.StatusForbidden)
				return
			}
		}
		jpg, at, err := cam.out.Snapshot(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Age", fmt.Sprintf("%.1f", time.Since(at).Seconds()))
		_, _ = w.Write(jpg)
	})

	srv := &http.Server{Addr: fmt.Sprintf(":%d", app.cfg.HTTPPort), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Printf("status og /health på port %d", app.cfg.HTTPPort)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP: %v", err)
	}
}
