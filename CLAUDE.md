# dk — notes for Claude Code

## Using the CLI (most common case)

If you are here to *search DigiKey or build a parts list*, not to modify this
repo, run **`dk guide`** first. It prints the full contract: output format, exit
codes, error shapes, which commands need which authentication, and a recommended
workflow. Everything below is about developing dk itself.

Two rules that matter regardless:

- **dk never places an order.** It stages lists for a human to review and buy.
  Do not claim a part has been ordered.
- **You cannot run `dk auth login`** — it needs a browser. If dk exits 3 with
  `auth_required`, stop and ask the human to run it once.

## Layout

```
cmd/dk/            main; just calls cli.Main()
internal/cli/      cobra command tree, output views, error classification
internal/digikey/  typed client for Product Information v4 and MyLists v1
internal/auth/     both OAuth flows, token cache, HTTPS callback listener
internal/config/   config file + env resolution
internal/output/   json / table / csv rendering
internal/atomicfile/  crash-safe file replacement, shared by config and auth
```

## Design invariants

Break these and the CLI stops being safe to drive from a program:

- **stdout carries only the result.** Prose, prompts, and progress go to stderr.
  Use `Printer.PrintText` for confirmations: it writes to the printer's `Err`
  stream and is a no-op in JSON and CSV mode. Both halves matter — suppressing
  it in machine formats alone would still leak prose into
  `dk list show --output table > parts.txt`, since table output is capturable
  too. (With no explicit `--output`, a redirected stdout is a non-TTY and
  resolves to JSON, where PrintText is already suppressed.)
- **Every failure is classified.** Return an `*Error` (or something `classify`
  recognizes) so the exit code and the JSON `error.code` stay in sync. A new
  failure mode needs a code in `errors.go` *and* a line in `guide.go`; a test
  enforces the latter.
- **Commands take `*App`, not globals.** That is what lets the whole CLI run
  in-process against an `httptest` server. Do not reach for package state.
- **`ProductView` is a deliberate flattening.** DigiKey's `Product` hides the
  orderable part number, MOQ, and pricing one level down in `ProductVariations`.
  The view exists so callers do not have to know that. `--raw` is the escape
  hatch for the untouched payload, and it means it: it goes through
  `RawKeywordSearch`/`RawProductResponse`, which decode into a
  `json.RawMessage`. Never implement `--raw` by re-encoding a decoded struct —
  that drops every field the structs do not model and invents zero values for
  fields DigiKey never sent, which is exactly what the flag exists to avoid.
- **List commands accept a name or an id.** GUIDs are hostile to both humans and
  agents. `ResolveList` handles it; an ambiguous name is an error, never a guess.

- **Anything that needs a complete list pages for it.** `Lists`/`ListParts` are
  single-page calls; `AllLists`/`AllListParts` page until a short page. A name
  or part number on page two is otherwise indistinguishable from one that does
  not exist, which turns into a false `not_found` for `list rm`/`list set` and,
  worse, a silently truncated BOM out of `list export`. Only `list show` with an
  explicit `--limit`/`--offset` reads a single page, and it reports `returned`
  alongside `total_parts` so the difference is visible.

- **Empty arrays serialize as `[]`, never `null`.** A documented array whose
  empty case is normal (`products`, `options`, `documents`, `parts`) is
  initialized to an empty slice at construction. Likewise `pricing`'s `best` is
  not `omitempty`: the guide documents it as `{...} | null`, so the key has to
  be present for a caller testing against null.
- **Filters are named, not numbered.** `--param "Capacitance=0.1 µF"` resolves
  through the facets rather than making callers pass opaque ids. When a name
  fails to resolve, the error lists what *is* available — that is what lets an
  agent self-correct in one round trip instead of two.

## Parametric filtering

There is no endpoint that lists a category's filters. `KeywordResponse.FilterOptions`
carries them as facets for the current result set, which is why `dk filters` runs
a real search (with `Limit: 1` — facets come back regardless of page size) and
why `dk search --param` costs a second call to resolve names to ids.

DigiKey only honors `ParameterFilterRequest` alongside a `CategoryFilter`, and
parameter ids are category-scoped. `resolveParamSpecs` derives that category from
the parameters themselves and rejects specs that span two categories, since the
API cannot express it.

## Documents

`Product.DatasheetUrl` carries the primary datasheet and rides along with every
search result, so `dk docs` (the `/media` endpoint) is only for the rest:
additional datasheets, manuals, reference designs, CAD models, PCNs.

`--download` treats every filename as hostile — it comes from a remote URL.
`sanitizeFilename` reduces it to a single path element with no separators and no
leading dots; `TestDocumentFilenameIsContained` is the guard. Downloads are
atomic (temp file + rename), size-capped, and carry no `Authorization` header,
since the files live on a CDN rather than the API.

## Endpoint coverage

Product Information v4 is fully covered except `/pricing`, `/pricingbyquantity`,
and `/digireelpricing`. Those are deliberate omissions: `ProductDetails` already
returns `MyPricing` per variation, `packagetypebyquantity` returns strictly more
than `pricingbyquantity`, and DigiReel is a custom-reel service irrelevant to
this tool's purpose. Note that `dk pricing` wraps `packagetypebyquantity`, *not*
the `/pricing` endpoint — the command is named for what it answers.

MyLists v1 is covered at both layers except `GetPartByUniqueId` and
`validate/{name}`: the first is subsumed by `GetPartsByListId` (which `dk list
show` already calls), and the second by `validate/name/{name}`, which `--auto-rename`
uses and which answers the same question plus a suggestion. Neither had a caller,
and an unreachable client method is not "coverage".

`ProductSummary` (associations, alternate packaging) returns `UnitPrice` as a
preformatted **string**; `Product` and `RecommendedProduct` return it as a
**number**. Do not unify these — it is DigiKey's inconsistency, and flattening it
would mean parsing currency-formatted text. The views preserve the split:
`SummaryView.UnitPrice` is a string, `RecommendedView.UnitPrice` a float64, and
`dk guide` tells callers to check the type before doing arithmetic.

Every command returns a dk view, never a decoded DigiKey struct. `SummaryView`
is the shared flattening of `ProductSummary` and backs `dk related`,
`dk product --alternate-packaging`, and `--substitutes` (via `SubstituteView`),
so the three cannot drift apart. Printing a decoded struct directly is what put
DigiKey's PascalCase — and phantom fields like `"ProductUrl": ""` that DigiKey
never sent — into the output of three commands. `--raw` is the way to get the
real payload.

## Auth model

Product Information accepts a `client_credentials` token. MyLists requires an
`authorization_code` token, because lists belong to a user account. Endpoints
declare this with `request.requireUser`; `auth.Manager` decides which token to
mint. When a 3-legged token is cached it is preferred everywhere, because it
returns account-specific pricing.

DigiKey rejects non-HTTPS redirect URIs, which is why `auth/callback.go` mints a
short-lived self-signed certificate for the loopback listener rather than
serving plain HTTP.

## Testing

```
make test        # go test ./...
make test-race   # -race -cover
make lint        # gofmt check + go vet
make lint-full   # the golangci-lint set CI runs
```

CI (`.github/workflows/ci.yml`) runs gofmt, `go mod tidy` verification, vet,
`go test -race -cover`, and golangci-lint on every push and pull request.

Tests never touch the real API. `DIGIKEY_API_BASE_URL` points the client at an
`httptest` server and `DK_CONFIG_DIR` at a temp dir; `run`/`runAuthed` in
`internal/cli` wire both up. `runAuthed` seeds a user token so list commands work
without a browser.

Prefer table-driven tests, and say what the assertion protects in the failure
message — several tests here exist to pin behavior that is easy to regress
silently (JSON never gaining prose, exit codes staying distinct, part-number
escaping surviving on the wire).

## API schema reference

Field names come from DigiKey's OpenAPI specs and are PascalCase; a lowercase key
is silently ignored by the API. Two known inconsistencies are handled explicitly:

- `ProductVariation` spells stock `QuantityAvailableforPackageType` (lowercase
  `f`) in v4, with `QuantityAvailable` as an older alias — `Stock()` covers both.
- Category children are `Children` on the taxonomy endpoints but
  `ChildCategories` inside a `Product` — `CategoryNode.Children()` covers both.

MyLists uses `X-DIGIKEY-Account-Id` where Product Information uses
`X-DIGIKEY-Customer-Id`; the client sends both from one config value.

`internal/digikey/testdata/listparts_obsolete.json` is a real `/parts` response,
lightly trimmed. Three things in it contradicted what the hand-written fixtures
assumed, so prefer it when changing list pricing:

- **`SelectedPackType` came back empty**; the selection lives in
  `SelectedPackOptionIndex`. `SelectedPackOption` therefore tries the index
  first and treats the name as a fallback. Matching on the name alone would
  never fire against real data.
- **`PackOptions` was an empty array** for an Obsolete, zero-stock part. There
  is no price anywhere on such a line, which is why it must read as *unpriced*
  (`unpriced_parts`) rather than free — a zero total that looks like a bargain
  is the failure mode to avoid.
- **`DigiKeyPartNumber` differed from `RequestedPartNumber`**: a `490-1532-1-ND`
  request resolved to the `-2-ND` reel. Never assume the two match.
