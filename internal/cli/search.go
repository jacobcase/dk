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

			resp, err := client.KeywordSearch(ctx, req)
			if err != nil {
				return err
			}

			if raw {
				return app.Printer.Print(resp, nil)
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

			table := productTable(views, descWidth)
			if resp.ProductsCount > offset+len(views) {
				table.Empty = "No products matched."
			}
			if err := app.Printer.Print(result, table); err != nil {
				return err
			}
			if remaining := resp.ProductsCount - (offset + len(views)); remaining > 0 {
				app.Printer.PrintText("\n%d of %d matches shown. Use --offset %d for the next page.",
					offset+len(views), resp.ProductsCount, offset+len(views))
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

// resolveManufacturerIDs turns names or numeric ids into DigiKey filter ids.
// Accepting names matters for agent callers, which know "Murata" but not 2359.
func resolveManufacturerIDs(ctx context.Context, client *digikey.Client, values []string) ([]digikey.FilterID, error) {
	var needLookup bool
	for _, v := range values {
		if _, err := strconv.Atoi(strings.TrimSpace(v)); err != nil {
			needLookup = true
			break
		}
	}

	var index []digikey.NamedID
	if needLookup {
		list, err := client.Manufacturers(ctx)
		if err != nil {
			return nil, fmt.Errorf("look up manufacturers: %w", err)
		}
		index = list
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
		id, err := matchNamedID(v, index, "manufacturer")
		if err != nil {
			return nil, err
		}
		out = append(out, digikey.FilterID{ID: strconv.Itoa(id)})
	}
	return out, nil
}

// resolveCategoryIDs is resolveManufacturerIDs for the category taxonomy. The
// taxonomy is a tree, so it is flattened before matching.
func resolveCategoryIDs(ctx context.Context, client *digikey.Client, values []string) ([]digikey.FilterID, error) {
	var needLookup bool
	for _, v := range values {
		if _, err := strconv.Atoi(strings.TrimSpace(v)); err != nil {
			needLookup = true
			break
		}
	}

	var index []digikey.NamedID
	if needLookup {
		tree, err := client.Categories(ctx)
		if err != nil {
			return nil, fmt.Errorf("look up categories: %w", err)
		}
		index = flattenCategories(tree, nil)
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
		id, err := matchNamedID(v, index, "category")
		if err != nil {
			return nil, err
		}
		out = append(out, digikey.FilterID{ID: strconv.Itoa(id)})
	}
	return out, nil
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
func matchNamedID(name string, index []digikey.NamedID, kind string) (int, error) {
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
		return 0, usageErrorf("%s %q is ambiguous (%s)", kind, name, describeCandidates(exact))
	}
	switch len(partial) {
	case 0:
		return 0, usageErrorf("unknown %s %q; run `dk %ss` to list valid values", kind, name, kind)
	case 1:
		return partial[0].ID, nil
	default:
		return 0, usageErrorf("%s %q matches %d entries (%s); be more specific or pass the numeric id",
			kind, name, len(partial), describeCandidates(partial))
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
