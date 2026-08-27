package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/config"
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

RESPONSE CACHE
  Successful catalog and list reads are cached on disk (default ` + config.DefaultCacheTTL + `) and
  an identical read is served from there without touching the API. Re-running a
  query to reshape, filter, or pipe its output is therefore free: run it again
  rather than contriving a way to avoid a second call.

  Only reads are cached, and only successful ones — a rate-limit or an expired
  token is never stored. Writing to a list drops the cached list reads at once,
  so "dk list show" always reflects a "dk list add" that just ran. The read a
  write depends on is never cached either: "dk list rm" and "dk list set"
  resolve part numbers against live contents, and the part count behind
  "dk list delete"'s --force guard is always asked of the API.

    --no-cache        ask DigiKey even when a fresh entry exists, and replace it
    --cache-ttl 0     switch the cache off for one invocation
    dk cache status   where the entries live and how many are held
    dk cache clear    delete all of them

  Stock and price move, and an entry may be up to the TTL old. Pass --no-cache
  before quoting a figure a human is about to act on.

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
    usage_error, credentials_missing, config_invalid, auth_required, not_found,
    ambiguous_list, rate_limited, api_error, cancelled, error

  Ctrl-C (or SIGTERM) exits 1 with code "cancelled", so an interrupted run is
  distinguishable from a genuine failure.

AUTHENTICATION
  Search commands need only a client id and secret (2-legged OAuth):
      DIGIKEY_CLIENT_ID, DIGIKEY_CLIENT_SECRET
    or: dk config set client_id <id>; dk config set client_secret <secret>

  DigiKey documents X-DIGIKEY-Account-Id as "required to receive a successful
  response" under 2-legged OAuth. dk sends it only when account_id is set. If a
  product command fails in a way the error does not explain, set it:
      dk config set account_id <id>

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
                           [--recommended] [--alternate-packaging]
  Accepts a DigiKey or manufacturer part number. The view flags are mutually
  exclusive.

  Plain, --variations, and --parameters all query the same endpoint, and in JSON
  mode all three return the identical full view (parameters, price breaks,
  variations) — the flag only picks which section the TABLE shows.

  The other three flags query different endpoints and each returns its own
  object, all in dk's snake_case:
    --substitutes          what you could buy INSTEAD of this part
      {"part_number","substitutes":[{"substitute_type","digikey_part_number",
       "manufacturer_part_number","manufacturer","description","unit_price",
       "quantity_available","product_url"}],"count"}
    --recommended          what others bought alongside it
      {"part_number","recommended":[{"digikey_part_number",
       "manufacturer_part_number","manufacturer","description","unit_price",
       "quantity_available","product_url"}]}
    --alternate-packaging  the same part in other packaging
      {"part_number","packaging":[ ...same fields as substitutes, minus
       substitute_type... ]}

  unit_price is a preformatted STRING in --substitutes and
  --alternate-packaging, and a NUMBER in --recommended. That mirrors DigiKey,
  which returns the two differently; dk reports the difference rather than
  parsing currency text to hide it. Check the type before doing arithmetic.

  Add --raw to any of these to get DigiKey's untouched payload instead, in its
  original PascalCase and including fields dk does not model.

WHAT ELSE DO I NEED TO BUY?
  dk related <part-number> [--kind mating|kits|accessories|for-use-with|all]

  Returns the products DigiKey relates to a part: the other half of a connector
  pair, kits containing it, crimpers and tools. Distinct from --substitutes:
  "related" is what you need ALONGSIDE the part, not instead of it.

  JSON: {"part_number","products":[{"relation","digikey_part_number",
         "manufacturer_part_number","manufacturer","description","unit_price",
         "quantity_available"}],"counts":{"mating":N,...}}
  Check .counts to see if a mating half exists without scanning the list.
  unit_price is a preformatted STRING here, not a number.
  Most parts have no associations; an empty result is normal, not an error.

COSTING A QUANTITY
  dk pricing <part-number> --qty N [--packaging CT|DKR]

  Answers "I need N of these — what do I order and what does it cost?" Returns
  every way DigiKey will sell that quantity.

  JSON: {"part_number","requested_quantity","currency",
         "best":{...} | null,
         "options":[{"option","order_quantity","forced_up","total_price",
                     "in_stock",
                     "products":[{"digikey_part_number","packaging","quantity",
                                  "unit_price","extended_price",
                                  "minimum_order_quantity","quantity_available",
                                  "in_stock","product_status"}]}]}

  Read .best first: the cheapest option that is actually orderable, or null if
  none are. Then read .forced_up — true when order_quantity exceeds what was
  asked for. Always report order_quantity and total_price to the human, not
  just a unit price.

  .option is DigiKey's own label for the option, one of:
    Exact                 buys exactly the requested quantity
    MinimumOrderQuantity  a minimum forced the quantity up
    BetterValue           costs LESS than the exact option while buying more
    MaxOrderQuantity      capped above the requested quantity
  BetterValue is worth surfacing unprompted: 5000 on a reel can cost less than
  4500 on cut tape. Nothing you can compute from a single option produces it.

  An option can hold SEVERAL products: a quantity past a standard reel is
  filled with the reel plus a cut-tape remainder, priced as one option. Sum
  .products[].quantity, or read .order_quantity, but never quote one product's
  price as the option's cost. Every figure inside a product describes that
  product only.

  quantity_available and product_status are joined from the product record —
  the pricing endpoint returns no stock — so dk makes two calls here. in_stock
  on a product means its own line can be filled; on an option it means every
  product in it can.

  --packaging filters the returned options rather than asking DigiKey for a
  preference. An option mixing a reel with a cut-tape remainder is not a CT
  option and is filtered out of --packaging CT.

DATASHEETS AND DOCUMENTS
  The primary datasheet URL is already in search and product output as
  datasheet_url — no extra call needed. For everything else:

    dk docs <part-number> [--type datasheet] [--download DIR] [--overwrite]

  Lists datasheets, manuals, reference designs, CAD models, photos, PCNs, and
  videos, each with type, title, and url. --download writes them to disk and
  reports the path per document; a document that fails to download gets an
  "error" field rather than aborting the rest.

  To read a datasheet, fetching the url directly is usually enough. Use
  --download when you want the PDF on disk (large datasheets are easier to
  handle as a file than through a fetch tool).

LISTS
  dk list ls                                  show all lists
  dk list create <name> [--tag T] [--auto-rename]
  dk list show <list> [--limit N] [--offset N] [--raw]
                                              parts with live pricing and stock
  dk list add <list> <part[:qty]>... [--qty N] [--ref R1,R2] [--note TEXT]
                                              [--from-json FILE|-] [--verify]
  dk list set <list> <unique-id-or-part> [--qty N] [--ref R] [--note TEXT]
                                              [--customer-ref C]
  dk list rm <list> <unique-id-or-part>...
  dk list copy <source-list> <new-name> [--auto-rename]
  dk list rename <list> <new-name>
  dk list delete <list> [--force]
  dk list export <list> --output csv          BOM-shaped columns

  <list> is a list NAME or a list ID. Names match exactly first, then
  case-insensitively; an ambiguous name is an error (code "ambiguous_list")
  with the candidate ids in .error.details.candidates.

  "dk list show" and "dk list export" page through the whole list, so
  estimated_total and the exported rows cover all of it. Passing --limit or
  --offset to "dk list show" fetches a single page instead: compare .returned
  against .total_parts to tell, and remember that estimated_total then covers
  only the returned page.

  Bulk add with per-part metadata:
    echo '[{"part":"1276-1000-1-ND","quantity":10,"reference":"C1-C10",
            "note":"input decoupling"}]' | dk list add "My List" --from-json -

  Use "dk list set" to change a quantity, NOT rm followed by add: set edits in
  place and keeps the unique id, reference designators, and notes. Only the
  flags you pass are changed. An ambiguous target is an error (unlike rm, which
  applies to every match), so pass the unique id when a part appears twice.

  "dk list copy" clones a list, which is the clean way to start a rev B without
  disturbing a BOM you have already reviewed.

  --verify checks each part against the catalog first and skips ones that do not
  resolve. Without it, DigiKey silently accepts unknown part numbers and marks
  the line unmatched. Always check "matched" in "dk list show" output, or the
  "unmatched_parts" count.

CHOOSING A SEARCH STRATEGY
  DigiKey's parametric data describes COMPONENTS well and BOARDS poorly. Which
  strategy works depends on what is being asked for:

  Discrete components (resistors, capacitors, connectors, terminals, ICs)
    Parametric filtering works well. Attributes like stud size, tolerance,
    dielectric, pin count, and supply voltage are real parameters.
      dk filters "ring terminal heat shrink"
      dk search "ring terminal" --param "Stud/Tab Size=#10" --param "Insulation=Heat Shrink"

  Dev boards and modules (Feather, QT Py, Pico, breakout sensor boards)
    Parametric coverage is thin: GPIO count, connector type (USB-C vs micro-B),
    and ecosystem branding (STEMMA QT, Qwiic, Grove) are usually NOT parameters.
    They live in the title and detailed_description. So:
      - lead with keywords, including the branding term itself
      - use --manufacturer (Adafruit, SparkFun, Seeed) to narrow hard
      - use --full and read detailed_description and parameters to verify
      - expect to supply candidate part families from your own knowledge, then
        use dk to confirm they exist, are in stock, and cost what you expect
      Example: "STEMMA QT DC voltage sensor" is a keyword search plus knowing
      that INA219/INA260/ADS1115 breakouts exist; dk verifies, it does not
      discover.

  Mixed requirements
    Split them. Filter parametrically on what DigiKey models, keyword-match the
    rest, then verify each candidate with dk product --full or its datasheet.

  When a search returns thousands of matches, run dk filters before guessing at
  more keywords: the product_count on each value shows what will actually narrow
  it. When a search returns nothing, the keywords are likely over-specified —
  drop the branding or packaging terms first.

RECOMMENDED AGENT WORKFLOW
  1. dk auth status --output json                  confirm .user_logged_in
  2. dk search "<part description>" --in-stock --limit 5 --output json
     If too many matches, narrow parametrically:
       dk filters "<part description>" --output json
       dk search "<part description>" --param "<name>=<value>" --in-stock
  3. Pick a part; confirm what buying your quantity really means:
       dk pricing <dkpn> --qty <n> --output json   check .best and .forced_up
     Add --no-cache before reporting a price or a stock figure to a human:
     .best names the cheapest option in stock as of the cached reply, which
     may be up to the cache TTL old.
  4. For connectors and terminals, check what else is needed:
       dk related <dkpn> --output json             mating half, crimper, kit
  5. dk list create "<project name>"               once per project
  6. dk list add "<project>" <dkpn>:<qty> --ref <designators> --verify
     Correcting a quantity later: dk list set "<project>" <dkpn> --qty <n>
  7. dk list show "<project>" --output json        verify matched and totals
     estimated_total is priced from that same window; pass --no-cache before
     handing a total to a human.
  8. Report the list URL to the human for review and ordering.

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
