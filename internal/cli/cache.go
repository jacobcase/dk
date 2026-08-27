package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/cache"
	"github.com/jacobcase/dk/internal/config"
	"github.com/jacobcase/dk/internal/output"
)

func newCacheCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and clear the cached API responses",
		Long: `dk stores the responses to catalog and list reads on disk and serves an
identical read from that store while it is fresh, so that running a query twice
— once to look at, once to pipe somewhere — costs one API call rather than two.

Only reads are cached, and only successful ones. Writing to a list drops the
cached list reads immediately, so ` + "`dk list show`" + ` never reports what a
` + "`dk list add`" + ` just changed. Errors are never cached: a rate-limit or an
expired token would otherwise outlive the condition that caused it.

  --no-cache                   ask DigiKey even if an entry is fresh, and replace it
  --cache-ttl 30s              shorten the freshness window for one invocation
  --cache-ttl 0                switch the cache off for one invocation
  DK_CACHE_TTL=1h              per-shell override
  dk config set cache_ttl 0    switch it off for good

Entries hold pricing tied to your account, so they are written 0600 and expire
on their own. "dk cache clear" removes them now.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		newCacheStatusCommand(app),
		newCacheClearCommand(app),
	)
	return cmd
}

// cacheStatusView is the JSON shape of `dk cache status`.
type cacheStatusView struct {
	// Enabled is false when the TTL resolved to zero. Entries can still be
	// non-zero in that case: turning the cache off stops it being read, it does
	// not delete what earlier runs wrote.
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"`
	TTL     string `json:"ttl"`
	// TTLSeconds is a float, not an int. Truncating 500ms to 0 would report the
	// value that means "off" everywhere else in this feature, so a caller
	// branching on it would conclude the opposite of Enabled.
	TTLSeconds float64 `json:"ttl_seconds"`
	Entries    int     `json:"entries"`
	Fresh      int     `json:"fresh_entries"`
	Bytes      int64   `json:"bytes"`
}

func newCacheStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"show", "path"},
		Short:   "Report where cached responses live and how many are held",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := config.CacheDir()
			if err != nil {
				return err
			}
			// A bad cache_ttl is worth reporting from the command whose subject
			// is the cache configuration, and the hint names the setting to fix.
			ttl, err := app.CacheTTL()
			if err != nil {
				return err
			}
			stats, err := cache.Stat(dir, ttl)
			if err != nil {
				return err
			}

			view := cacheStatusView{
				Enabled:    ttl > 0,
				Dir:        dir,
				TTL:        ttl.String(),
				TTLSeconds: ttl.Seconds(),
				Entries:    stats.Entries,
				Fresh:      stats.Fresh,
				Bytes:      stats.Bytes,
			}

			t := &output.Table{Headers: []string{"KEY", "VALUE"}}
			t.AddRow("enabled", strconv.FormatBool(view.Enabled))
			t.AddRow("dir", view.Dir)
			t.AddRow("ttl", view.TTL)
			t.AddRow("entries", strconv.Itoa(view.Entries))
			t.AddRow("fresh", strconv.Itoa(view.Fresh))
			t.AddRow("bytes", strconv.FormatInt(view.Bytes, 10))
			return app.Printer.Print(view, t)
		},
	}
}

// cacheClearView is the JSON shape of `dk cache clear`.
type cacheClearView struct {
	Dir string `json:"dir"`
	// Removed is what was there before the clear, counted first so the number
	// reported is the number actually deleted.
	Removed int   `json:"removed"`
	Bytes   int64 `json:"bytes"`
}

func newCacheClearCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "clear",
		Aliases: []string{"purge", "rm"},
		Short:   "Delete every cached response",
		Long: `Delete every cached response.

Nothing is lost that cannot be fetched again; the only cost is that the next
read of each thing goes to DigiKey. Reach for it when a response looks stale and
--no-cache is not enough, or to reclaim the disk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := config.CacheDir()
			if err != nil {
				return err
			}
			// A cache_ttl this command cannot parse is not a reason to refuse
			// to clear: clearing is one of the two ways out of a broken
			// configuration, and the TTL only feeds a fresh count that clear
			// does not print.
			ttl, _ := app.CacheTTL()
			// Counted before the removal: afterwards there is nothing left to
			// count, and reporting zero would say nothing about what was freed.
			stats, err := cache.Stat(dir, ttl)
			if err != nil {
				return err
			}
			if err := cache.Clear(dir); err != nil {
				return err
			}

			view := cacheClearView{Dir: dir, Removed: stats.Entries, Bytes: stats.Bytes}
			t := &output.Table{Headers: []string{"REMOVED", "BYTES", "DIR"}}
			t.AddRow(fmt.Sprint(view.Removed), fmt.Sprint(view.Bytes), dir)
			return app.Printer.Print(view, t)
		},
	}
}
