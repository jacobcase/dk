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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TokenSource supplies bearer tokens. requireUser selects the 3-legged token.
type TokenSource interface {
	Token(ctx context.Context, requireUser bool) (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(ctx context.Context, requireUser bool) (string, error)

// Token implements TokenSource.
func (f TokenSourceFunc) Token(ctx context.Context, requireUser bool) (string, error) {
	return f(ctx, requireUser)
}

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
		baseURL:   strings.TrimSuffix(opts.BaseURL, "/"),
		clientID:  opts.ClientID,
		accountID: opts.AccountID,
		locale:    opts.Locale,
		tokens:    opts.Tokens,
		httpc:     httpc,
		userAgent: ua,
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
}

// do executes a request, decoding a JSON body into req.out on success and
// returning an *APIError on any non-2xx response.
func (c *Client) do(ctx context.Context, req request) error {
	token, err := c.tokens.Token(ctx, req.requireUser)
	if err != nil {
		return err
	}

	endpoint := c.baseURL + req.path
	if len(req.query) > 0 {
		endpoint += "?" + req.query.Encode()
	}

	var bodyReader io.Reader
	if req.body != nil {
		payload, err := json.Marshal(req.body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
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

	if req.out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, req.out); err != nil {
		return fmt.Errorf("decode response from %s: %w", req.path, err)
	}
	return nil
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

// apiErrorResponse is DigiKey's shared error envelope.
type apiErrorResponse struct {
	StatusCode       int               `json:"StatusCode"`
	ErrorMessage     string            `json:"ErrorMessage"`
	ErrorDetails     string            `json:"ErrorDetails"`
	RequestID        string            `json:"RequestId"`
	ValidationErrors []ValidationError `json:"ValidationErrors"`
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
		if len(e.rawBody) > 300 {
			return e.rawBody[:300] + "..."
		}
		return e.rawBody
	default:
		return ""
	}
}

// NotFound reports whether the error is a 404.
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// Unauthorized reports whether the error is a 401 or 403.
func (e *APIError) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// RateLimited reports whether the error is a 429.
func (e *APIError) RateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }
