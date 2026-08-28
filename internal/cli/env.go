package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/config"
	"github.com/jacobcase/dk/internal/output"
)

// EnvStatus is the JSON shape of `dk env` and `dk env <name>`.
//
// The setting form reports the same shape as the reading form on purpose: a
// caller that switches environments and one that only asks which is active
// parse one structure, not two.
type EnvStatus struct {
	Environment     string `json:"environment"`
	BaseURL         string `json:"base_url"`
	ClientIDSet     bool   `json:"client_id_set"`
	ClientSecretSet bool   `json:"client_secret_set"`
	CredentialsFile string `json:"credentials_file,omitempty"`
	ConfigFile      string `json:"config_file,omitempty"`
}

// EnvListEntry is one row of `dk env list`.
type EnvListEntry struct {
	Environment     string `json:"environment"`
	Active          bool   `json:"active"`
	BaseURL         string `json:"base_url"`
	CredentialsSet  bool   `json:"credentials_set"`
	CredentialsFile string `json:"credentials_file,omitempty"`
}

func newEnvCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env [production|sandbox]",
		Short: "Show or switch the active DigiKey environment",
		Long: `Show which DigiKey environment dk is talking to, or switch to the other one.

  dk env              print the active environment
  dk env prod         switch to production
  dk env sandbox      switch to the sandbox
  dk env list         show both environments and which has credentials

The environment is persistent state, not a per-command flag: it is stored in
the config file and stays in effect until changed. Each environment keeps its
own credentials file, because DigiKey scopes a client id to one deployment —
a production client id is rejected by the sandbox host with "clientId invalid
for requested resource". Switching environments therefore switches credentials
too, and neither set overwrites the other.

Cached tokens are already kept per environment, so switching back does not
require logging in again.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: config.Environments(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return app.printEnvStatus(app.Cfg.Environment)
			}

			env, err := config.ParseEnvironment(args[0])
			if err != nil {
				return usageErrorf("%s", err.Error())
			}
			if err := config.SaveEnvironment(env); err != nil {
				return err
			}
			// app.Cfg was resolved before this ran and still names the old
			// environment; report the new one rather than re-reading, so the
			// output describes what was just written.
			return app.printEnvStatus(env)
		},
	}
	cmd.AddCommand(newEnvListCommand(app))
	return cmd
}

// newEnvListCommand makes `dk env list` a real subcommand, so it appears in
// help and completion. Cobra matches a subcommand name before the parent's own
// Args run, so the parent never sees "list" as an environment to switch to.
func newEnvListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show both environments and which one has credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.printEnvList()
		},
	}
}

func (a *App) printEnvStatus(environment string) error {
	cfg, err := a.envConfig(environment)
	if err != nil {
		return err
	}

	status := EnvStatus{
		Environment:     environment,
		BaseURL:         cfg.BaseURL(),
		ClientIDSet:     cfg.ClientID != "",
		ClientSecretSet: cfg.ClientSecret != "",
	}
	if path, err := config.CredentialsPath(environment); err == nil {
		status.CredentialsFile = path
	}
	if path, err := config.Path(); err == nil {
		status.ConfigFile = path
	}

	t := output.KeyValueTable([][2]string{
		{"environment", status.Environment},
		{"api base url", status.BaseURL},
		{"client id set", fmt.Sprintf("%t", status.ClientIDSet)},
		{"client secret set", fmt.Sprintf("%t", status.ClientSecretSet)},
		{"credentials file", status.CredentialsFile},
		{"config file", status.ConfigFile},
	})
	if err := a.Printer.Print(status, t); err != nil {
		return err
	}

	if !status.ClientIDSet || !status.ClientSecretSet {
		a.Printer.PrintText(fmt.Sprintf("\nNo credentials stored for %s. Register an app at https://developer.digikey.com,\nthen run `dk config set client_id <id>` and `dk config set client_secret <secret>`.", status.Environment))
	}
	return nil
}

func (a *App) printEnvList() error {
	environments := config.Environments()
	entries := make([]EnvListEntry, 0, len(environments))
	t := &output.Table{Headers: []string{"ENVIRONMENT", "ACTIVE", "CREDENTIALS", "API BASE URL"}}

	for _, env := range environments {
		cfg, err := a.envConfig(env)
		if err != nil {
			return err
		}
		entry := EnvListEntry{
			Environment:    env,
			Active:         env == a.Cfg.Environment,
			BaseURL:        cfg.BaseURL(),
			CredentialsSet: cfg.ClientID != "" && cfg.ClientSecret != "",
		}
		if path, err := config.CredentialsPath(env); err == nil {
			entry.CredentialsFile = path
		}
		entries = append(entries, entry)

		active := ""
		if entry.Active {
			active = "*"
		}
		t.AddRow(entry.Environment, active, fmt.Sprintf("%t", entry.CredentialsSet), entry.BaseURL)
	}
	return a.Printer.Print(entries, t)
}

// envConfig returns the configuration for one environment.
//
// The active environment is reported from a.Cfg, the config the run already
// resolved, so that a client id supplied through DIGIKEY_CLIENT_ID counts as
// set — it is what the next command will really send. Any other environment is
// read from disk, since the variables in this process were never meant for it.
func (a *App) envConfig(environment string) (config.Config, error) {
	if environment == a.Cfg.Environment {
		return a.Cfg, nil
	}
	return config.LoadEnvironment(environment)
}
