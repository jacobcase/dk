package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// SubstitutesResult is the JSON shape of `dk product --substitutes`.
type SubstitutesResult struct {
	PartNumber  string           `json:"part_number"`
	Substitutes []SubstituteView `json:"substitutes"`
	Count       int              `json:"count"`
}

// RecommendedResult is the JSON shape of `dk product --recommended`.
type RecommendedResult struct {
	PartNumber  string            `json:"part_number"`
	Recommended []RecommendedView `json:"recommended"`
}

// AlternatePackagingResult is the JSON shape of
// `dk product --alternate-packaging`.
type AlternatePackagingResult struct {
	PartNumber string        `json:"part_number"`
	Packaging  []SummaryView `json:"packaging"`
}

func newProductCommand(app *App) *cobra.Command {
	var (
		raw          bool
		variations   bool
		parameters   bool
		substitutes  bool
		recommended  bool
		altPackaging bool
		recLimit     int
	)

	cmd := &cobra.Command{
		Use:   "product <part-number>",
		Short: "Show details for one product",
		Long: `Show full detail for a DigiKey or manufacturer part number, including live
pricing and stock.

  dk product 1276-1000-1-ND
  dk product STM32G031K8T6 --parameters
  dk product 1276-1000-1-ND --variations

Works best with a DigiKey part number. Some manufacturer part numbers are used
by more than one manufacturer, in which case DigiKey may return a different part
than you expect.

Each packaging option has its own DigiKey part number; --variations lists them
so you can pick the right one for "dk list add".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			partNumber := strings.TrimSpace(args[0])
			if partNumber == "" {
				return usageErrorf("part number must not be empty")
			}
			// Checked here rather than with MarkFlagsMutuallyExclusive because
			// cobra validates flag groups after the pre-run hook, which would
			// leave the failure classified as a runtime error instead of a
			// usage error.
			if err := atMostOneOf(cmd, "variations", "parameters", "substitutes",
				"recommended", "alternate-packaging"); err != nil {
				return err
			}
			if err := app.checkRawFormat(raw); err != nil {
				return err
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}

			// --raw fetches through the raw path rather than re-encoding a decoded
			// struct, so the caller gets exactly what DigiKey sent.
			if raw {
				endpoint := digikey.RawProductDetails
				switch {
				case recommended:
					endpoint = digikey.RawProductRecommended
				case altPackaging:
					endpoint = digikey.RawProductAltPackaging
				case substitutes:
					endpoint = digikey.RawProductSubstitutes
				}
				payload, err := client.RawProductResponse(ctx, partNumber, endpoint, recLimit)
				if err != nil {
					return err
				}
				return app.printRaw(payload)
			}

			// These three query their own endpoints and return their own shapes.
			// Each is flattened into a dk view for the same reason ProductView
			// exists — one command's JSON should look like the next's — and each
			// initializes its slice so an empty result is [] rather than null.
			if recommended {
				recs, err := client.RecommendedProducts(ctx, partNumber, recLimit)
				if err != nil {
					return err
				}
				result := RecommendedResult{PartNumber: partNumber, Recommended: []RecommendedView{}}
				// DigiKey groups recommendations by requested product number;
				// with one part number in, there is only ever one group, so the
				// nesting is flattened away here.
				for _, r := range recs {
					for _, p := range r.RecommendedProducts {
						result.Recommended = append(result.Recommended, RecommendedView{
							DigiKeyPartNumber:      p.DigiKeyProductNumber,
							ManufacturerPartNumber: p.ManufacturerProductNumber,
							Manufacturer:           p.ManufacturerName,
							Description:            p.ProductDescription,
							UnitPrice:              p.UnitPrice,
							QuantityAvailable:      p.QuantityAvailable,
							ProductURL:             normalizeAssetURL(p.ProductURL),
						})
					}
				}
				return app.Printer.Print(result, recommendedTable(result.Recommended))
			}

			if altPackaging {
				packs, err := client.AlternatePackaging(ctx, partNumber)
				if err != nil {
					return err
				}
				result := AlternatePackagingResult{
					PartNumber: partNumber,
					Packaging:  summaryViews(packs),
				}
				return app.Printer.Print(result, summaryTable(result.Packaging, "PACKAGING OPTION"))
			}

			if substitutes {
				resp, err := client.Substitutions(ctx, partNumber)
				if err != nil {
					return err
				}
				result := SubstitutesResult{
					PartNumber:  partNumber,
					Substitutes: []SubstituteView{},
				}
				for _, s := range resp.ProductSubstitutes {
					result.Substitutes = append(result.Substitutes, SubstituteView{
						SubstituteType: s.SubstituteType,
						SummaryView: SummaryView{
							DigiKeyPartNumber:      s.DigiKeyProductNumber,
							ManufacturerPartNumber: s.ManufacturerProductNumber,
							Manufacturer:           s.Manufacturer.Name,
							Description:            s.Description,
							UnitPrice:              s.UnitPrice,
							QuantityAvailable:      s.QuantityAvailable,
							ProductURL:             normalizeAssetURL(s.ProductURL),
						},
					})
				}
				// DigiKey's own count is echoed rather than derived, so a
				// mismatch with the array length stays visible.
				result.Count = resp.ProductSubstitutesCount
				return app.Printer.Print(result, substitutesTable(result.Substitutes))
			}

			details, err := client.ProductDetails(ctx, partNumber)
			if err != nil {
				return err
			}

			currency := details.SearchLocaleUsed.Currency
			if currency == "" {
				currency = app.Cfg.Locale.Currency
			}
			view := withDetails(newProductView(details.Product, currency), details.Product)

			// The table view is one section at a time; JSON always carries the
			// whole view so a program never has to make three calls.
			switch {
			case variations:
				return app.Printer.Print(view, variationsTable(view.Variations))
			case parameters:
				return app.Printer.Print(view, parametersTable(view.Parameters))
			default:
				// One Print per command: price breaks are rows in this table,
				// not a table of their own. See detailPairs.
				if err := app.Printer.Print(view, output.KeyValueTable(detailPairs(view))); err != nil {
					return err
				}
				if len(view.Variations) > 1 {
					app.Printer.PrintText("\n%d packaging variations; run with --variations to list them.", len(view.Variations))
				}
				return nil
			}
		},
	}

	f := cmd.Flags()
	f.BoolVar(&variations, "variations", false, "show every packaging variation and its DigiKey part number")
	f.BoolVar(&parameters, "parameters", false, "show parametric attributes")
	f.BoolVar(&substitutes, "substitutes", false, "show substitute parts instead of details")
	f.BoolVar(&recommended, "recommended", false, "show products commonly bought with this one")
	// DigiKey defaults this endpoint to a single result, so a default of 1 here
	// would silently look like "there is only one recommendation".
	f.IntVar(&recLimit, "recommended-limit", 10, "how many recommendations to request (DigiKey defaults to 1)")
	f.BoolVar(&altPackaging, "alternate-packaging", false, "show the same part in other packaging")
	f.BoolVar(&raw, "raw", false, "emit DigiKey's unmodified response (implies --output json)")

	return cmd
}

// atMostOneOf returns a usage error if more than one of the named boolean
// flags was set. Zero is allowed; the caller falls back to its default view.
func atMostOneOf(cmd *cobra.Command, names ...string) error {
	var set []string
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			set = append(set, "--"+name)
		}
	}
	if len(set) > 1 {
		return usageErrorf("%s cannot be combined; pick one (or use --output json, which returns all of them)", strings.Join(set, " and "))
	}
	return nil
}

// recommendedTable renders "customers also bought" suggestions.
func recommendedTable(recs []RecommendedView) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT"},
		Empty:   "DigiKey has no recommendations for this product.",
	}
	for _, p := range recs {
		t.AddRow(
			p.DigiKeyPartNumber,
			p.ManufacturerPartNumber,
			output.Truncate(p.Manufacturer, 20),
			output.Truncate(p.Description, 40),
			p.QuantityAvailable,
			output.Money(p.UnitPrice),
		)
	}
	return t
}

// summaryTable renders the flattened summary shape shared by the
// alternate-packaging and association responses.
func summaryTable(items []SummaryView, empty string) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT"},
		Empty:   "No " + strings.ToLower(empty) + "s returned.",
	}
	for _, p := range items {
		t.AddRow(
			p.DigiKeyPartNumber,
			p.ManufacturerPartNumber,
			output.Truncate(p.Manufacturer, 20),
			output.Truncate(p.Description, 40),
			p.QuantityAvailable,
			p.UnitPrice,
		)
	}
	return t
}

func variationsTable(variations []VariationView) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "PACKAGING", "STOCK", "MOQ", "UNIT", "MARKETPLACE"},
		Empty:   "No packaging variations returned.",
	}
	for _, v := range variations {
		t.AddRow(v.DigiKeyPartNumber, v.Packaging, v.QuantityAvailable, v.MinimumOrderQuantity, output.Money(v.UnitPrice), v.MarketPlace)
	}
	return t
}

func parametersTable(params []ParameterView) *output.Table {
	t := &output.Table{Headers: []string{"PARAMETER", "VALUE"}, Empty: "No parameters returned."}
	for _, p := range params {
		t.AddRow(p.Name, p.Value)
	}
	return t
}

func substitutesTable(subs []SubstituteView) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT", "TYPE"},
		Empty:   "No substitutes listed.",
	}
	for _, s := range subs {
		t.AddRow(
			s.DigiKeyPartNumber,
			s.ManufacturerPartNumber,
			output.Truncate(s.Manufacturer, 22),
			output.Truncate(s.Description, 40),
			s.QuantityAvailable,
			s.UnitPrice,
			s.SubstituteType,
		)
	}
	return t
}

func newCategoriesCommand(app *App) *cobra.Command {
	var flat bool

	cmd := &cobra.Command{
		Use:     "categories [category-id]",
		Aliases: []string{"category"},
		Short:   "List DigiKey product categories",
		Long: `List the DigiKey category taxonomy. Category ids feed "dk search --category",
which accepts either an id or a name.

  dk categories
  dk categories --flat
  dk categories 3 `,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}

			var nodes []digikey.CategoryNode
			if len(args) == 1 {
				id, err := parsePositiveInt(args[0], "category id")
				if err != nil {
					return err
				}
				node, err := client.Category(ctx, id)
				if err != nil {
					return err
				}
				nodes = []digikey.CategoryNode{*node}
			} else {
				nodes, err = client.Categories(ctx)
				if err != nil {
					return err
				}
			}

			if flat {
				items := newNamedIDViews(flattenCategories(nodes, nil))
				return app.Printer.Print(items, namedIDTable(items, "CATEGORY"))
			}
			views := newCategoryViews(nodes)
			return app.Printer.Print(views, categoryTable(views))
		},
	}
	cmd.Flags().BoolVar(&flat, "flat", false, "flatten the tree into a single id/name list")
	return cmd
}

// categoryTable renders the taxonomy with indentation to convey depth.
func categoryTable(nodes []CategoryView) *output.Table {
	t := &output.Table{Headers: []string{"ID", "CATEGORY", "PRODUCTS"}, Empty: "No categories returned."}
	var walk func(ns []CategoryView, depth int)
	walk = func(ns []CategoryView, depth int) {
		for _, n := range ns {
			t.AddRow(n.ID, strings.Repeat("  ", depth)+n.Name, n.ProductCount)
			walk(n.Children, depth+1)
		}
	}
	walk(nodes, 0)
	return t
}

func newManufacturersCommand(app *App) *cobra.Command {
	var filter string

	cmd := &cobra.Command{
		Use:     "manufacturers",
		Aliases: []string{"manufacturer", "mfrs"},
		Short:   "List DigiKey manufacturers",
		Long: `List manufacturer ids and names. Ids feed "dk search --manufacturer", which
also accepts names directly.

  dk manufacturers --filter murata`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := app.Client()
			if err != nil {
				return err
			}
			items, err := client.Manufacturers(cmd.Context())
			if err != nil {
				return err
			}
			if filter != "" {
				needle := strings.ToLower(filter)
				matched := make([]digikey.NamedID, 0, len(items))
				for _, m := range items {
					if strings.Contains(strings.ToLower(m.Name), needle) {
						matched = append(matched, m)
					}
				}
				items = matched
			}
			views := newNamedIDViews(items)
			return app.Printer.Print(views, namedIDTable(views, "MANUFACTURER"))
		},
	}
	cmd.Flags().StringVar(&filter, "filter", "", "case-insensitive substring filter on the manufacturer name")
	return cmd
}
