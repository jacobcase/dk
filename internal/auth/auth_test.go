package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testEnv = "production"

// newTestStore returns a Store backed by a temp file.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "token.json"))
}

// tokenServer stands in for DigiKey's /v1/oauth2/token endpoint. It records the
// form values of each request so tests can assert on the grant type.
type tokenServer struct {
	*httptest.Server
	calls atomic.Int32
	forms []url.Values
}

func newTokenServer(t *testing.T, handler func(ts *tokenServer, form url.Values) (int, string)) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath {
			t.Errorf("token request path = %q, want %q", r.URL.Path, tokenPath)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		ts.calls.Add(1)
		ts.forms = append(ts.forms, r.PostForm)

		status, body := handler(ts, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// okTokenBody returns a successful token response.
func okTokenBody(access, refresh string, expiresIn int) string {
	b, _ := json.Marshal(map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"expires_in":    expiresIn,
		"token_type":    "Bearer",
	})
	return string(b)
}

func TestTokenValid(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		token *Token
		skew  time.Duration
		want  bool
	}{
		{"nil token", nil, 0, false},
		{"empty access token", &Token{ExpiresAt: now.Add(time.Hour)}, 0, false},
		{"valid", &Token{AccessToken: "a", ExpiresAt: now.Add(time.Hour)}, 0, true},
		{"expired", &Token{AccessToken: "a", ExpiresAt: now.Add(-time.Second)}, 0, false},
		{"within skew counts as expired", &Token{AccessToken: "a", ExpiresAt: now.Add(30 * time.Second)}, time.Minute, false},
		{"outside skew still valid", &Token{AccessToken: "a", ExpiresAt: now.Add(2 * time.Minute)}, time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.token.Valid(now, tt.skew); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStoreRoundTrip(t *testing.T) {
	store := newTestStore(t)

	if got, err := store.Get(KindUser, testEnv); err != nil || got != nil {
		t.Fatalf("Get on empty store = (%v, %v), want (nil, nil)", got, err)
	}

	want := &Token{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	if err := store.Put(KindUser, testEnv, want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// A second Store over the same path proves the token actually reached disk.
	reopened := NewStore(store.Path())
	got, err := reopened.Get(KindUser, testEnv)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil || got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
}

func TestStoreSeparatesKindsAndEnvironments(t *testing.T) {
	store := newTestStore(t)

	if err := store.Put(KindApp, "production", &Token{AccessToken: "prod-app"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(KindUser, "production", &Token{AccessToken: "prod-user"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(KindApp, "sandbox", &Token{AccessToken: "sandbox-app"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		kind Kind
		env  string
		want string
	}{
		{KindApp, "production", "prod-app"},
		{KindUser, "production", "prod-user"},
		{KindApp, "sandbox", "sandbox-app"},
	}
	for _, c := range cases {
		got, err := store.Get(c.kind, c.env)
		if err != nil {
			t.Fatalf("Get(%s, %s) error = %v", c.kind, c.env, err)
		}
		if got == nil || got.AccessToken != c.want {
			t.Errorf("Get(%s, %s) = %+v, want access token %q", c.kind, c.env, got, c.want)
		}
	}

	// A sandbox user token was never stored and must not fall back to production.
	if got, _ := store.Get(KindUser, "sandbox"); got != nil {
		t.Errorf("Get(user, sandbox) = %+v, want nil", got)
	}
}

func TestStoreGetReturnsCopy(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{AccessToken: "original"}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(KindUser, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	got.AccessToken = "mutated"

	again, err := store.Get(KindUser, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if again.AccessToken != "original" {
		t.Errorf("cached token was mutated through the returned pointer: got %q", again.AccessToken)
	}
}

func TestStoreDeleteAndClear(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{AccessToken: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(KindApp, testEnv, &Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(KindUser, testEnv); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got, _ := store.Get(KindUser, testEnv); got != nil {
		t.Errorf("user token = %+v after Delete, want nil", got)
	}
	if got, _ := store.Get(KindApp, testEnv); got == nil {
		t.Error("Delete(user) also removed the app token")
	}

	// Deleting again must not error.
	if err := store.Delete(KindUser, testEnv); err != nil {
		t.Errorf("second Delete() error = %v, want nil", err)
	}

	if err := store.Clear(); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if got, _ := store.Get(KindApp, testEnv); got != nil {
		t.Errorf("app token = %+v after Clear, want nil", got)
	}
}

func TestStoreFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}
	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %#o, want 0600: tokens must not be world readable", perm)
	}
}

func TestAppTokenCachesUntilExpiry(t *testing.T) {
	ts := newTokenServer(t, func(_ *tokenServer, form url.Values) (int, string) {
		if got := form.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", got)
		}
		return http.StatusOK, okTokenBody("app-token", "", 1800)
	})

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "secret",
		Environment: testEnv, Store: newTestStore(t), HTTPClient: ts.Client(),
		Now: func() time.Time { return now },
	}

	for i := range 3 {
		tok, err := m.AppToken(context.Background())
		if err != nil {
			t.Fatalf("AppToken() call %d error = %v", i, err)
		}
		if tok != "app-token" {
			t.Errorf("AppToken() = %q, want %q", tok, "app-token")
		}
	}
	if got := ts.calls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1: the token should be cached", got)
	}

	// Past expiry, a new token must be fetched.
	now = now.Add(31 * time.Minute)
	if _, err := m.AppToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ts.calls.Load(); got != 2 {
		t.Errorf("token endpoint called %d times after expiry, want 2", got)
	}
}

func TestAppTokenDefaultsExpiryWhenOmitted(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		// No expires_in field at all.
		return http.StatusOK, `{"access_token":"a","token_type":"Bearer"}`
	})

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t)
	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(),
		Now: func() time.Time { return now },
	}
	if _, err := m.AppToken(context.Background()); err != nil {
		t.Fatal(err)
	}

	tok, err := store.Get(KindApp, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	// A missing expires_in must not be read as "already expired".
	if !tok.ExpiresAt.After(now) {
		t.Errorf("ExpiresAt = %v, want a time after %v", tok.ExpiresAt, now)
	}
}

func TestUserTokenRequiresLogin(t *testing.T) {
	m := &Manager{
		BaseURL: "https://example.invalid", ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: newTestStore(t),
	}
	_, err := m.UserToken(context.Background())
	if !errors.Is(err, ErrLoginRequired) {
		t.Errorf("UserToken() on an empty store error = %v, want ErrLoginRequired", err)
	}
}

func TestUserTokenRefreshesAndRotates(t *testing.T) {
	ts := newTokenServer(t, func(_ *tokenServer, form url.Values) (int, string) {
		if got := form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", got)
		}
		if got := form.Get("refresh_token"); got != "old-refresh" {
			t.Errorf("refresh_token = %q, want %q", got, "old-refresh")
		}
		return http.StatusOK, okTokenBody("new-access", "new-refresh", 1800)
	})

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{
		AccessToken:  "stale-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(),
		Now: func() time.Time { return now },
	}

	got, err := m.UserToken(context.Background())
	if err != nil {
		t.Fatalf("UserToken() error = %v", err)
	}
	if got != "new-access" {
		t.Errorf("UserToken() = %q, want %q", got, "new-access")
	}

	// DigiKey rotates the refresh token; storing the new one is what keeps the
	// non-expiring session alive.
	stored, err := store.Get(KindUser, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "new-refresh" {
		t.Errorf("stored refresh token = %q, want the rotated %q", stored.RefreshToken, "new-refresh")
	}
}

func TestUserTokenKeepsRefreshTokenWhenResponseOmitsIt(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		return http.StatusOK, okTokenBody("new-access", "", 1800)
	})

	now := time.Now()
	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{
		AccessToken: "stale", RefreshToken: "keep-me", ExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(),
	}
	if _, err := m.UserToken(context.Background()); err != nil {
		t.Fatal(err)
	}

	stored, _ := store.Get(KindUser, testEnv)
	if stored.RefreshToken != "keep-me" {
		t.Errorf("refresh token = %q, want the previous %q carried forward", stored.RefreshToken, "keep-me")
	}
}

func TestUserTokenRejectedRefreshRequiresLogin(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		return http.StatusBadRequest, `{"error":"invalid_grant","error_description":"refresh token revoked"}`
	})

	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{
		AccessToken: "stale", RefreshToken: "revoked", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(),
	}

	_, err := m.UserToken(context.Background())
	// A 4xx on refresh is unrecoverable without a new browser login, so it must
	// surface as ErrLoginRequired rather than a generic API error.
	if !errors.Is(err, ErrLoginRequired) {
		t.Errorf("UserToken() error = %v, want ErrLoginRequired", err)
	}
	if !strings.Contains(err.Error(), "refresh token revoked") {
		t.Errorf("error %q should include DigiKey's explanation", err)
	}

	// The dead token has to be dropped. Left cached, `dk auth status` keeps
	// reporting user_logged_in while every list command exits 3 — and an agent,
	// which cannot run `dk auth login`, has no way to reconcile the two.
	tok, err := NewStore(store.Path()).Get(KindUser, testEnv)
	if err != nil {
		t.Fatalf("reading the store after a rejected refresh: %v", err)
	}
	if tok != nil && (tok.AccessToken != "" || tok.RefreshToken != "") {
		t.Errorf("revoked token survived in the cache: %+v", tok)
	}
}

// A transient 5xx must NOT discard a refresh token that is probably still good;
// doing so would turn a blip into a mandatory browser login.
func TestUserTokenServerErrorKeepsRefreshToken(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		return http.StatusInternalServerError, `{"error":"server_error"}`
	})

	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{
		AccessToken: "stale", RefreshToken: "good", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(),
	}
	if _, err := m.UserToken(context.Background()); err == nil {
		t.Fatal("UserToken() error = nil, want a server error")
	}

	tok, err := NewStore(store.Path()).Get(KindUser, testEnv)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	if tok == nil || tok.RefreshToken != "good" {
		t.Errorf("refresh token was discarded on a transient 5xx: %+v", tok)
	}
}

func TestUserTokenServerErrorIsNotLoginRequired(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		return http.StatusInternalServerError, `{"error":"server_error"}`
	})

	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{
		AccessToken: "stale", RefreshToken: "good", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(),
	}

	_, err := m.UserToken(context.Background())
	// A 5xx is transient; forcing a re-login would be wrong.
	if errors.Is(err, ErrLoginRequired) {
		t.Errorf("UserToken() error = %v, want a transient error rather than ErrLoginRequired", err)
	}
	if err == nil {
		t.Fatal("UserToken() error = nil, want a server error")
	}
}

func TestTokenPrefersUserWhenAvailable(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		return http.StatusOK, okTokenBody("app-token", "", 1800)
	})

	store := newTestStore(t)
	if err := store.Put(KindUser, testEnv, &Token{
		AccessToken: "user-token", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(), PreferUser: true,
	}

	got, err := m.Token(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	// The user token yields account-specific pricing, so it wins even where an
	// app token would do.
	if got != "user-token" {
		t.Errorf("Token(requireUser=false) = %q, want the cached user token", got)
	}
	if ts.calls.Load() != 0 {
		t.Error("token endpoint was called even though a valid user token was cached")
	}
}

func TestTokenFallsBackToAppWhenNotLoggedIn(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		return http.StatusOK, okTokenBody("app-token", "", 1800)
	})

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: newTestStore(t), HTTPClient: ts.Client(), PreferUser: true,
	}

	got, err := m.Token(context.Background(), false)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if got != "app-token" {
		t.Errorf("Token() = %q, want the client-credentials token", got)
	}
}

func TestTokenRequireUserDoesNotFallBack(t *testing.T) {
	ts := newTokenServer(t, func(*tokenServer, url.Values) (int, string) {
		return http.StatusOK, okTokenBody("app-token", "", 1800)
	})

	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: newTestStore(t), HTTPClient: ts.Client(), PreferUser: true,
	}

	// MyLists would reject an app token, so requireUser must fail loudly rather
	// than silently sending one.
	if _, err := m.Token(context.Background(), true); !errors.Is(err, ErrLoginRequired) {
		t.Errorf("Token(requireUser=true) error = %v, want ErrLoginRequired", err)
	}
	if ts.calls.Load() != 0 {
		t.Error("an app token was requested for a user-only operation")
	}
}

func TestRequestTokenRequiresCredentials(t *testing.T) {
	m := &Manager{BaseURL: "https://example.invalid", Environment: testEnv, Store: newTestStore(t)}
	if _, err := m.AppToken(context.Background()); err == nil {
		t.Error("AppToken() error = nil, want an error when the client id and secret are unset")
	}
}

func TestExchangeStoresToken(t *testing.T) {
	ts := newTokenServer(t, func(_ *tokenServer, form url.Values) (int, string) {
		if got := form.Get("grant_type"); got != "authorization_code" {
			t.Errorf("grant_type = %q, want authorization_code", got)
		}
		if got := form.Get("code"); got != "the-code" {
			t.Errorf("code = %q, want %q", got, "the-code")
		}
		if got := form.Get("redirect_uri"); got != "https://localhost:8139/cb" {
			t.Errorf("redirect_uri = %q, want it echoed back exactly", got)
		}
		return http.StatusOK, okTokenBody("acc", "ref", 1800)
	})

	store := newTestStore(t)
	m := &Manager{
		BaseURL: ts.URL, ClientID: "id", ClientSecret: "s",
		Environment: testEnv, Store: store, HTTPClient: ts.Client(),
	}

	tok, err := m.Exchange(context.Background(), "the-code", "https://localhost:8139/cb")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if tok.AccessToken != "acc" || tok.RefreshToken != "ref" {
		t.Errorf("Exchange() = %+v, want access acc / refresh ref", tok)
	}

	stored, _ := store.Get(KindUser, testEnv)
	if stored == nil || stored.RefreshToken != "ref" {
		t.Errorf("stored token = %+v, want the refresh token persisted", stored)
	}
}

func TestAuthorizationURL(t *testing.T) {
	m := &Manager{BaseURL: "https://api.digikey.com", ClientID: "my-client"}

	raw, err := m.AuthorizationURL("https://localhost:8139/digikey_callback", "st4te")
	if err != nil {
		t.Fatalf("AuthorizationURL() error = %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("produced an unparseable url %q: %v", raw, err)
	}

	if u.Host != "api.digikey.com" {
		t.Errorf("host = %q, want api.digikey.com", u.Host)
	}
	if u.Path != authorizePath {
		t.Errorf("path = %q, want %q", u.Path, authorizePath)
	}
	q := u.Query()
	want := map[string]string{
		"response_type": "code",
		"client_id":     "my-client",
		"redirect_uri":  "https://localhost:8139/digikey_callback",
		"state":         "st4te",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("query %q = %q, want %q", k, got, v)
		}
	}
}

func TestAuthorizationURLOmitsEmptyState(t *testing.T) {
	m := &Manager{BaseURL: "https://api.digikey.com", ClientID: "c"}
	raw, err := m.AuthorizationURL("https://localhost/cb", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "state=") {
		t.Errorf("url %q contains an empty state parameter", raw)
	}
}

func TestNewStateIsRandomAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		s, err := NewState()
		if err != nil {
			t.Fatalf("NewState() error = %v", err)
		}
		if s == "" {
			t.Fatal("NewState() returned an empty string")
		}
		if seen[s] {
			t.Fatalf("NewState() repeated the value %q", s)
		}
		seen[s] = true
		if url.QueryEscape(s) != s {
			t.Errorf("NewState() = %q, which is not URL-safe as-is", s)
		}
	}
}

func TestErrorMessagePrefersMostSpecific(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{"description", &Error{Description: "desc", Code: "invalid_grant"}, "desc"},
		{"details", &Error{ErrorDetails: "details", ErrorMessage: "msg"}, "details"},
		{"message", &Error{ErrorMessage: "msg"}, "msg"},
		{"code", &Error{Code: "invalid_client"}, "invalid_client"},
		{"raw body", &Error{rawBody: "<html>gateway</html>"}, "<html>gateway</html>"},
		{"status text", &Error{StatusCode: http.StatusTeapot}, http.StatusText(http.StatusTeapot)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Message(); got != tt.want {
				t.Errorf("Message() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewErrorParsesBothPayloadShapes(t *testing.T) {
	t.Run("rfc6749", func(t *testing.T) {
		e := newError(400, []byte(`{"error":"invalid_grant","error_description":"code expired"}`))
		if e.Code != "invalid_grant" || e.Description != "code expired" {
			t.Errorf("parsed %+v, want the RFC 6749 fields", e)
		}
	})
	t.Run("digikey envelope", func(t *testing.T) {
		e := newError(401, []byte(`{"ErrorMessage":"Bearer token is invalid","RequestId":"abc-123"}`))
		if e.ErrorMessage != "Bearer token is invalid" || e.RequestID != "abc-123" {
			t.Errorf("parsed %+v, want the DigiKey envelope fields", e)
		}
	})
	t.Run("non json", func(t *testing.T) {
		e := newError(http.StatusBadGateway, []byte("  Bad Gateway  "))
		if e.rawBody != "Bad Gateway" {
			t.Errorf("rawBody = %q, want the trimmed body", e.rawBody)
		}
		if e.StatusCode != http.StatusBadGateway {
			t.Errorf("StatusCode = %d, want %d", e.StatusCode, http.StatusBadGateway)
		}
	})
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate() = %q, want the string unchanged", got)
	}
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("truncate() = %q, want %q", got, "hello...")
	}
}
