package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

func newProductCommand(app *App) *cobra.Command {
	var (
		raw          bool
		variations   bool
		parameters   bool
		substitutes  bool
		recommended  bool
		altPackaging bool
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
			if err := exactlyOneOf(cmd, "variations", "parameters", "substitutes",
				"recommended", "alternate-packaging"); err != nil {
				return err
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}

			if recommended {
				recs, err := client.RecommendedProducts(ctx, partNumber)
				if err != nil {
					return err
				}
				if raw {
					return app.Printer.Print(recs, nil)
				}
				return app.Printer.Print(recs, recommendedTable(recs))
			}

			if altPackaging {
				packs, err := client.AlternatePackaging(ctx, partNumber)
				if err != nil {
					return err
				}
				if raw {
					return app.Printer.Print(packs, nil)
				}
				return app.Printer.Print(packs, summaryTable(packs, "PACKAGING OPTION"))
			}

			if substitutes {
				resp, err := client.Substitutions(ctx, partNumber)
				if err != nil {
					return err
				}
				if raw {
					return app.Printer.Print(resp, nil)
				}
				return app.Printer.Print(resp, substitutesTable(resp.ProductSubstitutes))
			}

			details, err := client.ProductDetails(ctx, partNumber)
			if err != nil {
				return err
			}
			if raw {
				return app.Printer.Print(details, nil)
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
				pairs := detailPairs(view)
				table := output.KeyValueTable(pairs)
				if err := app.Printer.Print(view, table); err != nil {
					return err
				}
				if len(view.PriceBreaks) > 0 {
					app.Printer.PrintText("\nPrice breaks (%s):", currency)
					if app.Printer.Format != output.FormatJSON {
						if err := app.Printer.Print(view.PriceBreaks, priceBreakTable(view.PriceBreaks)); err != nil {
							return err
						}
					}
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
	f.BoolVar(&altPackaging, "alternate-packaging", false, "show the same part in other packaging")
	f.BoolVar(&raw, "raw", false, "emit DigiKey's unmodified response (implies --output json)")

	return cmd
}

// exactlyOneOf returns a usage error if more than one of the named boolean
// flags was set. Zero is allowed; the caller falls back to its default view.
func exactlyOneOf(cmd *cobra.Command, names ...string) error {
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
func recommendedTable(recs []digikey.Recommendation) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT"},
		Empty:   "DigiKey has no recommendations for this product.",
	}
	for _, r := range recs {
		for _, p := range r.RecommendedProducts {
			t.AddRow(
				p.DigiKeyProductNumber,
				p.ManufacturerProductNumber,
				output.Truncate(p.ManufacturerName, 20),
				output.Truncate(p.ProductDescription, 40),
				p.QuantityAvailable,
				output.Money(p.UnitPrice),
			)
		}
	}
	return t
}

// summaryTable renders the compact ProductSummary shape shared by the
// alternate-packaging and association responses.
func summaryTable(items []digikey.ProductSummary, empty string) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT"},
		Empty:   "No " + strings.ToLower(empty) + "s returned.",
	}
	for _, p := range items {
		t.AddRow(
			p.DigiKeyProductNumber,
			p.ManufacturerProductNumber,
			output.Truncate(p.Manufacturer.Name, 20),
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

func priceBreakTable(breaks []PriceBreakView) *output.Table {
	t := &output.Table{Headers: []string{"QTY", "UNIT", "EXTENDED"}, Empty: "No price breaks returned."}
	for _, b := range breaks {
		t.AddRow(b.Quantity, output.Money(b.UnitPrice), output.Money(b.TotalPrice))
	}
	return t
}

func substitutesTable(subs []digikey.ProductSubstitute) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT", "TYPE"},
		Empty:   "No substitutes listed.",
	}
	for _, s := range subs {
		t.AddRow(
			s.DigiKeyProductNumber,
			s.ManufacturerProductNumber,
			output.Truncate(s.Manufacturer.Name, 22),
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
				items := flattenCategories(nodes, nil)
				return app.Printer.Print(items, namedIDTable(items, "CATEGORY"))
			}
			return app.Printer.Print(nodes, categoryTable(nodes))
		},
	}
	cmd.Flags().BoolVar(&flat, "flat", false, "flatten the tree into a single id/name list")
	return cmd
}

// categoryTable renders the taxonomy with indentation to convey depth.
func categoryTable(nodes []digikey.CategoryNode) *output.Table {
	t := &output.Table{Headers: []string{"ID", "CATEGORY", "PRODUCTS"}, Empty: "No categories returned."}
	var walk func(ns []digikey.CategoryNode, depth int)
	walk = func(ns []digikey.CategoryNode, depth int) {
		for _, n := range ns {
			t.AddRow(n.CategoryID, strings.Repeat("  ", depth)+n.Name, n.ProductCount)
			walk(n.Children(), depth+1)
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
			return app.Printer.Print(items, namedIDTable(items, "MANUFACTURER"))
		},
	}
	cmd.Flags().StringVar(&filter, "filter", "", "case-insensitive substring filter on the manufacturer name")
	return cmd
}
