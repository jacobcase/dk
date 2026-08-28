package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcase/dk/internal/auth"
	"github.com/jacobcase/dk/internal/digikey"
)

// loggedIn seeds a valid 3-legged token so list commands can run without an
// interactive browser login.
func loggedIn(t *testing.T, dir string) {
	t.Helper()
	store := auth.NewStore(filepath.Join(dir, "token.json"))
	err := store.Put(auth.KindUser, "production", &auth.Token{
		AccessToken:  "user-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runAuthed is run() with a cached user token in place.
func runAuthed(t *testing.T, m *mockDigiKey, args ...string) result {
	return runAuthedStdin(t, m, "", args...)
}

func runAuthedStdin(t *testing.T, m *mockDigiKey, stdin string, args ...string) result {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("DK_CONFIG_DIR", dir)
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	t.Setenv("DIGIKEY_ENV", "production")
	if m != nil {
		t.Setenv("DIGIKEY_API_BASE_URL", m.server.URL)
	}
	loggedIn(t, dir)

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr)
	return result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

const listsBody = `[
  {"Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":2,"DateModified":"2026-08-01T10:00:00Z","Tags":["project"]},
  {"Id":"bbb-222","ListName":"Audio Amp","TotalParts":0,"DateModified":"2026-07-15T09:00:00Z"}
]`

const listPartsBody = `{
  "TotalParts": 2,
  "PartsList": [
    {
      "UniqueId":"uid-1","DigiKeyPartNumber":"490-1532-1-ND",
      "RequestedPartNumber":"490-1532-1-ND","Notes":"decoupling",
      "ManufacturerPartNumber":"GRM188R71C104KA01D","Manufacturer":"Murata Electronics",
      "Description":"CAP CER 0.1UF 16V X7R 0603","ReferenceDesignator":"C1,C2",
      "QuantityAvailable":250000,"PartStatus":"Active","Flags":{"IsMatched":true},
      "Quantities":[{"QuantityRequested":10,"SelectedPackType":"Cut Tape",
        "PackOptions":[{"DigiKeyPartNumber":"490-1532-1-ND","PackType":"Cut Tape",
                       "CalculatedUnitPrice":0.048,"ExtendedPrice":0.48}]}]
    },
    {
      "UniqueId":"uid-2","RequestedPartNumber":"TYPO-PART-123",
      "Description":"","Flags":{"IsMatched":false},
      "Quantities":[{"QuantityRequested":5,"PackOptions":[]}]
    }
  ]
}`

func TestListLs(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)

	res := runAuthed(t, m, "list", "ls")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got []ListView
	res.JSON(t, &got)
	if len(got) != 2 {
		t.Fatalf("got %d lists, want 2", len(got))
	}
	if got[0].Name != "Bench PSU rev A" {
		t.Errorf("name = %q", got[0].Name)
	}
	// The web URL is what a human opens to review and order.
	if got[0].URL != listWebURLPrefix+"aaa-111" {
		t.Errorf("url = %q, want the digikey.com list page", got[0].URL)
	}
}

func TestListLsUsesUserToken(t *testing.T) {
	m := newMockDigiKey(t)
	m.routes["GET /mylists/v1/lists"] = func(w http.ResponseWriter, r *http.Request) {
		// The cached 3-legged token must be used verbatim; a client-credentials
		// token would be rejected by MyLists.
		if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
			t.Errorf("Authorization = %q, want Bearer user-token", got)
		}
		_, _ = w.Write([]byte(listsBody))
	}

	if res := runAuthed(t, m, "list", "ls"); res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	// No token request should have been made: the cached one is still valid.
	for _, r := range m.requests {
		if r.Path == "/v1/oauth2/token" {
			t.Error("a new token was requested despite a valid cached one")
		}
	}
}

func TestListSetClearsAFieldWithAnEmptyValue(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK,
		`[{"Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":1}]`)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK,
		`{"TotalParts":1,"PartsList":[
		  {"UniqueId":"u1","DigiKeyPartNumber":"490-1532-1-ND","RequestedPartNumber":"490-1532-1-ND",
		   "ReferenceDesignator":"C1-C10","Notes":"keep me",
		   "Quantities":[{"QuantityRequested":10}]}]}`)
	// The live shape: /lists/{id} answers with a correct TotalParts and an
	// empty PartsList. Everything `dk list set` needs comes from /parts, and
	// this route is here to fail the test if it is ever read again.
	m.handle("GET", "/mylists/v1/lists/aaa-111", http.StatusOK,
		`{"Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":1,"PartsList":[]}`)
	m.handle("PUT", "/mylists/v1/lists/aaa-111/parts/u1", http.StatusOK, ``)

	res := runAuthed(t, m, "list", "set", "Bench PSU rev A", "490-1532-1-ND", "--ref", "")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var sent map[string]any
	for _, r := range m.requests {
		if r.Method == http.MethodPut {
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("update body is not json: %v", err)
			}
		}
	}
	// --ref "" has to mean "clear it". Reading an empty value as "unchanged"
	// would leave a stale designator with no way to remove it.
	if got, ok := sent["ReferenceDesignator"]; ok && got != "" {
		t.Errorf("ReferenceDesignator = %v, want it cleared", got)
	}
	// Untouched fields still survive the read-modify-write.
	if sent["Notes"] != "keep me" {
		t.Errorf("Notes = %v, want the existing note preserved", sent["Notes"])
	}
}

func TestListSetRejectsNoFlags(t *testing.T) {
	// Checked before any request, so the mock needs no routes.
	res := runAuthed(t, nil, "list", "set", "Bench PSU rev A", "490-1532-1-ND")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
}

func TestListLsPages(t *testing.T) {
	// The paging flags existed as variables long before they were registered,
	// so assert they reach the wire rather than merely being accepted.
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)

	res := runAuthed(t, m, "list", "ls", "--limit", "5", "--offset", "10")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var query string
	for _, r := range m.requests {
		if r.Path == "/mylists/v1/lists" {
			query = r.Query
		}
	}
	for _, want := range []string{"limit=5", "startIndex=10"} {
		if !strings.Contains(query, want) {
			t.Errorf("lists query = %q, want it to contain %q", query, want)
		}
	}
}

func TestListCreate(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/mylists/v1/lists", http.StatusOK, `"new-id-123"`)

	res := runAuthed(t, m, "list", "create", "Bench PSU rev A", "--tag", "project", "--tag", "psu")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListView
	res.JSON(t, &got)
	if got.ID != "new-id-123" {
		t.Errorf("id = %q, want new-id-123", got.ID)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags = %v, want two entries", got.Tags)
	}

	var body map[string]any
	for _, r := range m.requests {
		if r.Method == http.MethodPost && r.Path == "/mylists/v1/lists" {
			_ = json.Unmarshal([]byte(r.Body), &body)
		}
	}
	if body["ListName"] != "Bench PSU rev A" {
		t.Errorf("ListName = %v", body["ListName"])
	}
}

func TestListCreateAutoRename(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists/validate/name/Bench PSU", http.StatusOK, `"Bench PSU (2)"`)
	m.handle("POST", "/mylists/v1/lists", http.StatusOK, `"new-id"`)

	res := runAuthed(t, m, "list", "create", "Bench PSU", "--auto-rename")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListView
	res.JSON(t, &got)
	// Unattended runs need a deterministic outcome rather than a duplicate-name
	// failure.
	if got.Name != "Bench PSU (2)" {
		t.Errorf("name = %q, want DigiKey's suggested variant", got.Name)
	}
}

func TestListCreateRejectsBadVisibility(t *testing.T) {
	res := runAuthed(t, nil, "list", "create", "X", "--visibility", "public")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

// --auto-rename costs a request, and a full paged walk of the account's lists
// when DigiKey's validate route 404s. A flag dk can reject on its own must be
// rejected before any of that.
func TestListCreateRejectsBadVisibilityBeforeAnyRequest(t *testing.T) {
	m := newMockDigiKey(t)

	res := runAuthed(t, m, "list", "create", "Bench PSU", "--auto-rename", "--visibility", "public")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	for _, r := range m.requests {
		if strings.HasPrefix(r.Path, "/mylists/") {
			t.Errorf("an invalid flag reached the API: %s %s", r.Method, r.Path)
		}
	}
}

func TestListShow(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListDetail
	res.JSON(t, &got)
	if got.ID != "aaa-111" {
		t.Errorf("id = %q, want aaa-111 resolved from the name", got.ID)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(got.Parts))
	}
	if got.Parts[0].Quantity != 10 || got.Parts[0].UnitPrice != 0.048 {
		t.Errorf("first part = %+v, want qty 10 at 0.048", got.Parts[0])
	}
	if got.EstimatedTotal != 0.48 {
		t.Errorf("estimated_total = %v, want 0.48", got.EstimatedTotal)
	}
	// The unmatched line is the one a caller must act on; it has to be counted
	// and flagged, not silently included.
	if got.UnmatchedParts != 1 {
		t.Errorf("unmatched_parts = %d, want 1", got.UnmatchedParts)
	}
	if got.Parts[1].Matched {
		t.Error("the TYPO-PART-123 line is marked matched")
	}
}

func TestListShowByID(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/bbb-222/parts", http.StatusOK, `{"PartsList":[],"TotalParts":0}`)

	res := runAuthed(t, m, "list", "show", "bbb-222")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got ListDetail
	res.JSON(t, &got)
	if got.Name != "Audio Amp" {
		t.Errorf("name = %q, want the list resolved by id", got.Name)
	}
}

func TestListShowUnknownNameExits4(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)

	res := runAuthed(t, m, "list", "show", "No Such List")
	if res.Code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitNotFound, res.Stderr)
	}
	p := res.ErrorJSON(t)
	if p.Error.Code != CodeNotFound {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeNotFound)
	}
	if !strings.Contains(p.Error.Hint, "dk list ls") {
		t.Errorf("hint = %q, want it to suggest listing the lists", p.Error.Hint)
	}
}

func TestListAmbiguousNameReportsCandidates(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK,
		`[{"Id":"1","ListName":"psu"},{"Id":"2","ListName":"PSU"},{"Id":"3","ListName":"Psu"}]`)

	res := runAuthed(t, m, "list", "show", "pSu")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}

	p := res.ErrorJSON(t)
	if p.Error.Code != CodeAmbiguous {
		t.Fatalf("error code = %q, want %q", p.Error.Code, CodeAmbiguous)
	}
	// Machine-readable candidates let the caller retry with an id rather than
	// parsing the message.
	candidates, ok := p.Error.Details["candidates"].([]any)
	if !ok || len(candidates) != 3 {
		t.Fatalf("details.candidates = %v, want three entries", p.Error.Details["candidates"])
	}
	first := candidates[0].(map[string]any)
	if first["id"] == nil || first["name"] == nil {
		t.Errorf("candidate = %v, want both id and name", first)
	}
}

func TestListAddSingle(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("POST", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `["uid-new"]`)

	res := runAuthed(t, m, "list", "add", "Bench PSU rev A", "490-1532-1-ND:10",
		"--ref", "C1,C2", "--note", "input decoupling")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got AddResult
	res.JSON(t, &got)
	if len(got.Added) != 1 {
		t.Fatalf("added = %+v, want one part", got.Added)
	}
	if got.Added[0].Part != "490-1532-1-ND" || got.Added[0].Quantity != 10 {
		t.Errorf("added[0] = %+v, want the part and quantity parsed from PART:QTY", got.Added[0])
	}
	// The unique id is the handle for later `dk list rm`.
	if got.Added[0].UniqueID != "uid-new" {
		t.Errorf("unique_id = %q, want uid-new", got.Added[0].UniqueID)
	}

	var parts []map[string]any
	for _, r := range m.requests {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/parts") {
			_ = json.Unmarshal([]byte(r.Body), &parts)
		}
	}
	if len(parts) != 1 {
		t.Fatalf("sent %d parts, want 1", len(parts))
	}
	if parts[0]["ReferenceDesignator"] != "C1,C2" {
		t.Errorf("ReferenceDesignator = %v, want C1,C2", parts[0]["ReferenceDesignator"])
	}
	if parts[0]["Notes"] != "input decoupling" {
		t.Errorf("Notes = %v", parts[0]["Notes"])
	}
}

func TestListAddMultipleWithPerPartQuantities(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("POST", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `["u1","u2","u3"]`)

	res := runAuthed(t, m, "list", "add", "aaa-111",
		"490-1532-1-ND:10", "311-10.0KHRCT-ND:20", "296-1234-5-ND", "--qty", "3")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got AddResult
	res.JSON(t, &got)
	if len(got.Added) != 3 {
		t.Fatalf("added %d parts, want 3", len(got.Added))
	}
	wantQty := []int{10, 20, 3}
	for i, want := range wantQty {
		if got.Added[i].Quantity != want {
			t.Errorf("added[%d].quantity = %d, want %d (--qty applies only where :QTY is absent)",
				i, got.Added[i].Quantity, want)
		}
	}
}

func TestListAddRejectsLineFlagsWithMultipleParts(t *testing.T) {
	m := newMockDigiKey(t)

	// Silently applying one --ref to three parts would corrupt the BOM.
	res := runAuthed(t, m, "list", "add", "aaa-111", "A:1", "B:2", "--ref", "C1")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "--from-json") {
		t.Errorf("error should point at --from-json for per-part metadata:\n%s", res.Stderr)
	}
}

func TestListAddFromJSONStdin(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("POST", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `["u1","u2"]`)

	stdin := `[
	  {"part":"490-1532-1-ND","quantity":10,"reference":"C1-C10","note":"decoupling"},
	  {"part":"311-10.0KHRCT-ND","quantity":20,"reference":"R1-R20"}
	]`
	res := runAuthedStdin(t, m, stdin, "list", "add", "aaa-111", "--from-json", "-")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got AddResult
	res.JSON(t, &got)
	if len(got.Added) != 2 {
		t.Fatalf("added %d parts, want 2", len(got.Added))
	}

	var parts []map[string]any
	for _, r := range m.requests {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/parts") {
			_ = json.Unmarshal([]byte(r.Body), &parts)
		}
	}
	// Per-part metadata is the whole point of --from-json.
	if parts[0]["ReferenceDesignator"] != "C1-C10" || parts[1]["ReferenceDesignator"] != "R1-R20" {
		t.Errorf("reference designators = %v / %v, want them kept per part",
			parts[0]["ReferenceDesignator"], parts[1]["ReferenceDesignator"])
	}
}

func TestListAddFromJSONFile(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("POST", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `["u1"]`)

	path := filepath.Join(t.TempDir(), "bom.json")
	if err := os.WriteFile(path, []byte(`[{"part":"490-1532-1-ND","quantity":7}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	res := runAuthed(t, m, "list", "add", "aaa-111", "--from-json", path)
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got AddResult
	res.JSON(t, &got)
	if len(got.Added) != 1 || got.Added[0].Quantity != 7 {
		t.Errorf("added = %+v, want one part with quantity 7", got.Added)
	}
}

func TestListAddFromJSONErrors(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
	}{
		{"not an array", `{"part":"X"}`},
		{"malformed", `[{"part":`},
		{"empty input", ``},
		{"missing part", `[{"quantity":5}]`},
		{"negative quantity", `[{"part":"X","quantity":-1}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runAuthedStdin(t, newMockDigiKey(t), tt.stdin, "list", "add", "aaa-111", "--from-json", "-")
			if res.Code != ExitUsage {
				t.Errorf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
			}
		})
	}
}

func TestListAddRequiresParts(t *testing.T) {
	res := runAuthed(t, newMockDigiKey(t), "list", "add", "aaa-111")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

func TestListAddVerifySkipsUnknownParts(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/products/v4/search/490-1532-1-ND/productdetails", http.StatusOK, productDetailsBody)
	m.handle("GET", "/products/v4/search/NOSUCHPART/productdetails", http.StatusNotFound,
		`{"StatusCode":404,"ErrorMessage":"not found"}`)
	m.handle("POST", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `["u1"]`)

	res := runAuthed(t, m, "list", "add", "aaa-111", "490-1532-1-ND:5", "NOSUCHPART:2", "--verify")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got AddResult
	res.JSON(t, &got)
	if len(got.Added) != 1 || got.Added[0].Part != "490-1532-1-ND" {
		t.Errorf("added = %+v, want only the part that resolved", got.Added)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Part != "NOSUCHPART" {
		t.Errorf("skipped = %+v, want the unresolvable part recorded with a reason", got.Skipped)
	}
}

func TestListAddVerifyAllSkippedExits4(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/products/v4/search/NOSUCHPART/productdetails", http.StatusNotFound,
		`{"StatusCode":404,"ErrorMessage":"not found"}`)

	res := runAuthed(t, m, "list", "add", "aaa-111", "NOSUCHPART", "--verify")
	// Nothing was added, so exiting 0 would tell a caller the BOM is complete
	// when it is not.
	if res.Code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitNotFound, res.Stderr)
	}
	// The add endpoint must never have been called.
	for _, r := range m.requests {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/parts") {
			t.Error("parts were posted even though verification rejected them all")
		}
	}
}

func TestListRemoveByPartNumber(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-1", http.StatusNoContent, "")

	// A caller knows the part number; the unique id is an implementation detail.
	res := runAuthed(t, m, "list", "rm", "aaa-111", "490-1532-1-ND")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got map[string]any
	res.JSON(t, &got)
	if got["removed"] != float64(1) {
		t.Errorf("removed = %v, want 1", got["removed"])
	}
}

func TestListRemoveByManufacturerPartNumber(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-1", http.StatusNoContent, "")

	res := runAuthed(t, m, "list", "rm", "aaa-111", "GRM188R71C104KA01D")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
}

func TestListRemoveByUniqueID(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-2", http.StatusNoContent, "")

	res := runAuthed(t, m, "list", "rm", "aaa-111", "uid-2")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
}

// One line answers to several names — the fixture's uid-1 to both its DigiKey
// part number and its manufacturer part number — so two targets can resolve to
// the same id. Deleting it twice would draw a 404 on the second call and fail a
// removal that in fact succeeded.
func TestListRemoveDeletesEachLineOnce(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-1", http.StatusNoContent, "")

	res := runAuthed(t, m, "list", "rm", "aaa-111", "490-1532-1-ND", "GRM188R71C104KA01D")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	deletes := 0
	for _, r := range m.requests {
		if r.Method == http.MethodDelete {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("sent %d deletes for one line, want 1", deletes)
	}

	var got map[string]any
	res.JSON(t, &got)
	if got["removed"] != float64(1) {
		t.Errorf("removed = %v, want 1", got["removed"])
	}
}

func TestListRemoveNoMatchExits4(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	res := runAuthed(t, m, "list", "rm", "aaa-111", "NOT-IN-THIS-LIST")
	if res.Code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitNotFound, res.Stderr)
	}
}

func TestListRename(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("PUT", "/mylists/v1/lists/aaa-111/listName/Bench PSU rev B", http.StatusOK, "")

	res := runAuthed(t, m, "list", "rename", "Bench PSU rev A", "Bench PSU rev B")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got ListView
	res.JSON(t, &got)
	if got.Name != "Bench PSU rev B" {
		t.Errorf("name = %q, want the new name", got.Name)
	}
}

func TestListDeleteRefusesNonEmptyWithoutForce(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	// The guard asks the parts endpoint rather than trusting the summary count,
	// which lags a mutation.
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	res := runAuthed(t, m, "list", "delete", "Bench PSU rev A")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	p := res.ErrorJSON(t)
	if !strings.Contains(p.Error.Hint, "--force") {
		t.Errorf("hint = %q, want it to mention --force", p.Error.Hint)
	}
	// Nothing must have been deleted.
	for _, r := range m.requests {
		if r.Method == http.MethodDelete {
			t.Error("a DELETE was issued despite the guard")
		}
	}
}

func TestListDeleteEmptyListWithoutForce(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/bbb-222/parts", http.StatusOK,
		`{"PartsList":[],"TotalParts":0}`)
	m.handle("DELETE", "/mylists/v1/lists/bbb-222", http.StatusNoContent, "")

	// The Audio Amp list has zero parts, so no confirmation is needed.
	res := runAuthed(t, m, "list", "delete", "Audio Amp")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
}

// The summary count from /lists lags a mutation, so a list that reports zero
// parts there but is actually non-empty must still be protected. This is the
// case that made the old guard unsafe: it would have deleted the list.
func TestListDeleteGuardIgnoresStaleSummaryCount(t *testing.T) {
	m := newMockDigiKey(t)
	// "Audio Amp" (bbb-222) carries TotalParts: 0 in listsBody...
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	// ...but the parts endpoint, which is authoritative, says otherwise.
	m.handle("GET", "/mylists/v1/lists/bbb-222/parts", http.StatusOK,
		`{"PartsList":[{"UniqueId":"u-1","DigiKeyPartNumber":"296-1234-1-ND"}],"TotalParts":1}`)
	m.handle("DELETE", "/mylists/v1/lists/bbb-222", http.StatusNoContent, "")

	res := runAuthed(t, m, "list", "delete", "Audio Amp")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d (stale zero must not authorize a delete)\nstderr: %s",
			res.Code, ExitUsage, res.Stderr)
	}
	for _, r := range m.requests {
		if r.Method == http.MethodDelete {
			t.Error("a DELETE was issued for a list the parts endpoint reported as non-empty")
		}
	}
}

func TestListDeleteWithForce(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111", http.StatusNoContent, "")

	res := runAuthed(t, m, "list", "delete", "Bench PSU rev A", "--force")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got map[string]any
	res.JSON(t, &got)
	if got["deleted"] != true {
		t.Errorf("deleted = %v, want true", got["deleted"])
	}
}

func TestListExportCSV(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	res := runAuthed(t, m, "list", "export", "Bench PSU rev A", "--output", "csv")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d csv lines, want a header plus two parts:\n%s", len(lines), res.Stdout)
	}
	if !strings.HasPrefix(lines[0], "Quantity,DigiKeyPartNumber") {
		t.Errorf("header = %q, want BOM-shaped columns", lines[0])
	}
	// A reference designator list contains commas and must stay one field.
	if !strings.Contains(lines[1], `"C1,C2"`) {
		t.Errorf("row = %q, want the reference designators quoted as one field", lines[1])
	}
}

func TestParsePartSpec(t *testing.T) {
	tests := []struct {
		name     string
		arg      string
		defQty   int
		wantPart string
		wantQty  int
		wantErr  bool
	}{
		{"bare part uses the default", "490-1532-1-ND", 5, "490-1532-1-ND", 5, false},
		{"explicit quantity", "490-1532-1-ND:10", 5, "490-1532-1-ND", 10, false},
		{"hyphens and periods survive", "311-10.0KHRCT-ND:20", 1, "311-10.0KHRCT-ND", 20, false},
		{"whitespace trimmed", "  ABC:3  ", 1, "ABC", 3, false},
		{"non-numeric quantity", "ABC:many", 1, "", 0, true},
		{"zero quantity", "ABC:0", 1, "", 0, true},
		{"negative quantity", "ABC:-5", 1, "", 0, true},
		{"missing part number", ":10", 1, "", 0, true},
		{"empty argument", "  ", 1, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePartSpec(tt.arg, tt.defQty)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePartSpec(%q) error = %v, wantErr %v", tt.arg, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.Part != tt.wantPart || got.Quantity != tt.wantQty {
				t.Errorf("parsePartSpec(%q) = {%q, %d}, want {%q, %d}",
					tt.arg, got.Part, got.Quantity, tt.wantPart, tt.wantQty)
			}
		})
	}
}

func TestParsePartSpecSplitsOnLastColon(t *testing.T) {
	// Splitting on the first colon would mangle a part number that contained one.
	got, err := parsePartSpec("WEIRD:PART:12", 1)
	if err != nil {
		t.Fatalf("parsePartSpec() error = %v", err)
	}
	if got.Part != "WEIRD:PART" || got.Quantity != 12 {
		t.Errorf("parsePartSpec() = {%q, %d}, want {WEIRD:PART, 12}", got.Part, got.Quantity)
	}
}

func TestMatchListEntries(t *testing.T) {
	parts := []digikey.ListPart{
		{
			UniqueID:               "uid-1",
			DigiKeyPartNumber:      "490-1532-1-ND",
			ManufacturerPartNumber: "GRM188R71C104KA01D",
		},
		{
			UniqueID:            "uid-2",
			RequestedPartNumber: "TYPO-PART-123",
		},
		{
			// The same part added twice on separate lines.
			UniqueID:          "uid-3",
			DigiKeyPartNumber: "490-1532-1-ND",
		},
	}

	tests := []struct {
		name   string
		target string
		want   []string
	}{
		{"by unique id", "uid-2", []string{"uid-2"}},
		{"by manufacturer part number", "GRM188R71C104KA01D", []string{"uid-1"}},
		{"by requested part number", "TYPO-PART-123", []string{"uid-2"}},
		// Part numbers get typed by hand, so matching tolerates case.
		{"case insensitive", "490-1532-1-nd", []string{"uid-1", "uid-3"}},
		{"duplicate lines all match", "490-1532-1-ND", []string{"uid-1", "uid-3"}},
		{"no match", "NOT-PRESENT", nil},
		{"empty target", "  ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchListEntries(parts, tt.target)
			if len(got) != len(tt.want) {
				t.Fatalf("matchListEntries(%q) = %v, want %v", tt.target, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("matchListEntries(%q)[%d] = %q, want %q", tt.target, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestShortDate(t *testing.T) {
	tests := []struct{ in, want string }{
		{"2026-08-01T10:00:00Z", "2026-08-01"},
		{"2026-08-01", "2026-08-01"},
		{"short", "short"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := shortDate(tt.in); got != tt.want {
			t.Errorf("shortDate(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParsePositiveInt(t *testing.T) {
	if _, err := parsePositiveInt("abc", "category id"); err == nil {
		t.Error("parsePositiveInt(\"abc\") error = nil")
	}
	if _, err := parsePositiveInt("0", "category id"); err == nil {
		t.Error("parsePositiveInt(\"0\") error = nil, want a positivity check")
	}
	if got, err := parsePositiveInt(" 42 ", "category id"); err != nil || got != 42 {
		t.Errorf("parsePositiveInt(\" 42 \") = (%d, %v), want (42, nil)", got, err)
	}
}

// listDataBody is GetListByListId output, in the shape the live API actually
// returns it: metadata with a correct TotalParts beside an empty PartsList.
// The spec says that array holds the editable RequestedParts; no live response
// has ever carried one, so nothing reads it and `dk list set` builds its update
// from GetPartsByListId instead.
const listDataBody = `{
  "Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":2,
  "PartsList":[]}`

func TestListSetQuantity(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111", http.StatusOK, listDataBody)
	m.handle("PUT", "/mylists/v1/lists/aaa-111/parts/uid-1", http.StatusOK, "")

	res := runAuthed(t, m, "list", "set", "Bench PSU rev A", "490-1532-1-ND", "--qty", "20")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got SetResult
	res.JSON(t, &got)
	if got.Before.Quantity != 10 || got.After.Quantity != 20 {
		t.Errorf("quantity %d -> %d, want 10 -> 20", got.Before.Quantity, got.After.Quantity)
	}
	// The unique id surviving is the whole point: rm+add would mint a new one.
	if got.UniqueID != "uid-1" {
		t.Errorf("unique_id = %q, want uid-1", got.UniqueID)
	}
	if len(got.Changed) != 1 || got.Changed[0] != "quantity" {
		t.Errorf("changed = %v, want only quantity", got.Changed)
	}

	// GET /lists/{id} answers with an empty PartsList live, so reading it to
	// find the line reported every part in every list as an unknown unique id.
	// The route is still registered above; asserting it goes untouched is what
	// keeps a future refactor from reaching for it again.
	for _, r := range m.requests {
		if r.Method == http.MethodGet && r.Path == "/mylists/v1/lists/aaa-111" {
			t.Error("dk list set read GET /lists/{id}; its PartsList is empty live, so the edit cannot come from there")
		}
	}

	var body map[string]any
	for _, r := range m.requests {
		if r.Method == http.MethodPut && strings.Contains(r.Path, "/parts/uid-1") {
			if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if body == nil {
		t.Fatal("no PUT was sent")
	}
	quantities := body["Quantities"].([]any)
	if q := quantities[0].(map[string]any); q["Quantity"] != float64(20) {
		t.Errorf("Quantity = %v, want 20", q["Quantity"])
	}
	// DigiKey's update is a replace, so untouched fields must be sent back
	// intact rather than silently cleared.
	if body["ReferenceDesignator"] != "C1,C2" {
		t.Errorf("ReferenceDesignator = %v, want it preserved", body["ReferenceDesignator"])
	}
	if body["Notes"] != "decoupling" {
		t.Errorf("Notes = %v, want it preserved", body["Notes"])
	}
	if q := quantities[0].(map[string]any); q["SelectedPackType"] != "Cut Tape" {
		t.Errorf("SelectedPackType = %v, want the existing pack type preserved", q["SelectedPackType"])
	}
}

func TestListSetMetadataOnly(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111", http.StatusOK, listDataBody)
	m.handle("PUT", "/mylists/v1/lists/aaa-111/parts/uid-1", http.StatusOK, "")

	res := runAuthed(t, m, "list", "set", "aaa-111", "uid-1", "--ref", "C1-C10", "--note", "bulk")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got SetResult
	res.JSON(t, &got)
	// Quantity was not passed, so it must not change.
	if got.After.Quantity != 10 {
		t.Errorf("quantity = %d, want it untouched at 10", got.After.Quantity)
	}
	if got.After.ReferenceDesignator != "C1-C10" || got.After.Notes != "bulk" {
		t.Errorf("after = %+v", got.After)
	}
}

func TestListSetRequiresAChange(t *testing.T) {
	res := runAuthed(t, newMockDigiKey(t), "list", "set", "aaa-111", "uid-1")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "--qty") {
		t.Errorf("error should name the flags that would change something:\n%s", res.Stderr)
	}
}

func TestListSetRejectsZeroQuantity(t *testing.T) {
	res := runAuthed(t, newMockDigiKey(t), "list", "set", "aaa-111", "uid-1", "--qty", "0")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", res.Code, ExitUsage)
	}
	// Quantity zero is a removal, and should say so rather than silently
	// creating a zero-quantity line.
	if !strings.Contains(res.Stderr, "list rm") {
		t.Errorf("error should point at `dk list rm`:\n%s", res.Stderr)
	}
}

func TestListSetUnknownPartExits4(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	res := runAuthed(t, m, "list", "set", "aaa-111", "NOT-IN-LIST", "--qty", "5")
	if res.Code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitNotFound, res.Stderr)
	}
}

func TestListSetAmbiguousTargetIsRejected(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	// Two lines carrying the same part number.
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `{"TotalParts":2,"PartsList":[
	  {"UniqueId":"uid-1","DigiKeyPartNumber":"DUP-ND","Quantities":[{"QuantityRequested":1}]},
	  {"UniqueId":"uid-3","DigiKeyPartNumber":"DUP-ND","Quantities":[{"QuantityRequested":2}]}]}`)

	res := runAuthed(t, m, "list", "set", "aaa-111", "DUP-ND", "--qty", "5")
	// Unlike rm, editing every match would be destructive guesswork.
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "unique id") {
		t.Errorf("error should tell the caller to use the unique id:\n%s", res.Stderr)
	}
	for _, r := range m.requests {
		if r.Method == http.MethodPut {
			t.Error("a PUT was issued despite the ambiguity")
		}
	}
}

func TestListCopy(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("POST", "/mylists/v1/lists", http.StatusOK, `"copied-id"`)

	res := runAuthed(t, m, "list", "copy", "Bench PSU rev A", "Bench PSU rev B")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListView
	res.JSON(t, &got)
	if got.ID != "copied-id" || got.Name != "Bench PSU rev B" {
		t.Errorf("view = %+v", got)
	}

	var body map[string]any
	for _, r := range m.requests {
		if r.Method == http.MethodPost && r.Path == "/mylists/v1/lists" {
			_ = json.Unmarshal([]byte(r.Body), &body)
		}
	}
	refList, ok := body["RefList"].(map[string]any)
	if !ok {
		t.Fatalf("RefList missing from the create body: %v", body)
	}
	// RefList is what makes DigiKey seed the new list from the old one.
	if refList["ListId"] != "aaa-111" {
		t.Errorf("RefList.ListId = %v, want the source list id", refList["ListId"])
	}
}

func TestListCopyResolvesSourceByName(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("POST", "/mylists/v1/lists", http.StatusOK, `"new"`)

	if res := runAuthed(t, m, "list", "copy", "audio amp", "Audio Amp v2"); res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var body map[string]any
	for _, r := range m.requests {
		if r.Method == http.MethodPost {
			_ = json.Unmarshal([]byte(r.Body), &body)
		}
	}
	if refList := body["RefList"].(map[string]any); refList["ListId"] != "bbb-222" {
		t.Errorf("RefList.ListId = %v, want bbb-222 resolved from the name", refList["ListId"])
	}
}

func TestListCopyRejectsEmptyName(t *testing.T) {
	res := runAuthed(t, newMockDigiKey(t), "list", "copy", "aaa-111", "  ")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d", res.Code, ExitUsage)
	}
}

func TestSetQuantityPreservesPackType(t *testing.T) {
	got := setQuantity([]digikey.RequestedQuantity{
		{Quantity: 10, SelectedPackType: "Cut Tape", TargetPrice: 0.05},
	}, 0, 25)
	if len(got) != 1 {
		t.Fatalf("got %d quantity lines, want 1", len(got))
	}
	if got[0].Quantity != 25 {
		t.Errorf("Quantity = %d, want 25", got[0].Quantity)
	}
	if got[0].SelectedPackType != "Cut Tape" {
		t.Errorf("SelectedPackType = %q, want it preserved", got[0].SelectedPackType)
	}
}

func TestSetQuantityOnEmptyLine(t *testing.T) {
	got := setQuantity(nil, 0, 7)
	if len(got) != 1 || got[0].Quantity != 7 {
		t.Errorf("setQuantity(nil, 0, 7) = %+v, want one line of 7", got)
	}
}

func TestSetQuantityKeepsTheSelectedLine(t *testing.T) {
	existing := []digikey.RequestedQuantity{
		{Quantity: 10, SelectedPackType: "CT"},
		{Quantity: 25, SelectedPackType: "TR"},
		{Quantity: 40, SelectedPackType: "DKR"},
	}
	// Quantities[0] is not necessarily the line DigiKey priced; collapsing to
	// it would change the pack type as a side effect of a quantity edit.
	got := setQuantity(existing, 2, 100)
	if len(got) != 1 {
		t.Fatalf("got %d quantity lines, want 1", len(got))
	}
	if got[0].SelectedPackType != "DKR" || got[0].Quantity != 100 {
		t.Errorf("kept %+v, want the selected DKR line at quantity 100", got[0])
	}

	// An index DigiKey never sent, or one past the end, falls back to the first
	// line rather than panicking on a list the user can see.
	for _, bad := range []int{-1, 3, 99} {
		got := setQuantity(existing, bad, 100)
		if len(got) != 1 || got[0].SelectedPackType != "CT" {
			t.Errorf("setQuantity(existing, %d, 100) = %+v, want a fallback to the first line", bad, got)
		}
	}
}

func TestFindListPart(t *testing.T) {
	parts := []digikey.ListPart{
		{UniqueID: "uid-1", RequestedPartNumber: "A"},
		{UniqueID: "uid-2", RequestedPartNumber: "B"},
	}
	if got, ok := findListPart(parts, "uid-2"); !ok || got.RequestedPartNumber != "B" {
		t.Errorf("findListPart(uid-2) = (%+v, %v)", got, ok)
	}
	if got, ok := findListPart(parts, "UID-1"); !ok || got.RequestedPartNumber != "A" {
		t.Errorf("findListPart should be case-insensitive, got (%+v, %v)", got, ok)
	}
	if _, ok := findListPart(parts, "nope"); ok {
		t.Error("findListPart(nope) ok = true")
	}
}

func TestListSetQuantityResetsTheSelectedIndex(t *testing.T) {
	// DigiKey's update is a replace, so `dk list set` sends back the
	// RequestedPart it read — including SelectedQuantityIndex. --qty collapses
	// Quantities to a single entry, which leaves any non-zero index naming an
	// entry that no longer exists. It has to be reset with the array, and the
	// entry that survives has to be the one the index pointed at, or the line
	// silently changes pack type along with its quantity.
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK,
		`[{"Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":1}]`)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK,
		`{"TotalParts":1,"PartsList":[
		  {"UniqueId":"u1","DigiKeyPartNumber":"490-1532-1-ND","RequestedPartNumber":"490-1532-1-ND",
		   "SelectedQuantityIndex":2,
		   "Quantities":[
		     {"QuantityRequested":10,"SelectedPackType":"CT"},
		     {"QuantityRequested":25,"SelectedPackType":"TR"},
		     {"QuantityRequested":40,"SelectedPackType":"DKR"}]}]}`)
	m.handle("GET", "/mylists/v1/lists/aaa-111", http.StatusOK,
		`{"Id":"aaa-111","ListName":"Bench PSU rev A","TotalParts":1,"PartsList":[]}`)
	m.handle("PUT", "/mylists/v1/lists/aaa-111/parts/u1", http.StatusOK, ``)

	res := runAuthed(t, m, "list", "set", "Bench PSU rev A", "490-1532-1-ND", "--qty", "100")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var sent struct {
		SelectedQuantityIndex int `json:"SelectedQuantityIndex"`
		Quantities            []struct {
			Quantity         int    `json:"Quantity"`
			SelectedPackType string `json:"SelectedPackType"`
		} `json:"Quantities"`
	}
	var found bool
	for _, r := range m.requests {
		if r.Method == http.MethodPut {
			found = true
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("update body is not json: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("no update was sent")
	}

	if len(sent.Quantities) != 1 {
		t.Fatalf("sent %d quantities, want 1", len(sent.Quantities))
	}
	if sent.SelectedQuantityIndex >= len(sent.Quantities) {
		t.Errorf("SelectedQuantityIndex = %d with %d quantities sent: it names an entry that does not exist",
			sent.SelectedQuantityIndex, len(sent.Quantities))
	}
	// The update is a replace, so the reset has to be *stated*. Under
	// omitempty a reset to 0 vanishes from the body and DigiKey is left to
	// infer it, which a replace API is not obliged to do.
	var raw map[string]json.RawMessage
	for _, r := range m.requests {
		if r.Method == http.MethodPut {
			if err := json.Unmarshal([]byte(r.Body), &raw); err != nil {
				t.Fatalf("update body is not json: %v", err)
			}
		}
	}
	if _, ok := raw["SelectedQuantityIndex"]; !ok {
		t.Error("SelectedQuantityIndex is absent from the update body; a replace cannot read an omitted field as a reset to 0")
	}
	if sent.Quantities[0].Quantity != 100 {
		t.Errorf("Quantity = %d, want 100", sent.Quantities[0].Quantity)
	}
	// Index 2 was Digi-Reel; keeping Quantities[0] would repack the line as CT.
	if sent.Quantities[0].SelectedPackType != "DKR" {
		t.Errorf("SelectedPackType = %q, want DKR — the pack type of the entry the index selected",
			sent.Quantities[0].SelectedPackType)
	}
}
