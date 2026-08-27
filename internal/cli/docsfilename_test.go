package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Two documents on one product routinely share a basename. Without
// disambiguation the second overwrites the first and both are reported as
// written, so the user believes they have two files and has one.
func TestUniqueFilenameDisambiguatesCollisions(t *testing.T) {
	taken := map[string]int{}

	got := []string{
		uniqueFilename("datasheet.pdf", taken),
		uniqueFilename("datasheet.pdf", taken),
		uniqueFilename("datasheet.pdf", taken),
		uniqueFilename("other.pdf", taken),
		// Case-insensitive filesystems treat this as the same file as the first.
		uniqueFilename("DATASHEET.PDF", taken),
		// A name with no extension still has to disambiguate.
		uniqueFilename("readme", taken),
		uniqueFilename("readme", taken),
	}
	want := []string{
		"datasheet.pdf", "datasheet-2.pdf", "datasheet-3.pdf",
		"other.pdf", "DATASHEET-4.PDF",
		"readme", "readme-2",
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}

	// The whole point is that no two results collide on a case-insensitive
	// filesystem.
	seen := map[string]bool{}
	for _, name := range got {
		key := strings.ToLower(name)
		if seen[key] {
			t.Errorf("duplicate filename %q would overwrite an earlier download", name)
		}
		seen[key] = true
	}
}

func TestSanitizeFilenameTruncatesOnRuneBoundary(t *testing.T) {
	// maxFilenameBytes is 120, which is divisible by 2, 3 and 4 — so a uniform
	// repeat of any single multi-byte rune lands exactly ON a boundary and would
	// pass even with a naive name[:120]. Each case below carries an ASCII prefix
	// chosen to push the cut INSIDE a rune, which is the actual defect.
	tests := []struct {
		name string
		in   string
	}{
		// 2-byte runes at an odd offset: the cut falls between é's two bytes.
		{"two-byte runes at an odd offset", "a" + strings.Repeat("é", 200)},
		// 3-byte runes: 120-2 = 118, not a multiple of 3.
		{"three-byte runes", "ab" + strings.Repeat("€", 200)},
		// 4-byte runes, offset by one.
		{"four-byte runes", "x" + strings.Repeat("🔧", 100)},
		// Mixed widths. The group is 10 bytes, which divides 120 evenly, so it
		// needs the prefix too — otherwise this case lands on a boundary and
		// proves nothing.
		{"mixed widths", "z" + strings.Repeat("aé€🔧", 40)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeFilename(tt.in + ".pdf")
			if got == "" {
				t.Fatal("sanitizeFilename returned empty for a long name")
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q — APFS rejects such a name outright", got)
			}
			if len(got) > maxFilenameBytes {
				t.Errorf("result is %d bytes, want at most %d", len(got), maxFilenameBytes)
			}
			// A trailing dot or space exposed by the cut must be trimmed away.
			if strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") {
				t.Errorf("result %q ends in a dot or space", got)
			}
		})
	}
}

// sanitizeFilename's output is used directly as a path element, so whatever
// the remote sends, the result must be a single harmless name or empty (which
// the caller replaces with a default).
//
// Note: truncation can never itself empty the name out. The leading Trim runs
// first, so the surviving string always starts with a non-dot, non-space rune,
// and any prefix of it therefore does too. An earlier version of this test
// claimed to cover that case; it is unreachable, and the test asserted nothing
// the other cases here do not.
func TestSanitizeFilenameAlwaysYieldsASafeElement(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"dots after a long stem", strings.Repeat("a", 130) + strings.Repeat(".", 5)},
		{"only dots", strings.Repeat(".", 130)},
		{"only spaces", strings.Repeat(" ", 130)},
		{"dot dot", ".."},
		{"single dot", "."},
		{"traversal", "../../../etc/passwd"},
		{"windows traversal", `..\..\windows\system32\config`},
		{"absolute path", "/etc/passwd"},
		{"leading dotfile", ".bashrc"},
		{"embedded nul and control chars", "a\x00b\x01c.pdf"},
		{"reserved windows chars", `a<b>c:d"e|f?g*h.pdf`},
		{"interior dots past the cap", "a" + strings.Repeat(".", 200) + "b"},
		{"empty", ""},
		{"whitespace and dots interleaved", " . . . . "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeFilename(tt.in)

			// Empty is a valid answer; the caller substitutes a default.
			if got == "" {
				return
			}
			if got == "." || got == ".." {
				t.Fatalf("returned %q, which is not a usable filename", got)
			}
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("returned %q, which contains a path separator", got)
			}
			if strings.HasPrefix(got, ".") {
				t.Errorf("returned %q, a dotfile the user would not see in a listing", got)
			}
			if strings.ContainsRune(got, 0) {
				t.Errorf("returned %q, which contains a NUL byte", got)
			}
			if len(got) > maxFilenameBytes {
				t.Errorf("returned %d bytes, want at most %d", len(got), maxFilenameBytes)
			}
			// The result must stay inside the download directory when joined.
			if joined := filepath.Join("/downloads", got); !strings.HasPrefix(joined, "/downloads/") {
				t.Errorf("filepath.Join escaped the directory: %q", joined)
			}
		})
	}
}

// --download must not silently replace one document with another when their
// names collide, and every reported path must be a distinct real file.
func TestDocsDownloadDoesNotOverwriteCollidingNames(t *testing.T) {
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body identifies which document it is, so a clobbered file is
		// detectable by content rather than just by count.
		_, _ = w.Write([]byte("body-for" + r.URL.Path))
	}))
	t.Cleanup(files.Close)

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/490-1532-1-ND/media", http.StatusOK, `{"MediaLinks":[
		{"MediaType":"Datasheets","Title":"Primary","Url":"`+files.URL+`/a/datasheet.pdf"},
		{"MediaType":"Datasheets","Title":"Secondary","Url":"`+files.URL+`/b/datasheet.pdf"}
	]}`)

	dir := t.TempDir()
	res := run(t, m, "docs", "490-1532-1-ND", "--download", dir)
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	var got DocsResult
	res.JSON(t, &got)
	if got.Downloaded != 2 {
		t.Fatalf("downloaded = %d, want 2\n%s", got.Downloaded, res.Stdout)
	}

	paths := map[string]bool{}
	for _, d := range got.Documents {
		if d.Path == "" {
			t.Fatalf("document %q reports no path: %+v", d.Title, d)
		}
		if paths[d.Path] {
			t.Errorf("two documents claim the same path %q; one overwrote the other", d.Path)
		}
		paths[d.Path] = true

		if _, err := os.Stat(d.Path); err != nil {
			t.Errorf("reported path %q does not exist: %v", d.Path, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("download dir holds %d files %v, want 2", len(entries), names)
	}
}

// os.CreateTemp creates the file 0600; a datasheet should land with the mode
// every other download has.
func TestDocsDownloadFileMode(t *testing.T) {
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("%PDF-1.4 fake"))
	}))
	t.Cleanup(files.Close)

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/490-1532-1-ND/media", http.StatusOK,
		`{"MediaLinks":[{"MediaType":"Datasheets","Title":"D","Url":"`+files.URL+`/ds.pdf"}]}`)

	dir := t.TempDir()
	res := run(t, m, "docs", "490-1532-1-ND", "--download", dir)
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	info, err := os.Stat(filepath.Join(dir, "ds.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %v, want 0644 (the temp file's 0600 must not leak through)", got)
	}
}

// --overwrite replaces the contents, but must not quietly tighten a mode the
// user chose.
func TestDocsDownloadOverwritePreservesExistingMode(t *testing.T) {
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("new contents"))
	}))
	t.Cleanup(files.Close)

	m := newMockDigiKey(t)
	m.handle("GET", "/products/v4/search/490-1532-1-ND/media", http.StatusOK,
		`{"MediaLinks":[{"MediaType":"Datasheets","Title":"D","Url":"`+files.URL+`/ds.pdf"}]}`)

	dir := t.TempDir()
	dest := filepath.Join(dir, "ds.pdf")
	if err := os.WriteFile(dest, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by the process umask, so under umask 077 the
	// file would land 0600 and this test would assert against the wrong
	// baseline. Chmod is not masked; set the mode explicitly.
	if err := os.Chmod(dest, 0o640); err != nil {
		t.Fatal(err)
	}

	res := run(t, m, "docs", "490-1532-1-ND", "--download", dir, "--overwrite")
	if res.Code != ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", res.Code, res.Stderr)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("file mode = %v, want the pre-existing 0640", got)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new contents" {
		t.Errorf("file contents = %q, want the replacement", body)
	}
}
