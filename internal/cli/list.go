package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// listWebURLPrefix builds the digikey.com page for a list, so a human can open
// what an agent just assembled and place the order.
const listWebURLPrefix = "https://www.digikey.com/en/mylists/list/"

// ListView is the JSON shape of a list's metadata.
type ListView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	TotalParts   int      `json:"total_parts"`
	Tags         []string `json:"tags,omitempty"`
	Notes        string   `json:"notes,omitempty"`
	DateCreated  string   `json:"date_created,omitempty"`
	DateModified string   `json:"date_modified,omitempty"`
	CanEdit      bool     `json:"can_edit"`
	URL          string   `json:"url"`
}

func newListView(l digikey.ListSummary) ListView {
	return ListView{
		ID:           l.ID,
		Name:         l.ListName,
		TotalParts:   l.TotalParts,
		Tags:         l.Tags,
		Notes:        l.Notes,
		DateCreated:  l.DateCreated,
		DateModified: l.DateModified,
		CanEdit:      l.CanEdit,
		URL:          listWebURLPrefix + l.ID,
	}
}

// ListPartView is the JSON shape of one line in a list.
type ListPartView struct {
	UniqueID               string  `json:"unique_id"`
	DigiKeyPartNumber      string  `json:"digikey_part_number"`
	RequestedPartNumber    string  `json:"requested_part_number,omitempty"`
	ManufacturerPartNumber string  `json:"manufacturer_part_number,omitempty"`
	Manufacturer           string  `json:"manufacturer,omitempty"`
	Description            string  `json:"description,omitempty"`
	Quantity               int     `json:"quantity"`
	UnitPrice              float64 `json:"unit_price"`
	ExtendedPrice          float64 `json:"extended_price"`
	QuantityAvailable      int     `json:"quantity_available"`
	ReferenceDesignator    string  `json:"reference_designator,omitempty"`
	CustomerReference      string  `json:"customer_reference,omitempty"`
	Notes                  string  `json:"notes,omitempty"`
	Status                 string  `json:"status,omitempty"`
	DatasheetURL           string  `json:"datasheet_url,omitempty"`
	// Matched is false when DigiKey could not map the requested part number to a
	// catalog product. Those lines are not orderable and need fixing.
	Matched bool `json:"matched"`
}

func newListPartView(p digikey.ListPart) ListPartView {
	return ListPartView{
		UniqueID:               p.UniqueID,
		DigiKeyPartNumber:      p.OrderablePartNumber(),
		RequestedPartNumber:    p.RequestedPartNumber,
		ManufacturerPartNumber: p.ManufacturerPartNumber,
		Manufacturer:           p.Manufacturer,
		Description:            p.Description,
		Quantity:               p.RequestedQty(),
		UnitPrice:              p.UnitPrice(),
		ExtendedPrice:          p.ExtendedPrice(),
		QuantityAvailable:      p.QuantityAvailable,
		ReferenceDesignator:    p.ReferenceDesignator,
		CustomerReference:      p.CustomerReference,
		Notes:                  p.Notes,
		Status:                 p.PartStatus,
		DatasheetURL:           normalizeAssetURL(p.PrimaryDatasheetURL),
		Matched:                p.Flags.IsMatched || p.DigiKeyPartNumber != "",
	}
}

// ListDetail is the JSON shape of `dk list show`.
type ListDetail struct {
	ListView
	Currency string         `json:"currency,omitempty"`
	Parts    []ListPartView `json:"parts"`
	// Returned is how many parts are in Parts. It differs from TotalParts only
	// when an explicit --limit/--offset asked for a single page, and exists so
	// that case is visible rather than looking like a complete list.
	Returned int `json:"returned"`
	// EstimatedTotal sums the resolved line totals of the parts actually
	// returned. It excludes shipping, tax, and any line DigiKey could not price,
	// so treat it as a rough figure.
	EstimatedTotal float64 `json:"estimated_total"`
	UnmatchedParts int     `json:"unmatched_parts"`
	// UnpricedParts counts lines that matched a catalog product but carry no
	// price — typically a pack type DigiKey did not quote at that quantity.
	// Each contributes 0 to EstimatedTotal, so without this count the total
	// would understate the real cost with nothing to indicate it.
	UnpricedParts int `json:"unpriced_parts"`
}

func newListCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Create and manage DigiKey MyLists order lists",
		Long: `Create and manage DigiKey "MyLists" order lists.

Lists are the staging area for a project's bill of materials. dk never places an
order: build the list here, then open it on digikey.com to review and buy.

  dk list create "Bench PSU rev A"
  dk list add "Bench PSU rev A" 1276-1000-1-ND:10 --ref C1-C10
  dk list show "Bench PSU rev A"
  dk list export "Bench PSU rev A" --output csv > bom.csv

Every subcommand accepts a list name or a list id. Names are matched exactly
first, then case-insensitively; an ambiguous name is an error rather than a
guess.

These commands require a 3-legged token because lists belong to your DigiKey
account. Run "dk auth login" once; the refresh token does not expire.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		newListLsCommand(app),
		newListCreateCommand(app),
		newListShowCommand(app),
		newListAddCommand(app),
		newListSetCommand(app),
		newListRemoveCommand(app),
		newListRenameCommand(app),
		newListCopyCommand(app),
		newListDeleteCommand(app),
		newListExportCommand(app),
	)
	return cmd
}

// SetResult is the JSON shape of `dk list set`.
type SetResult struct {
	ListID   string `json:"list_id"`
	ListName string `json:"list_name"`
	UniqueID string `json:"unique_id"`
	Part     string `json:"part"`
	// Changed names the fields this call actually modified.
	Changed []string      `json:"changed"`
	Before  ListEntryView `json:"before"`
	After   ListEntryView `json:"after"`
	URL     string        `json:"url"`
}

// ListEntryView is the editable state of one line in a list.
type ListEntryView struct {
	Quantity            int    `json:"quantity"`
	ReferenceDesignator string `json:"reference_designator,omitempty"`
	CustomerReference   string `json:"customer_reference,omitempty"`
	Notes               string `json:"notes,omitempty"`
}

func entryView(p digikey.RequestedPart) ListEntryView {
	total := 0
	for _, q := range p.Quantities {
		total += q.Quantity
	}
	return ListEntryView{
		Quantity:            total,
		ReferenceDesignator: p.ReferenceDesignator,
		CustomerReference:   p.CustomerReference,
		Notes:               p.Notes,
	}
}

func newListSetCommand(app *App) *cobra.Command {
	var (
		qty     int
		ref     string
		note    string
		custRef string
	)

	cmd := &cobra.Command{
		Use:     "set <list> <unique-id-or-part>",
		Aliases: []string{"update", "edit"},
		Short:   "Change the quantity or metadata of a part already in a list",
		Long: `Edit a line that is already in a list, in place.

  dk list set "Bench PSU rev A" 1276-1000-1-ND --qty 20
  dk list set "Bench PSU rev A" 1276-1000-1-ND --ref C1-C20 --note "bulk decoupling"

The part is identified the same way as in "dk list rm": by unique id or by any
part number on the line. Unlike rm, an ambiguous match is an error rather than
applying to every matching line.

Only the flags you pass are changed; everything else on the line is preserved.
Passing an empty value clears a field, so --ref "" removes the reference
designators rather than leaving them alone.

This is a genuine edit, so the unique id survives — removing and re-adding would
mint a new one and lose the reference designators and notes.`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			listRef, target := args[0], args[1]

			// Whether a flag was passed, not whether its value is non-empty:
			// --ref "" has to mean "clear it", not "leave it alone".
			if !cmd.Flags().Changed("qty") && !cmd.Flags().Changed("ref") &&
				!cmd.Flags().Changed("note") && !cmd.Flags().Changed("customer-ref") {
				return usageErrorf("nothing to change; pass at least one of --qty, --ref, --note, or --customer-ref")
			}
			if cmd.Flags().Changed("qty") && qty < 1 {
				return usageErrorf("--qty must be at least 1; use `dk list rm` to remove a part")
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			summary, err := client.ResolveList(ctx, listRef)
			if err != nil {
				return err
			}

			// Resolve the target against the fully-resolved parts, which carry
			// every part number the user might have typed. Every page of them:
			// a part on page two would otherwise report as not found.
			parts, err := client.AllListParts(ctx, summary.ID, app.locale())
			if err != nil {
				return err
			}
			uniqueIDs := matchListEntries(parts.PartsList, target)
			switch len(uniqueIDs) {
			case 0:
				return &Error{
					Code:     CodeNotFound,
					Message:  fmt.Sprintf("no part matching %q in list %q", target, summary.ListName),
					Hint:     "Run `dk list show " + summary.ListName + "` to see what is in the list.",
					ExitCode: ExitNotFound,
				}
			case 1:
			default:
				return usageErrorf("%q matches %d lines in %q; pass the unique id instead (see `dk list show --output json`)",
					target, len(uniqueIDs), summary.ListName)
			}
			uniqueID := uniqueIDs[0]

			// DigiKey's update is a replace, not a patch, so read the existing
			// RequestedPart and modify it rather than sending a partial object.
			full, err := client.GetList(ctx, summary.ID)
			if err != nil {
				return err
			}
			existing, ok := findRequestedPart(full.PartsList, uniqueID)
			if !ok {
				return &Error{
					Code:     CodeNotFound,
					Message:  fmt.Sprintf("list %q has no entry with unique id %s", summary.ListName, uniqueID),
					ExitCode: ExitNotFound,
				}
			}

			before := entryView(existing)
			updated := existing
			var changed []string

			if cmd.Flags().Changed("qty") {
				updated.Quantities = setQuantity(updated.Quantities, qty)
				changed = append(changed, "quantity")
			}
			if cmd.Flags().Changed("ref") {
				updated.ReferenceDesignator = ref
				changed = append(changed, "reference_designator")
			}
			if cmd.Flags().Changed("customer-ref") {
				updated.CustomerReference = custRef
				changed = append(changed, "customer_reference")
			}
			if cmd.Flags().Changed("note") {
				updated.Notes = note
				changed = append(changed, "notes")
			}

			if err := client.UpdatePart(ctx, summary.ID, uniqueID, updated); err != nil {
				return err
			}

			result := SetResult{
				ListID:   summary.ID,
				ListName: summary.ListName,
				UniqueID: uniqueID,
				Part:     target,
				Changed:  changed,
				Before:   before,
				After:    entryView(updated),
				URL:      listWebURLPrefix + summary.ID,
			}

			t := &output.Table{Headers: []string{"FIELD", "BEFORE", "AFTER"}}
			t.AddRow("quantity", before.Quantity, result.After.Quantity)
			t.AddRow("reference", before.ReferenceDesignator, result.After.ReferenceDesignator)
			t.AddRow("customer ref", before.CustomerReference, result.After.CustomerReference)
			t.AddRow("notes", before.Notes, result.After.Notes)
			if err := app.Printer.Print(result, t); err != nil {
				return err
			}
			app.Printer.PrintText("\nUpdated %s in %q.", target, summary.ListName)
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVar(&qty, "qty", 0, "new quantity")
	f.StringVar(&ref, "ref", "", "new reference designators, e.g. \"C1,C2\"")
	f.StringVar(&custRef, "customer-ref", "", "new customer reference")
	f.StringVar(&note, "note", "", "new note")
	return cmd
}

// findRequestedPart locates a list entry by unique id.
func findRequestedPart(parts []digikey.RequestedPart, uniqueID string) (digikey.RequestedPart, bool) {
	for _, p := range parts {
		if strings.EqualFold(p.UniqueID, uniqueID) {
			return p, true
		}
	}
	return digikey.RequestedPart{}, false
}

// setQuantity replaces the quantity on a line, preserving the pack type that
// was already selected. DigiKey allows several quantity lines per part; this
// collapses them to one, which is what a caller asking for a single quantity
// means.
func setQuantity(existing []digikey.RequestedQuantity, qty int) []digikey.RequestedQuantity {
	if len(existing) == 0 {
		return []digikey.RequestedQuantity{{Quantity: qty}}
	}
	first := existing[0]
	first.Quantity = qty
	return []digikey.RequestedQuantity{first}
}

func newListCopyCommand(app *App) *cobra.Command {
	var (
		tags       []string
		autoRename bool
	)

	cmd := &cobra.Command{
		Use:     "copy <source-list> <new-name>",
		Aliases: []string{"clone"},
		Short:   "Copy a list to a new one",
		Long: `Create a new list seeded from an existing one.

  dk list copy "Bench PSU rev A" "Bench PSU rev B"

Useful for iterating on a design without disturbing a BOM you have already
reviewed. The copy is independent: editing it does not affect the source.`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			newName := strings.TrimSpace(args[1])
			if newName == "" {
				return usageErrorf("new list name must not be empty")
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			source, err := client.ResolveList(ctx, args[0])
			if err != nil {
				return err
			}

			if autoRename {
				suggested, err := client.SuggestListName(ctx, newName)
				if err != nil {
					return err
				}
				if suggested != "" {
					newName = suggested
				}
			}

			id, err := client.CreateList(ctx, digikey.CreateListRequest{
				ListName: newName,
				Tags:     tags,
				Source:   "external",
				RefList:  &digikey.ReferenceList{ListID: source.ID},
			})
			if err != nil {
				return err
			}

			view := ListView{ID: id, Name: newName, Tags: tags, URL: listWebURLPrefix + id}
			t := &output.Table{Headers: []string{"NAME", "ID", "COPIED FROM", "URL"}}
			t.AddRow(view.Name, view.ID, source.ListName, view.URL)
			if err := app.Printer.Print(view, t); err != nil {
				return err
			}
			app.Printer.PrintText("\nCopied %q to %q.", source.ListName, newName)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringSliceVar(&tags, "tag", nil, "tag to attach to the new list (repeatable)")
	f.BoolVar(&autoRename, "auto-rename", false, "if the name is taken, let DigiKey pick the next free variant")
	return cmd
}

func newListLsCommand(app *App) *cobra.Command {
	var limit, offset int

	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list", "ls-lists"},
		Short:   "List your DigiKey lists",
		Long: `List every list on your DigiKey account.

Without --limit or --offset this pages through the whole set, so the output
matches what the other list commands can resolve by name. Pass either flag to
fetch a single page instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := app.Client()
			if err != nil {
				return err
			}

			var lists []digikey.ListSummary
			if limit > 0 || offset > 0 {
				lists, err = client.Lists(cmd.Context(), offset, limit)
			} else {
				lists, err = client.AllLists(cmd.Context())
			}
			if err != nil {
				return err
			}

			views := make([]ListView, 0, len(lists))
			for _, l := range lists {
				views = append(views, newListView(l))
			}

			t := &output.Table{
				Headers: []string{"NAME", "PARTS", "MODIFIED", "ID"},
				Empty:   "No lists yet. Create one with `dk list create <name>`.",
			}
			for _, v := range views {
				t.AddRow(v.Name, v.TotalParts, shortDate(v.DateModified), v.ID)
			}
			return app.Printer.Print(views, t)
		},
	}

	f := cmd.Flags()
	f.IntVarP(&limit, "limit", "n", 0, "maximum lists to return (0 for DigiKey's default)")
	f.IntVar(&offset, "offset", 0, "index of the first list, for paging")
	return cmd
}

func newListCreateCommand(app *App) *cobra.Command {
	var (
		tags       []string
		visibility string
		autoRename bool
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new list",
		Long: `Create a new list and print its id.

  dk list create "Bench PSU rev A" --tag project --tag psu

DigiKey rejects a duplicate name; --auto-rename asks DigiKey for the next free
variant instead of failing, which is useful in unattended runs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return usageErrorf("list name must not be empty")
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}

			if autoRename {
				suggested, err := client.SuggestListName(ctx, name)
				if err != nil {
					return err
				}
				if suggested != "" {
					name = suggested
				}
			}

			req := digikey.CreateListRequest{ListName: name, Tags: tags, Source: "external"}
			if visibility != "" {
				v, err := normalizeVisibility(visibility)
				if err != nil {
					return err
				}
				req.ListSettings = &digikey.ListSettings{Visibility: v}
			}

			id, err := client.CreateList(ctx, req)
			if err != nil {
				return err
			}

			view := ListView{ID: id, Name: name, Tags: tags, URL: listWebURLPrefix + id}
			t := &output.Table{Headers: []string{"NAME", "ID", "URL"}}
			t.AddRow(view.Name, view.ID, view.URL)
			return app.Printer.Print(view, t)
		},
	}

	f := cmd.Flags()
	f.StringSliceVar(&tags, "tag", nil, "tag to attach to the list (repeatable)")
	f.StringVar(&visibility, "visibility", "", "list visibility: private, readonly, or readwrite")
	f.BoolVar(&autoRename, "auto-rename", false, "if the name is taken, let DigiKey pick the next free variant")
	return cmd
}

func normalizeVisibility(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "private":
		return digikey.VisibilityPrivate, nil
	case "readonly", "read-only":
		return digikey.VisibilityReadOnly, nil
	case "readwrite", "read-write":
		return digikey.VisibilityReadWrite, nil
	default:
		return "", usageErrorf("invalid --visibility %q (want private, readonly, or readwrite)", v)
	}
}

func newListShowCommand(app *App) *cobra.Command {
	var (
		limit, offset int
		raw           bool
	)

	cmd := &cobra.Command{
		Use:   "show <list>",
		Short: "Show a list's parts with live pricing and stock",
		Long: `Show a list's contents, resolved against DigiKey's live catalog.

  dk list show "Bench PSU rev A"

Without --limit or --offset this pages through every part, so the totals cover
the whole list. Pass either flag to fetch a single page instead.

The MATCHED column is the one to watch: a false there means DigiKey could not
map your part number to a catalog product, so that line cannot be ordered.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return usageErrorf("--limit must not be negative")
			}
			if offset < 0 {
				return usageErrorf("--offset must not be negative")
			}
			if err := app.checkRawFormat(raw); err != nil {
				return err
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			summary, err := client.ResolveList(ctx, args[0])
			if err != nil {
				return err
			}

			// --raw is a single page by design: it exists to show what DigiKey
			// actually sends, and stitching several responses together would be
			// a shape DigiKey never returned.
			if raw {
				payload, err := client.RawListParts(ctx, summary.ID, offset, limit, app.locale())
				if err != nil {
					return err
				}
				return app.printRaw(payload)
			}

			var parts *digikey.PartsResponse
			if limit > 0 || offset > 0 {
				parts, err = client.ListParts(ctx, summary.ID, offset, limit, app.locale())
			} else {
				parts, err = client.AllListParts(ctx, summary.ID, app.locale())
			}
			if err != nil {
				return err
			}

			detail := buildListDetail(*summary, parts, app.Cfg.Locale.Currency)
			if err := app.Printer.Print(detail, listPartsTable(detail.Parts)); err != nil {
				return err
			}

			if detail.Returned < detail.TotalParts {
				app.Printer.PrintText("\nShowing %d of %d parts; the total below covers only this page.",
					detail.Returned, detail.TotalParts)
			}
			app.Printer.PrintText("\n%d parts, estimated total %s %s (excludes shipping and tax)",
				len(detail.Parts), output.Money(detail.EstimatedTotal), detail.Currency)
			if detail.UnmatchedParts > 0 {
				app.Printer.PrintText("%d part(s) did not match a DigiKey product and cannot be ordered.", detail.UnmatchedParts)
			}
			if detail.UnpricedParts > 0 {
				app.Printer.PrintText("%d matched part(s) carry no price, so the total above is an underestimate.", detail.UnpricedParts)
			}
			app.Printer.PrintText("Review and order at %s", detail.URL)
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVarP(&limit, "limit", "n", 0, "maximum parts to return (0 for DigiKey's default)")
	f.IntVar(&offset, "offset", 0, "index of the first part, for paging")
	f.BoolVar(&raw, "raw", false, "emit DigiKey's unmodified response for one page (implies --output json)")
	return cmd
}

func buildListDetail(summary digikey.ListSummary, parts *digikey.PartsResponse, currency string) ListDetail {
	detail := ListDetail{ListView: newListView(summary), Currency: currency}
	if parts == nil {
		detail.Parts = []ListPartView{}
		return detail
	}
	// TotalParts from the parts endpoint is authoritative; the summary's count
	// can lag right after a mutation. Taken unconditionally, including zero —
	// guarding on `> 0` let a stale summary claim a list still had parts after
	// the last one was removed, producing "Showing 0 of 5 parts".
	detail.TotalParts = parts.TotalParts
	detail.Parts = make([]ListPartView, 0, len(parts.PartsList))
	for _, p := range parts.PartsList {
		v := newListPartView(p)
		detail.Parts = append(detail.Parts, v)
		detail.EstimatedTotal += v.ExtendedPrice
		switch {
		case !v.Matched:
			detail.UnmatchedParts++
		case v.ExtendedPrice <= 0:
			detail.UnpricedParts++
		}
	}
	detail.Returned = len(detail.Parts)
	if detail.TotalParts < detail.Returned {
		detail.TotalParts = detail.Returned
	}
	return detail
}

func listPartsTable(parts []ListPartView) *output.Table {
	t := &output.Table{
		Headers: []string{"QTY", "DKPN", "MPN", "MFR", "DESCRIPTION", "UNIT", "EXT", "STOCK", "REF", "MATCHED"},
		Empty:   "List is empty. Add parts with `dk list add <list> <part>`.",
	}
	for _, p := range parts {
		dkpn := p.DigiKeyPartNumber
		if dkpn == "" {
			dkpn = p.RequestedPartNumber
		}
		t.AddRow(
			p.Quantity,
			dkpn,
			p.ManufacturerPartNumber,
			output.Truncate(p.Manufacturer, 18),
			output.Truncate(p.Description, 36),
			output.Money(p.UnitPrice),
			output.Money(p.ExtendedPrice),
			p.QuantityAvailable,
			output.Truncate(p.ReferenceDesignator, 16),
			p.Matched,
		)
	}
	return t
}

// PartSpec is one part to add, as supplied on the command line or via --from-json.
type PartSpec struct {
	Part              string `json:"part"`
	Quantity          int    `json:"quantity"`
	Reference         string `json:"reference,omitempty"`
	CustomerReference string `json:"customer_reference,omitempty"`
	Note              string `json:"note,omitempty"`
	Manufacturer      string `json:"manufacturer,omitempty"`
	Packaging         string `json:"packaging,omitempty"`
}

// AddResult is the JSON shape of `dk list add`.
type AddResult struct {
	ListID   string       `json:"list_id"`
	ListName string       `json:"list_name"`
	URL      string       `json:"url"`
	Added    []AddedPart  `json:"added"`
	Skipped  []SkippedAdd `json:"skipped,omitempty"`
}

// AddedPart pairs a requested part with the unique id DigiKey assigned it. That
// id is the handle for `dk list rm` and future updates.
type AddedPart struct {
	Part     string `json:"part"`
	Quantity int    `json:"quantity"`
	UniqueID string `json:"unique_id,omitempty"`
}

// SkippedAdd records a part that was not added and why.
type SkippedAdd struct {
	Part   string `json:"part"`
	Reason string `json:"reason"`
}

func newListAddCommand(app *App) *cobra.Command {
	var (
		qty       int
		ref       string
		note      string
		custRef   string
		packaging string
		fromJSON  string
		verify    bool
	)

	cmd := &cobra.Command{
		Use:   "add <list> <part[:qty]>...",
		Short: "Add parts to a list",
		Long: `Add one or more parts to a list.

Each part is a DigiKey or manufacturer part number, optionally suffixed with
":QTY". Without a suffix, --qty applies (default 1).

  dk list add "Bench PSU rev A" 1276-1000-1-ND:10
  dk list add "Bench PSU rev A" 1276-1000-1-ND:10 311-10.0KHRCT-ND:20 296-1234-5-ND
  dk list add "Bench PSU rev A" 1276-1000-1-ND --qty 10 --ref C1-C10 --note "input decoupling"

--ref, --note, and --customer-ref apply to a single part; use --from-json for
per-part metadata in bulk:

  dk list add "Bench PSU rev A" --from-json bom.json
  cat bom.json | dk list add "Bench PSU rev A" --from-json -

The JSON is an array of objects:
  [{"part":"1276-1000-1-ND","quantity":10,"reference":"C1-C10","note":"decoupling"}]

DigiKey accepts unknown part numbers without complaint and simply marks the line
unmatched. Pass --verify to check each part against the catalog first and skip
the ones that do not resolve.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			listRef := args[0]
			positional := args[1:]

			if qty < 1 {
				return usageErrorf("--qty must be at least 1")
			}

			specs, err := collectPartSpecs(app, positional, fromJSON, qty, ref, custRef, note, packaging)
			if err != nil {
				return err
			}
			if len(specs) == 0 {
				return usageErrorf("no parts given; pass part numbers as arguments or use --from-json")
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			summary, err := client.ResolveList(ctx, listRef)
			if err != nil {
				return err
			}

			var skipped []SkippedAdd
			if verify {
				specs, skipped, err = verifySpecs(ctx, client, specs)
				if err != nil {
					return err
				}
			}

			result := AddResult{
				ListID:   summary.ID,
				ListName: summary.ListName,
				URL:      listWebURLPrefix + summary.ID,
				Skipped:  skipped,
			}

			if len(specs) == 0 {
				t := &output.Table{Headers: []string{"PART", "REASON"}, Empty: "Nothing to add."}
				for _, s := range skipped {
					t.AddRow(s.Part, s.Reason)
				}
				if err := app.Printer.Print(result, t); err != nil {
					return err
				}
				return &Error{
					Code:     CodeNotFound,
					Message:  "no parts could be verified against the DigiKey catalog",
					Hint:     "Check the part numbers with `dk search`, or drop --verify to add them anyway.",
					ExitCode: ExitNotFound,
				}
			}

			parts := make([]digikey.RequestedPart, 0, len(specs))
			for _, s := range specs {
				parts = append(parts, digikey.RequestedPart{
					RequestedPartNumber: s.Part,
					ManufacturerName:    s.Manufacturer,
					CustomerReference:   s.CustomerReference,
					ReferenceDesignator: s.Reference,
					Notes:               s.Note,
					Quantities: []digikey.RequestedQuantity{{
						Quantity:         s.Quantity,
						SelectedPackType: s.Packaging,
					}},
				})
			}

			ids, err := client.AddParts(ctx, summary.ID, parts)
			if err != nil {
				return err
			}

			for i, s := range specs {
				added := AddedPart{Part: s.Part, Quantity: s.Quantity}
				if i < len(ids) {
					added.UniqueID = ids[i]
				}
				result.Added = append(result.Added, added)
			}

			t := &output.Table{Headers: []string{"QTY", "PART", "UNIQUE ID"}, Empty: "Nothing added."}
			for _, a := range result.Added {
				t.AddRow(a.Quantity, a.Part, a.UniqueID)
			}
			if err := app.Printer.Print(result, t); err != nil {
				return err
			}

			app.Printer.PrintText("\nAdded %d part(s) to %q.", len(result.Added), summary.ListName)
			for _, s := range skipped {
				app.Printer.PrintText("Skipped %s: %s", s.Part, s.Reason)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVar(&qty, "qty", 1, "quantity for parts without an explicit :QTY suffix")
	f.StringVar(&ref, "ref", "", "reference designators for the part, e.g. \"C1,C2,C3\" (single part only)")
	f.StringVar(&custRef, "customer-ref", "", "your own part reference (single part only)")
	f.StringVar(&note, "note", "", "free-text note attached to the line (single part only)")
	f.StringVar(&packaging, "packaging", "", "preferred pack type, e.g. \"Cut Tape\" or \"Tape & Reel\"")
	f.StringVar(&fromJSON, "from-json", "", "read parts from a JSON file, or \"-\" for stdin")
	f.BoolVar(&verify, "verify", false, "check each part exists in the catalog before adding, skipping any that do not")
	return cmd
}

// collectPartSpecs merges positional PART[:QTY] arguments with --from-json
// input and applies the single-part metadata flags.
func collectPartSpecs(app *App, positional []string, fromJSON string, defaultQty int, ref, custRef, note, packaging string) ([]PartSpec, error) {
	var specs []PartSpec

	for _, arg := range positional {
		spec, err := parsePartSpec(arg, defaultQty)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}

	if fromJSON != "" {
		fromFile, err := readPartSpecs(app, fromJSON, defaultQty)
		if err != nil {
			return nil, err
		}
		specs = append(specs, fromFile...)
	}

	hasLineMetadata := ref != "" || custRef != "" || note != ""
	if hasLineMetadata && len(specs) > 1 {
		return nil, usageErrorf("--ref, --customer-ref, and --note apply to a single part; use --from-json for per-part metadata across %d parts", len(specs))
	}

	for i := range specs {
		if ref != "" {
			specs[i].Reference = ref
		}
		if custRef != "" {
			specs[i].CustomerReference = custRef
		}
		if note != "" {
			specs[i].Note = note
		}
		if packaging != "" && specs[i].Packaging == "" {
			specs[i].Packaging = packaging
		}
	}
	return specs, nil
}

// parsePartSpec parses "PART" or "PART:QTY".
//
// DigiKey part numbers contain hyphens and periods but not colons, so a colon
// unambiguously separates the quantity. The split is on the last colon so a
// part number is never mangled.
func parsePartSpec(arg string, defaultQty int) (PartSpec, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return PartSpec{}, usageErrorf("empty part argument")
	}

	i := strings.LastIndex(arg, ":")
	if i < 0 {
		return PartSpec{Part: arg, Quantity: defaultQty}, nil
	}

	part := strings.TrimSpace(arg[:i])
	qtyStr := strings.TrimSpace(arg[i+1:])
	if part == "" {
		return PartSpec{}, usageErrorf("missing part number in %q", arg)
	}
	qty, err := strconv.Atoi(qtyStr)
	if err != nil {
		return PartSpec{}, usageErrorf("invalid quantity %q in %q: expected PART or PART:QTY", qtyStr, arg)
	}
	if qty < 1 {
		return PartSpec{}, usageErrorf("quantity in %q must be at least 1", arg)
	}
	return PartSpec{Part: part, Quantity: qty}, nil
}

// readPartSpecs decodes --from-json input.
func readPartSpecs(app *App, path string, defaultQty int) ([]PartSpec, error) {
	var r io.Reader
	if path == "-" {
		r = app.In
		if r == nil {
			r = os.Stdin
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, usageErrorf("open --from-json %s: %v", path, err)
		}
		defer f.Close()
		r = f
	}

	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read --from-json input: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, usageErrorf("--from-json input was empty")
	}

	var specs []PartSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, usageErrorf("parse --from-json input: %v (expected an array like [{\"part\":\"1276-1000-1-ND\",\"quantity\":10}])", err)
	}

	for i := range specs {
		specs[i].Part = strings.TrimSpace(specs[i].Part)
		if specs[i].Part == "" {
			return nil, usageErrorf("--from-json entry %d has no \"part\"", i)
		}
		if specs[i].Quantity == 0 {
			specs[i].Quantity = defaultQty
		}
		if specs[i].Quantity < 1 {
			return nil, usageErrorf("--from-json entry %d (%s) has quantity %d; must be at least 1", i, specs[i].Part, specs[i].Quantity)
		}
	}
	return specs, nil
}

// verifySpecs checks each part against the catalog, returning the ones that
// resolve and a record of those that do not.
func verifySpecs(ctx context.Context, client *digikey.Client, specs []PartSpec) ([]PartSpec, []SkippedAdd, error) {
	var kept []PartSpec
	var skipped []SkippedAdd

	for _, s := range specs {
		details, err := client.ProductDetails(ctx, s.Part)
		if err != nil {
			var apiErr *digikey.APIError
			if errors.As(err, &apiErr) && apiErr.NotFound() {
				skipped = append(skipped, SkippedAdd{Part: s.Part, Reason: "not found in the DigiKey catalog"})
				continue
			}
			return nil, nil, err
		}
		if details.Product.ManufacturerProductNumber == "" && details.Product.DigiKeyPartNumber() == "" {
			skipped = append(skipped, SkippedAdd{Part: s.Part, Reason: "no matching product returned"})
			continue
		}
		kept = append(kept, s)
	}
	return kept, skipped, nil
}

func newListRemoveCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm <list> <unique-id-or-part>...",
		Aliases: []string{"remove", "delete-part"},
		Short:   "Remove parts from a list",
		Long: `Remove one or more parts from a list.

Each argument is either the unique id shown by "dk list show --output json", or
a part number as it appears in the list:

  dk list rm "Bench PSU rev A" 1276-1000-1-ND
  dk list rm "Bench PSU rev A" 3f2c1a9e-...`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			summary, err := client.ResolveList(ctx, args[0])
			if err != nil {
				return err
			}

			// Fetch the current contents so part numbers can be mapped to the
			// unique ids the delete endpoint requires. All of them: removing a
			// part on page two must not report it as absent.
			parts, err := client.AllListParts(ctx, summary.ID, app.locale())
			if err != nil {
				return err
			}

			type removal struct {
				Target   string `json:"target"`
				UniqueID string `json:"unique_id"`
				Removed  bool   `json:"removed"`
				Reason   string `json:"reason,omitempty"`
			}
			results := make([]removal, 0, len(args)-1)
			// A rejected delete is a real failure, not a per-row footnote. Keep
			// the first one so the command can exit with the code that matches it
			// — a rate limit has to surface as 5, not as a successful run.
			var firstErr error

			for _, target := range args[1:] {
				uniqueIDs := matchListEntries(parts.PartsList, target)
				if len(uniqueIDs) == 0 {
					results = append(results, removal{Target: target, Reason: "not found in this list"})
					continue
				}
				for _, uid := range uniqueIDs {
					if err := client.DeletePart(ctx, summary.ID, uid); err != nil {
						if firstErr == nil {
							firstErr = err
						}
						results = append(results, removal{Target: target, UniqueID: uid, Reason: err.Error()})
						continue
					}
					results = append(results, removal{Target: target, UniqueID: uid, Removed: true})
				}
			}

			t := &output.Table{Headers: []string{"TARGET", "UNIQUE ID", "REMOVED", "REASON"}}
			removed := 0
			for _, r := range results {
				if r.Removed {
					removed++
				}
				t.AddRow(r.Target, r.UniqueID, r.Removed, r.Reason)
			}

			payload := map[string]any{
				"list_id":   summary.ID,
				"list_name": summary.ListName,
				"results":   results,
				"removed":   removed,
			}
			if err := app.Printer.Print(payload, t); err != nil {
				return err
			}
			// The table above already names which targets failed and why; this
			// only decides the exit code. A DigiKey rejection wins over
			// "not found", because it is the more actionable failure.
			if firstErr != nil {
				return firstErr
			}
			if removed == 0 {
				return &Error{
					Code:     CodeNotFound,
					Message:  "no matching parts were removed",
					Hint:     "Run `dk list show <list>` to see the parts currently in the list.",
					ExitCode: ExitNotFound,
				}
			}
			return nil
		},
	}
	return cmd
}

// matchListEntries finds the unique ids matching a user-supplied target, which
// may be a unique id or any of the part numbers on the line.
func matchListEntries(parts []digikey.ListPart, target string) []string {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	var ids []string
	for _, p := range parts {
		if strings.EqualFold(p.UniqueID, target) ||
			strings.EqualFold(p.DigiKeyPartNumber, target) ||
			strings.EqualFold(p.RequestedPartNumber, target) ||
			strings.EqualFold(p.ManufacturerPartNumber, target) ||
			matchesPackOption(p, target) {
			ids = append(ids, p.UniqueID)
		}
	}
	return ids
}

// matchesPackOption reports whether target names one of the line's packaging
// options.
//
// Every pack option has its own DigiKey part number, and `dk list show` prints
// the one that was actually priced — which is frequently neither the
// part-level number nor the number originally requested. Without this, a user
// could copy a part number out of `list show` and have `list rm` tell them it
// is not in the list.
func matchesPackOption(p digikey.ListPart, target string) bool {
	for _, q := range p.Quantities {
		for _, opt := range q.PackOptions {
			if opt.DigiKeyPartNumber != "" && strings.EqualFold(opt.DigiKeyPartNumber, target) {
				return true
			}
		}
	}
	return false
}

func newListRenameCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:               "rename <list> <new-name>",
		Short:             "Rename a list",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			newName := strings.TrimSpace(args[1])
			if newName == "" {
				return usageErrorf("new list name must not be empty")
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			summary, err := client.ResolveList(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.RenameList(ctx, summary.ID, newName); err != nil {
				return err
			}

			view := newListView(*summary)
			view.Name = newName
			t := &output.Table{Headers: []string{"NAME", "ID", "URL"}}
			t.AddRow(view.Name, view.ID, view.URL)
			return app.Printer.Print(view, t)
		},
	}
}

func newListDeleteCommand(app *App) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "delete <list>",
		Short: "Delete a list permanently",
		Long: `Delete a list and everything in it. This cannot be undone.

Deleting a list that still has parts requires --force, so an agent cannot
discard a list it did not mean to touch.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			summary, err := client.ResolveList(ctx, args[0])
			if err != nil {
				return err
			}

			if !force {
				// summary.TotalParts comes from the lists endpoint, which the
				// rest of this file treats as lagging behind a mutation. Guarding
				// a permanent delete on a stale zero would discard a list that
				// had just been filled, so ask the parts endpoint instead. One
				// row is enough: TotalParts rides along with it.
				count, err := client.ListParts(ctx, summary.ID, 0, 1, app.locale())
				if err != nil {
					return err
				}
				remaining := max(count.TotalParts, len(count.PartsList))
				if remaining > 0 {
					return &Error{
						Code:     CodeUsage,
						Message:  fmt.Sprintf("list %q still has %d part(s)", summary.ListName, remaining),
						Hint:     "Re-run with --force to delete it anyway.",
						ExitCode: ExitUsage,
					}
				}
			}

			if err := client.DeleteList(ctx, summary.ID); err != nil {
				return err
			}

			payload := map[string]any{"list_id": summary.ID, "list_name": summary.ListName, "deleted": true}
			t := &output.Table{Headers: []string{"NAME", "ID", "DELETED"}}
			t.AddRow(summary.ListName, summary.ID, true)
			return app.Printer.Print(payload, t)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "delete even if the list still contains parts")
	return cmd
}

func newListExportCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <list>",
		Short: "Export a list as a BOM",
		Long: `Export a list in a form suited to a spreadsheet or a purchase review.

  dk list export "Bench PSU rev A" --output csv > bom.csv
  dk list export "Bench PSU rev A" --output json

Unlike "dk list show", the columns here are BOM-shaped: quantity, part numbers,
reference designators, and line pricing, with nothing truncated.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeListNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}
			summary, err := client.ResolveList(ctx, args[0])
			if err != nil {
				return err
			}
			// A BOM missing its last page is worse than no BOM, so this always
			// pages the whole list.
			parts, err := client.AllListParts(ctx, summary.ID, app.locale())
			if err != nil {
				return err
			}

			detail := buildListDetail(*summary, parts, app.Cfg.Locale.Currency)

			t := &output.Table{
				Headers: []string{
					"Quantity", "DigiKeyPartNumber", "ManufacturerPartNumber", "Manufacturer",
					"Description", "ReferenceDesignator", "CustomerReference", "Notes",
					"UnitPrice", "ExtendedPrice", "QuantityAvailable", "Status", "DatasheetURL",
				},
				Empty: "List is empty.",
			}
			for _, p := range detail.Parts {
				dkpn := p.DigiKeyPartNumber
				if dkpn == "" {
					dkpn = p.RequestedPartNumber
				}
				t.AddRow(
					p.Quantity, dkpn, p.ManufacturerPartNumber, p.Manufacturer,
					p.Description, p.ReferenceDesignator, p.CustomerReference, p.Notes,
					fmt.Sprintf("%.4f", p.UnitPrice), fmt.Sprintf("%.4f", p.ExtendedPrice),
					p.QuantityAvailable, p.Status, p.DatasheetURL,
				)
			}
			return app.Printer.Print(detail, t)
		},
	}
	return cmd
}

// completeListNames provides shell completion for list name arguments.
func (a *App) completeListNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := a.Client()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	lists, err := client.AllLists(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(lists))
	for _, l := range lists {
		names = append(names, l.ListName)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// shortDate trims an ISO 8601 timestamp to its date portion for table display.
func shortDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func parsePositiveInt(s, what string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, usageErrorf("invalid %s %q: expected a number", what, s)
	}
	if n <= 0 {
		return 0, usageErrorf("%s must be positive, got %d", what, n)
	}
	return n, nil
}
