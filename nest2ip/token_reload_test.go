package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeTokenServer accepts only the refresh token "new" and answers invalid_grant otherwise,
// like Google does for an expired or revoked token.
func fakeTokenServer(t *testing.T) *int {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "new" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"access-new","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	old := tokenURL
	tokenURL = srv.URL
	t.Cleanup(func() { tokenURL = old })
	return &calls
}

func writeHANest(t *testing.T, dir, refresh string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Join(dir, ".storage"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".storage", "core.config_entries"), []byte(`{"data":{"entries":[
	 {"domain":"nest","disabled_by":null,"data":{"auth_implementation":"nest_x","project_id":"proj-1","token":{"refresh_token":"`+refresh+`"}}}]}}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".storage", "application_credentials"), []byte(`{"data":{"items":[
	 {"id":"nest_x","domain":"nest","auth_domain":"nest_x","client_id":"cid","client_secret":"sec"}]}}`), 0o600)
}

func TestTokenPicksUpNewHALogin(t *testing.T) {
	calls := fakeTokenServer(t)
	dir := t.TempDir()
	writeHANest(t, dir, "old")
	cfg := &Config{UseHomeAssistant: true}
	creds, err := resolveCredentials(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGoogle(creds, nil)
	g.SetReload(func() (Credentials, error) { return resolveCredentials(cfg, dir) })

	// Home Assistant has not been signed in again yet: the old token keeps failing.
	if _, err := g.token(context.Background()); !isStatus(err, 400) {
		t.Fatalf("want 400 while the login is unchanged, got %v", err)
	}
	if *calls != 1 {
		t.Fatalf("an unchanged login must not be retried, got %d token calls", *calls)
	}

	// The user signs the Nest integration in again; HA stores a new refresh token.
	writeHANest(t, dir, "new")
	tok, err := g.token(context.Background())
	if err != nil || tok != "access-new" {
		t.Fatalf("want the new login to work, got %q, %v", tok, err)
	}
	if g.projectID() != "proj-1" {
		t.Fatalf("project ID lost: %q", g.projectID())
	}
}

func TestTokenWithoutReloadReturnsError(t *testing.T) {
	fakeTokenServer(t)
	g := NewGoogle(Credentials{ProjectID: "p", ClientID: "c", ClientSecret: "s", RefreshToken: "old"}, nil)
	if _, err := g.token(context.Background()); !isStatus(err, 400) {
		t.Fatalf("want 400, got %v", err)
	}
}
