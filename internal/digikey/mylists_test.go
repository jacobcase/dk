package digikey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// routedClient serves a path->response table, so multi-call flows (resolve a
// list, then act on it) can be exercised end to end.
type routedClient struct {
	*Client
	requests []*capture
}

type route struct {
	status int
	body   string
}

func newRoutedClient(t *testing.T, routes map[string]route) *routedClient {
	t.Helper()
	rc := &routedClient{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.requests = append(rc.requests, &capture{
			Method: r.Method, Path: r.URL.Path, RawURI: r.RequestURI,
			Query: r.URL.RawQuery, Headers: r.Header.Clone(), Body: b,
		})

		key := r.Method + " " + r.URL.Path
		rt, ok := routes[key]
		if !ok {
			t.Errorf("unexpected request %s", key)
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"ErrorMessage":"no route in test"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rt.status)
		_, _ = io.WriteString(w, rt.body)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{
		BaseURL:  srv.URL,
		ClientID: "cid",
		Locale:   Locale{Site: "US", Language: "en", Currency: "USD"},
		Tokens:   &staticTokens{app: "app-tok", user: "user-tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rc.Client = client
	return rc
}

const twoListsBody = `[
  {"Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":3,"DateModified":"2026-08-01T10:00:00Z"},
  {"Id":"bbb-222","ListName":"Audio Amp","TotalParts":0,"DateModified":"2026-07-15T09:00:00Z"}
]`

func TestListsUsesUserToken(t *testing.T) {
	tokens := &staticTokens{app: "app-tok", user: "user-tok"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// MyLists rejects a client-credentials token, so dk must send the
		// 3-legged one.
		if got := r.Header.Get("Authorization"); got != "Bearer user-tok" {
			t.Errorf("Authorization = %q, want the user token", got)
		}
		_, _ = io.WriteString(w, twoListsBody)
	}))
	defer srv.Close()

	client, err := New(Options{BaseURL: srv.URL, ClientID: "cid", Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Lists(context.Background(), 0, 0); err != nil {
		t.Fatalf("Lists() error = %v", err)
	}
	if !tokens.requestedUser {
		t.Error("Lists() did not request a user token")
	}
}

func TestListsPagingQuery(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, twoListsBody}})

	if _, err := rc.Lists(context.Background(), 10, 25); err != nil {
		t.Fatal(err)
	}
	q := rc.requests[0].Query
	if !strings.Contains(q, "startIndex=10") || !strings.Contains(q, "limit=25") {
		t.Errorf("query = %q, want startIndex=10 and limit=25", q)
	}
}

func TestListsOmitsZeroPaging(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, twoListsBody}})

	if _, err := rc.Lists(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	// limit=0 would be read as "return nothing" rather than "use the default".
	if q := rc.requests[0].Query; q != "" {
		t.Errorf("query = %q, want it empty so DigiKey applies its defaults", q)
	}
}

func TestResolveListByID(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, twoListsBody}})

	got, err := rc.ResolveList(context.Background(), "bbb-222")
	if err != nil {
		t.Fatalf("ResolveList() error = %v", err)
	}
	if got.ListName != "Audio Amp" {
		t.Errorf("ListName = %q, want Audio Amp", got.ListName)
	}
}

func TestResolveListByName(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, twoListsBody}})

	got, err := rc.ResolveList(context.Background(), "Bench PSU rev A")
	if err != nil {
		t.Fatalf("ResolveList() error = %v", err)
	}
	if got.ID != "aaa-111" {
		t.Errorf("ID = %q, want aaa-111", got.ID)
	}
}

func TestResolveListCaseInsensitiveFallback(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, twoListsBody}})

	got, err := rc.ResolveList(context.Background(), "audio amp")
	if err != nil {
		t.Fatalf("ResolveList() error = %v", err)
	}
	if got.ID != "bbb-222" {
		t.Errorf("ID = %q, want bbb-222", got.ID)
	}
}

func TestResolveListExactMatchBeatsCaseInsensitive(t *testing.T) {
	body := `[{"Id":"1","ListName":"psu"},{"Id":"2","ListName":"PSU"}]`
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, body}})

	// Two lists differing only in case must not be ambiguous when the caller
	// typed one of them exactly.
	got, err := rc.ResolveList(context.Background(), "PSU")
	if err != nil {
		t.Fatalf("ResolveList() error = %v", err)
	}
	if got.ID != "2" {
		t.Errorf("ID = %q, want 2 (the exact-case match)", got.ID)
	}
}

func TestResolveListNotFound(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, twoListsBody}})

	_, err := rc.ResolveList(context.Background(), "Nonexistent")
	if !errors.Is(err, ErrListNotFound) {
		t.Errorf("ResolveList() error = %v, want ErrListNotFound", err)
	}
}

func TestResolveListAmbiguous(t *testing.T) {
	body := `[{"Id":"1","ListName":"project"},{"Id":"2","ListName":"PROJECT"},{"Id":"3","ListName":"other"}]`
	rc := newRoutedClient(t, map[string]route{"GET /mylists/v1/lists": {http.StatusOK, body}})

	_, err := rc.ResolveList(context.Background(), "Project")
	var ambiguous *ErrAmbiguousList
	if !errors.As(err, &ambiguous) {
		t.Fatalf("ResolveList() error = %v, want *ErrAmbiguousList", err)
	}
	if len(ambiguous.Candidate) != 2 {
		t.Errorf("got %d candidates, want 2", len(ambiguous.Candidate))
	}
	// The message must name the ids so the caller can disambiguate.
	for _, id := range []string{"1", "2"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error %q should list candidate id %q", err, id)
		}
	}
}

func TestResolveListRejectsEmptyInput(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{})
	if _, err := rc.ResolveList(context.Background(), "  "); err == nil {
		t.Error("ResolveList(\"\") error = nil, want a validation error")
	}
	if len(rc.requests) != 0 {
		t.Error("ResolveList(\"\") made a network call before validating its input")
	}
}

func TestCreateListRequestAndResponse(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"POST /mylists/v1/lists": {http.StatusOK, `"new-list-id"`},
	})

	id, err := rc.CreateList(context.Background(), CreateListRequest{
		ListName: "Bench PSU rev A",
		Tags:     []string{"project"},
		Source:   "external",
	})
	if err != nil {
		t.Fatalf("CreateList() error = %v", err)
	}
	// DigiKey returns the id as a bare JSON string, not an object.
	if id != "new-list-id" {
		t.Errorf("CreateList() = %q, want new-list-id", id)
	}

	var body map[string]any
	if err := json.Unmarshal(rc.requests[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["ListName"] != "Bench PSU rev A" {
		t.Errorf("ListName = %v, want the PascalCase field to carry the name", body["ListName"])
	}
}

func TestCreateListRejectsEmptyName(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{})
	if _, err := rc.CreateList(context.Background(), CreateListRequest{ListName: " "}); err == nil {
		t.Error("CreateList(\"\") error = nil, want a validation error")
	}
}

func TestAddPartsBuildsRequest(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"POST /mylists/v1/lists/aaa-111/parts": {http.StatusOK, `["uid-1","uid-2"]`},
	})

	ids, err := rc.AddParts(context.Background(), "aaa-111", []RequestedPart{
		{
			RequestedPartNumber: "490-1532-1-ND",
			ReferenceDesignator: "C1,C2",
			Notes:               "decoupling",
			Quantities:          []RequestedQuantity{{Quantity: 10}},
		},
		{
			RequestedPartNumber: "311-10.0KHRCT-ND",
			Quantities:          []RequestedQuantity{{Quantity: 20}},
		},
	})
	if err != nil {
		t.Fatalf("AddParts() error = %v", err)
	}
	if len(ids) != 2 || ids[0] != "uid-1" {
		t.Errorf("AddParts() = %v, want [uid-1 uid-2]", ids)
	}

	// The body must be a bare array of RequestedPart, not a wrapper object.
	var parts []map[string]any
	if err := json.Unmarshal(rc.requests[0].Body, &parts); err != nil {
		t.Fatalf("request body is not a json array: %v (%s)", err, rc.requests[0].Body)
	}
	if len(parts) != 2 {
		t.Fatalf("sent %d parts, want 2", len(parts))
	}
	if parts[0]["RequestedPartNumber"] != "490-1532-1-ND" {
		t.Errorf("RequestedPartNumber = %v", parts[0]["RequestedPartNumber"])
	}
	if parts[0]["ReferenceDesignator"] != "C1,C2" {
		t.Errorf("ReferenceDesignator = %v, want C1,C2", parts[0]["ReferenceDesignator"])
	}
	quantities, ok := parts[0]["Quantities"].([]any)
	if !ok || len(quantities) != 1 {
		t.Fatalf("Quantities = %v, want one entry", parts[0]["Quantities"])
	}
	if q := quantities[0].(map[string]any); q["Quantity"] != float64(10) {
		t.Errorf("Quantity = %v, want 10", q["Quantity"])
	}
	// The second part carried no metadata; those fields must be omitted rather
	// than sent as empty strings.
	if _, present := parts[1]["Notes"]; present {
		t.Error("empty Notes was serialized instead of omitted")
	}
}

func TestAddPartsValidatesInput(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{})

	if _, err := rc.AddParts(context.Background(), "", []RequestedPart{{RequestedPartNumber: "x"}}); err == nil {
		t.Error("AddParts with no list id error = nil, want a validation error")
	}
	if _, err := rc.AddParts(context.Background(), "aaa", nil); err == nil {
		t.Error("AddParts with no parts error = nil, want a validation error")
	}
	if len(rc.requests) != 0 {
		t.Error("AddParts made a network call despite invalid input")
	}
}

func TestListPartsQueryCarriesLocale(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/aaa-111/parts": {http.StatusOK, `{"PartsList":[],"TotalParts":0}`},
	})

	_, err := rc.ListParts(context.Background(), "aaa-111", 0, 0, Locale{Site: "DE", Language: "de", Currency: "EUR"})
	if err != nil {
		t.Fatalf("ListParts() error = %v", err)
	}

	// This endpoint takes locale as query parameters, not headers.
	q := rc.requests[0].Query
	for _, want := range []string{"countryIso=DE", "languageIso=de", "currencyIso=EUR"} {
		if !strings.Contains(q, want) {
			t.Errorf("query = %q, want it to contain %q", q, want)
		}
	}
}

func TestListPartsDecodesPricing(t *testing.T) {
	body := `{
	  "TotalParts": 1,
	  "PartsList": [{
	    "UniqueId": "uid-1",
	    "DigiKeyPartNumber": "490-1532-1-ND",
	    "ManufacturerPartNumber": "GRM188R71C104KA01D",
	    "Manufacturer": "Murata Electronics",
	    "Description": "CAP CER 0.1UF",
	    "ReferenceDesignator": "C1,C2",
	    "QuantityAvailable": 250000,
	    "PartStatus": "Active",
	    "Flags": {"IsMatched": true},
	    "Quantities": [{
	      "QuantityRequested": 10,
	      "SelectedPackType": "Cut Tape",
	      "PackOptions": [{"DigiKeyPartNumber":"490-1532-1-ND","PackType":"Cut Tape",
	                       "CalculatedUnitPrice":0.048,"ExtendedPrice":0.48}]
	    }]
	  }]
	}`
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/aaa-111/parts": {http.StatusOK, body},
	})

	resp, err := rc.ListParts(context.Background(), "aaa-111", 0, 0, Locale{})
	if err != nil {
		t.Fatalf("ListParts() error = %v", err)
	}
	if len(resp.PartsList) != 1 {
		t.Fatalf("got %d parts, want 1", len(resp.PartsList))
	}

	p := resp.PartsList[0]
	if got := p.RequestedQty(); got != 10 {
		t.Errorf("RequestedQty() = %d, want 10", got)
	}
	if got := p.UnitPrice(); got != 0.048 {
		t.Errorf("UnitPrice() = %v, want 0.048", got)
	}
	if got := p.ExtendedPrice(); got != 0.48 {
		t.Errorf("ExtendedPrice() = %v, want 0.48", got)
	}
	if !p.Flags.IsMatched {
		t.Error("IsMatched = false, want true")
	}
}

func TestListPartPricingHelpersOnUnpricedLine(t *testing.T) {
	// An unmatched part has no pack options at all; the helpers must return
	// zero rather than panic on an empty slice.
	p := ListPart{Quantities: []ListPartQuantity{{QuantityRequested: 5}}}
	if got := p.RequestedQty(); got != 5 {
		t.Errorf("RequestedQty() = %d, want 5", got)
	}
	if got := p.UnitPrice(); got != 0 {
		t.Errorf("UnitPrice() = %v, want 0", got)
	}
	if got := p.ExtendedPrice(); got != 0 {
		t.Errorf("ExtendedPrice() = %v, want 0", got)
	}

	empty := ListPart{}
	if got := empty.RequestedQty(); got != 0 {
		t.Errorf("RequestedQty() on an empty part = %d, want 0", got)
	}
}

func TestRequestedQtySumsMultipleLines(t *testing.T) {
	// DigiKey allows several quantity lines per part (e.g. split pack types).
	p := ListPart{Quantities: []ListPartQuantity{
		{QuantityRequested: 10},
		{QuantityRequested: 5},
	}}
	if got := p.RequestedQty(); got != 15 {
		t.Errorf("RequestedQty() = %d, want 15", got)
	}
}

func TestRenameListEscapesPathSegments(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"PUT /mylists/v1/lists/aaa-111/listName/New Name/v2": {http.StatusOK, ""},
	})

	// The new name goes in the path, so slashes and spaces must be escaped.
	if err := rc.RenameList(context.Background(), "aaa-111", "New Name/v2"); err != nil {
		t.Fatalf("RenameList() error = %v", err)
	}
	raw := rc.requests[0].RawURI
	if !strings.Contains(raw, "New%20Name%2Fv2") {
		t.Errorf("request target = %q, want the new name percent-encoded", raw)
	}
}

func TestRenameListValidatesInput(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{})
	if err := rc.RenameList(context.Background(), "", "x"); err == nil {
		t.Error("RenameList with no list id error = nil, want a validation error")
	}
	if err := rc.RenameList(context.Background(), "aaa", " "); err == nil {
		t.Error("RenameList with an empty name error = nil, want a validation error")
	}
}

func TestDeletePartPath(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"DELETE /mylists/v1/lists/aaa-111/parts/uid-1": {http.StatusNoContent, ""},
	})
	if err := rc.DeletePart(context.Background(), "aaa-111", "uid-1"); err != nil {
		t.Fatalf("DeletePart() error = %v", err)
	}
	if rc.requests[0].Method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", rc.requests[0].Method)
	}
}

func TestGetListDecodesRequestedParts(t *testing.T) {
	// A decode test, not a statement about the live API: PartsList is populated
	// here so the tags round-trip, and comes back empty from every real
	// response. Nothing in dk looks a part up through GetList — see its doc.
	body := `{"Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":1,
	  "Tags":["project"],
	  "PartsList":[{"UniqueId":"uid-1","RequestedPartNumber":"490-1532-1-ND",
	                "Quantities":[{"Quantity":10}]}]}`
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/aaa-111": {http.StatusOK, body},
	})

	got, err := rc.GetList(context.Background(), "aaa-111")
	if err != nil {
		t.Fatalf("GetList() error = %v", err)
	}
	if got.ListName != "Bench PSU rev A" || len(got.PartsList) != 1 {
		t.Errorf("GetList() = %+v, want one part on the named list", got)
	}
	if got.PartsList[0].Quantities[0].Quantity != 10 {
		t.Errorf("Quantity = %d, want 10", got.PartsList[0].Quantities[0].Quantity)
	}
}

func TestAllListsPagesUntilShortPage(t *testing.T) {
	// Resolving a list by name needs every list. A list on page two must not
	// be indistinguishable from one that does not exist.
	var gotQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1800}`)
			return
		}
		gotQueries = append(gotQueries, r.URL.RawQuery)

		// Lists omits startIndex when it is 0, so an absent value is page one.
		if start := r.URL.Query().Get("startIndex"); start == "" || start == "0" {
			// A full page, which must trigger another request.
			var b strings.Builder
			b.WriteString(`[`)
			for i := range listPageSize {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"Id":"id-%d","ListName":"List %d"}`, i, i)
			}
			b.WriteString(`]`)
			_, _ = io.WriteString(w, b.String())
			return
		}
		_, _ = io.WriteString(w, `[{"Id":"last","ListName":"Last List"}]`)
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, ClientID: "id", Tokens: &staticTokens{app: "a", user: "u"}})
	if err != nil {
		t.Fatal(err)
	}

	all, err := c.AllLists(context.Background())
	if err != nil {
		t.Fatalf("AllLists() error = %v", err)
	}
	if len(all) != listPageSize+1 {
		t.Errorf("AllLists() returned %d lists, want %d", len(all), listPageSize+1)
	}
	if all[len(all)-1].ID != "last" {
		t.Errorf("last list = %q, want the entry from page two", all[len(all)-1].ID)
	}
	// Three requests, not two: DigiKey documents /lists as 50 per page while
	// this asks for 100, so a short page is not evidence of the end and the
	// walk confirms with one more request that returns nothing new.
	if len(gotQueries) != 3 {
		t.Fatalf("made %d list requests (%v), want 3", len(gotQueries), gotQueries)
	}
	if !strings.Contains(gotQueries[1], "startIndex=100") {
		t.Errorf("second request query = %q, want startIndex=100", gotQueries[1])
	}
	if !strings.Contains(gotQueries[2], "startIndex=101") {
		t.Errorf("third request query = %q, want startIndex=101 (resumed from rows held)", gotQueries[2])
	}
}

func TestAllListsStopsOnAServerThatIgnoresPaging(t *testing.T) {
	// A server that returns a full page forever would otherwise loop until the
	// process ran out of memory.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1800}`)
			return
		}
		var b strings.Builder
		b.WriteString(`[`)
		for i := range listPageSize {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"Id":"id-%d"}`, i)
		}
		b.WriteString(`]`)
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, ClientID: "id", Tokens: &staticTokens{app: "a", user: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AllLists(context.Background()); err == nil {
		t.Error("AllLists() returned nil error against a server that ignores startIndex")
	}
}

func TestSuggestListNameDecodesBareString(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/validate/name/Bench PSU": {http.StatusOK, `"Bench PSU (2)"`},
	})
	got, err := rc.SuggestListName(context.Background(), "Bench PSU")
	if err != nil {
		t.Fatalf("SuggestListName() error = %v", err)
	}
	if got != "Bench PSU (2)" {
		t.Errorf("SuggestListName() = %q, want %q", got, "Bench PSU (2)")
	}
}

func TestMyListsErrorSurfacesRequestID(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/missing": {http.StatusNotFound,
			`{"StatusCode":404,"ErrorMessage":"List not found","RequestId":"req-9"}`},
	})

	_, err := rc.GetList(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if !apiErr.NotFound() {
		t.Errorf("NotFound() = false for a 404")
	}
	if apiErr.RequestID != "req-9" {
		t.Errorf("RequestID = %q, want req-9", apiErr.RequestID)
	}
}

func TestAllListsSurvivesAServerThatCapsThePage(t *testing.T) {
	// The spec documents /lists as 50 per page while AllLists asks for 100, so
	// the first response is expected to be shorter than requested. Treating
	// that as the end returned the first 50 lists and reported them complete —
	// which makes ResolveList answer not_found for a list that exists, on the
	// write paths as well as the read ones.
	const total = 120
	const serverCap = 50

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1800}`)
			return
		}
		requests++
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		end := min(start+serverCap, total)

		var b strings.Builder
		b.WriteString("[")
		for i := start; i < end; i++ {
			if i > start {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"Id":"id-%d","ListName":"List %d"}`, i, i)
		}
		b.WriteString("]")
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, ClientID: "id", Tokens: &staticTokens{app: "a", user: "u"}})
	if err != nil {
		t.Fatal(err)
	}

	all, err := c.AllLists(context.Background())
	if err != nil {
		t.Fatalf("AllLists() error = %v", err)
	}
	if len(all) != total {
		t.Fatalf("AllLists() returned %d lists, want all %d: a capped page is not the end of the list", len(all), total)
	}
	if all[total-1].ID != fmt.Sprintf("id-%d", total-1) {
		t.Errorf("last list = %q, want id-%d", all[total-1].ID, total-1)
	}
	if requests < 3 {
		t.Errorf("made %d requests, want at least 3 to walk %d lists at %d per page", requests, total, serverCap)
	}
}

func TestRequestedPartRoundTripsEverySpecField(t *testing.T) {
	// DigiKey's part update is a replace, not a patch: `dk list set` reads a
	// RequestedPart and sends it back, so any field the struct cannot hold is
	// silently reset to its zero value by an unrelated edit. This asserts the
	// struct is complete against the MyLists spec's RequestedPart.
	const stored = `{
	  "UniqueId": "u-1",
	  "PartId": 42,
	  "RequestedPartNumber": "490-1532-1-ND",
	  "OriginalPartNumber": "GRM188R71H104KA93D",
	  "ManufacturerName": "Murata",
	  "CustomerReference": "C12",
	  "ReferenceDesignator": "C1,C2",
	  "Notes": "decoupling",
	  "Attrition": 5.5,
	  "AlternateParts": ["1276-1000-1-ND"],
	  "SelectedQuantityIndex": 2,
	  "Quantities": [{"Quantity": 100}]
	}`

	var part RequestedPart
	if err := json.Unmarshal([]byte(stored), &part); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if part.SelectedQuantityIndex != 2 {
		t.Errorf("SelectedQuantityIndex = %d, want 2", part.SelectedQuantityIndex)
	}

	out, err := json.Marshal(part)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(stored), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	for key, want := range before {
		got, ok := after[key]
		if !ok {
			t.Errorf("re-encoding dropped %q: a replace would reset it to zero", key)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q = %v after round trip, want %v", key, got, want)
		}
	}
}

func TestSuggestListNameFallsBackWhenDigiKeyLacksTheRoute(t *testing.T) {
	// DigiKey answers the documented validate route with 404 "Invalid resource
	// path", which used to fail --auto-rename outright. The fallback derives a
	// free name from the account's own lists instead.
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/validate/name/Bench PSU": {
			status: http.StatusNotFound,
			body:   `{"ErrorMessage":"Invalid resource path (Requested resource is not available)"}`,
		},
		"GET /mylists/v1/lists": {status: http.StatusOK, body: `[
		  {"Id":"a","ListName":"Bench PSU"},
		  {"Id":"b","ListName":"bench psu (2)"},
		  {"Id":"c","ListName":"Unrelated"}
		]`},
	})

	got, err := rc.SuggestListName(context.Background(), "Bench PSU")
	if err != nil {
		t.Fatalf("SuggestListName() error = %v, want the local fallback to answer", err)
	}
	// (2) is taken case-insensitively, so the next free variant is (3).
	if got != "Bench PSU (3)" {
		t.Errorf("SuggestListName() = %q, want \"Bench PSU (3)\"", got)
	}
}

func TestSuggestListNameFallbackKeepsAFreeName(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/validate/name/Fresh": {
			status: http.StatusNotFound,
			body:   `{"ErrorMessage":"Invalid resource path"}`,
		},
		"GET /mylists/v1/lists": {status: http.StatusOK, body: `[{"Id":"a","ListName":"Something Else"}]`},
	})

	got, err := rc.SuggestListName(context.Background(), "Fresh")
	if err != nil {
		t.Fatalf("SuggestListName() error = %v", err)
	}
	if got != "Fresh" {
		t.Errorf("SuggestListName() = %q, want the original name back untouched", got)
	}
}

func TestSuggestListNameDoesNotGuessOnOtherErrors(t *testing.T) {
	// A 429 says nothing about whether the name is taken. Inventing one from a
	// listing dk could not read would be a guess, so the error propagates.
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/validate/name/Bench PSU": {
			status: http.StatusTooManyRequests,
			body:   `{"ErrorMessage":"slow down"}`,
		},
	})

	if _, err := rc.SuggestListName(context.Background(), "Bench PSU"); err == nil {
		t.Error("SuggestListName() error = nil on a 429, want the error propagated")
	}
}

func TestAllListsRejectsAServerThatResendsACappedPage(t *testing.T) {
	// The guard this replaces asked whether the page came back full. The spec
	// documents a limit *default* of 50 and no maximum, so a server that caps
	// below listPageSize and ignores startIndex resends 50 rows forever and
	// never trips that test: the walk returned the first 50 lists and reported
	// them as the complete set, which is a false not_found for every name past
	// them — on `list rm` and `list set` as much as on `list show`.
	const serverCap = 50

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1800}`)
			return
		}
		requests++
		// startIndex ignored: always page one, always short of what was asked.
		var b strings.Builder
		b.WriteString("[")
		for i := range serverCap {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"Id":"id-%d","ListName":"List %d"}`, i, i)
		}
		b.WriteString("]")
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, ClientID: "id", Tokens: &staticTokens{app: "a", user: "u"}})
	if err != nil {
		t.Fatal(err)
	}

	all, err := c.AllLists(context.Background())
	if err == nil {
		t.Fatalf("AllLists() returned %d lists and no error against a server that ignores startIndex; "+
			"a truncated listing that looks complete is the failure this guards", len(all))
	}
	if !strings.Contains(err.Error(), "startIndex") {
		t.Errorf("error = %q, want it to name the cause (startIndex ignored)", err)
	}
	// It gives up on the repeat, not after maxListPages requests.
	if requests > 3 {
		t.Errorf("made %d requests, want it to stop as soon as a page repeats", requests)
	}
	if strings.Contains(err.Error(), strconv.Itoa(maxListPages)) {
		t.Errorf("error = %q, but the walk never made %d requests", err, maxListPages)
	}
}

func TestAllListsAcceptsAClampedTailAsTheEnd(t *testing.T) {
	// A server that clamps an out-of-range startIndex to the last page answers
	// with rows we already hold — indistinguishable from a non-paging server by
	// row count alone, and the reason the check is "does this batch start where
	// page one starts" rather than "is this batch non-empty". This one really
	// is the end, and erroring here would fail every list command outright.
	const total = 120
	const serverCap = 50

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1800}`)
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		if start > total-serverCap {
			start = total - serverCap // clamp instead of returning nothing
		}
		end := min(start+serverCap, total)

		var b strings.Builder
		b.WriteString("[")
		for i := start; i < end; i++ {
			if i > start {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"Id":"id-%d","ListName":"List %d"}`, i, i)
		}
		b.WriteString("]")
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, ClientID: "id", Tokens: &staticTokens{app: "a", user: "u"}})
	if err != nil {
		t.Fatal(err)
	}

	all, err := c.AllLists(context.Background())
	if err != nil {
		t.Fatalf("AllLists() error = %v against a server that clamps startIndex", err)
	}
	if len(all) != total {
		t.Fatalf("AllLists() returned %d lists, want all %d", len(all), total)
	}
}

// A name is trimmed before anything compares it. The local fallback normalizes
// the names it reads from the account, so an untrimmed candidate used to miss a
// list differing only by surrounding space and report a taken name as free —
// CreateList then failed on the duplicate --auto-rename exists to avoid.
func TestSuggestListNameTrimsBeforeComparing(t *testing.T) {
	rc := newRoutedClient(t, map[string]route{
		"GET /mylists/v1/lists/validate/name/ Bench PSU ": {
			status: http.StatusNotFound,
			body:   `{"ErrorMessage":"Invalid resource path"}`,
		},
		"GET /mylists/v1/lists/validate/name/Bench PSU": {
			status: http.StatusNotFound,
			body:   `{"ErrorMessage":"Invalid resource path"}`,
		},
		"GET /mylists/v1/lists": {status: http.StatusOK, body: `[
		  {"Id":"a","ListName":"Bench PSU"}
		]`},
	})

	got, err := rc.SuggestListName(context.Background(), "  Bench PSU  ")
	if err != nil {
		t.Fatalf("SuggestListName() error = %v", err)
	}
	if got != "Bench PSU (2)" {
		t.Errorf("SuggestListName(%q) = %q, want \"Bench PSU (2)\": surrounding space does not make a taken name free",
			"  Bench PSU  ", got)
	}
}
