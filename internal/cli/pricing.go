package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// PackagingOption is one way to buy a part at a requested quantity.
type PackagingOption struct {
	DigiKeyPartNumber string `json:"digikey_part_number"`
	Packaging         string `json:"packaging,omitempty"`
	// OrderQuantity is what you would actually have to buy. It exceeds the
	// requested quantity when a minimum order quantity or standard pack size
	// forces it up.
	OrderQuantity int `json:"order_quantity"`
	// ForcedUp is true when OrderQuantity had to exceed what was asked for.
	ForcedUp             bool    `json:"forced_up"`
	MinimumOrderQuantity int     `json:"minimum_order_quantity"`
	StandardPackage      int     `json:"standard_package,omitempty"`
	UnitPrice            float64 `json:"unit_price"`
	ExtendedPrice        float64 `json:"extended_price"`
	QuantityAvailable    int     `json:"quantity_available"`
	InStock              bool    `json:"in_stock"`
	MarketPlace          bool    `json:"marketplace,omitempty"`
	ProductStatus        string  `json:"product_status,omitempty"`
	LeadWeeks            string  `json:"lead_weeks,omitempty"`
	StockNote            string  `json:"stock_note,omitempty"`
}

// PricingResult is the JSON shape of `dk pricing`.
type PricingResult struct {
	PartNumber        string `json:"part_number"`
	RequestedQuantity int    `json:"requested_quantity"`
	Currency          string `json:"currency,omitempty"`
	// Best is the cheapest in-stock option, which is what most callers want.
	// It is nil when nothing is in stock.
	//
	// Deliberately not omitempty: the guide documents this key as
	// `{...} | null`, and a caller testing `.best === null` must not instead
	// find the key missing entirely.
	Best    *PackagingOption  `json:"best"`
	Options []PackagingOption `json:"options"`
}

func newPricingCommand(app *App) *cobra.Command {
	var (
		quantity   int
		preference string
	)

	cmd := &cobra.Command{
		Use:     "pricing <part-number>",
		Aliases: []string{"cost", "quote"},
		Short:   "Cost a part at a specific quantity across packaging options",
		Long: `Answer "I need N of these — what do I actually order, and what does it cost?"

  dk pricing 311-10.0KHRCT-ND --qty 250

Each packaging option (cut tape, tape & reel, digi-reel) is priced for the
requested quantity, including the quantity you would actually have to buy. That
last part is the point: a minimum order quantity or standard pack size can force
you well past what you asked for, which is how people end up with a 4000-piece
reel when they wanted 250. The FORCED-UP column flags exactly that.

Prices exclude shipping and tax. If you have run "dk auth login", your
account-specific pricing is used instead of list pricing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			partNumber := strings.TrimSpace(args[0])
			if partNumber == "" {
				return usageErrorf("part number must not be empty")
			}
			if quantity < 1 {
				return usageErrorf("--qty must be at least 1")
			}

			client, err := app.Client()
			if err != nil {
				return err
			}
			resp, err := client.PackageTypeByQuantity(cmd.Context(), partNumber, quantity, preference)
			if err != nil {
				return err
			}

			result := PricingResult{
				PartNumber:        partNumber,
				RequestedQuantity: quantity,
				Currency:          app.Cfg.Locale.Currency,
				Options:           []PackagingOption{},
			}

			for _, p := range resp.Products {
				opt := buildPackagingOption(p, quantity)
				result.Options = append(result.Options, opt)
			}
			result.Best = cheapestInStock(result.Options)

			t := &output.Table{
				Headers: []string{"DKPN", "PACKAGING", "ORDER QTY", "FORCED-UP", "UNIT", "EXTENDED", "STOCK", "STATUS"},
				Empty:   "DigiKey returned no packaging options for this part and quantity.",
			}
			for _, o := range result.Options {
				t.AddRow(
					o.DigiKeyPartNumber,
					output.Truncate(o.Packaging, 18),
					o.OrderQuantity,
					o.ForcedUp,
					output.Money(o.UnitPrice),
					output.Money(o.ExtendedPrice),
					o.QuantityAvailable,
					o.ProductStatus,
				)
			}
			if err := app.Printer.Print(result, t); err != nil {
				return err
			}

			if result.Best != nil {
				app.Printer.PrintText("\nCheapest in stock: %s (%s), order %d for %s %s total.",
					result.Best.DigiKeyPartNumber, result.Best.Packaging, result.Best.OrderQuantity,
					output.Money(result.Best.ExtendedPrice), result.Currency)
				if result.Best.ForcedUp {
					app.Printer.PrintText("Note: the minimum order forces %d units, not the %d requested.",
						result.Best.OrderQuantity, quantity)
				}
			} else if len(result.Options) > 0 {
				app.Printer.PrintText("\nNone of these packaging options are in stock.")
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVarP(&quantity, "qty", "n", 1, "quantity you want to end up with")
	f.StringVar(&preference, "packaging", "", "packaging preference passed to DigiKey, e.g. CutTapeOrTR, DigiReelOrTR, StandardPack")

	return cmd
}

func buildPackagingOption(p digikey.PackageTypeByQuantityProduct, requested int) PackagingOption {
	orderQty := p.RecommendedQuantity
	if orderQty == 0 {
		// DigiKey omits RecommendedQuantity on some options; fall back to the
		// larger of what was asked for and the minimum.
		orderQty = max(requested, p.MinimumOrderQuantity)
	}

	opt := PackagingOption{
		DigiKeyPartNumber:    p.DigiKeyProductNumber,
		Packaging:            p.PackageType.Name,
		OrderQuantity:        orderQty,
		ForcedUp:             orderQty > requested,
		MinimumOrderQuantity: p.MinimumOrderQuantity,
		StandardPackage:      p.StandardPackage,
		QuantityAvailable:    p.QuantityAvailable,
		InStock:              p.QuantityAvailable > 0,
		MarketPlace:          p.MarketPlace,
		ProductStatus:        p.ProductStatus,
		LeadWeeks:            p.ManufacturerLeadWeeks,
		StockNote:            p.StockNote,
	}
	opt.UnitPrice = unitPriceAtQuantity(p.Pricing(), orderQty)
	opt.ExtendedPrice = opt.UnitPrice * float64(orderQty)
	return opt
}

// unitPriceAtQuantity finds the price break that applies at a given quantity:
// the highest break whose threshold the quantity reaches.
func unitPriceAtQuantity(breaks []digikey.PriceBreak, quantity int) float64 {
	best := -1
	for i, b := range breaks {
		if b.BreakQuantity > quantity {
			continue
		}
		if best < 0 || b.BreakQuantity > breaks[best].BreakQuantity {
			best = i
		}
	}
	if best < 0 {
		// The quantity is below every break; the lowest break is the closest
		// thing to an applicable price.
		lowest := -1
		for i, b := range breaks {
			if lowest < 0 || b.BreakQuantity < breaks[lowest].BreakQuantity {
				lowest = i
			}
		}
		if lowest < 0 {
			return 0
		}
		return breaks[lowest].UnitPrice
	}
	return breaks[best].UnitPrice
}

// cheapestInStock returns the lowest-extended-price option that is actually
// available, or nil if none are.
func cheapestInStock(options []PackagingOption) *PackagingOption {
	best := -1
	for i, o := range options {
		if !o.InStock || o.ExtendedPrice <= 0 {
			continue
		}
		if best < 0 || o.ExtendedPrice < options[best].ExtendedPrice {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	cp := options[best]
	return &cp
}
