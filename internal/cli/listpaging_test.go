package cli

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// pagedParts registers a /parts route that honors startIndex and limit the way
// DigiKey does, defaulting to a small page when limit is absent. The small
// default is the point: it is what silently truncated a BOM.
func pagedParts(m *mockDigiKey, listID string, total int) {
	m.routes["GET /mylists/v1/lists/"+listID+"/parts"] = func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 10
		}
		end := min(start+limit, total)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"TotalParts":`+strconv.Itoa(total)+`,"PartsList":[`)
		for i := start; i < end; i++ {
			if i > start {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = fmt.Fprintf(w, `{
				"UniqueId":"uid-%d","DigiKeyPartNumber":"P-%d","ManufacturerPartNumber":"MPN-%d",
				"Flags":{"IsMatched":true},
				"Quantities":[{"QuantityRequested":1,"SelectedPackType":"Cut Tape",
					"PackOptions":[{"PackType":"Cut Tape","CalculatedUnitPrice":1.0,"ExtendedPrice":1.0}]}]
			}`, i, i, i)
		}
		_, _ = io.WriteString(w, `]}`)
	}
}

// A BOM missing its last page is worse than no BOM: it looks complete.
func TestListExportPagesTheWholeList(t *testing.T) {
	const total = 150

	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	pagedParts(m, "aaa-111", total)

	res := runAuthed(t, m, "list", "export", "Bench PSU rev A", "--output", "csv")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	rows, err := csv.NewReader(strings.NewReader(res.Stdout)).ReadAll()
	if err != nil {
		t.Fatalf("stdout is not valid csv: %v", err)
	}
	// One header row plus one row per part.
	if got := len(rows) - 1; got != total {
		t.Fatalf("exported %d parts, want %d — a short BOM is silently wrong", got, total)
	}
	// Spot-check a part that only exists past the first page.
	if !strings.Contains(res.Stdout, "P-149") {
		t.Error("the last part is missing from the export")
	}
}

func TestListShowPagesTheWholeListByDefault(t *testing.T) {
	const total = 150

	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	pagedParts(m, "aaa-111", total)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListDetail
	res.JSON(t, &got)
	if len(got.Parts) != total {
		t.Fatalf("returned %d parts, want %d", len(got.Parts), total)
	}
	if got.TotalParts != total || got.Returned != total {
		t.Errorf("total_parts = %d, returned = %d, want %d for both", got.TotalParts, got.Returned, total)
	}
	// Every line is priced at 1.0, so the total is the honest check that the
	// sum covered all pages rather than just the first.
	if got.EstimatedTotal != float64(total) {
		t.Errorf("estimated_total = %v, want %v — it covered only part of the list",
			got.EstimatedTotal, float64(total))
	}
}

// An explicit --limit/--offset still fetches one page, and the response has to
// say so rather than looking like a complete list.
func TestListShowExplicitPageReportsReturnedVersusTotal(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	pagedParts(m, "aaa-111", 150)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A", "--limit", "5", "--offset", "20")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListDetail
	res.JSON(t, &got)
	if got.Returned != 5 {
		t.Errorf("returned = %d, want 5", got.Returned)
	}
	if got.TotalParts != 150 {
		t.Errorf("total_parts = %d, want 150", got.TotalParts)
	}
	if len(got.Parts) != 5 || got.Parts[0].DigiKeyPartNumber != "P-20" {
		t.Errorf("--offset did not reach the wire; first part is %+v", got.Parts[0].DigiKeyPartNumber)
	}

	// The sibling `ls` test exists to pin exactly this; show needs it too.
	q := listPartsQuery(t, m, "aaa-111")
	if q.Get("limit") != "5" || q.Get("startIndex") != "20" {
		t.Errorf("query = %v, want limit=5 startIndex=20", q)
	}
}

func TestListShowRejectsNegativePaging(t *testing.T) {
	for _, args := range [][]string{
		{"list", "show", "Bench PSU rev A", "--limit", "-1"},
		{"list", "show", "Bench PSU rev A", "--offset", "-5"},
	} {
		res := runAuthed(t, newMockDigiKey(t), args...)
		if res.Code != ExitUsage {
			t.Errorf("%v: exit code = %d, want %d\nstderr: %s", args, res.Code, ExitUsage, res.Stderr)
		}
	}
}

// A part on page two is present; reporting it as not found would be a lie the
// caller cannot distinguish from a typo.
func TestListRemoveFindsPartBeyondTheFirstPage(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	pagedParts(m, "aaa-111", 150)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-140", http.StatusNoContent, "")

	res := runAuthed(t, m, "list", "rm", "Bench PSU rev A", "P-140")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0 for a part that is in the list\nstderr: %s", res.Code, res.Stderr)
	}

	var deleted bool
	for _, r := range m.requests {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.Path, "/uid-140") {
			deleted = true
		}
	}
	if !deleted {
		t.Error("no DELETE was issued for the matched part")
	}
}

func TestListSetFindsPartBeyondTheFirstPage(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	pagedParts(m, "aaa-111", 150)
	m.handle("GET", "/mylists/v1/lists/aaa-111", http.StatusOK,
		`{"Id":"aaa-111","ListName":"Bench PSU rev A","PartsList":[
			{"UniqueId":"uid-140","RequestedPartNumber":"P-140","Quantities":[{"Quantity":1}]}]}`)
	m.handle("PUT", "/mylists/v1/lists/aaa-111/parts/uid-140", http.StatusNoContent, "")

	res := runAuthed(t, m, "list", "set", "Bench PSU rev A", "P-140", "--qty", "20")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
}

// A rejected delete is a failure, not a per-row footnote. Exiting 0 would tell
// a caller the list is now in a state it is not in.
func TestListRemoveExitCodeReflectsDigiKeyRejection(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantExit int
		wantCode string
	}{
		{"rate limited", http.StatusTooManyRequests, ExitRateLimit, CodeRateLimit},
		{"unauthorized", http.StatusUnauthorized, ExitAuth, CodeAuth},
		{"server error", http.StatusInternalServerError, ExitError, CodeAPI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
			m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
			m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-1", tt.status,
				`{"ErrorMessage":"rejected"}`)

			res := runAuthed(t, m, "list", "rm", "Bench PSU rev A", "490-1532-1-ND")
			if res.Code != tt.wantExit {
				t.Fatalf("exit code = %d, want %d — a rejected delete must not look like success\nstderr: %s",
					res.Code, tt.wantExit, res.Stderr)
			}
			p := res.ErrorJSON(t)
			if p.Error.Code != tt.wantCode {
				t.Errorf("error.code = %q, want %q", p.Error.Code, tt.wantCode)
			}
		})
	}
}

// One target removed and another rejected is still a failed run: exiting 0
// would hide the rejection entirely.
func TestListRemovePartialFailureIsNotSuccess(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-1", http.StatusNoContent, "")
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-2", http.StatusTooManyRequests,
		`{"ErrorMessage":"slow down"}`)

	res := runAuthed(t, m, "list", "rm", "Bench PSU rev A", "490-1532-1-ND", "TYPO-PART-123")
	if res.Code == ExitOK {
		t.Fatalf("exit code = 0 despite a rejected delete\nstdout: %s", res.Stdout)
	}
	if res.Code != ExitRateLimit {
		t.Errorf("exit code = %d, want %d", res.Code, ExitRateLimit)
	}
}

// listPartsQuery returns the query of the first /parts request for a list.
func listPartsQuery(t *testing.T, m *mockDigiKey, listID string) url.Values {
	t.Helper()
	want := "/mylists/v1/lists/" + listID + "/parts"
	for _, r := range m.requests {
		if r.Path == want {
			q, err := url.ParseQuery(r.Query)
			if err != nil {
				t.Fatalf("parsing recorded query %q: %v", r.Query, err)
			}
			return q
		}
	}
	t.Fatalf("no request to %s was made", want)
	return nil
}

// A matched line that DigiKey did not price contributes 0 to estimated_total.
// Without a count of those lines, the total silently understates the real cost
// and nothing in the output says so.
func TestListShowCountsUnpricedParts(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `{
	  "TotalParts": 2,
	  "PartsList": [
	    {"UniqueId":"uid-1","DigiKeyPartNumber":"P-1","Flags":{"IsMatched":true},
	     "Quantities":[{"QuantityRequested":10,"SelectedPackType":"Cut Tape",
	       "PackOptions":[{"PackType":"Cut Tape","CalculatedUnitPrice":0.5,"ExtendedPrice":5.0}]}]},
	    {"UniqueId":"uid-2","DigiKeyPartNumber":"P-2","Flags":{"IsMatched":true},
	     "Quantities":[{"QuantityRequested":30,"SelectedPackType":"Tape & Reel",
	       "PackOptions":[{"PackType":"Tape & Reel"},
	                      {"PackType":"Cut Tape","CalculatedUnitPrice":0.1,"ExtendedPrice":3.0}]}]}
	  ]}`)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListDetail
	res.JSON(t, &got)

	if got.UnpricedParts != 1 {
		t.Errorf("unpriced_parts = %d, want 1 — an unpriced line must be visible, not just missing from the total",
			got.UnpricedParts)
	}
	// The Tape & Reel line must NOT be quoted at Cut Tape's price: the user
	// selected Tape & Reel, and 3.00 is for a product they did not choose.
	if got.EstimatedTotal != 5.0 {
		t.Errorf("estimated_total = %v, want 5.0 (the unpriced line must not borrow another pack type's price)",
			got.EstimatedTotal)
	}
	if got.UnmatchedParts != 0 {
		t.Errorf("unmatched_parts = %d, want 0 — the line matched, it just was not priced", got.UnmatchedParts)
	}
}

// A stale summary count must not outlive the parts endpoint's answer. The
// summary says 2 parts; the list is actually empty.
func TestListShowTrustsPartsEndpointOverStaleSummaryCount(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK,
		`{"TotalParts":0,"PartsList":[]}`)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got ListDetail
	res.JSON(t, &got)
	if got.TotalParts != 0 {
		t.Errorf("total_parts = %d, want 0 — the stale summary count won over the parts endpoint", got.TotalParts)
	}
	if strings.Contains(res.Stderr, "Showing 0 of") {
		t.Errorf("emitted a bogus truncation notice for a fully-paged empty list:\n%s", res.Stderr)
	}
}

// `dk list show --raw` is the only way to see SelectedPackType and
// PackOptions[].PackType as DigiKey spells them — the flattened view hides the
// pack-option machinery entirely, which is exactly what makes a spelling
// mismatch between those two fields invisible from normal output.
func TestListShowRawExposesPackOptions(t *testing.T) {
	const body = `{"TotalParts":1,"PartsList":[{"UniqueId":"uid-1",
	  "Quantities":[{"QuantityRequested":10,"SelectedPackType":"Cut Tape",
	    "PackOptions":[{"PackType":"Cut Tape (CT)","CalculatedUnitPrice":0.048},
	                   {"PackType":"Digi-Reel","CalculatedUnitPrice":0.1}]}],
	  "UndocumentedField":"kept"}]}`

	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, body)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A", "--raw")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	for _, want := range []string{"SelectedPackType", "PackOptions", "PackType", "UndocumentedField"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("--raw output is missing %q; it must be the untouched payload:\n%s", want, res.Stdout)
		}
	}
	// The flattened view must not appear.
	if strings.Contains(res.Stdout, "estimated_total") {
		t.Errorf("--raw emitted the flattened view instead of the payload:\n%s", res.Stdout)
	}
}

// --raw fetches one page, so it must not be combined with a format it cannot
// render.
func TestListShowRawRejectsCSV(t *testing.T) {
	res := runAuthed(t, newMockDigiKey(t), "list", "show", "Bench PSU rev A", "--raw", "--output", "csv")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
}
