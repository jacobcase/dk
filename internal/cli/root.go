// Package cli assembles dk's cobra command tree.
//
// Commands are built around an *App, which holds resolved configuration, the
// I/O streams, and lazily-constructed API and auth clients. Threading that
// through explicitly (rather than through package globals) is what makes the
// commands testable against an httptest server.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/auth"
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

	store   *auth.Store
	manager *auth.Manager
	client  *digikey.Client

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
}

// httpClient returns the shared HTTP client honoring --timeout.
func (a *App) httpClient() *http.Client {
	return &http.Client{Timeout: a.Timeout}
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

// Client returns the DigiKey API client.
func (a *App) Client() (*digikey.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	manager, err := a.Auth()
	if err != nil {
		return nil, err
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
		Tokens:     manager,
		HTTPClient: a.httpClient(),
		UserAgent:  "dk/" + Version,
	})
	if err != nil {
		return nil, err
	}
	a.client = client
	return client, nil
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

	_ = root.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return output.Formats(), cobra.ShellCompDirectiveNoFileComp
	})
	_ = root.RegisterFlagCompletionFunc("env", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{config.EnvProduction, config.EnvSandbox}, cobra.ShellCompDirectiveNoFileComp
	})

	root.AddCommand(
		newSearchCommand(app),
		newProductCommand(app),
		newCategoriesCommand(app),
		newManufacturersCommand(app),
		newListCommand(app),
		newAuthCommand(app),
		newConfigCommand(app),
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
		return &Error{Code: CodeError, Message: err.Error(), ExitCode: ExitConfig, Err: err}
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

	format, err := output.ParseFormat(a.flagOutput)
	if err != nil {
		return usageErrorf("%s", err.Error())
	}
	a.Format = format.Resolve(output.IsTTY(a.Out))
	a.Printer = &output.Printer{Format: a.Format, Out: a.Out}

	// Config changes invalidate anything already built from an earlier run
	// (only relevant to in-process tests that reuse an App).
	a.manager = nil
	a.client = nil
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

	// Context cancellation means the user hit Ctrl-C; report it quietly.
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(errOut, "Cancelled.")
		return ExitError
	}

	cliErr := classify(err)
	// Cobra rejects unknown commands, unparseable flags, wrong argument counts,
	// and violated flag groups before any hook runs. Those are all invocation
	// errors, so report them as such rather than as generic failures.
	if !app.setupRan && cliErr.Code == CodeError {
		cliErr.Code = CodeUsage
		cliErr.ExitCode = ExitUsage
	}
	// The format is unset if the failure happened before setup completed.
	format := app.Format
	if format == "" || format == output.FormatAuto {
		format = output.FormatAuto.Resolve(output.IsTTY(errOut))
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
