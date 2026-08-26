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

  1. command-line flags        --client-id, --env, --site, ...
  2. environment variables     DIGIKEY_CLIENT_ID, DIGIKEY_CLIENT_SECRET, ...
  3. the config file           ` + "`dk config path`" + `
  4. built-in defaults

The config file is written with 0600 permissions because it can hold your client
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
		Long:    `Show the configuration as dk resolved it. The client secret is masked unless --reveal is passed.`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			values := app.Cfg.Redacted()
			if reveal {
				values["client_secret"] = app.Cfg.ClientSecret
			}

			t := &output.Table{Headers: []string{"KEY", "VALUE"}}
			for _, key := range config.Keys() {
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
		Long: `Write a value to the config file.

  dk config set client_id AbCdEf123
  dk config set client_secret s3cr3t
  dk config set environment sandbox
  dk config set locale.currency EUR

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

			path, _ := config.Path()
			shown := value
			if strings.EqualFold(key, "client_secret") {
				shown = stored.Redacted()["client_secret"]
			}

			payload := map[string]string{"key": strings.ToLower(key), "value": shown, "file": path}
			t := &output.Table{Headers: []string{"KEY", "VALUE", "FILE"}}
			t.AddRow(payload["key"], shown, path)
			return app.Printer.Print(payload, t)
		},
	}
}

func newConfigPathCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config and token file locations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.Path()
			if err != nil {
				return err
			}
			store, err := app.Store()
			if err != nil {
				return err
			}

			payload := map[string]string{"config_file": cfgPath, "token_file": store.Path()}
			t := &output.Table{Headers: []string{"FILE", "PATH"}}
			t.AddRow("config", cfgPath)
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
