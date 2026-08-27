package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// PricingLine is one orderable product inside a pricing option.
//
// Every figure here describes this product and no other. An option can name
// several — DigiKey fills a quantity past a standard reel with the reel plus a
// cut-tape remainder — so a part number and a price at the option level would
// be a figure from one variation printed beside an identifier from another.
type PricingLine struct {
	DigiKeyPartNumber    string  `json:"digikey_part_number"`
	Packaging            string  `json:"packaging,omitempty"`
	Quantity             int     `json:"quantity"`
	UnitPrice            float64 `json:"unit_price"`
	ExtendedPrice        float64 `json:"extended_price"`
	MinimumOrderQuantity int     `json:"minimum_order_quantity"`
	// QuantityAvailable and ProductStatus do not come from the pricing
	// endpoint, which returns no stock at all. They are joined from
	// ProductDetails on the DigiKey part number.
	QuantityAvailable int    `json:"quantity_available"`
	InStock           bool   `json:"in_stock"`
	ProductStatus     string `json:"product_status,omitempty"`
	MarketPlace       bool   `json:"marketplace,omitempty"`
	TariffActive      bool   `json:"tariff_active,omitempty"`
}

// PricingOption is one way to buy the requested quantity.
type PricingOption struct {
	// Option is DigiKey's own classification of this way of buying:
	// "Exact", "MinimumOrderQuantity", "BetterValue", or "MaxOrderQuantity".
	Option string `json:"option"`
	// OrderQuantity is what you would actually receive, summed across the
	// products below. It exceeds the request when a minimum order quantity or
	// a standard pack forces it up, and when buying more costs less.
	OrderQuantity int `json:"order_quantity"`
	// ForcedUp is true when OrderQuantity exceeds what was asked for.
	ForcedUp   bool    `json:"forced_up"`
	TotalPrice float64 `json:"total_price"`
	// InStock is true only when every product in the option has enough stock
	// to fill its own line. An option is not orderable on the strength of one
	// half of it being available.
	InStock  bool          `json:"in_stock"`
	Products []PricingLine `json:"products"`
}

// PricingResult is the JSON shape of `dk pricing`.
type PricingResult struct {
	PartNumber        string `json:"part_number"`
	RequestedQuantity int    `json:"requested_quantity"`
	Currency          string `json:"currency,omitempty"`
	// Best is the cheapest option that is actually orderable, which is what
	// most callers want. It is nil when nothing is in stock.
	//
	// Deliberately not omitempty: the guide documents this key as
	// `{...} | null`, and a caller testing `.best === null` must not instead
	// find the key missing entirely.
	Best    *PricingOption  `json:"best"`
	Options []PricingOption `json:"options"`
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

DigiKey returns every way it will sell the requested quantity, and labels each
one: EXACT buys what was asked for, MINIMUMORDERQUANTITY is the quantity a
minimum forces you up to, and BETTERVALUE costs less than the exact option
while buying more — a 5000-piece reel that is cheaper than 4500 on cut tape.
A "*" marks any option that hands you more than you asked for.

An option can name more than one part number: a quantity past a standard reel
is filled with the reel plus a cut-tape remainder, priced as one option. Each
line is listed with its own quantity and price.

Stock and product status are joined from the product record, because the
pricing endpoint returns no stock of its own. Prices exclude shipping and tax.
If you have run "dk auth login", your account-specific pricing is used when
DigiKey returns any.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			partNumber := strings.TrimSpace(args[0])
			if partNumber == "" {
				return usageErrorf("part number must not be empty")
			}
			if quantity < 1 {
				return usageErrorf("--qty must be at least 1")
			}
			// CT and DKR are the two packagings DigiKey names. This is a filter
			// over what comes back rather than a request parameter: the pricing
			// endpoint has no packaging preference and answers with every
			// option, which is more than the old one could say.
			preference = strings.ToUpper(strings.TrimSpace(preference))
			switch preference {
			case "", "CT", "DKR":
			default:
				return usageErrorf("--packaging must be CT (cut tape) or DKR (Digi-Reel), got %q", preference)
			}

			client, err := app.Client()
			if err != nil {
				return err
			}
			resp, err := client.PricingByQuantity(cmd.Context(), partNumber, quantity, 0)
			if err != nil {
				return err
			}

			options := resp.Options()
			if preference != "" {
				options = filterByPackaging(options, preference)
			}

			// Stock is not on this response at any quantity, so it is joined
			// from the product record. Look it up by a DigiKey part number the
			// pricing answer itself returned rather than by what the caller
			// typed: every product in every option is a variation of the same
			// product, so one call covers them all, and a DigiKey number cannot
			// be the ambiguous manufacturer number that a caller's input can.
			stock := map[string]digikey.ProductVariation{}
			var status string
			if lookup := firstPartNumber(options); lookup != "" {
				details, err := client.ProductDetails(cmd.Context(), lookup)
				if err != nil {
					return err
				}
				status = details.Product.ProductStatus.Status
				for _, v := range details.Product.ProductVariations {
					stock[strings.ToUpper(v.DigiKeyProductNumber)] = v
				}
			}

			result := PricingResult{
				PartNumber:        partNumber,
				RequestedQuantity: quantity,
				Currency:          app.Cfg.Locale.Currency,
				Options:           []PricingOption{},
			}
			for _, o := range options {
				result.Options = append(result.Options, buildPricingOption(o, quantity, stock, status))
			}
			result.Best = cheapestInStock(result.Options)

			t := &output.Table{
				Headers: []string{"OPTION", "ORDER QTY", "TOTAL", "DKPN", "PACKAGING", "QTY", "UNIT", "STOCK", "STATUS"},
				Empty:   "DigiKey returned no pricing options for this part and quantity.",
			}
			for _, o := range result.Options {
				for i, p := range o.Products {
					// The option's own figures print once. Repeating a total on
					// every line of a two-part option invites reading it twice.
					option, orderQty, total := "", any(""), ""
					if i == 0 {
						option, orderQty, total = o.Option, o.OrderQuantity, output.Money(o.TotalPrice)
						if o.ForcedUp {
							option += " *"
						}
					}
					t.AddRow(option, orderQty, total,
						p.DigiKeyPartNumber, output.Truncate(p.Packaging, 18), p.Quantity,
						output.Money(p.UnitPrice), p.QuantityAvailable, p.ProductStatus)
				}
			}
			if err := app.Printer.Print(result, t); err != nil {
				return err
			}

			// The table marks a forced-up option with a "*", which needs
			// saying once. PrintText is a no-op in JSON and CSV.
			for _, o := range result.Options {
				if o.ForcedUp {
					app.Printer.PrintText("\n* hands you more than the %d requested.", quantity)
					break
				}
			}

			if result.Best != nil {
				app.Printer.PrintText("Cheapest in stock: %s, order %d for %s %s total.",
					describeOption(*result.Best), result.Best.OrderQuantity,
					output.Money(result.Best.TotalPrice), result.Currency)
				if result.Best.ForcedUp {
					app.Printer.PrintText("Note: this hands you %d units, not the %d requested.",
						result.Best.OrderQuantity, quantity)
				}
			} else if len(result.Options) > 0 {
				app.Printer.PrintText("\nNone of these pricing options are in stock.")
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVarP(&quantity, "qty", "n", 1, "quantity you want to end up with")
	f.StringVar(&preference, "packaging", "", "show only options in this packaging: CT (cut tape) or DKR (Digi-Reel)")

	return cmd
}

// buildPricingOption flattens one DigiKey option, joining stock and status onto
// each line by part number.
func buildPricingOption(o digikey.PricingOption, requested int, stock map[string]digikey.ProductVariation, status string) PricingOption {
	opt := PricingOption{
		Option:        o.PricingOption,
		OrderQuantity: o.TotalQuantityPriced,
		ForcedUp:      o.TotalQuantityPriced > requested,
		TotalPrice:    o.TotalPrice,
		InStock:       len(o.Products) > 0,
		Products:      []PricingLine{},
	}

	for _, p := range o.Products {
		line := PricingLine{
			DigiKeyPartNumber:    p.DigiKeyProductNumber,
			Packaging:            p.PackageType.Name,
			Quantity:             p.QuantityPriced,
			UnitPrice:            p.UnitPrice,
			ExtendedPrice:        p.ExtendedPrice,
			MinimumOrderQuantity: p.MinimumOrderQuantity,
			ProductStatus:        status,
			MarketPlace:          p.MarketPlace,
			TariffActive:         p.TariffInformation.TariffActive,
		}
		if v, ok := stock[strings.ToUpper(p.DigiKeyProductNumber)]; ok {
			line.QuantityAvailable = v.Stock()
		}
		// Enough stock to fill this line, not merely some stock: an option that
		// prices 5000 against 300 available is not orderable as quoted.
		line.InStock = line.QuantityAvailable >= line.Quantity && line.Quantity > 0
		if !line.InStock {
			opt.InStock = false
		}
		opt.Products = append(opt.Products, line)
	}
	return opt
}

// firstPartNumber returns a DigiKey part number from the pricing options, for
// the product lookup that supplies stock. Any of them resolves to the same
// product record, so the first one will do.
func firstPartNumber(options []digikey.PricingOption) string {
	for _, o := range options {
		for _, p := range o.Products {
			if p.DigiKeyProductNumber != "" {
				return p.DigiKeyProductNumber
			}
		}
	}
	return ""
}

// packagingNames maps the codes DigiKey documents for a packaging preference
// onto the package-type names it returns. The two vocabularies do not match:
// the codes are CT and DKR, the names are "Cut Tape (CT)" and "Digi-Reel®".
var packagingNames = map[string]string{
	"CT":  "cut tape",
	"DKR": "digi-reel",
}

// filterByPackaging keeps options built entirely from one packaging. An option
// that mixes a reel with a cut-tape remainder is not a CT option, and returning
// it under --packaging CT would quote a reel the caller filtered out.
func filterByPackaging(options []digikey.PricingOption, code string) []digikey.PricingOption {
	want, ok := packagingNames[code]
	if !ok {
		return options
	}
	kept := []digikey.PricingOption{}
	for _, o := range options {
		all := len(o.Products) > 0
		for _, p := range o.Products {
			if !strings.Contains(strings.ToLower(p.PackageType.Name), want) {
				all = false
				break
			}
		}
		if all {
			kept = append(kept, o)
		}
	}
	return kept
}

// describeOption names an option for the confirmation line: its part number
// when it has just one, and the split when it has more.
func describeOption(o PricingOption) string {
	switch len(o.Products) {
	case 0:
		return o.Option
	case 1:
		return o.Products[0].DigiKeyPartNumber + " (" + o.Products[0].Packaging + ")"
	default:
		parts := make([]string, 0, len(o.Products))
		for _, p := range o.Products {
			parts = append(parts, p.DigiKeyPartNumber)
		}
		return strings.Join(parts, " + ")
	}
}

// cheapestInStock returns the lowest-total-price option that is actually
// orderable, or nil if none are.
func cheapestInStock(options []PricingOption) *PricingOption {
	best := -1
	for i, o := range options {
		if !o.InStock || o.TotalPrice <= 0 {
			continue
		}
		if best < 0 || o.TotalPrice < options[best].TotalPrice {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	cp := options[best]
	return &cp
}
