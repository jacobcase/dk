// Package config loads and persists dk's on-disk configuration.
//
// The configuration is split across two kinds of file:
//
//   - config.json holds the settings that are the same whichever DigiKey
//     deployment you are talking to: the locale, the response-cache window, and
//     the pointer naming which environment is currently active.
//   - credentials-<environment>.json holds one registered app: its client id
//     and secret, the redirect URI registered with it, and the account id it
//     acts as. DigiKey scopes a client id to a single deployment — a production
//     client id is rejected by the sandbox host — so these cannot be shared,
//     and each environment gets its own file.
//
// Callers see the two merged into a single flat Config. Which credentials file
// is read is decided entirely by the active environment; there is no per-run
// override, so what `dk env` reports is always what the next command will use.
//
// Values resolve with the precedence: explicit flags (applied by the caller) >
// environment variables > the files above > built-in defaults. Secrets live in
// the credentials file, so it is always written with 0600 permissions.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jacobcase/dk/internal/atomicfile"
)

// Environment names the DigiKey deployment a command talks to.
const (
	EnvProduction = "production"
	EnvSandbox    = "sandbox"
)

// Environments lists every environment dk knows about, in display order.
func Environments() []string { return []string{EnvProduction, EnvSandbox} }

// ParseEnvironment normalizes an environment name as typed by a human. It
// accepts the short forms so that `dk env prod` and `dk env sbx` work.
func ParseEnvironment(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "production", "prod", "prd":
		return EnvProduction, nil
	case "sandbox", "sand", "sbx":
		return EnvSandbox, nil
	default:
		return "", fmt.Errorf("unknown environment %q: want %q or %q", v, EnvProduction, EnvSandbox)
	}
}

// environmentOrDefault normalizes an environment name, treating the empty
// string as the default rather than an error. A Config built in code rather
// than loaded may leave the field unset, and that should mean the same thing
// Default() means by it, not a failure to save.
func environmentOrDefault(v string) (string, error) {
	if strings.TrimSpace(v) == "" {
		return EnvProduction, nil
	}
	return ParseEnvironment(v)
}

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

// Config is the full set of settings, flattened from the shared file and the
// active environment's credentials file.
type Config struct {
	// Environment selects both the API host and which credentials file the
	// fields below were read from. It is persisted in config.json and changed
	// with `dk env`.
	Environment string `json:"environment,omitempty"`

	// Per-environment: these come from credentials-<Environment>.json.
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	RedirectURI  string `json:"redirect_uri,omitempty"`
	// AccountID populates X-DIGIKEY-Account-Id, which selects between multiple
	// DigiKey accounts tied to one login. Optional for most users.
	AccountID string `json:"account_id,omitempty"`

	// Shared: these come from config.json.
	Locale Locale `json:"locale,omitempty"`
	// CacheTTL is how long a cached API response stays fresh, as a Go duration
	// ("10m", "30s"). "0" disables the response cache. It is stored as text
	// rather than a duration so that the same merge and environment-overlay
	// rules that cover every other setting apply unchanged.
	CacheTTL string `json:"cache_ttl,omitempty"`
	// APIBaseURL overrides the environment-derived host. It exists for pointing
	// dk at a mock server during testing; leave it empty in normal use.
	APIBaseURL string `json:"api_base_url,omitempty"`
}

// sharedFile is the on-disk shape of config.json.
type sharedFile struct {
	Environment string `json:"environment,omitempty"`
	Locale      Locale `json:"locale,omitempty"`
	CacheTTL    string `json:"cache_ttl,omitempty"`
	APIBaseURL  string `json:"api_base_url,omitempty"`

	// The remaining fields are the credentials as the single-file layout stored
	// them, before each environment got a file of its own. They are read as a
	// fallback for the active environment when it has no credentials file yet,
	// which keeps a working install working across the change. Save never
	// writes them, so they disappear the first time anything is written.
	LegacyClientID     string `json:"client_id,omitempty"`
	LegacyClientSecret string `json:"client_secret,omitempty"`
	LegacyRedirectURI  string `json:"redirect_uri,omitempty"`
	LegacyAccountID    string `json:"account_id,omitempty"`
}

// credentialsFile is the on-disk shape of credentials-<environment>.json.
type credentialsFile struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	RedirectURI  string `json:"redirect_uri,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
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
		CacheTTL: DefaultCacheTTL,
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
		return fmt.Errorf("missing credentials for the %s environment: %s", c.Environment, strings.Join(missing, ", "))
	}
	if c.Environment != "" && !strings.EqualFold(c.Environment, EnvProduction) && !strings.EqualFold(c.Environment, EnvSandbox) {
		return fmt.Errorf("invalid environment %q: want %q or %q", c.Environment, EnvProduction, EnvSandbox)
	}
	return nil
}

// Dir returns the directory holding config.json and token.json. DK_CONFIG_DIR
// overrides it, which is what the tests (and throwaway sandboxes) use.
//
// On Unix this is $XDG_CONFIG_HOME/dk, or ~/.config/dk. Deliberately not
// os.UserConfigDir(): that maps to ~/Library/Application Support on macOS,
// which is Apple's convention for GUI applications — command-line tools live in
// ~/.config there, which is where gh, git, and htop all put themselves. The
// stdlib path also contains a space, and dk prints its config location from
// `dk config path` and `dk auth status`, so a caller pasting that into a shell
// would need to know to quote it.
//
// Windows keeps %AppData%, where os.UserConfigDir is already right.
func Dir() (string, error) {
	if dir := os.Getenv("DK_CONFIG_DIR"); dir != "" {
		return dir, nil
	}

	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("locate user config dir: %w", err)
		}
		return filepath.Join(base, "dk"), nil
	}

	// The XDG spec says a relative XDG_CONFIG_HOME is invalid and must be
	// ignored rather than resolved against the working directory — otherwise
	// the config location would depend on where dk was run from.
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "dk"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "dk"), nil
}

// DefaultCacheTTL is how long a cached API response stays fresh by default.
//
// Stock and pricing are the volatile fields, and both move on a scale of hours
// rather than minutes, so ten minutes is long enough to absorb the re-runs of
// one working session — the same query looked at, then run again to pipe its
// output somewhere — and short enough that no BOM is ever costed from a stale
// catalog. Set it to "0" to turn the response cache off entirely.
const DefaultCacheTTL = "10m"

// CacheDir returns the directory holding cached API responses. It is separate
// from Dir() because these files are reconstructible: deleting the cache costs
// a few API calls, while deleting the config directory costs a login.
//
// DK_CONFIG_DIR is honored ahead of the platform location even though this is
// not the config directory. That variable is dk's isolation lever — the tests
// and throwaway sandboxes set it to contain dk's entire on-disk footprint — and
// a cache that escaped it would let one test read another's responses, and let
// a test run pollute the real user cache.
func CacheDir() (string, error) {
	// Neither branch below returns the variable's value as given. This path is
	// what `dk cache clear` deletes, and a variable pointed at a home directory
	// must not turn that command into a way to erase one; each therefore ends
	// in a directory dk owns and created.
	if dir := os.Getenv("DK_CACHE_DIR"); dir != "" {
		return filepath.Join(dir, "dk"), nil
	}
	if dir := os.Getenv("DK_CONFIG_DIR"); dir != "" {
		// "cache" rather than "dk": this one is already inside a directory dk
		// was handed, so a second dk element would only nest.
		return filepath.Join(dir, "cache"), nil
	}

	if runtime.GOOS == "windows" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("locate user cache dir: %w", err)
		}
		return filepath.Join(base, "dk"), nil
	}

	// As in Dir(), a relative XDG path is invalid and must be ignored rather
	// than resolved against the working directory.
	if dir := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "dk"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "dk"), nil
}

// ParseCacheTTL turns a cache_ttl setting into a duration. An empty value means
// the default; zero or less means caching is off. A negative value is rejected
// rather than clamped, since "-5m" is far more likely to be a mistake than a
// deliberate way to spell "disabled".
func ParseCacheTTL(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		v = DefaultCacheTTL
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid cache_ttl %q: want a duration like 10m, 30s, or 0 to disable", v)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid cache_ttl %q: must not be negative (use 0 to disable the cache)", v)
	}
	return d, nil
}

// CacheTTLDuration returns the resolved response-cache freshness window.
func (c Config) CacheTTLDuration() (time.Duration, error) {
	return ParseCacheTTL(c.CacheTTL)
}

// Path returns the full path to config.json, the shared settings file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// CredentialsPath returns the full path to one environment's credentials file.
// The environment is normalized first, so the name on disk is always one of the
// canonical spellings rather than whatever short form was typed.
func CredentialsPath(environment string) (string, error) {
	env, err := environmentOrDefault(environment)
	if err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials-"+env+".json"), nil
}

// Load reads the shared file and the active environment's credentials, overlays
// environment variables, and fills in defaults. Missing files are not an error:
// dk is fully usable with environment variables alone.
func Load() (Config, error) {
	cfg, err := LoadFile()
	if err != nil {
		return Default(), err
	}
	return applyEnv(cfg), nil
}

// LoadFile reads the stored configuration for the active environment over the
// built-in defaults, without applying environment variables. `dk config set`
// uses it so that a secret passed only through the environment is never written
// to disk as a side effect.
func LoadFile() (Config, error) {
	shared, err := loadShared()
	if err != nil {
		return Default(), err
	}

	env := EnvProduction
	if shared.Environment != "" {
		env, err = ParseEnvironment(shared.Environment)
		if err != nil {
			path, _ := Path()
			return Default(), fmt.Errorf("%s: %w", path, err)
		}
	}
	return resolve(env, shared)
}

// LoadEnvironment reads the stored configuration for a named environment,
// active or not, without applying environment variables. `dk env` uses it to
// report on an environment it is about to switch to.
func LoadEnvironment(environment string) (Config, error) {
	env, err := ParseEnvironment(environment)
	if err != nil {
		return Default(), err
	}
	shared, err := loadShared()
	if err != nil {
		return Default(), err
	}
	return resolve(env, shared)
}

// resolve layers one environment's credentials and the shared settings over the
// built-in defaults.
func resolve(env string, shared sharedFile) (Config, error) {
	cfg := Default()
	cfg.Environment = env

	setIfNotEmpty(&cfg.Locale.Site, shared.Locale.Site)
	setIfNotEmpty(&cfg.Locale.Language, shared.Locale.Language)
	setIfNotEmpty(&cfg.Locale.Currency, shared.Locale.Currency)
	setIfNotEmpty(&cfg.CacheTTL, shared.CacheTTL)
	setIfNotEmpty(&cfg.APIBaseURL, shared.APIBaseURL)

	creds, found, err := loadCredentials(env)
	if err != nil {
		return cfg, err
	}
	if !found && strings.EqualFold(env, shared.Environment) {
		// No file for this environment yet. Credentials left in config.json by
		// the single-file layout belong to whichever environment that file
		// names, and only to that one — carrying them into the other
		// environment would hand the sandbox a production client id.
		creds = credentialsFile{
			ClientID:     shared.LegacyClientID,
			ClientSecret: shared.LegacyClientSecret,
			RedirectURI:  shared.LegacyRedirectURI,
			AccountID:    shared.LegacyAccountID,
		}
	}
	setIfNotEmpty(&cfg.ClientID, creds.ClientID)
	setIfNotEmpty(&cfg.ClientSecret, creds.ClientSecret)
	setIfNotEmpty(&cfg.RedirectURI, creds.RedirectURI)
	setIfNotEmpty(&cfg.AccountID, creds.AccountID)

	return cfg, nil
}

// loadShared reads config.json. A missing file yields the zero value.
func loadShared() (sharedFile, error) {
	var file sharedFile

	path, err := Path()
	if err != nil {
		return file, err
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &file); err != nil {
			return file, fmt.Errorf("parse %s: %w", path, err)
		}
		return file, nil
	case errors.Is(err, fs.ErrNotExist):
		return file, nil
	default:
		return file, fmt.Errorf("read %s: %w", path, err)
	}
}

// loadCredentials reads one environment's credentials file. The bool reports
// whether the file existed, which is what separates "no credentials stored for
// this environment" from "stored, but every field is empty" — only the former
// may fall back to the pre-split layout.
func loadCredentials(environment string) (credentialsFile, bool, error) {
	var file credentialsFile

	path, err := CredentialsPath(environment)
	if err != nil {
		return file, false, err
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &file); err != nil {
			return file, true, fmt.Errorf("parse %s: %w", path, err)
		}
		return file, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return file, false, nil
	default:
		return file, false, fmt.Errorf("read %s: %w", path, err)
	}
}

// applyEnv overlays DIGIKEY_* environment variables onto cfg.
//
// There is deliberately no variable for the environment itself. Which DigiKey
// deployment dk talks to is persistent state owned by `dk env`, so that the
// environment `dk env` and `dk auth status` report is always the one the next
// command will use — a variable that could differ per shell would make those
// reports advisory rather than authoritative.
func applyEnv(cfg Config) Config {
	setString(&cfg.ClientID, "DIGIKEY_CLIENT_ID")
	setString(&cfg.ClientSecret, "DIGIKEY_CLIENT_SECRET")
	setString(&cfg.RedirectURI, "DIGIKEY_REDIRECT_URI")
	setString(&cfg.AccountID, "DIGIKEY_ACCOUNT_ID")
	setString(&cfg.Locale.Site, "DIGIKEY_LOCALE_SITE")
	setString(&cfg.Locale.Language, "DIGIKEY_LOCALE_LANGUAGE")
	setString(&cfg.Locale.Currency, "DIGIKEY_LOCALE_CURRENCY")
	setString(&cfg.APIBaseURL, "DIGIKEY_API_BASE_URL")
	setString(&cfg.CacheTTL, "DK_CACHE_TTL")
	return cfg
}

func setString(dst *string, env string) {
	if v := os.Getenv(env); v != "" {
		*dst = v
	}
}

func setIfNotEmpty(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// Save writes cfg back to disk, splitting it across the shared file and the
// active environment's credentials file. Both are written with 0600
// permissions and atomically, so a crash cannot truncate either one.
//
// The credentials file is written even when only a shared setting changed. It
// is the cheaper mistake: the alternative is tracking which half a caller
// touched, and a stale secret left behind by a missed write is far worse than
// an identical file rewritten.
func Save(cfg Config) error {
	env, err := environmentOrDefault(cfg.Environment)
	if err != nil {
		return err
	}

	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	sharedPath, err := Path()
	if err != nil {
		return err
	}
	// Built from the fields explicitly, not from cfg wholesale: that is what
	// drops the pre-split credential keys rather than writing them back.
	if err := writeJSON(sharedPath, sharedFile{
		Environment: env,
		Locale:      cfg.Locale,
		CacheTTL:    cfg.CacheTTL,
		APIBaseURL:  cfg.APIBaseURL,
	}); err != nil {
		return err
	}

	credsPath, err := CredentialsPath(env)
	if err != nil {
		return err
	}
	return writeJSON(credsPath, credentialsFile{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURI:  cfg.RedirectURI,
		AccountID:    cfg.AccountID,
	})
}

// SaveEnvironment records which environment is active, leaving every other
// stored setting alone. `dk env` uses it rather than Save so that switching
// environments never rewrites the credentials of the one being left.
func SaveEnvironment(environment string) error {
	env, err := ParseEnvironment(environment)
	if err != nil {
		return err
	}

	shared, err := loadShared()
	if err != nil {
		return err
	}

	// Credentials left in config.json by the single-file layout are identified
	// only by the environment that file names — so they have to be moved out
	// before the name changes. Skipping this would not lose them: it would
	// silently hand them to the environment being switched *to*, which for a
	// production secret adopted by the sandbox is worse than losing them.
	if err := extractLegacyCredentials(shared); err != nil {
		return err
	}
	shared.LegacyClientID = ""
	shared.LegacyClientSecret = ""
	shared.LegacyRedirectURI = ""
	shared.LegacyAccountID = ""
	shared.Environment = env

	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	return writeJSON(path, shared)
}

// extractLegacyCredentials writes the pre-split credentials from config.json
// into the file of the environment that config.json currently names. It is a
// no-op when there are none, or when that environment already has a file of its
// own — an existing file is the newer truth and is never overwritten.
func extractLegacyCredentials(shared sharedFile) error {
	if shared.LegacyClientID == "" && shared.LegacyClientSecret == "" &&
		shared.LegacyRedirectURI == "" && shared.LegacyAccountID == "" {
		return nil
	}

	owner, err := environmentOrDefault(shared.Environment)
	if err != nil {
		// An unreadable environment name means there is no way to tell whose
		// credentials these are. Refuse rather than guess: guessing wrong
		// attaches one deployment's secret to the other.
		return fmt.Errorf("cannot tell which environment the credentials in config.json belong to: %w", err)
	}

	if _, found, err := loadCredentials(owner); err != nil {
		return err
	} else if found {
		return nil
	}

	path, err := CredentialsPath(owner)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	return writeJSON(path, credentialsFile{
		ClientID:     shared.LegacyClientID,
		ClientSecret: shared.LegacyClientSecret,
		RedirectURI:  shared.LegacyRedirectURI,
		AccountID:    shared.LegacyAccountID,
	})
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
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
		// Rejected rather than accepted as a synonym. The environment decides
		// which credentials file `dk config set` writes to, so setting it here
		// would mean one command that both redirects the write and performs it.
		return errors.New("the environment is not set through `dk config set`; use `dk env production` or `dk env sandbox`")
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
	case "cache_ttl":
		if _, err := ParseCacheTTL(value); err != nil {
			return err
		}
		c.CacheTTL = strings.TrimSpace(value)
	default:
		return fmt.Errorf("unknown config key %q (valid: %s)", key, strings.Join(Keys(), ", "))
	}
	return nil
}

// Keys lists the settable config keys, in the order `dk config set` documents
// them. The environment is absent on purpose: it is changed with `dk env`.
func Keys() []string {
	return []string{
		"client_id",
		"client_secret",
		"redirect_uri",
		"account_id",
		"locale.site",
		"locale.language",
		"locale.currency",
		"cache_ttl",
	}
}

// ShowKeys lists what `dk config show` prints: every settable key, plus the
// environment, which is shown because it decides where four of the others were
// read from and would be confusing to omit.
func ShowKeys() []string {
	return append([]string{"environment"}, Keys()...)
}

// PerEnvironmentKeys lists the keys stored in the active environment's
// credentials file rather than in the shared one.
func PerEnvironmentKeys() []string {
	return []string{"client_id", "client_secret", "redirect_uri", "account_id"}
}

// IsPerEnvironmentKey reports whether a key is stored in the credentials file.
// It accepts the same spellings Set does, so a caller need not normalize first.
func IsPerEnvironmentKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, k := range PerEnvironmentKeys() {
		if key == k {
			return true
		}
	}
	return false
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
		"cache_ttl":       c.CacheTTL,
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
