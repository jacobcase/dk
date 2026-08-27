package output

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"", FormatAuto, false},
		{"auto", FormatAuto, false},
		{"json", FormatJSON, false},
		{"JSON", FormatJSON, false},
		{"  table  ", FormatTable, false},
		{"csv", FormatCSV, false},
		{"yaml", "", true},
		{"xml", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseFormat(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseFormat(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseFormatErrorListsChoices(t *testing.T) {
	_, err := ParseFormat("bogus")
	if err == nil {
		t.Fatal("ParseFormat(\"bogus\") error = nil")
	}
	for _, want := range Formats() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list the valid format %q", err, want)
		}
	}
}

func TestFormatResolve(t *testing.T) {
	tests := []struct {
		name  string
		f     Format
		isTTY bool
		want  Format
	}{
		// This is the behavior the CLI's default depends on: a human at a
		// terminal gets a table, a program capturing stdout gets JSON.
		{"auto on a terminal", FormatAuto, true, FormatTable},
		{"auto when piped", FormatAuto, false, FormatJSON},
		{"explicit json on a terminal", FormatJSON, true, FormatJSON},
		{"explicit table when piped", FormatTable, false, FormatTable},
		{"explicit csv", FormatCSV, true, FormatCSV},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Resolve(tt.isTTY); got != tt.want {
				t.Errorf("Resolve(%v) = %q, want %q", tt.isTTY, got, tt.want)
			}
		})
	}
}

func TestIsTTY(t *testing.T) {
	if IsTTY(&bytes.Buffer{}) {
		t.Error("IsTTY(bytes.Buffer) = true, want false")
	}

	// A pipe is a real *os.File but not a character device.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTTY(w) {
		t.Error("IsTTY(pipe) = true, want false")
	}
}

func TestPrintJSON(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: FormatJSON, Out: &buf}

	data := map[string]any{"name": "R & D <part>", "count": 2}
	if err := p.Print(data, nil); err != nil {
		t.Fatalf("Print() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid json: %v\n%s", err, buf.String())
	}
	// HTML escaping would turn & and < into entities and corrupt part numbers
	// and descriptions on copy/paste.
	if !strings.Contains(buf.String(), "R & D <part>") {
		t.Errorf("output = %s, want the ampersand and angle brackets unescaped", buf.String())
	}
}

func TestPrintJSONIgnoresTable(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: FormatJSON, Out: &buf}

	table := &Table{Headers: []string{"A"}}
	table.AddRow("should not appear")

	if err := p.Print(map[string]string{"k": "v"}, table); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "should not appear") {
		t.Errorf("json output leaked table content: %s", buf.String())
	}
}

func TestPrintTable(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: FormatTable, Out: &buf}

	table := &Table{Headers: []string{"DKPN", "STOCK"}}
	table.AddRow("490-1532-1-ND", 250000)
	table.AddRow("311-10.0K-ND", 12)

	if err := p.Print(nil, table); err != nil {
		t.Fatalf("Print() error = %v", err)
	}

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header plus two rows:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "DKPN") {
		t.Errorf("first line = %q, want the header row", lines[0])
	}
	if !strings.Contains(lines[1], "250000") {
		t.Errorf("row = %q, want the integer rendered", lines[1])
	}
	// tabwriter should have padded the columns into alignment.
	if strings.Index(lines[1], "250000") != strings.Index(lines[2], "12") {
		t.Errorf("columns are not aligned:\n%s", out)
	}
}

func TestPrintTableEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: FormatTable, Out: &buf}

	table := &Table{Headers: []string{"A"}, Empty: "No products matched."}
	if err := p.Print(nil, table); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "No products matched." {
		t.Errorf("output = %q, want the empty message", got)
	}
}

func TestPrintCSVKeepsHeaderWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: FormatCSV, Out: &buf}

	table := &Table{Headers: []string{"A", "B"}, Empty: "nothing here"}
	if err := p.Print(nil, table); err != nil {
		t.Fatal(err)
	}
	// A CSV consumer needs the header row even with zero data rows; the Empty
	// prose would break parsing.
	if got := strings.TrimSpace(buf.String()); got != "A,B" {
		t.Errorf("output = %q, want just the header row", got)
	}
}

func TestPrintCSVQuotesFields(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: FormatCSV, Out: &buf}

	table := &Table{Headers: []string{"DESC", "REF"}}
	table.AddRow(`CAP CER 0.1UF, 16V`, `C1,C2`)

	if err := p.Print(nil, table); err != nil {
		t.Fatal(err)
	}

	records, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid csv: %v\n%s", err, buf.String())
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	// Descriptions and reference designator lists both contain commas.
	if records[1][0] != "CAP CER 0.1UF, 16V" || records[1][1] != "C1,C2" {
		t.Errorf("round trip = %v, want the commas preserved inside fields", records[1])
	}
}

func TestPrintRequiresTableForNonJSON(t *testing.T) {
	for _, format := range []Format{FormatTable, FormatCSV} {
		var buf bytes.Buffer
		p := &Printer{Format: format, Out: &buf}
		if err := p.Print(map[string]string{"a": "b"}, nil); err == nil {
			t.Errorf("Print(nil table) with format %q error = nil, want an error rather than silent no output", format)
		}
	}
}

func TestPrintTextSuppressedInMachineFormats(t *testing.T) {
	for _, format := range []Format{FormatJSON, FormatCSV} {
		var out, errOut bytes.Buffer
		p := &Printer{Format: format, Out: &out, Err: &errOut}
		p.PrintText("Added %d parts.", 3)
		// Prose on stdout would break a JSON or CSV parser downstream.
		if out.Len() != 0 {
			t.Errorf("format %q: PrintText wrote %q to stdout, want nothing", format, out.String())
		}
		if errOut.Len() != 0 {
			t.Errorf("format %q: PrintText wrote %q to stderr, want nothing", format, errOut.String())
		}
	}
}

func TestPrintTextGoesToStderrNotStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	p := &Printer{Format: FormatTable, Out: &out, Err: &errOut}
	p.PrintText("Added %d parts.", 3)

	if got := strings.TrimSpace(errOut.String()); got != "Added 3 parts." {
		t.Errorf("PrintText() wrote %q to stderr, want %q", got, "Added 3 parts.")
	}
	// The contract is that stdout carries only the result, in every format —
	// redirecting table output to a file must not capture the prose with it.
	if out.Len() != 0 {
		t.Errorf("PrintText() wrote %q to stdout, want nothing", out.String())
	}
}

// A Printer built without an Err writer still has to print somewhere rather
// than dropping the message on the floor.
func TestPrintTextFallsBackToOutWithoutErr(t *testing.T) {
	var out bytes.Buffer
	p := &Printer{Format: FormatTable, Out: &out}
	p.PrintText("Added %d parts.", 3)
	if got := strings.TrimSpace(out.String()); got != "Added 3 parts." {
		t.Errorf("PrintText() with nil Err wrote %q, want %q", got, "Added 3 parts.")
	}
}

func TestCell(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"string", "abc", "abc"},
		{"int", 42, "42"},
		{"bool", true, "true"},
		{"float trims trailing zeros", 0.5, "0.5"},
		{"newlines flattened", "line1\nline2", "line1 line2"},
		{"crlf flattened", "a\r\nb", "a b"},
		{"tabs flattened", "a\tb", "a b"},
		{"whitespace trimmed", "  x  ", "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cell(tt.in); got != tt.want {
				t.Errorf("Cell(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCellFlatteningProtectsTableAlignment(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: FormatTable, Out: &buf}

	table := &Table{Headers: []string{"DESC", "QTY"}}
	// A DigiKey detailed description can contain embedded newlines; unflattened
	// they would split one row across several lines.
	table.AddRow("multi\nline\ndescription", 5)

	if err := p.Print(nil, table); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want a header plus one row:\n%s", len(lines), buf.String())
	}
}

func TestMoney(t *testing.T) {
	// Zero means "DigiKey did not quote a price", which is different from free.
	// It must stay blank rather than a placeholder: these columns reach CSV,
	// where a non-numeric price field breaks consumers downstream.
	if got := Money(0); got != "" {
		t.Errorf("Money(0) = %q, want an empty string", got)
	}
	if got := Money(0.0483); got != "0.0483" {
		t.Errorf("Money(0.0483) = %q, want %q", got, "0.0483")
	}
	if got := Money(12.5); got != "12.5000" {
		t.Errorf("Money(12.5) = %q, want %q", got, "12.5000")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-ten", 11, "exactly-ten"},
		{"truncate me please", 8, "truncat…"},
		{"disabled", 0, "disabled"},
		{"disabled", -1, "disabled"},
	}
	for _, tt := range tests {
		if got := Truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestTruncateIsRuneAware(t *testing.T) {
	// Byte-slicing a multi-byte string would emit invalid UTF-8.
	got := Truncate("Ω±µF résistance", 6)
	if len([]rune(got)) != 6 {
		t.Errorf("Truncate() = %q with %d runes, want 6", got, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Truncate() = %q, want an ellipsis suffix", got)
	}
}

func TestKeyValueTableSkipsEmptyValues(t *testing.T) {
	table := KeyValueTable([][2]string{
		{"a", "1"},
		{"b", ""},
		{"c", "3"},
	})
	if len(table.Rows) != 2 {
		t.Fatalf("got %d rows, want 2: blank values should be dropped", len(table.Rows))
	}
	if table.Rows[0][0] != "a" || table.Rows[1][0] != "c" {
		t.Errorf("rows = %v, want a and c in order", table.Rows)
	}
}

func TestNewPrinterResolvesAgainstDestination(t *testing.T) {
	// A bytes.Buffer is not a terminal, so auto must resolve to json.
	p := NewPrinter(FormatAuto, &bytes.Buffer{}, &bytes.Buffer{})
	if p.Format != FormatJSON {
		t.Errorf("NewPrinter(auto, buffer).Format = %q, want json", p.Format)
	}
}
