package digikey

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// productsBasePath is the Product Information v4 base path.
const productsBasePath = "/products/v4"

// SearchOption values accepted by FilterOptionsRequest.SearchOptions.
const (
	SearchOptionInStock          = "InStock"
	SearchOptionNormallyStocking = "NormallyStocking"
	SearchOptionHasDatasheet     = "HasDatasheet"
	SearchOptionHas3DModel       = "Has3DModel"
	SearchOptionHasCadModel      = "HasCadModel"
	SearchOptionHasProductPhoto  = "HasProductPhoto"
	SearchOptionRohsCompliant    = "RohsCompliant"
	SearchOptionNewProduct       = "NewProduct"
	SearchOptionChipOutpost      = "ChipOutpost"
)

// FilterID is DigiKey's `{"Id": "..."}` filter wrapper.
type FilterID struct {
	ID string `json:"Id,omitempty"`
}

// ParametricCategory constrains a search to specific values of one parameter.
type ParametricCategory struct {
	ParameterID  int        `json:"ParameterId,omitempty"`
	FilterValues []FilterID `json:"FilterValues,omitempty"`
}

// ParameterFilterRequest holds parametric constraints, which DigiKey only
// honors alongside a category filter.
type ParameterFilterRequest struct {
	CategoryFilter   *FilterID            `json:"CategoryFilter,omitempty"`
	ParameterFilters []ParametricCategory `json:"ParameterFilters,omitempty"`
}

// FilterOptionsRequest narrows a keyword search.
type FilterOptionsRequest struct {
	ManufacturerFilter       []FilterID              `json:"ManufacturerFilter,omitempty"`
	CategoryFilter           []FilterID              `json:"CategoryFilter,omitempty"`
	StatusFilter             []FilterID              `json:"StatusFilter,omitempty"`
	PackagingFilter          []FilterID              `json:"PackagingFilter,omitempty"`
	MarketPlaceFilter        string                  `json:"MarketPlaceFilter,omitempty"`
	SeriesFilter             []FilterID              `json:"SeriesFilter,omitempty"`
	MinimumQuantityAvailable int                     `json:"MinimumQuantityAvailable,omitempty"`
	ParameterFilterRequest   *ParameterFilterRequest `json:"ParameterFilterRequest,omitempty"`
	SearchOptions            []string                `json:"SearchOptions,omitempty"`
}

// SortOptions orders keyword search results.
type SortOptions struct {
	Field     string `json:"Field,omitempty"`
	SortOrder string `json:"SortOrder,omitempty"`
}

// KeywordRequest is the POST body for /search/keyword.
type KeywordRequest struct {
	Keywords             string                `json:"Keywords,omitempty"`
	Limit                int                   `json:"Limit,omitempty"`
	Offset               int                   `json:"Offset,omitempty"`
	FilterOptionsRequest *FilterOptionsRequest `json:"FilterOptionsRequest,omitempty"`
	SortOptions          *SortOptions          `json:"SortOptions,omitempty"`
}

// Description holds a product's short and long descriptions.
type Description struct {
	ProductDescription  string `json:"ProductDescription"`
	DetailedDescription string `json:"DetailedDescription"`
}

// NamedID is DigiKey's recurring `{Id, Name}` shape (manufacturer, series,
// package type, supplier, base product).
type NamedID struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
}

// ProductStatus is a product's lifecycle status, e.g. "Active".
type ProductStatus struct {
	ID     int    `json:"Id"`
	Status string `json:"Status"`
}

// PriceBreak is one tier of quantity pricing.
type PriceBreak struct {
	BreakQuantity int     `json:"BreakQuantity"`
	UnitPrice     float64 `json:"UnitPrice"`
	TotalPrice    float64 `json:"TotalPrice"`
}

// ProductVariation is one orderable packaging option, and carries the DigiKey
// part number actually used when ordering or adding to a list.
type ProductVariation struct {
	DigiKeyProductNumber string       `json:"DigiKeyProductNumber"`
	PackageType          NamedID      `json:"PackageType"`
	StandardPricing      []PriceBreak `json:"StandardPricing"`
	MyPricing            []PriceBreak `json:"MyPricing"`
	MarketPlace          bool         `json:"MarketPlace"`
	TariffActive         bool         `json:"TariffActive"`
	Supplier             NamedID      `json:"Supplier"`
	// QuantityAvailableforPackageType is spelled with a lowercase "f" in the
	// DigiKey schema; QuantityAvailable is the older v4 alias.
	QuantityAvailableForPackageType int     `json:"QuantityAvailableforPackageType"`
	QuantityAvailable               int     `json:"QuantityAvailable"`
	MaxQuantityForDistribution      int     `json:"MaxQuantityForDistribution"`
	MinimumOrderQuantity            int     `json:"MinimumOrderQuantity"`
	StandardPackage                 int     `json:"StandardPackage"`
	DigiReelFee                     float64 `json:"DigiReelFee"`
}

// Stock returns the variation's available quantity, tolerating either spelling.
func (v ProductVariation) Stock() int {
	if v.QuantityAvailableForPackageType > 0 {
		return v.QuantityAvailableForPackageType
	}
	return v.QuantityAvailable
}

// ParameterValue is one parametric attribute of a product, e.g. "Capacitance: 100 nF".
type ParameterValue struct {
	ParameterID   int    `json:"ParameterId"`
	ParameterText string `json:"ParameterText"`
	ParameterType string `json:"ParameterType"`
	ValueID       string `json:"ValueId"`
	ValueText     string `json:"ValueText"`
}

// CategoryNode is a node in DigiKey's category taxonomy. The taxonomy
// endpoints nest children under "Children" while the copy embedded in a
// Product uses "ChildCategories"; both are decoded, and Children() picks
// whichever is populated.
type CategoryNode struct {
	CategoryID      int            `json:"CategoryId"`
	ParentID        int            `json:"ParentId"`
	Name            string         `json:"Name"`
	ProductCount    int            `json:"ProductCount"`
	NewProductCount int            `json:"NewProductCount"`
	SeoDescription  string         `json:"SeoDescription"`
	ChildCategories []CategoryNode `json:"ChildCategories,omitempty"`
	ChildNodes      []CategoryNode `json:"Children,omitempty"`
}

// Children returns the node's subcategories regardless of which field name the
// endpoint used.
func (c CategoryNode) Children() []CategoryNode {
	if len(c.ChildCategories) > 0 {
		return c.ChildCategories
	}
	return c.ChildNodes
}

// Classifications holds compliance metadata (RoHS, REACH, ECCN, HTSUS).
type Classifications struct {
	ReachStatus              string `json:"ReachStatus"`
	RohsStatus               string `json:"RohsStatus"`
	MoistureSensitivityLevel string `json:"MoistureSensitivityLevel"`
	ExportControlClassNumber string `json:"ExportControlClassNumber"`
	HtsusCode                string `json:"HtsusCode"`
}

// Product is a DigiKey catalog product.
type Product struct {
	Description                Description        `json:"Description"`
	Manufacturer               NamedID            `json:"Manufacturer"`
	ManufacturerProductNumber  string             `json:"ManufacturerProductNumber"`
	UnitPrice                  float64            `json:"UnitPrice"`
	ProductURL                 string             `json:"ProductUrl"`
	DatasheetURL               string             `json:"DatasheetUrl"`
	PhotoURL                   string             `json:"PhotoUrl"`
	ProductVariations          []ProductVariation `json:"ProductVariations"`
	QuantityAvailable          int                `json:"QuantityAvailable"`
	ProductStatus              ProductStatus      `json:"ProductStatus"`
	BackOrderNotAllowed        bool               `json:"BackOrderNotAllowed"`
	NormallyStocking           bool               `json:"NormallyStocking"`
	Discontinued               bool               `json:"Discontinued"`
	EndOfLife                  bool               `json:"EndOfLife"`
	Ncnr                       bool               `json:"Ncnr"`
	PrimaryVideoURL            string             `json:"PrimaryVideoUrl"`
	Parameters                 []ParameterValue   `json:"Parameters"`
	BaseProductNumber          NamedID            `json:"BaseProductNumber"`
	Category                   CategoryNode       `json:"Category"`
	DateLastBuyChance          string             `json:"DateLastBuyChance"`
	ManufacturerLeadWeeks      string             `json:"ManufacturerLeadWeeks"`
	ManufacturerPublicQuantity int                `json:"ManufacturerPublicQuantity"`
	Series                     NamedID            `json:"Series"`
	ShippingInfo               string             `json:"ShippingInfo"`
	Classifications            Classifications    `json:"Classifications"`
	OtherNames                 []string           `json:"OtherNames"`
	// MinimumOrderQuantity is not returned at the product level by v4; it lives
	// on each variation. Kept for callers that set it from a chosen variation.
	MinimumOrderQuantity int `json:"MinimumOrderQuantity,omitempty"`
}

// PrimaryVariation returns the variation a buyer would most likely order: the
// cheapest in-stock option, falling back to the first listed. Returns false
// when the product has no variations.
func (p Product) PrimaryVariation() (ProductVariation, bool) {
	if len(p.ProductVariations) == 0 {
		return ProductVariation{}, false
	}
	best := -1
	for i, v := range p.ProductVariations {
		if v.Stock() <= 0 {
			continue
		}
		if best < 0 || unitPriceOf(v) < unitPriceOf(p.ProductVariations[best]) {
			best = i
		}
	}
	if best < 0 {
		return p.ProductVariations[0], true
	}
	return p.ProductVariations[best], true
}

// DigiKeyPartNumber returns the part number to order or add to a list.
func (p Product) DigiKeyPartNumber() string {
	if v, ok := p.PrimaryVariation(); ok {
		return v.DigiKeyProductNumber
	}
	return ""
}

// unitPriceOf returns a variation's lowest-quantity unit price, preferring
// account-specific pricing when the token was 3-legged.
func unitPriceOf(v ProductVariation) float64 {
	breaks := v.MyPricing
	if len(breaks) == 0 {
		breaks = v.StandardPricing
	}
	if len(breaks) == 0 {
		return 0
	}
	best := breaks[0]
	for _, b := range breaks[1:] {
		if b.BreakQuantity < best.BreakQuantity {
			best = b
		}
	}
	return best.UnitPrice
}

// IsoSearchLocale echoes the locale DigiKey actually applied to the request.
type IsoSearchLocale struct {
	Site     string `json:"Site"`
	Language string `json:"Language"`
	Currency string `json:"Currency"`
}

// KeywordResponse is the /search/keyword result.
type KeywordResponse struct {
	Products         []Product       `json:"Products"`
	ProductsCount    int             `json:"ProductsCount"`
	ExactMatches     []Product       `json:"ExactMatches"`
	SearchLocaleUsed IsoSearchLocale `json:"SearchLocaleUsed"`
}

// ProductDetails is the /search/{productNumber}/productdetails result.
type ProductDetails struct {
	SearchLocaleUsed IsoSearchLocale `json:"SearchLocaleUsed"`
	Product          Product         `json:"Product"`
}

// ManufacturersResponse is the /search/manufacturers result.
type ManufacturersResponse struct {
	Manufacturers []NamedID `json:"Manufacturers"`
}

// CategoriesResponse is the /search/categories result.
type CategoriesResponse struct {
	Categories []CategoryNode `json:"Categories"`
}

// KeywordSearch searches the DigiKey catalog. DigiKey caps Limit at 50.
func (c *Client) KeywordSearch(ctx context.Context, req KeywordRequest) (*KeywordResponse, error) {
	var out KeywordResponse
	err := c.do(ctx, request{
		method: "POST",
		path:   productsBasePath + "/search/keyword",
		body:   req,
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ProductDetails fetches full detail for a DigiKey or manufacturer part number.
func (c *Client) ProductDetails(ctx context.Context, partNumber string) (*ProductDetails, error) {
	if strings.TrimSpace(partNumber) == "" {
		return nil, fmt.Errorf("product number is required")
	}
	var out ProductDetails
	err := c.do(ctx, request{
		method: "GET",
		path:   productsBasePath + "/search/" + url.PathEscape(partNumber) + "/productdetails",
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ProductSubstitute is a suggested alternative for a part. It is a flatter
// shape than Product: DigiKey returns UnitPrice as a preformatted string here.
type ProductSubstitute struct {
	SubstituteType            string  `json:"SubstituteType"`
	ProductURL                string  `json:"ProductUrl"`
	Description               string  `json:"Description"`
	Manufacturer              NamedID `json:"Manufacturer"`
	ManufacturerProductNumber string  `json:"ManufacturerProductNumber"`
	UnitPrice                 string  `json:"UnitPrice"`
	QuantityAvailable         int     `json:"QuantityAvailable"`
	DigiKeyProductNumber      string  `json:"DigiKeyProductNumber"`
}

// ProductSubstitutesResponse is the /search/{productNumber}/substitutions result.
type ProductSubstitutesResponse struct {
	ProductSubstitutesCount int                 `json:"ProductSubstitutesCount"`
	ProductSubstitutes      []ProductSubstitute `json:"ProductSubstitutes"`
	SearchLocaleUsed        IsoSearchLocale     `json:"SearchLocaleUsed"`
}

// Substitutions returns substitute products for a part number. Useful when a
// preferred part is out of stock.
func (c *Client) Substitutions(ctx context.Context, partNumber string) (*ProductSubstitutesResponse, error) {
	if strings.TrimSpace(partNumber) == "" {
		return nil, fmt.Errorf("product number is required")
	}
	var out ProductSubstitutesResponse
	err := c.do(ctx, request{
		method: "GET",
		path:   productsBasePath + "/search/" + url.PathEscape(partNumber) + "/substitutions",
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Manufacturers returns the manufacturer list, whose ids feed
// FilterOptionsRequest.ManufacturerFilter.
func (c *Client) Manufacturers(ctx context.Context) ([]NamedID, error) {
	var out ManufacturersResponse
	err := c.do(ctx, request{
		method: "GET",
		path:   productsBasePath + "/search/manufacturers",
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return out.Manufacturers, nil
}

// Categories returns the top-level category taxonomy, whose ids feed
// FilterOptionsRequest.CategoryFilter.
func (c *Client) Categories(ctx context.Context) ([]CategoryNode, error) {
	var out CategoriesResponse
	err := c.do(ctx, request{
		method: "GET",
		path:   productsBasePath + "/search/categories",
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return out.Categories, nil
}

// Category returns a single category subtree by id.
func (c *Client) Category(ctx context.Context, categoryID int) (*CategoryNode, error) {
	var out struct {
		Category CategoryNode `json:"Category"`
	}
	err := c.do(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("%s/search/categories/%d", productsBasePath, categoryID),
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return &out.Category, nil
}
