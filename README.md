# dk — DigiKey CLI

[![CI](https://github.com/jacobcase/dk/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/jacobcase/dk/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/jacobcase/dk/branch/master/graph/badge.svg)](https://codecov.io/gh/jacobcase/dk)

A Go CLI for searching the DigiKey catalog and staging parts into DigiKey
"MyLists" order lists.

Built to be driven by a human *or* by another program. Every command accepts
`--output json`, failures carry distinct exit codes, and errors in JSON mode are
structured objects with a stable `code` field.

**dk never places an order.** It builds lists for you to review and buy manually
on digikey.com.

```
dk search "0.1uF 0603 X7R 50V" --in-stock --limit 5
dk list create "Bench PSU rev A"
dk list add "Bench PSU rev A" 1276-1000-1-ND:10 --ref C1-C10
dk list show "Bench PSU rev A"
```

## Install

```
go install github.com/jacobcase/dk/cmd/dk@latest
```

Or from a clone:

```
make install     # builds and installs to $(go env GOPATH)/bin
make build       # just builds ./bin/dk
```

## Setup

### 1. Register an app with DigiKey

Go to <https://developer.digikey.com>, create an organization and a **Production**
app, then subscribe that app to:

- **Product Information** — for `dk search`, `dk product`, `dk categories`, `dk manufacturers`
- **MyLists** — for `dk list ...`

Set the app's **OAuth Callback / Redirect URI** to exactly:

```
https://localhost:8139/digikey_callback
```

(You can use a different one and set it with `dk config set redirect_uri <uri>`,
but DigiKey requires HTTPS — plain `http://localhost` is rejected.)

### 2. Give dk your credentials

Either environment variables:

```
export DIGIKEY_CLIENT_ID=...
export DIGIKEY_CLIENT_SECRET=...
```

or the config file (written `0600`):

```
dk config set client_id <id>
dk config set client_secret <secret>
```

`dk config path` prints where the config, credentials, and token files live.
Credentials belong to one environment — the commands above store them for
whichever is active, production unless you have switched. See
[Environments](#environments).

### 3. Log in (only needed for lists)

Search works immediately. Lists need a one-time browser login:

```
dk auth login
```

dk starts a local HTTPS listener on the redirect URI and opens your browser.
The listener uses a self-signed certificate, so your browser will warn once —
clicking through is expected. On a headless machine use `dk auth login --manual`,
which prints the URL and reads the redirected URL back from stdin.

DigiKey's refresh token does not expire, so this is a one-time step per machine.

Check state at any time:

```
dk auth status
```

## Why two logins?

DigiKey splits its APIs across two OAuth 2.0 grants:

| API | Grant | What dk needs |
|---|---|---|
| Product Information v4 | `client_credentials` (2-legged) | client id + secret |
| MyLists v1 | `authorization_code` (3-legged) | a one-time browser login |

Lists belong to a DigiKey *user account*, which is why they need a token that
represents you rather than just your application. dk caches both token types in
`token.json` (mode `0600`), keyed by environment, and refreshes them silently.

If a 3-legged token is cached, dk also uses it for product endpoints, which
returns your account-specific pricing (`MyPricing`) instead of list pricing.

## Commands

```
dk search <keywords...>       search the catalog
dk filters <keywords...>      discover the filters available for a search
dk product <part-number>      full detail for one part
dk related <part-number>      mating halves, kits, accessories
dk pricing <part-number>      cost a quantity across packaging options
dk docs <part-number>         list or download datasheets and documents
dk categories [id]            browse the category taxonomy
dk manufacturers              list manufacturer ids

dk list ls                    your lists
dk list create <name>         create a list
dk list show <list>           parts with live pricing and stock
dk list add <list> <part>...  add parts
dk list set <list> <part>     change quantity / refs / notes in place
dk list rm <list> <part>...   remove parts
dk list copy <list> <name>    clone a list (e.g. rev A -> rev B)
dk list rename <list> <name>  rename
dk list delete <list>         delete (needs --force if non-empty)
dk list export <list>         BOM-shaped output, pairs with --output csv

dk auth login|status|logout   authentication
dk env [production|sandbox]   show or switch the active environment
dk config show|set|path       configuration
dk cache status|clear         cached API responses
dk guide                      condensed reference for scripts and agents
dk version
```

`<list>` is always a list **name or id**. Names match exactly first, then
case-insensitively; an ambiguous name is an error rather than a guess.

### Adding parts

Quantities attach to a part with `:QTY`:

```
dk list add "Bench PSU rev A" 1276-1000-1-ND:10 311-10.0KHRCT-ND:20 296-1234-5-ND
```

Parts without a suffix use `--qty` (default 1). For a single part you can attach
metadata inline:

```
dk list add "Bench PSU rev A" 1276-1000-1-ND --qty 10 --ref C1-C10 --note "input decoupling"
```

For per-part metadata in bulk, use `--from-json`:

```
cat bom.json | dk list add "Bench PSU rev A" --from-json -
```

```json
[
  {"part": "1276-1000-1-ND", "quantity": 10, "reference": "C1-C10", "note": "decoupling"},
  {"part": "311-10.0KHRCT-ND", "quantity": 20, "reference": "R1-R20"}
]
```

### Editing a list

Use `dk list set` to change a quantity — not `rm` followed by `add`:

```
dk list set "Bench PSU rev A" 1276-1000-1-ND --qty 20
dk list set "Bench PSU rev A" 1276-1000-1-ND --ref C1-C20 --note "bulk decoupling"
```

`set` edits in place, so the unique ID, reference designators, and notes survive;
only the flags you pass change. An ambiguous target is an error (unlike `rm`,
which applies to every match) — pass the unique ID when a part appears twice.

`dk list copy "rev A" "rev B"` clones a list, which is the clean way to start a
revision without disturbing a BOM you have already reviewed.

**`--verify` is worth using.** DigiKey accepts unknown part numbers without
complaint and simply marks the line unmatched. `--verify` checks each part
against the catalog first and skips the ones that do not resolve. Either way,
watch the `MATCHED` column in `dk list show` (or `unmatched_parts` in JSON).

## Parametric filtering

DigiKey has **no endpoint that lists the filters for a category**. Filters come
back as facets on a search response — the parameters that would narrow *that*
result set. So narrowing is a two-step loop.

**1. Discover what you can filter on:**

```
$ dk filters "0603 ceramic capacitor"
PARAM ID  PARAMETER                TYPE           VALUES
2049      Capacitance              UnitOfMeasure  0.1 µF (1500), 1 µF (900), 10 µF (400), (+38 more)
1291      Tolerance                String         ±10% (2000), ±5% (1200)
2079      Temperature Coefficient  String         X7R (1800), C0G, NP0 (700)

Category: Ceramic Capacitors (id 60)
4210 products match "0603 ceramic capacitor".
```

The product counts show how much each choice would narrow things.

**2. Drill into one parameter** (the overview caps each value list):

```
dk filters "0603 ceramic capacitor" --parameter Capacitance
dk filters "0603 ceramic capacitor" --all-values --output json
```

**3. Apply the filters:**

```
dk search "0603 ceramic capacitor" \
  --param "Capacitance=0.1 µF" --param "Tolerance=±10%" --in-stock
```

Details worth knowing:

- **Names, not ids.** `--param` matches parameter and value names
  case-insensitively, and a unique substring is enough. A raw `value_id` from
  `dk filters` works too.
- **OR within a parameter, AND across them.**
  `--param "Resistance=10 kOhms,4.7 kOhms"` means either resistance;
  a second `--param` further narrows.
- **Values containing a comma** (e.g. `C0G, NP0`) would be split by the
  comma-join syntax. Repeat the flag instead — repeated `--param` for the same
  parameter merges its values.
- **Categories are implied.** Parameter ids are scoped to a category, so
  `--param` needs one. It is inferred from your keywords; pass `--category` if
  the inference is wrong or if dk reports that your parameters span more than
  one category.
- **Wrong guesses are recoverable.** An unknown parameter or value exits 2 and
  the error lists what *is* available, so a caller can correct itself without
  re-running `dk filters`.
- `--param` costs one extra API call, for discovery.
- Values with `range_type` of `Min`/`Max`/`Range` are synthetic bounds DigiKey
  attaches to numeric parameters, not discrete choices.

## Buying the right quantity

The same part number can be a trap: ask for 250 and a minimum order or standard
pack can land you a 5000-piece reel. Ask for 4500 and the reel is *cheaper*.

```
$ dk pricing 311-10.0KHRCT-ND --qty 4500
OPTION         ORDER QTY  TOTAL    DKPN               PACKAGING         QTY   UNIT    STOCK    STATUS
Exact          4500       29.7500  311-10.0KHRCT-ND   Cut Tape (CT)     4500  0.0066  4334182  Active
Exact          4500       36.9000  311-10.0KHRDKR-ND  Digi-Reel®        4500  0.0082  4334182  Active
BetterValue *  5000       23.4500  311-10.0KHRTR-ND   Tape & Reel (TR)  5000  0.0047  4333843  Active

Cheapest in stock: 311-10.0KHRTR-ND (Tape & Reel (TR)), order 5000 for 23.4500 USD total.
Note: this hands you 5000 units, not the 4500 requested.
```

`OPTION` is DigiKey's own label: `Exact`, `MinimumOrderQuantity`,
`BetterValue`, or `MaxOrderQuantity`. A `*` marks an option that hands you more
than you asked for. In JSON, read `.best` (cheapest option actually orderable,
or `null`) and `.forced_up`. The `best` key is always present, so `null` is the
"nothing in stock" signal.

One option can name several part numbers — a quantity past a standard reel is
filled with the reel plus a cut-tape remainder, priced together — so each
option carries a `products` array and the per-product figures live there.

## What else do I need to buy?

```
$ dk related WM4200-ND
RELATION   DKPN        MPN         MFR    DESCRIPTION           STOCK  UNIT
mating     WM4300-ND   22-01-3037  Molex  CONN HOUSING 3POS     15000  $0.28
accessory  WM9999-ND   63811-1000  Molex  HAND CRIMP TOOL       12     $249.00
```

Connector → mating half, terminal → crimper, board → compatible accessories.
`--kind mating` narrows it. This is a different question from
`dk product --substitutes`, which is what you'd buy *instead*.

### Packaging variations matter

The same physical part has a different DigiKey part number per packaging option
(cut tape vs. tape & reel vs. digi-reel), with different MOQs and pricing.
`dk search` reports the cheapest in-stock variation. To see them all:

```
dk product 1276-1000-1-ND --variations
```

## Datasheets and documents

The primary datasheet URL comes back with every search and product result as
`datasheet_url` — no extra call. For everything else DigiKey attaches to a part:

```
$ dk docs STM32G031K8T6
TYPE              TITLE                    URL
Datasheets        STM32G031x4/x6/x8        https://mm.digikey.com/.../stm32g031k8.pdf
Manuals           RM0444 Reference Manual  https://mm.digikey.com/.../rm0444.pdf
EDA / CAD Models  Ultra Librarian          https://app.ultralibrarian.com/...
Product Photos    LQFP-32                  https://mm.digikey.com/...
```

To get the PDFs on disk:

```
dk docs STM32G031K8T6 --type datasheet --download ./datasheets
```

Downloads are written atomically, refuse to clobber existing files without
`--overwrite`, and are capped at 128 MB each. A document that fails to download
gets an `error` field in the JSON rather than aborting the rest; the command
exits non-zero only if nothing at all was downloaded. No bearer token is sent to
the CDN hosting the files.

## What this is good at (and what it isn't)

DigiKey's parametric data describes **components** well and **boards** poorly.
That shapes how you should drive it:

- **Discrete components** — resistors, capacitors, connectors, terminals, ICs.
  Parametric filtering works well; attributes like stud size, tolerance,
  dielectric, pin count, and supply voltage are real, filterable parameters.
- **Dev boards and modules** — Feather, QT Py, Pico, sensor breakouts. GPIO
  count, connector type (USB-C vs micro-B), and ecosystem branding (STEMMA QT,
  Qwiic, Grove) are generally *not* parameters. They live in the product title
  and `detailed_description`. Lead with keywords, narrow with
  `--manufacturer Adafruit`, and verify with `--full`.

For board-level questions dk is a **verification** tool more than a discovery
tool: bring candidate part families, and use dk to confirm they exist, are in
stock, and cost what you expect. `dk guide` spells this out for agent callers.

## Output

| `--output` | Behavior |
|---|---|
| `auto` (default) | table on a terminal, JSON when piped |
| `json` | always valid JSON on stdout |
| `table` | aligned columns |
| `csv` | same columns, comma-separated |

Progress notes, prompts, and errors go to **stderr**; stdout carries only the
result. That means `dk list show X --output json > out.json` is always parseable.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic failure |
| 2 | bad flags or arguments |
| 3 | authentication required or rejected — run `dk auth login` |
| 4 | product or list not found |
| 5 | DigiKey rate limit hit |
| 6 | configuration missing or invalid |

In JSON mode a failure prints one object on stderr:

```json
{
  "error": {
    "code": "auth_required",
    "message": "digikey user login required",
    "hint": "Run `dk auth login` once in an interactive terminal. ...",
    "exit_code": 3
  }
}
```

Branch on `.error.code`, not on the message text. Codes: `usage_error`,
`credentials_missing`, `config_invalid`, `auth_required`, `not_found`,
`ambiguous_list`, `rate_limited`, `api_error`, `cancelled`, `error`.

`cancelled` is Ctrl-C or SIGTERM, which exits 1 — distinguishable from a
genuine failure by the code. A second Ctrl-C force-quits.

## Environments

dk talks to one DigiKey deployment at a time:

```
dk env                  which environment is active
dk env list             both, and which has credentials
dk env sandbox          switch (persists until you change it back)
dk env prod             switch back
```

This is persistent state, not a per-command flag. There is no `--env` and no
`DIGIKEY_ENV`: the environment `dk env` and `dk auth status` report is always
the one the next command will use, which would not be true if a variable in one
shell could quietly disagree.

**Each environment needs its own registered app.** DigiKey scopes a client id to
a single deployment, and its developer portal registers a sandbox app separately
from a production one — a production client id is not served sandbox data. So
register a second app for the sandbox, give it the same
callback URL, and store its credentials while the sandbox is active:

```
dk env sandbox
dk config set client_id <sandbox id>
dk config set client_secret <sandbox secret>
dk auth login                          # only if you need lists
```

Credentials live in a separate file per environment, so neither set overwrites
the other, and cached tokens are keyed by environment too — switching back does
not mean logging in again.

Sandbox data is not real. Check `dk env` before trusting a part number, a stock
figure, or a price.

## Configuration

Resolution order, highest first:

1. flags (`--client-id`, `--site`, `--currency`, ...)
2. environment variables
3. config file (`dk config path`)
4. built-in defaults

| Key | Env var | Scope | Default |
|---|---|---|---|
| `client_id` | `DIGIKEY_CLIENT_ID` | per environment | — |
| `client_secret` | `DIGIKEY_CLIENT_SECRET` | per environment | — |
| `redirect_uri` | `DIGIKEY_REDIRECT_URI` | per environment | `https://localhost:8139/digikey_callback` |
| `account_id` | `DIGIKEY_ACCOUNT_ID` | per environment | — |
| `locale.site` | `DIGIKEY_LOCALE_SITE` | shared | `US` |
| `locale.language` | `DIGIKEY_LOCALE_LANGUAGE` | shared | `en` |
| `locale.currency` | `DIGIKEY_LOCALE_CURRENCY` | shared | `USD` |
| `cache_ttl` | `DK_CACHE_TTL` | shared | `10m` |

The environment itself is not in the table because it is not a setting you
override per run: it is changed with `dk env` and has no variable and no flag.
See [Environments](#environments).

Per-environment keys are stored in `credentials-<environment>.json` and the
shared ones in `config.json`. `dk config set` writes to whichever environment is
active, so run `dk env` first if you are not sure which that is; `dk config path`
prints both files.

Config and the token cache live in `$XDG_CONFIG_HOME/dk`, defaulting to
`~/.config/dk` — including on macOS, alongside the other command-line tools,
rather than in `~/Library/Application Support`. Windows uses `%AppData%\dk`.

Cached responses live in `$XDG_CACHE_HOME/dk` (`~/.cache/dk`, `%LocalAppData%\dk`
on Windows) — separate from the config, because deleting them costs a few API
calls while deleting the config costs a login.

`DK_CONFIG_DIR` relocates the config and token directory, and the cache with it,
so one variable contains dk's whole on-disk footprint. `DK_CACHE_DIR` moves only
the cache; like every other location, it names a parent rather than the
directory itself, and dk keeps its files one level inside it, so `dk cache
clear` never deletes anything but its own — foreign files and directories
sharing the parent are left where they are. `DIGIKEY_API_BASE_URL` points dk at
a different host, which is how the test suite runs against a mock.

`dk config set` only ever writes what you pass it — a secret exported into the
environment is never persisted to disk as a side effect.

## Response cache

DigiKey's API is rate limited, and dk is a one-shot process, so running the same
search twice — once to read, once to pipe somewhere — used to cost two requests
out of that quota. Successful catalog and list reads are now cached on disk for
ten minutes and an identical read is served from there.

```
dk search "0.1uF 0603" --output json                 # one API call
dk search "0.1uF 0603" --output json | jq '.products[].digikey_part_number'
                                                     # same question, served from disk
dk search "0.1uF 0603" --limit 50 --output json      # a different question: one more call
```

Only reads are cached, and only successful ones — a rate-limit reply or an
expired token is never stored. Writing to a list drops the cached list reads
immediately, so `dk list show` always reflects the `dk list add` that just ran;
catalog entries survive that, which is what keeps the cache useful across a
search-add-search loop. That invalidation happens even when the cache is
switched off for the invocation doing the writing, since what an earlier run
stored would otherwise outlive the change.

The read a write depends on is never served from the cache: `dk list rm` and
`dk list set` resolve a part number to a line id against live contents, and the
part count that decides whether `dk list delete` demands `--force` is asked of
the API every time. Entries are written `0600` because with a user token in
play they carry account-specific pricing.

| Control | Effect |
|---|---|
| `--no-cache` | ask DigiKey even if an entry is fresh, and replace it |
| `--cache-ttl 30s` | shorten the window for one invocation |
| `--cache-ttl 0` | switch the cache off for one invocation |
| `dk config set cache_ttl 0` | switch it off permanently |
| `dk cache status` | where entries live, how many, how much disk |
| `dk cache clear` | delete them all |

Stock and price move. An entry can be up to the TTL old, so pass `--no-cache`
before quoting a figure someone is about to act on.

Entries are keyed by the grant they were read under — a logged-in token returns
account-specific pricing — so `dk auth login` and `dk auth logout` empty the
cache rather than risk serving one account's prices to another.

## For agents

Run `dk guide` for a condensed operating manual: output contract, exit codes,
error shapes, which commands need which auth, and a recommended workflow. It is
plain text in every output mode.

The one thing an agent cannot do is `dk auth login` — it needs a browser. If dk
exits 3 with `auth_required`, stop and ask the human to run it once.

## Development

```
make test          # go test ./...
make test-race     # with -race and coverage
make lint          # gofmt check + go vet
make lint-full     # the golangci-lint set CI runs
make build
```

There are no runtime dependencies beyond `spf13/cobra`. Tests run entirely
against `httptest` servers — nothing touches the real DigiKey API.

CI (`.github/workflows/ci.yml`) runs formatting, `go mod tidy` verification,
`go vet`, `go test -race`, and golangci-lint on every push and pull request.

Changing dk itself? [CONTRIBUTING.md](CONTRIBUTING.md) has the design notes: the
invariants that keep the output safe to parse, how the response cache is keyed,
which endpoints are deliberately not wrapped, and the API behaviors already
settled against the live server.

## License

MIT — see [LICENSE](LICENSE). Use it however you like; it comes with no
warranty.
