package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/output"
)

// maxDownloadBytes caps a single downloaded document. Datasheets run to a few
// megabytes; CAD archives can be larger, but a cap keeps a hostile or
// misconfigured URL from filling the disk.
const maxDownloadBytes = 128 << 20

// DocumentView is one document or asset attached to a product.
type DocumentView struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// Filename is the name --download would write, so a caller can predict the
	// path without downloading first.
	Filename string `json:"filename,omitempty"`
	// Path is set only for documents this run actually wrote to disk.
	Path string `json:"path,omitempty"`
	// Error records a download failure without aborting the other documents.
	Error string `json:"error,omitempty"`
}

// DocsResult is the JSON shape of `dk docs`.
type DocsResult struct {
	PartNumber string         `json:"part_number"`
	Documents  []DocumentView `json:"documents"`
	Downloaded int            `json:"downloaded,omitempty"`
}

func newDocsCommand(app *App) *cobra.Command {
	var (
		docType   string
		download  string
		overwrite bool
	)

	cmd := &cobra.Command{
		Use:     "docs <part-number>",
		Aliases: []string{"media", "datasheet"},
		Short:   "List or download a product's datasheets and documents",
		Long: `List every document DigiKey attaches to a product: datasheets, manuals,
reference designs, CAD models, photos, PCNs, and videos.

  dk docs 490-1532-1-ND
  dk docs STM32G031K8T6 --type datasheet
  dk docs STM32G031K8T6 --type datasheet --download ./datasheets

Without --download this only prints URLs, which is usually enough — the primary
datasheet URL is already included in "dk search" and "dk product" output as
datasheet_url, with no extra API call. Use this command when you want the other
documents, or when you want the PDF on disk.

--download writes each matching document into the given directory and prints
where it landed. Existing files are left alone unless --overwrite is passed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			partNumber := strings.TrimSpace(args[0])
			if partNumber == "" {
				return usageErrorf("part number must not be empty")
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}

			links, err := client.Media(ctx, partNumber)
			if err != nil {
				return err
			}

			result := DocsResult{PartNumber: partNumber, Documents: []DocumentView{}}
			// Two documents on one product routinely share a basename
			// ("datasheet.pdf"). Without disambiguation the second overwrites the
			// first and both are reported as written.
			taken := map[string]int{}
			for _, link := range links {
				u := normalizeAssetURL(link.URL)
				if u == "" {
					continue
				}
				if docType != "" && !strings.Contains(strings.ToLower(link.MediaType), strings.ToLower(docType)) {
					continue
				}
				result.Documents = append(result.Documents, DocumentView{
					Type:     link.MediaType,
					Title:    link.Title,
					URL:      u,
					Filename: uniqueFilename(documentFilename(u, link.Title), taken),
				})
			}

			if download != "" {
				if len(result.Documents) == 0 {
					return &Error{
						Code:     CodeNotFound,
						Message:  fmt.Sprintf("no documents to download for %s", partNumber),
						Hint:     "Run `dk docs " + partNumber + "` without --type to see everything DigiKey lists.",
						ExitCode: ExitNotFound,
					}
				}
				if err := os.MkdirAll(download, 0o755); err != nil {
					return fmt.Errorf("create download directory: %w", err)
				}
				httpc := app.downloadClient()
				for i := range result.Documents {
					doc := &result.Documents[i]
					dest := filepath.Join(download, doc.Filename)
					written, err := downloadDocument(ctx, httpc, doc.URL, dest, overwrite)
					if err != nil {
						doc.Error = err.Error()
						continue
					}
					doc.Path = written
					result.Downloaded++
				}
			}

			if err := app.Printer.Print(result, docsTable(result.Documents, download != "")); err != nil {
				return err
			}

			if download != "" {
				app.Printer.PrintText("\nDownloaded %d of %d document(s) into %s.",
					result.Downloaded, len(result.Documents), download)
			}
			// An interrupt aborts the transfers, but each failure is recorded
			// per-document rather than returned, so without this check Ctrl-C
			// would exit 0 on a half-done job — or report the generic "error"
			// code when nothing finished. Cancellation has its own code.
			if err := ctx.Err(); err != nil {
				return err
			}
			// Surface partial failure rather than exiting 0 on a half-done job.
			if download != "" && result.Downloaded == 0 {
				return &Error{
					Code:     CodeError,
					Message:  "no documents could be downloaded",
					Hint:     "See the ERROR column for why. The URLs are still listed and can be fetched directly.",
					ExitCode: ExitError,
				}
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&docType, "type", "", "only documents whose media type contains this, e.g. datasheet, manual, cad")
	f.StringVar(&download, "download", "", "download matching documents into this directory")
	f.BoolVar(&overwrite, "overwrite", false, "replace files that already exist in the download directory")

	_ = cmd.RegisterFlagCompletionFunc("type", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"datasheet", "manual", "photo", "cad", "video", "pcn"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func docsTable(docs []DocumentView, downloading bool) *output.Table {
	headers := []string{"TYPE", "TITLE", "URL"}
	if downloading {
		headers = []string{"TYPE", "TITLE", "PATH", "ERROR"}
	}
	t := &output.Table{Headers: headers, Empty: "DigiKey lists no documents for this product."}

	for _, d := range docs {
		if downloading {
			t.AddRow(d.Type, output.Truncate(d.Title, 40), d.Path, output.Truncate(d.Error, 48))
			continue
		}
		t.AddRow(d.Type, output.Truncate(d.Title, 40), d.URL)
	}
	return t
}

// normalizeAssetURL repairs the protocol-relative URLs DigiKey returns for some
// assets ("//mm.digikey.com/...") and rejects anything not fetchable over HTTP.
//
// This is not a docs-only concern: roughly 40% of datasheet_url values in a
// sampled search response came back protocol-relative, which no HTTP client
// will fetch as-is. Every URL dk hands a caller goes through here, so what the
// output promises is always something that can actually be retrieved.
//
// A URL that cannot be repaired yields "" and the field is omitted. An absent
// datasheet is a state callers already handle; a present-but-unfetchable one
// just fails later, further from the cause.
func normalizeAssetURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.String()
	default:
		return ""
	}
}

// documentFilename derives a safe local filename for a document.
//
// The name comes from an untrusted remote URL, so it is reduced to a single
// path element with no separators or dot-dot segments: it must never be able to
// escape the download directory.
func documentFilename(rawURL, title string) string {
	name := ""
	if u, err := url.Parse(rawURL); err == nil {
		name = path.Base(u.Path)
	}
	if name == "" || name == "." || name == "/" {
		name = title
	}

	name = sanitizeFilename(name)
	if name == "" {
		name = sanitizeFilename(title)
	}
	if name == "" {
		name = "document"
	}
	return name
}

// uniqueFilename returns name, or a variant with "-2", "-3" ... inserted before
// the extension when an earlier document already claimed it.
//
// Keys are lowercased because the filesystems this most often lands on (APFS,
// NTFS) are case-insensitive: "Datasheet.pdf" and "datasheet.pdf" are the same
// file there, and treating them as distinct would reintroduce the overwrite.
func uniqueFilename(name string, taken map[string]int) string {
	key := strings.ToLower(name)
	n, clash := taken[key]
	if !clash {
		taken[key] = 1
		return name
	}

	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for {
		n++
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		ck := strings.ToLower(candidate)
		if _, exists := taken[ck]; !exists {
			taken[key] = n
			taken[ck] = 1
			return candidate
		}
	}
}

// maxFilenameBytes bounds a derived filename. Most filesystems cap a single
// path element at 255 bytes; this leaves room for the "-2" disambiguation
// suffix uniqueFilename may add.
const maxFilenameBytes = 120

// sanitizeFilename strips directory structure and characters that are unsafe or
// awkward in a filename, leaving a single harmless path element.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	// Collapse to the last element, then defend against separators that survive
	// on the other OS's convention.
	name = filepath.Base(filepath.FromSlash(name))
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")

	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// Drop control characters entirely.
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.Trim(b.String(), " .")

	// Cut on a rune boundary. Slicing bytes can land mid-rune, and the invalid
	// UTF-8 that produces is rejected outright by APFS.
	if len(name) > maxFilenameBytes {
		var cut strings.Builder
		for _, r := range name {
			if cut.Len()+utf8.RuneLen(r) > maxFilenameBytes {
				break
			}
			cut.WriteRune(r)
		}
		// Truncation can expose a trailing dot or space, so re-trim.
		name = strings.Trim(cut.String(), " .")
	}

	// "." and ".." reduce to empty here, which the caller replaces. Checked
	// after truncation, which can also empty the name.
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

// downloadDocument fetches one document to dest, returning the path written.
func downloadDocument(ctx context.Context, httpc *http.Client, rawURL, dest string, overwrite bool) (string, error) {
	if !overwrite {
		if _, err := os.Stat(dest); err == nil {
			return "", errors.New("file already exists (pass --overwrite to replace)")
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	// Documents live on DigiKey's CDN, not the API, so no bearer token is sent.
	req.Header.Set("User-Agent", "dk/"+Version)

	resp, err := httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// Write to a temp file first so an interrupted download cannot leave a
	// truncated PDF that looks complete.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".dk-download-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	written, err := io.Copy(tmp, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("write file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	if written > maxDownloadBytes {
		return "", fmt.Errorf("document exceeds the %d MB download limit", maxDownloadBytes>>20)
	}
	if written == 0 {
		return "", errors.New("document was empty")
	}

	// os.CreateTemp creates the file 0600. A datasheet is not a secret, and one
	// that lands in a shared directory should look like every other download, so
	// widen it before the rename. When replacing an existing file, keep the mode
	// it already had rather than silently tightening it.
	mode := os.FileMode(0o644)
	if info, err := os.Stat(dest); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return "", fmt.Errorf("set file mode: %w", err)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("move into place: %w", err)
	}
	return dest, nil
}
