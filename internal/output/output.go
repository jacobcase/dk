// Package output renders command results as JSON, an aligned table, or CSV.
//
// Commands build both a structured value (for JSON) and a Table (for the human
// formats), then hand both to a Printer. Keeping the two side by side means the
// JSON shape never silently drifts from what a human sees.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Format is an output encoding.
type Format string

// Supported formats. FormatAuto resolves to table on a terminal and json
// otherwise, so piping into an agent yields machine-readable output by default.
const (
	FormatAuto  Format = "auto"
	FormatJSON  Format = "json"
	FormatTable Format = "table"
	FormatCSV   Format = "csv"
)

// Formats lists the values accepted by --output.
func Formats() []string {
	return []string{string(FormatAuto), string(FormatJSON), string(FormatTable), string(FormatCSV)}
}

// ParseFormat validates a --output value.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case "", FormatAuto:
		return FormatAuto, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatTable:
		return FormatTable, nil
	case FormatCSV:
		return FormatCSV, nil
	default:
		return "", fmt.Errorf("invalid output format %q (want one of: %s)", s, strings.Join(Formats(), ", "))
	}
}

// Resolve turns FormatAuto into a concrete format. isTTY reports whether the
// destination is an interactive terminal.
func (f Format) Resolve(isTTY bool) Format {
	if f != FormatAuto {
		return f
	}
	if isTTY {
		return FormatTable
	}
	return FormatJSON
}

// IsTTY reports whether w is a character device, i.e. an interactive terminal.
// Anything that is not an *os.File (a test buffer, a pipe) is not a TTY.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Table is a rectangular result set for the human-readable formats.
type Table struct {
	Headers []string
	Rows    [][]string
	// Empty is printed instead of an empty table body in table format. It is
	// omitted from CSV, which should stay parseable even with zero rows.
	Empty string
}

// AddRow appends a row, coercing values to strings.
func (t *Table) AddRow(cells ...any) {
	row := make([]string, len(cells))
	for i, c := range cells {
		row[i] = Cell(c)
	}
	t.Rows = append(t.Rows, row)
}

// Cell formats a single value for display, normalizing whitespace so a
// multi-line description cannot break table alignment.
func Cell(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		s = ""
	case string:
		s = x
	case bool:
		s = strconv.FormatBool(x)
	case int:
		s = strconv.Itoa(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	default:
		s = fmt.Sprint(x)
	}
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

// Money formats a price for display. Prices of exactly zero usually mean
// "DigiKey did not quote one", so they render as a dash rather than $0.00.
func Money(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.4f", v)
}

// Truncate shortens s to at most n runes, appending an ellipsis. It is
// rune-aware so multi-byte descriptions are not cut mid-character.
func Truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// Printer writes results in the resolved format.
type Printer struct {
	Format Format
	Out    io.Writer
	// NoTruncate disables column width limits in table format.
	NoTruncate bool
}

// NewPrinter resolves format against out's TTY-ness and returns a Printer.
func NewPrinter(format Format, out io.Writer) *Printer {
	return &Printer{Format: format.Resolve(IsTTY(out)), Out: out}
}

// Print writes data as JSON, or table as a table/CSV, depending on the format.
// Passing a nil table with a non-JSON format is a programming error and yields
// an error rather than silent no-output.
func (p *Printer) Print(data any, table *Table) error {
	switch p.Format {
	case FormatJSON:
		return p.printJSON(data)
	case FormatCSV:
		if table == nil {
			return fmt.Errorf("output: csv format requested but command produced no table")
		}
		return p.printCSV(table)
	default:
		if table == nil {
			return fmt.Errorf("output: table format requested but command produced no table")
		}
		return p.printTable(table)
	}
}

func (p *Printer) printJSON(data any) error {
	enc := json.NewEncoder(p.Out)
	enc.SetIndent("", "  ")
	// Part numbers and descriptions contain characters like & and < that
	// DigiKey returns verbatim; escaping them would corrupt copy/paste.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

func (p *Printer) printTable(t *Table) error {
	if len(t.Rows) == 0 && t.Empty != "" {
		_, err := fmt.Fprintln(p.Out, t.Empty)
		return err
	}

	tw := tabwriter.NewWriter(p.Out, 0, 4, 2, ' ', 0)
	if len(t.Headers) > 0 {
		if _, err := fmt.Fprintln(tw, strings.Join(t.Headers, "\t")); err != nil {
			return err
		}
	}
	for _, row := range t.Rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (p *Printer) printCSV(t *Table) error {
	w := csv.NewWriter(p.Out)
	if len(t.Headers) > 0 {
		if err := w.Write(t.Headers); err != nil {
			return err
		}
	}
	for _, row := range t.Rows {
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// PrintText writes a plain message, but only in human-readable formats. It is
// for confirmations ("Created list X") that would otherwise pollute JSON
// output consumed by another program.
func (p *Printer) PrintText(format string, args ...any) {
	if p.Format == FormatJSON || p.Format == FormatCSV {
		return
	}
	fmt.Fprintf(p.Out, format+"\n", args...)
}

// KeyValueTable renders an ordered key/value view, used by detail commands.
func KeyValueTable(pairs [][2]string) *Table {
	t := &Table{Headers: []string{"FIELD", "VALUE"}}
	for _, kv := range pairs {
		if kv[1] == "" {
			continue
		}
		t.AddRow(kv[0], kv[1])
	}
	return t
}
