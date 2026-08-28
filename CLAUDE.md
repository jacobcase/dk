# dk — notes for Claude Code

## Using the CLI (most common case)

If you are here to *search DigiKey or build a parts list*, not to modify this
repo, run **`dk guide`** first. It prints the full contract: output format, exit
codes, error shapes, which commands need which authentication, and a recommended
workflow. Nothing else in this file is relevant to that.

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

## Testing

```
make test        # go test ./...
make test-race   # -race -cover
make lint        # gofmt check + go vet
make lint-full   # the golangci-lint set CI runs
```

Tests never touch the real API: `DIGIKEY_API_BASE_URL` points the client at an
`httptest` server and `DK_CONFIG_DIR` at a temp dir. Prefer table-driven tests,
and say what the assertion protects in the failure message.

## Before changing anything

**Read [CONTRIBUTING.md](CONTRIBUTING.md).** Its design notes are not in this
file because they are long and rarely all needed at once, but they are
load-bearing:

- **Design invariants** — the rules that keep the CLI safe to drive from a
  program (stdout carries only the result, every failure is classified, commands
  take `*App`, `--raw` means untouched bytes, list commands accept a name or an
  id, anything needing a complete list pages for it).
- **Response cache internals** — what is cached, what the key covers, and why
  invalidation is not conditional on this run reading the cache.
- **Pairing a part number with its price** — two commands once reported an
  identifier and a figure that described different things.
- **Settled against the live API — do not reopen** — questions already answered
  by probing the real API, with the answers and the dates. Read this one before
  proposing a defensive guard; several entries are negative results recorded
  specifically to stop a plausible-looking fix from being re-proposed.
- **Endpoint coverage**, **Auth model**, **API schema reference** — which
  endpoints are wrapped and which are deliberately not, which token each needs,
  and DigiKey's known field-name inconsistencies.

During a code review, "Settled against the live API" and "Design invariants" are
the two that most often decide whether a finding is real.
