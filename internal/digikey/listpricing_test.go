package digikey

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A quantity line carries every pack option DigiKey priced and names the one
// that applies. Taking the first priced option instead quotes a reel price for
// a cut-tape line, and that number is what a human sees in the BOM total.
func TestSelectedPackOptionHonorsSelectedPackType(t *testing.T) {
	tests := []struct {
		name     string
		quantity ListPartQuantity
		wantPack string
		wantUnit float64
		wantOK   bool
	}{
		{
			name: "selected pack type wins over an earlier priced option",
			quantity: ListPartQuantity{
				QuantityRequested: 10,
				SelectedPackType:  "Cut Tape",
				PackOptions: []ListPackOption{
					{PackType: "Digi-Reel", CalculatedUnitPrice: 0.10, ExtendedPrice: 8.00},
					{PackType: "Cut Tape", CalculatedUnitPrice: 0.048, ExtendedPrice: 0.48},
				},
			},
			wantPack: "Cut Tape", wantUnit: 0.048, wantOK: true,
		},
		{
			name: "match is case- and space-insensitive",
			quantity: ListPartQuantity{
				SelectedPackType: "  tape & reel  ",
				PackOptions: []ListPackOption{
					{PackType: "Cut Tape", CalculatedUnitPrice: 0.05, ExtendedPrice: 0.50},
					{PackType: "Tape & Reel", CalculatedUnitPrice: 0.01, ExtendedPrice: 40.0},
				},
			},
			wantPack: "Tape & Reel", wantUnit: 0.01, wantOK: true,
		},
		{
			name: "no selected pack type falls back to the first priced option",
			quantity: ListPartQuantity{
				PackOptions: []ListPackOption{
					{PackType: "Unpriced"},
					{PackType: "Cut Tape", CalculatedUnitPrice: 0.048, ExtendedPrice: 0.48},
				},
			},
			wantPack: "Cut Tape", wantUnit: 0.048, wantOK: true,
		},
		{
			name: "selected pack type DigiKey did not price falls back",
			quantity: ListPartQuantity{
				SelectedPackType: "Bulk",
				PackOptions: []ListPackOption{
					{PackType: "Cut Tape", CalculatedUnitPrice: 0.048, ExtendedPrice: 0.48},
				},
			},
			wantPack: "Cut Tape", wantUnit: 0.048, wantOK: true,
		},
		{
			name:     "no options at all reports no selection",
			quantity: ListPartQuantity{SelectedPackType: "Cut Tape"},
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.quantity.SelectedPackOption()
			if ok != tt.wantOK {
				t.Fatalf("SelectedPackOption() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.PackType != tt.wantPack {
				t.Errorf("pack type = %q, want %q (wrong option means a wrong price in the BOM)",
					got.PackType, tt.wantPack)
			}
			if got.CalculatedUnitPrice != tt.wantUnit {
				t.Errorf("unit price = %v, want %v", got.CalculatedUnitPrice, tt.wantUnit)
			}
		})
	}
}

// RequestedQty sums every quantity line, so the money has to as well. Reporting
// one line's extended price beside every line's quantity understates the cost.
func TestListPartPricingAggregatesAllQuantityLines(t *testing.T) {
	part := ListPart{
		Quantities: []ListPartQuantity{
			{
				QuantityRequested: 10,
				SelectedPackType:  "Cut Tape",
				PackOptions: []ListPackOption{
					{PackType: "Cut Tape", CalculatedUnitPrice: 0.50, ExtendedPrice: 5.00},
				},
			},
			{
				QuantityRequested: 30,
				SelectedPackType:  "Cut Tape",
				PackOptions: []ListPackOption{
					{PackType: "Cut Tape", CalculatedUnitPrice: 0.30, ExtendedPrice: 9.00},
				},
			},
		},
	}

	if got, want := part.RequestedQty(), 40; got != want {
		t.Errorf("RequestedQty() = %d, want %d", got, want)
	}
	if got, want := part.ExtendedPrice(), 14.00; got != want {
		t.Errorf("ExtendedPrice() = %v, want %v (both lines must be counted)", got, want)
	}
	// The weighted average keeps unit_price * quantity consistent with
	// extended_price, which is what a BOM reader assumes.
	if got, want := part.UnitPrice(), 14.00/40; got != want {
		t.Errorf("UnitPrice() = %v, want %v", got, want)
	}
}

// The unit_price * quantity == extended_price invariant holds only when every
// line is priced. When one is not, RequestedQty still counts its quantity while
// ExtendedPrice cannot count its money, so the derived unit price is lower than
// any real one. This pins that behavior rather than implying the invariant is
// unconditional — the CLI surfaces the gap as unpriced_parts instead.
func TestListPartPricingWithAnUnpricedLineUnderstatesTheUnitPrice(t *testing.T) {
	part := ListPart{
		Quantities: []ListPartQuantity{
			{
				QuantityRequested: 10,
				SelectedPackType:  "Cut Tape",
				PackOptions: []ListPackOption{
					{PackType: "Cut Tape", CalculatedUnitPrice: 0.50, ExtendedPrice: 5.00},
				},
			},
			{
				// DigiKey returned the selected pack type but did not price it.
				QuantityRequested: 30,
				SelectedPackType:  "Tape & Reel",
				PackOptions: []ListPackOption{
					{PackType: "Tape & Reel"},
				},
			},
		},
	}

	if got, want := part.RequestedQty(), 40; got != want {
		t.Errorf("RequestedQty() = %d, want %d", got, want)
	}
	if got, want := part.ExtendedPrice(), 5.00; got != want {
		t.Errorf("ExtendedPrice() = %v, want %v (the unpriced line contributes nothing)", got, want)
	}
	// 5.00/40 = 0.125, well below the 0.50 the priced line actually costs.
	if got, want := part.UnitPrice(), 5.00/40; got != want {
		t.Errorf("UnitPrice() = %v, want %v", got, want)
	}
}

// A selected pack type that DigiKey returned but did not price must NOT be
// silently re-quoted at another pack type's price — that was the original
// defect. Zero here is the honest answer, and the CLI counts it as unpriced.
func TestSelectedPackOptionDoesNotSubstituteAnotherPackTypesPrice(t *testing.T) {
	q := ListPartQuantity{
		QuantityRequested: 10,
		SelectedPackType:  "Tape & Reel (TR)",
		PackOptions: []ListPackOption{
			{PackType: "Tape & Reel (TR)"}, // present, unpriced
			{PackType: "Cut Tape (CT)", CalculatedUnitPrice: 0.048, ExtendedPrice: 0.48},
		},
	}

	got, ok := q.SelectedPackOption()
	if !ok {
		t.Fatal("SelectedPackOption() ok = false, want the selected option")
	}
	if got.PackType != "Tape & Reel (TR)" {
		t.Fatalf("pack type = %q, want the selected one even though it is unpriced", got.PackType)
	}
	if got.CalculatedUnitPrice != 0 || got.ExtendedPrice != 0 {
		t.Errorf("prices = %v/%v, want 0/0 — quoting Cut Tape's price for a Tape & Reel line is the bug",
			got.CalculatedUnitPrice, got.ExtendedPrice)
	}
}

func TestListPartPricingSingleLineReportsItsOwnUnitPrice(t *testing.T) {
	part := ListPart{
		Quantities: []ListPartQuantity{{
			QuantityRequested: 10,
			SelectedPackType:  "Cut Tape",
			PackOptions: []ListPackOption{
				{PackType: "Digi-Reel", CalculatedUnitPrice: 0.10, ExtendedPrice: 8.00},
				{PackType: "Cut Tape", CalculatedUnitPrice: 0.048, ExtendedPrice: 0.48},
			},
		}},
	}
	if got, want := part.UnitPrice(), 0.048; got != want {
		t.Errorf("UnitPrice() = %v, want %v", got, want)
	}
	if got, want := part.ExtendedPrice(), 0.48; got != want {
		t.Errorf("ExtendedPrice() = %v, want %v", got, want)
	}
}

func TestListPartPricingUnresolvedIsZero(t *testing.T) {
	part := ListPart{Quantities: []ListPartQuantity{{QuantityRequested: 5}}}
	if got := part.UnitPrice(); got != 0 {
		t.Errorf("UnitPrice() = %v, want 0 for an unmatched part", got)
	}
	if got := part.ExtendedPrice(); got != 0 {
		t.Errorf("ExtendedPrice() = %v, want 0 for an unmatched part", got)
	}
}

// pagedPartsServer serves `total` parts in pages of partsPageSize, honoring
// startIndex and limit the way DigiKey does.
func pagedPartsServer(t *testing.T, total int) (*Client, *int) {
	t.Helper()
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 10 // DigiKey's undocumented default, deliberately small
		}
		end := min(start+limit, total)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"TotalParts":`+strconv.Itoa(total)+`,"PartsList":[`)
		for i := start; i < end; i++ {
			if i > start {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{"UniqueId":"uid-%d","DigiKeyPartNumber":"P-%d"}`, i, i)
		}
		_, _ = io.WriteString(w, `]}`)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{
		BaseURL:  srv.URL,
		ClientID: "cid",
		Tokens:   &staticTokens{app: "app-tok", user: "user-tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

// A part beyond the first page is otherwise indistinguishable from one that is
// absent, which is what made `dk list export` write a short BOM and
// `dk list rm` report a present part as not found.
func TestAllListPartsPagesPastTheFirstPage(t *testing.T) {
	const total = 250
	client, calls := pagedPartsServer(t, total)

	got, err := client.AllListParts(context.Background(), "list-1", Locale{})
	if err != nil {
		t.Fatalf("AllListParts() error = %v", err)
	}
	if len(got.PartsList) != total {
		t.Fatalf("returned %d parts, want %d (a truncated list silently loses BOM lines)",
			len(got.PartsList), total)
	}
	if got.TotalParts != total {
		t.Errorf("TotalParts = %d, want %d", got.TotalParts, total)
	}
	if *calls < 3 {
		t.Errorf("made %d requests, want at least 3 — it did not actually page", *calls)
	}
	// Order must be preserved and every part distinct.
	for i, p := range got.PartsList {
		if want := "uid-" + strconv.Itoa(i); p.UniqueID != want {
			t.Fatalf("part %d has unique id %q, want %q", i, p.UniqueID, want)
		}
	}
}

func TestAllListPartsEmptyListReturnsEmptySlice(t *testing.T) {
	client, _ := pagedPartsServer(t, 0)

	got, err := client.AllListParts(context.Background(), "list-1", Locale{})
	if err != nil {
		t.Fatalf("AllListParts() error = %v", err)
	}
	if got.PartsList == nil {
		t.Error("PartsList is nil; it must be an empty slice so JSON renders [] rather than null")
	}
	if len(got.PartsList) != 0 || got.TotalParts != 0 {
		t.Errorf("got %d parts / TotalParts %d, want 0 / 0", len(got.PartsList), got.TotalParts)
	}
}

// A server that ignores startIndex would otherwise loop to the page cap,
// accumulating the same page over and over.
func TestAllListPartsStopsWhenServerIgnoresStartIndex(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > maxPartPages+2 {
			t.Error("AllListParts did not terminate")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Always a full page, always the same one, with an honest total.
		_, _ = io.WriteString(w, `{"TotalParts":100,"PartsList":[`)
		for i := range partsPageSize {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{"UniqueId":"uid-%d"}`, i)
		}
		_, _ = io.WriteString(w, `]}`)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{BaseURL: srv.URL, ClientID: "cid", Tokens: &staticTokens{user: "t"}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.AllListParts(context.Background(), "list-1", Locale{})
	if err != nil {
		t.Fatalf("AllListParts() error = %v", err)
	}
	if len(got.PartsList) != partsPageSize {
		t.Errorf("returned %d parts, want %d — TotalParts should have stopped the loop after one page",
			len(got.PartsList), partsPageSize)
	}
	if calls != 1 {
		t.Errorf("made %d requests, want 1", calls)
	}
}

// --raw exists to hand back exactly what DigiKey sent. These pin that the raw
// path preserves fields the typed structs do not model — the whole reason it
// cannot be implemented by re-encoding a decoded struct.
func TestRawKeywordSearchPreservesUnmodeledFields(t *testing.T) {
	const body = `{"ProductsCount":3,"UndocumentedField":"keep me","Products":[]}`
	client, cap := newTestClient(t, http.StatusOK, body)

	got, err := client.RawKeywordSearch(context.Background(), KeywordRequest{Keywords: "cap"})
	if err != nil {
		t.Fatalf("RawKeywordSearch() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("raw payload is not valid json: %v", err)
	}
	if decoded["UndocumentedField"] != "keep me" {
		t.Errorf("raw payload lost an unmodeled field: %s", got)
	}
	if cap.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", cap.Method)
	}
}

func TestRawProductResponsePreservesUnmodeledFields(t *testing.T) {
	const body = `{"UndocumentedField":"keep me","Product":{}}`
	client, cap := newTestClient(t, http.StatusOK, body)

	got, err := client.RawProductResponse(context.Background(), "490-1532-1-ND", RawProductDetails)
	if err != nil {
		t.Fatalf("RawProductResponse() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("raw payload is not valid json: %v", err)
	}
	if decoded["UndocumentedField"] != "keep me" {
		t.Errorf("raw payload lost an unmodeled field: %s", got)
	}
	if want := "/products/v4/search/490-1532-1-ND/productdetails"; cap.Path != want {
		t.Errorf("path = %q, want %q", cap.Path, want)
	}
}

// The endpoint is an allowlist so a caller cannot use it to assemble an
// arbitrary request path.
func TestRawProductResponseRejectsUnknownEndpoint(t *testing.T) {
	client, _ := newTestClient(t, http.StatusOK, `{}`)

	for _, endpoint := range []string{"", "../../admin", "media", "productdetails/../.."} {
		if _, err := client.RawProductResponse(context.Background(), "P", endpoint); err == nil {
			t.Errorf("RawProductResponse(%q) error = nil, want a rejection", endpoint)
		}
	}
}

func TestRawProductResponseRequiresPartNumber(t *testing.T) {
	client, _ := newTestClient(t, http.StatusOK, `{}`)
	if _, err := client.RawProductResponse(context.Background(), "  ", RawProductDetails); err == nil {
		t.Error("RawProductResponse() with a blank part number error = nil, want an error")
	}
}

// cappingPartsServer serves `total` parts but never returns more than `cap`
// rows per request, whatever limit is asked for. DigiKey's real per-page cap is
// undocumented, so the pager must not assume its requested size is honored.
func cappingPartsServer(t *testing.T, total, cap int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > cap {
			limit = cap
		}
		end := min(start+limit, total)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"TotalParts":`+strconv.Itoa(total)+`,"PartsList":[`)
		for i := start; i < end; i++ {
			if i > start {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{"UniqueId":"uid-%d","DigiKeyPartNumber":"P-%d"}`, i, i)
		}
		_, _ = io.WriteString(w, `]}`)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{BaseURL: srv.URL, ClientID: "cid", Tokens: &staticTokens{user: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A short page is NOT proof the list ended. If DigiKey caps pages below
// partsPageSize, treating the first short page as the end truncates the BOM on
// request one — which is the exact bug AllListParts exists to prevent.
func TestAllListPartsSurvivesServerSidePageCap(t *testing.T) {
	for _, cap := range []int{1, 7, 50, 99} {
		t.Run(fmt.Sprintf("cap=%d", cap), func(t *testing.T) {
			const total = 150
			got, err := cappingPartsServer(t, total, cap).
				AllListParts(context.Background(), "list-1", Locale{})
			if err != nil {
				t.Fatalf("AllListParts() error = %v", err)
			}
			if len(got.PartsList) != total {
				t.Fatalf("returned %d parts, want %d — a server-side page cap silently truncated the list",
					len(got.PartsList), total)
			}
			for i, p := range got.PartsList {
				if want := "uid-" + strconv.Itoa(i); p.UniqueID != want {
					t.Fatalf("part %d = %q, want %q (pages overlapped or skipped)", i, p.UniqueID, want)
				}
			}
		})
	}
}

// A server that ignores startIndex must not produce duplicates. The earlier
// version of this test used a list that fit in one page, so it only proved the
// loop terminated — not that the result was right.
func TestAllListPartsDeduplicatesWhenServerIgnoresStartIndex(t *testing.T) {
	const total = 250 // larger than one page, so the bug would show as triplication
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > maxPartPages+2 {
			t.Error("AllListParts did not terminate")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Always the first page, whatever startIndex says.
		_, _ = io.WriteString(w, `{"TotalParts":`+strconv.Itoa(total)+`,"PartsList":[`)
		for i := range partsPageSize {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{"UniqueId":"uid-%d"}`, i)
		}
		_, _ = io.WriteString(w, `]}`)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{BaseURL: srv.URL, ClientID: "cid", Tokens: &staticTokens{user: "t"}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.AllListParts(context.Background(), "list-1", Locale{})
	if err != nil {
		t.Fatalf("AllListParts() error = %v", err)
	}
	if len(got.PartsList) != partsPageSize {
		t.Fatalf("returned %d parts, want %d — the same page was accumulated more than once",
			len(got.PartsList), partsPageSize)
	}
	seen := map[string]bool{}
	for _, p := range got.PartsList {
		if seen[p.UniqueID] {
			t.Fatalf("duplicate part %q in the result", p.UniqueID)
		}
		seen[p.UniqueID] = true
	}
	// TotalParts keeps the server's claim of 250 rather than being clamped to
	// what we managed to fetch. That is deliberate: the discrepancy between
	// TotalParts and the returned count is the only signal the caller has that
	// the server short-changed us, and `dk list show` surfaces it as
	// "Showing 100 of 250 parts". Clamping would hide a broken server.
	if got.TotalParts != total {
		t.Errorf("TotalParts = %d, want %d — the shortfall must stay visible to the caller",
			got.TotalParts, total)
	}
}

// The tests above use fixtures this codebase invented. These use a response
// captured from the live MyLists API, which contradicted two assumptions those
// fixtures encoded: SelectedPackType came back EMPTY with the selection carried
// in SelectedPackOptionIndex instead, and PackOptions was an empty array for a
// part DigiKey would not price.
func TestRealListPartsResponse(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "listparts_obsolete.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resp PartsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("the real payload does not decode into PartsResponse: %v", err)
	}

	if resp.TotalParts != 1 || len(resp.PartsList) != 1 {
		t.Fatalf("TotalParts=%d, parts=%d, want 1 and 1", resp.TotalParts, len(resp.PartsList))
	}
	p := resp.PartsList[0]

	// DigiKey resolves the requested part number to whichever variation it
	// stocks — here a -1-ND request came back as the -2-ND reel.
	if p.RequestedPartNumber != "490-1532-1-ND" || p.DigiKeyPartNumber != "490-1532-2-ND" {
		t.Errorf("requested=%q digikey=%q; the two are expected to differ in this capture",
			p.RequestedPartNumber, p.DigiKeyPartNumber)
	}
	if !p.Flags.IsMatched {
		t.Error("IsMatched=false; the part did resolve to a catalog product")
	}

	if len(p.Quantities) != 1 {
		t.Fatalf("got %d quantity lines, want 1", len(p.Quantities))
	}
	q := p.Quantities[0]

	// The finding that drove the index-first lookup: the name is blank and the
	// index carries the selection. Name matching alone could never fire here.
	if q.SelectedPackType != "" {
		t.Errorf("SelectedPackType = %q; the capture has it empty. If DigiKey now "+
			"populates it, re-check the lookup order in SelectedPackOption", q.SelectedPackType)
	}
	if q.SelectedPackOptionIndex == nil {
		t.Fatal("SelectedPackOptionIndex did not decode; it is the field DigiKey actually fills in")
	}
	if *q.SelectedPackOptionIndex != 0 {
		t.Errorf("SelectedPackOptionIndex = %d, want 0", *q.SelectedPackOptionIndex)
	}

	// An obsolete, zero-stock part gets no pack options at all, so it is
	// unpriced rather than free. The index pointing into an empty slice must
	// not panic or fabricate a price.
	if len(q.PackOptions) != 0 {
		t.Fatalf("got %d pack options, want 0 for this obsolete part", len(q.PackOptions))
	}
	if _, ok := q.SelectedPackOption(); ok {
		t.Error("SelectedPackOption() reported a selection for a line with no options")
	}
	if got := p.UnitPrice(); got != 0 {
		t.Errorf("UnitPrice() = %v, want 0", got)
	}
	if got := p.ExtendedPrice(); got != 0 {
		t.Errorf("ExtendedPrice() = %v, want 0", got)
	}
	if got := p.RequestedQty(); got != 10 {
		t.Errorf("RequestedQty() = %d, want 10", got)
	}
}

// An out-of-range or negative index must fall through rather than panic —
// DigiKey uses -1 for "nothing selected".
func TestSelectedPackOptionIndexBounds(t *testing.T) {
	idx := func(i int) *int { return &i }
	options := []ListPackOption{
		{PackType: "Cut Tape (CT)", CalculatedUnitPrice: 0.048, ExtendedPrice: 0.48},
		{PackType: "Digi-Reel", CalculatedUnitPrice: 0.10, ExtendedPrice: 1.00},
	}

	tests := []struct {
		name     string
		index    *int
		packType string
		wantPack string
	}{
		{"index selects the second option", idx(1), "", "Digi-Reel"},
		{"index 0 selects the first", idx(0), "", "Cut Tape (CT)"},
		{"negative index falls through to the name", idx(-1), "Digi-Reel", "Digi-Reel"},
		{"out-of-range index falls through to the name", idx(99), "Digi-Reel", "Digi-Reel"},
		{"absent index falls through to the name", nil, "Digi-Reel", "Digi-Reel"},
		{"no index and no name takes the first priced option", nil, "", "Cut Tape (CT)"},
		// The index wins over a name that disagrees: it is the field DigiKey
		// actually maintains.
		{"index outranks a conflicting name", idx(1), "Cut Tape (CT)", "Digi-Reel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := ListPartQuantity{
				SelectedPackOptionIndex: tt.index,
				SelectedPackType:        tt.packType,
				PackOptions:             options,
			}
			got, ok := q.SelectedPackOption()
			if !ok {
				t.Fatal("SelectedPackOption() ok = false, want a selection")
			}
			if got.PackType != tt.wantPack {
				t.Errorf("pack type = %q, want %q", got.PackType, tt.wantPack)
			}
		})
	}
}
