package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mediaResponseBody = `{
  "MediaLinks": [
    {"MediaType":"Datasheets","Title":"GRM Series Datasheet",
     "Url":"https://mm.digikey.com/Volume0/opasdata/d220001/derivates/6/001/234/GRM.pdf"},
    {"MediaType":"Datasheets","Title":"Reliability Report",
     "Url":"//mm.digikey.com/Volume0/opasdata/reliability.pdf"},
    {"MediaType":"Product Photos","Title":"0603 Package",
     "Url":"https://mm.digikey.com/photos/0603.jpg"},
    {"MediaType":"EDA / CAD Models","Title":"Ultra Librarian",
     "Url":"https://app.ultralibrarian.com/details/abc"},
    {"MediaType":"Videos","Title":"Overview","Url":""}
  ]
}`

func TestDocsListsDocuments(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/490-1532-1-ND/media", http.StatusOK, mediaResponseBody)

	res := run(t, m, "docs", "490-1532-1-ND")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got DocsResult
	res.JSON(t, &got)
	// The entry with an empty URL is dropped; the other four survive.
	if len(got.Documents) != 4 {
		t.Fatalf("got %d documents, want 4:\n%s", len(got.Documents), res.Stdout)
	}
	if got.Documents[0].Type != "Datasheets" || got.Documents[0].Title != "GRM Series Datasheet" {
		t.Errorf("first document = %+v", got.Documents[0])
	}
	if got.Documents[0].Filename != "GRM.pdf" {
		t.Errorf("filename = %q, want GRM.pdf derived from the URL", got.Documents[0].Filename)
	}
}

func TestDocsNormalizesProtocolRelativeURL(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK, mediaResponseBody)

	res := run(t, m, "docs", "X")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d", res.Code)
	}
	var got DocsResult
	res.JSON(t, &got)

	// DigiKey returns some asset URLs protocol-relative; left as-is they are
	// not fetchable.
	if want := "https://mm.digikey.com/Volume0/opasdata/reliability.pdf"; got.Documents[1].URL != want {
		t.Errorf("url = %q, want %q", got.Documents[1].URL, want)
	}
}

func TestDocsTypeFilter(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK, mediaResponseBody)

	res := run(t, m, "docs", "X", "--type", "datasheet")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	var got DocsResult
	res.JSON(t, &got)
	if len(got.Documents) != 2 {
		t.Fatalf("got %d documents, want the 2 datasheets", len(got.Documents))
	}
	for _, d := range got.Documents {
		if d.Type != "Datasheets" {
			t.Errorf("document type = %q, want only datasheets", d.Type)
		}
	}
}

func TestDocsEmptyMedia(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK, `{"MediaLinks":[]}`)

	res := run(t, m, "docs", "X")
	// No documents is a valid answer, not a failure.
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	var got DocsResult
	res.JSON(t, &got)
	if len(got.Documents) != 0 {
		t.Errorf("documents = %+v, want none", got.Documents)
	}
}

func TestDocsNotFound(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/NOPE/media", http.StatusNotFound,
		`{"StatusCode":404,"ErrorMessage":"Product not found"}`)

	res := run(t, m, "docs", "NOPE")
	if res.Code != ExitNotFound {
		t.Errorf("exit code = %d, want %d", res.Code, ExitNotFound)
	}
}

// fileServer serves fixed bodies for document downloads.
func fileServer(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDocsDownload(t *testing.T) {
	files := fileServer(t, map[string]string{
		"/datasheets/GRM.pdf": "%PDF-1.4 fake datasheet",
	})

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK,
		`{"MediaLinks":[{"MediaType":"Datasheets","Title":"Datasheet","Url":"`+
			files.URL+`/datasheets/GRM.pdf"}]}`)

	dir := filepath.Join(t.TempDir(), "sheets")
	res := run(t, m, "docs", "X", "--download", dir)
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got DocsResult
	res.JSON(t, &got)
	if got.Downloaded != 1 {
		t.Fatalf("downloaded = %d, want 1 (errors: %+v)", got.Downloaded, got.Documents)
	}

	want := filepath.Join(dir, "GRM.pdf")
	if got.Documents[0].Path != want {
		t.Errorf("path = %q, want %q", got.Documents[0].Path, want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("downloaded file is not readable: %v", err)
	}
	if string(data) != "%PDF-1.4 fake datasheet" {
		t.Errorf("file contents = %q", data)
	}
}

func TestDocsDownloadSkipsExistingWithoutOverwrite(t *testing.T) {
	files := fileServer(t, map[string]string{"/GRM.pdf": "new contents"})

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK,
		`{"MediaLinks":[{"MediaType":"Datasheets","Title":"D","Url":"`+files.URL+`/GRM.pdf"}]}`)

	dir := t.TempDir()
	existing := filepath.Join(dir, "GRM.pdf")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := run(t, m, "docs", "X", "--download", dir)
	// Nothing downloaded means a non-zero exit, so a caller is not told the
	// job succeeded when no file was written.
	if res.Code != ExitError {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitError, res.Stderr)
	}
	data, _ := os.ReadFile(existing)
	if string(data) != "original" {
		t.Errorf("existing file was overwritten without --overwrite: %q", data)
	}
}

func TestDocsDownloadOverwrite(t *testing.T) {
	files := fileServer(t, map[string]string{"/GRM.pdf": "new contents"})

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK,
		`{"MediaLinks":[{"MediaType":"Datasheets","Title":"D","Url":"`+files.URL+`/GRM.pdf"}]}`)

	dir := t.TempDir()
	existing := filepath.Join(dir, "GRM.pdf")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := run(t, m, "docs", "X", "--download", dir, "--overwrite")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}
	data, _ := os.ReadFile(existing)
	if string(data) != "new contents" {
		t.Errorf("file = %q, want it replaced", data)
	}
}

func TestDocsDownloadPartialFailureIsReported(t *testing.T) {
	files := fileServer(t, map[string]string{"/good.pdf": "ok"})

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK,
		`{"MediaLinks":[
		   {"MediaType":"Datasheets","Title":"Good","Url":"`+files.URL+`/good.pdf"},
		   {"MediaType":"Datasheets","Title":"Missing","Url":"`+files.URL+`/gone.pdf"}
		 ]}`)

	dir := t.TempDir()
	res := run(t, m, "docs", "X", "--download", dir)
	// One success is still a success overall; the failure is reported per-document.
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}

	var got DocsResult
	res.JSON(t, &got)
	if got.Downloaded != 1 {
		t.Errorf("downloaded = %d, want 1", got.Downloaded)
	}
	if got.Documents[1].Error == "" {
		t.Error("the failed document should carry an error, not be silently dropped")
	}
	if got.Documents[1].Path != "" {
		t.Error("a failed document should have no path")
	}
}

func TestDocsDownloadLeavesNoTempFilesOnFailure(t *testing.T) {
	files := fileServer(t, map[string]string{})

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK,
		`{"MediaLinks":[{"MediaType":"Datasheets","Title":"Gone","Url":"`+files.URL+`/gone.pdf"}]}`)

	dir := t.TempDir()
	run(t, m, "docs", "X", "--download", dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The atomic-write temp file must be cleaned up.
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("download dir contains %v, want it empty after a failed download", names)
	}
}

func TestDocsDownloadWithNoMatchesExits4(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/X/media", http.StatusOK, mediaResponseBody)

	res := run(t, m, "docs", "X", "--type", "schematic", "--download", t.TempDir())
	if res.Code != ExitNotFound {
		t.Errorf("exit code = %d, want %d", res.Code, ExitNotFound)
	}
}

func TestNormalizeMediaURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https passes through", "https://mm.digikey.com/a.pdf", "https://mm.digikey.com/a.pdf"},
		{"http passes through", "http://mm.digikey.com/a.pdf", "http://mm.digikey.com/a.pdf"},
		{"protocol relative gets https", "//mm.digikey.com/a.pdf", "https://mm.digikey.com/a.pdf"},
		{"whitespace trimmed", "  https://x/a.pdf  ", "https://x/a.pdf"},
		{"empty", "", ""},
		// A javascript: or file: URL must never reach the downloader.
		{"javascript rejected", "javascript:alert(1)", ""},
		{"file rejected", "file:///etc/passwd", ""},
		{"relative rejected", "/just/a/path.pdf", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeMediaURL(tt.in); got != tt.want {
				t.Errorf("normalizeMediaURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDocumentFilenameIsContained(t *testing.T) {
	// Filenames come from remote URLs, so none of these may escape the
	// download directory or name a hidden/parent path.
	tests := []struct {
		name  string
		url   string
		title string
	}{
		{"traversal in path", "https://x/../../../etc/passwd", "t"},
		{"encoded traversal", "https://x/%2e%2e%2f%2e%2e%2fetc%2fpasswd", "t"},
		{"absolute path", "https://x//etc/passwd", "t"},
		{"dot segment", "https://x/.", "t"},
		{"trailing slash", "https://x/dir/", "t"},
		{"backslashes in title", "https://x/", `..\..\windows\system32\evil`},
		{"empty everything", "https://x/", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := documentFilename(tt.url, tt.title)
			if got == "" {
				t.Fatal("documentFilename() returned an empty name")
			}
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("filename %q contains a path separator", got)
			}
			if got == "." || got == ".." || strings.HasPrefix(got, "..") {
				t.Errorf("filename %q is a parent-directory reference", got)
			}
			// The decisive check: joining it must stay inside the directory.
			joined := filepath.Join("/downloads", got)
			if !strings.HasPrefix(joined, "/downloads/") {
				t.Errorf("filepath.Join(/downloads, %q) = %q, which escapes the directory", got, joined)
			}
		})
	}
}

func TestDocumentFilenamePrefersURLBase(t *testing.T) {
	got := documentFilename("https://mm.digikey.com/a/b/GRM188.pdf", "Some Long Title")
	if got != "GRM188.pdf" {
		t.Errorf("documentFilename() = %q, want the URL basename", got)
	}
}

func TestDocumentFilenameFallsBackToTitle(t *testing.T) {
	got := documentFilename("https://mm.digikey.com/", "Reliability Report")
	if got != "Reliability Report" {
		t.Errorf("documentFilename() = %q, want the title when the URL has no basename", got)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ in, want string }{
		{"normal.pdf", "normal.pdf"},
		{"  spaced.pdf  ", "spaced.pdf"},
		{"with/slash.pdf", "slash.pdf"},
		// A backslash is a legal filename character on unix, so it is
		// neutralized rather than split on. Either way no separator survives,
		// which is what containment depends on.
		{`with\backslash.pdf`, "with_backslash.pdf"},
		{"bad<>:\"|?*chars.pdf", "bad_______chars.pdf"},
		{"..", ""},
		{".", ""},
		{"", ""},
		{"trailing dots...", "trailing dots"},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.in); got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeFilenameStripsControlCharacters(t *testing.T) {
	got := sanitizeFilename("data\x00sheet\x1b.pdf")
	if strings.ContainsAny(got, "\x00\x1b") {
		t.Errorf("sanitizeFilename() = %q, want control characters removed", got)
	}
}

func TestSanitizeFilenameCapsLength(t *testing.T) {
	got := sanitizeFilename(strings.Repeat("a", 500) + ".pdf")
	if len(got) > 120 {
		t.Errorf("sanitizeFilename() returned %d bytes, want it capped at 120", len(got))
	}
}

func TestDownloadDocumentRejectsOversizedBody(t *testing.T) {
	// Serve more than the cap allows, in a body the limit reader will cut off.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 1<<20)
		for range (maxDownloadBytes >> 20) + 2 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "big.bin")
	_, err := downloadDocument(context.Background(), srv.Client(), srv.URL+"/big.bin", dest, false)
	if err == nil {
		t.Fatal("downloadDocument() error = nil, want the size cap enforced")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want it to mention the limit", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("an oversized download left a file behind")
	}
}

func TestDownloadDocumentRejectsEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "empty.pdf")
	if _, err := downloadDocument(context.Background(), srv.Client(), srv.URL, dest, false); err == nil {
		t.Fatal("downloadDocument() error = nil, want an empty response rejected")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("an empty download created a file")
	}
}

func TestDownloadDocumentSendsNoBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "pdf")
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "a.pdf")
	if _, err := downloadDocument(context.Background(), srv.Client(), srv.URL, dest, false); err != nil {
		t.Fatal(err)
	}
	// Documents live on a CDN, not the API. Leaking the token there would be
	// sending a credential to a host that has no business seeing it.
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want none sent to the CDN", gotAuth)
	}
}
