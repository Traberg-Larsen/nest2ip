package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tokenURL = "https://oauth2.googleapis.com/token"
	sdmBase  = "https://smartdevicemanagement.googleapis.com/v1"
)

// APIError is a non-200 answer from Google.
type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Google svarede %d: %s", e.Status, e.Body)
}

func isStatus(err error, code int) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Status == code
}

// Google talks to OAuth and the SDM API. Every executeCommand goes through the limiter.
type Google struct {
	creds   Credentials
	limiter *Limiter
	httpc   *http.Client

	mu      sync.Mutex
	access  string
	expires time.Time
}

func NewGoogle(creds Credentials, lim *Limiter) *Google {
	return &Google{creds: creds, limiter: lim, httpc: &http.Client{Timeout: 30 * time.Second}}
}

func (g *Google) token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.access != "" && time.Until(g.expires) > 2*time.Minute {
		return g.access, nil
	}
	form := url.Values{
		"client_id":     {g.creds.ClientID},
		"client_secret": {g.creds.ClientSecret},
		"refresh_token": {g.creds.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		// The body never contains our secrets, only Google's error code.
		return "", &APIError{Status: resp.StatusCode, Body: "token: " + truncate(string(body), 200)}
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("uventet token-svar fra Google")
	}
	g.access = tr.AccessToken
	g.expires = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return g.access, nil
}

func (g *Google) do(ctx context.Context, method, url string, payload any) ([]byte, error) {
	tok, err := g.token(ctx)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 200 {
		return body, nil
	}
	ae := &APIError{Status: resp.StatusCode, Body: truncate(strings.Join(strings.Fields(string(body)), " "), 300)}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil {
			ae.RetryAfter = time.Duration(s) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			ae.RetryAfter = time.Until(t)
		}
	}
	if resp.StatusCode == 401 {
		g.mu.Lock()
		g.access = "" // force a fresh token next time
		g.mu.Unlock()
	}
	return nil, ae
}

// Device is the part of an SDM device we use.
type Device struct {
	Name   string                     `json:"name"`
	Type   string                     `json:"type"`
	Traits map[string]json.RawMessage `json:"traits"`
	Parent []struct {
		DisplayName string `json:"displayName"`
	} `json:"parentRelations"`
}

func (d Device) ID() string {
	return d.Name[strings.LastIndex(d.Name, "/")+1:]
}

func (d Device) DisplayName() string {
	var info struct {
		CustomName string `json:"customName"`
	}
	if raw, ok := d.Traits["sdm.devices.traits.Info"]; ok {
		_ = json.Unmarshal(raw, &info)
	}
	if info.CustomName != "" {
		return info.CustomName
	}
	if len(d.Parent) > 0 {
		return d.Parent[0].DisplayName
	}
	return d.ID()
}

func (d Device) Protocols() []string {
	var ls struct {
		SupportedProtocols []string `json:"supportedProtocols"`
	}
	if raw, ok := d.Traits["sdm.devices.traits.CameraLiveStream"]; ok {
		_ = json.Unmarshal(raw, &ls)
	}
	return ls.SupportedProtocols
}

// ListDevices is not an executeCommand call and has its own (5/min) limit; we call it rarely.
func (g *Google) ListDevices(ctx context.Context) ([]Device, error) {
	body, err := g.do(ctx, "GET", fmt.Sprintf("%s/enterprises/%s/devices", sdmBase, g.creds.ProjectID), nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Devices, nil
}

// Execute runs one executeCommand through the limiter.
func (g *Google) Execute(ctx context.Context, cam, deviceID, command string, params any, prio Priority) (json.RawMessage, error) {
	if err := g.limiter.Acquire(ctx, cam, prio); err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/enterprises/%s/devices/%s:executeCommand", sdmBase, g.creds.ProjectID, deviceID)
	body, err := g.do(ctx, "POST", u, map[string]any{"command": command, "params": params})
	short := command[strings.LastIndex(command, ".")+1:]
	if err != nil {
		if isStatus(err, 429) {
			ra := err.(*APIError).RetryAfter
			if ra < time.Minute {
				ra = time.Minute
			}
			g.limiter.Pause(time.Now().Add(ra))
		}
		log.Printf("[%s] %s fejlede: %v", cam, short, err)
		return nil, err
	}
	var r struct {
		Results json.RawMessage `json:"results"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Results, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
