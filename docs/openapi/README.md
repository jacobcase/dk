# DigiKey OpenAPI specs

The `*.json` specs that belong in this directory are **deliberately not
committed** — `.gitignore` excludes them. DigiKey's API User Agreement
(<https://developer.digikey.com/api-user-agreement>) defines "Documentation" as
"any documentation, specifications, and other materials ... related to the API",
grants only a non-transferable, non-sublicensable, personal license to *access*
it (§3.1.3), and separately forbids licensees to "distribute, publish, transfer,
or otherwise make available the API or Documentation" (§3.2(iii)). §4 also names
the Documentation as DigiKey Confidential Information. Committing the specs to
this public repo would be publishing them; the repo's MIT license would also
purport to sublicense them, which §3.1 does not permit.

To get them, sign in at <https://developer.digikey.com/products>, open the API
you need, and use the spec download on the endpoint page. Each download is the
**whole** API, not one operation:

| File | API | Base path | Paths |
|---|---|---|---|
| `ProductSearch.json` | Product Information v4 | `/products/v4` | 14 |
| `MyLists.json` | MyLists v1 | `/mylists/v1` | 7 |

Both are Swagger 2.0, self-contained (every `$ref` resolves inside the file),
and are what `internal/digikey` is modeled against. dk calls 11 of the 14
Product Information paths (`/pricing`, `/pricingbyquantity`, and
`/digireelpricing` are the documented omissions) and 7 of the 9 MyLists
operations. The Product Change Notifications API has its own spec download and
is deliberately not kept here: dk never calls it, and "PCN" in this codebase is
a MediaType string from `/search/{productNumber}/media`, not that API.
