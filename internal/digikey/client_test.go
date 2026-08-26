package digikey

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// staticTokens is a TokenSource that returns fixed values and records whether a
// user token was demanded.
type staticTokens struct {
	app           string
	user          string
	userErr       error
	requestedUser bool
}

func (s *staticTokens) Token(_ context.Context, requireUser bool) (string, error) {
	if requireUser {
		s.requestedUser = true
		if s.userErr != nil {
			return "", s.userErr
		}
		return s.user, nil
	}
	return s.app, nil
}

// capture records the last request a test server received.
type capture struct {
	Method string
	Path   string
	// RawURI is the undecoded request target, which is where percent-encoding
	// of path segments is actually observable (r.URL.Path is already decoded).
	RawURI  string
	Query   string
	Headers http.Header
	Body    []byte
}

// newTestClient spins up a server returning the given status and body, and
// returns a Client pointed at it plus the capture buffer.
func newTestClient(t *testing.T, status int, body string, opts ...func(*Options)) (*Client, *capture) {
	t.Helper()
	cap := &capture{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.Method = r.Method
		cap.Path = r.URL.Path
		cap.RawURI = r.RequestURI
		cap.Query = r.URL.RawQuery
		cap.Headers = r.Header.Clone()
		cap.Body = b

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	o := Options{
		BaseURL:  srv.URL,
		ClientID: "test-client-id",
		Locale:   Locale{Site: "US", Language: "en", Currency: "USD"},
		Tokens:   &staticTokens{app: "app-tok", user: "user-tok"},
	}
	for _, opt := range opts {
		opt(&o)
	}

	client, err := New(o)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client, cap
}

func TestNewValidatesOptions(t *testing.T) {
	tokens := &staticTokens{}
	tests := []struct {
		name string
		opts Options
	}{
		{"missing base url", Options{ClientID: "id", Tokens: tokens}},
		{"missing client id", Options{BaseURL: "https://x", Tokens: tokens}},
		{"missing token source", Options{BaseURL: "https://x", ClientID: "id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Error("New() error = nil, want a validation error")
			}
		})
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c, err := New(Options{BaseURL: "https://api.digikey.com/", ClientID: "id", Tokens: &staticTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "https://api.digikey.com" {
		t.Errorf("baseURL = %q, want the trailing slash removed", c.baseURL)
	}
}

func TestRequestHeaders(t *testing.T) {
	client, cap := newTestClient(t, http.StatusOK, `{"Products":[]}`, func(o *Options) {
		o.AccountID = "9988"
		o.UserAgent = "dk/1.2.3"
	})

	if _, err := client.KeywordSearch(context.Background(), KeywordRequest{Keywords: "resistor"}); err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}

	want := map[string]string{
		"Authorization":             "Bearer app-tok",
		"X-DIGIKEY-Client-Id":       "test-client-id",
		"X-DIGIKEY-Locale-Site":     "US",
		"X-DIGIKEY-Locale-Language": "en",
		"X-DIGIKEY-Locale-Currency": "USD",
		"X-DIGIKEY-Account-Id":      "9988",
		"Content-Type":              "application/json",
		"Accept":                    "application/json",
		"User-Agent":                "dk/1.2.3",
	}
	for k, v := range want {
		if got := cap.Headers.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

func TestRequestOmitsLocaleHeadersWhenUnset(t *testing.T) {
	client, cap := newTestClient(t, http.StatusOK, `{}`, func(o *Options) {
		o.Locale = Locale{}
		o.AccountID = ""
	})

	if _, err := client.KeywordSearch(context.Background(), KeywordRequest{Keywords: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"X-DIGIKEY-Locale-Site", "X-DIGIKEY-Locale-Language", "X-DIGIKEY-Locale-Currency", "X-DIGIKEY-Account-Id"} {
		if got := cap.Headers.Get(h); got != "" {
			t.Errorf("header %s = %q, want it omitted when unconfigured", h, got)
		}
	}
}

func TestKeywordSearchRequestBody(t *testing.T) {
	client, cap := newTestClient(t, http.StatusOK, `{"Products":[],"ProductsCount":0}`)

	req := KeywordRequest{
		Keywords: "0.1uF 0603",
		Limit:    25,
		Offset:   50,
		FilterOptionsRequest: &FilterOptionsRequest{
			SearchOptions:            []string{SearchOptionInStock},
			MinimumQuantityAvailable: 100,
		},
		SortOptions: &SortOptions{Field: "Price", SortOrder: "Ascending"},
	}
	if _, err := client.KeywordSearch(context.Background(), req); err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}

	if cap.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", cap.Method)
	}
	if cap.Path != "/products/v4/search/keyword" {
		t.Errorf("path = %q, want /products/v4/search/keyword", cap.Path)
	}

	// DigiKey's schema is PascalCase; a lowercase key would be silently ignored.
	var body map[string]any
	if err := json.Unmarshal(cap.Body, &body); err != nil {
		t.Fatalf("request body is not valid json: %v", err)
	}
	if body["Keywords"] != "0.1uF 0603" {
		t.Errorf("Keywords = %v, want %q", body["Keywords"], "0.1uF 0603")
	}
	if body["Limit"] != float64(25) || body["Offset"] != float64(50) {
		t.Errorf("Limit/Offset = %v/%v, want 25/50", body["Limit"], body["Offset"])
	}
	filters, ok := body["FilterOptionsRequest"].(map[string]any)
	if !ok {
		t.Fatalf("FilterOptionsRequest missing from body %s", cap.Body)
	}
	if filters["MinimumQuantityAvailable"] != float64(100) {
		t.Errorf("MinimumQuantityAvailable = %v, want 100", filters["MinimumQuantityAvailable"])
	}
}

func TestKeywordSearchOmitsEmptyFields(t *testing.T) {
	client, cap := newTestClient(t, http.StatusOK, `{}`)

	if _, err := client.KeywordSearch(context.Background(), KeywordRequest{Keywords: "x"}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(cap.Body, &body); err != nil {
		t.Fatal(err)
	}
	// Sending Limit:0 would ask DigiKey for zero results.
	for _, key := range []string{"Limit", "Offset", "FilterOptionsRequest", "SortOptions"} {
		if _, present := body[key]; present {
			t.Errorf("body includes %q when unset; it should be omitted", key)
		}
	}
}

func TestKeywordSearchDecodesResponse(t *testing.T) {
	body := `{
	  "ProductsCount": 1234,
	  "SearchLocaleUsed": {"Site":"US","Language":"en","Currency":"USD"},
	  "Products": [{
	    "ManufacturerProductNumber": "GRM188R71C104KA01D",
	    "Manufacturer": {"Id": 2359, "Name": "Murata Electronics"},
	    "Description": {"ProductDescription": "CAP CER 0.1UF 16V X7R 0603"},
	    "QuantityAvailable": 250000,
	    "UnitPrice": 0.01,
	    "ProductStatus": {"Id": 0, "Status": "Active"},
	    "DatasheetUrl": "https://example.com/ds.pdf",
	    "ProductVariations": [
	      {"DigiKeyProductNumber":"490-1532-1-ND","PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
	       "QuantityAvailableforPackageType":250000,"MinimumOrderQuantity":1,
	       "StandardPricing":[{"BreakQuantity":1,"UnitPrice":0.10,"TotalPrice":0.10},
	                          {"BreakQuantity":100,"UnitPrice":0.02,"TotalPrice":2.00}]}
	    ]
	  }]
	}`
	client, _ := newTestClient(t, http.StatusOK, body)

	resp, err := client.KeywordSearch(context.Background(), KeywordRequest{Keywords: "0.1uF"})
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}
	if resp.ProductsCount != 1234 {
		t.Errorf("ProductsCount = %d, want 1234", resp.ProductsCount)
	}
	if len(resp.Products) != 1 {
		t.Fatalf("got %d products, want 1", len(resp.Products))
	}

	p := resp.Products[0]
	if p.Manufacturer.Name != "Murata Electronics" {
		t.Errorf("Manufacturer = %q, want %q", p.Manufacturer.Name, "Murata Electronics")
	}
	if p.Description.ProductDescription != "CAP CER 0.1UF 16V X7R 0603" {
		t.Errorf("Description = %q", p.Description.ProductDescription)
	}
	if got := p.DigiKeyPartNumber(); got != "490-1532-1-ND" {
		t.Errorf("DigiKeyPartNumber() = %q, want %q", got, "490-1532-1-ND")
	}
	if resp.SearchLocaleUsed.Currency != "USD" {
		t.Errorf("SearchLocaleUsed.Currency = %q, want USD", resp.SearchLocaleUsed.Currency)
	}
}

func TestProductVariationStockHandlesBothSpellings(t *testing.T) {
	tests := []struct {
		name string
		v    ProductVariation
		want int
	}{
		{"package-type spelling", ProductVariation{QuantityAvailableForPackageType: 42}, 42},
		{"legacy spelling", ProductVariation{QuantityAvailable: 17}, 17},
		{"package-type wins", ProductVariation{QuantityAvailableForPackageType: 42, QuantityAvailable: 17}, 42},
		{"neither", ProductVariation{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.Stock(); got != tt.want {
				t.Errorf("Stock() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPrimaryVariationPrefersCheapestInStock(t *testing.T) {
	p := Product{ProductVariations: []ProductVariation{
		{DigiKeyProductNumber: "OUT-OF-STOCK", StandardPricing: []PriceBreak{{BreakQuantity: 1, UnitPrice: 0.01}}},
		{DigiKeyProductNumber: "EXPENSIVE", QuantityAvailableForPackageType: 10, StandardPricing: []PriceBreak{{BreakQuantity: 1, UnitPrice: 5.00}}},
		{DigiKeyProductNumber: "CHEAP", QuantityAvailableForPackageType: 10, StandardPricing: []PriceBreak{{BreakQuantity: 1, UnitPrice: 0.50}}},
	}}

	got, ok := p.PrimaryVariation()
	if !ok {
		t.Fatal("PrimaryVariation() ok = false, want true")
	}
	// Cheapest wins, but only among variations that are actually in stock.
	if got.DigiKeyProductNumber != "CHEAP" {
		t.Errorf("PrimaryVariation() = %q, want CHEAP", got.DigiKeyProductNumber)
	}
}

func TestPrimaryVariationFallsBackWhenNothingInStock(t *testing.T) {
	p := Product{ProductVariations: []ProductVariation{
		{DigiKeyProductNumber: "FIRST"},
		{DigiKeyProductNumber: "SECOND"},
	}}
	got, ok := p.PrimaryVariation()
	if !ok || got.DigiKeyProductNumber != "FIRST" {
		t.Errorf("PrimaryVariation() = (%q, %v), want (FIRST, true)", got.DigiKeyProductNumber, ok)
	}
}

func TestPrimaryVariationEmpty(t *testing.T) {
	if _, ok := (Product{}).PrimaryVariation(); ok {
		t.Error("PrimaryVariation() ok = true for a product with no variations")
	}
	if got := (Product{}).DigiKeyPartNumber(); got != "" {
		t.Errorf("DigiKeyPartNumber() = %q, want empty", got)
	}
}

func TestPrimaryVariationUsesMyPricingWhenPresent(t *testing.T) {
	// With a 3-legged token DigiKey adds MyPricing; it must win over the list
	// price when picking the cheapest variation.
	p := Product{ProductVariations: []ProductVariation{
		{
			DigiKeyProductNumber: "A", QuantityAvailableForPackageType: 1,
			StandardPricing: []PriceBreak{{BreakQuantity: 1, UnitPrice: 1.00}},
			MyPricing:       []PriceBreak{{BreakQuantity: 1, UnitPrice: 0.10}},
		},
		{
			DigiKeyProductNumber: "B", QuantityAvailableForPackageType: 1,
			StandardPricing: []PriceBreak{{BreakQuantity: 1, UnitPrice: 0.50}},
		},
	}}
	got, _ := p.PrimaryVariation()
	if got.DigiKeyProductNumber != "A" {
		t.Errorf("PrimaryVariation() = %q, want A (its MyPricing is cheaper)", got.DigiKeyProductNumber)
	}
}

func TestProductDetailsEscapesPartNumber(t *testing.T) {
	client, cap := newTestClient(t, http.StatusOK, `{"Product":{"ManufacturerProductNumber":"X"}}`)

	// Manufacturer part numbers can contain slashes, which must go on the wire
	// percent-encoded rather than as extra path segments.
	if _, err := client.ProductDetails(context.Background(), "ABC/123"); err != nil {
		t.Fatalf("ProductDetails() error = %v", err)
	}
	want := "/products/v4/search/ABC%2F123/productdetails"
	if cap.RawURI != want {
		t.Errorf("request target = %q, want %q", cap.RawURI, want)
	}
}

func TestProductDetailsRejectsEmptyPartNumber(t *testing.T) {
	client, _ := newTestClient(t, http.StatusOK, `{}`)
	if _, err := client.ProductDetails(context.Background(), "   "); err == nil {
		t.Error("ProductDetails(\"\") error = nil, want a validation error")
	}
}

func TestAPIErrorParsing(t *testing.T) {
	body := `{
	  "StatusCode": 400,
	  "ErrorMessage": "Bad Request",
	  "ErrorDetails": "The Keywords field is required.",
	  "RequestId": "req-abc-123",
	  "ValidationErrors": [{"Field":"Keywords","Message":"The Keywords field is required."}]
	}`
	client, _ := newTestClient(t, http.StatusBadRequest, body)

	_, err := client.KeywordSearch(context.Background(), KeywordRequest{})
	if err == nil {
		t.Fatal("KeywordSearch() error = nil, want an API error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.RequestID != "req-abc-123" {
		t.Errorf("RequestID = %q, want req-abc-123", apiErr.RequestID)
	}
	if len(apiErr.ValidationErrors) != 1 || apiErr.ValidationErrors[0].Field != "Keywords" {
		t.Errorf("ValidationErrors = %+v, want the Keywords entry", apiErr.ValidationErrors)
	}
	// The rendered error should carry enough to debug without a second call.
	msg := apiErr.Error()
	for _, want := range []string{"400", "Keywords field is required", "req-abc-123"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}

func TestAPIErrorClassifiers(t *testing.T) {
	tests := []struct {
		status                              int
		notFound, unauthorized, rateLimited bool
	}{
		{http.StatusNotFound, true, false, false},
		{http.StatusUnauthorized, false, true, false},
		{http.StatusForbidden, false, true, false},
		{http.StatusTooManyRequests, false, false, true},
		{http.StatusInternalServerError, false, false, false},
	}
	for _, tt := range tests {
		e := &APIError{StatusCode: tt.status}
		if e.NotFound() != tt.notFound {
			t.Errorf("status %d: NotFound() = %v, want %v", tt.status, e.NotFound(), tt.notFound)
		}
		if e.Unauthorized() != tt.unauthorized {
			t.Errorf("status %d: Unauthorized() = %v, want %v", tt.status, e.Unauthorized(), tt.unauthorized)
		}
		if e.RateLimited() != tt.rateLimited {
			t.Errorf("status %d: RateLimited() = %v, want %v", tt.status, e.RateLimited(), tt.rateLimited)
		}
	}
}

func TestAPIErrorReadsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"ErrorMessage":"rate limited"}`)
	}))
	defer srv.Close()

	client, err := New(Options{BaseURL: srv.URL, ClientID: "id", Tokens: &staticTokens{app: "t"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.KeywordSearch(context.Background(), KeywordRequest{Keywords: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %v, want 42s", apiErr.RetryAfter)
	}
}

func TestAPIErrorFallsBackToRawBody(t *testing.T) {
	client, _ := newTestClient(t, http.StatusBadGateway, "<html>upstream is down</html>")

	_, err := client.KeywordSearch(context.Background(), KeywordRequest{Keywords: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	// Non-JSON error bodies still have to produce a useful message.
	if !strings.Contains(apiErr.Message(), "upstream is down") {
		t.Errorf("Message() = %q, want it to include the raw body", apiErr.Message())
	}
}

func TestTokenErrorPropagates(t *testing.T) {
	sentinel := errors.New("no token for you")
	client, _ := newTestClient(t, http.StatusOK, `{}`, func(o *Options) {
		o.Tokens = &staticTokens{userErr: sentinel}
	})

	_, err := client.Lists(context.Background(), 0, 0)
	if !errors.Is(err, sentinel) {
		t.Errorf("Lists() error = %v, want the token source error to propagate", err)
	}
}

func TestContextCancellationIsHonored(t *testing.T) {
	// The handler stalls long enough for the client's deadline to fire, but
	// bounded so Close never blocks the test if cancellation is not propagated
	// all the way to the server goroutine.
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// Defers run last-in-first-out, so releasing the handler is declared second
	// to guarantee it happens before Close waits on in-flight requests.
	defer srv.Close()
	defer close(release)

	client, err := New(Options{BaseURL: srv.URL, ClientID: "id", Tokens: &staticTokens{app: "t"}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := client.KeywordSearch(ctx, KeywordRequest{Keywords: "x"}); err == nil {
		t.Error("KeywordSearch() error = nil, want the context deadline to surface")
	}
}

func TestEmptyResponseBodyIsNotAnError(t *testing.T) {
	// DELETE endpoints return 204 with no body; decoding must not choke.
	client, _ := newTestClient(t, http.StatusNoContent, "")
	if err := client.DeleteList(context.Background(), "list-1"); err != nil {
		t.Errorf("DeleteList() error = %v, want nil for an empty 204 response", err)
	}
}

func TestCategoriesDecodesBothChildFieldNames(t *testing.T) {
	client, _ := newTestClient(t, http.StatusOK, `{"Categories":[
	  {"CategoryId":1,"Name":"Capacitors","Children":[{"CategoryId":11,"Name":"Ceramic"}]},
	  {"CategoryId":2,"Name":"Resistors","ChildCategories":[{"CategoryId":21,"Name":"Chip"}]}
	]}`)

	cats, err := client.Categories(context.Background())
	if err != nil {
		t.Fatalf("Categories() error = %v", err)
	}
	if len(cats) != 2 {
		t.Fatalf("got %d categories, want 2", len(cats))
	}
	// The taxonomy endpoint uses "Children"; the copy inside a Product uses
	// "ChildCategories". Children() must paper over the difference.
	if kids := cats[0].Children(); len(kids) != 1 || kids[0].Name != "Ceramic" {
		t.Errorf("cats[0].Children() = %+v, want the Ceramic node", kids)
	}
	if kids := cats[1].Children(); len(kids) != 1 || kids[0].Name != "Chip" {
		t.Errorf("cats[1].Children() = %+v, want the Chip node", kids)
	}
}

func TestCategoryUnwrapsResponse(t *testing.T) {
	client, cap := newTestClient(t, http.StatusOK, `{"Category":{"CategoryId":3,"Name":"Connectors"}}`)

	node, err := client.Category(context.Background(), 3)
	if err != nil {
		t.Fatalf("Category() error = %v", err)
	}
	if node.Name != "Connectors" {
		t.Errorf("Name = %q, want Connectors", node.Name)
	}
	if cap.Path != "/products/v4/search/categories/3" {
		t.Errorf("path = %q, want /products/v4/search/categories/3", cap.Path)
	}
}

func TestManufacturersDecodes(t *testing.T) {
	client, cap := newTestClient(t, http.StatusOK, `{"Manufacturers":[{"Id":2359,"Name":"Murata Electronics"}]}`)

	mfrs, err := client.Manufacturers(context.Background())
	if err != nil {
		t.Fatalf("Manufacturers() error = %v", err)
	}
	if len(mfrs) != 1 || mfrs[0].ID != 2359 {
		t.Errorf("Manufacturers() = %+v, want the Murata entry", mfrs)
	}
	if cap.Path != "/products/v4/search/manufacturers" {
		t.Errorf("path = %q", cap.Path)
	}
}

func TestSubstitutionsDecodes(t *testing.T) {
	client, _ := newTestClient(t, http.StatusOK, `{
	  "ProductSubstitutesCount": 1,
	  "ProductSubstitutes": [{"DigiKeyProductNumber":"111-ND","ManufacturerProductNumber":"ALT1",
	    "Manufacturer":{"Id":1,"Name":"Alt Co"},"UnitPrice":"$0.10","QuantityAvailable":50,
	    "SubstituteType":"Similar"}]
	}`)

	resp, err := client.Substitutions(context.Background(), "490-1532-1-ND")
	if err != nil {
		t.Fatalf("Substitutions() error = %v", err)
	}
	if len(resp.ProductSubstitutes) != 1 {
		t.Fatalf("got %d substitutes, want 1", len(resp.ProductSubstitutes))
	}
	// DigiKey returns UnitPrice preformatted here, unlike everywhere else.
	if resp.ProductSubstitutes[0].UnitPrice != "$0.10" {
		t.Errorf("UnitPrice = %q, want the preformatted string", resp.ProductSubstitutes[0].UnitPrice)
	}
}
