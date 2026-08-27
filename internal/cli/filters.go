package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jacobcase/dk/internal/digikey"
	"github.com/jacobcase/dk/internal/output"
)

// discoveryLimit is the page size used for facet-discovery searches. DigiKey
// returns the full FilterOptions facet set regardless of how many products come
// back, so ask for the minimum.
const discoveryLimit = 1

// CategoryRef identifies a category in filter output.
type CategoryRef struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	ProductCount int     `json:"product_count,omitempty"`
	Score        float64 `json:"score,omitempty"`
}

// FacetValue is one selectable value of a non-parametric filter.
type FacetValue struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ProductCount int    `json:"product_count,omitempty"`
}

// ParameterFacet is one filterable parameter and the values available for it.
type ParameterFacet struct {
	ParameterID   int                   `json:"parameter_id"`
	ParameterName string                `json:"parameter_name"`
	ParameterType string                `json:"parameter_type,omitempty"`
	CategoryID    int                   `json:"category_id,omitempty"`
	CategoryName  string                `json:"category_name,omitempty"`
	ValueCount    int                   `json:"value_count"`
	Values        []ParameterFacetValue `json:"values"`
}

// ParameterFacetValue is one value of a parametric filter. RangeType is set only
// for the synthetic Min/Max/Range entries DigiKey adds to numeric parameters.
type ParameterFacetValue struct {
	ValueID      string `json:"value_id"`
	ValueName    string `json:"value_name"`
	ProductCount int    `json:"product_count,omitempty"`
	RangeType    string `json:"range_type,omitempty"`
}

// FiltersResult is the JSON shape of `dk filters`.
type FiltersResult struct {
	Query        string `json:"query"`
	TotalMatches int    `json:"total_matches"`
	// Category is the category parametric filters apply to. Parameter ids are
	// category-scoped, so `dk search --param` needs this to be unambiguous.
	Category      *CategoryRef     `json:"category,omitempty"`
	TopCategories []CategoryRef    `json:"top_categories,omitempty"`
	Parameters    []ParameterFacet `json:"parameters"`
	Manufacturers []FacetValue     `json:"manufacturers,omitempty"`
	Packaging     []FacetValue     `json:"packaging,omitempty"`
	Status        []FacetValue     `json:"status,omitempty"`
	Series        []FacetValue     `json:"series,omitempty"`
}

func newFiltersCommand(app *App) *cobra.Command {
	var (
		category  string
		parameter string
		inStock   bool
		maxValues int
		allValues bool
	)

	cmd := &cobra.Command{
		Use:     "filters <keywords...>",
		Aliases: []string{"facets"},
		Short:   "Discover the filters available for a search",
		Long: `Show which parameters can narrow a search, and what values each one offers.

DigiKey has no endpoint that lists the filters for a category in the abstract.
Filters are discovered from a search: this command runs your query and reports
the facets DigiKey returned for that result set. So the workflow is a loop:

  dk filters "0603 ceramic capacitor"              # what can I filter on?
  dk filters "0603 ceramic capacitor" --parameter Capacitance   # what values?
  dk search "0603 ceramic capacitor" --param "Capacitance=0.1 µF" --param "Tolerance=±10%"

Because the values shown are the ones present in the current result set, the
product counts tell you how much each choice would narrow things.

Parameter ids are scoped to a category, so parametric filtering needs one. It is
inferred from the best-matching category for your keywords; pass --category to
pin it explicitly.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keywords := strings.Join(args, " ")
			if maxValues < 1 {
				return usageErrorf("--values must be at least 1")
			}

			ctx := cmd.Context()
			client, err := app.Client()
			if err != nil {
				return err
			}

			facets, resp, err := discoverFacets(ctx, client, keywords, category, inStock)
			if err != nil {
				return err
			}

			result := buildFiltersResult(keywords, resp.ProductsCount, facets)

			// Drilling into one parameter is the second half of the loop: the
			// overview caps each value list, this shows one in full.
			if parameter != "" {
				facet, err := findParameterFacet(result.Parameters, parameter)
				if err != nil {
					return err
				}
				return app.Printer.Print(facet, parameterValuesTable(*facet))
			}

			limit := maxValues
			if allValues {
				limit = 0
			}
			if err := app.Printer.Print(result, parametersTableFor(result, limit)); err != nil {
				return err
			}

			if result.Category != nil {
				app.Printer.PrintText("\nCategory: %s (id %d)", result.Category.Name, result.Category.ID)
			}
			app.Printer.PrintText("%d products match %q.", result.TotalMatches, keywords)
			if len(result.Parameters) > 0 {
				app.Printer.PrintText("Drill into one with `dk filters %q --parameter <name>`, "+
					"then apply it with `dk search %q --param \"<name>=<value>\"`.", keywords, keywords)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&category, "category", "", "scope filters to a category by name or id")
	f.StringVar(&parameter, "parameter", "", "show every value of one parameter, by name or id")
	f.BoolVar(&inStock, "in-stock", false, "only consider products with stock on hand")
	f.IntVar(&maxValues, "values", 6, "values to show per parameter in table output (JSON always has all)")
	f.BoolVar(&allValues, "all-values", false, "show every value of every parameter")

	return cmd
}

// discoverFacets runs a minimal keyword search purely to collect the facet set.
func discoverFacets(ctx context.Context, client *digikey.Client, keywords, category string, inStock bool) (digikey.FilterOptions, *digikey.KeywordResponse, error) {
	req := digikey.KeywordRequest{Keywords: keywords, Limit: discoveryLimit}

	filters := &digikey.FilterOptionsRequest{}
	if inStock {
		filters.SearchOptions = append(filters.SearchOptions, digikey.SearchOptionInStock)
	}
	if category != "" {
		ids, err := resolveCategoryIDs(ctx, client, []string{category})
		if err != nil {
			return digikey.FilterOptions{}, nil, err
		}
		filters.CategoryFilter = ids
	}
	if !isZeroFilter(filters) {
		req.FilterOptionsRequest = filters
	}

	resp, err := client.KeywordSearch(ctx, req)
	if err != nil {
		return digikey.FilterOptions{}, nil, err
	}
	return resp.FilterOptions, resp, nil
}

// buildFiltersResult converts DigiKey's facet payload into dk's view.
func buildFiltersResult(keywords string, total int, facets digikey.FilterOptions) FiltersResult {
	result := FiltersResult{
		Query:         keywords,
		TotalMatches:  total,
		Manufacturers: baseFacets(facets.Manufacturers),
		Packaging:     baseFacets(facets.Packaging),
		Status:        baseFacets(facets.Status),
		Series:        baseFacets(facets.Series),
	}

	for _, tc := range facets.TopCategories {
		if tc.Category.ID == 0 {
			continue
		}
		result.TopCategories = append(result.TopCategories, CategoryRef{
			ID:           tc.Category.ID,
			Name:         tc.Category.Name,
			ProductCount: tc.Category.ProductCount,
			Score:        tc.Score,
		})
	}
	if best, ok := facets.BestCategory(); ok {
		result.Category = &CategoryRef{ID: best.ID, Name: best.Name, ProductCount: best.ProductCount}
	}

	for _, pf := range facets.ParametricFilters {
		facet := ParameterFacet{
			ParameterID:   pf.ParameterID,
			ParameterName: pf.ParameterName,
			ParameterType: pf.ParameterType,
			CategoryID:    pf.Category.ID,
			CategoryName:  pf.Category.Value,
			ValueCount:    len(pf.FilterValues),
		}
		for _, v := range pf.FilterValues {
			facet.Values = append(facet.Values, ParameterFacetValue{
				ValueID:      v.ValueID,
				ValueName:    v.ValueName,
				ProductCount: v.ProductCount,
				RangeType:    v.RangeFilterType,
			})
		}
		result.Parameters = append(result.Parameters, facet)
	}

	// The parametric filters carry their own category, which is more precise
	// than the keyword-derived guess when the two disagree.
	if catID, catName, ok := commonParameterCategory(result.Parameters); ok {
		result.Category = &CategoryRef{ID: catID, Name: catName}
	}
	return result
}

// commonParameterCategory returns the category shared by every parametric
// filter, if they agree on one.
func commonParameterCategory(params []ParameterFacet) (int, string, bool) {
	id, name := 0, ""
	for _, p := range params {
		if p.CategoryID == 0 {
			continue
		}
		if id == 0 {
			id, name = p.CategoryID, p.CategoryName
			continue
		}
		if p.CategoryID != id {
			return 0, "", false
		}
	}
	return id, name, id != 0
}

func baseFacets(in []digikey.BaseFilter) []FacetValue {
	out := make([]FacetValue, 0, len(in))
	for _, f := range in {
		out = append(out, FacetValue{ID: strconv.Itoa(f.ID), Name: f.Value, ProductCount: f.ProductCount})
	}
	return out
}

// parametersTableFor renders the overview. maxValues caps each value list; 0
// means show them all.
func parametersTableFor(r FiltersResult, maxValues int) *output.Table {
	t := &output.Table{
		Headers: []string{"PARAM ID", "PARAMETER", "TYPE", "VALUES"},
		Empty:   "DigiKey returned no parametric filters for this search. Try narrower keywords or --category.",
	}
	for _, p := range r.Parameters {
		t.AddRow(p.ParameterID, p.ParameterName, p.ParameterType, summarizeValues(p, maxValues))
	}
	return t
}

// summarizeValues renders a parameter's values inline, with product counts so
// the caller can see how much each choice narrows the search.
func summarizeValues(p ParameterFacet, maxValues int) string {
	parts := make([]string, 0, len(p.Values))
	for i, v := range p.Values {
		if maxValues > 0 && i == maxValues {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(p.Values)-maxValues))
			break
		}
		name := v.ValueName
		if v.RangeType != "" {
			name = v.RangeType + ":" + name
		}
		if v.ProductCount > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d)", name, v.ProductCount))
			continue
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

func parameterValuesTable(p ParameterFacet) *output.Table {
	t := &output.Table{
		Headers: []string{"VALUE ID", "VALUE", "PRODUCTS", "RANGE"},
		Empty:   "This parameter has no values in the current result set.",
	}
	for _, v := range p.Values {
		t.AddRow(v.ValueID, v.ValueName, v.ProductCount, v.RangeType)
	}
	return t
}

// findParameterFacet resolves a --parameter argument against the discovered
// facets, by numeric id or by name.
func findParameterFacet(params []ParameterFacet, key string) (*ParameterFacet, error) {
	key = strings.TrimSpace(key)
	if id, err := strconv.Atoi(key); err == nil {
		for i := range params {
			if params[i].ParameterID == id {
				return &params[i], nil
			}
		}
		return nil, usageErrorf("no parameter with id %d in this result set; run `dk filters` without --parameter to see the available ids", id)
	}

	lower := strings.ToLower(key)
	var exact, partial []int
	for i, p := range params {
		name := strings.ToLower(p.ParameterName)
		switch {
		case name == lower:
			exact = append(exact, i)
		case strings.Contains(name, lower):
			partial = append(partial, i)
		}
	}
	if len(exact) == 1 {
		return &params[exact[0]], nil
	}

	candidates := exact
	if len(candidates) == 0 {
		candidates = partial
	}
	switch len(candidates) {
	case 0:
		return nil, usageErrorf("no parameter named %q in this result set (available: %s)", key, parameterNames(params, 8))
	case 1:
		return &params[candidates[0]], nil
	default:
		names := make([]string, 0, len(candidates))
		for _, i := range candidates {
			names = append(names, fmt.Sprintf("%s (id %d)", params[i].ParameterName, params[i].ParameterID))
		}
		return nil, usageErrorf("parameter %q is ambiguous: %s", key, strings.Join(names, ", "))
	}
}

func parameterNames(params []ParameterFacet, max int) string {
	names := make([]string, 0, max)
	for i, p := range params {
		if i == max {
			names = append(names, "...")
			break
		}
		names = append(names, p.ParameterName)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// ParamSpec is one --param constraint: a parameter, plus the values that would
// satisfy it. Multiple values on one parameter are OR-ed by DigiKey; separate
// parameters are AND-ed.
type ParamSpec struct {
	Key    string
	Values []string
}

// parseParamSpec parses `NAME=VALUE` or `NAME=VALUE,VALUE`.
//
// The split is on the first `=`, since parameter names never contain one but
// values sometimes do (a tolerance of "=0.1%", a voltage range).
func parseParamSpec(arg string) (ParamSpec, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ParamSpec{}, usageErrorf("empty --param value")
	}

	key, rest, found := strings.Cut(arg, "=")
	if !found {
		return ParamSpec{}, usageErrorf("invalid --param %q: expected NAME=VALUE, e.g. --param \"Tolerance=±10%%\"", arg)
	}

	key = strings.TrimSpace(key)
	if key == "" {
		return ParamSpec{}, usageErrorf("invalid --param %q: the parameter name is empty", arg)
	}

	var values []string
	for _, v := range strings.Split(rest, ",") {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return ParamSpec{}, usageErrorf("invalid --param %q: no value given for %q", arg, key)
	}
	return ParamSpec{Key: key, Values: values}, nil
}

// mergeParamSpecs combines repeated --param flags naming the same parameter,
// so `--param "X=a" --param "X=b"` means the same as `--param "X=a,b"`. That
// matters for values that themselves contain a comma.
func mergeParamSpecs(specs []ParamSpec) []ParamSpec {
	order := make([]string, 0, len(specs))
	byKey := make(map[string]*ParamSpec, len(specs))

	for _, s := range specs {
		k := strings.ToLower(s.Key)
		existing, ok := byKey[k]
		if !ok {
			cp := s
			byKey[k] = &cp
			order = append(order, k)
			continue
		}
		existing.Values = append(existing.Values, s.Values...)
	}

	out := make([]ParamSpec, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// resolveParamSpecs turns name-based --param constraints into the id-based
// payload DigiKey expects, using the facets discovered for the same query.
//
// It returns the parametric filters plus the category they belong to, because
// DigiKey only honors parametric filters alongside a CategoryFilter.
func resolveParamSpecs(specs []ParamSpec, result FiltersResult) ([]digikey.ParametricCategory, int, error) {
	out := make([]digikey.ParametricCategory, 0, len(specs))
	categoryID := 0

	for _, spec := range specs {
		facet, err := findParameterFacet(result.Parameters, spec.Key)
		if err != nil {
			return nil, 0, err
		}

		if facet.CategoryID != 0 {
			if categoryID != 0 && categoryID != facet.CategoryID {
				return nil, 0, usageErrorf(
					"parameters span more than one category (%q is in category %d); filter to one category first with --category",
					facet.ParameterName, facet.CategoryID)
			}
			categoryID = facet.CategoryID
		}

		values := make([]digikey.FilterID, 0, len(spec.Values))
		for _, want := range spec.Values {
			id, err := matchFacetValue(*facet, want)
			if err != nil {
				return nil, 0, err
			}
			values = append(values, digikey.FilterID{ID: id})
		}

		out = append(out, digikey.ParametricCategory{
			ParameterID:  facet.ParameterID,
			FilterValues: values,
		})
	}

	if categoryID == 0 && result.Category != nil {
		categoryID = result.Category.ID
	}
	return out, categoryID, nil
}

// resolveParams runs facet discovery for a query and resolves raw --param
// arguments against it, returning the parametric filters and their category.
func resolveParams(ctx context.Context, client *digikey.Client, keywords, category string, inStock bool, raw []string) ([]digikey.ParametricCategory, int, error) {
	specs := make([]ParamSpec, 0, len(raw))
	for _, arg := range raw {
		spec, err := parseParamSpec(arg)
		if err != nil {
			return nil, 0, err
		}
		specs = append(specs, spec)
	}
	specs = mergeParamSpecs(specs)

	facets, resp, err := discoverFacets(ctx, client, keywords, category, inStock)
	if err != nil {
		return nil, 0, err
	}
	result := buildFiltersResult(keywords, resp.ProductsCount, facets)

	if len(result.Parameters) == 0 {
		return nil, 0, usageErrorf(
			"DigiKey returned no parametric filters for %q, so --param has nothing to match against; "+
				"try narrower keywords or --category", keywords)
	}
	return resolveParamSpecs(specs, result)
}

// matchFacetValue resolves one value of a parametric filter. It accepts the
// display name (exact, then case-insensitive, then unique substring) or the
// raw ValueId, so a caller can round-trip ids straight out of `dk filters`.
func matchFacetValue(facet ParameterFacet, want string) (string, error) {
	want = strings.TrimSpace(want)

	var partial []ParameterFacetValue
	lower := strings.ToLower(want)

	for _, v := range facet.Values {
		if v.ValueName == want || v.ValueID == want {
			return v.ValueID, nil
		}
		if strings.EqualFold(v.ValueName, want) {
			return v.ValueID, nil
		}
		if strings.Contains(strings.ToLower(v.ValueName), lower) {
			partial = append(partial, v)
		}
	}

	switch len(partial) {
	case 1:
		return partial[0].ValueID, nil
	case 0:
		return "", usageErrorf("%s has no value matching %q (available: %s)",
			facet.ParameterName, want, sampleValueNames(facet, 10))
	default:
		names := make([]string, 0, len(partial))
		for i, v := range partial {
			if i == 6 {
				names = append(names, "...")
				break
			}
			names = append(names, v.ValueName)
		}
		return "", usageErrorf("%s value %q is ambiguous: %s", facet.ParameterName, want, strings.Join(names, ", "))
	}
}

// sampleValueNames lists a parameter's most common values, which is the most
// useful thing to show a caller that guessed wrong.
func sampleValueNames(facet ParameterFacet, max int) string {
	values := make([]ParameterFacetValue, len(facet.Values))
	copy(values, facet.Values)
	slices.SortStableFunc(values, func(a, b ParameterFacetValue) int { return b.ProductCount - a.ProductCount })

	names := make([]string, 0, max)
	for i, v := range values {
		if i == max {
			names = append(names, fmt.Sprintf("... and %d more, see `dk filters --parameter %q`",
				len(values)-max, facet.ParameterName))
			break
		}
		names = append(names, v.ValueName)
	}
	if len(names) == 0 {
		return "none in this result set"
	}
	return strings.Join(names, ", ")
}
