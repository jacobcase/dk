package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcase/dk/internal/auth"
)

// --- output contract -------------------------------------------------------

// An agent branching on `.length` must not have to handle null as well as [].
// Each of these commands has an empty result that the guide calls normal.
func TestDocumentedArraysAreNeverNullWhenEmpty(t *testing.T) {
	tests := []struct {
		name  string
		route string
		body  string
		args  []string
		// key is the JSON field that must be [] rather than null.
		key string
	}{
		{
			name:  "related products",
			route: "/products/v4/search/WM4200-ND/associations",
			body:  `{"ProductAssociations":{}}`,
			args:  []string{"related", "WM4200-ND"},
			key:   "products",
		},
		{
			name:  "docs documents",
			route: "/products/v4/search/490-1532-1-ND/media",
			body:  `{"MediaLinks":[]}`,
			args:  []string{"docs", "490-1532-1-ND"},
			key:   "documents",
		},
		{
			name:  "pricing options",
			route: "/products/v4/search/packagetypebyquantity/311-10.0KHRCT-ND",
			body:  `{"Products":[]}`,
			args:  []string{"pricing", "311-10.0KHRCT-ND", "--qty", "10"},
			key:   "options",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("GET", tt.route, http.StatusOK, tt.body)

			res := run(t, m, tt.args...)
			if res.Code != ExitOK {
				t.Fatalf("exit code = %d, want 0 (an empty result is not an error)\nstderr: %s",
					res.Code, res.Stderr)
			}

			var raw map[string]json.RawMessage
			res.JSON(t, &raw)
			got, ok := raw[tt.key]
			if !ok {
				t.Fatalf("key %q missing from output: %s", tt.key, res.Stdout)
			}
			if string(got) != "[]" {
				t.Errorf("%s = %s, want [] — null forces every caller to special-case it",
					tt.key, got)
			}
		})
	}
}

// The guide documents `best` as `{...} | null`. omitempty would drop the key
// entirely, so a caller testing `.best === null` would instead find undefined.
func TestPricingBestKeyPresentWhenNothingInStock(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/packagetypebyquantity/311-10.0KHRCT-ND", http.StatusOK,
		`{"Products":[{
			"DigiKeyProductNumber":"311-10.0KHRCT-ND",
			"PackageType":{"Id":2,"Name":"Cut Tape (CT)"},
			"RecommendedQuantity":250,"MinimumOrderQuantity":1,
			"QuantityAvailable":0,
			"StandardPricing":[{"BreakQuantity":1,"UnitPrice":0.02,"TotalPrice":0.02}]
		}]}`)

	res := run(t, m, "pricing", "311-10.0KHRCT-ND", "--qty", "250")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var raw map[string]json.RawMessage
	res.JSON(t, &raw)
	got, ok := raw["best"]
	if !ok {
		t.Fatal("`best` key is absent; the guide documents it as {...} | null, so it must always be present")
	}
	if string(got) != "null" {
		t.Errorf("best = %s, want null when nothing is in stock", got)
	}
}

// --raw promises DigiKey's untouched payload. Re-encoding a decoded struct
// would drop unmodeled fields and add zero values DigiKey never sent.
func TestRawEmitsDigiKeysUntouchedPayload(t *testing.T) {
	// UndocumentedField is modeled by nothing in this codebase; it is the proof
	// that the bytes were passed through rather than round-tripped.
	const body = `{"ProductsCount":1,"UndocumentedField":"keep me","Products":[]}`

	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, body)

	res := run(t, m, "search", "capacitor", "--raw")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got map[string]any
	res.JSON(t, &got)
	if got["UndocumentedField"] != "keep me" {
		t.Errorf("--raw dropped a field dk does not model; output was:\n%s", res.Stdout)
	}
	// A re-serialized ProductView would have introduced snake_case keys.
	if strings.Contains(res.Stdout, "digikey_part_number") {
		t.Errorf("--raw emitted dk's view shape rather than DigiKey's payload:\n%s", res.Stdout)
	}
}

func TestRawProductEmitsUntouchedPayload(t *testing.T) {
	const body = `{"ProductSubstitutesCount":0,"UndocumentedField":"keep me","ProductSubstitutes":[]}`

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/490-1532-1-ND/substitutions", http.StatusOK, body)

	res := run(t, m, "product", "490-1532-1-ND", "--substitutes", "--raw")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got map[string]any
	res.JSON(t, &got)
	if got["UndocumentedField"] != "keep me" {
		t.Errorf("--raw dropped an unmodeled field:\n%s", res.Stdout)
	}
}

// --exact reads ExactMatches, a separate unpaged array. Reporting the keyword
// count as total_matches advertises pages --offset cannot reach.
func TestExactSearchReportsItsOwnTotal(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, `{
		"ProductsCount": 137,
		"SearchLocaleUsed": {"Currency":"USD"},
		"Products": [],
		"ExactMatches": [{
			"ManufacturerProductNumber":"GRM188R71C104KA01D",
			"Manufacturer":{"Id":2359,"Name":"Murata"},
			"Description":{"ProductDescription":"CAP CER"},
			"ProductVariations":[{"DigiKeyProductNumber":"490-1532-1-ND"}]
		}]
	}`)

	res := run(t, m, "search", "GRM188R71C104KA01D", "--exact")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got SearchResult
	res.JSON(t, &got)
	if got.TotalMatches != 1 {
		t.Errorf("total_matches = %d, want 1 — 137 describes the keyword result set, not the exact one",
			got.TotalMatches)
	}
	if strings.Contains(res.Stderr, "--offset") {
		t.Errorf("emitted a paging hint for --exact, which --offset cannot page:\n%s", res.Stderr)
	}
}

// "stdout carries only the result" holds in every format, not just JSON.
// Redirecting table output to a file must not capture the trailing prose.
func TestTableOutputKeepsProseOffStdout(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A", "--output", "table")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	// These are PrintText confirmations; they belong on stderr.
	for _, prose := range []string{"estimated total", "Review and order at"} {
		if strings.Contains(res.Stdout, prose) {
			t.Errorf("stdout contains prose %q:\n%s", prose, res.Stdout)
		}
		if !strings.Contains(res.Stderr, prose) {
			t.Errorf("stderr is missing %q; it must still reach the user:\n%s", prose, res.Stderr)
		}
	}
	// The table itself does belong on stdout.
	if !strings.Contains(res.Stdout, "490-1532-1-ND") {
		t.Errorf("stdout is missing the table:\n%s", res.Stdout)
	}
}

// --- error classification --------------------------------------------------

// Every branch of classify has to keep the exit code and the JSON code in
// agreement; these are the ones nothing else exercised.
func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*mockDigiKey)
		args     []string
		authed   bool
		wantCode string
		wantExit int
	}{
		{
			name: "rejected client credentials become auth_required",
			setup: func(m *mockDigiKey) {
				// The OAuth endpoint itself rejects the app credentials, which
				// surfaces as *auth.Error rather than *digikey.APIError.
				m.routes["POST /v1/oauth2/token"] = func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"bad client secret"}`)
				}
			},
			args:     []string{"search", "capacitor"},
			wantCode: CodeAuth,
			wantExit: ExitAuth,
		},
		{
			name: "a 500 becomes api_error, not something more specific",
			setup: func(m *mockDigiKey) {
				m.handle("POST", "/products/v4/search/keyword", http.StatusInternalServerError,
					`{"ErrorMessage":"upstream exploded","RequestId":"req-42"}`)
			},
			args:     []string{"search", "capacitor"},
			wantCode: CodeAPI,
			wantExit: ExitError,
		},
		{
			name: "a 400 becomes api_error too",
			setup: func(m *mockDigiKey) {
				m.handle("POST", "/products/v4/search/keyword", http.StatusBadRequest,
					`{"ErrorMessage":"bad request"}`)
			},
			args:     []string{"search", "capacitor"},
			wantCode: CodeAPI,
			wantExit: ExitError,
		},
		{
			name: "429 becomes rate_limited",
			setup: func(m *mockDigiKey) {
				m.routes["POST /products/v4/search/keyword"] = func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"ErrorMessage":"slow down"}`)
				}
			},
			args:     []string{"search", "capacitor"},
			wantCode: CodeRateLimit,
			wantExit: ExitRateLimit,
		},
		{
			name:     "an empty list name is a usage error, not a missing list",
			setup:    func(m *mockDigiKey) { m.handle("GET", "/mylists/v1/lists", http.StatusOK, `[]`) },
			args:     []string{"list", "show", "   "},
			authed:   true,
			wantCode: CodeUsage,
			wantExit: ExitUsage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockDigiKey(t)
			tt.setup(m)

			var res result
			if tt.authed {
				res = runAuthed(t, m, tt.args...)
			} else {
				res = run(t, m, tt.args...)
			}

			if res.Code != tt.wantExit {
				t.Errorf("exit code = %d, want %d\nstderr: %s", res.Code, tt.wantExit, res.Stderr)
			}
			p := res.ErrorJSON(t)
			if p.Error.Code != tt.wantCode {
				t.Errorf("error.code = %q, want %q", p.Error.Code, tt.wantCode)
			}
			// The two must never disagree: a caller branching on $? and one
			// branching on .error.code have to reach the same conclusion.
			if p.Error.ExitCode != res.Code {
				t.Errorf("error.exit_code = %d but the process exited %d", p.Error.ExitCode, res.Code)
			}
		})
	}
}

// Structured validation errors are the actionable part of a 400, so they have
// to reach the JSON error object rather than only the message text.
func TestAPIValidationErrorsReachErrorDetails(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusBadRequest, `{
		"ErrorMessage":"validation failed",
		"RequestId":"req-7",
		"ValidationErrors":[{"Field":"Limit","Message":"must be 1-50"}]
	}`)

	res := run(t, m, "search", "capacitor")
	if res.Code != ExitError {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitError, res.Stderr)
	}

	p := res.ErrorJSON(t)
	if p.Error.Details["request_id"] != "req-7" {
		t.Errorf("details.request_id = %v, want req-7", p.Error.Details["request_id"])
	}
	ve, ok := p.Error.Details["validation_errors"].([]any)
	if !ok || len(ve) != 1 {
		t.Fatalf("details.validation_errors = %v, want one entry", p.Error.Details["validation_errors"])
	}
}

// A malformed config file exits 6, so its code must say so too rather than
// falling back to the generic "error".
func TestMalformedConfigCodeMatchesItsExitCode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DK_CONFIG_DIR", dir)

	var stdout, stderr strings.Builder
	code := Execute(t.Context(), []string{"search", "capacitor", "--output", "json"},
		strings.NewReader(""), &stdout, &stderr)

	if code != ExitConfig {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, ExitConfig, stderr.String())
	}
	res := result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
	p := res.ErrorJSON(t)
	if p.Error.Code != CodeConfig {
		t.Errorf("error.code = %q, want %q", p.Error.Code, CodeConfig)
	}
	if p.Error.ExitCode != ExitConfig {
		t.Errorf("error.exit_code = %d, want %d", p.Error.ExitCode, ExitConfig)
	}
}

// A failure before setup finishes must still honor an explicit --output.
//
// This deliberately asserts the TABLE direction. Test streams are never TTYs,
// so an early failure already defaults to JSON — asserting "--output json
// yields JSON" would pass whether or not the flag was consulted at all.
// Asking for table output is the only direction that distinguishes the two.
func TestEarlyFailureHonorsExplicitOutputFormat(t *testing.T) {
	writeBadConfig := func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DK_CONFIG_DIR", dir)
	}

	t.Run("explicit table yields prose, not JSON", func(t *testing.T) {
		writeBadConfig(t)

		var stdout, stderr strings.Builder
		code := Execute(t.Context(), []string{"search", "capacitor", "--output", "table"},
			strings.NewReader(""), &stdout, &stderr)

		if code != ExitConfig {
			t.Fatalf("exit code = %d, want %d", code, ExitConfig)
		}
		if json.Valid([]byte(stderr.String())) {
			t.Errorf("stderr is JSON despite --output table; the flag was ignored:\n%s", stderr.String())
		}
		if !strings.HasPrefix(stderr.String(), "Error:") {
			t.Errorf("stderr = %q, want the prose error form", stderr.String())
		}
	})

	t.Run("explicit json yields a parseable error object", func(t *testing.T) {
		writeBadConfig(t)

		var stdout, stderr strings.Builder
		Execute(t.Context(), []string{"search", "capacitor", "--output", "json"},
			strings.NewReader(""), &stdout, &stderr)

		var p errorPayload
		if err := json.Unmarshal([]byte(stderr.String()), &p); err != nil {
			t.Fatalf("stderr is not JSON despite --output json: %v\nstderr: %s", err, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout got %q, want nothing on a failure", stdout.String())
		}
	})
}

// --- auth state ------------------------------------------------------------

func TestAuthStatusReportsLoggedIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DK_CONFIG_DIR", dir)
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	t.Setenv("DIGIKEY_ENV", "production")
	loggedIn(t, dir)

	var stdout, stderr strings.Builder
	code := Execute(t.Context(), []string{"auth", "status"}, strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
	}

	var got AuthStatus
	result{Stdout: stdout.String()}.JSON(t, &got)
	if !got.UserLoggedIn {
		t.Error("user_logged_in = false with a cached user token; this is the field agents gate list work on")
	}
	if !got.HasRefreshToken {
		t.Error("has_refresh_token = false despite a stored refresh token")
	}
	if got.UserTokenExpiresAt == "" {
		t.Error("user_token_expires_at is empty")
	}
}

// logout with nothing cached was covered; the case that actually removes a
// token was not, so a logout that silently kept the token would have passed.
func TestAuthLogoutRemovesCachedUserToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DK_CONFIG_DIR", dir)
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	t.Setenv("DIGIKEY_ENV", "production")
	loggedIn(t, dir)

	store := auth.NewStore(filepath.Join(dir, "token.json"))
	if tok, err := store.Get(auth.KindUser, "production"); err != nil || tok == nil {
		t.Fatalf("precondition failed: no user token cached (%v)", err)
	}

	var stdout, stderr strings.Builder
	code := Execute(t.Context(), []string{"auth", "logout"}, strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
	}

	after := auth.NewStore(filepath.Join(dir, "token.json"))
	tok, err := after.Get(auth.KindUser, "production")
	if err != nil {
		t.Fatalf("reading the store after logout: %v", err)
	}
	if tok != nil && (tok.AccessToken != "" || tok.RefreshToken != "") {
		t.Errorf("user token survived logout: %+v", tok)
	}
}

// --all must clear the app token too, not just the current environment's user
// token.
func TestAuthLogoutAllClearsAppToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DK_CONFIG_DIR", dir)
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	t.Setenv("DIGIKEY_ENV", "production")
	loggedIn(t, dir)

	store := auth.NewStore(filepath.Join(dir, "token.json"))
	err := store.Put(auth.KindApp, "production", &auth.Token{
		AccessToken: "app-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := Execute(t.Context(), []string{"auth", "logout", "--all"}, strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
	}

	after := auth.NewStore(filepath.Join(dir, "token.json"))
	for _, kind := range []auth.Kind{auth.KindUser, auth.KindApp} {
		tok, err := after.Get(kind, "production")
		if err != nil {
			t.Fatalf("reading %s token: %v", kind, err)
		}
		if tok != nil && tok.AccessToken != "" {
			t.Errorf("%s token survived `logout --all`: %+v", kind, tok)
		}
	}
}

// --- commands with no coverage at all --------------------------------------

const categoriesBody = `{"Categories":[
  {"CategoryId":3,"Name":"Capacitors","ProductCount":100,"Children":[
    {"CategoryId":4,"Name":"Ceramic Capacitors","ProductCount":80}
  ]}
]}`

const manufacturersBody = `{"Manufacturers":[
  {"Id":2359,"Name":"Murata Electronics"},
  {"Id":10,"Name":"Adafruit Industries LLC"}
]}`

func TestCategoriesCommand(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/categories", http.StatusOK, categoriesBody)

	res := run(t, m, "categories")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got []map[string]any
	res.JSON(t, &got)
	if len(got) != 1 {
		t.Fatalf("got %d top-level categories, want 1", len(got))
	}
	if got[0]["name"] != "Capacitors" {
		t.Errorf("category name = %v, want Capacitors", got[0]["name"])
	}
	// /search/categories returns DigiKey's Category, which has five fields. dk
	// decodes it into CategoryNode -- really the richer in-Product shape -- so
	// printing that struct emitted NewProductCount and SeoDescription, which
	// DigiKey never sent. The view exists to keep them out.
	for _, phantom := range []string{"NewProductCount", "SeoDescription", "Id", "Name"} {
		if _, ok := got[0][phantom]; ok {
			t.Errorf("category JSON has key %q; the view must not leak DigiKey's struct", phantom)
		}
	}
}

// --flat has to walk the tree; a flattener that dropped children would still
// pass a test that only checked the top level.
func TestCategoriesFlatIncludesChildren(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/categories", http.StatusOK, categoriesBody)

	res := run(t, m, "categories", "--flat")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	res.JSON(t, &got)
	if len(got) != 2 {
		t.Fatalf("got %d flattened categories, want 2 (parent and child)", len(got))
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "Ceramic Capacitors") {
		t.Errorf("flattened list %v is missing the child category", names)
	}
}

func TestCategoryByIDCommand(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/categories/3", http.StatusOK,
		`{"Category":{"CategoryId":3,"Name":"Capacitors","ProductCount":100}}`)

	res := run(t, m, "categories", "3")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
}

func TestCategoryRejectsNonNumericID(t *testing.T) {
	res := run(t, newMockDigiKey(t), "categories", "capacitors")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d for a non-numeric category id\nstderr: %s",
			res.Code, ExitUsage, res.Stderr)
	}
}

func TestManufacturersCommand(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/manufacturers", http.StatusOK, manufacturersBody)

	res := run(t, m, "manufacturers")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got []map[string]any
	res.JSON(t, &got)
	if len(got) != 2 {
		t.Fatalf("got %d manufacturers, want 2", len(got))
	}
}

func TestManufacturersFilterIsCaseInsensitive(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/manufacturers", http.StatusOK, manufacturersBody)

	res := run(t, m, "manufacturers", "--filter", "MURATA")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got []struct {
		Name string `json:"name"`
	}
	res.JSON(t, &got)
	if len(got) != 1 || !strings.Contains(got[0].Name, "Murata") {
		t.Errorf("filter returned %v, want only Murata", got)
	}
}

// --category accepts a name, which costs a taxonomy lookup and must reach the
// wire as the resolved numeric id.
func TestSearchCategoryNameResolvesToID(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/categories", http.StatusOK, categoriesBody)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "capacitor", "--category", "Ceramic Capacitors")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	body := requestBody(t, m, "POST", "/products/v4/search/keyword")
	if !strings.Contains(body, `"Id":"4"`) {
		t.Errorf("category name did not resolve to id 4 on the wire:\n%s", body)
	}
}

func TestSearchUnknownCategoryNameIsAUsageError(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/categories", http.StatusOK, categoriesBody)

	res := run(t, m, "search", "capacitor", "--category", "Nonexistent Widgets")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	// The error has to name the command that lists valid values, so a caller can
	// correct itself in one round trip.
	if !strings.Contains(res.Stderr, "dk categories") {
		t.Errorf("error does not point at `dk categories`:\n%s", res.Stderr)
	}
}

func TestProductViewFlags(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		route string
		body  string
		want  string
	}{
		{
			name:  "substitutes",
			flag:  "--substitutes",
			route: "/products/v4/search/490-1532-1-ND/substitutions",
			body: `{"ProductSubstitutesCount":1,"ProductSubstitutes":[
				{"DigiKeyProductNumber":"490-9999-1-ND","SubstituteType":"Equivalent","UnitPrice":"$0.10"}]}`,
			want: "490-9999-1-ND",
		},
		{
			name:  "recommended",
			flag:  "--recommended",
			route: "/products/v4/search/490-1532-1-ND/recommendedproducts",
			body: `{"Recommendations":[{"ProductNumber":"490-1532-1-ND","RecommendedProducts":[
				{"DigiKeyProductNumber":"296-1234-1-ND","UnitPrice":0.5}]}]}`,
			want: "296-1234-1-ND",
		},
		{
			name:  "alternate packaging",
			flag:  "--alternate-packaging",
			route: "/products/v4/search/490-1532-1-ND/alternatepackaging",
			body: `{"AlternatePackagings":{"AlternatePackaging":[
				{"DigiKeyProductNumber":"490-1532-2-ND","UnitPrice":"$0.01"}]}}`,
			want: "490-1532-2-ND",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("GET", tt.route, http.StatusOK, tt.body)

			res := run(t, m, "product", "490-1532-1-ND", tt.flag)
			if res.Code != ExitOK {
				t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
			}
			if !strings.Contains(res.Stdout, tt.want) {
				t.Errorf("output does not contain %q:\n%s", tt.want, res.Stdout)
			}
		})
	}
}

// --variations and --parameters change which table section is shown, but JSON
// carries the whole view either way. That is what the guide promises.
func TestProductVariationsAndParametersReturnTheFullView(t *testing.T) {
	for _, flag := range []string{"--variations", "--parameters"} {
		t.Run(flag, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("GET", "/products/v4/search/490-1532-1-ND/productdetails",
				http.StatusOK, productDetailsBody)

			res := run(t, m, "product", "490-1532-1-ND", flag)
			if res.Code != ExitOK {
				t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
			}

			var got ProductView
			res.JSON(t, &got)
			if len(got.Variations) == 0 {
				t.Errorf("%s: JSON is missing variations; the full view is promised regardless of flag", flag)
			}
			if len(got.Parameters) == 0 {
				t.Errorf("%s: JSON is missing parameters", flag)
			}
		})
	}
}

func TestProductViewFlagsAreMutuallyExclusive(t *testing.T) {
	res := run(t, newMockDigiKey(t), "product", "490-1532-1-ND", "--variations", "--parameters")
	if res.Code != ExitUsage {
		t.Errorf("exit code = %d, want %d\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
}

// requestBody returns the body of the first recorded request to a route.
func requestBody(t *testing.T, m *mockDigiKey, method, path string) string {
	t.Helper()
	for _, r := range m.requests {
		if r.Method == method && r.Path == path {
			return r.Body
		}
	}
	t.Fatalf("no %s %s request was made", method, path)
	return ""
}

// Ctrl-C during `dk auth login` must classify as "cancelled"/exit 1, not as an
// auth failure.
//
// classify checks for an *Error before it checks for context.Canceled, so
// wrapping the cancellation in browserLogin would shadow the cancelled branch
// entirely. That sends an agent off to ask a human to re-authorize when there
// is nothing wrong with the credentials — the one instruction the guide tells
// agents to act on.
func TestBrowserLoginCancellationIsNotAnAuthError(t *testing.T) {
	app := &App{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}

	// Port 0 lets the OS pick, so this cannot collide with a real dk login.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := app.browserLogin(ctx, "https://example.invalid/authorize",
		"https://localhost:0/digikey_callback", "state-value", true)
	if err == nil {
		t.Fatal("browserLogin() error = nil, want a cancellation")
	}

	cliErr := classify(err)
	if cliErr.Code != CodeCancelled {
		t.Errorf("error code = %q, want %q (message: %s)", cliErr.Code, CodeCancelled, cliErr.Message)
	}
	if cliErr.ExitCode != ExitError {
		t.Errorf("exit code = %d, want %d — the guide documents Ctrl-C as exit 1",
			cliErr.ExitCode, ExitError)
	}
	if cliErr.Code == CodeAuth {
		t.Error("an interrupted login reported auth_required, which tells an agent to ask a human to re-login")
	}
}

// --- flattened product view payloads ---------------------------------------

// All three of these wrap DigiKey payloads that used to be printed verbatim, in
// PascalCase, with phantom fields for anything DigiKey omitted. They now return
// dk views like every other command; --raw is the escape hatch.
func TestProductViewFlagsReturnFlattenedViews(t *testing.T) {
	// Each fixture carries a field dk does not model, to prove the view drops it
	// while --raw keeps it, and omits ProductUrl, to prove no phantom key appears.
	tests := []struct {
		name      string
		flag      string
		route     string
		body      string
		arrayKey  string
		wantFirst map[string]any
	}{
		{
			name:     "alternate packaging",
			flag:     "--alternate-packaging",
			route:    "/products/v4/search/WM4200-ND/alternatepackaging",
			body:     `{"AlternatePackagings":{"AlternatePackaging":[{"DigiKeyProductNumber":"WM4300-CT-ND","ManufacturerProductNumber":"22-01-3037","Manufacturer":{"Id":1,"Name":"Molex"},"Description":"CONN HOUSING","UnitPrice":"$0.28","QuantityAvailable":15000,"Undocumented":"kept"}]}}`,
			arrayKey: "packaging",
			wantFirst: map[string]any{
				"digikey_part_number":      "WM4300-CT-ND",
				"manufacturer_part_number": "22-01-3037",
				"manufacturer":             "Molex",
				"unit_price":               "$0.28",
				"quantity_available":       float64(15000),
			},
		},
		{
			name:     "substitutes",
			flag:     "--substitutes",
			route:    "/products/v4/search/WM4200-ND/substitutions",
			body:     `{"ProductSubstitutesCount":1,"ProductSubstitutes":[{"SubstituteType":"Equivalent","DigiKeyProductNumber":"WM9999-ND","Manufacturer":{"Id":1,"Name":"Molex"},"UnitPrice":"$0.31","QuantityAvailable":42,"Undocumented":"kept"}]}`,
			arrayKey: "substitutes",
			wantFirst: map[string]any{
				"substitute_type":     "Equivalent",
				"digikey_part_number": "WM9999-ND",
				"manufacturer":        "Molex",
				"unit_price":          "$0.31",
				"quantity_available":  float64(42),
			},
		},
		{
			name:     "recommended",
			flag:     "--recommended",
			route:    "/products/v4/search/WM4200-ND/recommendedproducts",
			body:     `{"Recommendations":[{"ProductNumber":"WM4200-ND","RecommendedProducts":[{"DigiKeyProductNumber":"296-1234-1-ND","ManufacturerName":"TI","ProductDescription":"OPAMP","UnitPrice":0.5,"QuantityAvailable":7,"Undocumented":"kept"}]}]}`,
			arrayKey: "recommended",
			wantFirst: map[string]any{
				"digikey_part_number": "296-1234-1-ND",
				"manufacturer":        "TI",
				"description":         "OPAMP",
				// A NUMBER here, unlike the string above. DigiKey's split, preserved.
				"unit_price":         float64(0.5),
				"quantity_available": float64(7),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("GET", tt.route, http.StatusOK, tt.body)

			res := run(t, m, "product", "WM4200-ND", tt.flag)
			if res.Code != ExitOK {
				t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
			}

			var got map[string]any
			res.JSON(t, &got)

			if got["part_number"] != "WM4200-ND" {
				t.Errorf("part_number = %v, want the requested part", got["part_number"])
			}
			arr, ok := got[tt.arrayKey].([]any)
			if !ok {
				t.Fatalf("%q is not an array: %s", tt.arrayKey, res.Stdout)
			}
			if len(arr) != 1 {
				t.Fatalf("got %d entries, want 1", len(arr))
			}
			first, ok := arr[0].(map[string]any)
			if !ok {
				t.Fatalf("entry is not an object: %v", arr[0])
			}

			for k, want := range tt.wantFirst {
				if first[k] != want {
					t.Errorf("%s = %#v, want %#v", k, first[k], want)
				}
			}
			// No PascalCase leaking through, and no field DigiKey never sent.
			if _, bad := first["DigiKeyProductNumber"]; bad {
				t.Errorf("PascalCase key survived into the view: %v", first)
			}
			if _, bad := first["product_url"]; bad {
				t.Errorf("product_url present though DigiKey omitted it; the view invented a field: %v", first)
			}
			if _, bad := first["Undocumented"]; bad {
				t.Errorf("unmodeled field leaked into the view: %v", first)
			}
		})
	}
}

// The flattened view drops unmodeled fields by design; --raw must still carry
// them, or there is no way to reach data dk does not model.
func TestProductViewFlagsRawKeepsUnmodeledFields(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/WM4200-ND/alternatepackaging", http.StatusOK,
		`{"AlternatePackagings":{"AlternatePackaging":[{"DigiKeyProductNumber":"WM4300-CT-ND","Undocumented":"kept"}]}}`)

	res := run(t, m, "product", "WM4200-ND", "--alternate-packaging", "--raw")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	if !strings.Contains(res.Stdout, `"Undocumented": "kept"`) {
		t.Errorf("--raw dropped an unmodeled field:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "digikey_part_number") {
		t.Errorf("--raw emitted the flattened view instead of the payload:\n%s", res.Stdout)
	}
}

// Empty results stay [] rather than null, same as everywhere else.
func TestProductViewFlagsEmptyArraysAreNotNull(t *testing.T) {
	cases := []struct{ flag, route, body, key string }{
		{"--alternate-packaging", "/products/v4/search/X-ND/alternatepackaging", `{"AlternatePackagings":{}}`, "packaging"},
		{"--substitutes", "/products/v4/search/X-ND/substitutions", `{"ProductSubstitutesCount":0}`, "substitutes"},
		{"--recommended", "/products/v4/search/X-ND/recommendedproducts", `{"Recommendations":[]}`, "recommended"},
	}
	for _, c := range cases {
		t.Run(c.flag, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("GET", c.route, http.StatusOK, c.body)

			res := run(t, m, "product", "X-ND", c.flag)
			if res.Code != ExitOK {
				t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
			}
			var raw map[string]json.RawMessage
			res.JSON(t, &raw)
			if string(raw[c.key]) != "[]" {
				t.Errorf("%s = %s, want []", c.key, raw[c.key])
			}
		})
	}
}

// dk related and dk product --alternate-packaging wrap the same DigiKey type.
// Sharing SummaryView is what keeps them identical; this pins that they are.
func TestRelatedAndAlternatePackagingShareTheSameShape(t *testing.T) {
	const entry = `{"DigiKeyProductNumber":"WM4300-ND","ManufacturerProductNumber":"22-01-3037","Manufacturer":{"Id":1,"Name":"Molex"},"Description":"CONN HOUSING","UnitPrice":"$0.28","QuantityAvailable":15000,"ProductUrl":"https://example.com/p"}`

	m1 := newMockDigiKey(t)
	m1.handle("GET", "/products/v4/search/WM4200-ND/associations", http.StatusOK,
		`{"ProductAssociations":{"MatingProducts":[`+entry+`]}}`)
	var relatedOut struct {
		Products []map[string]any `json:"products"`
	}
	run(t, m1, "related", "WM4200-ND").JSON(t, &relatedOut)

	m2 := newMockDigiKey(t)
	m2.handle("GET", "/products/v4/search/WM4200-ND/alternatepackaging", http.StatusOK,
		`{"AlternatePackagings":{"AlternatePackaging":[`+entry+`]}}`)
	var altOut struct {
		Packaging []map[string]any `json:"packaging"`
	}
	run(t, m2, "product", "WM4200-ND", "--alternate-packaging").JSON(t, &altOut)

	if len(relatedOut.Products) != 1 || len(altOut.Packaging) != 1 {
		t.Fatalf("expected one entry each, got %d and %d", len(relatedOut.Products), len(altOut.Packaging))
	}

	rel, alt := relatedOut.Products[0], altOut.Packaging[0]
	// related adds "relation"; everything else must match key for key.
	delete(rel, "relation")
	if len(rel) != len(alt) {
		t.Errorf("key sets differ:\n related: %v\n alt:     %v", rel, alt)
	}
	for k, v := range alt {
		if rel[k] != v {
			t.Errorf("%s differs: related=%#v alternate-packaging=%#v", k, rel[k], v)
		}
	}
}

// --- asset URL normalization -----------------------------------------------

// DigiKey returns protocol-relative datasheet URLs for a large fraction of
// products — 8 of 20 in a sampled search. Neither curl nor Go's http client
// will fetch "//host/path", so emitting it verbatim hands the caller a string
// that looks like a URL and cannot be used.
func TestDatasheetURLsAreFetchable(t *testing.T) {
	const body = `{
	  "ProductsCount": 1,
	  "SearchLocaleUsed": {"Currency":"USD"},
	  "Products": [{
	    "ManufacturerProductNumber":"GRM188R71C104KA01D",
	    "Manufacturer":{"Id":2359,"Name":"Murata"},
	    "Description":{"ProductDescription":"CAP CER"},
	    "DatasheetUrl":"//mm.digikey.com/Volume0/opasdata/d220001/medias/docus/8942/x.pdf",
	    "ProductUrl":"//www.digikey.com/en/products/detail/x/1",
	    "ProductVariations":[{"DigiKeyProductNumber":"490-1532-1-ND"}]
	  }]
	}`

	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, body)

	res := run(t, m, "search", "capacitor")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got SearchResult
	res.JSON(t, &got)
	p := got.Products[0]

	if !strings.HasPrefix(p.DatasheetURL, "https://") {
		t.Errorf("datasheet_url = %q, want an absolute https URL", p.DatasheetURL)
	}
	if !strings.Contains(p.DatasheetURL, "mm.digikey.com") {
		t.Errorf("datasheet_url = %q, want the host preserved", p.DatasheetURL)
	}
	if !strings.HasPrefix(p.ProductURL, "https://") {
		t.Errorf("product_url = %q, want an absolute https URL", p.ProductURL)
	}
}

// The same repair has to apply to a list line's datasheet, which comes from a
// different field on a different endpoint.
func TestListDatasheetURLsAreFetchable(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `{
	  "TotalParts":1,
	  "PartsList":[{"UniqueId":"u1","DigiKeyPartNumber":"490-1532-1-ND",
	    "PrimaryDatasheetUrl":"//mm.digikey.com/docus/x.pdf",
	    "Flags":{"IsMatched":true},
	    "Quantities":[{"QuantityRequested":1,"SelectedPackOptionIndex":0,
	      "PackOptions":[{"PackType":"CT","DigiKeyPartNumber":"490-1532-1-ND",
	        "CalculatedUnitPrice":0.1,"ExtendedPrice":0.1}]}]}]}`)

	res := runAuthed(t, m, "list", "show", "Bench PSU rev A")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got ListDetail
	res.JSON(t, &got)
	if !strings.HasPrefix(got.Parts[0].DatasheetURL, "https://") {
		t.Errorf("datasheet_url = %q, want an absolute https URL", got.Parts[0].DatasheetURL)
	}
}

func TestNormalizeAssetURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"protocol-relative gains https", "//mm.digikey.com/x.pdf", "https://mm.digikey.com/x.pdf"},
		{"https passes through", "https://example.com/x.pdf", "https://example.com/x.pdf"},
		{"http is left alone", "http://example.com/x.pdf", "http://example.com/x.pdf"},
		{"surrounding space is trimmed", "  //mm.digikey.com/x.pdf  ", "https://mm.digikey.com/x.pdf"},
		{"empty stays empty", "", ""},
		{"whitespace only", "   ", ""},
		// Anything a caller cannot fetch is dropped rather than passed on.
		{"ftp is dropped", "ftp://example.com/x.pdf", ""},
		{"file is dropped", "file:///etc/passwd", ""},
		{"javascript is dropped", "javascript:alert(1)", ""},
		{"bare path is dropped", "media/x.pdf", ""},
		{"query and fragment survive", "//h/x.pdf?a=1#p2", "https://h/x.pdf?a=1#p2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeAssetURL(tt.in); got != tt.want {
				t.Errorf("normalizeAssetURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRecommendedSendsALimit(t *testing.T) {
	// DigiKey defaults recommendedproducts to a single result, and the response
	// carries no total, so a missing limit looks exactly like a part with one
	// recommendation. The flag must reach the wire.
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/recommendedproducts", http.StatusOK,
		`{"Recommendations":[{"RecommendedProducts":[{"DigiKeyProductNumber":"A"},{"DigiKeyProductNumber":"B"}]}]}`)

	res := run(t, m, "product", "X", "--recommended", "--recommended-limit", "25")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var query string
	for _, r := range m.requests {
		if strings.Contains(r.Path, "recommendedproducts") {
			query = r.Query
		}
	}
	if !strings.Contains(query, "limit=25") {
		t.Errorf("query = %q, want limit=25", query)
	}
}

func TestRecommendedDefaultLimitIsNotOne(t *testing.T) {
	// The default has to beat DigiKey's, or the flag is the only way to avoid
	// a silently truncated answer.
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/recommendedproducts", http.StatusOK,
		`{"Recommendations":[]}`)

	if res := run(t, m, "product", "X", "--recommended"); res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	for _, r := range m.requests {
		if strings.Contains(r.Path, "recommendedproducts") {
			if strings.Contains(r.Query, "limit=1&") || strings.HasSuffix(r.Query, "limit=1") || r.Query == "" {
				t.Errorf("query = %q, want a limit above DigiKey's default of 1", r.Query)
			}
		}
	}
}
