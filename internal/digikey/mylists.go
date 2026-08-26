package digikey

import (
	"context"
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

// ListPartQuantity is the resolved quantity/pricing for a part in a list.
type ListPartQuantity struct {
	QuantityRequested  int     `json:"QuantityRequested"`
	CalculatedQuantity int     `json:"CalculatedQuantity"`
	TargetPrice        float64 `json:"TargetPrice"`
	SelectedPackType   string  `json:"SelectedPackType"`
	PackOptions        []struct {
		DigiKeyPartNumber   string  `json:"DigiKeyPartNumber"`
		PackType            string  `json:"PackType"`
		Quantity            int     `json:"Quantity"`
		QuantityAvailable   int     `json:"QuantityAvailable"`
		CalculatedUnitPrice float64 `json:"CalculatedUnitPrice"`
		ExtendedPrice       float64 `json:"ExtendedPrice"`
	} `json:"PackOptions"`
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

// UnitPrice returns the unit price of the selected pack option, or 0 if DigiKey
// did not resolve one (e.g. an unmatched part number).
func (p ListPart) UnitPrice() float64 {
	for _, q := range p.Quantities {
		for _, opt := range q.PackOptions {
			if opt.CalculatedUnitPrice > 0 {
				return opt.CalculatedUnitPrice
			}
		}
	}
	return 0
}

// ExtendedPrice returns the line total for this part, or 0 if unresolved.
func (p ListPart) ExtendedPrice() float64 {
	for _, q := range p.Quantities {
		for _, opt := range q.PackOptions {
			if opt.ExtendedPrice > 0 {
				return opt.ExtendedPrice
			}
		}
	}
	return 0
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
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
	})
}

// IsValidListName reports whether a list name is still available.
func (c *Client) IsValidListName(ctx context.Context, name string) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, errors.New("list name is required")
	}
	var ok bool
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists/validate/" + url.PathEscape(name),
		requireUser: true,
		out:         &ok,
	})
	if err != nil {
		return false, err
	}
	return ok, nil
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
	})
	if err != nil {
		return "", err
	}
	return suggested, nil
}

// ListParts returns resolved parts (with live pricing and stock) for a list.
func (c *Client) ListParts(ctx context.Context, listID string, startIndex, limit int, locale Locale) (*PartsResponse, error) {
	if strings.TrimSpace(listID) == "" {
		return nil, errors.New("list id is required")
	}
	// This endpoint takes locale as query parameters rather than headers.
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

	var out PartsResponse
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/parts",
		query:       q,
		requireUser: true,
		out:         &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AddParts appends parts to a list and returns the unique ids DigiKey assigned,
// which are the handles used to update or remove individual lines.
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
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// GetPart returns one resolved part from a list by its unique id.
func (c *Client) GetPart(ctx context.Context, listID, uniqueID string, locale Locale) (*ListPart, error) {
	if strings.TrimSpace(listID) == "" || strings.TrimSpace(uniqueID) == "" {
		return nil, errors.New("list id and unique id are required")
	}
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

	var out ListPart
	err := c.do(ctx, request{
		method:      "GET",
		path:        myListsBasePath + "/lists/" + url.PathEscape(listID) + "/parts/" + url.PathEscape(uniqueID),
		query:       q,
		requireUser: true,
		out:         &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
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
		return nil, errors.New("list name or id is required")
	}

	lists, err := c.Lists(ctx, 0, 0)
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
