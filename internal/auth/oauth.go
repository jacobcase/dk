// Package auth implements DigiKey's two OAuth 2.0 flows and caches the
// resulting tokens on disk.
//
// DigiKey splits its APIs across two grants:
//
//   - client_credentials ("2-legged") is enough for the Product Information
//     API. No human is involved; tokens live 30 minutes and are simply
//     re-requested on expiry.
//   - authorization_code ("3-legged") is required by the MyLists API, because
//     lists belong to a DigiKey user account. A human logs in once via the
//     browser; the resulting refresh token does not expire, so later
//     non-interactive runs (including agent-driven ones) keep working.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Kind distinguishes the two token flavors dk caches.
type Kind string

const (
	// KindApp is a client-credentials (2-legged) token.
	KindApp Kind = "app"
	// KindUser is an authorization-code (3-legged) token tied to a DigiKey login.
	KindUser Kind = "user"
)

// OAuth endpoint paths, relative to the environment base URL.
const (
	authorizePath = "/v1/oauth2/authorize"
	tokenPath     = "/v1/oauth2/token"
)

// defaultSkew is the headroom subtracted from a token's expiry before it is
// considered stale. DigiKey access tokens live 30 minutes.
const defaultSkew = 60 * time.Second

// ErrLoginRequired is returned when an operation needs a 3-legged token and no
// usable one is cached. Commands map this to a dedicated exit code.
var ErrLoginRequired = errors.New("digikey user login required")

// Manager mints, caches, and refreshes DigiKey OAuth tokens.
type Manager struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	// Environment keys the token cache so sandbox and production tokens coexist.
	Environment string
	Store       *Store
	HTTPClient  *http.Client
	// PreferUser makes Token report a valid 3-legged token even when only an
	// app token is required. That yields account-specific pricing on product
	// endpoints at no extra cost.
	PreferUser bool

	// Now is overridable for tests; nil means time.Now.
	Now func() time.Time

	mu sync.Mutex
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) httpClient() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return http.DefaultClient
}

// Token returns a bearer token. When requireUser is true only a 3-legged token
// will do, and ErrLoginRequired is returned if none can be produced.
func (m *Manager) Token(ctx context.Context, requireUser bool) (string, error) {
	if requireUser {
		return m.UserToken(ctx)
	}
	if m.PreferUser {
		if tok, err := m.UserToken(ctx); err == nil {
			return tok, nil
		}
	}
	return m.AppToken(ctx)
}

// AppToken returns a cached or freshly minted client-credentials token.
func (m *Manager) AppToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cached, err := m.Store.Get(KindApp, m.Environment)
	if err != nil {
		return "", err
	}
	if cached.Valid(m.now(), defaultSkew) {
		return cached.AccessToken, nil
	}

	tok, err := m.requestToken(ctx, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {m.ClientID},
		"client_secret": {m.ClientSecret},
	})
	if err != nil {
		return "", err
	}
	if err := m.Store.Put(KindApp, m.Environment, tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// UserToken returns a cached 3-legged token, refreshing it when expired. It
// returns ErrLoginRequired if the user has never run `dk auth login` (or the
// refresh token has been revoked).
func (m *Manager) UserToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cached, err := m.Store.Get(KindUser, m.Environment)
	if err != nil {
		return "", err
	}
	if cached == nil || (cached.AccessToken == "" && cached.RefreshToken == "") {
		return "", ErrLoginRequired
	}
	if cached.Valid(m.now(), defaultSkew) {
		return cached.AccessToken, nil
	}
	if cached.RefreshToken == "" {
		return "", ErrLoginRequired
	}

	tok, err := m.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {m.ClientID},
		"client_secret": {m.ClientSecret},
		"refresh_token": {cached.RefreshToken},
	})
	if err != nil {
		// A rejected refresh token cannot be recovered from without a new login.
		var oerr *Error
		if errors.As(err, &oerr) && oerr.StatusCode >= 400 && oerr.StatusCode < 500 {
			return "", fmt.Errorf("%w: stored refresh token was rejected (%s)", ErrLoginRequired, oerr.Message())
		}
		return "", err
	}
	// DigiKey rotates the refresh token on every grant; carry the old one
	// forward only if the response omitted a replacement.
	if tok.RefreshToken == "" {
		tok.RefreshToken = cached.RefreshToken
	}
	if err := m.Store.Put(KindUser, m.Environment, tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// AuthorizationURL builds the URL a human opens to grant dk access to their
// DigiKey account.
func (m *Manager) AuthorizationURL(redirectURI, state string) (string, error) {
	u, err := url.Parse(m.BaseURL)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}
	u.Path = authorizePath
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {m.ClientID},
		"redirect_uri":  {redirectURI},
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange trades an authorization code for a token pair and caches it.
// DigiKey authorization codes expire one minute after issue.
func (m *Manager) Exchange(ctx context.Context, code, redirectURI string) (*Token, error) {
	tok, err := m.requestToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {m.ClientID},
		"client_secret": {m.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		return nil, err
	}
	if err := m.Store.Put(KindUser, m.Environment, tok); err != nil {
		return nil, err
	}
	return tok, nil
}

// tokenResponse mirrors DigiKey's OAuth token endpoint payload.
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"`
}

// requestToken POSTs to the token endpoint and decodes the result.
func (m *Manager) requestToken(ctx context.Context, form url.Values) (*Token, error) {
	if m.ClientID == "" || m.ClientSecret == "" {
		return nil, errors.New("client id and secret are required")
	}

	endpoint := strings.TrimSuffix(m.BaseURL, "/") + tokenPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	// Token error payloads are small; reading the whole body keeps the error
	// message useful when DigiKey returns HTML instead of JSON.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newError(resp.StatusCode, body)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token response contained no access_token")
	}

	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		// DigiKey documents a 30 minute access token lifetime; assume it if the
		// response omits expires_in rather than treating the token as expired.
		expiresIn = 1800
	}

	return &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    m.now().Add(time.Duration(expiresIn) * time.Second),
		Scope:        tr.Scope,
	}, nil
}

// Error is a non-2xx response from the OAuth token endpoint.
type Error struct {
	StatusCode   int
	Code         string `json:"error"`
	Description  string `json:"error_description"`
	ErrorMessage string `json:"ErrorMessage"`
	ErrorDetails string `json:"ErrorDetails"`
	RequestID    string `json:"RequestId"`
	rawBody      string
}

func newError(status int, body []byte) *Error {
	e := &Error{rawBody: strings.TrimSpace(string(body))}
	// Best effort: DigiKey returns RFC 6749 error payloads for some failures and
	// its own ApiErrorResponse shape for others. Both decode into Error.
	_ = json.Unmarshal(body, e)
	e.StatusCode = status
	return e
}

// Message returns the most specific human-readable description available.
func (e *Error) Message() string {
	switch {
	case e.Description != "":
		return e.Description
	case e.ErrorDetails != "":
		return e.ErrorDetails
	case e.ErrorMessage != "":
		return e.ErrorMessage
	case e.Code != "":
		return e.Code
	case e.rawBody != "":
		return truncate(e.rawBody, 300)
	default:
		return http.StatusText(e.StatusCode)
	}
}

func (e *Error) Error() string {
	return fmt.Sprintf("digikey oauth: %d %s: %s", e.StatusCode, http.StatusText(e.StatusCode), e.Message())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
