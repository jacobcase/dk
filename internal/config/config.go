// Package config loads and persists dk's on-disk configuration.
//
// Values resolve with the precedence: explicit flags (applied by the caller) >
// environment variables > config file > built-in defaults. Secrets may live in
// the config file, so it is always written with 0600 permissions.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcase/dk/internal/atomicfile"
)

// Environment names the DigiKey deployment a command talks to.
const (
	EnvProduction = "production"
	EnvSandbox    = "sandbox"
)

// Base URLs for the two DigiKey environments. Both OAuth and the REST APIs are
// served from these hosts.
const (
	ProductionBaseURL = "https://api.digikey.com"
	SandboxBaseURL    = "https://sandbox-api.digikey.com"
)

// DefaultRedirectURI is the OAuth callback dk listens on during `dk auth
// login`. DigiKey requires redirect URIs to use TLS, so even the loopback
// listener is HTTPS (served with a self-signed certificate).
const DefaultRedirectURI = "https://localhost:8139/digikey_callback"

// Locale controls the DigiKey site, language, and currency used for pricing and
// availability. It maps onto the X-DIGIKEY-Locale-* request headers.
type Locale struct {
	Site     string `json:"site,omitempty"`
	Language string `json:"language,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// Config is the full set of persisted settings.
type Config struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Environment  string `json:"environment,omitempty"`
	RedirectURI  string `json:"redirect_uri,omitempty"`
	// AccountID populates X-DIGIKEY-Account-Id, which selects between multiple
	// DigiKey accounts tied to one login. Optional for most users.
	AccountID string `json:"account_id,omitempty"`
	Locale    Locale `json:"locale,omitempty"`
	// APIBaseURL overrides the environment-derived host. It exists for pointing
	// dk at a mock server during testing; leave it empty in normal use.
	APIBaseURL string `json:"api_base_url,omitempty"`
}

// Default returns a Config populated with built-in defaults only.
func Default() Config {
	return Config{
		Environment: EnvProduction,
		RedirectURI: DefaultRedirectURI,
		Locale: Locale{
			Site:     "US",
			Language: "en",
			Currency: "USD",
		},
	}
}

// BaseURL returns the API host for the configured environment, or the explicit
// override if one is set.
func (c Config) BaseURL() string {
	if c.APIBaseURL != "" {
		return strings.TrimSuffix(c.APIBaseURL, "/")
	}
	if strings.EqualFold(c.Environment, EnvSandbox) {
		return SandboxBaseURL
	}
	return ProductionBaseURL
}

// Validate reports whether the config can be used to talk to DigiKey at all.
func (c Config) Validate() error {
	var missing []string
	if c.ClientID == "" {
		missing = append(missing, "client_id")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "client_secret")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing credentials: %s", strings.Join(missing, ", "))
	}
	if c.Environment != "" && !strings.EqualFold(c.Environment, EnvProduction) && !strings.EqualFold(c.Environment, EnvSandbox) {
		return fmt.Errorf("invalid environment %q: want %q or %q", c.Environment, EnvProduction, EnvSandbox)
	}
	return nil
}

// Dir returns the directory holding config.json and token.json. DK_CONFIG_DIR
// overrides it, which is what the tests (and throwaway sandboxes) use.
func Dir() (string, error) {
	if dir := os.Getenv("DK_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, "dk"), nil
}

// Path returns the full path to config.json.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads config.json (if present), overlays environment variables, and
// fills in defaults. A missing file is not an error: dk is fully usable with
// environment variables alone.
func Load() (Config, error) {
	cfg, err := LoadFile()
	if err != nil {
		return Default(), err
	}
	return applyEnv(cfg), nil
}

// LoadFile reads config.json over the built-in defaults, without applying
// environment variables. `dk config set` uses it so that a secret passed only
// through the environment is never written to disk as a side effect.
func LoadFile() (Config, error) {
	cfg := Default()

	path, err := Path()
	if err != nil {
		return cfg, err
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var fileCfg Config
		if err := json.Unmarshal(data, &fileCfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
		return merge(cfg, fileCfg), nil
	case errors.Is(err, fs.ErrNotExist):
		// No config file; defaults are enough.
		return cfg, nil
	default:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
}

// applyEnv overlays DIGIKEY_* environment variables onto cfg.
func applyEnv(cfg Config) Config {
	setString(&cfg.ClientID, "DIGIKEY_CLIENT_ID")
	setString(&cfg.ClientSecret, "DIGIKEY_CLIENT_SECRET")
	setString(&cfg.Environment, "DIGIKEY_ENV")
	setString(&cfg.RedirectURI, "DIGIKEY_REDIRECT_URI")
	setString(&cfg.AccountID, "DIGIKEY_ACCOUNT_ID")
	setString(&cfg.Locale.Site, "DIGIKEY_LOCALE_SITE")
	setString(&cfg.Locale.Language, "DIGIKEY_LOCALE_LANGUAGE")
	setString(&cfg.Locale.Currency, "DIGIKEY_LOCALE_CURRENCY")
	setString(&cfg.APIBaseURL, "DIGIKEY_API_BASE_URL")
	return cfg
}

func setString(dst *string, env string) {
	if v := os.Getenv(env); v != "" {
		*dst = v
	}
}

// merge overlays non-empty fields of over onto base.
func merge(base, over Config) Config {
	setIfNotEmpty(&base.ClientID, over.ClientID)
	setIfNotEmpty(&base.ClientSecret, over.ClientSecret)
	setIfNotEmpty(&base.Environment, over.Environment)
	setIfNotEmpty(&base.RedirectURI, over.RedirectURI)
	setIfNotEmpty(&base.AccountID, over.AccountID)
	setIfNotEmpty(&base.Locale.Site, over.Locale.Site)
	setIfNotEmpty(&base.Locale.Language, over.Locale.Language)
	setIfNotEmpty(&base.Locale.Currency, over.Locale.Currency)
	setIfNotEmpty(&base.APIBaseURL, over.APIBaseURL)
	return base
}

func setIfNotEmpty(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// Save writes cfg to config.json with 0600 permissions, creating the directory
// if needed. The write is atomic so a crash cannot truncate an existing config.
func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	return atomicfile.Write(path, data, 0o600)
}

// Set assigns a config field by its dotted key name, as used by `dk config set`.
func (c *Config) Set(key, value string) error {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "client_id":
		c.ClientID = value
	case "client_secret":
		c.ClientSecret = value
	case "environment", "env":
		if !strings.EqualFold(value, EnvProduction) && !strings.EqualFold(value, EnvSandbox) {
			return fmt.Errorf("environment must be %q or %q", EnvProduction, EnvSandbox)
		}
		c.Environment = strings.ToLower(value)
	case "redirect_uri":
		c.RedirectURI = value
	case "account_id":
		c.AccountID = value
	case "locale.site", "site":
		c.Locale.Site = value
	case "locale.language", "language":
		c.Locale.Language = value
	case "locale.currency", "currency":
		c.Locale.Currency = value
	default:
		return fmt.Errorf("unknown config key %q (valid: %s)", key, strings.Join(Keys(), ", "))
	}
	return nil
}

// Keys lists the settable config keys, in the order `dk config show` prints them.
func Keys() []string {
	return []string{
		"client_id",
		"client_secret",
		"environment",
		"redirect_uri",
		"account_id",
		"locale.site",
		"locale.language",
		"locale.currency",
	}
}

// Redacted returns a key/value view of the config with the client secret masked,
// safe to print or serialize to stdout.
func (c Config) Redacted() map[string]string {
	return map[string]string{
		"client_id":       c.ClientID,
		"client_secret":   mask(c.ClientSecret),
		"environment":     c.Environment,
		"redirect_uri":    c.RedirectURI,
		"account_id":      c.AccountID,
		"locale.site":     c.Locale.Site,
		"locale.language": c.Locale.Language,
		"locale.currency": c.Locale.Currency,
	}
}

// mask reduces a secret to a length hint plus its last four characters, which is
// enough to tell two credentials apart without disclosing either.
func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}
