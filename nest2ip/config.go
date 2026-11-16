package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CameraConfig is one camera in the add-on options.
type CameraConfig struct {
	Name      string `json:"name"`
	DeviceID  string `json:"device_id"`
	Port      int    `json:"port"`
	Path      string `json:"path"`
	MutedPath string `json:"muted_path"`
	AlwaysOn  *bool  `json:"always_on"`
}

// Config is the add-on options (/data/options.json).
type Config struct {
	ProjectID           string         `json:"project_id"`
	ClientID            string         `json:"client_id"`
	ClientSecret        string         `json:"client_secret"`
	RefreshToken        string         `json:"refresh_token"`
	UseHomeAssistant    bool           `json:"use_home_assistant_login"`
	HTTPPort            int            `json:"http_port"`
	IdleTimeoutMinutes  int            `json:"idle_timeout_minutes"`
	SnapshotKeyRequired *bool          `json:"snapshot_key_required"`
	Cameras             []CameraConfig `json:"cameras"`

	// Rate limits. Defaults are set below; they are deliberately below Google's sandbox limits.
	ProjectPerMinute int `json:"project_calls_per_minute"`
	CameraPerMinute  int `json:"camera_calls_per_minute"`
	CameraPerHour    int `json:"camera_calls_per_hour"`
}

func (c *Config) applyDefaults() {
	if c.HTTPPort == 0 {
		c.HTTPPort = 8787
	}
	if c.IdleTimeoutMinutes == 0 {
		c.IdleTimeoutMinutes = 10
	}
	if c.ProjectPerMinute == 0 {
		c.ProjectPerMinute = 8
	}
	if c.CameraPerMinute == 0 {
		c.CameraPerMinute = 4
	}
	if c.CameraPerHour == 0 {
		c.CameraPerHour = 60
	}
	if c.SnapshotKeyRequired == nil {
		t := true
		c.SnapshotKeyRequired = &t
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns "Indkørsel" into "indkorsel".
func slugify(s string) string {
	r := strings.NewReplacer("æ", "ae", "ø", "o", "å", "aa", "Æ", "ae", "Ø", "o", "Å", "aa")
	s = strings.ToLower(r.Replace(s))
	return strings.Trim(slugRe.ReplaceAllString(s, "_"), "_")
}

func (c *Config) validate() error {
	if len(c.Cameras) == 0 {
		return errors.New("ingen kameraer i konfigurationen (cameras)")
	}
	ports := map[int]string{}
	slugs := map[string]bool{}
	for i := range c.Cameras {
		cam := &c.Cameras[i]
		cam.Path = strings.Trim(cam.Path, "/")
		cam.MutedPath = strings.Trim(cam.MutedPath, "/")
		if cam.Name == "" {
			return fmt.Errorf("kamera %d mangler name", i+1)
		}
		if cam.Port <= 0 || cam.Port > 65535 {
			return fmt.Errorf("kamera %q har ugyldig port", cam.Name)
		}
		if other, ok := ports[cam.Port]; ok {
			return fmt.Errorf("kamera %q og %q bruger samme port %d", cam.Name, other, cam.Port)
		}
		ports[cam.Port] = cam.Name
		if cam.Path == "" {
			return fmt.Errorf("kamera %q mangler path", cam.Name)
		}
		if cam.MutedPath == cam.Path {
			return fmt.Errorf("kamera %q: muted_path må ikke være lig path", cam.Name)
		}
		s := slugify(cam.Name)
		if slugs[s] {
			return fmt.Errorf("to kameraer hedder det samme: %q", cam.Name)
		}
		slugs[s] = true
	}
	return nil
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("kan ikke læse %s: %w", path, err)
	}
	c.applyDefaults()
	return &c, c.validate()
}

// Credentials are what we need to talk to Google.
type Credentials struct {
	ProjectID    string
	ClientID     string
	ClientSecret string
	RefreshToken string
	Source       string
}

func (c Credentials) complete() bool {
	return c.ProjectID != "" && c.ClientID != "" && c.ClientSecret != "" && c.RefreshToken != ""
}

// resolveCredentials uses the options first and fills gaps from Home Assistant's own
// Nest login when allowed. Secrets are never logged.
func resolveCredentials(cfg *Config, haConfigDir string) (Credentials, error) {
	cr := Credentials{
		ProjectID:    strings.TrimSpace(cfg.ProjectID),
		ClientID:     strings.TrimSpace(cfg.ClientID),
		ClientSecret: strings.TrimSpace(cfg.ClientSecret),
		RefreshToken: strings.TrimSpace(cfg.RefreshToken),
		Source:       "indstillinger",
	}
	if cr.complete() || !cfg.UseHomeAssistant {
		if !cr.complete() {
			return cr, errors.New("mangler project_id, client_id, client_secret eller refresh_token (eller slå use_home_assistant_login til)")
		}
		return cr, nil
	}
	ha, err := loadHACredentials(haConfigDir, cr.ProjectID)
	if err != nil {
		return cr, fmt.Errorf("kunne ikke hente Home Assistants Nest-login: %w", err)
	}
	if cr.ProjectID == "" {
		cr.ProjectID = ha.ProjectID
	}
	if cr.ClientID == "" {
		cr.ClientID = ha.ClientID
	}
	if cr.ClientSecret == "" {
		cr.ClientSecret = ha.ClientSecret
	}
	if cr.RefreshToken == "" {
		cr.RefreshToken = ha.RefreshToken
	}
	cr.Source = "Home Assistants Nest-integration"
	if !cr.complete() {
		return cr, errors.New("Home Assistants Nest-login er ufuldstændig")
	}
	return cr, nil
}

type haEntries struct {
	Data struct {
		Entries []struct {
			Domain   string          `json:"domain"`
			Disabled *string         `json:"disabled_by"`
			Data     json.RawMessage `json:"data"`
		} `json:"entries"`
	} `json:"data"`
}

// Other integrations store all kinds of shapes in "data", so it is only parsed for Nest entries.
type haNestData struct {
	AuthImplementation string `json:"auth_implementation"`
	ProjectID          string `json:"project_id"`
	Token              struct {
		RefreshToken string `json:"refresh_token"`
	} `json:"token"`
}

type haAppCreds struct {
	Data struct {
		Items []struct {
			ID           string `json:"id"`
			Domain       string `json:"domain"`
			AuthDomain   string `json:"auth_domain"`
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"items"`
	} `json:"data"`
}

func loadHACredentials(dir, wantProject string) (Credentials, error) {
	var out Credentials
	b, err := os.ReadFile(filepath.Join(dir, ".storage", "core.config_entries"))
	if err != nil {
		return out, err
	}
	var ents haEntries
	if err := json.Unmarshal(b, &ents); err != nil {
		return out, err
	}
	authImpl := ""
	found := false
	for _, e := range ents.Data.Entries {
		if e.Domain != "nest" || e.Disabled != nil {
			continue
		}
		var nd haNestData
		if err := json.Unmarshal(e.Data, &nd); err != nil {
			continue
		}
		if wantProject != "" && nd.ProjectID != "" && nd.ProjectID != wantProject {
			continue
		}
		out.ProjectID = nd.ProjectID
		out.RefreshToken = nd.Token.RefreshToken
		authImpl = nd.AuthImplementation
		found = true
		break
	}
	if !found {
		return out, errors.New("ingen aktiv Nest-integration fundet i Home Assistant")
	}
	b, err = os.ReadFile(filepath.Join(dir, ".storage", "application_credentials"))
	if err != nil {
		return out, err
	}
	var ac haAppCreds
	if err := json.Unmarshal(b, &ac); err != nil {
		return out, err
	}
	var candidates int
	for _, it := range ac.Data.Items {
		if it.Domain != "nest" {
			continue
		}
		candidates++
		if it.ID == authImpl || it.AuthDomain == authImpl || candidates == 1 {
			out.ClientID = it.ClientID
			out.ClientSecret = it.ClientSecret
			if it.ID == authImpl || it.AuthDomain == authImpl {
				break
			}
		}
	}
	if out.ClientID == "" {
		return out, errors.New("ingen Nest-applikationsoplysninger fundet i Home Assistant")
	}
	return out, nil
}
