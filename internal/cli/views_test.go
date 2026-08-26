package cli

import (
	"strings"
	"testing"

	"github.com/jacobcase/dk/internal/digikey"
)

func TestNewProductViewFlattensVariation(t *testing.T) {
	p := digikey.Product{
		ManufacturerProductNumber: "GRM188R71C104KA01D",
		Manufacturer:              digikey.NamedID{ID: 2359, Name: "Murata"},
		Description:               digikey.Description{ProductDescription: "CAP CER 0.1UF"},
		QuantityAvailable:         100,
		ProductStatus:             digikey.ProductStatus{Status: "Active"},
		ProductVariations: []digikey.ProductVariation{{
			DigiKeyProductNumber:            "490-1532-1-ND",
			PackageType:                     digikey.NamedID{Name: "Cut Tape (CT)"},
			QuantityAvailableForPackageType: 100,
			MinimumOrderQuantity:            1,
			StandardPricing:                 []digikey.PriceBreak{{BreakQuantity: 1, UnitPrice: 0.10}},
		}},
	}

	v := newProductView(p, "USD")
	// The orderable part number and MOQ live on the variation, not the product,
	// which is exactly the flattening the view exists to do.
	if v.DigiKeyPartNumber != "490-1532-1-ND" {
		t.Errorf("digikey_part_number = %q", v.DigiKeyPartNumber)
	}
	if v.Packaging != "Cut Tape (CT)" {
		t.Errorf("packaging = %q", v.Packaging)
	}
	if v.MinimumOrderQuantity != 1 {
		t.Errorf("minimum_order_quantity = %d, want 1", v.MinimumOrderQuantity)
	}
	if v.Currency != "USD" {
		t.Errorf("currency = %q", v.Currency)
	}
}

func TestNewProductViewFallsBackToVariationPrice(t *testing.T) {
	// Keyword search sometimes omits the product-level UnitPrice; the lowest
	// break on the chosen variation has to fill in.
	p := digikey.Product{
		QuantityAvailable: 10,
		ProductVariations: []digikey.ProductVariation{{
			DigiKeyProductNumber:            "X-ND",
			QuantityAvailableForPackageType: 10,
			StandardPricing: []digikey.PriceBreak{
				{BreakQuantity: 100, UnitPrice: 0.02},
				{BreakQuantity: 1, UnitPrice: 0.10},
			},
		}},
	}
	v := newProductView(p, "USD")
	if v.UnitPrice != 0.10 {
		t.Errorf("unit_price = %v, want the quantity-1 break price 0.10", v.UnitPrice)
	}
}

func TestProductViewOrderable(t *testing.T) {
	tests := []struct {
		name string
		p    digikey.Product
		want bool
	}{
		{"in stock", digikey.Product{QuantityAvailable: 10}, true},
		{
			"out of stock but backorderable",
			digikey.Product{QuantityAvailable: 0, BackOrderNotAllowed: false},
			true,
		},
		{
			"out of stock and no backorder",
			digikey.Product{QuantityAvailable: 0, BackOrderNotAllowed: true},
			false,
		},
		{"discontinued", digikey.Product{QuantityAvailable: 10, Discontinued: true}, false},
		{"end of life", digikey.Product{QuantityAvailable: 10, EndOfLife: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newProductView(tt.p, "USD").Orderable; got != tt.want {
				t.Errorf("orderable = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStatusLabel(t *testing.T) {
	tests := []struct {
		name string
		v    ProductView
		want string
	}{
		{"active", ProductView{Status: "Active"}, "Active"},
		{"nothing known", ProductView{}, "-"},
		{"flags appended", ProductView{Status: "Active", NCNR: true}, "Active/NCNR"},
		{
			"every flag",
			ProductView{Status: "Obsolete", Discontinued: true, EndOfLife: true, NCNR: true},
			"Obsolete/DISCONTINUED/EOL/NCNR",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusLabel(tt.v); got != tt.want {
				t.Errorf("statusLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithDetailsPopulatesEverySection(t *testing.T) {
	p := digikey.Product{
		Parameters: []digikey.ParameterValue{
			{ParameterText: "Capacitance", ValueText: "0.1 µF"},
			{ParameterText: "Tolerance", ValueText: "±10%"},
		},
		ProductVariations: []digikey.ProductVariation{
			{
				DigiKeyProductNumber:            "490-1532-1-ND",
				PackageType:                     digikey.NamedID{Name: "Cut Tape"},
				QuantityAvailableForPackageType: 10,
				StandardPricing:                 []digikey.PriceBreak{{BreakQuantity: 1, UnitPrice: 0.10, TotalPrice: 0.10}},
			},
			{
				DigiKeyProductNumber: "490-1532-2-ND",
				PackageType:          digikey.NamedID{Name: "Tape & Reel"},
				StandardPricing:      []digikey.PriceBreak{{BreakQuantity: 4000, UnitPrice: 0.01}},
			},
		},
	}

	v := withDetails(newProductView(p, "USD"), p)
	if len(v.Parameters) != 2 {
		t.Errorf("parameters = %+v, want two", v.Parameters)
	}
	if len(v.Variations) != 2 {
		t.Errorf("variations = %+v, want two", v.Variations)
	}
	if len(v.PriceBreaks) != 1 {
		t.Errorf("price_breaks = %+v, want the chosen variation's tiers", v.PriceBreaks)
	}
}

func TestPriceBreaksPrefersMyPricing(t *testing.T) {
	// A 3-legged token yields account pricing; it must be what the view reports.
	v := digikey.ProductVariation{
		StandardPricing: []digikey.PriceBreak{{BreakQuantity: 1, UnitPrice: 1.00}},
		MyPricing:       []digikey.PriceBreak{{BreakQuantity: 1, UnitPrice: 0.25}},
	}
	breaks := priceBreakViews(v)
	if len(breaks) != 1 || breaks[0].UnitPrice != 0.25 {
		t.Errorf("priceBreakViews() = %+v, want the MyPricing tier", breaks)
	}
	if got := lowestUnitPrice(v); got != 0.25 {
		t.Errorf("lowestUnitPrice() = %v, want 0.25", got)
	}
}

func TestLowestUnitPriceWithNoPricing(t *testing.T) {
	if got := lowestUnitPrice(digikey.ProductVariation{}); got != 0 {
		t.Errorf("lowestUnitPrice() = %v, want 0 for a variation with no pricing", got)
	}
}

func TestProductTableTruncatesDescription(t *testing.T) {
	views := []ProductView{{
		DigiKeyPartNumber: "X-ND",
		Description:       strings.Repeat("very long description ", 10),
	}}
	table := productTable(views, 20)
	if len(table.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(table.Rows))
	}
	if got := []rune(table.Rows[0][3]); len(got) > 20 {
		t.Errorf("description column is %d runes, want at most 20", len(got))
	}
}

func TestProductTableHasEmptyMessage(t *testing.T) {
	table := productTable(nil, 40)
	if table.Empty == "" {
		t.Error("an empty search result should render an explanatory message, not a bare header")
	}
}

func TestDetailPairsOmitBlanks(t *testing.T) {
	v := ProductView{DigiKeyPartNumber: "X-ND", Manufacturer: "Acme"}
	pairs := detailPairs(v)

	found := map[string]bool{}
	for _, kv := range pairs {
		found[kv[0]] = true
		if kv[0] == "Datasheet" && kv[1] != "" {
			t.Errorf("Datasheet = %q, want empty for a product with no datasheet", kv[1])
		}
	}
	for _, want := range []string{"DigiKey Part Number", "Manufacturer", "Orderable"} {
		if !found[want] {
			t.Errorf("detailPairs() is missing the %q row", want)
		}
	}
}

func TestPriceWithCurrency(t *testing.T) {
	tests := []struct {
		price    float64
		currency string
		want     string
	}{
		{0, "USD", ""},
		{0.048, "USD", "0.0480 USD"},
		{1.5, "", "1.5000"},
	}
	for _, tt := range tests {
		if got := priceWithCurrency(tt.price, tt.currency); got != tt.want {
			t.Errorf("priceWithCurrency(%v, %q) = %q, want %q", tt.price, tt.currency, got, tt.want)
		}
	}
}

func TestFlattenCategories(t *testing.T) {
	tree := []digikey.CategoryNode{
		{
			CategoryID: 1, Name: "Capacitors",
			ChildNodes: []digikey.CategoryNode{
				{CategoryID: 11, Name: "Ceramic"},
				{
					CategoryID: 12, Name: "Electrolytic",
					ChildCategories: []digikey.CategoryNode{{CategoryID: 121, Name: "Aluminum"}},
				},
			},
		},
		{CategoryID: 2, Name: "Resistors"},
	}

	flat := flattenCategories(tree, nil)
	if len(flat) != 5 {
		t.Fatalf("got %d categories, want 5 (the whole tree, depth-first)", len(flat))
	}
	wantOrder := []int{1, 11, 12, 121, 2}
	for i, want := range wantOrder {
		if flat[i].ID != want {
			t.Errorf("flat[%d].ID = %d, want %d", i, flat[i].ID, want)
		}
	}
}

func TestMatchNamedID(t *testing.T) {
	index := []digikey.NamedID{
		{ID: 1, Name: "Murata Electronics"},
		{ID: 2, Name: "Texas Instruments"},
		{ID: 3, Name: "Texas Components"},
	}

	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"exact name", "Murata Electronics", 1, false},
		{"exact ignoring case", "murata electronics", 1, false},
		{"unique substring", "murata", 1, false},
		{"ambiguous substring", "texas", 0, true},
		{"no match", "nonesuch", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := matchNamedID(tt.input, index, "manufacturer")
			if (err != nil) != tt.wantErr {
				t.Fatalf("matchNamedID(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("matchNamedID(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestMatchNamedIDExactBeatsSubstring(t *testing.T) {
	index := []digikey.NamedID{
		{ID: 1, Name: "TDK"},
		{ID: 2, Name: "TDK Corporation"},
	}
	// "TDK" is a substring of both, but it exactly names the first.
	got, err := matchNamedID("TDK", index, "manufacturer")
	if err != nil {
		t.Fatalf("matchNamedID() error = %v", err)
	}
	if got != 1 {
		t.Errorf("matchNamedID(\"TDK\") = %d, want 1 (the exact match)", got)
	}
}

func TestDescribeCandidatesCaps(t *testing.T) {
	var many []digikey.NamedID
	for i := range 10 {
		many = append(many, digikey.NamedID{ID: i, Name: "Mfr"})
	}
	got := describeCandidates(many)
	// An unbounded list would flood the terminal on a broad substring match.
	if !strings.HasSuffix(got, "...") {
		t.Errorf("describeCandidates() = %q, want it truncated with an ellipsis", got)
	}
	if strings.Count(got, "Mfr=") > 5 {
		t.Errorf("describeCandidates() listed more than five entries: %q", got)
	}
}

func TestIsZeroFilter(t *testing.T) {
	if !isZeroFilter(&digikey.FilterOptionsRequest{}) {
		t.Error("isZeroFilter(empty) = false, want true")
	}
	nonEmpty := []*digikey.FilterOptionsRequest{
		{SearchOptions: []string{"InStock"}},
		{MinimumQuantityAvailable: 1},
		{MarketPlaceFilter: "ExcludeMarketPlace"},
		{ManufacturerFilter: []digikey.FilterID{{ID: "1"}}},
		{CategoryFilter: []digikey.FilterID{{ID: "1"}}},
	}
	for i, f := range nonEmpty {
		if isZeroFilter(f) {
			t.Errorf("isZeroFilter(case %d) = true, want false", i)
		}
	}
}
