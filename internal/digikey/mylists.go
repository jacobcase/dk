package digikey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// myListsBasePath is the MyLists v1 base path. Every endpoint below requires a
// 3-legged token, because lists belong to a DigiKey user account.
const myListsBasePath = "/mylists/v1"

// List visibility values accepted by ListSettings.
const (
	VisibilityPrivate   = "Private"
	VisibilityReadOnly  = "ReadOnly"
	VisibilityReadWrite = "ReadWrite"
)

// ErrListNotFound is returned when a list name or id does not resolve.
var ErrListNotFound = errors.New("list not found")

// ErrListRefRequired is returned when the list name-or-id argument is blank.
// It is distinct from ErrListNotFound because an empty argument is a bad
// invocation, not a missing list, and the two carry different exit codes.
var ErrListRefRequired = errors.New("list name or id is required")

// ErrAmbiguousList is returned when a list name matches more than one list.
type ErrAmbiguousList struct {
	Name      string
	Candidate []ListSummary
}

func (e *ErrAmbiguousList) Error() string {
	ids := make([]string, 0, len(e.Candidate))
	for _, c := range e.Candidate {
		ids = append(ids, c.ID)
	}
	return fmt.Sprintf("list name %q matches %d lists (%s); pass the list id instead",
		e.Name, len(e.Candidate), strings.Join(ids, ", "))
}

// ListSettings controls list-level preferences.
type ListSettings struct {
	Visibility            string `json:"Visibility,omitempty"`
	PackagePreference     string `json:"PackagePreference,omitempty"`
	AutoCorrectQuantities *bool  `json:"AutoCorrectQuantities,omitempty"`
	AttritionEnabled      *bool  `json:"AttritionEnabled,omitempty"`
	AutoPopulateCref      *bool  `json:"AutoPopulateCref,omitempty"`
}

// ReferenceList points a new list at an existing one, which is how DigiKey
// models cloning: the created list starts as a copy of RefList's contents.
type ReferenceList struct {
	ListID      string `json:"ListId,omitempty"`
	IsGuestList bool   `json:"IsGuestList,omitempty"`
	IsRevision  bool   `json:"IsRevision,omitempty"`
}

// CreateListRequest is the POST body for creating a list.
type CreateListRequest struct {
	ListName     string        `json:"ListName"`
	CreatedBy    string        `json:"CreatedBy,omitempty"`
	Tags         []string      `json:"Tags,omitempty"`
	Source       string        `json:"Source,omitempty"`
	ListSettings *ListSettings `json:"ListSettings,omitempty"`
	// RefList, when set, makes the new list a copy of an existing one.
	RefList *ReferenceList `json:"RefList,omitempty"`
}

// RequestedQuantity is a quantity line on a requested part.
type RequestedQuantity struct {
	SelectedPackType    string  `json:"SelectedPackType,omitempty"`
	SelectedSubPackType string  `json:"SelectedSubPackType,omitempty"`
	Quantity            int     `json:"Quantity"`
	TargetPrice         float64 `json:"TargetPrice,omitempty"`
}

// RequestedPart is the payload for adding or updating a part in a list.
type RequestedPart struct {
	UniqueID            string              `json:"UniqueId,omitempty"`
	PartID              int                 `json:"PartId,omitempty"`
	RequestedPartNumber string              `json:"RequestedPartNumber,omitempty"`
	OriginalPartNumber  string              `json:"OriginalPartNumber,omitempty"`
	ManufacturerName    string              `json:"ManufacturerName,omitempty"`
	CustomerReference   string              `json:"CustomerReference,omitempty"`
	ReferenceDesignator string              `json:"ReferenceDesignator,omitempty"`
	Notes               string              `json:"Notes,omitempty"`
	Attrition           float64             `json:"Attrition,omitempty"`
	AlternateParts      []string            `json:"AlternateParts,omitempty"`
	Quantities          []RequestedQuantity `json:"Quantities,omitempty"`
	// SelectedQuantityIndex names which Quantities entry the line is priced
	// from. It has to be here because DigiKey's part update is a replace, not a
	// patch: `dk list set` reads a line and sends it back, so a field this
	// struct cannot hold is a field the write silently resets to zero.
	//
	// No omitempty, unlike its neighbours. `dk list set --qty` resets this to 0
	// deliberately, and omitempty would drop that reset from the body — leaving
	// a replace-semantics API to infer a zero dk meant to state. The one field
	// here whose zero is a decision is the one field that has to be sent.
	SelectedQuantityIndex int `json:"SelectedQuantityIndex"`
}

// ListSummary is the metadata DigiKey returns for a list.
type ListSummary struct {
	ID               string        `json:"Id"`
	ListName         string        `json:"ListName"`
	CreatedBy        string        `json:"CreatedBy"`
	CustomerID       int           `json:"CustomerId"`
	AccountID        int           `json:"AccountId"`
	CompanyName      string        `json:"CompanyName"`
	Notes            string        `json:"Notes"`
	TotalParts       int           `json:"TotalParts"`
	DateCreated      string        `json:"DateCreated"`
	DateLastAccessed string        `json:"DateLastAccessed"`
	DateModified     string        `json:"DateModified"`
	Tags             []string      `json:"Tags"`
	ListSettings     *ListSettings `json:"ListSettings,omitempty"`
	// PartsList is decoded because the spec documents it, and is empty in every
	// live response — TotalParts is correct while this stays []. Do not read it
	// to find a line; use GetPartsByListId (AllListParts). The only operation
	// that would ever populate it, GET /lists/{listId}, is not wrapped for
	// exactly that reason — see the note above CreateList.
	PartsList []RequestedPart `json:"PartsList,omitempty"`
	CanEdit   bool            `json:"CanEdit"`
}

// ListPackOption is one packaging choice DigiKey priced for a quantity line.
// A line carries every option (cut tape, tape & reel, digi-reel) and names the
// one that applies in ListPartQuantity.SelectedPackType.
type ListPackOption struct {
	DigiKeyPartNumber   string  `json:"DigiKeyPartNumber"`
	PackType            string  `json:"PackType"`
	Quantity            int     `json:"Quantity"`
	QuantityAvailable   int     `json:"QuantityAvailable"`
	CalculatedUnitPrice float64 `json:"CalculatedUnitPrice"`
	ExtendedPrice       float64 `json:"ExtendedPrice"`
}

// ListPartQuantity is the resolved quantity/pricing for a part in a list.
type ListPartQuantity struct {
	QuantityRequested  int     `json:"QuantityRequested"`
	CalculatedQuantity int     `json:"CalculatedQuantity"`
	TargetPrice        float64 `json:"TargetPrice"`
	// SelectedPackOptionIndex points into PackOptions and is what DigiKey
	// actually populates — observed responses carry the index with
	// SelectedPackType left empty. A pointer so an absent field is
	// distinguishable from a genuine index 0; DigiKey uses -1 for "none",
	// as it does for SelectedSubPackOptionIndex.
	SelectedPackOptionIndex *int             `json:"SelectedPackOptionIndex"`
	SelectedPackType        string           `json:"SelectedPackType"`
	SelectedSubPackType     string           `json:"SelectedSubPackType"`
	PackOptions             []ListPackOption `json:"PackOptions"`
}

// packTypeForWrite names the pack type to send back when this line is written
// out as a RequestedQuantity, which carries a pack type by name and has no
// index to carry instead.
//
// It uses only what DigiKey actually selected: the index when it names a real
// option, then the name if one is set. SelectedPackOption's "first priced
// option" fallback is deliberately not used here. Guessing is defensible when
// the answer is a price to display and wrong by a column; it is not when the
// answer is a pack type about to be written back over the line, where a guess
// silently repacks the part.
func (q ListPartQuantity) packTypeForWrite() string {
	if i := q.SelectedPackOptionIndex; i != nil && *i >= 0 && *i < len(q.PackOptions) {
		if t := strings.TrimSpace(q.PackOptions[*i].PackType); t != "" {
			return t
		}
	}
	return q.SelectedPackType
}

// SelectedPackOption returns the pack option that applies to this quantity
// line, and false when the line carries no options at all.
//
// Identifying the right option matters because its price is what reaches the
// BOM total a human buys from: quoting the reel price for a line set to cut
// tape overstates that line several-fold.
//
// The index is tried first because that is the field DigiKey actually fills
// in. A captured response carries SelectedPackOptionIndex: 0 alongside
// SelectedPackType: "", so name matching alone would never fire on real data
// and every line would fall through to the "first priced" guess. The name is
// kept as a second key for responses that supply it instead.
//
// When the selected option exists but DigiKey did not price it, it is returned
// as-is with its zero price rather than falling through. Substituting another
// pack type's price would be the very defect described above, and a zero is
// visible downstream — callers report it as an unpriced line, not a free one.
func (q ListPartQuantity) SelectedPackOption() (ListPackOption, bool) {
	if len(q.PackOptions) == 0 {
		return ListPackOption{}, false
	}

	// DigiKey uses -1 for "nothing selected", as it does for the sub-pack index.
	if i := q.SelectedPackOptionIndex; i != nil && *i >= 0 && *i < len(q.PackOptions) {
		return q.PackOptions[*i], true
	}

	if want := strings.TrimSpace(q.SelectedPackType); want != "" {
		for _, opt := range q.PackOptions {
			if strings.EqualFold(strings.TrimSpace(opt.PackType), want) {
				return opt, true
			}
		}
	}

	// No usable selection. Prefer a priced option over an unpriced one, so a
	// line still reports a cost when DigiKey declined to name a choice.
	for _, opt := range q.PackOptions {
		if opt.CalculatedUnitPrice > 0 || opt.ExtendedPrice > 0 {
			return opt, true
		}
	}
	return q.PackOptions[0], true
}

// ListPart is a fully resolved part in a list, including live availability.
type ListPart struct {
	PartID              int    `json:"PartId"`
	UniqueID            string `json:"UniqueId"`
	CustomerReference   string `json:"CustomerReference"`
	ReferenceDesignator string `json:"ReferenceDesignator"`
	Notes               string `json:"Notes"`
	MinOrderQty         int    `json:"MinOrderQty"`
	RequestedPartNumber string `json:"RequestedPartNumber"`
	DigiKeyPartNumber   string `json:"DigiKeyPartNumber"`
	// The next four are carried for one reason: this struct is the only live
	// source of a list line, so an edit reads it and writes it back. DigiKey's
	// part update is a replace, and a field this struct cannot hold is a field
	// every `dk list set` silently clears. See RequestedPart below.
	OriginalPartNumber        string             `json:"OriginalPartNumber"`
	AlternateParts            []string           `json:"AlternateParts"`
	Attrition                 float64            `json:"Attrition"`
	SelectedQuantityIndex     int                `json:"SelectedQuantityIndex"`
	ManufacturerPartNumber    string             `json:"ManufacturerPartNumber"`
	RequestedManufacturerName string             `json:"RequestedManufacturerName"`
	Manufacturer              string             `json:"Manufacturer"`
	Description               string             `json:"Description"`
	PartStatus                string             `json:"PartStatus"`
	Availability              string             `json:"Availability"`
	QuantityAvailable         int                `json:"QuantityAvailable"`
	Quantities                []ListPartQuantity `json:"Quantities"`
	VendorLeadWeeks           int                `json:"VendorLeadWeeks"`
	PartDetailURL             string             `json:"PartDetailUrl"`
	PrimaryDatasheetURL       string             `json:"PrimaryDatasheetUrl"`
	SupplierName              string             `json:"SupplierName"`
	ReachStatus               string             `json:"ReachStatus"`
	RohsStatusMessage         string             `json:"RohsStatusMessage"`
	Category                  string             `json:"Category"`
	Flags                     struct {
		NonStock      bool `json:"NonStock"`
		IsNCNR        bool `json:"IsNCNR"`
		IsMatched     bool `json:"IsMatched"`
		IsMarketPlace bool `json:"IsMarketPlace"`
		BoNotAllowed  bool `json:"BoNotAllowed"`
	} `json:"Flags"`
}

// RequestedPart converts a resolved list line back into the shape DigiKey's
// part update takes.
//
// This exists because the two are not the same object and only one of them can
// be read live. `GET /lists/{listId}` documents a PartsList of RequestedParts
// and returns it empty for every list — see CONTRIBUTING.md — so the only way
// to see what is on a line is GetPartsByListId, which answers in ListParts.
// An update is a replace, so everything the write model carries has to survive
// the trip: a field dropped here is a field `dk list set --note` erases while
// changing the note.
func (p ListPart) RequestedPart() RequestedPart {
	quantities := make([]RequestedQuantity, 0, len(p.Quantities))
	for _, q := range p.Quantities {
		quantities = append(quantities, RequestedQuantity{
			// QuantityRequested is what the user asked for; CalculatedQuantity
			// is that plus attrition, which DigiKey derives. Sending the
			// derived figure back would compound the attrition on every edit.
			Quantity:            q.QuantityRequested,
			SelectedPackType:    q.packTypeForWrite(),
			SelectedSubPackType: q.SelectedSubPackType,
			TargetPrice:         q.TargetPrice,
		})
	}
	return RequestedPart{
		UniqueID:              p.UniqueID,
		PartID:                p.PartID,
		RequestedPartNumber:   p.RequestedPartNumber,
		OriginalPartNumber:    p.OriginalPartNumber,
		ManufacturerName:      p.RequestedManufacturerName,
		CustomerReference:     p.CustomerReference,
		ReferenceDesignator:   p.ReferenceDesignator,
		Notes:                 p.Notes,
		Attrition:             p.Attrition,
		AlternateParts:        p.AlternateParts,
		Quantities:            quantities,
		SelectedQuantityIndex: p.SelectedQuantityIndex,
	}
}

// RequestedQty returns the quantity the user asked for, summed across quantity
// lines (DigiKey allows more than one per part).
func (p ListPart) RequestedQty() int {
	total := 0
	for _, q := range p.Quantities {
		total += q.QuantityRequested
	}
	return total
}

// OrderablePartNumber returns the DigiKey part number of the pack option that
// was actually priced, falling back to the part-level number.
//
// These differ routinely. A captured response for a cut-tape request carries
// DigiKeyPartNumber 311-1088-2-ND — the reel — while the selected pack option
// is 311-1088-1-ND at a quite different price. Reporting the part-level number
// beside the selected option's price pairs an identifier with a figure that
// does not describe it, and that pair is what a human orders from.
func (p ListPart) OrderablePartNumber() string {
	for _, q := range p.Quantities {
		if opt, ok := q.SelectedPackOption(); ok && opt.DigiKeyPartNumber != "" {
			return opt.DigiKeyPartNumber
		}
	}
	return p.DigiKeyPartNumber
}

// pricedLines sums the selected pack option across every quantity line.
//
// quantity counts every line, priced or not. RequestedQty sums them all and
// UnitPrice has to agree with it: a line DigiKey declined to price still
// contributes units the caller asked for, and dividing the money by only the
// priced units reports a figure that does not multiply out. unpriced says
// whether any line was skipped, which is the only way a caller can tell that
// extended covers less than the whole part.
func (p ListPart) pricedLines() (extended float64, quantity int, firstUnit float64, lines int, unpriced bool) {
	for _, q := range p.Quantities {
		quantity += q.QuantityRequested
		opt, ok := q.SelectedPackOption()
		if !ok {
			unpriced = true
			continue
		}
		if lines == 0 {
			firstUnit = opt.CalculatedUnitPrice
		}
		lines++
		extended += opt.ExtendedPrice
	}
	return extended, quantity, firstUnit, lines, unpriced
}

// UnitPrice returns the unit price of the selected pack option, or 0 if DigiKey
// did not resolve one (e.g. an unmatched part number).
//
// It always agrees with the other two figures dk reports for the line:
// UnitPrice multiplied by RequestedQty equals ExtendedPrice. The single fully
// priced line — very nearly every line in practice — reports DigiKey's own
// CalculatedUnitPrice rather than a division that would only reproduce it with
// less precision. Everything else divides, which covers both several priced
// lines and the mixed case where one line priced and another came back with no
// pack options at all: dividing by only the priced units would there report a
// unit price several times the real one.
func (p ListPart) UnitPrice() float64 {
	extended, quantity, firstUnit, lines, unpriced := p.pricedLines()
	if extended <= 0 || quantity <= 0 {
		return firstUnit
	}
	if lines == 1 && !unpriced {
		return firstUnit
	}
	return extended / float64(quantity)
}

// ExtendedPrice returns the line total for this part, summed across every
// quantity line, or 0 if unresolved.
func (p ListPart) ExtendedPrice() float64 {
	extended, _, _, _, _ := p.pricedLines()
	return extended
}

// HasUnpricedLine reports whether any quantity line came back without a pack
// option to price it.
//
// A part can be partly priced: ExtendedPrice is then a real figure that covers
// only some of the units, so a caller summing it into a BOM total gets a number
// that is quietly too low. That is the failure this exists to make visible —
// `dk list show` counts such a part into unpriced_parts rather than treating a
// non-zero total as proof the whole line is covered.
func (p ListPart) HasUnpricedLine() bool {
	_, _, _, _, unpriced := p.pricedLines()
	return unpriced
}

// PartsResponse is the GetPartsByListId result.
type PartsResponse struct {
	PartsList  []ListPart `json:"PartsList"`
	TotalParts int        `json:"TotalParts"`
}

// Lists returns the caller's lists. startIndex and limit are optional; pass 0
// for DigiKey's defaults.
func (c *Client) Lists(ctx context.Context, startIndex, limit int) ([]ListSummary, error) {
	q := url.Values{}
	if startIndex > 0 {
		q.Set("startIndex", strconv.Itoa(startIndex))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []ListSummary
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists",
		query:       q,
		requireUser: true,
		out:         &out,
		cacheScope:  ScopeLists,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Paging bounds for AllLists. The spec gives /lists a limit default of 50 and
// documents no maximum, so asking for listPageSize is asking for more than
// DigiKey promises; the loop below therefore cannot treat a short page as the
// end, nor treat a full one as proof the server is paging. maxListPages guards
// against a server that ignores startIndex and would otherwise loop forever.
const (
	listPageSize = 100
	// listPageDefault is the limit default the spec documents for /lists, and
	// the only page size it names. A repeated page at least this big is a
	// server paging in place; a smaller one is just a small account.
	listPageDefault = 50
	maxListPages    = 500
)

// AllLists returns every list the account owns. Resolving a list by name needs
// the complete set: a list on page two is otherwise indistinguishable from one
// that does not exist, which turns into a false not_found for every command
// that resolves a name — including the ones that then write to it.
func (c *Client) AllLists(ctx context.Context) ([]ListSummary, error) {
	all := []ListSummary{}
	seen := make(map[string]bool)

	for range maxListPages {
		// Resume from what we actually hold rather than from page*pageSize, and
		// do not stop on a short page. The spec defaults /lists to 50 per page
		// while this asks for 100, so the very first response is expected to be
		// "short" — treating that as the end would return the first 50 lists and
		// report them as complete. Same reasoning as AllListParts below.
		batch, err := c.Lists(ctx, len(all), listPageSize)
		if err != nil {
			return nil, err
		}

		// Count only lists we have not already seen. A server that ignores
		// startIndex resends the same page forever; without this the loop would
		// pile up duplicates until the page cap.
		added := 0
		for _, l := range batch {
			if l.ID != "" {
				if seen[l.ID] {
					continue
				}
				seen[l.ID] = true
			}
			all = append(all, l)
			added++
		}

		// A page that added nothing new ends the walk — unless it is page one
		// again, which means the server ignored startIndex and the walk cannot
		// reach whatever lies past it. Returning there would silently truncate,
		// which is the failure this function exists to prevent, so it is an
		// error instead.
		//
		// The test cannot be "was the page full": the spec documents a page
		// size default, not a cap, so a server capped below listPageSize
		// resends its short page forever and never trips that. Nor can it be
		// "was the page non-empty" — an account holding three lists answers
		// every startIndex with the same three, and erroring there would break
		// every list command for most users. Two things separate a server
		// paging in place from one that has simply run out: the batch starts
		// where page one starts (a server clamping an out-of-range startIndex
		// returns the tail instead, and that really is the end), and it is at
		// least as big as the page size the spec documents (below that it
		// cannot be a cap). An account whose list count lands exactly on a
		// clamping server's page boundary still trips this; that is a loud
		// error in an edge no spec describes, which is the trade this whole
		// function is: never a listing that is short and looks complete.
		if added == 0 {
			if len(batch) >= listPageDefault && len(all) > 0 && batch[0].ID == all[0].ID {
				return nil, fmt.Errorf(
					"digikey ignored startIndex while paging lists: the request at index %d resent the first page (%d lists)",
					len(all), len(batch))
			}
			return all, nil
		}
	}
	return nil, fmt.Errorf("digikey did not finish paging lists after %d requests (%d lists so far)",
		maxListPages, len(all))
}

// GET /lists/{listId} is deliberately not wrapped. It looks like the way to
// read one list, and it cannot be: live it answers with a correct TotalParts
// beside an empty PartsList, for every list, so no part can be looked up
// through it — `dk list set` did exactly that and could not find a single part
// in any list. Metadata already comes from Lists/AllLists via ResolveList, and
// contents from AllListParts, so wrapping it would only offer a caller a
// worse answer. See "Settled against the live API" in CONTRIBUTING.md.

// CreateList creates a list and returns its id.
func (c *Client) CreateList(ctx context.Context, req CreateListRequest) (string, error) {
	if strings.TrimSpace(req.ListName) == "" {
		return "", errors.New("list name is required")
	}
	// DigiKey returns the new list id as a bare JSON string.
	var id string
	err := c.do(ctx, request{
		method:      "POST",
		path:        myListsBasePath + "/lists",
		body:        req,
		requireUser: true,
		out:         &id,
		invalidates: ScopeLists,
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// DeleteList permanently removes a list.
func (c *Client) DeleteList(ctx context.Context, listID string) error {
	if strings.TrimSpace(listID) == "" {
		return errors.New("list id is required")
	}
	return c.do(ctx, request{
		method:      "DELETE",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID),
		requireUser: true,
		invalidates: ScopeLists,
	})
}

// RenameList changes a list's name.
func (c *Client) RenameList(ctx context.Context, listID, newName string) error {
	if strings.TrimSpace(listID) == "" {
		return errors.New("list id is required")
	}
	if strings.TrimSpace(newName) == "" {
		return errors.New("new list name is required")
	}
	return c.do(ctx, request{
		method:      "PUT",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/listName/" + url.PathEscape(newName),
		requireUser: true,
		invalidates: ScopeLists,
	})
}

// SuggestListName returns name if it is free, or a de-duplicated variant.
//
// MyLists v1 documents /lists/validate/name/{listName} for this, but DigiKey
// answers it with 404 "Invalid resource path" — the route is in the spec and
// not on the server. Rather than fail --auto-rename on an endpoint that does
// not exist, a 404 falls back to deriving a free name from the account's own
// lists. Every other error is returned: a 401 or a 429 says nothing about
// whether the name is taken, and inventing one from a listing dk could not
// read would be a guess.
func (c *Client) SuggestListName(ctx context.Context, name string) (string, error) {
	// Trimmed once, here, rather than at each comparison below: the local
	// fallback normalizes the names it reads from the account, so an untrimmed
	// candidate would miss a list differing only by surrounding space and
	// report a taken name as free — the one collision --auto-rename exists to
	// avoid. Both CLI callers already trim; this is the library boundary.
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("list name is required")
	}
	var suggested string
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists/validate/name/" + url.PathEscape(name),
		requireUser: true,
		out:         &suggested,
		// Deliberately uncached despite being a GET. The answer is "is this
		// name free right now", and --auto-rename acts on it immediately; a
		// reply from ten minutes ago would hand back a name that has since been
		// taken, which is the one thing this call exists to prevent.
	})
	if err == nil {
		return suggested, nil
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.NotFound() {
		return "", err
	}
	return c.suggestListNameLocally(ctx, name)
}

// suggestListNameLocally picks the first free "name (n)" by reading the
// account's lists. It is the fallback for DigiKey's missing validate route.
//
// The listing is read through Live so the names it compares against are
// current: --auto-rename exists to avoid colliding with a list that already
// exists, and a cached listing is exactly how it would collide anyway.
func (c *Client) suggestListNameLocally(ctx context.Context, name string) (string, error) {
	lists, err := c.AllLists(Live(ctx))
	if err != nil {
		return "", fmt.Errorf("suggest a free list name: %w", err)
	}

	taken := make(map[string]bool, len(lists))
	for _, l := range lists {
		taken[strings.ToLower(strings.TrimSpace(l.ListName))] = true
	}
	if !taken[strings.ToLower(name)] {
		return name, nil
	}
	// Bounded by the number of lists plus one: with n lists, at most n of the
	// candidates can be taken, so one of the first n+1 is always free.
	for i := 2; i <= len(lists)+2; i++ {
		candidate := fmt.Sprintf("%s (%d)", name, i)
		if !taken[strings.ToLower(candidate)] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free variant of list name %q", name)
}

// listPartsQuery builds the parts-endpoint query. Unlike the rest of MyLists,
// this endpoint takes locale as query parameters rather than headers.
func listPartsQuery(startIndex, limit int, locale Locale) url.Values {
	q := url.Values{}
	if locale.Site != "" {
		q.Set("countryIso", locale.Site)
	}
	if locale.Currency != "" {
		q.Set("currencyIso", locale.Currency)
	}
	if locale.Language != "" {
		q.Set("languageIso", locale.Language)
	}
	if startIndex > 0 {
		q.Set("startIndex", strconv.Itoa(startIndex))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return q
}

// ListParts returns resolved parts (with live pricing and stock) for a list.
func (c *Client) ListParts(ctx context.Context, listID string, startIndex, limit int, locale Locale) (*PartsResponse, error) {
	if strings.TrimSpace(listID) == "" {
		return nil, errors.New("list id is required")
	}
	var out PartsResponse
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/parts",
		query:       listPartsQuery(startIndex, limit, locale),
		requireUser: true,
		out:         &out,
		cacheScope:  ScopeLists,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Paging bounds for AllListParts, mirroring the list-level pair above.
//
// maxPartPages is generous because DigiKey may cap a page below partsPageSize,
// which costs iterations without costing parts: a 25-part cap on a 5000-part
// list needs 200 requests. Exhausting it is an error rather than a short
// result — silently returning a truncated list is the failure this whole
// function exists to prevent.
const (
	partsPageSize = 100
	maxPartPages  = 500
)

// AllListParts returns every part in a list, paging until it holds the count
// DigiKey reports or the server runs out of rows — never merely until a short
// page.
//
// The same argument as AllLists applies, and costs more here: a part on page
// two is indistinguishable from one that is absent, so `dk list rm` would
// report a part it can see in the web UI as not found. Worse, `dk list export`
// would write a BOM missing everything past the first page — silently, since
// TotalParts would still report the full count.
func (c *Client) AllListParts(ctx context.Context, listID string, locale Locale) (*PartsResponse, error) {
	all := &PartsResponse{PartsList: []ListPart{}}
	seen := make(map[string]bool)
	done := false

	for range maxPartPages {
		// Resume from what we actually hold rather than from page*pageSize.
		// DigiKey is free to return fewer rows than the limit asks for, and if
		// it caps pages below partsPageSize then treating a short page as the
		// end of the list truncates the BOM on the very first request — the bug
		// this function exists to prevent.
		batch, err := c.ListParts(ctx, listID, len(all.PartsList), partsPageSize, locale)
		if err != nil {
			return nil, err
		}
		if batch.TotalParts > all.TotalParts {
			all.TotalParts = batch.TotalParts
		}

		// Count only rows we have not already seen. A server that ignores
		// startIndex resends the same page forever; without this the loop would
		// pile up duplicates until the page cap.
		added := 0
		for _, p := range batch.PartsList {
			if p.UniqueID != "" {
				if seen[p.UniqueID] {
					continue
				}
				seen[p.UniqueID] = true
			}
			all.PartsList = append(all.PartsList, p)
			added++
		}

		// Holding the count DigiKey itself reports ends the walk, whatever the
		// next page would have said.
		if all.TotalParts > 0 && len(all.PartsList) >= all.TotalParts {
			done = true
			break
		}

		// Nothing new, and TotalParts says rows are missing. An empty page is
		// still the end: TotalParts can lag a delete, so it is no evidence the
		// server has rows left to give. A non-empty page that begins where page
		// one begins is different — the server is ignoring startIndex and will
		// never hand over the rest, so returning here would write a BOM missing
		// everything past the first page while TotalParts still reported the
		// full count. That is the exact failure this function exists to
		// prevent, so it is an error. Anything else is a tail already held,
		// which is what a server clamping an out-of-range startIndex returns,
		// and that really is the end.
		//
		// Unlike AllLists, no page-size heuristic is needed to tell a small
		// list from a capped one: the check above already ended the walk for
		// any list that fits in what we hold.
		if added == 0 {
			if len(batch.PartsList) > 0 && len(all.PartsList) > 0 &&
				batch.PartsList[0].UniqueID == all.PartsList[0].UniqueID {
				return nil, fmt.Errorf(
					"digikey ignored startIndex while paging list %s: the request at index %d resent the first page (%d parts of %d)",
					listID, len(all.PartsList), len(all.PartsList), all.TotalParts)
			}
			done = true
			break
		}
	}

	if !done {
		return nil, fmt.Errorf("digikey did not finish paging list %s after %d requests (%d parts so far)",
			listID, maxPartPages, len(all.PartsList))
	}

	// TotalParts can lag or be omitted; never report fewer than we hold.
	if all.TotalParts < len(all.PartsList) {
		all.TotalParts = len(all.PartsList)
	}
	return all, nil
}

// RawListParts returns one page of a list's parts exactly as DigiKey sent it.
//
// The flattened ListPartView deliberately hides the pack-option machinery, so
// this is the only way to see SelectedPackType and PackOptions[].PackType as
// DigiKey actually spells them — which is what `dk list show --raw` exists to
// expose. Same reasoning as RawKeywordSearch: decoding and re-encoding would
// drop whatever these structs do not model.
func (c *Client) RawListParts(ctx context.Context, listID string, startIndex, limit int, locale Locale) (json.RawMessage, error) {
	if strings.TrimSpace(listID) == "" {
		return nil, ErrListRefRequired
	}
	var out json.RawMessage
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/parts",
		query:       listPartsQuery(startIndex, limit, locale),
		requireUser: true,
		out:         &out,
		cacheScope:  ScopeLists,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddParts adds parts to a list and returns the unique ids DigiKey assigned,
// which are the handles used to update or remove individual lines.
//
// The endpoint takes an optional `index` query parameter that defaults to 0,
// so parts land at the head of the list rather than the tail. Nothing here
// sends it; the position is DigiKey's to choose and no caller depends on it.
func (c *Client) AddParts(ctx context.Context, listID string, parts []RequestedPart) ([]string, error) {
	if strings.TrimSpace(listID) == "" {
		return nil, errors.New("list id is required")
	}
	if len(parts) == 0 {
		return nil, errors.New("at least one part is required")
	}
	var ids []string
	err := c.do(ctx, request{
		method:      "POST",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/parts",
		body:        parts,
		requireUser: true,
		out:         &ids,
		invalidates: ScopeLists,
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// UpdatePart replaces a part line in a list. DigiKey expects the full
// RequestedPart, so callers should read-modify-write rather than send a patch.
func (c *Client) UpdatePart(ctx context.Context, listID, uniqueID string, part RequestedPart) error {
	if strings.TrimSpace(listID) == "" || strings.TrimSpace(uniqueID) == "" {
		return errors.New("list id and unique id are required")
	}
	return c.do(ctx, request{
		method:      "PUT",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/parts/" + url.PathEscape(uniqueID),
		body:        part,
		requireUser: true,
		invalidates: ScopeLists,
	})
}

// DeletePart removes a part line from a list.
func (c *Client) DeletePart(ctx context.Context, listID, uniqueID string) error {
	if strings.TrimSpace(listID) == "" || strings.TrimSpace(uniqueID) == "" {
		return errors.New("list id and unique id are required")
	}
	return c.do(ctx, request{
		method:      "DELETE",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/parts/" + url.PathEscape(uniqueID),
		requireUser: true,
		invalidates: ScopeLists,
	})
}

// ResolveList turns a user-supplied list name or id into a list id.
//
// DigiKey list ids are opaque GUIDs, which are painful for humans and agents to
// pass around, so every list command accepts a name. An exact case-sensitive
// name match wins; otherwise a unique case-insensitive match is accepted.
func (c *Client) ResolveList(ctx context.Context, nameOrID string) (*ListSummary, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, ErrListRefRequired
	}

	lists, err := c.AllLists(ctx)
	if err != nil {
		return nil, err
	}

	for i := range lists {
		if lists[i].ID == nameOrID {
			return &lists[i], nil
		}
	}

	var exact, fold []ListSummary
	for i := range lists {
		switch {
		case lists[i].ListName == nameOrID:
			exact = append(exact, lists[i])
		case strings.EqualFold(lists[i].ListName, nameOrID):
			fold = append(fold, lists[i])
		}
	}

	candidates := exact
	if len(candidates) == 0 {
		candidates = fold
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("%w: %q", ErrListNotFound, nameOrID)
	case 1:
		return &candidates[0], nil
	default:
		return nil, &ErrAmbiguousList{Name: nameOrID, Candidate: candidates}
	}
}
