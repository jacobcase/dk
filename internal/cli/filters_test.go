package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcase/dk/internal/digikey"
)

// facetResponseBody is a keyword search response carrying the FilterOptions
// facet set, which is the only way DigiKey exposes available filters.
const facetResponseBody = `{
  "ProductsCount": 4210,
  "SearchLocaleUsed": {"Site":"US","Language":"en","Currency":"USD"},
  "Products": [],
  "FilterOptions": {
    "Manufacturers": [
      {"Id":2359,"Value":"Murata Electronics","ProductCount":1200},
      {"Id":10,"Value":"Vishay","ProductCount":800}
    ],
    "Packaging": [{"Id":2,"Value":"Cut Tape (CT)","ProductCount":4000}],
    "Status": [{"Id":0,"Value":"Active","ProductCount":4100}],
    "TopCategories": [
      {"Category":{"Id":60,"Name":"Ceramic Capacitors","ProductCount":4210},
       "RootCategory":{"Id":3,"Name":"Capacitors"},"Score":0.98},
      {"Category":{"Id":61,"Name":"Film Capacitors","ProductCount":90},
       "RootCategory":{"Id":3,"Name":"Capacitors"},"Score":0.21}
    ],
    "ParametricFilters": [
      {
        "Category": {"Id":60,"Value":"Ceramic Capacitors"},
        "ParameterId": 2049, "ParameterName": "Capacitance", "ParameterType": "UnitOfMeasure",
        "FilterValues": [
          {"ValueId":"u0.1","ValueName":"0.1 µF","ProductCount":1500},
          {"ValueId":"u1.0","ValueName":"1 µF","ProductCount":900},
          {"ValueId":"u10","ValueName":"10 µF","ProductCount":400},
          {"ValueId":"min","ValueName":"1 pF","ProductCount":0,"RangeFilterType":"Min"}
        ]
      },
      {
        "Category": {"Id":60,"Value":"Ceramic Capacitors"},
        "ParameterId": 1291, "ParameterName": "Tolerance", "ParameterType": "String",
        "FilterValues": [
          {"ValueId":"t10","ValueName":"±10%","ProductCount":2000},
          {"ValueId":"t5","ValueName":"±5%","ProductCount":1200}
        ]
      },
      {
        "Category": {"Id":60,"Value":"Ceramic Capacitors"},
        "ParameterId": 2079, "ParameterName": "Temperature Coefficient", "ParameterType": "String",
        "FilterValues": [
          {"ValueId":"x7r","ValueName":"X7R","ProductCount":1800},
          {"ValueId":"c0g","ValueName":"C0G, NP0","ProductCount":700}
        ]
      }
    ]
  }
}`

func TestFiltersDiscoversParameters(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "filters", "0603", "ceramic", "capacitor")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got FiltersResult
	res.JSON(t, &got)

	if got.TotalMatches != 4210 {
		t.Errorf("total_matches = %d, want 4210", got.TotalMatches)
	}
	if len(got.Parameters) != 3 {
		t.Fatalf("got %d parameters, want 3", len(got.Parameters))
	}
	if got.Parameters[0].ParameterName != "Capacitance" || got.Parameters[0].ParameterID != 2049 {
		t.Errorf("first parameter = %+v, want Capacitance/2049", got.Parameters[0])
	}
	// JSON always carries every value; only the table view caps them.
	if len(got.Parameters[0].Values) != 4 {
		t.Errorf("got %d capacitance values, want all 4", len(got.Parameters[0].Values))
	}
	// The category the parametric filters belong to is what `--param` needs.
	if got.Category == nil || got.Category.ID != 60 {
		t.Errorf("category = %+v, want Ceramic Capacitors (60)", got.Category)
	}
	if len(got.Manufacturers) != 2 || got.Manufacturers[0].Name != "Murata Electronics" {
		t.Errorf("manufacturers = %+v", got.Manufacturers)
	}
}

func TestFiltersUsesMinimalDiscoverySearch(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	if res := run(t, m, "filters", "capacitor"); res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var body map[string]any
	for _, r := range m.requests {
		if r.Path == "/products/v4/search/keyword" {
			_ = json.Unmarshal([]byte(r.Body), &body)
		}
	}
	// Facets come back regardless of page size, so discovery should not pull
	// products it will throw away.
	if body["Limit"] != float64(discoveryLimit) {
		t.Errorf("Limit = %v, want %d for a discovery search", body["Limit"], discoveryLimit)
	}
}

func TestFiltersTableCapsValues(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "filters", "capacitor", "--output", "table", "--values", "2")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	// Capacitance has four values; two shown plus an overflow marker.
	if !strings.Contains(res.Stdout, "(+2 more)") {
		t.Errorf("table should mark the truncated values:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "0.1 µF (1500)") {
		t.Errorf("table should show values with product counts:\n%s", res.Stdout)
	}
}

func TestFiltersDrillIntoParameter(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "filters", "capacitor", "--parameter", "Tolerance")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	// The drill-down narrows the documented envelope rather than replacing it,
	// so one JSON path reads both halves of the discovery loop.
	var got FiltersResult
	res.JSON(t, &got)
	if got.Query != "capacitor" || got.TotalMatches == 0 {
		t.Errorf("query = %q, total_matches = %d: the drill-down must keep the envelope",
			got.Query, got.TotalMatches)
	}
	if len(got.Parameters) != 1 {
		t.Fatalf("got %d parameters, want 1: --parameter filters the array, it does not replace the shape",
			len(got.Parameters))
	}
	if got.Parameters[0].ParameterName != "Tolerance" {
		t.Errorf("parameter_name = %q", got.Parameters[0].ParameterName)
	}
	if len(got.Parameters[0].Values) != 2 {
		t.Errorf("got %d values, want 2", len(got.Parameters[0].Values))
	}
}

func TestFiltersDrillByParameterID(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "filters", "capacitor", "--parameter", "2049")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got FiltersResult
	res.JSON(t, &got)
	if len(got.Parameters) != 1 {
		t.Fatalf("got %d parameters, want 1", len(got.Parameters))
	}
	if got.Parameters[0].ParameterID != 2049 {
		t.Errorf("parameter_id = %d, want 2049", got.Parameters[0].ParameterID)
	}
}

func TestFiltersUnknownParameterListsAvailable(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "filters", "capacitor", "--parameter", "Inductance")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	// Telling the caller what *is* available is what lets an agent recover
	// without a second round trip.
	if !strings.Contains(res.Stderr, "Capacitance") {
		t.Errorf("error should list the available parameters:\n%s", res.Stderr)
	}
}

func TestSearchWithParamsBuildsParametricRequest(t *testing.T) {
	m := newMockDigiKey(t)

	// Two calls hit the same endpoint: discovery, then the filtered search.
	var bodies []map[string]any
	m.routes["POST /products/v4/search/keyword"] = func(w http.ResponseWriter, r *http.Request) {
		if len(bodies) == 0 {
			_, _ = w.Write([]byte(facetResponseBody))
		} else {
			_, _ = w.Write([]byte(searchResponseBody))
		}
		bodies = append(bodies, nil)
	}

	res := run(t, m, "search", "0603 ceramic capacitor",
		"--param", "Capacitance=0.1 µF", "--param", "Tolerance=±10%")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var searchBodies []map[string]any
	for _, r := range m.requests {
		if r.Path == "/products/v4/search/keyword" {
			var b map[string]any
			if err := json.Unmarshal([]byte(r.Body), &b); err != nil {
				t.Fatal(err)
			}
			searchBodies = append(searchBodies, b)
		}
	}
	if len(searchBodies) != 2 {
		t.Fatalf("made %d keyword searches, want 2 (discovery then filtered)", len(searchBodies))
	}

	filters, ok := searchBodies[1]["FilterOptionsRequest"].(map[string]any)
	if !ok {
		t.Fatalf("filtered search had no FilterOptionsRequest: %v", searchBodies[1])
	}
	pfr, ok := filters["ParameterFilterRequest"].(map[string]any)
	if !ok {
		t.Fatalf("no ParameterFilterRequest: %v", filters)
	}

	// DigiKey only honors parametric filters alongside the owning category.
	catFilter, ok := pfr["CategoryFilter"].(map[string]any)
	if !ok || catFilter["Id"] != "60" {
		t.Errorf("ParameterFilterRequest.CategoryFilter = %v, want the inferred category 60", pfr["CategoryFilter"])
	}
	if cf, ok := filters["CategoryFilter"].([]any); !ok || len(cf) != 1 {
		t.Errorf("top-level CategoryFilter = %v, want the category scoped explicitly too", filters["CategoryFilter"])
	}

	parameterFilters, ok := pfr["ParameterFilters"].([]any)
	if !ok || len(parameterFilters) != 2 {
		t.Fatalf("ParameterFilters = %v, want two entries", pfr["ParameterFilters"])
	}

	// Names were resolved to the ids DigiKey actually accepts.
	first := parameterFilters[0].(map[string]any)
	if first["ParameterId"] != float64(2049) {
		t.Errorf("ParameterId = %v, want 2049 resolved from \"Capacitance\"", first["ParameterId"])
	}
	values := first["FilterValues"].([]any)
	if len(values) != 1 || values[0].(map[string]any)["Id"] != "u0.1" {
		t.Errorf("FilterValues = %v, want the id resolved from \"0.1 µF\"", values)
	}

	second := parameterFilters[1].(map[string]any)
	if second["ParameterId"] != float64(1291) {
		t.Errorf("second ParameterId = %v, want 1291", second["ParameterId"])
	}
}

func TestSearchParamAcceptsMultipleValues(t *testing.T) {
	m := newMockDigiKey(t)
	calls := 0
	m.routes["POST /products/v4/search/keyword"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(facetResponseBody))
			return
		}
		_, _ = w.Write([]byte(searchResponseBody))
	}

	res := run(t, m, "search", "capacitor", "--param", "Tolerance=±10%,±5%")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var last map[string]any
	for _, r := range m.requests {
		if r.Path == "/products/v4/search/keyword" {
			_ = json.Unmarshal([]byte(r.Body), &last)
		}
	}
	pfr := last["FilterOptionsRequest"].(map[string]any)["ParameterFilterRequest"].(map[string]any)
	values := pfr["ParameterFilters"].([]any)[0].(map[string]any)["FilterValues"].([]any)
	// Several values on one parameter are OR-ed by DigiKey.
	if len(values) != 2 {
		t.Errorf("FilterValues = %v, want both tolerance values", values)
	}
}

func TestSearchParamUnknownValueListsAvailable(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "search", "capacitor", "--param", "Capacitance=47 farads")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "0.1 µF") {
		t.Errorf("error should list the values that do exist:\n%s", res.Stderr)
	}
}

func TestSearchParamMalformed(t *testing.T) {
	for _, arg := range []string{"NoEqualsSign", "=value", "Capacitance="} {
		res := run(t, nil, "search", "cap", "--param", arg)
		if res.Code != ExitUsage {
			t.Errorf("--param %q exit code = %d, want %d", arg, res.Code, ExitUsage)
		}
	}
}

func TestSearchParamWithNoFacetsIsUsageError(t *testing.T) {
	m := newMockDigiKey(t)
	// A response with no ParametricFilters at all.
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, `{"ProductsCount":0,"Products":[]}`)

	res := run(t, m, "search", "gibberish", "--param", "Capacitance=0.1 µF")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "no parametric filters") {
		t.Errorf("error should explain there was nothing to match against:\n%s", res.Stderr)
	}
}

func TestParseParamSpec(t *testing.T) {
	tests := []struct {
		name       string
		arg        string
		wantKey    string
		wantValues []string
		wantErr    bool
	}{
		{"single value", "Capacitance=0.1 µF", "Capacitance", []string{"0.1 µF"}, false},
		{"multiple values", "Tolerance=±10%,±5%", "Tolerance", []string{"±10%", "±5%"}, false},
		{"whitespace trimmed", "  Tolerance = ±10% , ±5% ", "Tolerance", []string{"±10%", "±5%"}, false},
		{"numeric id key", "2049=u0.1", "2049", []string{"u0.1"}, false},
		// Values can legitimately contain '=', so only the first one splits.
		{"equals inside the value", "Tolerance==0.1%", "Tolerance", []string{"=0.1%"}, false},
		{"no separator", "Capacitance", "", nil, true},
		{"empty key", "=0.1 µF", "", nil, true},
		{"empty value", "Capacitance=", "", nil, true},
		{"empty argument", "   ", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseParamSpec(tt.arg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseParamSpec(%q) error = %v, wantErr %v", tt.arg, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.Key != tt.wantKey {
				t.Errorf("key = %q, want %q", got.Key, tt.wantKey)
			}
			if len(got.Values) != len(tt.wantValues) {
				t.Fatalf("values = %v, want %v", got.Values, tt.wantValues)
			}
			for i := range got.Values {
				if got.Values[i] != tt.wantValues[i] {
					t.Errorf("values[%d] = %q, want %q", i, got.Values[i], tt.wantValues[i])
				}
			}
		})
	}
}

func TestMergeParamSpecs(t *testing.T) {
	// Repeating a flag is the escape hatch for values containing a comma, so
	// the repeats have to collapse into one constraint.
	got := mergeParamSpecs([]ParamSpec{
		{Key: "Tolerance", Values: []string{"±10%"}},
		{Key: "Capacitance", Values: []string{"0.1 µF"}},
		{Key: "tolerance", Values: []string{"±5%"}},
	})
	if len(got) != 2 {
		t.Fatalf("got %d specs, want 2", len(got))
	}
	if got[0].Key != "Tolerance" || len(got[0].Values) != 2 {
		t.Errorf("first spec = %+v, want the two tolerance values merged", got[0])
	}
	// Order of first appearance is preserved so error messages stay predictable.
	if got[1].Key != "Capacitance" {
		t.Errorf("second spec = %+v, want Capacitance", got[1])
	}
}

// testFacets is the parsed form of facetResponseBody, for unit-level tests.
func testFacets(t *testing.T) FiltersResult {
	t.Helper()
	var resp digikey.KeywordResponse
	if err := json.Unmarshal([]byte(facetResponseBody), &resp); err != nil {
		t.Fatal(err)
	}
	return buildFiltersResult("test", resp.ProductsCount, resp.FilterOptions)
}

func TestMatchFacetValue(t *testing.T) {
	result := testFacets(t)
	capacitance := result.Parameters[0]

	tests := []struct {
		name    string
		want    string
		wantID  string
		wantErr bool
	}{
		{"exact name", "0.1 µF", "u0.1", false},
		{"case insensitive", "0.1 ΜF", "u0.1", false},
		{"raw value id", "u1.0", "u1.0", false},
		{"unique substring", "10 µF", "u10", false},
		{"no match", "47 F", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := matchFacetValue(capacitance, tt.want)
			if (err != nil) != tt.wantErr {
				t.Fatalf("matchFacetValue(%q) error = %v, wantErr %v", tt.want, err, tt.wantErr)
			}
			if err == nil && got != tt.wantID {
				t.Errorf("matchFacetValue(%q) = %q, want %q", tt.want, got, tt.wantID)
			}
		})
	}
}

func TestMatchFacetValueExactBeatsSubstring(t *testing.T) {
	result := testFacets(t)
	capacitance := result.Parameters[0]

	// "1 µF" is a substring of "0.1 µF" and of "10 µF", but it exactly names one
	// value, so it must not be reported as ambiguous.
	got, err := matchFacetValue(capacitance, "1 µF")
	if err != nil {
		t.Fatalf("matchFacetValue() error = %v", err)
	}
	if got != "u1.0" {
		t.Errorf("matchFacetValue(\"1 µF\") = %q, want u1.0 (the exact match)", got)
	}
}

func TestMatchFacetValueHandlesValuesContainingCommas(t *testing.T) {
	result := testFacets(t)
	tempco := result.Parameters[2]

	// "C0G, NP0" contains a comma, which is why repeating --param exists.
	got, err := matchFacetValue(tempco, "C0G, NP0")
	if err != nil {
		t.Fatalf("matchFacetValue() error = %v", err)
	}
	if got != "c0g" {
		t.Errorf("matchFacetValue(\"C0G, NP0\") = %q, want c0g", got)
	}
}

func TestResolveParamSpecsInfersCategory(t *testing.T) {
	result := testFacets(t)

	filters, categoryID, err := resolveParamSpecs([]ParamSpec{
		{Key: "Capacitance", Values: []string{"0.1 µF"}},
		{Key: "Temperature Coefficient", Values: []string{"X7R"}},
	}, result)
	if err != nil {
		t.Fatalf("resolveParamSpecs() error = %v", err)
	}
	if len(filters) != 2 {
		t.Fatalf("got %d parametric filters, want 2", len(filters))
	}
	// The parameters carry their own category, which is more precise than the
	// keyword-derived guess.
	if categoryID != 60 {
		t.Errorf("categoryID = %d, want 60", categoryID)
	}
	if filters[0].ParameterID != 2049 || filters[0].FilterValues[0].ID != "u0.1" {
		t.Errorf("first filter = %+v", filters[0])
	}
}

func TestResolveParamSpecsRejectsCrossCategoryParameters(t *testing.T) {
	result := testFacets(t)
	// Simulate a query whose facets span two categories.
	result.Parameters = append(result.Parameters, ParameterFacet{
		ParameterID:   999,
		ParameterName: "Inductance",
		CategoryID:    70,
		CategoryName:  "Inductors",
		Values:        []ParameterFacetValue{{ValueID: "i1", ValueName: "1 µH"}},
	})

	_, _, err := resolveParamSpecs([]ParamSpec{
		{Key: "Capacitance", Values: []string{"0.1 µF"}},
		{Key: "Inductance", Values: []string{"1 µH"}},
	}, result)
	// DigiKey scopes parameter ids to one category, so this cannot be expressed.
	if err == nil {
		t.Fatal("resolveParamSpecs() error = nil, want a cross-category rejection")
	}
	if !strings.Contains(err.Error(), "--category") {
		t.Errorf("error = %q, want it to suggest --category", err)
	}
}

func TestBestCategoryPicksHighestScore(t *testing.T) {
	facets := digikey.FilterOptions{TopCategories: []digikey.TopCategory{
		{Category: digikey.TopCategoryNode{ID: 61, Name: "Film"}, Score: 0.21},
		{Category: digikey.TopCategoryNode{ID: 60, Name: "Ceramic"}, Score: 0.98},
	}}
	got, ok := facets.BestCategory()
	if !ok {
		t.Fatal("BestCategory() ok = false")
	}
	if got.ID != 60 {
		t.Errorf("BestCategory() = %+v, want the highest-scoring category", got)
	}
}

func TestBestCategoryEmpty(t *testing.T) {
	if _, ok := (digikey.FilterOptions{}).BestCategory(); ok {
		t.Error("BestCategory() ok = true for a response with no category facet")
	}
	// A malformed entry with no id must not be chosen.
	facets := digikey.FilterOptions{TopCategories: []digikey.TopCategory{{Score: 1.0}}}
	if _, ok := facets.BestCategory(); ok {
		t.Error("BestCategory() ok = true for an entry with no category id")
	}
}

func TestSummarizeValuesMarksRangeEntries(t *testing.T) {
	result := testFacets(t)
	got := summarizeValues(result.Parameters[0], 0)
	// The synthetic Min/Max entries are not discrete choices and must be
	// distinguishable from ordinary values.
	if !strings.Contains(got, "Min:1 pF") {
		t.Errorf("summarizeValues() = %q, want the range entry labeled", got)
	}
}

func TestFiltersRejectsBadValuesFlag(t *testing.T) {
	res := run(t, nil, "filters", "cap", "--values", "0")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

func TestFiltersParametersIsAlwaysAnArray(t *testing.T) {
	// DigiKey omits ParametricFilters whenever the result set does not resolve
	// to a single category, which a broad query does routinely. guide.go
	// documents parameters as an array, so the empty case must be [] and never
	// null -- a caller testing len() on it should not have to handle both.
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK,
		`{"ProductsCount":23739,"Products":[],"FilterOptions":{"Manufacturers":[{"Id":399,"Value":"KEMET","ProductCount":5615}]}}`)

	res := run(t, m, "filters", "0603 X7R ceramic capacitor")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var raw map[string]json.RawMessage
	res.JSON(t, &raw)
	got, ok := raw["parameters"]
	if !ok {
		t.Fatal("filters JSON has no parameters key")
	}
	if string(got) != "[]" {
		t.Errorf("parameters = %s, want []", got)
	}
}

func TestFiltersParameterValuesIsAlwaysAnArray(t *testing.T) {
	// Same contract one level down. guide.go documents each parameter as
	// carrying a "values" array, so a facet DigiKey returns with no
	// FilterValues has to emit [] — a caller that just checked parameters was
	// non-empty should not then find null inside it.
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK,
		`{"ProductsCount":4210,"Products":[],"FilterOptions":{"ParametricFilters":[
		  {"Category":{"Id":60,"Value":"Ceramic Capacitors"},
		   "ParameterId":2049,"ParameterName":"Capacitance","ParameterType":"UnitOfMeasure",
		   "FilterValues":[]}]}}`)

	res := run(t, m, "filters", "0603 X7R ceramic capacitor")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var raw struct {
		Parameters []map[string]json.RawMessage `json:"parameters"`
	}
	res.JSON(t, &raw)
	if len(raw.Parameters) != 1 {
		t.Fatalf("got %d parameters, want 1", len(raw.Parameters))
	}
	got, ok := raw.Parameters[0]["values"]
	if !ok {
		t.Fatal("parameter has no values key")
	}
	if string(got) != "[]" {
		t.Errorf("values = %s, want []", got)
	}
}

// The packaging facet is how a caller keeps reels out of a search they intend
// to hand-assemble. There is no endpoint listing pack types, so the ids come
// from a discovery search on the same keywords.
func TestSearchResolvesPackagingNameToID(t *testing.T) {
	for _, value := range []string{"Cut Tape", "CT", "cut tape (ct)"} {
		t.Run(value, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

			res := run(t, m, "search", "capacitor", "--packaging", value)
			if res.Code != ExitOK {
				t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
			}

			// The last keyword call is the search itself; the first was the
			// facet discovery that resolved the name.
			var body map[string]any
			for _, r := range m.requests {
				if r.Path == "/products/v4/search/keyword" {
					_ = json.Unmarshal([]byte(r.Body), &body)
				}
			}
			filters, ok := body["FilterOptionsRequest"].(map[string]any)
			if !ok {
				t.Fatalf("no FilterOptionsRequest in %v", body)
			}
			pkg, ok := filters["PackagingFilter"].([]any)
			if !ok || len(pkg) != 1 {
				t.Fatalf("PackagingFilter = %v, want one entry", filters["PackagingFilter"])
			}
			if id := pkg[0].(map[string]any)["Id"]; id != "2" {
				t.Errorf("packaging id = %v, want \"2\" for %q", id, value)
			}
		})
	}
}

func TestSearchNumericPackagingSkipsDiscovery(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "search", "capacitor", "--packaging", "3")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	// A numeric id needs no facet lookup, so the search is the only call.
	searches := 0
	for _, r := range m.requests {
		if r.Path == "/products/v4/search/keyword" {
			searches++
		}
	}
	if searches != 1 {
		t.Errorf("made %d keyword calls for a numeric id, want 1", searches)
	}
}

func TestSearchUnknownPackagingIsUsageError(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, facetResponseBody)

	res := run(t, m, "search", "capacitor", "--packaging", "Sausage")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	// Naming the value and where to look is what lets a caller recover without
	// a second round trip.
	if !strings.Contains(res.Stderr, "Sausage") || !strings.Contains(res.Stderr, "dk filters") {
		t.Errorf("error should name the value and point at `dk filters`:\n%s", res.Stderr)
	}
}
