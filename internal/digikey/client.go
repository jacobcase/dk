// Package digikey is a typed client for the DigiKey Product Information v4 and
// MyLists v1 APIs.
//
// The two APIs differ in their auth requirements: Product Information accepts a
// client-credentials token, while MyLists needs a token obtained through the
// authorization-code flow because lists belong to a user account. Callers
// express that via the TokenSource passed to New.
package digikey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TokenSource supplies bearer tokens. requireUser selects the 3-legged token.
//
// user reports which grant the returned token actually came from, which the
// caller cannot infer from requireUser: a cached 3-legged token is preferred
// everywhere, so it may well answer a request that did not require one. The
// response cache needs to know, because a 3-legged token returns
// account-specific pricing.
type TokenSource interface {
	Token(ctx context.Context, requireUser bool) (token string, user bool, err error)
}

// ResponseCache stores successful responses to idempotent reads, so that
// repeating one does not spend another request against DigiKey's quota.
//
// Get never reports an error: an unreadable entry has to read as a miss, since
// the fallback — asking DigiKey — is exactly what the caller wanted anyway.
type ResponseCache interface {
	Get(scope, key string) ([]byte, bool)
	Put(scope, key string, body []byte) error
	Invalidate(scope string) error
}

// Cache scopes. They are separate so that writing to a list invalidates only
// the list reads: a `dk list add` that dropped the catalog responses too would
// make the cache close to useless in the one workflow it exists to serve —
// search, add, search again.
const (
	ScopeProduct = "product"
	ScopeLists   = "lists"
)

// Locale maps onto the X-DIGIKEY-Locale-* headers that select site, language,
// and pricing currency.
type Locale struct {
	Site     string
	Language string
	Currency string
}

// Options configures a Client.
type Options struct {
	BaseURL   string
	ClientID  string
	AccountID string
	Locale    Locale
	Tokens    TokenSource
	// HTTPClient defaults to a client with a 30 second timeout.
	HTTPClient *http.Client
	UserAgent  string
	// Cache, when non-nil, serves and stores responses to idempotent reads.
	Cache ResponseCache
	// CacheRefresh ignores stored entries while still storing fresh ones, which
	// is what HTTP's own Cache-Control: no-cache means and what dk's --no-cache
	// flag promises: the next read is answered by DigiKey, and the entry it
	// replaces is the stale one that prompted the flag.
	CacheRefresh bool
}

// Client talks to the DigiKey REST APIs.
type Client struct {
	baseURL   string
	clientID  string
	accountID string
	locale    Locale
	tokens    TokenSource
	httpc     *http.Client
	userAgent string

	cache        ResponseCache
	cacheRefresh bool
}

// New returns a Client. It returns an error only for configuration that would
// make every request fail.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("digikey: base url is required")
	}
	if opts.ClientID == "" {
		return nil, errors.New("digikey: client id is required")
	}
	if opts.Tokens == nil {
		return nil, errors.New("digikey: token source is required")
	}

	httpc := opts.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "dk-cli"
	}

	return &Client{
		baseURL:      strings.TrimSuffix(opts.BaseURL, "/"),
		clientID:     opts.ClientID,
		accountID:    opts.AccountID,
		locale:       opts.Locale,
		tokens:       opts.Tokens,
		httpc:        httpc,
		userAgent:    ua,
		cache:        opts.Cache,
		cacheRefresh: opts.CacheRefresh,
	}, nil
}

// request describes one API call.
type request struct {
	method string
	path   string
	query  url.Values
	body   any
	// requireUser marks endpoints that only accept a 3-legged token.
	requireUser bool
	// out, when non-nil, receives the decoded JSON response body.
	out any
	// cacheScope, when non-empty, marks this request as an idempotent read and
	// names the scope its response is stored under. Empty means never cached,
	// which is the default on purpose: a new endpoint has to be looked at and
	// declared safe to repeat, rather than inheriting caching by accident.
	cacheScope string
	// invalidates names a scope to drop after a successful response, for
	// requests that change what a read in that scope would return.
	invalidates string
}

// liveReadKey marks a context whose cached reads must go to DigiKey.
type liveReadKey struct{}

// Live returns a context whose cached reads are answered by DigiKey rather than
// from the store, while still replacing the entry they bypassed — the same
// semantics as --no-cache, scoped to one call.
//
// It exists for the read a write is about to act on. `dk list delete` decides
// whether to demand --force from a part count, and `dk list rm`/`dk list set`
// aim a write at a unique id they just read. A stale answer there is not an
// out-of-date figure on a screen; it is a delete pointed at the wrong thing.
func Live(ctx context.Context) context.Context {
	return context.WithValue(ctx, liveReadKey{}, true)
}

// isLiveRead reports whether ctx was marked by Live.
func isLiveRead(ctx context.Context) bool {
	v, _ := ctx.Value(liveReadKey{}).(bool)
	return v
}

// do executes a request, decoding a JSON body into req.out on success and
// returning an *APIError on any non-2xx response.
//
// A request marked with a cacheScope is answered from the response cache when a
// fresh entry exists, which costs nothing against DigiKey's quota. Only
// successful responses are stored, and only after the body decodes: a cached
// 401, 429, or undecodable 200 would outlive the condition that produced it and
// turn a transient failure into a sticky one.
func (c *Client) do(ctx context.Context, req request) error {
	token, userGrant, err := c.tokens.Token(ctx, req.requireUser)
	if err != nil {
		return err
	}

	endpoint := c.baseURL + req.path
	if len(req.query) > 0 {
		endpoint += "?" + req.query.Encode()
	}

	var payload []byte
	if req.body != nil {
		payload, err = json.Marshal(req.body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
	}

	var cacheKey string
	if c.cache != nil && req.cacheScope != "" {
		cacheKey = c.cacheKey(userGrant, req, endpoint, payload)
		if !c.cacheRefresh && !isLiveRead(ctx) {
			if cached, ok := c.cache.Get(req.cacheScope, cacheKey); ok {
				return decodeBody(req, cached)
			}
		}
	}

	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("X-DIGIKEY-Client-Id", c.clientID)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.userAgent)
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if c.accountID != "" {
		// MyLists v1 names this header Account-Id; Product Information v4 uses
		// Customer-Id for the same purpose. Sending both is harmless and lets
		// one config value serve both APIs.
		httpReq.Header.Set("X-DIGIKEY-Account-Id", c.accountID)
		httpReq.Header.Set("X-DIGIKEY-Customer-Id", c.accountID)
	}
	if c.locale.Site != "" {
		httpReq.Header.Set("X-DIGIKEY-Locale-Site", c.locale.Site)
	}
	if c.locale.Language != "" {
		httpReq.Header.Set("X-DIGIKEY-Locale-Language", c.locale.Language)
	}
	if c.locale.Currency != "" {
		httpReq.Header.Set("X-DIGIKEY-Locale-Currency", c.locale.Currency)
	}

	if c.cache != nil && req.invalidates != "" {
		// Dropped on the attempt, not on the reply. A write whose response is
		// lost to a timeout or a connection reset may well have landed at
		// DigiKey, and a scope left intact after it did is the one cache
		// failure that answers with what the write already changed. Dropping a
		// scope for a write that never arrived costs a refetch; keeping it for
		// one that did costs `dk list show` its promise to reflect the `dk list
		// add` that just ran.
		//
		// The failure is still ignored, with one caveat worth stating: it is
		// the only cache failure that produces a wrong answer rather than a
		// slow one. It takes a cache directory that accepted writes and then
		// stopped accepting deletes, and `dk cache clear` is the way out.
		defer func() { _ = c.cache.Invalidate(req.invalidates) }()
	}

	resp, err := c.httpc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("digikey request: %w", err)
	}
	defer resp.Body.Close()

	// Cap the read so a malformed or hostile response cannot exhaust memory.
	// Full product search pages run well under this.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp, body)
	}

	// Decoded before it is stored. A 2xx carrying something that is not the
	// expected JSON — a proxy interstitial, a WAF challenge — is a failed
	// response wearing a successful status, and storing it makes a one-off into
	// a sticky failure exactly as a cached 401 would: every identical request
	// for the rest of the TTL replays the decode error from disk without ever
	// reaching DigiKey.
	if err := decodeBody(req, body); err != nil {
		return err
	}

	if c.cache != nil && cacheKey != "" {
		// A cache that cannot be written is not a reason to fail a request that
		// already succeeded. The cost is one more API call next time.
		_ = c.cache.Put(req.cacheScope, cacheKey, body)
	}

	return nil
}

// decodeBody fills req.out from a response body, whether it came from DigiKey
// or from the cache. Both paths share it so a cached response cannot decode
// into anything different from the live one that produced it.
func decodeBody(req request, body []byte) error {
	if req.out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, req.out); err != nil {
		return fmt.Errorf("decode response from %s: %w", req.path, err)
	}
	return nil
}

// cacheKey identifies one response. Everything that can change what DigiKey
// returns for the same method and path belongs in here, because a key that
// omits one of them serves the wrong document rather than a stale one.
func (c *Client) cacheKey(userGrant bool, req request, endpoint string, payload []byte) string {
	// The grant, not the token. A 3-legged token returns account-specific
	// pricing, so the same query answered under the two grants is two different
	// documents and they must not share an entry.
	//
	// Keying on the token itself also worked, and cost far too much: DigiKey's
	// client_credentials token lives 600 seconds against a 10m default TTL, so
	// the whole namespace rotated about as fast as entries expired and a
	// search-only caller got almost nothing out of the cache. Fingerprinting
	// the refresh token instead would fare no better — DigiKey rotates that on
	// every grant too (see auth.Manager.UserToken).
	//
	// What the token fingerprint separated incidentally was one login's entries
	// from the next. `dk auth login` and `dk auth logout` clear the cache for
	// that, which is exact rather than a side effect.
	grant := "app"
	if userGrant {
		grant = "user"
	}

	// The environment needs no element of its own: endpoint carries the base
	// URL, so sandbox and production keys already differ.
	//
	// The leading version tag exists so that changing what goes into a key can
	// never quietly match entries built under the old rules.
	return strings.Join([]string{
		"v2",
		grant,
		c.clientID,
		c.accountID,
		c.locale.Site + "/" + c.locale.Language + "/" + c.locale.Currency,
		req.method,
		endpoint,
		string(payload),
	}, "\n")
}

// APIError is a non-2xx response from a DigiKey API.
type APIError struct {
	StatusCode int
	// Endpoint is the request path, included so errors are traceable without
	// enabling verbose logging.
	Endpoint string
	// RequestID is DigiKey's correlation id; quote it when opening a support ticket.
	RequestID        string
	ErrorMessage     string
	ErrorDetails     string
	ValidationErrors []ValidationError
	// RetryAfter is populated from the Retry-After header on 429 responses.
	RetryAfter time.Duration
	rawBody    string
}

// ValidationError describes one invalid input field.
type ValidationError struct {
	Field   string `json:"Field"`
	Message string `json:"Message"`
}

// apiErrorResponse is MyLists v1's error envelope (ApiErrorResponse).
type apiErrorResponse struct {
	StatusCode       int               `json:"StatusCode"`
	ErrorMessage     string            `json:"ErrorMessage"`
	ErrorDetails     string            `json:"ErrorDetails"`
	RequestID        string            `json:"RequestId"`
	ValidationErrors []ValidationError `json:"ValidationErrors"`
}

// problemDetails is Product Information v4's error envelope (DKProblemDetails),
// an RFC 7807 problem document. The two APIs do not share a shape: this one is
// lowercase and names its fields differently, so decoding a product error into
// apiErrorResponse succeeds with every field zero and loses the message, the
// correlation id, and the per-field validation errors. Both are tried.
type problemDetails struct {
	Title         string              `json:"title"`
	Detail        string              `json:"detail"`
	Status        int                 `json:"status"`
	Instance      string              `json:"instance"`
	CorrelationID string              `json:"correlationId"`
	Errors        map[string][]string `json:"errors"`
}

func parseAPIError(resp *http.Response, body []byte) *APIError {
	e := &APIError{
		StatusCode: resp.StatusCode,
		Endpoint:   resp.Request.URL.Path,
		rawBody:    strings.TrimSpace(string(body)),
	}

	var payload apiErrorResponse
	if err := json.Unmarshal(body, &payload); err == nil {
		e.ErrorMessage = payload.ErrorMessage
		e.ErrorDetails = payload.ErrorDetails
		e.RequestID = payload.RequestID
		e.ValidationErrors = payload.ValidationErrors
	}

	// A body that carried nothing under the MyLists names may still be a
	// problem document. Only fill what is still empty, so a MyLists reply that
	// happens to include a lowercase key keeps its own values.
	if e.ErrorMessage == "" && e.ErrorDetails == "" && e.RequestID == "" && len(e.ValidationErrors) == 0 {
		var pd problemDetails
		if err := json.Unmarshal(body, &pd); err == nil {
			e.ErrorMessage = pd.Title
			e.ErrorDetails = pd.Detail
			e.RequestID = pd.CorrelationID
			// errors maps a field name to its messages; flatten so the JSON
			// output shape stays the same for callers regardless of which API
			// failed.
			for _, field := range slices.Sorted(maps.Keys(pd.Errors)) {
				for _, msg := range pd.Errors[field] {
					e.ValidationErrors = append(e.ValidationErrors, ValidationError{
						Field:   field,
						Message: msg,
					})
				}
			}
		}
	}

	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "digikey %s: %d %s", e.Endpoint, e.StatusCode, http.StatusText(e.StatusCode))

	if msg := e.Message(); msg != "" {
		b.WriteString(": ")
		b.WriteString(msg)
	}
	for _, ve := range e.ValidationErrors {
		fmt.Fprintf(&b, "\n  - %s: %s", ve.Field, ve.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, "\n  request id: %s", e.RequestID)
	}
	return b.String()
}

// Message returns the most specific description DigiKey supplied.
func (e *APIError) Message() string {
	switch {
	case e.ErrorDetails != "" && e.ErrorMessage != "" && e.ErrorDetails != e.ErrorMessage:
		return e.ErrorMessage + " (" + e.ErrorDetails + ")"
	case e.ErrorMessage != "":
		return e.ErrorMessage
	case e.ErrorDetails != "":
		return e.ErrorDetails
	case e.rawBody != "":
		return truncate(e.rawBody, 300)
	default:
		return ""
	}
}

// truncate bounds an error body so a DigiKey HTML error page does not become
// the whole terminal. It counts runes, since a byte slice can land mid-rune and
// produce mojibake in the one place a user is already confused.
//
// Deliberately not shared with the identical helper in package auth: making the
// API client depend on another package for six lines would be a worse trade
// than the duplication.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// NotFound reports whether the error is a 404.
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// Unauthorized reports whether the error is a 401 or 403.
func (e *APIError) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// RateLimited reports whether the error is a 429.
func (e *APIError) RateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }
