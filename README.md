# dk — DigiKey CLI

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

`dk config path` prints where the config and token files live.

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
dk product <part-number>      full detail for one part
dk categories [id]            browse the category taxonomy
dk manufacturers              list manufacturer ids

dk list ls                    your lists
dk list create <name>         create a list
dk list show <list>           parts with live pricing and stock
dk list add <list> <part>...  add parts
dk list rm <list> <part>...   remove parts
dk list rename <list> <name>  rename
dk list delete <list>         delete (needs --force if non-empty)
dk list export <list>         BOM-shaped output, pairs with --output csv

dk auth login|status|logout   authentication
dk config show|set|path       configuration
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

**`--verify` is worth using.** DigiKey accepts unknown part numbers without
complaint and simply marks the line unmatched. `--verify` checks each part
against the catalog first and skips the ones that do not resolve. Either way,
watch the `MATCHED` column in `dk list show` (or `unmatched_parts` in JSON).

### Packaging variations matter

The same physical part has a different DigiKey part number per packaging option
(cut tape vs. tape & reel vs. digi-reel), with different MOQs and pricing.
`dk search` reports the cheapest in-stock variation. To see them all:

```
dk product 1276-1000-1-ND --variations
```

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
`credentials_missing`, `auth_required`, `not_found`, `ambiguous_list`,
`rate_limited`, `api_error`, `error`.

## Configuration

Resolution order, highest first:

1. flags (`--client-id`, `--env`, `--site`, `--currency`, ...)
2. environment variables
3. config file (`dk config path`)
4. built-in defaults

| Key | Env var | Default |
|---|---|---|
| `client_id` | `DIGIKEY_CLIENT_ID` | — |
| `client_secret` | `DIGIKEY_CLIENT_SECRET` | — |
| `environment` | `DIGIKEY_ENV` | `production` |
| `redirect_uri` | `DIGIKEY_REDIRECT_URI` | `https://localhost:8139/digikey_callback` |
| `account_id` | `DIGIKEY_ACCOUNT_ID` | — |
| `locale.site` | `DIGIKEY_LOCALE_SITE` | `US` |
| `locale.language` | `DIGIKEY_LOCALE_LANGUAGE` | `en` |
| `locale.currency` | `DIGIKEY_LOCALE_CURRENCY` | `USD` |

`DK_CONFIG_DIR` relocates the config and token directory. `DIGIKEY_API_BASE_URL`
points dk at a different host, which is how the test suite runs against a mock.

`dk config set` only ever writes what you pass it — a secret exported into the
environment is never persisted to disk as a side effect.

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
make build
```

There are no runtime dependencies beyond `spf13/cobra`. Tests run entirely
against `httptest` servers — nothing touches the real DigiKey API.

## License

MIT
