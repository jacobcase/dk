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
}

// ListSummary is the metadata DigiKey returns for a list. The parts array is
// only populated by GetListByListId.
type ListSummary struct {
	ID               string          `json:"Id"`
	ListName         string          `json:"ListName"`
	CreatedBy        string          `json:"CreatedBy"`
	CustomerID       int             `json:"CustomerId"`
	AccountID        int             `json:"AccountId"`
	CompanyName      string          `json:"CompanyName"`
	Notes            string          `json:"Notes"`
	TotalParts       int             `json:"TotalParts"`
	DateCreated      string          `json:"DateCreated"`
	DateLastAccessed string          `json:"DateLastAccessed"`
	DateModified     string          `json:"DateModified"`
	Tags             []string        `json:"Tags"`
	ListSettings     *ListSettings   `json:"ListSettings,omitempty"`
	PartsList        []RequestedPart `json:"PartsList,omitempty"`
	CanEdit          bool            `json:"CanEdit"`
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
	PackOptions             []ListPackOption `json:"PackOptions"`
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
	PartID                    int                `json:"PartId"`
	UniqueID                  string             `json:"UniqueId"`
	CustomerReference         string             `json:"CustomerReference"`
	ReferenceDesignator       string             `json:"ReferenceDesignator"`
	Notes                     string             `json:"Notes"`
	MinOrderQty               int                `json:"MinOrderQty"`
	RequestedPartNumber       string             `json:"RequestedPartNumber"`
	DigiKeyPartNumber         string             `json:"DigiKeyPartNumber"`
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
// RequestedQty already sums all lines, so the money has to as well: reporting
// one line's extended price next to every line's quantity would understate the
// cost of a part that carries more than one line.
func (p ListPart) pricedLines() (extended float64, quantity int, firstUnit float64, lines int) {
	for _, q := range p.Quantities {
		opt, ok := q.SelectedPackOption()
		if !ok {
			continue
		}
		if lines == 0 {
			firstUnit = opt.CalculatedUnitPrice
		}
		lines++
		extended += opt.ExtendedPrice
		quantity += q.QuantityRequested
	}
	return extended, quantity, firstUnit, lines
}

// UnitPrice returns the unit price of the selected pack option, or 0 if DigiKey
// did not resolve one (e.g. an unmatched part number).
//
// With the usual single quantity line this is that line's price. With several,
// it is the quantity-weighted average, so UnitPrice multiplied by RequestedQty
// still agrees with ExtendedPrice.
func (p ListPart) UnitPrice() float64 {
	extended, quantity, firstUnit, lines := p.pricedLines()
	if lines <= 1 || extended <= 0 || quantity <= 0 {
		return firstUnit
	}
	return extended / float64(quantity)
}

// ExtendedPrice returns the line total for this part, summed across every
// quantity line, or 0 if unresolved.
func (p ListPart) ExtendedPrice() float64 {
	extended, _, _, _ := p.pricedLines()
	return extended
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

// Paging bounds for AllLists. The spec documents /lists as defaulting to 50 per
// page, so asking for listPageSize is asking for more than DigiKey promises;
// the loop below therefore cannot treat a short page as the end. maxListPages
// guards against a server that ignores startIndex and would otherwise make the
// loop run forever.
const (
	listPageSize = 100
	maxListPages = 500
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
		// do not stop on a short page. DigiKey documents /lists as 50 per page
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

		// A page that added nothing new ends the walk — but only if it was not
		// a full page. A *full* page of rows we already hold means the server
		// ignored startIndex and is resending page one; returning there would
		// silently truncate, which is the failure this function exists to
		// prevent, so it is an error instead.
		if added == 0 {
			if len(batch) == listPageSize {
				break
			}
			return all, nil
		}
	}
	return nil, fmt.Errorf("digikey did not finish paging lists after %d requests (%d lists so far)",
		maxListPages, len(all))
}

// GetList returns a list's metadata and requested parts by id.
func (c *Client) GetList(ctx context.Context, listID string) (*ListSummary, error) {
	if strings.TrimSpace(listID) == "" {
		return nil, errors.New("list id is required")
	}
	var out ListSummary
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID),
		requireUser: true,
		out:         &out,
		cacheScope:  ScopeLists,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

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
func (c *Client) SuggestListName(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
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
	if err != nil {
		return "", err
	}
	return suggested, nil
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

// AllListParts returns every part in a list, paging until DigiKey returns a
// short page.
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

		// No new rows means the list ended, or the server is not paging at all.
		// Either way there is nothing further to fetch.
		if added == 0 || (all.TotalParts > 0 && len(all.PartsList) >= all.TotalParts) {
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
