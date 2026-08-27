package cli

import (
	"fmt"
	"strings"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// ProductView is dk's normalized product shape.
//
// DigiKey's raw Product is deep and inconsistent (packaging-specific fields
// live one level down in ProductVariations, prices appear in two places), which
// makes it awkward for both humans and programs. ProductView flattens the
// fields that actually matter when choosing a part, and stays stable even if
// DigiKey reshuffles its payload. Use --raw when the untouched payload is
// needed.
type ProductView struct {
	DigiKeyPartNumber      string  `json:"digikey_part_number"`
	ManufacturerPartNumber string  `json:"manufacturer_part_number"`
	Manufacturer           string  `json:"manufacturer"`
	Description            string  `json:"description"`
	DetailedDescription    string  `json:"detailed_description,omitempty"`
	UnitPrice              float64 `json:"unit_price"`
	Currency               string  `json:"currency,omitempty"`
	QuantityAvailable      int     `json:"quantity_available"`
	MinimumOrderQuantity   int     `json:"minimum_order_quantity"`
	Packaging              string  `json:"packaging,omitempty"`
	Status                 string  `json:"status,omitempty"`
	Category               string  `json:"category,omitempty"`
	Series                 string  `json:"series,omitempty"`
	// Orderable is false for parts that cannot be bought as-is: no stock and no
	// backorder, discontinued, or end of life.
	Orderable    bool   `json:"orderable"`
	Discontinued bool   `json:"discontinued,omitempty"`
	EndOfLife    bool   `json:"end_of_life,omitempty"`
	NCNR         bool   `json:"ncnr,omitempty"`
	LeadWeeks    string `json:"lead_weeks,omitempty"`
	RohsStatus   string `json:"rohs_status,omitempty"`
	DatasheetURL string `json:"datasheet_url,omitempty"`
	ProductURL   string `json:"product_url,omitempty"`

	// PriceBreaks and Parameters are only populated by detail views.
	PriceBreaks []PriceBreakView `json:"price_breaks,omitempty"`
	Parameters  []ParameterView  `json:"parameters,omitempty"`
	Variations  []VariationView  `json:"variations,omitempty"`
}

// SummaryView is the flattened form of DigiKey's compact ProductSummary, which
// it returns from the association and alternate-packaging endpoints.
//
// It exists for the same reason ProductView does: DigiKey's shape is PascalCase
// with the manufacturer nested one level down, and emitting it verbatim would
// make one dk command's output look nothing like the next. Decoding into a view
// also drops the phantom fields a partial payload would otherwise materialize
// as empty strings. --raw is the escape hatch for the untouched payload.
//
// UnitPrice is a STRING here because DigiKey preformats it on these endpoints.
// That is not unified with the numeric unit_price elsewhere: normalizing would
// mean parsing currency-formatted text, and guessing wrong about money is worse
// than making the caller notice the difference.
type SummaryView struct {
	DigiKeyPartNumber      string `json:"digikey_part_number"`
	ManufacturerPartNumber string `json:"manufacturer_part_number,omitempty"`
	Manufacturer           string `json:"manufacturer,omitempty"`
	Description            string `json:"description,omitempty"`
	UnitPrice              string `json:"unit_price,omitempty"`
	QuantityAvailable      int    `json:"quantity_available"`
	ProductURL             string `json:"product_url,omitempty"`
}

func newSummaryView(p digikey.ProductSummary) SummaryView {
	return SummaryView{
		DigiKeyPartNumber:      p.DigiKeyProductNumber,
		ManufacturerPartNumber: p.ManufacturerProductNumber,
		Manufacturer:           p.Manufacturer.Name,
		Description:            p.Description,
		UnitPrice:              p.UnitPrice,
		QuantityAvailable:      p.QuantityAvailable,
		ProductURL:             p.ProductURL,
	}
}

func summaryViews(items []digikey.ProductSummary) []SummaryView {
	out := make([]SummaryView, 0, len(items))
	for _, p := range items {
		out = append(out, newSummaryView(p))
	}
	return out
}

// SubstituteView is one substitute part: a SummaryView plus why DigiKey
// considers it a substitute.
type SubstituteView struct {
	SubstituteType string `json:"substitute_type,omitempty"`
	SummaryView
}

// RecommendedView is one "customers also bought" suggestion.
//
// Its unit_price is a NUMBER, unlike SummaryView's string — DigiKey returns it
// that way on the recommendations endpoint. Same reasoning as above: the
// inconsistency is DigiKey's and is reported rather than papered over.
type RecommendedView struct {
	DigiKeyPartNumber      string  `json:"digikey_part_number"`
	ManufacturerPartNumber string  `json:"manufacturer_part_number,omitempty"`
	Manufacturer           string  `json:"manufacturer,omitempty"`
	Description            string  `json:"description,omitempty"`
	UnitPrice              float64 `json:"unit_price"`
	QuantityAvailable      int     `json:"quantity_available"`
	ProductURL             string  `json:"product_url,omitempty"`
}

// PriceBreakView is one quantity-price tier.
type PriceBreakView struct {
	Quantity   int     `json:"quantity"`
	UnitPrice  float64 `json:"unit_price"`
	TotalPrice float64 `json:"total_price"`
}

// ParameterView is one parametric attribute.
type ParameterView struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// VariationView is one orderable packaging option. The DigiKey part number
// differs per packaging (cut tape vs. reel), and it is the packaging-specific
// number that must be used when adding to a list.
type VariationView struct {
	DigiKeyPartNumber    string           `json:"digikey_part_number"`
	Packaging            string           `json:"packaging"`
	QuantityAvailable    int              `json:"quantity_available"`
	MinimumOrderQuantity int              `json:"minimum_order_quantity"`
	UnitPrice            float64          `json:"unit_price"`
	MarketPlace          bool             `json:"marketplace,omitempty"`
	PriceBreaks          []PriceBreakView `json:"price_breaks,omitempty"`
}

// newProductView flattens a DigiKey product. currency is echoed from the
// response locale so a JSON consumer knows what the prices are denominated in.
func newProductView(p digikey.Product, currency string) ProductView {
	v := ProductView{
		ManufacturerPartNumber: p.ManufacturerProductNumber,
		Manufacturer:           p.Manufacturer.Name,
		Description:            p.Description.ProductDescription,
		DetailedDescription:    p.Description.DetailedDescription,
		UnitPrice:              p.UnitPrice,
		Currency:               currency,
		QuantityAvailable:      p.QuantityAvailable,
		Status:                 p.ProductStatus.Status,
		Category:               p.Category.Name,
		Series:                 p.Series.Name,
		Discontinued:           p.Discontinued,
		EndOfLife:              p.EndOfLife,
		NCNR:                   p.Ncnr,
		LeadWeeks:              p.ManufacturerLeadWeeks,
		RohsStatus:             p.Classifications.RohsStatus,
		DatasheetURL:           p.DatasheetURL,
		ProductURL:             p.ProductURL,
	}

	if pv, ok := p.PrimaryVariation(); ok {
		v.DigiKeyPartNumber = pv.DigiKeyProductNumber
		v.Packaging = pv.PackageType.Name
		v.MinimumOrderQuantity = pv.MinimumOrderQuantity
		if v.UnitPrice == 0 {
			v.UnitPrice = pv.LowestUnitPrice()
		}
	}
	if v.MinimumOrderQuantity == 0 {
		v.MinimumOrderQuantity = p.MinimumOrderQuantity
	}

	// A part is orderable if stock exists, or if DigiKey will accept a backorder
	// and the part has not been retired.
	inStock := p.QuantityAvailable > 0
	v.Orderable = !p.Discontinued && !p.EndOfLife && (inStock || !p.BackOrderNotAllowed)
	return v
}

// withDetails adds the fields that only make sense on a single-product view.
func withDetails(v ProductView, p digikey.Product) ProductView {
	for _, param := range p.Parameters {
		v.Parameters = append(v.Parameters, ParameterView{Name: param.ParameterText, Value: param.ValueText})
	}
	if pv, ok := p.PrimaryVariation(); ok {
		v.PriceBreaks = priceBreakViews(pv)
	}
	for _, variation := range p.ProductVariations {
		v.Variations = append(v.Variations, VariationView{
			DigiKeyPartNumber:    variation.DigiKeyProductNumber,
			Packaging:            variation.PackageType.Name,
			QuantityAvailable:    variation.Stock(),
			MinimumOrderQuantity: variation.MinimumOrderQuantity,
			UnitPrice:            variation.LowestUnitPrice(),
			MarketPlace:          variation.MarketPlace,
			PriceBreaks:          priceBreakViews(variation),
		})
	}
	return v
}

func priceBreakViews(v digikey.ProductVariation) []PriceBreakView {
	breaks := v.Pricing()
	out := make([]PriceBreakView, 0, len(breaks))
	for _, b := range breaks {
		out = append(out, PriceBreakView{Quantity: b.BreakQuantity, UnitPrice: b.UnitPrice, TotalPrice: b.TotalPrice})
	}
	return out
}

// productTable renders product views as a table. descWidth caps the description
// column; pass 0 to leave it untruncated.
func productTable(views []ProductView, descWidth int) *output.Table {
	t := &output.Table{
		Headers: []string{"DKPN", "MPN", "MFR", "DESCRIPTION", "STOCK", "UNIT", "MOQ", "PKG", "STATUS"},
		Empty:   "No products matched.",
	}
	for _, v := range views {
		t.AddRow(
			v.DigiKeyPartNumber,
			v.ManufacturerPartNumber,
			output.Truncate(v.Manufacturer, 22),
			output.Truncate(v.Description, descWidth),
			v.QuantityAvailable,
			output.Money(v.UnitPrice),
			v.MinimumOrderQuantity,
			output.Truncate(v.Packaging, 16),
			statusLabel(v),
		)
	}
	return t
}

// statusLabel condenses the lifecycle flags into one column.
func statusLabel(v ProductView) string {
	var flags []string
	if v.Status != "" {
		flags = append(flags, v.Status)
	}
	if v.Discontinued {
		flags = append(flags, "DISCONTINUED")
	}
	if v.EndOfLife {
		flags = append(flags, "EOL")
	}
	if v.NCNR {
		flags = append(flags, "NCNR")
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, "/")
}

// detailPairs renders a single product as ordered key/value rows.
func detailPairs(v ProductView) [][2]string {
	pairs := [][2]string{
		{"DigiKey Part Number", v.DigiKeyPartNumber},
		{"Manufacturer Part Number", v.ManufacturerPartNumber},
		{"Manufacturer", v.Manufacturer},
		{"Description", v.Description},
		{"Detailed Description", v.DetailedDescription},
		{"Category", v.Category},
		{"Series", v.Series},
		{"Status", statusLabel(v)},
		{"Orderable", fmt.Sprintf("%t", v.Orderable)},
		{"Quantity Available", fmt.Sprintf("%d", v.QuantityAvailable)},
		{"Minimum Order Quantity", fmt.Sprintf("%d", v.MinimumOrderQuantity)},
		{"Packaging", v.Packaging},
		{"Unit Price", priceWithCurrency(v.UnitPrice, v.Currency)},
		{"Manufacturer Lead Weeks", v.LeadWeeks},
		{"RoHS", v.RohsStatus},
		{"Datasheet", v.DatasheetURL},
		{"Product Page", v.ProductURL},
	}

	// Price breaks belong in this block rather than a second table: a product
	// detail is one result, and a second table on the same stdout would be a
	// second CSV document. KeyValueTable drops rows with an empty value, so
	// breaks DigiKey did not quote fall out on their own.
	for _, b := range v.PriceBreaks {
		pairs = append(pairs, [2]string{
			fmt.Sprintf("Price @ %d", b.Quantity),
			priceWithCurrency(b.UnitPrice, v.Currency),
		})
	}
	return pairs
}

func priceWithCurrency(price float64, currency string) string {
	if price == 0 {
		return ""
	}
	if currency == "" {
		return fmt.Sprintf("%.4f", price)
	}
	return fmt.Sprintf("%.4f %s", price, currency)
}
