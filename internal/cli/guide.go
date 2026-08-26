package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// guideText is a condensed operating manual aimed at programs driving dk.
//
// It exists because an agent that shells out to an unfamiliar CLI otherwise has
// to discover behavior by trial and error. One `dk guide` call establishes the
// contract: what needs auth, what the exit codes mean, and what the JSON looks
// like.
const guideText = `dk — DigiKey CLI

PURPOSE
  Search the DigiKey catalog and stage parts into DigiKey "MyLists" order lists.
  dk NEVER places an order. Lists are staged for a human to review and buy on
  digikey.com.

OUTPUT CONTRACT
  --output json    always valid JSON on stdout; nothing else is written there
  --output table   aligned columns for humans (the default on a terminal)
  --output csv     the same columns, comma-separated
  Default is table when stdout is a terminal and json otherwise, so a program
  capturing stdout gets JSON without passing a flag. Pass --output json anyway
  if you want certainty.

  Progress notes and prompts go to stderr, never stdout.

EXIT CODES
  0  success
  1  generic failure
  2  bad flags or arguments
  3  authentication required or rejected      -> run: dk auth login
  4  the product or list was not found
  5  DigiKey rate limit hit (retry later)
  6  configuration missing or invalid         -> set client id / secret

ERRORS IN JSON MODE
  Failures print a single object on stderr:
    {"error":{"code":"auth_required","message":"...","hint":"...","exit_code":3}}
  Branch on .error.code, not on the message text. Codes:
    usage_error, credentials_missing, auth_required, not_found,
    ambiguous_list, rate_limited, api_error, error

AUTHENTICATION
  Search commands need only a client id and secret (2-legged OAuth):
      DIGIKEY_CLIENT_ID, DIGIKEY_CLIENT_SECRET
    or: dk config set client_id <id>; dk config set client_secret <secret>

  List commands additionally need a 3-legged token, because lists belong to a
  DigiKey user account. That requires a ONE-TIME interactive browser login:
      dk auth login
  An agent cannot perform this step. If dk exits 3 with code "auth_required",
  stop and ask the human to run "dk auth login". The refresh token does not
  expire, so this is needed only once per machine.

  Check before starting work:
      dk auth status --output json     -> .user_logged_in tells you if lists work

SEARCH
  dk search <keywords...> [--limit N] [--offset N] [--in-stock] [--sort price]
                          [--manufacturer NAME|ID] [--category NAME|ID]
                          [--rohs] [--no-marketplace] [--exact] [--full]

  Keywords are joined with spaces; quoting is optional.
  --limit is capped at 50 by DigiKey. Page with --offset.
  --manufacturer and --category accept names, not just numeric ids.

  JSON: {"query","total_matches","returned","offset","currency","products":[...]}
  Each product has: digikey_part_number, manufacturer_part_number, manufacturer,
  description, unit_price, quantity_available, minimum_order_quantity,
  packaging, status, orderable, datasheet_url, product_url.

  IMPORTANT: digikey_part_number is packaging-specific. The same part has a
  different number for cut tape vs. tape & reel. Use
  "dk product <part> --variations" to see all of them before committing to one.

PARAMETRIC FILTERING
  DigiKey has no endpoint listing the filters for a category. Filters are
  discovered from a search response, so narrowing is a two-step loop:

    dk filters <keywords...>                      what can I filter on?
      [--parameter NAME|ID]                       every value of one parameter
      [--category NAME|ID] [--in-stock]
      [--values N | --all-values]                 table value cap (JSON has all)

    dk search <keywords...> --param "NAME=VALUE"  apply it

  Example loop:
    dk filters "0603 ceramic capacitor" --output json
    dk filters "0603 ceramic capacitor" --parameter Capacitance --output json
    dk search "0603 ceramic capacitor" \
      --param "Capacitance=0.1 µF" --param "Tolerance=±10%" --in-stock

  filters JSON: {"query","total_matches","category":{"id","name"},
                 "top_categories":[...],"parameters":[{"parameter_id",
                 "parameter_name","parameter_type","category_id","value_count",
                 "values":[{"value_id","value_name","product_count",
                 "range_type"}]}],"manufacturers","packaging","status","series"}

  Rules that matter:
  - product_count on each value tells you how much that choice narrows the
    search. Use it to pick a filter that actually helps.
  - --param matches names case-insensitively; a unique substring is enough. A
    raw value_id from "dk filters" also works.
  - Several values on ONE parameter are OR-ed:  --param "Resistance=10 kOhms,4.7 kOhms"
    Separate --param flags are AND-ed.
  - If a value itself contains a comma (e.g. "C0G, NP0"), repeat the flag
    instead of comma-joining: --param "X=C0G, NP0" works, and repeating
    --param for the same name merges the values.
  - Parameter ids are scoped to a category, so --param implies one. It is
    inferred from the keywords; pass --category if the inference is wrong or if
    dk reports that the parameters span more than one category.
  - A wrong parameter or value name exits 2 and the error lists what IS
    available, so you can correct without re-running "dk filters".
  - values whose range_type is Min/Max/Range are synthetic bounds DigiKey adds
    to numeric parameters, not discrete choices.
  - --param costs one extra API call for discovery.

PRODUCT DETAIL
  dk product <part-number> [--variations] [--parameters] [--substitutes]
  Accepts a DigiKey or manufacturer part number. In JSON mode the full view
  (parameters, price breaks, variations) is always returned regardless of flags.

LISTS
  dk list ls                                  show all lists
  dk list create <name> [--tag T] [--auto-rename]
  dk list show <list>                         parts with live pricing and stock
  dk list add <list> <part[:qty]>... [--qty N] [--ref R1,R2] [--note TEXT]
                                              [--from-json FILE|-] [--verify]
  dk list rm <list> <unique-id-or-part>...
  dk list rename <list> <new-name>
  dk list delete <list> [--force]
  dk list export <list> --output csv          BOM-shaped columns

  <list> is a list NAME or a list ID. Names match exactly first, then
  case-insensitively; an ambiguous name is an error (code "ambiguous_list")
  with the candidate ids in .error.details.candidates.

  Bulk add with per-part metadata:
    echo '[{"part":"1276-1000-1-ND","quantity":10,"reference":"C1-C10",
            "note":"input decoupling"}]' | dk list add "My List" --from-json -

  --verify checks each part against the catalog first and skips ones that do not
  resolve. Without it, DigiKey silently accepts unknown part numbers and marks
  the line unmatched. Always check "matched" in "dk list show" output, or the
  "unmatched_parts" count.

RECOMMENDED AGENT WORKFLOW
  1. dk auth status --output json                  confirm .user_logged_in
  2. dk search "<part description>" --in-stock --limit 5 --output json
     If too many matches, narrow parametrically:
       dk filters "<part description>" --output json
       dk search "<part description>" --param "<name>=<value>" --in-stock
  3. Pick a part; confirm packaging with dk product <dkpn> --variations
  4. dk list create "<project name>"               once per project
  5. dk list add "<project>" <dkpn>:<qty> --ref <designators> --verify
  6. dk list show "<project>" --output json        verify matched and totals
  7. Report the list URL to the human for review and ordering.

NOTES
  - Prices exclude shipping and tax. estimated_total is a rough figure and skips
    any line DigiKey could not price.
  - Marketplace items ship separately from the supplier; --no-marketplace
    excludes them.
  - Use --env sandbox against DigiKey's sandbox host. Sandbox data is not real.
`

func newGuideCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "Print a condensed reference for scripts and LLM agents",
		Long: `Print a condensed operating manual: the output contract, exit codes, error
shapes, which commands need which authentication, and a recommended workflow.

Intended as the first thing an automated caller reads.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Always plain text: this is documentation, not a result set, and
			// wrapping it in JSON would only make it harder to read.
			_, err := fmt.Fprint(app.Out, guideText)
			return err
		},
	}
}
