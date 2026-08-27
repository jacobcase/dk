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
internal/config/   config file + env resolution (~/.config/dk, XDG not
                   os.UserConfigDir — see Dir())
internal/cache/    on-disk response cache, keyed per token/locale/query
internal/output/   json / table / csv rendering
internal/atomicfile/  crash-safe file replacement, shared by config, auth, and
                   the response cache
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
