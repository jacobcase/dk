package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// RelatedProduct is one product related to the one asked about.
//
// The embedded SummaryView is the same shape `dk product --alternate-packaging`
// and `--substitutes` return, because all three wrap DigiKey's ProductSummary.
// Sharing it is what keeps them from drifting apart; the embedding is inlined
// by encoding/json, so the JSON stays flat.
type RelatedProduct struct {
	// Relation is why this product is listed: mating, kit, accessory, or
	// for-use-with.
	Relation string `json:"relation"`
	SummaryView
}

// RelatedResult is the JSON shape of `dk related`.
type RelatedResult struct {
	PartNumber string           `json:"part_number"`
	Products   []RelatedProduct `json:"products"`
	// Counts breaks the total down by relation, so a caller can tell at a
	// glance whether a mating half exists without scanning the list.
	Counts map[string]int `json:"counts"`
}

// relationKinds maps the --kind values onto DigiKey's association groups.
var relationKinds = []struct {
	Key   string
	Label string
	Pick  func(digikey.ProductAssociations) []digikey.ProductSummary
}{
	{"mating", "mating", func(a digikey.ProductAssociations) []digikey.ProductSummary { return a.MatingProducts }},
	{"kits", "kit", func(a digikey.ProductAssociations) []digikey.ProductSummary { return a.Kits }},
	{"accessories", "accessory", func(a digikey.ProductAssociations) []digikey.ProductSummary { return a.AssociatedProducts }},
	{"for-use-with", "for-use-with", func(a digikey.ProductAssociations) []digikey.ProductSummary { return a.ForUseWithProducts }},
}

func relationKindNames() []string {
	names := make([]string, 0, len(relationKinds)+1)
	names = append(names, "all")
	for _, k := range relationKinds {
		names = append(names, k.Key)
	}
	return names
}

func newRelatedCommand(app *App) *cobra.Command {
	var kind string

	cmd := &cobra.Command{
		Use:     "related <part-number>",
		Aliases: []string{"associations", "accessories"},
		Short:   "Show mating halves, kits, and accessories for a product",
		Long: `Show the products DigiKey relates to a part: the other half of a connector
pair, kits that contain it, and accessories such as crimpers and tools.

  dk related WM4200-ND
  dk related WM4200-ND --kind mating

This answers "what else do I need to buy for this to work?", which is a
different question from "dk product --substitutes" ("what could I buy instead").

Relations:
  mating        the other half of a connector pair
  kits          assortments that contain this product
  accessories   tools and hardware associated with it
  for-use-with  products this one is intended to be used with

Works best with a DigiKey part number. Many products have no associations at
all, which is a valid empty result rather than an error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			partNumber := strings.TrimSpace(args[0])
			if partNumber == "" {
				return usageErrorf("part number must not be empty")
			}
			if kind != "" && kind != "all" && !validRelationKind(kind) {
				return usageErrorf("invalid --kind %q (want one of: %s)", kind, strings.Join(relationKindNames(), ", "))
			}

			client, err := app.Client()
			if err != nil {
				return err
			}
			resp, err := client.Associations(cmd.Context(), partNumber)
			if err != nil {
				return err
			}

			// Products starts as an empty slice, not nil: most parts have no
			// associations, and the guide calls that a normal result, so it must
			// serialize as [] rather than null.
			result := RelatedResult{
				PartNumber: partNumber,
				Products:   []RelatedProduct{},
				Counts:     map[string]int{},
			}
			for _, rk := range relationKinds {
				if kind != "" && kind != "all" && kind != rk.Key {
					continue
				}
				items := rk.Pick(resp.ProductAssociations)
				result.Counts[rk.Key] = len(items)
				for _, p := range items {
					result.Products = append(result.Products, RelatedProduct{
						Relation:    rk.Label,
						SummaryView: newSummaryView(p),
					})
				}
			}

			t := &output.Table{
				Headers: []string{"RELATION", "DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT"},
				Empty:   "DigiKey lists no related products for this part.",
			}
			for _, p := range result.Products {
				t.AddRow(
					p.Relation,
					p.DigiKeyPartNumber,
					p.ManufacturerPartNumber,
					output.Truncate(p.Manufacturer, 20),
					output.Truncate(p.Description, 40),
					p.QuantityAvailable,
					p.UnitPrice,
				)
			}
			return app.Printer.Print(result, t)
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "all", "limit to one relation: "+strings.Join(relationKindNames(), ", "))
	_ = cmd.RegisterFlagCompletionFunc("kind", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return relationKindNames(), cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func validRelationKind(kind string) bool {
	for _, rk := range relationKinds {
		if rk.Key == kind {
			return true
		}
	}
	return false
}
