package cli

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// pricingBody is trimmed from a real /pricingbyquantity response for
// 311-10.0KHRCT-ND at quantity 250: cut tape and Digi-Reel at the exact
// quantity, and the reel DigiKey will only sell in 5000s. Note what is NOT in
// it — no QuantityAvailable anywhere, at any level. The live endpoint never
// returns one, which is why stock is joined from the product record.
const pricingBody = `{
  "RequestedProduct": "311-10.0KHRCT-ND",
  "RequestedQuantity": 250,
  "ManufacturerPartNumber": "RC0603FR-0710KL",
  "Manufacturer": {"Id": 13, "Name": "YAGEO"},
  "SettingsUsed": {"SearchLocaleUsed": {"Site":"US","Language":"en","Currency":"USD"}},
  "MyPricingOptions": [],
  "StandardPricingOptions": [
    {"PricingOption":"Exact","TotalQuantityPriced":250,"TotalPrice":2.35,
     "Products":[{"DigiKeyProductNumber":"311-10.0KHRCT-ND","QuantityPriced":250,
       "MinimumOrderQuantity":1,"ExtendedPrice":2.35,"UnitPrice":0.0094,
       "PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
       "TariffInformation":{"TariffActive":true}}]},
    {"PricingOption":"Exact","TotalQuantityPriced":250,"TotalPrice":9.35,
     "Products":[{"DigiKeyProductNumber":"311-10.0KHRDKR-ND","QuantityPriced":250,
       "MinimumOrderQuantity":1,"ExtendedPrice":9.35,"UnitPrice":0.0374,
       "PackageType":{"Id":243,"Name":"Digi-Reel®"},
       "TariffInformation":{"TariffActive":true}}]},
    {"PricingOption":"MinimumOrderQuantity","TotalQuantityPriced":5000,"TotalPrice":23.45,
     "Products":[{"DigiKeyProductNumber":"311-10.0KHRTR-ND","QuantityPriced":5000,
       "MinimumOrderQuantity":1,"ExtendedPrice":23.45,"UnitPrice":0.00469,
       "PackageType":{"Id":1,"Name":"Tape & Reel (TR)"},
       "TariffInformation":{"TariffActive":true}}]}
  ]
}`

// detailsBody is the product record the pricing answer is joined against. All
// three part numbers above are variations of this one product, which is why a
// single lookup covers every option.
const detailsBody = `{
  "Product": {
    "ManufacturerProductNumber": "RC0603FR-0710KL",
    "ProductStatus": {"Id": 0, "Status": "Active"},
    "ProductVariations": [
      {"DigiKeyProductNumber":"311-10.0KHRTR-ND","PackageType":{"Id":1,"Name":"Tape & Reel (TR)"},
       "QuantityAvailableforPackageType":4333843,"MinimumOrderQuantity":5000},
      {"DigiKeyProductNumber":"311-10.0KHRCT-ND","PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
       "QuantityAvailableforPackageType":4334182,"MinimumOrderQuantity":1},
      {"DigiKeyProductNumber":"311-10.0KHRDKR-ND","PackageType":{"Id":243,"Name":"Digi-Reel®"},
       "QuantityAvailableforPackageType":4334182,"MinimumOrderQuantity":1}
    ]
  }
}`

// pricingMock wires both calls `dk pricing` makes: the quote and the product
// record it joins stock from.
func pricingMock(t *testing.T, part string, qty string, pricing, details string) *mockDigiKey {
	t.Helper()
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/"+part+"/pricingbyquantity/"+qty, http.StatusOK, pricing)
	// dk looks the product up by a part number the quote returned, and which
	// one that is depends on the options left after filtering. Every variation
	// resolves to the same product record, which is the point, so the mock
	// answers on any of them.
	if details != "" {
		for _, dkpn := range []string{"311-10.0KHRCT-ND", "311-10.0KHRDKR-ND", "311-10.0KHRTR-ND"} {
			m.handle("GET", "/products/v4/search/"+dkpn+"/productdetails", http.StatusOK, details)
		}
	}
	return m
}

func TestPricingCostsEachOption(t *testing.T) {
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, detailsBody)

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
	if cut.Option != "Exact" || cut.OrderQuantity != 250 || cut.ForcedUp {
		t.Errorf("cut tape option = %+v, want an Exact 250 that is not forced up", cut)
	}
	if cut.TotalPrice != 2.35 {
		t.Errorf("cut tape total_price = %v, want 2.35 straight from DigiKey", cut.TotalPrice)
	}
	if len(cut.Products) != 1 {
		t.Fatalf("cut tape option has %d products, want 1", len(cut.Products))
	}
	// Every figure on the line describes the part number on the same line.
	line := cut.Products[0]
	if line.DigiKeyPartNumber != "311-10.0KHRCT-ND" || line.UnitPrice != 0.0094 ||
		line.ExtendedPrice != 2.35 || line.Quantity != 250 {
		t.Errorf("cut tape line = %+v, want the CT part number with its own price", line)
	}

	// The reel is only sold in 5000s, which DigiKey labels rather than dk
	// having to infer it from a minimum order quantity.
	reel := got.Options[2]
	if reel.Option != "MinimumOrderQuantity" || !reel.ForcedUp || reel.OrderQuantity != 5000 {
		t.Errorf("reel option = %+v, want a forced-up MinimumOrderQuantity of 5000", reel)
	}
}

// The pricing endpoint returns no stock at all, so an unjoined answer would
// report everything as unavailable and hand back best = null.
func TestPricingJoinsStockFromTheProductRecord(t *testing.T) {
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, detailsBody)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got PricingResult
	res.JSON(t, &got)
	line := got.Options[0].Products[0]
	if line.QuantityAvailable != 4334182 || !line.InStock {
		t.Errorf("cut tape line = %+v, want the stock joined from the product record", line)
	}
	if line.ProductStatus != "Active" {
		t.Errorf("product_status = %q, want Active joined from the product record", line.ProductStatus)
	}
	// The stock has to land on the matching part number, not the first
	// variation: the product record lists the reel first.
	reelLine := got.Options[2].Products[0]
	if reelLine.DigiKeyPartNumber != "311-10.0KHRTR-ND" || reelLine.QuantityAvailable != 4333843 {
		t.Errorf("reel line = %+v, want the TR variation's own stock", reelLine)
	}

	// One product lookup, whatever the option count.
	details := 0
	for _, r := range m.requests {
		if strings.Contains(r.Path, "productdetails") {
			details++
		}
	}
	if details != 1 {
		t.Errorf("made %d product lookups, want 1 for all three options", details)
	}
}

func TestPricingPicksCheapestInStock(t *testing.T) {
	// Cut tape at 2.35 is cheapest overall, but out of stock; Digi-Reel at 9.35
	// is the cheapest that can actually be filled.
	details := strings.Replace(detailsBody,
		`"DigiKeyProductNumber":"311-10.0KHRCT-ND","PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
       "QuantityAvailableforPackageType":4334182`,
		`"DigiKeyProductNumber":"311-10.0KHRCT-ND","PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
       "QuantityAvailableforPackageType":0`, 1)
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, details)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got PricingResult
	res.JSON(t, &got)
	if got.Best == nil {
		t.Fatal("best = nil, want the cheapest orderable option")
	}
	if len(got.Best.Products) != 1 || got.Best.Products[0].DigiKeyPartNumber != "311-10.0KHRDKR-ND" {
		t.Errorf("best = %+v, want the cheapest option that is actually in stock", got.Best)
	}
}

// Stock is per line, and a line quoting 5000 against 300 available is not
// orderable however much "some stock" the part has.
func TestPricingRequiresEnoughStockToFillTheLine(t *testing.T) {
	details := strings.Replace(detailsBody, `"QuantityAvailableforPackageType":4333843`,
		`"QuantityAvailableforPackageType":300`, 1)
	m := pricingMock(t, "311-10.0KHRCT-ND", "5000", pricingBody, details)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "5000")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	reel := got.Options[2]
	if reel.Products[0].QuantityAvailable != 300 {
		t.Fatalf("reel stock = %d, want 300", reel.Products[0].QuantityAvailable)
	}
	if reel.InStock || reel.Products[0].InStock {
		t.Error("a 5000-piece line against 300 in stock must not read as in stock")
	}
}

// A mixed option is the case that broke the old flat shape: one option, two
// part numbers, each with its own quantity and price.
func TestPricingKeepsAMixedOptionTogether(t *testing.T) {
	const mixed = `{
	  "RequestedProduct":"311-10.0KHRCT-ND","RequestedQuantity":5250,
	  "MyPricingOptions": [],
	  "StandardPricingOptions":[
	    {"PricingOption":"Exact","TotalQuantityPriced":5250,"TotalPrice":24.62,
	     "Products":[
	       {"DigiKeyProductNumber":"311-10.0KHRTR-ND","QuantityPriced":5000,
	        "ExtendedPrice":23.45,"UnitPrice":0.00469,"PackageType":{"Id":1,"Name":"Tape & Reel (TR)"}},
	       {"DigiKeyProductNumber":"311-10.0KHRCT-ND","QuantityPriced":250,
	        "ExtendedPrice":1.17,"UnitPrice":0.00469,"PackageType":{"Id":2,"Name":"Cut Tape (CT)"}}]}]}`

	m := pricingMock(t, "311-10.0KHRCT-ND", "5250", mixed, detailsBody)
	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "5250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got PricingResult
	res.JSON(t, &got)
	if len(got.Options) != 1 || len(got.Options[0].Products) != 2 {
		t.Fatalf("options = %+v, want one option holding two lines", got.Options)
	}
	opt := got.Options[0]
	if opt.OrderQuantity != 5250 || opt.TotalPrice != 24.62 {
		t.Errorf("option = %+v, want the whole split at 5250 for 24.62", opt)
	}
	// The reel's quantity must not be printed against the cut tape's number.
	if opt.Products[0].Quantity != 5000 || opt.Products[1].Quantity != 250 {
		t.Errorf("lines = %+v, want 5000 on the reel and 250 on the cut tape", opt.Products)
	}
	// The product lookup is by a part number the quote returned, so the join
	// still lands even though the caller typed a different one.
	if opt.Products[0].QuantityAvailable != 4333843 || opt.Products[1].QuantityAvailable != 4334182 {
		t.Errorf("lines = %+v, want each line's own stock", opt.Products)
	}
}

func TestPricingNoneInStock(t *testing.T) {
	details := strings.NewReplacer(
		`"QuantityAvailableforPackageType":4333843`, `"QuantityAvailableforPackageType":0`,
		`"QuantityAvailableforPackageType":4334182`, `"QuantityAvailableforPackageType":0`,
	).Replace(detailsBody)
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, details)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	if got.Best != nil {
		t.Errorf("best = %+v, want nil when nothing is in stock", got.Best)
	}
	if len(got.Options) != 3 {
		t.Error("options should still be listed even when unavailable")
	}
}

func TestPricingRequestsTheQuantityInThePath(t *testing.T) {
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, detailsBody)
	if res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250"); res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var path string
	for _, r := range m.requests {
		if strings.Contains(r.Path, "pricingbyquantity") {
			path = r.Path
		}
	}
	// The quantity is a path segment on this endpoint, not a query parameter.
	if path != "/products/v4/search/311-10.0KHRCT-ND/pricingbyquantity/250" {
		t.Errorf("path = %q, want the quantity in the path", path)
	}
}

// --packaging is a filter over what came back, since the endpoint has no
// preference parameter of its own.
func TestPricingFiltersByPackaging(t *testing.T) {
	tests := []struct {
		packaging string
		want      []string
	}{
		{"CT", []string{"311-10.0KHRCT-ND"}},
		{"ct", []string{"311-10.0KHRCT-ND"}},
		{"DKR", []string{"311-10.0KHRDKR-ND"}},
		// A full reel is routinely the cheapest way to buy a quantity, so the
		// packaging BetterValue arrives in has to be one --packaging can name.
		{"TR", []string{"311-10.0KHRTR-ND"}},
		{"", []string{"311-10.0KHRCT-ND", "311-10.0KHRDKR-ND", "311-10.0KHRTR-ND"}},
	}

	for _, tc := range tests {
		t.Run("packaging="+tc.packaging, func(t *testing.T) {
			m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, detailsBody)
			args := []string{"pricing", "311-10.0KHRCT-ND", "--qty", "250"}
			if tc.packaging != "" {
				args = append(args, "--packaging", tc.packaging)
			}
			res := run(t, m, args...)
			if res.Code != ExitOK {
				t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
			}

			var got PricingResult
			res.JSON(t, &got)
			var parts []string
			for _, o := range got.Options {
				for _, p := range o.Products {
					parts = append(parts, p.DigiKeyPartNumber)
				}
			}
			if strings.Join(parts, ",") != strings.Join(tc.want, ",") {
				t.Errorf("--packaging %q kept %v, want %v", tc.packaging, parts, tc.want)
			}
		})
	}
}

// A mixed option is not a CT option: quoting it under --packaging CT would hand
// back the reel the caller filtered out.
func TestPricingPackagingFilterRejectsMixedOptions(t *testing.T) {
	const mixed = `{"RequestedProduct":"X","RequestedQuantity":5250,"MyPricingOptions":[],
	  "StandardPricingOptions":[
	    {"PricingOption":"Exact","TotalQuantityPriced":5250,"TotalPrice":24.62,
	     "Products":[
	       {"DigiKeyProductNumber":"311-10.0KHRTR-ND","QuantityPriced":5000,"ExtendedPrice":23.45,
	        "UnitPrice":0.00469,"PackageType":{"Id":1,"Name":"Tape & Reel (TR)"}},
	       {"DigiKeyProductNumber":"311-10.0KHRCT-ND","QuantityPriced":250,"ExtendedPrice":1.17,
	        "UnitPrice":0.00469,"PackageType":{"Id":2,"Name":"Cut Tape (CT)"}}]}]}`

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/pricingbyquantity/5250", http.StatusOK, mixed)

	res := run(t, m, "pricing", "X", "--qty", "5250", "--packaging", "CT")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	if len(got.Options) != 0 {
		t.Errorf("options = %+v, want the mixed option filtered out of --packaging CT", got.Options)
	}
	// Nothing to look stock up for, so no second call is made.
	for _, r := range m.requests {
		if strings.Contains(r.Path, "productdetails") {
			t.Error("a product lookup was made with no options to join it onto")
		}
	}
}

// The documented empty case is [], not null, for both levels of the shape.
func TestPricingArraysAreNeverNull(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/pricingbyquantity/10", http.StatusOK,
		`{"RequestedProduct":"X","RequestedQuantity":10,"MyPricingOptions":[],"StandardPricingOptions":[]}`)

	res := run(t, m, "pricing", "X", "--qty", "10")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var raw map[string]json.RawMessage
	res.JSON(t, &raw)
	if string(raw["options"]) != "[]" {
		t.Errorf("options = %s, want []", raw["options"])
	}
	if string(raw["best"]) != "null" {
		t.Errorf("best = %s, want an explicit null", raw["best"])
	}
}

// MyPricingOptions wins when DigiKey returns any. It comes back empty even for
// an authenticated caller on an account with no negotiated pricing, so its
// absence is the normal case rather than a failure.
func TestPricingPrefersAccountOptions(t *testing.T) {
	const both = `{"RequestedProduct":"X","RequestedQuantity":10,
	  "MyPricingOptions":[{"PricingOption":"Exact","TotalQuantityPriced":10,"TotalPrice":1.0,
	    "Products":[{"DigiKeyProductNumber":"MINE-ND","QuantityPriced":10,"ExtendedPrice":1.0,
	      "UnitPrice":0.1,"PackageType":{"Id":2,"Name":"Cut Tape (CT)"}}]}],
	  "StandardPricingOptions":[{"PricingOption":"Exact","TotalQuantityPriced":10,"TotalPrice":9.0,
	    "Products":[{"DigiKeyProductNumber":"LIST-ND","QuantityPriced":10,"ExtendedPrice":9.0,
	      "UnitPrice":0.9,"PackageType":{"Id":2,"Name":"Cut Tape (CT)"}}]}]}`

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/pricingbyquantity/10", http.StatusOK, both)
	m.handle("GET", "/products/v4/search/MINE-ND/productdetails", http.StatusOK,
		`{"Product":{"ProductStatus":{"Status":"Active"},"ProductVariations":[
		  {"DigiKeyProductNumber":"MINE-ND","QuantityAvailableforPackageType":500}]}}`)

	res := run(t, m, "pricing", "X", "--qty", "10")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	if len(got.Options) != 1 || got.Options[0].Products[0].DigiKeyPartNumber != "MINE-ND" {
		t.Errorf("options = %+v, want the account pricing to win", got.Options)
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

func TestPricingRejectsUnknownPackaging(t *testing.T) {
	// CT, TR and DKR are the packagings this filter can name. Anything else was
	// silently ignored server-side by the endpoint this command used to call,
	// and here it would filter every option away for no stated reason.
	// "CutTapeOrTR" is the value dk itself documented until the flag was
	// narrowed, and it is still in shell history.
	tests := []struct {
		name      string
		packaging string
		want      int
	}{
		{"documented cut tape", "CT", ExitOK},
		{"documented tape and reel", "TR", ExitOK},
		{"documented digi-reel", "DKR", ExitOK},
		{"lowercase is accepted", "ct", ExitOK},
		{"empty means every option", "", ExitOK},
		{"the value dk used to document", "CutTapeOrTR", ExitUsage},
		{"anything else", "reel", ExitUsage},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, detailsBody)

			res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250", "--packaging", tc.packaging)
			if res.Code != tc.want {
				t.Fatalf("--packaging %q exit code = %d, want %d\nstderr: %s",
					tc.packaging, res.Code, tc.want, res.Stderr)
			}
			if tc.want != ExitUsage {
				return
			}
			// Rejected before the wire: a bad preference must not spend a
			// request whose answer would be filtered to nothing anyway.
			for _, r := range m.requests {
				if strings.Contains(r.Path, "pricingbyquantity") {
					t.Errorf("a rejected --packaging still called the API: %s", r.Path)
				}
			}
		})
	}
}

// cappedBody is the live answer for WK-KIT-ND at quantity 1000, trimmed.
// DigiKey has 172 of a discontinued part and quotes those 172 as the only
// option, labelled MaxOrderQuantity. Note what the option does NOT say: the
// quantity went down rather than up, so forced_up is false, and the total is
// the real price of the 172 it will sell. Nothing here reads as a shortfall.
const cappedBody = `{
  "RequestedProduct": "WK-KIT-ND",
  "RequestedQuantity": 1000,
  "SettingsUsed": {"SearchLocaleUsed": {"Site":"US","Language":"en","Currency":"USD"}},
  "MyPricingOptions": [],
  "StandardPricingOptions": [
    {"PricingOption":"MaxOrderQuantity","TotalQuantityPriced":172,"TotalPrice":1327.84,
     "Products":[{"DigiKeyProductNumber":"WK-KIT-ND","QuantityPriced":172,
       "MinimumOrderQuantity":1,"ExtendedPrice":1327.84,"UnitPrice":7.72,
       "PackageType":{"Id":0,"Name":"Bag"},
       "TariffInformation":{"TariffActive":true}}]}
  ]
}`

const cappedDetailsBody = `{
  "Product": {
    "ProductStatus": {"Id": 1, "Status": "Discontinued at DigiKey"},
    "ProductVariations": [
      {"DigiKeyProductNumber":"WK-KIT-ND","PackageType":{"Id":0,"Name":"Bag"},
       "QuantityAvailableforPackageType":172,"MinimumOrderQuantity":1}
    ]
  }
}`

// An option that hands over fewer parts than were asked for must never be
// best. It is in stock and it is the cheapest thing on offer — necessarily so,
// since it buys less — and picking it answers "what do I order for 1000?" with
// an order for 172.
func TestPricingNeverRecommendsAnOptionShortOfTheRequest(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/WK-KIT-ND/pricingbyquantity/1000", http.StatusOK, cappedBody)
	m.handle("GET", "/products/v4/search/WK-KIT-ND/productdetails", http.StatusOK, cappedDetailsBody)

	res := run(t, m, "pricing", "WK-KIT-ND", "--qty", "1000")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got PricingResult
	res.JSON(t, &got)
	if len(got.Options) != 1 {
		t.Fatalf("got %d options, want 1", len(got.Options))
	}
	opt := got.Options[0]
	if !opt.Short {
		t.Errorf("option = %+v, want short: 172 does not fill a request for 1000", opt)
	}
	if opt.ForcedUp {
		t.Error("forced_up must stay false when the quantity was capped downward")
	}
	// The line itself is genuinely orderable — this is not an out-of-stock part.
	if !opt.InStock {
		t.Errorf("option = %+v, want in_stock: DigiKey has all 172 it quoted", opt)
	}
	if got.Best != nil {
		t.Errorf("best = %+v, want nil: no option covers the 1000 requested", got.Best)
	}
}

// Reporting a capped part as out of stock sends the reader looking for a second
// source when there is stock on the shelf. The count it will sell is the answer.
func TestPricingSaysHowManyACappedPartWillSell(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/WK-KIT-ND/pricingbyquantity/1000", http.StatusOK, cappedBody)
	m.handle("GET", "/products/v4/search/WK-KIT-ND/productdetails", http.StatusOK, cappedDetailsBody)

	res := run(t, m, "pricing", "WK-KIT-ND", "--qty", "1000", "--output", "table")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "the most DigiKey will sell is 172") {
		t.Errorf("stderr = %q, want the quantity DigiKey will actually sell", res.Stderr)
	}
	if strings.Contains(res.Stderr, "None of these pricing options are in stock") {
		t.Error("a part with 172 on the shelf must not be reported as out of stock")
	}
	// The table has to carry the same warning the JSON does.
	if !strings.Contains(res.Stdout, "!") || !strings.Contains(res.Stderr, "capped below the 1000 requested") {
		t.Errorf("table = %q\nstderr = %q, want the capped option marked and explained", res.Stdout, res.Stderr)
	}
}

// The currency label and the figures beside it have to come from one source.
// DigiKey answers a --currency it will not honor for the site by pricing in the
// site's own currency, and says so in SettingsUsed; the config value is only
// what dk asked for.
func TestPricingReportsTheCurrencyDigiKeyApplied(t *testing.T) {
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, detailsBody)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250", "--currency", "EUR")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	if got.Currency != "USD" {
		t.Errorf("currency = %q, want USD: DigiKey priced in USD however the request was made", got.Currency)
	}
}

// Falling back to the requested currency keeps the key populated for a response
// that carries no echo of its own.
func TestPricingFallsBackToTheRequestedCurrency(t *testing.T) {
	body := strings.Replace(pricingBody,
		`"SettingsUsed": {"SearchLocaleUsed": {"Site":"US","Language":"en","Currency":"USD"}},`, "", 1)
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", body, detailsBody)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250", "--currency", "EUR")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	if got.Currency != "EUR" {
		t.Errorf("currency = %q, want the requested EUR when DigiKey echoed nothing", got.Currency)
	}
}

// A two-product option prints its option-level figures once, so a spreadsheet
// summing TOTAL does not treble it. That leaves the CSV with blank cells, and
// blanks are all a machine reader has to go on — without the option number it
// cannot tell a continuation row from an option with no label and no total.
func TestPricingCSVKeepsAMixedOptionGroupable(t *testing.T) {
	const mixed = `{
	  "RequestedProduct":"311-10.0KHRCT-ND","RequestedQuantity":5250,
	  "MyPricingOptions": [],
	  "StandardPricingOptions":[
	    {"PricingOption":"Exact","TotalQuantityPriced":5250,"TotalPrice":24.62,
	     "Products":[
	       {"DigiKeyProductNumber":"311-10.0KHRTR-ND","QuantityPriced":5000,
	        "ExtendedPrice":23.45,"UnitPrice":0.00469,"PackageType":{"Id":1,"Name":"Tape & Reel (TR)"}},
	       {"DigiKeyProductNumber":"311-10.0KHRCT-ND","QuantityPriced":250,
	        "ExtendedPrice":1.17,"UnitPrice":0.00469,"PackageType":{"Id":2,"Name":"Cut Tape (CT)"}}]}]}`

	m := pricingMock(t, "311-10.0KHRCT-ND", "5250", mixed, detailsBody)
	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "5250", "--output", "csv")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	rows, err := csv.NewReader(strings.NewReader(res.Stdout)).ReadAll()
	if err != nil {
		t.Fatalf("stdout is not valid csv: %v\n%s", err, res.Stdout)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d csv rows, want a header and one row per product:\n%s", len(rows), res.Stdout)
	}
	if rows[0][0] != "#" {
		t.Fatalf("first column = %q, want the option number that groups the rows", rows[0][0])
	}
	// Both lines belong to option 1, and say so on their own row.
	if rows[1][0] != "1" || rows[2][0] != "1" {
		t.Errorf("option numbers = %q and %q, want both rows in option 1", rows[1][0], rows[2][0])
	}
	// The total still appears once, so summing the column gives 24.62.
	if rows[1][3] != "24.6200" || rows[2][3] != "" {
		t.Errorf("totals = %q and %q, want the option total on the first row only", rows[1][3], rows[2][3])
	}
	// And each row keeps its own product's figures.
	if rows[1][4] != "311-10.0KHRTR-ND" || rows[2][4] != "311-10.0KHRCT-ND" {
		t.Errorf("part numbers = %q and %q, want one product per row", rows[1][4], rows[2][4])
	}
}

// The filter matches PackageType.Id, not PackageType.Name. Name is display text
// that DigiKey localizes — a JP-site response calls id 2 "カット テープ（CT）" —
// so an English substring match returned nothing at all off the US site and
// reported a normally-priced part as unavailable.
func TestPricingPackagingMatchesIDNotLocalizedName(t *testing.T) {
	const localized = `{"RequestedProduct":"X","RequestedQuantity":250,"MyPricingOptions":[],
	  "StandardPricingOptions":[
	    {"PricingOption":"Exact","TotalQuantityPriced":250,"TotalPrice":1.17,
	     "Products":[
	       {"DigiKeyProductNumber":"311-10.0KHRCT-ND","QuantityPriced":250,"ExtendedPrice":1.17,
	        "UnitPrice":0.00469,"PackageType":{"Id":2,"Name":"\u30ab\u30c3\u30c8 \u30c6\u30fc\u30d7\uff08CT\uff09"}}]}]}`
	const details = `{"Product":{"ProductStatus":{"Status":"Active"},
	  "ProductVariations":[{"DigiKeyProductNumber":"311-10.0KHRCT-ND",
	    "QuantityAvailableforPackageType":4244251,"PackageType":{"Id":2,"Name":"\u30ab\u30c3\u30c8 \u30c6\u30fc\u30d7\uff08CT\uff09"}}]}}`

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/pricingbyquantity/250", http.StatusOK, localized)
	m.handle("GET", "/products/v4/search/311-10.0KHRCT-ND/productdetails", http.StatusOK, details)

	res := run(t, m, "pricing", "X", "--qty", "250", "--packaging", "CT")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got PricingResult
	res.JSON(t, &got)
	if len(got.Options) != 1 {
		t.Fatalf("options = %+v, want the cut-tape option kept despite its localized name", got.Options)
	}
}

// An empty result has two very different causes, and the output has to say
// which. "DigiKey returned no pricing options" for options DigiKey did return
// and --packaging discarded reports a priced part as unpriceable.
func TestPricingEmptyAfterFilterBlamesTheFilter(t *testing.T) {
	m2 := newMockDigiKey(t)
	const ctOnly = `{"RequestedProduct":"X","RequestedQuantity":250,"MyPricingOptions":[],
	  "StandardPricingOptions":[
	    {"PricingOption":"Exact","TotalQuantityPriced":250,"TotalPrice":1.17,
	     "Products":[
	       {"DigiKeyProductNumber":"311-10.0KHRCT-ND","QuantityPriced":250,"ExtendedPrice":1.17,
	        "UnitPrice":0.00469,"PackageType":{"Id":2,"Name":"Cut Tape (CT)"}}]}]}`
	m2.handle("GET", "/products/v4/search/X/pricingbyquantity/250", http.StatusOK, ctOnly)

	res2 := run(t, m2, "pricing", "X", "--qty", "250", "--packaging", "DKR", "--output", "table")
	if res2.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res2.Code, res2.Stderr)
	}
	out := res2.Stdout + res2.Stderr
	if strings.Contains(out, "DigiKey returned no pricing options") {
		t.Errorf("empty result blames DigiKey for what --packaging dropped:\n%s", out)
	}
	if !strings.Contains(out, "packaging") {
		t.Errorf("empty result does not say the filter emptied it:\n%s", out)
	}

	// The table line is prose, and prose is suppressed in JSON by design, so a
	// scripted caller can only tell the two cases apart from the payload.
	m3 := newMockDigiKey(t)
	m3.handle("GET", "/products/v4/search/X/pricingbyquantity/250", http.StatusOK, ctOnly)
	res3 := run(t, m3, "pricing", "X", "--qty", "250", "--packaging", "DKR")
	if res3.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res3.Code, res3.Stderr)
	}
	var got PricingResult
	res3.JSON(t, &got)
	if len(got.Options) != 0 || got.Best != nil {
		t.Fatalf("options = %+v, best = %+v, want both empty", got.Options, got.Best)
	}
	if got.PackagingFilteredOut != 1 {
		t.Errorf("packaging_filtered_out = %d, want 1: without it options:[] reads as an unpriceable part",
			got.PackagingFilteredOut)
	}
	if got.Packaging != "DKR" {
		t.Errorf("packaging = %q, want DKR", got.Packaging)
	}
}

// No filter, no keys: a caller testing for packaging_filtered_out must not find
// a zero on a result that was never narrowed.
func TestPricingOmitsPackagingKeysWithoutTheFlag(t *testing.T) {
	m := pricingMock(t, "311-10.0KHRCT-ND", "250", pricingBody, detailsBody)
	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		t.Fatalf("stdout is not json: %v", err)
	}
	for _, k := range []string{"packaging", "packaging_filtered_out"} {
		if _, ok := raw[k]; ok {
			t.Errorf("%q is present without --packaging", k)
		}
	}
}
