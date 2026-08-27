package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// maxSearchLimit is DigiKey's hard cap on results per keyword request.
const maxSearchLimit = 50

// SearchResult is the JSON shape of `dk search`.
type SearchResult struct {
	Query string `json:"query"`
	// TotalMatches is DigiKey's total, which is usually far larger than the
	// page returned; use --offset to page through it.
	TotalMatches int           `json:"total_matches"`
	Returned     int           `json:"returned"`
	Offset       int           `json:"offset"`
	Currency     string        `json:"currency,omitempty"`
	Products     []ProductView `json:"products"`
}

// sortFieldAliases maps the friendly --sort values onto DigiKey's field names.
var sortFieldAliases = map[string]string{
	"price":        "Price",
	"stock":        "QuantityAvailable",
	"quantity":     "QuantityAvailable",
	"mpn":          "ManufacturerProductNumber",
	"dkpn":         "DigiKeyProductNumber",
	"manufacturer": "Manufacturer",
	"packaging":    "Packaging",
	"status":       "ProductStatus",
	"moq":          "MinimumQuantity",
	"supplier":     "Supplier",
}

func sortFieldNames() []string {
	names := make([]string, 0, len(sortFieldAliases))
	for k := range sortFieldAliases {
		names = append(names, k)
	}
	return names
}

func newSearchCommand(app *App) *cobra.Command {
	var (
		limit         int
		offset        int
		inStock       bool
		minQty        int
		manufacturers []string
		categories    []string
		params        []string
		rohs          bool
		hasDatasheet  bool
		noMarketplace bool
		sortBy        string
		descending    bool
		exactOnly     bool
		raw           bool
		full          bool
		descWidth     int
	)

	cmd := &cobra.Command{
		Use:   "search <keywords...>",
		Short: "Search the DigiKey catalog",
		Long: `Search the DigiKey catalog by keyword, part number, or parametric description.

Keywords are joined with spaces, so quoting is optional:

  dk search 0.1uF 0603 X7R 50V
  dk search "STM32G031K8T6"
  dk search "10k resistor 0603 1%" --in-stock --sort price --limit 5

The DKPN column is the DigiKey part number to pass to "dk list add". Note that
each packaging option (cut tape, tape & reel, digi-reel) has its own DKPN; run
"dk product <part> --variations" to see them all.

Parametric filtering uses --param, with names and values as DigiKey spells them:

  dk search "0603 ceramic capacitor" --param "Capacitance=0.1 µF" --param "Tolerance=±10%"

Several values on one parameter mean "any of these"; separate parameters are
combined with AND:

  dk search "0603 resistor" --param "Resistance=10 kOhms,4.7 kOhms"

Run "dk filters <keywords>" first to see which parameters exist for a query and
what values they accept. Parameter names and values are matched case-insensitively,
and a unique prefix or substring is enough. Parameter ids are category-scoped, so
--param implies a category; it is inferred from the keywords unless you pass
--category.

Uses application-level (2-legged) auth, so no "dk auth login" is needed.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keywords := strings.Join(args, " ")
			if limit < 1 || limit > maxSearchLimit {
				return usageErrorf("--limit must be between 1 and %d (DigiKey's cap)", maxSearchLimit)
			}
			if offset < 0 {
				return usageErrorf("--offset must not be negative")
			}
			if err := app.checkRawFormat(raw); err != nil {
				return err
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}

			filters := &digikey.FilterOptionsRequest{}
			if inStock {
				filters.SearchOptions = append(filters.SearchOptions, digikey.SearchOptionInStock)
			}
			if rohs {
				filters.SearchOptions = append(filters.SearchOptions, digikey.SearchOptionRohsCompliant)
			}
			if hasDatasheet {
				filters.SearchOptions = append(filters.SearchOptions, digikey.SearchOptionHasDatasheet)
			}
			if noMarketplace {
				filters.MarketPlaceFilter = "ExcludeMarketPlace"
			}
			if minQty > 0 {
				filters.MinimumQuantityAvailable = minQty
			}

			if len(manufacturers) > 0 {
				ids, err := resolveManufacturerIDs(ctx, client, manufacturers)
				if err != nil {
					return err
				}
				filters.ManufacturerFilter = ids
			}
			if len(categories) > 0 {
				ids, err := resolveCategoryIDs(ctx, client, categories)
				if err != nil {
					return err
				}
				filters.CategoryFilter = ids
			}

			if len(params) > 0 {
				// Parametric filtering needs a facet lookup: parameter and value
				// ids are only knowable from a search response. One extra call
				// buys name-based filtering instead of opaque numeric ids.
				categoryHint := ""
				if len(categories) > 0 {
					categoryHint = categories[0]
				}
				parametric, categoryID, err := resolveParams(ctx, client, keywords, categoryHint, inStock, params)
				if err != nil {
					return err
				}
				if categoryID == 0 {
					return usageErrorf("could not determine which category these parameters belong to; " +
						"pass --category (see `dk filters` for the categories matching your keywords)")
				}
				filters.ParameterFilterRequest = &digikey.ParameterFilterRequest{
					CategoryFilter:   &digikey.FilterID{ID: strconv.Itoa(categoryID)},
					ParameterFilters: parametric,
				}
				// DigiKey ignores parametric filters unless the search is scoped
				// to the owning category, so make that scoping explicit.
				if len(filters.CategoryFilter) == 0 {
					filters.CategoryFilter = []digikey.FilterID{{ID: strconv.Itoa(categoryID)}}
				}
			}

			req := digikey.KeywordRequest{
				Keywords: keywords,
				Limit:    limit,
				Offset:   offset,
			}
			if !isZeroFilter(filters) {
				req.FilterOptionsRequest = filters
			}
			if sortBy != "" {
				field, ok := sortFieldAliases[strings.ToLower(sortBy)]
				if !ok {
					return usageErrorf("invalid --sort %q (want one of: %s)", sortBy, strings.Join(sortFieldNames(), ", "))
				}
				order := "Ascending"
				if descending {
					order = "Descending"
				}
				req.SortOptions = &digikey.SortOptions{Field: field, SortOrder: order}
			}

			if raw {
				payload, err := client.RawKeywordSearch(ctx, req)
				if err != nil {
					return err
				}
				return app.printRaw(payload)
			}

			resp, err := client.KeywordSearch(ctx, req)
			if err != nil {
				return err
			}

			products := resp.Products
			if exactOnly {
				products = resp.ExactMatches
			}

			currency := resp.SearchLocaleUsed.Currency
			if currency == "" {
				currency = app.Cfg.Locale.Currency
			}

			views := make([]ProductView, 0, len(products))
			for _, p := range products {
				v := newProductView(p, currency)
				if full {
					v = withDetails(v, p)
				} else {
					// Keep list output compact; the long description is a
					// duplicate of the short one plus parametrics.
					v.DetailedDescription = ""
				}
				views = append(views, v)
			}

			result := SearchResult{
				Query:        keywords,
				TotalMatches: resp.ProductsCount,
				Returned:     len(views),
				Offset:       offset,
				Currency:     currency,
				Products:     views,
			}
			// ExactMatches is a separate, unpaged array on the same response, so
			// ProductsCount describes the keyword result set rather than this
			// one. Reporting it here would advertise pages that --offset cannot
			// reach, and the hint below would loop forever.
			if exactOnly {
				result.TotalMatches = len(views)
			}

			if err := app.Printer.Print(result, productTable(views, descWidth)); err != nil {
				return err
			}
			if shown := offset + len(views); !exactOnly && resp.ProductsCount > shown {
				app.Printer.PrintText("\n%d of %d matches shown. Use --offset %d for the next page.",
					shown, resp.ProductsCount, shown)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVarP(&limit, "limit", "n", 10, fmt.Sprintf("number of results to return (1-%d)", maxSearchLimit))
	f.IntVar(&offset, "offset", 0, "index of the first result, for paging")
	f.BoolVar(&inStock, "in-stock", false, "only products with stock on hand")
	f.IntVar(&minQty, "min-qty", 0, "only products with at least this many in stock")
	f.StringSliceVar(&manufacturers, "manufacturer", nil, "restrict to manufacturers by name or id (repeatable)")
	f.StringSliceVar(&categories, "category", nil, "restrict to categories by name or id (repeatable)")
	// StringArray, not StringSlice: values like "1 µF, 10%" contain commas that
	// must not be split into separate flag values.
	f.StringArrayVar(&params, "param", nil,
		"parametric filter as NAME=VALUE or NAME=VALUE,VALUE (repeatable); run `dk filters` to discover names and values")
	f.BoolVar(&rohs, "rohs", false, "only RoHS compliant products")
	f.BoolVar(&hasDatasheet, "has-datasheet", false, "only products with a datasheet")
	f.BoolVar(&noMarketplace, "no-marketplace", false, "exclude Marketplace items, which ship separately from the supplier")
	f.StringVar(&sortBy, "sort", "", fmt.Sprintf("sort field: %s", strings.Join(sortFieldNames(), ", ")))
	f.BoolVar(&descending, "desc", false, "sort descending")
	f.BoolVar(&exactOnly, "exact", false, "only exact part number matches")
	f.BoolVar(&full, "full", false, "include parameters, price breaks, and all packaging variations")
	f.BoolVar(&raw, "raw", false, "emit DigiKey's unmodified response (implies --output json)")
	f.IntVar(&descWidth, "desc-width", 44, "truncate the description column to this many characters (0 disables)")

	_ = cmd.RegisterFlagCompletionFunc("sort", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return sortFieldNames(), cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// isZeroFilter reports whether the filter request would add nothing to the
// payload. DigiKey rejects some empty filter objects, so they are omitted.
func isZeroFilter(f *digikey.FilterOptionsRequest) bool {
	return len(f.SearchOptions) == 0 &&
		len(f.ManufacturerFilter) == 0 &&
		len(f.CategoryFilter) == 0 &&
		len(f.StatusFilter) == 0 &&
		len(f.PackagingFilter) == 0 &&
		len(f.SeriesFilter) == 0 &&
		f.MarketPlaceFilter == "" &&
		f.MinimumQuantityAvailable == 0 &&
		f.ParameterFilterRequest == nil
}

// filterIndex describes one kind of name-to-id lookup a search filter accepts.
type filterIndex struct {
	// kind is the singular noun used in error messages: "manufacturer".
	kind string
	// command is the dk subcommand that lists valid values: "manufacturers".
	command string
	// fetch returns the flat id/name index, and is called at most once.
	fetch func(context.Context) ([]digikey.NamedID, error)
}

// resolveFilterIDs turns names or numeric ids into DigiKey filter ids.
// Accepting names matters for agent callers, which know "Murata" but not 2359.
//
// The index is only fetched when at least one value actually needs resolving,
// so a caller passing ids alone costs no extra request.
func resolveFilterIDs(ctx context.Context, values []string, idx filterIndex) ([]digikey.FilterID, error) {
	var index []digikey.NamedID
	for _, v := range values {
		if _, err := strconv.Atoi(strings.TrimSpace(v)); err != nil {
			index, err = idx.fetch(ctx)
			if err != nil {
				return nil, fmt.Errorf("look up %s list: %w", idx.kind, err)
			}
			break
		}
	}

	out := make([]digikey.FilterID, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, err := strconv.Atoi(v); err == nil {
			out = append(out, digikey.FilterID{ID: v})
			continue
		}
		id, err := matchNamedID(v, index, idx)
		if err != nil {
			return nil, err
		}
		out = append(out, digikey.FilterID{ID: strconv.Itoa(id)})
	}
	return out, nil
}

func resolveManufacturerIDs(ctx context.Context, client *digikey.Client, values []string) ([]digikey.FilterID, error) {
	return resolveFilterIDs(ctx, values, filterIndex{
		kind:    "manufacturer",
		command: "manufacturers",
		fetch:   func(ctx context.Context) ([]digikey.NamedID, error) { return client.Manufacturers(ctx) },
	})
}

// resolveCategoryIDs is resolveManufacturerIDs for the category taxonomy. The
// taxonomy is a tree, so it is flattened before matching.
func resolveCategoryIDs(ctx context.Context, client *digikey.Client, values []string) ([]digikey.FilterID, error) {
	return resolveFilterIDs(ctx, values, filterIndex{
		kind:    "category",
		command: "categories",
		fetch: func(ctx context.Context) ([]digikey.NamedID, error) {
			tree, err := client.Categories(ctx)
			if err != nil {
				return nil, err
			}
			return flattenCategories(tree, nil), nil
		},
	})
}

func flattenCategories(nodes []digikey.CategoryNode, acc []digikey.NamedID) []digikey.NamedID {
	for _, n := range nodes {
		acc = append(acc, digikey.NamedID{ID: n.CategoryID, Name: n.Name})
		acc = flattenCategories(n.Children(), acc)
	}
	return acc
}

// matchNamedID resolves a user-supplied name against an id index. An exact
// case-insensitive name wins; otherwise a unique substring match is accepted.
func matchNamedID(name string, index []digikey.NamedID, idx filterIndex) (int, error) {
	var exact, partial []digikey.NamedID
	lower := strings.ToLower(name)
	for _, n := range index {
		nl := strings.ToLower(n.Name)
		switch {
		case nl == lower:
			exact = append(exact, n)
		case strings.Contains(nl, lower):
			partial = append(partial, n)
		}
	}
	if len(exact) == 1 {
		return exact[0].ID, nil
	}
	if len(exact) > 1 {
		return 0, usageErrorf("%s %q is ambiguous (%s)", idx.kind, name, describeCandidates(exact))
	}
	switch len(partial) {
	case 0:
		return 0, usageErrorf("unknown %s %q; run `dk %s` to list valid values", idx.kind, name, idx.command)
	case 1:
		return partial[0].ID, nil
	default:
		return 0, usageErrorf("%s %q matches %d entries (%s); be more specific or pass the numeric id",
			idx.kind, name, len(partial), describeCandidates(partial))
	}
}

func describeCandidates(candidates []digikey.NamedID) string {
	const maxShown = 5
	parts := make([]string, 0, maxShown)
	for i, c := range candidates {
		if i == maxShown {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", c.Name, c.ID))
	}
	return strings.Join(parts, ", ")
}

// namedIDTable renders manufacturers or flat categories.
func namedIDTable(items []digikey.NamedID, header string) *output.Table {
	t := &output.Table{Headers: []string{"ID", header}, Empty: "No results."}
	for _, item := range items {
		t.AddRow(item.ID, item.Name)
	}
	return t
}
