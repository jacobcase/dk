# Contributing to dk

dk is a Go CLI for searching the DigiKey catalog and staging parts into MyLists
order lists. This document is for people changing dk itself — for using it, see
[README.md](README.md), and run `dk guide` for the machine-facing contract.

Most of what follows is not process. It is the reasoning behind decisions that
look arbitrary from the outside: invariants that keep the CLI safe to drive from
a program, and answers already settled against the live API. A fair share of
these paragraphs exist because a reasonable-looking change reopened a question
that had already been decided, so they are worth reading before changing a wire
type, the response cache, or anything that pairs an identifier with a price —
and worth reading during a code review, where they often decide whether a
finding is real.

## Getting set up

Go 1.26 or newer. There is one runtime dependency, `spf13/cobra`.

```
git clone https://github.com/jacobcase/dk
cd dk
make build       # ./bin/dk
make install     # to $(go env GOPATH)/bin
```

The test suite needs no credentials and never reaches DigiKey. You only need an
API app and `dk auth login` to try a change against the real thing — worth doing
for anything touching a response shape, since several entries under [Settled
against the live API](#settled-against-the-live-api--do-not-reopen) exist because
the live answer contradicted the spec. README.md covers registering the app.

## Development loop

```
make test        # go test ./...
make test-race   # -race -cover
make lint        # gofmt check + go vet
make lint-full   # the golangci-lint set CI runs
make build
```

Branch off `master`, and run `make test-race` and `make lint-full` before
pushing — CI (`.github/workflows/ci.yml`) runs gofmt, `go mod tidy` verification,
`go vet`, `go test -race -cover`, and golangci-lint on every push and pull
request. Commit subjects follow Conventional Commits (`feat:`, `fix:`, `docs:`).

## Testing

The `make` targets and what CI runs are under [Development loop](#development-loop) above.

Tests never touch the real API. `DIGIKEY_API_BASE_URL` points the client at an
`httptest` server and `DK_CONFIG_DIR` at a temp dir; `run`/`runAuthed` in
`internal/cli` wire both up. `runAuthed` seeds a user token so list commands work
without a browser.

Prefer table-driven tests, and say what the assertion protects in the failure
message — several tests here exist to pin behavior that is easy to regress
silently (JSON never gaining prose, exit codes staying distinct, part-number
escaping surviving on the wire).

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
  single-page calls; `AllLists`/`AllListParts` page until a request returns
  nothing new — never merely until a short page. The spec gives `/lists` a limit
  default of 50 and no maximum, so a request for 100 coming back short is the
  expected first response, not an ending. A name or part number on page two is
  otherwise indistinguishable from one that does not exist, which turns into a
  false `not_found` for `list rm`/`list set` and, worse, a silently truncated
  BOM out of `list export`. Only `list show` with an explicit
  `--limit`/`--offset` reads a single page, and it reports `returned` alongside
  `total_parts` so the difference is visible.

  **A server that ignores `startIndex` is an error, not a short answer.** It can
  never hand over the rest, so returning what was read is the truncation above
  wearing a success. Neither walk can detect that by asking whether the page was
  *full* — a server capped below the requested limit resends a short page
  forever — and neither can call any non-empty repeat a failure: one that
  *clamps* an out-of-range `startIndex` replies with a tail already held, and
  that really is the end. Both compare where the batch starts: a repeat of page
  one is the error. `AllLists` additionally requires the repeat to be at least
  `listPageDefault` rows, because an account holding three lists answers every
  `startIndex` with the same three; `AllListParts` needs no such threshold,
  since `TotalParts` has already ended the walk for any list that fits in what
  it holds. `TotalParts` cannot be the whole test either — it lags a delete, so
  an *empty* page still ends the walk whatever the count claims.

  This deliberately overturns the older reading that a short read was acceptable
  because `TotalParts` left the shortfall visible next to `returned`. It is not
  visible where it costs most: `list export` writes CSV, where `PrintText` is
  suppressed, so the BOM is short and looks whole.

- **Empty arrays serialize as `[]`, never `null`.** A documented array whose
  empty case is normal (`products`, `options`, `documents`, `parts`) is
  initialized to an empty slice at construction. Likewise `pricing`'s `best` is
  not `omitempty`: the guide documents it as `{...} | null`, so the key has to
  be present for a caller testing against null.
- **Filters are named, not numbered.** `--param "Capacitance=0.1 µF"` resolves
  through the facets rather than making callers pass opaque ids. When a name
  fails to resolve, the error lists what *is* available — that is what lets an
  agent self-correct in one round trip instead of two.

## Response cache

`Client.do` serves and stores responses for requests marked with a `cacheScope`.
The rules that keep it from being worse than no cache at all:

- **Caching is opt-in per request, never per method.** A new endpoint gets no
  cache until someone sets `cacheScope` on it. `SuggestListName` is the reason:
  it is a GET, and caching it would hand `--auto-rename` a name that has since
  been taken — the one thing that call exists to prevent.
- **Only successful responses are stored, and only once they decode.** A cached
  429 or 401 outlives the condition that produced it and turns a transient
  failure into a sticky one. A 2xx whose body is not the expected JSON — a proxy
  interstitial, a WAF challenge — is the same failure wearing a successful
  status, so `do` decodes before it calls `Put`; stored, it would replay its
  decode error from disk until the TTL ran out, and no retry could clear it.
- **The key covers everything that changes the answer**, not just the URL: the
  grant, the client and account ids, the locale, and the request body. The grant
  is in there because a 3-legged token returns account-specific pricing, so the
  same query under the two grants is two different documents. Serving one for
  the other would misprice a BOM, which is the same class of bug as pairing a
  part number with another variation's price. The environment needs no element:
  `endpoint` carries the base URL.
- **The key names the grant, never the token.** `TokenSource.Token` reports
  which grant answered, because the caller cannot infer it — a cached 3-legged
  token is preferred everywhere, so it may answer a request that did not require
  one. Keying on the token bytes was correct and unaffordable: DigiKey's
  `client_credentials` token lives 600s against a 10m TTL, so the namespace
  rotated about as fast as entries expired. The refresh token is no better —
  DigiKey rotates it on every grant. What the fingerprint separated incidentally
  was one login's entries from the next, so `dk auth login` and `dk auth logout`
  call `dropCachedResponses`; that is exact, and it keeps a credential out of a
  string that decides a file name.
- **Scopes exist so a list write does not throw away catalog reads.** MyLists
  mutations set `invalidates: ScopeLists`; searches live in `ScopeProduct` and
  survive. Without that split the cache would be useless in the workflow it was
  built for — search, add, search again.
- **Invalidation is not conditional on this run reading the cache.** `--cache-ttl
  0` and `DK_CACHE_TTL=0` turn off `Get` and `Put`, not `Invalidate`: what an
  earlier run stored for a list this one just changed has to go regardless.
  `cache.New` therefore returns a usable `*Cache` for a zero TTL — only an empty
  directory yields nil — and `App.Cache()` no longer short-circuits. Gating
  invalidation on the read setting is how `dk list add --cache-ttl 0` left the
  next `dk list show` serving the contents from before the add.
- **Invalidation happens on the write attempt, not on its reply.** The
  `Invalidate` is a `defer` registered before `httpc.Do`, so a timeout, a
  connection reset, or a 5xx after the mutation landed still drops the scope. A
  lost reply is no evidence that the write did not arrive, and this is the one
  cache failure that answers with something a write already changed. Dropping a
  scope for a write that never reached DigiKey costs a refetch.
- **Every read a write is aimed at goes through `digikey.Live(ctx)`.** `dk list
  rm`/`list set` turn a part number into the unique id they are about to write
  to, `dk list set` re-sends the `RequestedPart` it read (DigiKey's update is a
  replace, not a patch), and `dk list delete`'s `--force` guard decides a
  permanent delete from a part count. A stale answer there is not an
  out-of-date figure on a screen; it is a write pointed at the wrong line, a
  field silently reverted, or a filled list discarded. `Live` bypasses the read
  and still refreshes the entry, exactly like `--no-cache`.

  **The name lookup counts as such a read.** `ResolveList` reads the cached
  listing, and a name that has been renamed or recreated since resolves to a
  different list — which is where the write then lands. `resolveListForWrite` is
  the one entry point for the commands that mutate; `list show` and `list
  export` call `ResolveList` directly and stay cached, which is the point.
- **The cache stores response bytes, not decoded structs**, so `--raw` and the
  typed path share one entry. Storing decoded structs would double the traffic
  *and* re-introduce the field loss `--raw` exists to avoid.
- **Freshness is the entry's mtime against the current TTL**, and `Put` stamps
  the file from the cache's own clock. One source of time, and lowering
  `--cache-ttl` takes effect at once rather than after the old entries age out.

`config.CacheDir()` honors `DK_CONFIG_DIR` even though the cache is not the
config directory: that variable is dk's isolation lever, and a cache that
escaped it would let one test read another's responses and pollute the real
user cache. `DK_CACHE_DIR` is checked *first*, so `internal/cli`'s `TestMain`
clears it (along with `DK_CACHE_TTL`) before anything runs — a developer with it
exported would otherwise have the suite write to, and `cache clear`, their real
cache. Tests share a config dir across invocations on purpose — the cache only
pays off between processes, so a per-run temp dir would test nothing.

No branch of `CacheDir()` returns the variable it was handed: each ends in a
directory dk owns and created — `dk` under `DK_CACHE_DIR`, `cache` under
`DK_CONFIG_DIR`, which is already a directory dk was given. The returned path is
what `dk cache clear` deletes, and `DK_CACHE_DIR=$HOME dk cache clear` erasing a
home directory is the failure that rule exists to prevent.

Nothing in the package removes a file it did not write or a directory it did not
fill. `Clear`, `Invalidate`, and `prune` delete only names matching
`sha256hex + .json` (`isEntryName`) or the `.tmp` file an interrupted
`atomicfile.Write` left beside one (`isTempName`), and they `rmdir` the scope
*only after removing something* — an empty directory whose name merely passes
`validScope` may be the user's, since the root is whatever `DK_CACHE_DIR` named.
`Stat` reads the same single level, so nothing it counts is beyond `Clear`'s
reach; it counts entries only, since a half-written file is not a response
anyone can be served.

The `cache_ttl` value is parsed in `App.CacheTTL()`, not in `setup()`. Validating
it for every command would make an unparseable value in config.json fail `dk
config set cache_ttl` — the command that repairs it — and `dk cache clear` along
with it, leaving hand-editing the JSON as the only way out. `dk cache clear`
ignores the parse error outright: it needs the TTL only for a count it does not
print, and it is one of the two exits from a broken configuration.

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

Every URL dk emits — `datasheet_url`, `product_url`, and each document `url` —
goes through `normalizeAssetURL`. DigiKey returns protocol-relative URLs
(`//mm.digikey.com/...`) for a large share of datasheets: 8 of 20 in a sampled
search. No HTTP client fetches those, so they are repaired to `https:` and
anything still unfetchable is dropped rather than handed on as a string that
looks like a URL.

`--download` treats every filename as hostile — it comes from a remote URL.
`sanitizeFilename` reduces it to a single path element with no separators and no
leading dots; `TestDocumentFilenameIsContained` is the guard. Downloads are
atomic (temp file + rename), size-capped, and carry no `Authorization` header,
since the files live on a CDN rather than the API.

## Pairing a part number with its price

Two commands used to report a part number and a price that described *different*
things. Both were caught only by running against the live API, and both are the
same mistake:

- **A list line's part-level `DigiKeyPartNumber` is not the one that was
  priced.** A cut-tape request comes back with the *reel* at the top level while
  `SelectedPackOptionIndex` points at the cut-tape pack option. `ListPart.
  OrderablePartNumber` returns the selected option's number, so
  `dk list show` names the thing it quoted. `matchListEntries` therefore also
  matches pack-option part numbers — anything `list show` prints has to be
  removable by `list rm`.
- **`Product.UnitPrice` is not the chosen variation's price.** It is the catalog
  headline figure (cut tape at qty 1); `PrimaryVariation` often selects the reel.
  Emitting both put a 4000-piece MOQ next to a single-unit price and overstated
  the line sevenfold. `newProductView` prefers the variation's own price.

The rule: whenever a view reports an identifier and a figure side by side, both
must come from the same variation or pack option.

## Settled against the live API — do not reopen

Two fields look like they should be modeled and are not. Both were tested
against the real API on 2026-08-27; the answers are counterintuitive enough
that they get written down rather than rediscovered.

- **`ListPartQuantity.IsInactive` must not gate pricing.** The obvious reading
  — an inactive line should not count toward `estimated_total` — is backwards.
  A discontinued part with 172 in stock (`WK-KIT-ND`) comes back `IsInactive:
  true` while carrying a real pack option: MOQ 1, $8.88, `ExtendedPrice` 44.40,
  and `dk product` reports it orderable. A zero-stock discontinued part
  (`18-880129-ND`) comes back `IsInactive: false` with no pack options at all.
  Excluding inactive lines would report $0.00 for a BOM someone can actually
  buy, which is the zero-total-that-looks-like-a-bargain failure in the
  direction that costs money. Price from `PackOptions`; a line without them is
  `unpriced`, which is what the flag would otherwise be standing in for.

- **`KeywordResponse.AppliedParametricFiltersDto` is always empty.** It reads
  like the echo that would tell a caller whether DigiKey honored a
  `ParameterFilterRequest` — the one failure mode `--param` has. It is not.
  A `Ceramic Capacitors` search returning 930,932 products still returned `[]`
  after `Tolerance=±10%` cut it to 192,092. The whole payload carries no other
  applied-filter signal; `SearchLocaleUsed` is the only "what did you actually
  use" field on the response. There is nothing to model.

Four more, tested 2026-08-28. The last three are negative results — probes that
came back clean — and they are here so the next review does not propose guards
for shapes DigiKey does not produce.

- **`MaxOrderQuantity` can price *below* the request.** `WK-KIT-ND` at quantity
  1000 answers with one option: 172 pieces for $1327.84, labelled
  `MaxOrderQuantity`. `TotalQuantityPriced` went *down*, so `forced_up` is false
  and `TotalPrice` is the honest price of the 172 — nothing on the option says
  the request cannot be filled. `PricingOption.Short` is the mirror of
  `ForcedUp` for exactly this, and `cheapestInStock` skips short options: they
  are cheaper than any option that fills the order by definition, so price alone
  always picks them. A capped part is also not "out of stock" — it is on the
  shelf in smaller numbers, and the command reports the number.

- **DigiKey ignores a currency it will not honor and does not say so in the
  price.** `--currency EUR` against a `US` site returns USD figures;
  `SettingsUsed.SearchLocaleUsed.Currency` is the only field that admits it.
  `dk pricing` labels the totals from there and falls back to the configured
  currency only when the echo is absent. The config value is what dk *asked*
  for, which is not a currency label.

- **`/mylists/v1/lists` pages correctly.** `startIndex` and `limit` are both
  honored, and an out-of-range `startIndex` returns `[]` — it is not clamped to
  the first page and not ignored. `AllLists`'s "digikey ignored startIndex"
  guard is therefore unreachable against the live server, and its walk ends on
  the empty page like any ordinary listing. A review reading that guard will
  read it as a live error path for accounts with 50+ lists; it is not one.
  Rewriting the walk to prove the server pages (overlap paging, in-range probes)
  buys nothing this probe has not already settled.

- **`QuantityPriced` and `Products` are always populated** on
  `pricingbyquantity`. Across ten live responses — quantities 1 through
  99,999,999, cut tape, reel, Digi-Reel, split reel-plus-remainder, MOQ,
  discontinued — every product carried `QuantityPriced` and no option came back
  with an empty `Products`. Do not add a "missing quantity" or "option with no
  products" branch on the strength of the spec; neither shape exists.

Five more, tested 2026-08-28 against the live API.

- **`GET /mylists/v1/lists/{listId}` returns an empty `PartsList`, always.**
  The spec documents it as the list's editable `RequestedPart`s, and it is the
  natural place to read a line before writing it back. Live it answers with a
  correct `TotalParts` beside `PartsList: []` — 1-part and 17-part lists both.
  `dk list set` read it, found nothing, and reported every part in every list as
  an unknown unique id: the command could not edit anything and never could.
  Contents come from `GetPartsByListId` (`AllListParts`), which returns
  `ListPart`, so an edit converts with `ListPart.RequestedPart()`. The unit
  tests did not catch this because their fixture filled the array in.

- **`PackageType.Name` is localized; `PackageType.Id` is not.** A JP-site
  response calls id 2 "カット テープ（CT）" and id 1 "テープ＆リール（TR）", while
  the ids are 1/2/243 on every locale. `--packaging` matched the English name
  and so matched nothing off the US site — `--packaging CT` printed "DigiKey
  returned no pricing options" for a part DigiKey had just priced three ways.
  Match ids. Note it takes site *and* language to trigger: `language=ja` against
  the US site still answers in English. Ids seen live: 1 Tape & Reel, 2 Cut
  Tape, 62 Bag, 243 Digi-Reel.

- **`recommendedproducts` ignores `limit`.** The spec documents a default of 1.
  Live, `limit=1`, `limit=3`, `limit=25`, `limit=50` and sending no limit at all
  every one returned the same 10 recommendations. `--recommended-limit` is a
  request, not a cap; do not read the size of the result as evidence it applied,
  and do not "fix" it by validating the value.

- **The `pricingbyquantity` → `ProductDetails` stock join does not miss.**
  Across 15 parts — multi-variation passives, kits, dev boards, a discontinued
  part, an out-of-stock part, a split reel-plus-remainder option — every DigiKey
  part number in a pricing option was also a `ProductVariation` of the product
  looked up. Zero unmatched. The zero-stock lines that turned up were genuinely
  zero. Do not add a "join missed vs really out of stock" distinction on the
  strength of the spec; the shape it would guard against did not occur.

- **`AllLists` cannot end on its first page, and the second request is not
  waste.** An out-of-range `startIndex` returns `[]` (see above), so the walk
  has to see that empty page to know it is done. Ending early on a page shorter
  than `listPageDefault` looks like a free request saved and is not: a server
  capping pages below 50 returns a short first page for an account with more
  lists behind it, and the walk would return a truncated listing that looks
  complete. `/lists` answers with a bare array and no total, so there is no
  cheaper signal to end on. Two GETs per walk is the price of the guarantee.

## Endpoint coverage

Product Information v4 is fully covered except `/pricing`,
`/packagetypebyquantity`, and `/digireelpricing`. Those are deliberate
omissions: `ProductDetails` already returns `MyPricing` per variation, DigiReel
is a custom-reel service irrelevant to this tool's purpose, and
`packagetypebyquantity` is the endpoint DigiKey's own spec deprecates in favor
of `pricingbyquantity`, which `dk pricing` now wraps. Note that `dk pricing`
is not the `/pricing` endpoint — the command is named for what it answers.

An earlier note here claimed `packagetypebyquantity` "returns strictly more
than `pricingbyquantity`". That was wrong, and it is written down because it is
the kind of claim that stops anyone re-checking. The two are not in a superset
relation. The old one returns more *catalog metadata* (status, lead weeks,
stock note, RoHS/REACH, datasheet) and a raw price-break table per package
type. The new one returns more *pricing structure*, and the structure is the
half `dk pricing` exists for:

- **A pricing option can name several products.** DigiKey fills a quantity past
  a standard reel with the reel plus a cut-tape remainder and prices them as
  one option. The old endpoint's one-row-per-package-type shape could not say
  that, which is why `PricingOption` in the view holds a `products` array and
  carries no part number or unit price of its own — the same rule as pairing a
  part number with its price, one level up.
- **`PricingOption` is DigiKey's own classification**: `Exact`,
  `MinimumOrderQuantity`, `BetterValue`, `MaxOrderQuantity`. `BetterValue` —
  cheaper to buy *more* — cannot be derived from a single option, and dk could
  not express it at all before. Live: 4500 on cut tape is $29.75, while 5000 on
  a reel is $23.45.
- **The arithmetic is DigiKey's, not dk's.** The old path picked
  `RecommendedQuantity`, walked the break table for a unit price, and
  multiplied. `TotalPrice` and per-product `ExtendedPrice` now come priced.

**`pricingbyquantity` returns no stock, so `dk pricing` makes two calls.** The
spec documents `QuantityAvailable` on `PricingOptionsForQuantity`; the live API
never sends it, at any quantity, for any part tested. Stock and status are
joined from `ProductDetails`, which is exactly one extra call however many
options come back: every product an option can name is a variation of the same
product. The lookup uses a DigiKey part number *from the pricing response*, not
the caller's input — a manufacturer number can be ambiguous, a DigiKey one
cannot. Without that join `dk pricing` cannot answer its own headline question,
so a failed lookup fails the command rather than reporting everything as out of
stock.

Two live quirks worth knowing before touching this: `MyPricingOptions` comes
back `[]` even under a 3-legged token on an account with no negotiated pricing,
so its absence is normal and `Options()` falls back to
`StandardPricingOptions`. And a discontinued zero-stock part (`18-880129-ND`)
answers `500 NullReferenceException` where the old endpoint answered `400` with
a readable message — the failure is not new, but it is worse-shaped.

MyLists v1 is covered at both layers except `GetPartByUniqueId` and
`validate/{name}`: the first is subsumed by `GetPartsByListId` (which `dk list
show` already calls), and the second by `validate/name/{name}`, which `--auto-rename`
uses and which answers the same question plus a suggestion. Neither had a caller,
and an unreachable client method is not "coverage".

**`validate/name/{name}` is in the spec but not on the server.** Against the
live API it answers `404 Invalid resource path`, which used to fail
`--auto-rename` outright. `SuggestListName` therefore treats a 404 as "this
route does not exist" and derives a free name from `AllLists` instead. Only a
404: a 401 or a 429 says nothing about whether the name is taken, and a name
invented from a listing dk could not read would be a guess. If DigiKey ever
ships the route, the fallback stops being reached on its own.

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
## API schema reference

Field names come from DigiKey's OpenAPI specs and are PascalCase; a lowercase key
is silently ignored by the API. The specs themselves are not in the repo — see
`docs/openapi/README.md` for why and how to fetch them; they are worth having
locally before changing any wire type. Two known inconsistencies are handled explicitly:

- `ProductVariation` spells stock `QuantityAvailableforPackageType` (lowercase
  `f`) in v4, with `QuantityAvailable` as an older alias — `Stock()` covers both.
- Category children are `Children` on the taxonomy endpoints but
  `ChildCategories` inside a `Product` — `CategoryNode.Children()` covers both.

Both APIs name this header `X-DIGIKEY-Account-Id` — Product Information v4
declares it on 5 paths, MyLists v1 on 6, and neither spec mentions
`X-DIGIKEY-Customer-Id` at all. The client sends both from one config value;
Customer-Id is a v3-era carryover kept only because DigiKey ignores unknown
headers, not because v4 wants it. Product Information's parameter description
adds that Account-Id "is a required field to receive a successful response"
under 2-legged OAuth, so a client-credentials caller with no `account_id`
configured is sending less than the API asks for.

`internal/digikey/testdata/listparts_obsolete.json` is a real `/parts` response,
lightly trimmed. Three things in it contradicted what the hand-written fixtures
assumed, so prefer it when changing list pricing:

- **`SelectedPackType` came back empty**; the selection lives in
  `SelectedPackOptionIndex`. `SelectedPackOption` therefore tries the index
  first and treats the name as a fallback. Matching on the name alone would
  never fire against real data — and could not work anyway: MyLists spells pack
  types as short codes (`CT`, `DKR`, `TR`) while Product Information uses
  `Cut Tape (CT)`, `Digi-Reel®`, `Tape & Reel (TR)`. The two vocabularies do
  not overlap. See `testdata/listparts_priced.json`.
- **`PackOptions` was an empty array** for an Obsolete, zero-stock part. There
  is no price anywhere on such a line, which is why it must read as *unpriced*
  (`unpriced_parts`) rather than free — a zero total that looks like a bargain
  is the failure mode to avoid.
- **`DigiKeyPartNumber` differed from `RequestedPartNumber`**: a `490-1532-1-ND`
  request resolved to the `-2-ND` reel. Never assume the two match.
