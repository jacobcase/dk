package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockDigiKey is a stand-in for the DigiKey API and its OAuth endpoint. Handlers
// are keyed by "METHOD /path"; a missing route fails the test rather than
// silently 404ing, so a command that calls an unexpected endpoint is caught.
type mockDigiKey struct {
	t        *testing.T
	server   *httptest.Server
	routes   map[string]http.HandlerFunc
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func newMockDigiKey(t *testing.T) *mockDigiKey {
	t.Helper()
	m := &mockDigiKey{t: t, routes: map[string]http.HandlerFunc{}}

	// Every command needs a token first; grant one unconditionally.
	m.routes["POST /v1/oauth2/token"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1800,"token_type":"Bearer"}`)
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.requests = append(m.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body),
		})

		key := r.Method + " " + r.URL.Path
		h, ok := m.routes[key]
		if !ok {
			m.t.Errorf("unexpected API call %s", key)
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"ErrorMessage":"no route registered in test"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(m.server.Close)
	return m
}

// handle registers a JSON response for a route.
func (m *mockDigiKey) handle(method, path string, status int, body string) {
	m.routes[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// result is the outcome of running the CLI in-process.
type result struct {
	Code   int
	Stdout string
	Stderr string
}

// JSON decodes stdout, failing the test if it is not valid JSON.
func (r result) JSON(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.Stdout), into); err != nil {
		t.Fatalf("stdout is not valid json: %v\nstdout: %s\nstderr: %s", err, r.Stdout, r.Stderr)
	}
}

// errorPayload is the structured error dk emits on stderr in JSON mode.
type errorPayload struct {
	Error struct {
		Code     string         `json:"code"`
		Message  string         `json:"message"`
		Hint     string         `json:"hint"`
		ExitCode int            `json:"exit_code"`
		Details  map[string]any `json:"details"`
	} `json:"error"`
}

// ErrorJSON decodes the structured error from stderr.
func (r result) ErrorJSON(t *testing.T) errorPayload {
	t.Helper()
	var p errorPayload
	if err := json.Unmarshal([]byte(r.Stderr), &p); err != nil {
		t.Fatalf("stderr is not a valid json error: %v\nstderr: %s", err, r.Stderr)
	}
	return p
}

// run executes dk in-process with the environment pointed at the mock server.
// Output goes to buffers, which are never terminals, so --output defaults to
// JSON exactly as it would for a program capturing dk's stdout.
func run(t *testing.T, m *mockDigiKey, args ...string) result {
	t.Helper()

	t.Setenv("DK_CONFIG_DIR", t.TempDir())
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	if m != nil {
		t.Setenv("DIGIKEY_API_BASE_URL", m.server.URL)
	}

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	return result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

// runWithStdin is run() with stdin content, for --from-json -.
func runWithStdin(t *testing.T, m *mockDigiKey, stdin string, args ...string) result {
	t.Helper()

	t.Setenv("DK_CONFIG_DIR", t.TempDir())
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	if m != nil {
		t.Setenv("DIGIKEY_API_BASE_URL", m.server.URL)
	}

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr)
	return result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

const searchResponseBody = `{
  "ProductsCount": 137,
  "SearchLocaleUsed": {"Site":"US","Language":"en","Currency":"USD"},
  "Products": [{
    "ManufacturerProductNumber": "GRM188R71C104KA01D",
    "Manufacturer": {"Id": 2359, "Name": "Murata Electronics"},
    "Description": {"ProductDescription":"CAP CER 0.1UF 16V X7R 0603","DetailedDescription":"long form"},
    "QuantityAvailable": 250000,
    "ProductStatus": {"Id":0,"Status":"Active"},
    "DatasheetUrl": "https://example.com/ds.pdf",
    "ProductUrl": "https://www.digikey.com/p/1",
    "Category": {"CategoryId":3,"Name":"Ceramic Capacitors"},
    "ProductVariations": [{
      "DigiKeyProductNumber":"490-1532-1-ND",
      "PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
      "QuantityAvailableforPackageType":250000,
      "MinimumOrderQuantity":1,
      "StandardPricing":[{"BreakQuantity":1,"UnitPrice":0.10,"TotalPrice":0.10}]
    },{
      "DigiKeyProductNumber":"490-1532-2-ND",
      "PackageType":{"Id":3,"Name":"Tape & Reel (TR)"},
      "QuantityAvailableforPackageType":250000,
      "MinimumOrderQuantity":4000,
      "StandardPricing":[{"BreakQuantity":4000,"UnitPrice":0.01,"TotalPrice":40.0}]
    }]
  }]
}`

const productDetailsBody = `{
  "SearchLocaleUsed": {"Site":"US","Language":"en","Currency":"USD"},
  "Product": {
    "ManufacturerProductNumber": "GRM188R71C104KA01D",
    "Manufacturer": {"Id":2359,"Name":"Murata Electronics"},
    "Description": {"ProductDescription":"CAP CER 0.1UF 16V X7R 0603"},
    "QuantityAvailable": 100,
    "ProductStatus": {"Id":0,"Status":"Active"},
    "Parameters": [{"ParameterId":2049,"ParameterText":"Capacitance","ValueText":"0.1 µF"}],
    "ProductVariations": [{
      "DigiKeyProductNumber":"490-1532-1-ND",
      "PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
      "QuantityAvailableforPackageType":100,
      "MinimumOrderQuantity":1,
      "StandardPricing":[{"BreakQuantity":1,"UnitPrice":0.10,"TotalPrice":0.10}]
    }]
  }
}`

func TestSearchJSONOutput(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "0.1uF", "0603")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}

	var got SearchResult
	res.JSON(t, &got)

	// Multi-word keywords are joined, so quoting is optional for the caller.
	if got.Query != "0.1uF 0603" {
		t.Errorf("query = %q, want the args joined with a space", got.Query)
	}
	if got.TotalMatches != 137 {
		t.Errorf("total_matches = %d, want 137", got.TotalMatches)
	}
	if len(got.Products) != 1 {
		t.Fatalf("got %d products, want 1", len(got.Products))
	}

	p := got.Products[0]
	// The cheapest in-stock variation wins, and its packaging-specific part
	// number is what a caller must use with `dk list add`.
	if p.DigiKeyPartNumber != "490-1532-2-ND" {
		t.Errorf("digikey_part_number = %q, want the cheapest in-stock variation", p.DigiKeyPartNumber)
	}
	if p.Manufacturer != "Murata Electronics" {
		t.Errorf("manufacturer = %q", p.Manufacturer)
	}
	if p.Currency != "USD" {
		t.Errorf("currency = %q, want the value echoed by DigiKey", p.Currency)
	}
	if !p.Orderable {
		t.Error("orderable = false for an active, in-stock part")
	}
	// The compact view drops the long description; --full restores it.
	if p.DetailedDescription != "" {
		t.Errorf("detailed_description = %q, want it omitted without --full", p.DetailedDescription)
	}
}

func TestSearchFullIncludesVariationsAndParameters(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "0.1uF", "--full")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}

	var got SearchResult
	res.JSON(t, &got)
	p := got.Products[0]
	if len(p.Variations) != 2 {
		t.Errorf("got %d variations, want 2", len(p.Variations))
	}
	if p.DetailedDescription != "long form" {
		t.Errorf("detailed_description = %q, want it present with --full", p.DetailedDescription)
	}
}

func TestSearchTableOutput(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "0.1uF", "--output", "table")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	for _, want := range []string{"DKPN", "490-1532-2-ND", "Murata", "Active"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("table output missing %q:\n%s", want, res.Stdout)
		}
	}
}

func TestSearchRequestBodyReflectsFlags(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "cap", "--limit", "5", "--offset", "10",
		"--in-stock", "--rohs", "--min-qty", "100", "--no-marketplace", "--sort", "price", "--desc")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var body map[string]any
	for _, r := range m.requests {
		if r.Path == "/products/v4/search/keyword" {
			if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
				t.Fatalf("search body is not json: %v", err)
			}
		}
	}
	if body == nil {
		t.Fatal("no keyword search request was recorded")
	}

	if body["Limit"] != float64(5) || body["Offset"] != float64(10) {
		t.Errorf("Limit/Offset = %v/%v, want 5/10", body["Limit"], body["Offset"])
	}
	filters := body["FilterOptionsRequest"].(map[string]any)
	opts := filters["SearchOptions"].([]any)
	if len(opts) != 2 {
		t.Errorf("SearchOptions = %v, want InStock and RohsCompliant", opts)
	}
	if filters["MinimumQuantityAvailable"] != float64(100) {
		t.Errorf("MinimumQuantityAvailable = %v, want 100", filters["MinimumQuantityAvailable"])
	}
	if filters["MarketPlaceFilter"] != "ExcludeMarketPlace" {
		t.Errorf("MarketPlaceFilter = %v", filters["MarketPlaceFilter"])
	}
	sort := body["SortOptions"].(map[string]any)
	if sort["Field"] != "Price" || sort["SortOrder"] != "Descending" {
		t.Errorf("SortOptions = %v, want Price/Descending", sort)
	}
}

func TestSearchRejectsOutOfRangeLimit(t *testing.T) {
	for _, limit := range []string{"0", "51", "-1"} {
		res := run(t, nil, "search", "cap", "--limit", limit)
		if res.Code != ExitUsage {
			t.Errorf("--limit %s exit code = %d, want %d", limit, res.Code, ExitUsage)
		}
		p := res.ErrorJSON(t)
		if p.Error.Code != CodeUsage {
			t.Errorf("--limit %s error code = %q, want %q", limit, p.Error.Code, CodeUsage)
		}
	}
}

func TestSearchRejectsInvalidSortField(t *testing.T) {
	res := run(t, nil, "search", "cap", "--sort", "bogus")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", res.Code, ExitUsage)
	}
	// The message should name the valid choices so a caller can self-correct.
	if !strings.Contains(res.Stderr, "price") {
		t.Errorf("error should list valid sort fields:\n%s", res.Stderr)
	}
}

func TestSearchRequiresKeywords(t *testing.T) {
	res := run(t, nil, "search")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d for missing arguments", res.Code, ExitUsage)
	}
}

func TestSearchResolvesManufacturerNameToID(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/manufacturers", http.StatusOK,
		`{"Manufacturers":[{"Id":2359,"Name":"Murata Electronics"},{"Id":10,"Name":"Vishay"}]}`)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "cap", "--manufacturer", "murata")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var body map[string]any
	for _, r := range m.requests {
		if r.Path == "/products/v4/search/keyword" {
			_ = json.Unmarshal([]byte(r.Body), &body)
		}
	}
	filters := body["FilterOptionsRequest"].(map[string]any)
	mfrs := filters["ManufacturerFilter"].([]any)
	if len(mfrs) != 1 {
		t.Fatalf("ManufacturerFilter = %v, want one entry", mfrs)
	}
	// A caller knows "Murata", not 2359, so the name must be resolved for them.
	if id := mfrs[0].(map[string]any)["Id"]; id != "2359" {
		t.Errorf("manufacturer id = %v, want \"2359\"", id)
	}
}

func TestSearchNumericManufacturerSkipsLookup(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	// No manufacturers route is registered; calling it would fail the test.
	res := run(t, m, "search", "cap", "--manufacturer", "2359")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
}

func TestSearchUnknownManufacturerIsUsageError(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/manufacturers", http.StatusOK,
		`{"Manufacturers":[{"Id":10,"Name":"Vishay"}]}`)

	res := run(t, m, "search", "cap", "--manufacturer", "NoSuchCompany")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
	if !strings.Contains(res.Stderr, "NoSuchCompany") {
		t.Errorf("error should name the unresolved manufacturer:\n%s", res.Stderr)
	}
}

func TestSearchAmbiguousManufacturerIsRejected(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/manufacturers", http.StatusOK,
		`{"Manufacturers":[{"Id":1,"Name":"Texas Instruments"},{"Id":2,"Name":"Texas Components"}]}`)

	// Guessing between two plausible matches would silently skew results.
	res := run(t, m, "search", "opamp", "--manufacturer", "texas")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

func TestSearchRaw(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "cap", "--raw")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	// --raw must preserve DigiKey's own field names, not dk's normalized view.
	var got map[string]any
	res.JSON(t, &got)
	if _, ok := got["ProductsCount"]; !ok {
		t.Errorf("--raw output lost DigiKey's PascalCase fields:\n%s", res.Stdout)
	}
}

func TestProductDetail(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/490-1532-1-ND/productdetails", http.StatusOK, productDetailsBody)

	res := run(t, m, "product", "490-1532-1-ND")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ProductView
	res.JSON(t, &got)
	if got.DigiKeyPartNumber != "490-1532-1-ND" {
		t.Errorf("digikey_part_number = %q", got.DigiKeyPartNumber)
	}
	// In JSON mode the full view is always returned, so a caller never has to
	// re-run with --parameters or --variations.
	if len(got.Parameters) != 1 || got.Parameters[0].Name != "Capacitance" {
		t.Errorf("parameters = %+v, want the capacitance entry", got.Parameters)
	}
	if len(got.PriceBreaks) != 1 {
		t.Errorf("price_breaks = %+v, want one tier", got.PriceBreaks)
	}
}

func TestProductNotFoundExitCode(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/NOSUCHPART/productdetails", http.StatusNotFound,
		`{"StatusCode":404,"ErrorMessage":"Product not found","RequestId":"req-1"}`)

	res := run(t, m, "product", "NOSUCHPART")
	if res.Code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitNotFound, res.Stderr)
	}
	p := res.ErrorJSON(t)
	if p.Error.Code != CodeNotFound {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeNotFound)
	}
	if p.Error.ExitCode != ExitNotFound {
		t.Errorf("error.exit_code = %d, want %d", p.Error.ExitCode, ExitNotFound)
	}
	// The request id must survive into the structured error for support tickets.
	if p.Error.Details["request_id"] != "req-1" {
		t.Errorf("details.request_id = %v, want req-1", p.Error.Details["request_id"])
	}
}

func TestProductMutuallyExclusiveFlags(t *testing.T) {
	res := run(t, nil, "product", "X", "--variations", "--parameters")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d for mutually exclusive flags", res.Code, ExitUsage)
	}
}

func TestRateLimitExitCodeAndRetryAfter(t *testing.T) {
	m := newMockDigiKey(t)
	m.routes["POST /products/v4/search/keyword"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"ErrorMessage":"Rate limit exceeded"}`)
	}

	res := run(t, m, "search", "cap")
	if res.Code != ExitRateLimit {
		t.Fatalf("exit code = %d, want %d", res.Code, ExitRateLimit)
	}
	p := res.ErrorJSON(t)
	if p.Error.Code != CodeRateLimit {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeRateLimit)
	}
	if p.Error.Details["retry_after_seconds"] != float64(30) {
		t.Errorf("details.retry_after_seconds = %v, want 30", p.Error.Details["retry_after_seconds"])
	}
}

func TestUnauthorizedExitCode(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusUnauthorized,
		`{"ErrorMessage":"Bearer token is invalid"}`)

	res := run(t, m, "search", "cap")
	if res.Code != ExitAuth {
		t.Fatalf("exit code = %d, want %d", res.Code, ExitAuth)
	}
	p := res.ErrorJSON(t)
	if p.Error.Code != CodeAuth {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeAuth)
	}
	if p.Error.Hint == "" {
		t.Error("an auth failure should carry a hint describing the fix")
	}
}

func TestMissingCredentialsExitCode(t *testing.T) {
	t.Setenv("DK_CONFIG_DIR", t.TempDir())
	t.Setenv("DIGIKEY_CLIENT_ID", "")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "")

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), []string{"search", "cap"}, strings.NewReader(""), &stdout, &stderr)

	if code != ExitConfig {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, ExitConfig, stderr.String())
	}
	var p errorPayload
	if err := json.Unmarshal([]byte(stderr.String()), &p); err != nil {
		t.Fatalf("stderr is not a json error: %v\n%s", err, stderr.String())
	}
	if p.Error.Code != CodeCredentials {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeCredentials)
	}
	if !strings.Contains(p.Error.Hint, "DIGIKEY_CLIENT_ID") {
		t.Errorf("hint should name the environment variables to set: %q", p.Error.Hint)
	}
}

func TestListsRequireLoginExitCode(t *testing.T) {
	m := newMockDigiKey(t)

	// No user token is cached, so `dk list ls` must stop before making any API
	// call and tell the caller to run `dk auth login`.
	res := run(t, m, "list", "ls")
	if res.Code != ExitAuth {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitAuth, res.Stderr)
	}
	p := res.ErrorJSON(t)
	if p.Error.Code != CodeAuth {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeAuth)
	}
	if !strings.Contains(p.Error.Hint, "dk auth login") {
		t.Errorf("hint = %q, want it to name `dk auth login`", p.Error.Hint)
	}
}

func TestGuideIsPlainTextRegardlessOfFormat(t *testing.T) {
	for _, format := range []string{"json", "table", "csv"} {
		res := run(t, nil, "guide", "--output", format)
		if res.Code != ExitOK {
			t.Fatalf("guide --output %s exit code = %d", format, res.Code)
		}
		// The guide is documentation, not a result set, so it must not be
		// wrapped in the requested encoding.
		for _, want := range []string{"EXIT CODES", "auth_required", "dk auth login", "PARAMETRIC FILTERING", "--param", "DATASHEETS AND DOCUMENTS", "dk docs"} {
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("guide --output %s missing %q", format, want)
			}
		}
	}
}

func TestGuideDocumentsEveryExitCode(t *testing.T) {
	res := run(t, nil, "guide")
	// The guide is the contract an agent reads; a code missing from it is a
	// code the caller cannot handle.
	for _, code := range []string{CodeUsage, CodeAuth, CodeNotFound, CodeRateLimit, CodeAPI, CodeAmbiguous, CodeCredentials} {
		if !strings.Contains(res.Stdout, code) {
			t.Errorf("guide does not document the error code %q", code)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	res := run(t, nil, "version")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d", res.Code)
	}
	var got map[string]string
	res.JSON(t, &got)
	if got["version"] == "" {
		t.Error("version is empty")
	}
}

func TestInvalidOutputFormatIsUsageError(t *testing.T) {
	res := run(t, nil, "version", "--output", "yaml")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	res := run(t, nil, "frobnicate")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

func TestRootWithNoArgsShowsHelp(t *testing.T) {
	res := run(t, nil)
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0", res.Code)
	}
	if !strings.Contains(res.Stdout, "Available Commands") {
		t.Errorf("bare invocation should print help:\n%s", res.Stdout)
	}
}

func TestConfigSetAndShow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DK_CONFIG_DIR", dir)
	t.Setenv("DIGIKEY_CLIENT_ID", "")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "")

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), []string{"config", "set", "client_id", "written-id"},
		strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("config set exit code = %d\nstderr: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"config", "show"}, strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("config show exit code = %d\nstderr: %s", code, stderr.String())
	}

	var values map[string]string
	if err := json.Unmarshal([]byte(stdout.String()), &values); err != nil {
		t.Fatalf("config show output is not json: %v\n%s", err, stdout.String())
	}
	if values["client_id"] != "written-id" {
		t.Errorf("client_id = %q, want written-id", values["client_id"])
	}
}

func TestConfigShowMasksSecret(t *testing.T) {
	t.Setenv("DK_CONFIG_DIR", t.TempDir())
	t.Setenv("DIGIKEY_CLIENT_SECRET", "supersecretvalue")
	t.Setenv("DIGIKEY_CLIENT_ID", "id")

	var stdout, stderr strings.Builder
	if code := Execute(context.Background(), []string{"config", "show"}, strings.NewReader(""), &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "supersecretvalue") {
		t.Errorf("config show leaked the client secret:\n%s", stdout.String())
	}
}

func TestConfigSetRejectsUnknownKey(t *testing.T) {
	res := run(t, nil, "config", "set", "not_a_key", "x")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

func TestConfigSetDoesNotPersistEnvironmentSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DK_CONFIG_DIR", dir)
	t.Setenv("DIGIKEY_CLIENT_SECRET", "env-only-secret")
	t.Setenv("DIGIKEY_CLIENT_ID", "env-id")

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), []string{"config", "set", "environment", "sandbox"},
		strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Setting an unrelated key must not sweep an environment-only secret onto
	// disk as a side effect.
	if strings.Contains(string(raw), "env-only-secret") {
		t.Errorf("config file captured a secret that was only in the environment:\n%s", raw)
	}
}

func TestAuthStatusReportsNotLoggedIn(t *testing.T) {
	res := run(t, nil, "auth", "status")
	if res.Code != ExitOK {
		t.Fatalf("auth status exit code = %d, want 0: it must report state, not fail\nstderr: %s", res.Code, res.Stderr)
	}

	var got AuthStatus
	res.JSON(t, &got)
	if !got.ClientIDSet || !got.ClientSecretSet {
		t.Errorf("credentials should be reported as set: %+v", got)
	}
	// This is the field an agent checks before attempting list work.
	if got.UserLoggedIn {
		t.Error("user_logged_in = true with no cached token")
	}
}

func TestAuthLogoutSucceedsWithoutToken(t *testing.T) {
	res := run(t, nil, "auth", "logout")
	if res.Code != ExitOK {
		t.Errorf("auth logout exit code = %d, want 0 even with nothing cached\nstderr: %s", res.Code, res.Stderr)
	}
}
