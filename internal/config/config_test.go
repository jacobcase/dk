package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withConfigDir points config at a fresh temp directory for the duration of a
// test, so tests never read or clobber the developer's real config.
// digikeyEnvVars is every variable applyEnv consults. Keep it in sync with
// that function: a new variable left out here would let a developer's real
// credentials leak into these tests and quietly change what they assert.
var digikeyEnvVars = []string{
	"DIGIKEY_CLIENT_ID",
	"DIGIKEY_CLIENT_SECRET",
	"DIGIKEY_ENV",
	"DIGIKEY_REDIRECT_URI",
	"DIGIKEY_ACCOUNT_ID",
	"DIGIKEY_LOCALE_SITE",
	"DIGIKEY_LOCALE_LANGUAGE",
	"DIGIKEY_LOCALE_CURRENCY",
	"DIGIKEY_API_BASE_URL",
}

// withConfigDir points config at a temp dir and clears the ambient DigiKey
// environment, so these tests describe the code rather than the machine they
// run on. t.Setenv restores the previous values at cleanup.
func withConfigDir(t *testing.T) string {
	t.Helper()
	for _, key := range digikeyEnvVars {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	t.Setenv("DK_CONFIG_DIR", dir)
	return dir
}

func TestDefaultIsProductionUS(t *testing.T) {
	cfg := Default()
	if cfg.Environment != EnvProduction {
		t.Errorf("Environment = %q, want %q", cfg.Environment, EnvProduction)
	}
	if got := cfg.BaseURL(); got != ProductionBaseURL {
		t.Errorf("BaseURL() = %q, want %q", got, ProductionBaseURL)
	}
	if cfg.Locale.Site != "US" || cfg.Locale.Currency != "USD" || cfg.Locale.Language != "en" {
		t.Errorf("Locale = %+v, want US/en/USD", cfg.Locale)
	}
}

func TestBaseURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"production", Config{Environment: EnvProduction}, ProductionBaseURL},
		{"sandbox", Config{Environment: EnvSandbox}, SandboxBaseURL},
		{"case insensitive sandbox", Config{Environment: "SANDBOX"}, SandboxBaseURL},
		{"empty defaults to production", Config{}, ProductionBaseURL},
		{"override wins", Config{Environment: EnvSandbox, APIBaseURL: "http://127.0.0.1:9/"}, "http://127.0.0.1:9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.BaseURL(); got != tt.want {
				t.Errorf("BaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{ClientID: "id", ClientSecret: "secret", Environment: EnvProduction}, false},
		{"missing id", Config{ClientSecret: "secret"}, true},
		{"missing secret", Config{ClientID: "id"}, true},
		{"missing both", Config{}, true},
		{"bad environment", Config{ClientID: "id", ClientSecret: "s", Environment: "staging"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	withConfigDir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a missing config file", err)
	}
	if cfg.Environment != EnvProduction {
		t.Errorf("Environment = %q, want %q", cfg.Environment, EnvProduction)
	}
	if cfg.RedirectURI != DefaultRedirectURI {
		t.Errorf("RedirectURI = %q, want %q", cfg.RedirectURI, DefaultRedirectURI)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	withConfigDir(t)

	if err := Save(Config{ClientID: "from-file", ClientSecret: "file-secret", Environment: EnvProduction}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	t.Setenv("DIGIKEY_CLIENT_ID", "from-env")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ClientID != "from-env" {
		t.Errorf("ClientID = %q, want the environment value %q", cfg.ClientID, "from-env")
	}
	// The secret was only in the file, so it must survive the env overlay.
	if cfg.ClientSecret != "file-secret" {
		t.Errorf("ClientSecret = %q, want the file value %q", cfg.ClientSecret, "file-secret")
	}
}

// The active environment is persistent state owned by `dk env`. No variable may
// redirect it, or `dk env` and `dk auth status` would report an environment
// that the next command in a differently-configured shell does not use.
func TestLoadIgnoresDigikeyEnvVariable(t *testing.T) {
	withConfigDir(t)

	if err := Save(Config{ClientID: "prod-id", ClientSecret: "prod-secret", Environment: EnvProduction}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	t.Setenv("DIGIKEY_ENV", EnvSandbox)
	t.Setenv("DIGIKEY_ENVIRONMENT", EnvSandbox)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Environment != EnvProduction {
		t.Errorf("Environment = %q, want %q: DIGIKEY_ENV must not switch environments", cfg.Environment, EnvProduction)
	}
	if cfg.BaseURL() != ProductionBaseURL {
		t.Errorf("BaseURL() = %q, want %q: the host must follow the stored environment", cfg.BaseURL(), ProductionBaseURL)
	}
	// The credentials must come from the stored environment too, or the
	// variable would have swapped the host's credentials without the host.
	if cfg.ClientID != "prod-id" {
		t.Errorf("ClientID = %q, want %q", cfg.ClientID, "prod-id")
	}
}

func TestLoadFileIgnoresEnvironment(t *testing.T) {
	withConfigDir(t)

	if err := Save(Config{ClientID: "from-file"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	t.Setenv("DIGIKEY_CLIENT_SECRET", "env-only-secret")

	cfg, err := LoadFile()
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	// This is what stops `dk config set` from persisting a secret that the user
	// only exported for one run.
	if cfg.ClientSecret != "" {
		t.Errorf("ClientSecret = %q, want empty: LoadFile must not read the environment", cfg.ClientSecret)
	}
	if cfg.ClientID != "from-file" {
		t.Errorf("ClientID = %q, want %q", cfg.ClientID, "from-file")
	}
}

func TestLoadCorruptFileErrors(t *testing.T) {
	dir := withConfigDir(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("Load() error = nil, want a parse error for a corrupt config file")
	}
}

func TestSaveRoundTripAndPermissions(t *testing.T) {
	dir := withConfigDir(t)

	want := Config{
		ClientID:     "abc",
		ClientSecret: "shhh",
		Environment:  EnvSandbox,
		RedirectURI:  "https://localhost:9999/cb",
		AccountID:    "12345",
		Locale:       Locale{Site: "DE", Language: "de", Currency: "EUR"},
		CacheTTL:     "30s",
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := LoadFile()
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The file can hold a client secret, so it must not be group or world readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %#o, want 0600", perm)
	}
}

func TestSaveIsAtomicOverwrite(t *testing.T) {
	dir := withConfigDir(t)

	if err := Save(Config{ClientID: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{ClientID: "second"}); err != nil {
		t.Fatal(err)
	}

	// The client id is per environment, so it is the credentials file that has
	// to have been overwritten cleanly.
	data, err := os.ReadFile(filepath.Join(dir, "credentials-production.json"))
	if err != nil {
		t.Fatal(err)
	}
	var creds Config
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatalf("second write produced unparseable json: %v", err)
	}
	if creds.ClientID != "second" {
		t.Errorf("ClientID = %q, want %q", creds.ClientID, "second")
	}

	// The temporary files used for the atomic renames must not be left behind.
	written := map[string]bool{"config.json": true, "credentials-production.json": true}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !written[e.Name()] {
			t.Errorf("unexpected leftover file %q in config dir", e.Name())
		}
	}
}

// Save splits one Config across two files; both have to be unreadable to anyone
// else, since the credentials file is where the secret actually lands.
func TestSaveWritesBothFiles0600(t *testing.T) {
	dir := withConfigDir(t)

	if err := Save(Config{ClientID: "id", ClientSecret: "secret", Environment: EnvSandbox}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"config.json", "credentials-sandbox.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", name, perm)
		}
	}
}

// Each environment's credentials are separate on disk. DigiKey rejects a
// production client id sent to the sandbox host, so a switch that carried
// credentials across would produce a confusing 401 rather than a clear
// "no credentials for this environment".
func TestCredentialsAreNotSharedBetweenEnvironments(t *testing.T) {
	withConfigDir(t)

	if err := Save(Config{Environment: EnvProduction, ClientID: "prod-id", ClientSecret: "prod-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Environment: EnvSandbox, ClientID: "sbx-id", ClientSecret: "sbx-secret"}); err != nil {
		t.Fatal(err)
	}

	prod, err := LoadEnvironment(EnvProduction)
	if err != nil {
		t.Fatal(err)
	}
	if prod.ClientID != "prod-id" || prod.ClientSecret != "prod-secret" {
		t.Errorf("production credentials = %q/%q, want prod-id/prod-secret: the sandbox write overwrote them", prod.ClientID, prod.ClientSecret)
	}

	sbx, err := LoadEnvironment(EnvSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if sbx.ClientID != "sbx-id" {
		t.Errorf("sandbox ClientID = %q, want %q", sbx.ClientID, "sbx-id")
	}
	if sbx.BaseURL() != SandboxBaseURL {
		t.Errorf("sandbox BaseURL() = %q, want %q", sbx.BaseURL(), SandboxBaseURL)
	}
}

// SaveEnvironment switches the pointer only. Rewriting credentials on a switch
// would be a way to lose the ones belonging to the environment being left.
func TestSaveEnvironmentLeavesCredentialsAlone(t *testing.T) {
	withConfigDir(t)

	if err := Save(Config{Environment: EnvProduction, ClientID: "prod-id", ClientSecret: "prod-secret", Locale: Locale{Currency: "EUR"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveEnvironment(EnvSandbox); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != EnvSandbox {
		t.Errorf("Environment = %q, want %q", cfg.Environment, EnvSandbox)
	}
	if cfg.ClientID != "" {
		t.Errorf("ClientID = %q, want empty: the sandbox has no credentials of its own yet", cfg.ClientID)
	}
	// Shared settings must survive a switch; only the pointer changed.
	if cfg.Locale.Currency != "EUR" {
		t.Errorf("Locale.Currency = %q, want %q", cfg.Locale.Currency, "EUR")
	}

	prod, err := LoadEnvironment(EnvProduction)
	if err != nil {
		t.Fatal(err)
	}
	if prod.ClientID != "prod-id" {
		t.Errorf("production ClientID = %q, want %q: switching away destroyed it", prod.ClientID, "prod-id")
	}
}

// A config.json written by the single-file layout still has credentials at the
// top level. They belong to the environment that file names, and must not be
// handed to the other one.
func TestLegacySingleFileCredentialsAreAdoptedByTheirEnvironmentOnly(t *testing.T) {
	dir := withConfigDir(t)

	legacy := `{"client_id":"legacy-id","client_secret":"legacy-secret","environment":"production","account_id":"42"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "legacy-id" || cfg.ClientSecret != "legacy-secret" {
		t.Errorf("credentials = %q/%q, want legacy-id/legacy-secret: an existing install lost its login", cfg.ClientID, cfg.ClientSecret)
	}
	if cfg.AccountID != "42" {
		t.Errorf("AccountID = %q, want %q", cfg.AccountID, "42")
	}

	sbx, err := LoadEnvironment(EnvSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if sbx.ClientID != "" {
		t.Errorf("sandbox ClientID = %q, want empty: production credentials must not leak into the sandbox", sbx.ClientID)
	}
}

// The sequence a real upgrade takes: a config.json still in the single-file
// layout, then a switch to the sandbox, then storing sandbox credentials. The
// production secret must not follow the pointer into the sandbox's file.
func TestSwitchingEnvironmentDoesNotReassignLegacyCredentials(t *testing.T) {
	dir := withConfigDir(t)

	legacy := `{"client_id":"prod-id","client_secret":"prod-secret","environment":"production"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SaveEnvironment(EnvSandbox); err != nil {
		t.Fatal(err)
	}

	sbx, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if sbx.ClientID != "" || sbx.ClientSecret != "" {
		t.Errorf("sandbox credentials = %q/%q, want empty: the production pair was reassigned by the switch", sbx.ClientID, sbx.ClientSecret)
	}

	// Moved to production's own file rather than dropped, so the login that was
	// working before the switch still works after switching back.
	prod, err := LoadEnvironment(EnvProduction)
	if err != nil {
		t.Fatal(err)
	}
	if prod.ClientID != "prod-id" || prod.ClientSecret != "prod-secret" {
		t.Errorf("production credentials = %q/%q, want prod-id/prod-secret: the switch lost them", prod.ClientID, prod.ClientSecret)
	}

	// And config.json no longer holds the secret at all.
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "prod-secret") {
		t.Errorf("config.json still holds the client secret after the switch:\n%s", raw)
	}
}

// An environment that already has a credentials file is the newer truth; the
// pre-split fields must not overwrite it on a switch.
func TestExtractingLegacyCredentialsDoesNotOverwriteAnExistingFile(t *testing.T) {
	dir := withConfigDir(t)

	if err := Save(Config{Environment: EnvProduction, ClientID: "current-id", ClientSecret: "current-secret"}); err != nil {
		t.Fatal(err)
	}
	// Put stale pre-split keys back alongside the file that supersedes them.
	stale := `{"environment":"production","client_id":"stale-id","client_secret":"stale-secret"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SaveEnvironment(EnvSandbox); err != nil {
		t.Fatal(err)
	}

	prod, err := LoadEnvironment(EnvProduction)
	if err != nil {
		t.Fatal(err)
	}
	if prod.ClientID != "current-id" || prod.ClientSecret != "current-secret" {
		t.Errorf("production credentials = %q/%q, want current-id/current-secret: stale keys overwrote the real file", prod.ClientID, prod.ClientSecret)
	}
}

// Writing anything drops the pre-split credential keys from config.json, so the
// secret does not linger in a second file after the move.
func TestSaveDropsLegacyCredentialsFromSharedFile(t *testing.T) {
	dir := withConfigDir(t)

	legacy := `{"client_id":"legacy-id","client_secret":"legacy-secret","environment":"production"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "legacy-secret") {
		t.Errorf("config.json still holds the client secret after a write:\n%s", data)
	}

	// It moved rather than vanished.
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ClientSecret != "legacy-secret" {
		t.Errorf("ClientSecret = %q, want %q", reloaded.ClientSecret, "legacy-secret")
	}
}

func TestSet(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
		check   func(Config) bool
	}{
		{"client id", "client_id", "abc", false, func(c Config) bool { return c.ClientID == "abc" }},
		{"env alias rejected", "env", "sandbox", true, nil},
		{"environment rejected", "environment", "SANDBOX", true, nil},
		{"locale dotted", "locale.currency", "EUR", false, func(c Config) bool { return c.Locale.Currency == "EUR" }},
		{"locale short alias", "currency", "GBP", false, func(c Config) bool { return c.Locale.Currency == "GBP" }},
		{"key is case insensitive", "CLIENT_ID", "x", false, func(c Config) bool { return c.ClientID == "x" }},
		{"cache ttl", "cache_ttl", "45s", false, func(c Config) bool { return c.CacheTTL == "45s" }},
		{"cache ttl disabled", "cache_ttl", "0", false, func(c Config) bool { return c.CacheTTL == "0" }},
		// Rejected at write time so a typo surfaces at `dk config set` rather
		// than on the next command that happens to need the cache.
		{"cache ttl unparseable", "cache_ttl", "ten minutes", true, nil},
		{"cache ttl negative", "cache_ttl", "-5m", true, nil},
		{"invalid environment", "environment", "staging", true, nil},
		{"unknown key", "nope", "x", true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			err := cfg.Set(tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Set(%q, %q) error = %v, wantErr %v", tt.key, tt.value, err, tt.wantErr)
			}
			if err == nil && tt.check != nil && !tt.check(cfg) {
				t.Errorf("Set(%q, %q) produced %+v, which failed its check", tt.key, tt.value, cfg)
			}
		})
	}
}

func TestRedactedMasksSecret(t *testing.T) {
	cfg := Config{ClientID: "public-id", ClientSecret: "supersecret"}
	got := cfg.Redacted()

	if got["client_id"] != "public-id" {
		t.Errorf("client_id = %q, want it unmasked", got["client_id"])
	}
	secret := got["client_secret"]
	if secret == "supersecret" {
		t.Fatal("client_secret was returned in clear text")
	}
	// The tail is kept so two credentials can be told apart at a glance.
	if want := "*******cret"; secret != want {
		t.Errorf("client_secret = %q, want %q", secret, want)
	}
}

func TestMaskShortSecrets(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "*"},
		{"abcd", "****"},
		{"abcde", "*bcde"},
	}
	for _, tt := range tests {
		if got := mask(tt.in); got != tt.want {
			t.Errorf("mask(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Dir must land alongside the other command-line tools in ~/.config, not in
// macOS's ~/Library/Application Support, which is where os.UserConfigDir would
// put it and where GUI applications live.
func TestDirUsesXDGConfigHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps %AppData%, where os.UserConfigDir is already correct")
	}

	tests := []struct {
		name    string
		dkDir   string
		xdg     string
		home    string
		want    string
		wantErr bool
	}{
		{
			name:  "DK_CONFIG_DIR wins over everything",
			dkDir: "/tmp/explicit",
			xdg:   "/tmp/xdg",
			home:  "/tmp/home",
			want:  "/tmp/explicit",
		},
		{
			name: "absolute XDG_CONFIG_HOME is honored",
			xdg:  "/tmp/xdg",
			home: "/tmp/home",
			want: "/tmp/xdg/dk",
		},
		{
			name: "no XDG falls back to ~/.config",
			home: "/tmp/home",
			want: "/tmp/home/.config/dk",
		},
		{
			// The spec says a relative XDG_CONFIG_HOME is invalid. Resolving it
			// would make the config location depend on the working directory.
			name: "relative XDG_CONFIG_HOME is ignored",
			xdg:  "relative/path",
			home: "/tmp/home",
			want: "/tmp/home/.config/dk",
		},
		{
			name: "empty XDG_CONFIG_HOME is ignored",
			xdg:  "",
			home: "/tmp/home",
			want: "/tmp/home/.config/dk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DK_CONFIG_DIR", tt.dkDir)
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			t.Setenv("HOME", tt.home)

			got, err := Dir()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Dir() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Dir() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The path dk prints must be pasteable into a shell without quoting. The old
// location, ~/Library/Application Support/dk, was not.
func TestCacheDirIsNeverTheBareEnvironmentValue(t *testing.T) {
	// `dk cache clear` deletes what CacheDir() returns, so no branch may return
	// the variable it was given: returning it as-is would make
	// `DK_CACHE_DIR=$HOME dk cache clear` a way to lose a home directory. Both
	// variables are covered, because the guarantee is about what the command
	// can reach, not about which variable named it.
	tests := []struct {
		name string
		env  string
	}{
		{"DK_CACHE_DIR", "DK_CACHE_DIR"},
		{"DK_CONFIG_DIR", "DK_CONFIG_DIR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "somewhere")
			t.Setenv("DK_CACHE_DIR", "")
			t.Setenv("DK_CONFIG_DIR", "")
			t.Setenv(tt.env, dir)

			got, err := CacheDir()
			if err != nil {
				t.Fatal(err)
			}
			if got == dir {
				t.Errorf("CacheDir() = %q, the %s value verbatim; want a dk-owned directory under it", got, tt.env)
			}
			if filepath.Dir(got) != dir {
				t.Errorf("CacheDir() = %q, want a single dk-owned element directly under %s (%q)", got, tt.env, dir)
			}
		})
	}
}

func TestCacheDirStaysInsideTheConfigDir(t *testing.T) {
	// DK_CONFIG_DIR is dk's isolation lever: the tests and throwaway sandboxes
	// point it at a temp dir to contain dk's whole on-disk footprint, and a
	// cache that escaped it would read and pollute the real user cache.
	dir := t.TempDir()
	t.Setenv("DK_CACHE_DIR", "")
	t.Setenv("DK_CONFIG_DIR", dir)

	got, err := CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, dir) {
		t.Errorf("CacheDir() = %q, want it under DK_CONFIG_DIR %q", got, dir)
	}
}

func TestDirHasNoSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths routinely contain spaces")
	}
	t.Setenv("DK_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/Users/example")

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, " \t") {
		t.Errorf("Dir() = %q contains whitespace; `dk config path` output would need quoting", got)
	}
	if !strings.HasSuffix(got, "/.config/dk") {
		t.Errorf("Dir() = %q, want it under ~/.config/dk", got)
	}
}
