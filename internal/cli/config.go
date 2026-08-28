package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/config"
	"github.com/jacobcase/dk/internal/output"
)

func newConfigCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View and edit dk's configuration",
		Long: `View and edit dk's configuration.

Settings resolve in this order, highest first:

  1. command-line flags        --client-id, --site, ...
  2. environment variables     DIGIKEY_CLIENT_ID, DIGIKEY_CLIENT_SECRET, ...
  3. the stored files          ` + "`dk config path`" + `
  4. built-in defaults

Settings are stored in two places. The client id, client secret, redirect URI,
and account id belong to one registered DigiKey app, so they live in a
credentials file per environment; everything else is shared. Writes go to
whichever environment is active — check it with ` + "`dk env`" + ` before setting a
credential, and switch with ` + "`dk env sandbox`" + ` rather than through this command.

Both files are written with 0600 permissions because one holds your client
secret. If you would rather not store the secret on disk at all, set
DIGIKEY_CLIENT_SECRET in the environment instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		newConfigShowCommand(app),
		newConfigSetCommand(app),
		newConfigPathCommand(app),
	)
	return cmd
}

func newConfigShowCommand(app *App) *cobra.Command {
	var reveal bool

	cmd := &cobra.Command{
		Use:     "show",
		Aliases: []string{"get", "list"},
		Short:   "Show the effective configuration",
		Long: `Show the configuration as dk resolved it, for the environment that is
currently active. The client secret is masked unless --reveal is passed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			values := app.Cfg.Redacted()
			if reveal {
				values["client_secret"] = app.Cfg.ClientSecret
			}

			t := &output.Table{Headers: []string{"KEY", "VALUE"}}
			for _, key := range config.ShowKeys() {
				t.AddRow(key, values[key])
			}
			return app.Printer.Print(values, t)
		},
	}
	cmd.Flags().BoolVar(&reveal, "reveal", false, "print the client secret in clear text")
	return cmd
}

func newConfigSetCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Long: `Write a value to the stored configuration.

  dk config set client_id AbCdEf123
  dk config set client_secret s3cr3t
  dk config set locale.currency EUR

These keys belong to one registered DigiKey app and are written to the active
environment's credentials file: ` + strings.Join(config.PerEnvironmentKeys(), ", ") + `.
Run ` + "`dk env`" + ` first if you are not sure which environment that is. The
environment itself is changed with ` + "`dk env`" + `, not here.

Valid keys: ` + strings.Join(config.Keys(), ", "),
		Args: cobra.ExactArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return config.Keys(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]

			// Read the file directly rather than using app.Cfg: config.Load overlays
			// environment variables, and baking those into the file would silently
			// persist a secret the user only meant to pass for one run.
			stored, err := config.LoadFile()
			if err != nil {
				return err
			}
			if err := stored.Set(key, value); err != nil {
				return usageErrorf("%s", err.Error())
			}
			if err := config.Save(stored); err != nil {
				return err
			}

			// Report the file the value actually landed in. Save writes both,
			// but naming the shared file after storing a client secret would
			// point anyone auditing where their secret went at the wrong one.
			path, _ := config.Path()
			if config.IsPerEnvironmentKey(key) {
				if p, err := config.CredentialsPath(stored.Environment); err == nil {
					path = p
				}
			}

			shown := value
			if strings.EqualFold(key, "client_secret") {
				shown = stored.Redacted()["client_secret"]
			}

			payload := map[string]string{
				"key":         strings.ToLower(strings.TrimSpace(key)),
				"value":       shown,
				"file":        path,
				"environment": stored.Environment,
			}
			t := &output.Table{Headers: []string{"KEY", "VALUE", "FILE"}}
			t.AddRow(payload["key"], shown, path)
			return app.Printer.Print(payload, t)
		},
	}
}

func newConfigPathCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config, credentials, and token file locations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.Path()
			if err != nil {
				return err
			}
			credsPath, err := config.CredentialsPath(app.Cfg.Environment)
			if err != nil {
				return err
			}
			store, err := app.Store()
			if err != nil {
				return err
			}

			payload := map[string]string{
				"config_file":      cfgPath,
				"credentials_file": credsPath,
				"token_file":       store.Path(),
				"environment":      app.Cfg.Environment,
			}
			t := &output.Table{Headers: []string{"FILE", "PATH"}}
			t.AddRow("config", cfgPath)
			// Named for the environment it belongs to: the path alone already
			// says which, but a caller reading the table should not have to
			// parse a filename to find out.
			t.AddRow("credentials ("+app.Cfg.Environment+")", credsPath)
			t.AddRow("tokens", store.Path())
			return app.Printer.Print(payload, t)
		},
	}
}

func newVersionCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the dk version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]string{"version": Version}
			t := &output.Table{Headers: []string{"VERSION"}}
			t.AddRow(Version)
			return app.Printer.Print(payload, t)
		},
	}
}
