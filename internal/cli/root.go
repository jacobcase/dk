// Package cli assembles dk's cobra command tree.
//
// Commands are built around an *App, which holds resolved configuration, the
// I/O streams, and lazily-constructed API and auth clients. Threading that
// through explicitly (rather than through package globals) is what makes the
// commands testable against an httptest server.
package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/auth"
	"github.com/jacobcase/dk/internal/cache"
	"github.com/jacobcase/dk/internal/config"
	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

// App is the shared state every command operates on.
type App struct {
	Cfg     config.Config
	Format  output.Format
	Printer *output.Printer

	In  io.Reader
	Out io.Writer
	Err io.Writer

	// Timeout bounds every HTTP request dk makes.
	Timeout time.Duration

	// NoCache ignores what is stored without disabling storing. The freshness
	// window itself is resolved on demand by CacheTTL().
	NoCache bool

	store   *auth.Store
	manager *auth.Manager
	client  *digikey.Client
	cache   *cache.Cache

	// setupRan records whether PersistentPreRunE executed. Cobra validates the
	// command name, flags, and argument counts before that hook, so an error
	// with setupRan still false can only be a bad invocation.
	setupRan bool

	// flag overrides captured before config is loaded
	flagOutput       string
	flagEnv          string
	flagClientID     string
	flagClientSecret string
	flagAccountID    string
	flagSite         string
	flagLanguage     string
	flagCurrency     string
	flagTimeout      time.Duration
	flagNoCache      bool
	flagCacheTTL     time.Duration
	// cacheTTLSet records whether --cache-ttl was passed, since its zero value
	// is a meaningful setting rather than an absent one.
	cacheTTLSet bool
}

// httpClient returns the shared HTTP client honoring --timeout.
func (a *App) httpClient() *http.Client {
	return &http.Client{Timeout: a.Timeout}
}

// downloadClient returns an HTTP client for fetching documents.
//
// --timeout bounds an API call, where a slow response means something is
// wrong. A 100 MB CAD archive on a slow link is not wrong, so the total is
// left unbounded and only the wait for the first byte is capped. Ctrl-C still
// aborts the transfer, because the request carries the signal context.
func (a *App) downloadClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = a.Timeout
	return &http.Client{Transport: tr}
}

// Store returns the on-disk token cache.
func (a *App) Store() (*auth.Store, error) {
	if a.store != nil {
		return a.store, nil
	}
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	a.store = auth.NewStore(filepath.Join(dir, "token.json"))
	return a.store, nil
}

// Auth returns the OAuth manager, verifying that credentials are present.
func (a *App) Auth() (*auth.Manager, error) {
	if a.manager != nil {
		return a.manager, nil
	}
	if err := a.Cfg.Validate(); err != nil {
		return nil, &Error{
			Code:     CodeCredentials,
			Message:  err.Error(),
			Hint:     "Set DIGIKEY_CLIENT_ID and DIGIKEY_CLIENT_SECRET, or run `dk config set client_id <id>` and `dk config set client_secret <secret>`. Register an app at https://developer.digikey.com to get them.",
			ExitCode: ExitConfig,
			Err:      err,
		}
	}
	store, err := a.Store()
	if err != nil {
		return nil, err
	}
	a.manager = &auth.Manager{
		BaseURL:      a.Cfg.BaseURL(),
		ClientID:     a.Cfg.ClientID,
		ClientSecret: a.Cfg.ClientSecret,
		Environment:  a.Cfg.Environment,
		Store:        store,
		HTTPClient:   a.httpClient(),
		// A cached 3-legged token also works for product endpoints and returns
		// account-specific pricing, so use it when one is available.
		PreferUser: true,
	}
	return a.manager, nil
}

// CacheTTL resolves how long a cached API response stays fresh: --cache-ttl
// when it was passed, otherwise DK_CACHE_TTL or the config file. Zero means the
// response cache is off.
//
// The error is classified as a config error so that the exit code and the JSON
// error.code agree, and carries the hint naming the setting to fix.
func (a *App) CacheTTL() (time.Duration, error) {
	if a.cacheTTLSet {
		return a.flagCacheTTL, nil
	}
	ttl, err := a.Cfg.CacheTTLDuration()
	if err != nil {
		return 0, &Error{
			Code:     CodeConfig,
			Message:  err.Error(),
			Hint:     "Set a duration like `dk config set cache_ttl 10m`, or 0 to disable the response cache.",
			ExitCode: ExitConfig,
			Err:      err,
		}
	}
	return ttl, nil
}

// Cache returns the on-disk response cache, or nil when there is no directory
// to put it in.
//
// It is returned even when the TTL is zero, which is what switches caching off.
// A zero-TTL cache reads and writes nothing but still invalidates, so a list
// write made with the cache off does not leave an earlier run's entries behind
// for the next run to serve. Deciding not to read the cache is a preference;
// not stranding a stale list read is not.
func (a *App) Cache() (*cache.Cache, error) {
	if a.cache != nil {
		return a.cache, nil
	}
	ttl, err := a.CacheTTL()
	if err != nil {
		return nil, err
	}
	dir, err := config.CacheDir()
	if err != nil {
		return nil, err
	}
	a.cache = cache.New(dir, ttl)
	return a.cache, nil
}

// Client returns the DigiKey API client.
func (a *App) Client() (*digikey.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	manager, err := a.Auth()
	if err != nil {
		return nil, err
	}
	responses, err := a.Cache()
	if err != nil {
		return nil, err
	}
	// Assigned through a nil check rather than directly: a nil *cache.Cache
	// stored in a non-nil interface would make every "is caching on" test in
	// the client come out true.
	var responseCache digikey.ResponseCache
	if responses != nil {
		responseCache = responses
	}
	client, err := digikey.New(digikey.Options{
		BaseURL:   a.Cfg.BaseURL(),
		ClientID:  a.Cfg.ClientID,
		AccountID: a.Cfg.AccountID,
		Locale: digikey.Locale{
			Site:     a.Cfg.Locale.Site,
			Language: a.Cfg.Locale.Language,
			Currency: a.Cfg.Locale.Currency,
		},
		Tokens:       manager,
		HTTPClient:   a.httpClient(),
		UserAgent:    "dk/" + Version,
		Cache:        responseCache,
		CacheRefresh: a.NoCache,
	})
	if err != nil {
		return nil, err
	}
	a.client = client
	return client, nil
}

// locale returns the DigiKey locale implied by the resolved configuration.
func (a *App) locale() digikey.Locale {
	return digikey.Locale{
		Site:     a.Cfg.Locale.Site,
		Language: a.Cfg.Locale.Language,
		Currency: a.Cfg.Locale.Currency,
	}
}

// checkRawFormat rejects --raw alongside an explicit --output that cannot
// render a raw payload. Commands call it before touching the API, so a bad
// invocation costs no request.
//
// flagOutput is empty unless --output was passed, so --raw wins silently over
// an auto-resolved table — that is what makes `dk search --raw` work on a
// terminal — and conflicts loudly with an explicit --output csv: a caller that
// asked for CSV should be told, not handed JSON.
func (a *App) checkRawFormat(raw bool) error {
	if !raw {
		return nil
	}
	// Already validated in setup, so a parse failure here is unreachable.
	requested, _ := output.ParseFormat(a.flagOutput)
	if requested != output.FormatAuto && requested != output.FormatJSON {
		return usageErrorf("--raw emits DigiKey's unmodified JSON and cannot be combined with --output %s", requested)
	}
	return nil
}

// printRaw emits DigiKey's untouched payload. A raw response has no table or
// CSV representation, so --raw always prints JSON, which is what its help text
// promises; checkRawFormat has already rejected an explicit request for
// anything else.
func (a *App) printRaw(payload any) error {
	return (&output.Printer{Format: output.FormatJSON, Out: a.Out}).Print(payload, nil)
}

// NewRootCommand builds the full command tree bound to app.
func NewRootCommand(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "dk",
		Short: "DigiKey CLI for searching parts and building order lists",
		Long: `dk searches the DigiKey catalog and manages DigiKey "MyLists" order lists.

It is built to be driven either by a human or by another program: every command
accepts --output json, failures carry distinct exit codes, and errors in JSON
mode are structured objects with a stable "code" field.

Typical DIY workflow:

  dk search "0.1uF 0603 X7R 50V" --in-stock --limit 5
  dk list create "Bench PSU rev A"
  dk list add "Bench PSU rev A" 1276-1000-1-ND:10 --ref C1-C10
  dk list show "Bench PSU rev A"

Nothing dk does places an order. Lists are staged for you to review and buy
manually on digikey.com.

Run "dk guide" for a condensed reference aimed at automated callers.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Show help rather than an error when invoked bare.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return app.setup(cmd)
		},
	}

	root.SetIn(app.In)
	root.SetOut(app.Out)
	root.SetErr(app.Err)

	f := root.PersistentFlags()
	f.StringVarP(&app.flagOutput, "output", "o", "", fmt.Sprintf("output format: %v (default: table on a terminal, json when piped)", output.Formats()))
	f.StringVar(&app.flagEnv, "env", "", "DigiKey environment: production or sandbox")
	f.StringVar(&app.flagClientID, "client-id", "", "DigiKey app client id (overrides config and DIGIKEY_CLIENT_ID)")
	f.StringVar(&app.flagClientSecret, "client-secret", "", "DigiKey app client secret (overrides config and DIGIKEY_CLIENT_SECRET)")
	f.StringVar(&app.flagAccountID, "account-id", "", "DigiKey account id, for logins with more than one account")
	f.StringVar(&app.flagSite, "site", "", "DigiKey locale site code, e.g. US, CA, DE")
	f.StringVar(&app.flagLanguage, "language", "", "locale language code, e.g. en, de")
	f.StringVar(&app.flagCurrency, "currency", "", "pricing currency code, e.g. USD, EUR")
	f.DurationVar(&app.flagTimeout, "timeout", 30*time.Second, "per-request timeout")
	f.BoolVar(&app.flagNoCache, "no-cache", false, "ignore cached responses and ask DigiKey (the fresh reply still replaces the cached one)")
	f.DurationVar(&app.flagCacheTTL, "cache-ttl", 0, "how long a cached response stays fresh; 0 disables the cache (default from config, "+config.DefaultCacheTTL+")")

	_ = root.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return output.Formats(), cobra.ShellCompDirectiveNoFileComp
	})
	_ = root.RegisterFlagCompletionFunc("env", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{config.EnvProduction, config.EnvSandbox}, cobra.ShellCompDirectiveNoFileComp
	})

	root.AddCommand(
		newSearchCommand(app),
		newFiltersCommand(app),
		newProductCommand(app),
		newDocsCommand(app),
		newRelatedCommand(app),
		newPricingCommand(app),
		newCategoriesCommand(app),
		newManufacturersCommand(app),
		newListCommand(app),
		newAuthCommand(app),
		newConfigCommand(app),
		newCacheCommand(app),
		newGuideCommand(app),
		newVersionCommand(app),
	)
	return root
}

// setup loads configuration, applies flag overrides, and prepares the printer.
// It runs before every command.
func (a *App) setup(cmd *cobra.Command) error {
	a.setupRan = true

	cfg, err := config.Load()
	if err != nil {
		// CodeConfig, not CodeError: exit 6 and the JSON code have to agree, or
		// a caller branching on .error.code reaches a different conclusion than
		// one branching on $?.
		return &Error{
			Code:     CodeConfig,
			Message:  err.Error(),
			Hint:     "Fix or remove the config file, or point DK_CONFIG_DIR at a different directory.",
			ExitCode: ExitConfig,
			Err:      err,
		}
	}

	if a.flagClientID != "" {
		cfg.ClientID = a.flagClientID
	}
	if a.flagClientSecret != "" {
		cfg.ClientSecret = a.flagClientSecret
	}
	if a.flagEnv != "" {
		cfg.Environment = a.flagEnv
	}
	if a.flagAccountID != "" {
		cfg.AccountID = a.flagAccountID
	}
	if a.flagSite != "" {
		cfg.Locale.Site = a.flagSite
	}
	if a.flagLanguage != "" {
		cfg.Locale.Language = a.flagLanguage
	}
	if a.flagCurrency != "" {
		cfg.Locale.Currency = a.flagCurrency
	}
	a.Cfg = cfg

	a.Timeout = a.flagTimeout
	if a.Timeout <= 0 {
		a.Timeout = 30 * time.Second
	}

	// --cache-ttl beats DK_CACHE_TTL and the config file, but only when it was
	// actually passed: its zero value means "disabled", so treating an unset
	// flag as a value would turn the cache off for everyone.
	//
	// The configured value is not parsed here. Every other config field is
	// validated inside the accessor that needs it, and for the same reason: a
	// bad cache_ttl rejected in this hook would fail every command, including
	// `dk config set cache_ttl` — the one that repairs it — and `dk cache
	// clear`, leaving hand-editing config.json as the only way out.
	a.cacheTTLSet = cmd.Flags().Changed("cache-ttl")
	if a.cacheTTLSet && a.flagCacheTTL < 0 {
		return usageErrorf("--cache-ttl must not be negative (pass 0 to disable the cache)")
	}
	// Applied only when the flag was actually passed, exactly as --cache-ttl
	// above: NoCache is part of App's surface, and an in-process caller that
	// set it before Execute must not have it reset to a flag's zero value.
	if cmd.Flags().Changed("no-cache") {
		a.NoCache = a.flagNoCache
	}

	format, err := output.ParseFormat(a.flagOutput)
	if err != nil {
		return usageErrorf("%s", err.Error())
	}
	a.Printer = output.NewPrinter(format, a.Out, a.Err)
	a.Format = a.Printer.Format

	// Config changes invalidate anything already built from an earlier run
	// (only relevant to in-process tests that reuse an App).
	a.manager = nil
	a.client = nil
	a.cache = nil
	return nil
}

// Execute builds and runs the CLI, returning a process exit code. It never
// panics on a command error: everything is funneled through classify so the
// exit code and the rendered message stay in sync.
func Execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	app := &App{In: in, Out: out, Err: errOut, Format: output.FormatAuto}
	root := NewRootCommand(app)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	cliErr := classify(err)
	// Cobra rejects unknown commands, unparseable flags, wrong argument counts,
	// and violated flag groups before any hook runs. Those are all invocation
	// errors, so report them as such rather than as generic failures.
	if !app.setupRan && cliErr.Code == CodeError {
		cliErr.Code = CodeUsage
		cliErr.ExitCode = ExitUsage
	}
	// The format is unset if the failure happened before setup completed. Cobra
	// still parsed the flags by then, so fall back to an explicit --output
	// rather than to TTY detection: a caller that asked for JSON must not get a
	// line of prose because the config file happened to be malformed.
	format := app.Format
	if format == "" || format == output.FormatAuto {
		if requested, perr := output.ParseFormat(app.flagOutput); perr == nil && requested != output.FormatAuto {
			format = requested
		} else {
			format = output.FormatAuto.Resolve(output.IsTTY(errOut))
		}
	}
	writeError(errOut, format, cliErr)
	return cliErr.ExitCode
}

// Main is the process entry point.
func Main() int {
	ctx, cancel := signalContext(context.Background())
	defer cancel()
	return Execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}
