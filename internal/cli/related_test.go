package cli

import (
	"net/http"
	"strings"
	"testing"
)

const associationsBody = `{
  "SearchLocaleUsed": {"Currency":"USD"},
  "ProductAssociations": {
    "MatingProducts": [
      {"DigiKeyProductNumber":"WM4300-ND","ManufacturerProductNumber":"22-01-3037",
       "Manufacturer":{"Id":1,"Name":"Molex"},"Description":"CONN HOUSING 3POS",
       "UnitPrice":"$0.28","QuantityAvailable":15000,
       "ProductUrl":"https://www.digikey.com/p/wm4300"}
    ],
    "AssociatedProducts": [
      {"DigiKeyProductNumber":"WM9999-ND","ManufacturerProductNumber":"63811-1000",
       "Manufacturer":{"Id":1,"Name":"Molex"},"Description":"HAND CRIMP TOOL",
       "UnitPrice":"$249.00","QuantityAvailable":12}
    ],
    "Kits": [
      {"DigiKeyProductNumber":"KIT-1-ND","Description":"CONNECTOR KIT ASSORTMENT",
       "Manufacturer":{"Id":1,"Name":"Molex"},"UnitPrice":"$45.00","QuantityAvailable":3}
    ],
    "ForUseWithProducts": []
  }
}`

func TestRelatedListsAllRelations(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/WM4200-ND/associations", http.StatusOK, associationsBody)

	res := run(t, m, "related", "WM4200-ND")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got RelatedResult
	res.JSON(t, &got)
	if len(got.Products) != 3 {
		t.Fatalf("got %d related products, want 3", len(got.Products))
	}

	// Relations are grouped in a fixed order, mating first, because the mating
	// half is the thing most likely to be needed.
	if got.Products[0].Relation != "mating" || got.Products[0].DigiKeyPartNumber != "WM4300-ND" {
		t.Errorf("first product = %+v, want the mating half", got.Products[0])
	}
	// UnitPrice is a preformatted string on this endpoint, unlike Product.
	if got.Products[0].UnitPrice != "$0.28" {
		t.Errorf("unit_price = %q, want the preformatted string", got.Products[0].UnitPrice)
	}

	// The counts let a caller check for a mating half without scanning.
	if got.Counts["mating"] != 1 || got.Counts["accessories"] != 1 || got.Counts["kits"] != 1 {
		t.Errorf("counts = %v", got.Counts)
	}
	if got.Counts["for-use-with"] != 0 {
		t.Errorf("counts[for-use-with] = %d, want 0", got.Counts["for-use-with"])
	}
}

func TestRelatedKindFilter(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/associations", http.StatusOK, associationsBody)

	res := run(t, m, "related", "X", "--kind", "mating")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got RelatedResult
	res.JSON(t, &got)
	if len(got.Products) != 1 || got.Products[0].Relation != "mating" {
		t.Errorf("products = %+v, want only the mating half", got.Products)
	}
	if _, present := got.Counts["kits"]; present {
		t.Error("counts should only cover the requested kind")
	}
}

func TestRelatedInvalidKind(t *testing.T) {
	res := run(t, nil, "related", "X", "--kind", "bogus")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", res.Code, ExitUsage)
	}
	if !strings.Contains(res.Stderr, "mating") {
		t.Errorf("error should list the valid kinds:\n%s", res.Stderr)
	}
}

func TestRelatedEmptyIsNotAnError(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/associations", http.StatusOK,
		`{"ProductAssociations":{}}`)

	res := run(t, m, "related", "X")
	// Most parts have no associations; that is an answer, not a failure.
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	var got RelatedResult
	res.JSON(t, &got)
	if len(got.Products) != 0 {
		t.Errorf("products = %+v, want none", got.Products)
	}
}

func TestRelatedTableOutput(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/associations", http.StatusOK, associationsBody)

	res := run(t, m, "related", "X", "--output", "table")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d", res.Code)
	}
	for _, want := range []string{"RELATION", "mating", "accessory", "kit", "HAND CRIMP TOOL"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("table missing %q:\n%s", want, res.Stdout)
		}
	}
}

const recommendedBody = `{
  "Recommendations": [{
    "ProductNumber": "X",
    "RecommendedProducts": [
      {"DigiKeyProductNumber":"REC-1-ND","ManufacturerProductNumber":"MPN1",
       "ManufacturerName":"Acme","ProductDescription":"WIDGET","QuantityAvailable":100,
       "UnitPrice":1.25}
    ]
  }]
}`

func TestProductRecommended(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/recommendedproducts", http.StatusOK, recommendedBody)

	res := run(t, m, "product", "X", "--recommended")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "REC-1-ND") {
		t.Errorf("output missing the recommendation:\n%s", res.Stdout)
	}
}

func TestProductAlternatePackaging(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/alternatepackaging", http.StatusOK,
		`{"AlternatePackagings":{"AlternatePackaging":[
		   {"DigiKeyProductNumber":"ALT-1-ND","ManufacturerProductNumber":"MPN1",
		    "Manufacturer":{"Name":"Acme"},"Description":"SAME PART, REEL",
		    "UnitPrice":"$0.01","QuantityAvailable":4000}]}}`)

	res := run(t, m, "product", "X", "--alternate-packaging")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	// The response is doubly nested; the command must unwrap both levels.
	if !strings.Contains(res.Stdout, "ALT-1-ND") {
		t.Errorf("output missing the alternate packaging:\n%s", res.Stdout)
	}
}

func TestProductViewFlagsStayMutuallyExclusive(t *testing.T) {
	pairs := [][]string{
		{"--recommended", "--variations"},
		{"--alternate-packaging", "--substitutes"},
		{"--recommended", "--alternate-packaging"},
	}
	for _, pair := range pairs {
		args := append([]string{"product", "X"}, pair...)
		res := run(t, nil, args...)
		if res.Code != ExitUsage {
			t.Errorf("%v exit code = %d, want %d", pair, res.Code, ExitUsage)
		}
	}
}
