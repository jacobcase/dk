package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/auth"
	"github.com/jacobcase/dk/internal/cache"
	"github.com/jacobcase/dk/internal/config"
	"github.com/jacobcase/dk/internal/output"
)

// loginTimeout bounds how long dk waits for the browser round-trip.
const loginTimeout = 5 * time.Minute

// dropCachedResponses empties the response cache after the login it was written
// under has changed.
//
// The cache key names the grant, not the token, so entries written for one
// account are indistinguishable from the next account's. Logging in or out is
// the only moment that can change, and it is a moment dk controls.
//
// The error is deliberately dropped: failing a completed login because the
// cache would not tidy is the wrong trade, and every entry is still bounded by
// the TTL.
func dropCachedResponses() {
	if dir, err := config.CacheDir(); err == nil {
		_ = cache.Clear(dir)
	}
}

func newAuthCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage DigiKey authentication",
		Long: `Manage DigiKey authentication.

DigiKey uses two OAuth flows, and dk uses both:

  Product search (dk search, dk product, dk categories, dk manufacturers)
    Uses the client-credentials grant. Your client id and secret are enough;
    no login is needed.

  Lists (dk list ...)
    Uses the authorization-code grant, because lists belong to a DigiKey user
    account. Run "dk auth login" once from an interactive terminal. The
    resulting refresh token does not expire, so later non-interactive runs
    keep working.

Credentials come from DIGIKEY_CLIENT_ID / DIGIKEY_CLIENT_SECRET or from
"dk config set". Register an app at https://developer.digikey.com and subscribe
it to both "Product Information" and "MyLists".

Credentials and cached tokens are both per environment: DigiKey scopes a client
id to one deployment, so the sandbox needs its own registered app and its own
login. Run "dk env" to see which environment these commands will act on.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		newAuthLoginCommand(app),
		newAuthStatusCommand(app),
		newAuthLogoutCommand(app),
	)
	return cmd
}

func newAuthLoginCommand(app *App) *cobra.Command {
	var (
		manual      bool
		noBrowser   bool
		redirectURI string
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authorize dk against your DigiKey account (required for lists)",
		Long: `Complete DigiKey's 3-legged OAuth flow so dk can manage your lists.

By default dk starts a local HTTPS server on the redirect URI and opens your
browser. DigiKey rejects plain-http redirect URIs, so the local server presents
a self-signed certificate: your browser will warn once, and clicking through is
expected.

The redirect URI must be registered on developer.digikey.com exactly as
configured here (default: ` + config.DefaultRedirectURI + `).

On a headless machine, use --manual: dk prints the authorization URL, you open
it anywhere, and paste the redirected URL back. The authorization code expires
one minute after DigiKey issues it, so paste promptly.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			manager, err := app.Auth()
			if err != nil {
				return err
			}

			uri := redirectURI
			if uri == "" {
				uri = app.Cfg.RedirectURI
			}
			if uri == "" {
				uri = config.DefaultRedirectURI
			}

			state, err := auth.NewState()
			if err != nil {
				return err
			}
			authURL, err := manager.AuthorizationURL(uri, state)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()

			var result auth.CallbackResult
			if manual {
				result, err = app.manualLogin(authURL, uri)
			} else {
				result, err = app.browserLogin(ctx, authURL, uri, state, noBrowser)
			}
			if err != nil {
				return err
			}

			if state != "" && result.State != "" && result.State != state {
				return &Error{
					Code:     CodeAuth,
					Message:  "oauth state mismatch",
					Hint:     "Start the login over. If it keeps happening, make sure only one `dk auth login` is running.",
					ExitCode: ExitAuth,
				}
			}

			// Exchange against a fresh context: the browser wait may have eaten
			// most of the login timeout, and the code lives only 60 seconds.
			exchangeCtx, exchangeCancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 30*time.Second)
			defer exchangeCancel()

			token, err := manager.Exchange(exchangeCtx, result.Code, uri)
			if err != nil {
				return err
			}
			// Whatever is cached was read for whoever was logged in before.
			dropCachedResponses()

			payload := map[string]any{
				"status":        "authenticated",
				"environment":   app.Cfg.Environment,
				"expires_at":    token.ExpiresAt.Format(time.RFC3339),
				"has_refresh":   token.RefreshToken != "",
				"redirect_uri":  uri,
				"token_file":    tokenPathOrEmpty(app),
				"lists_enabled": true,
			}
			t := &output.Table{Headers: []string{"FIELD", "VALUE"}}
			t.AddRow("status", "authenticated")
			t.AddRow("environment", app.Cfg.Environment)
			t.AddRow("access token expires", token.ExpiresAt.Format(time.RFC3339))
			t.AddRow("refresh token stored", fmt.Sprintf("%t", token.RefreshToken != ""))
			t.AddRow("token file", tokenPathOrEmpty(app))
			if err := app.Printer.Print(payload, t); err != nil {
				return err
			}
			app.Printer.PrintText("\n`dk list` commands are now available.")
			return nil
		},
	}

	f := cmd.Flags()
	f.BoolVar(&manual, "manual", false, "print the URL and read the redirected URL from stdin instead of listening locally")
	f.BoolVar(&noBrowser, "no-browser", false, "start the local listener but do not open a browser")
	f.StringVar(&redirectURI, "redirect-uri", "", "override the registered redirect URI")
	return cmd
}

// browserLogin runs the local HTTPS callback listener.
func (a *App) browserLogin(ctx context.Context, authURL, redirectURI, state string, noBrowser bool) (auth.CallbackResult, error) {
	server := &auth.CallbackServer{RedirectURI: redirectURI, State: state}
	// Bind before printing the URL so a port conflict fails before the user is
	// sent to the browser.
	if err := server.Start(); err != nil {
		return auth.CallbackResult{}, &Error{
			Code:     CodeAuth,
			Message:  err.Error(),
			Hint:     "Use `dk auth login --manual` if a local listener will not work here.",
			ExitCode: ExitAuth,
			Err:      err,
		}
	}
	defer server.Close()

	fmt.Fprintf(a.Err, "Open this URL to authorize dk:\n\n  %s\n\n", authURL)
	fmt.Fprintf(a.Err, "Waiting for the redirect to %s ...\n", redirectURI)
	fmt.Fprintf(a.Err, "Your browser will warn about the self-signed certificate on localhost; that is expected.\n")

	if !noBrowser {
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(a.Err, "(Could not open a browser automatically: %v)\n", err)
		}
	}

	result, err := server.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return auth.CallbackResult{}, &Error{
				Code:     CodeAuth,
				Message:  "timed out waiting for the DigiKey redirect",
				Hint:     "Re-run `dk auth login`, or use --manual to paste the redirect URL yourself.",
				ExitCode: ExitAuth,
				Err:      err,
			}
		}
		// Ctrl-C arrives here as context.Canceled, because Wait returns
		// ctx.Err(). It must pass through unwrapped: classify checks for an
		// *Error before it checks for context.Canceled, so wrapping this would
		// shadow the "cancelled" code and report an interrupted login as
		// auth_required — sending an agent off to ask a human to re-authorize
		// when nothing is wrong with the credentials.
		if errors.Is(err, context.Canceled) {
			return auth.CallbackResult{}, err
		}
		// Anything else here is DigiKey refusing the authorization — most often
		// the user declining consent. --manual reports that as an auth failure,
		// so this path must too, rather than exiting 1 for the same action.
		return auth.CallbackResult{}, &Error{
			Code:     CodeAuth,
			Message:  err.Error(),
			Hint:     "Re-run `dk auth login` and approve the request, or use --manual to paste the redirect URL yourself.",
			ExitCode: ExitAuth,
			Err:      err,
		}
	}
	return result, nil
}

// manualLogin prints the URL and reads the redirected URL back from stdin.
func (a *App) manualLogin(authURL, redirectURI string) (auth.CallbackResult, error) {
	fmt.Fprintf(a.Err, "Open this URL to authorize dk:\n\n  %s\n\n", authURL)
	fmt.Fprintf(a.Err, "After approving, your browser is redirected to %s with a ?code= parameter.\n", redirectURI)
	fmt.Fprintf(a.Err, "The page may fail to load; that is fine — copy the URL from the address bar.\n")
	fmt.Fprintf(a.Err, "The code expires 60 seconds after DigiKey issues it.\n\n")
	fmt.Fprint(a.Err, "Paste the redirected URL (or just the code): ")

	in := a.In
	if in == nil {
		in = os.Stdin
	}
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return auth.CallbackResult{}, &Error{
			Code:     CodeAuth,
			Message:  "no authorization code was provided",
			ExitCode: ExitAuth,
			Err:      err,
		}
	}

	result, err := auth.ParseAuthorizationCode(line)
	if err != nil {
		return auth.CallbackResult{}, &Error{
			Code:     CodeAuth,
			Message:  err.Error(),
			Hint:     "Paste the full URL your browser was redirected to, including the ?code=... part.",
			ExitCode: ExitAuth,
			Err:      err,
		}
	}
	return result, nil
}

// openBrowser launches the platform's default browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// AuthStatus is the JSON shape of `dk auth status`.
type AuthStatus struct {
	Environment       string `json:"environment"`
	BaseURL           string `json:"base_url"`
	ClientIDSet       bool   `json:"client_id_set"`
	ClientSecretSet   bool   `json:"client_secret_set"`
	AppTokenCached    bool   `json:"app_token_cached"`
	AppTokenExpiresAt string `json:"app_token_expires_at,omitempty"`
	// UserLoggedIn reports whether `dk list` commands will work.
	UserLoggedIn       bool   `json:"user_logged_in"`
	UserTokenExpiresAt string `json:"user_token_expires_at,omitempty"`
	HasRefreshToken    bool   `json:"has_refresh_token"`
	TokenFile          string `json:"token_file,omitempty"`
	ConfigFile         string `json:"config_file,omitempty"`
	CredentialsFile    string `json:"credentials_file,omitempty"`
	RedirectURI        string `json:"redirect_uri,omitempty"`
}

func newAuthStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show credential and token status",
		Long: `Report which credentials are configured and whether a list-capable token is
cached, for the environment that is currently active — "environment" in the
output names it, and everything below it describes that environment alone.
Exits 0 regardless of state; read "user_logged_in" to decide whether
"dk auth login" is still needed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			status := AuthStatus{
				Environment:     app.Cfg.Environment,
				BaseURL:         app.Cfg.BaseURL(),
				ClientIDSet:     app.Cfg.ClientID != "",
				ClientSecretSet: app.Cfg.ClientSecret != "",
				RedirectURI:     app.Cfg.RedirectURI,
			}
			if path, err := config.Path(); err == nil {
				status.ConfigFile = path
			}
			if path, err := config.CredentialsPath(app.Cfg.Environment); err == nil {
				status.CredentialsFile = path
			}

			store, err := app.Store()
			if err != nil {
				return err
			}
			status.TokenFile = store.Path()

			if tok, err := store.Get(auth.KindApp, app.Cfg.Environment); err == nil && tok != nil {
				status.AppTokenCached = tok.Valid(time.Now(), 0)
				status.AppTokenExpiresAt = tok.ExpiresAt.Format(time.RFC3339)
			}
			if tok, err := store.Get(auth.KindUser, app.Cfg.Environment); err == nil && tok != nil {
				status.HasRefreshToken = tok.RefreshToken != ""
				status.UserTokenExpiresAt = tok.ExpiresAt.Format(time.RFC3339)
				// A stored refresh token is enough: an expired access token is
				// renewed transparently on the next call.
				status.UserLoggedIn = status.HasRefreshToken || tok.Valid(time.Now(), 0)
			}

			t := output.KeyValueTable([][2]string{
				{"environment", status.Environment},
				{"api base url", status.BaseURL},
				{"client id set", fmt.Sprintf("%t", status.ClientIDSet)},
				{"client secret set", fmt.Sprintf("%t", status.ClientSecretSet)},
				{"search available", fmt.Sprintf("%t", status.ClientIDSet && status.ClientSecretSet)},
				{"lists available", fmt.Sprintf("%t", status.UserLoggedIn)},
				{"user token expires", status.UserTokenExpiresAt},
				{"refresh token stored", fmt.Sprintf("%t", status.HasRefreshToken)},
				{"config file", status.ConfigFile},
				{"credentials file", status.CredentialsFile},
				{"token file", status.TokenFile},
			})
			if err := app.Printer.Print(status, t); err != nil {
				return err
			}

			switch {
			case !status.ClientIDSet || !status.ClientSecretSet:
				app.Printer.PrintText(fmt.Sprintf("\nNext: set credentials for the %s environment with `dk config set client_id <id>`\nand `dk config set client_secret <secret>`. Switch environments with `dk env`.", status.Environment))
			case !status.UserLoggedIn:
				app.Printer.PrintText("\nSearch works now. Run `dk auth login` to enable `dk list` commands.")
			}
			return nil
		},
	}
}

func newAuthLogoutCommand(app *App) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Discard cached tokens",
		Long: `Discard the cached 3-legged token for the current environment, so "dk list"
commands require a new login. Client credentials in the config file are not
touched. Use --all to clear every cached token, including application tokens
and other environments.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := app.Store()
			if err != nil {
				return err
			}
			if all {
				if err := store.Clear(); err != nil {
					return err
				}
			} else if err := store.Delete(auth.KindUser, app.Cfg.Environment); err != nil {
				return err
			}
			// Logging out has to take the account-specific pricing with it,
			// both because it is the account's and because the next login may
			// be a different one.
			dropCachedResponses()

			payload := map[string]any{"status": "logged out", "all": all, "environment": app.Cfg.Environment}
			t := &output.Table{Headers: []string{"FIELD", "VALUE"}}
			t.AddRow("status", "logged out")
			t.AddRow("environment", app.Cfg.Environment)
			t.AddRow("cleared all tokens", fmt.Sprintf("%t", all))
			return app.Printer.Print(payload, t)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "clear every cached token, not just the current environment's user token")
	return cmd
}

func tokenPathOrEmpty(app *App) string {
	store, err := app.Store()
	if err != nil {
		return ""
	}
	return store.Path()
}
