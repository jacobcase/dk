package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcase/dk/internal/digikey"
)

// packagingBody offers cut tape (buyable at 250) and tape & reel (which forces
// the quantity up to a full 4000-piece reel) — the trap this command exists to
// surface.
const packagingBody = `{
  "AccountIdUsed": 0,
  "Products": [
    {
      "RecommendedQuantity": 250,
      "DigiKeyProductNumber": "311-10.0KHRCT-ND",
      "PackageType": {"Id":2,"Name":"Cut Tape (CT)"},
      "QuantityAvailable": 50000,
      "MinimumOrderQuantity": 1,
      "ProductStatus": "Active",
      "StandardPricing": [
        {"BreakQuantity":1,"UnitPrice":0.10,"TotalPrice":0.10},
        {"BreakQuantity":100,"UnitPrice":0.02,"TotalPrice":2.00},
        {"BreakQuantity":1000,"UnitPrice":0.01,"TotalPrice":10.00}
      ]
    },
    {
      "RecommendedQuantity": 4000,
      "DigiKeyProductNumber": "311-10.0KHRTR-ND",
      "PackageType": {"Id":3,"Name":"Tape & Reel (TR)"},
      "QuantityAvailable": 12000,
      "MinimumOrderQuantity": 4000,
      "StandardPackage": 4000,
      "ProductStatus": "Active",
      "StandardPricing": [{"BreakQuantity":4000,"UnitPrice":0.005,"TotalPrice":20.00}]
    },
    {
      "RecommendedQuantity": 250,
      "DigiKeyProductNumber": "311-OUTOFSTOCK-ND",
      "PackageType": {"Id":4,"Name":"Digi-Reel"},
      "QuantityAvailable": 0,
      "MinimumOrderQuantity": 1,
      "ProductStatus": "Active",
      "StandardPricing": [{"BreakQuantity":1,"UnitPrice":0.001,"TotalPrice":0.001}]
    }
  ]
}`

func TestPricingCostsEachPackaging(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/packagetypebyquantity/311-10.0KHRCT-ND",
		http.StatusOK, packagingBody)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got PricingResult
	res.JSON(t, &got)
	if got.RequestedQuantity != 250 {
		t.Errorf("requested_quantity = %d, want 250", got.RequestedQuantity)
	}
	if len(got.Options) != 3 {
		t.Fatalf("got %d options, want 3", len(got.Options))
	}

	cut := got.Options[0]
	// 250 falls in the 100+ break, so 0.02 each.
	if cut.UnitPrice != 0.02 {
		t.Errorf("cut tape unit_price = %v, want the 100+ break price 0.02", cut.UnitPrice)
	}
	if cut.ExtendedPrice != 5.0 {
		t.Errorf("cut tape extended_price = %v, want 5.00", cut.ExtendedPrice)
	}
	if cut.ForcedUp {
		t.Error("cut tape forced_up = true, but 250 is orderable as-is")
	}

	reel := got.Options[1]
	// This is the trap: asking for 250 gets you 4000.
	if reel.OrderQuantity != 4000 {
		t.Errorf("reel order_quantity = %d, want 4000", reel.OrderQuantity)
	}
	if !reel.ForcedUp {
		t.Error("reel forced_up = false, but the minimum forces 4000 for a request of 250")
	}
	if reel.ExtendedPrice != 20.0 {
		t.Errorf("reel extended_price = %v, want 20.00", reel.ExtendedPrice)
	}
}

func TestPricingPicksCheapestInStock(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/packagetypebyquantity/X", http.StatusOK, packagingBody)

	res := run(t, m, "pricing", "X", "--qty", "250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got PricingResult
	res.JSON(t, &got)
	if got.Best == nil {
		t.Fatal("best = nil, want the cheapest in-stock option")
	}
	// Digi-Reel is cheapest on paper but has zero stock, so cut tape wins.
	if got.Best.DigiKeyPartNumber != "311-10.0KHRCT-ND" {
		t.Errorf("best = %q, want the cheapest option that is actually in stock", got.Best.DigiKeyPartNumber)
	}
}

func TestPricingNoneInStock(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/packagetypebyquantity/X", http.StatusOK,
		`{"Products":[{"DigiKeyProductNumber":"A-ND","QuantityAvailable":0,
		  "RecommendedQuantity":10,"StandardPricing":[{"BreakQuantity":1,"UnitPrice":1.0}]}]}`)

	res := run(t, m, "pricing", "X", "--qty", "10")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	if got.Best != nil {
		t.Errorf("best = %+v, want nil when nothing is in stock", got.Best)
	}
	if len(got.Options) != 1 {
		t.Errorf("options should still be listed even when unavailable")
	}
}

func TestPricingSendsRequestedQuantity(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/packagetypebyquantity/X", http.StatusOK, packagingBody)

	// CT and DKR are the only values DigiKey documents for packagingPreference;
	// the CutTapeOrTR family belongs to MyLists' ListSettings and is ignored here.
	if res := run(t, m, "pricing", "X", "--qty", "250", "--packaging", "CT"); res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var query string
	for _, r := range m.requests {
		if strings.Contains(r.Path, "packagetypebyquantity") {
			query = r.Query
		}
	}
	if !strings.Contains(query, "requestedQuantity=250") {
		t.Errorf("query = %q, want requestedQuantity=250", query)
	}
	if !strings.Contains(query, "packagingPreference=CT") {
		t.Errorf("query = %q, want the packaging preference forwarded verbatim", query)
	}
}

func TestPricingRejectsBadQuantity(t *testing.T) {
	for _, qty := range []string{"0", "-5"} {
		res := run(t, nil, "pricing", "X", "--qty", qty)
		if res.Code != ExitUsage {
			t.Errorf("--qty %s exit code = %d, want %d", qty, res.Code, ExitUsage)
		}
	}
}

func TestUnitPriceAtQuantity(t *testing.T) {
	breaks := []digikey.PriceBreak{
		{BreakQuantity: 1, UnitPrice: 0.10},
		{BreakQuantity: 100, UnitPrice: 0.02},
		{BreakQuantity: 1000, UnitPrice: 0.01},
	}

	tests := []struct {
		name     string
		quantity int
		want     float64
	}{
		{"below the first break", 1, 0.10},
		{"between breaks", 50, 0.10},
		{"exactly on a break", 100, 0.02},
		{"above a break", 250, 0.02},
		{"top break", 5000, 0.01},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unitPriceAtQuantity(breaks, tt.quantity); got != tt.want {
				t.Errorf("unitPriceAtQuantity(%d) = %v, want %v", tt.quantity, got, tt.want)
			}
		})
	}
}

func TestUnitPriceAtQuantityBelowEveryBreak(t *testing.T) {
	// A reel-only part has no break below its minimum; quoting 0 would be worse
	// than quoting the lowest tier.
	breaks := []digikey.PriceBreak{{BreakQuantity: 4000, UnitPrice: 0.005}}
	if got := unitPriceAtQuantity(breaks, 250); got != 0.005 {
		t.Errorf("unitPriceAtQuantity() = %v, want the lowest break 0.005", got)
	}
}

func TestUnitPriceAtQuantityNoBreaks(t *testing.T) {
	if got := unitPriceAtQuantity(nil, 10); got != 0 {
		t.Errorf("unitPriceAtQuantity(nil) = %v, want 0", got)
	}
}

func TestPricingPrefersMyPricing(t *testing.T) {
	p := digikey.PackageTypeByQuantityProduct{
		RecommendedQuantity: 100,
		QuantityAvailable:   500,
		StandardPricing:     []digikey.PriceBreak{{BreakQuantity: 1, UnitPrice: 1.00}},
		MyPricing:           []digikey.PriceBreak{{BreakQuantity: 1, UnitPrice: 0.25}},
	}
	opt := buildPackagingOption(p, 100)
	// A 3-legged token yields account pricing, which must win.
	if opt.UnitPrice != 0.25 {
		t.Errorf("unit_price = %v, want the account price 0.25", opt.UnitPrice)
	}
}

func TestBuildPackagingOptionFallsBackWhenRecommendedQuantityMissing(t *testing.T) {
	p := digikey.PackageTypeByQuantityProduct{
		MinimumOrderQuantity: 500,
		QuantityAvailable:    1000,
		StandardPricing:      []digikey.PriceBreak{{BreakQuantity: 1, UnitPrice: 0.10}},
	}
	// With no RecommendedQuantity, the minimum still has to be respected.
	opt := buildPackagingOption(p, 100)
	if opt.OrderQuantity != 500 {
		t.Errorf("order_quantity = %d, want the minimum 500", opt.OrderQuantity)
	}
	if !opt.ForcedUp {
		t.Error("forced_up = false, but the minimum exceeds the request")
	}
}
