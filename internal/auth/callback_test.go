package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseAuthorizationCode(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCode  string
		wantState string
		wantErr   string
	}{
		{
			name:      "full redirect url",
			input:     "https://localhost:8139/digikey_callback?code=abc123&state=xyz",
			wantCode:  "abc123",
			wantState: "xyz",
		},
		{
			name:     "bare code",
			input:    "abc123",
			wantCode: "abc123",
		},
		{
			name:     "bare code with surrounding whitespace",
			input:    "  abc123  \n",
			wantCode: "abc123",
		},
		{
			name:     "query string only",
			input:    "?code=abc123",
			wantCode: "abc123",
		},
		{
			name:     "url with fragment appended by the browser",
			input:    "https://localhost:8139/cb?code=abc123#_=_",
			wantCode: "abc123",
		},
		{
			name:      "url encoded code is decoded",
			input:     "https://localhost/cb?code=a%2Bb%2Fc&state=s",
			wantCode:  "a+b/c",
			wantState: "s",
		},
		{
			name:    "empty input",
			input:   "   ",
			wantErr: "no authorization code",
		},
		{
			name:    "url without a code",
			input:   "https://localhost:8139/cb?state=xyz",
			wantErr: "no ?code=",
		},
		{
			name:    "denial is reported with the reason",
			input:   "https://localhost/cb?error=access_denied&error_description=user+said+no",
			wantErr: "access_denied: user said no",
		},
		{
			name:    "denial without a description",
			input:   "https://localhost/cb?error=access_denied",
			wantErr: "access_denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAuthorizationCode(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseAuthorizationCode(%q) error = nil, want an error containing %q", tt.input, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAuthorizationCode(%q) error = %v", tt.input, err)
			}
			if got.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tt.wantCode)
			}
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
		})
	}
}

func TestCallbackServerRejectsNonHTTPSRedirect(t *testing.T) {
	s := &CallbackServer{RedirectURI: "http://localhost:8139/cb"}
	err := s.Start()
	if err == nil {
		s.Close()
		t.Fatal("Start() error = nil, want a rejection: DigiKey requires https redirect URIs")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error = %q, want it to explain the https requirement", err)
	}
}

func TestCallbackServerRejectsNonLoopbackHost(t *testing.T) {
	s := &CallbackServer{RedirectURI: "https://example.com/cb"}
	err := s.Start()
	if err == nil {
		s.Close()
		t.Fatal("Start() error = nil, want a rejection for a non-loopback host")
	}
	if !strings.Contains(err.Error(), "--manual") {
		t.Errorf("error = %q, want it to point at the --manual fallback", err)
	}
}

// freePort reserves and releases a port so the callback server can bind it.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// insecureClient trusts the callback server's self-signed certificate, exactly
// as a human clicking through the browser warning would.
func insecureClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed loopback cert under test
		},
	}
}

func TestCallbackServerHappyPath(t *testing.T) {
	port := freePort(t)
	redirect := "https://localhost:" + port + "/digikey_callback"

	s := &CallbackServer{RedirectURI: redirect, State: "expected-state"}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	go func() {
		resp, err := insecureClient().Get(redirect + "?code=the-code&state=expected-state")
		if err == nil {
			resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := s.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got.Code != "the-code" {
		t.Errorf("Code = %q, want %q", got.Code, "the-code")
	}
	if got.State != "expected-state" {
		t.Errorf("State = %q, want %q", got.State, "expected-state")
	}
}

func TestCallbackServerRejectsStateMismatch(t *testing.T) {
	port := freePort(t)
	redirect := "https://localhost:" + port + "/digikey_callback"

	s := &CallbackServer{RedirectURI: redirect, State: "expected-state"}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	go func() {
		resp, err := insecureClient().Get(redirect + "?code=the-code&state=attacker-state")
		if err == nil {
			resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A forged state must not yield a usable code.
	if _, err := s.Wait(ctx); err == nil {
		t.Fatal("Wait() error = nil, want a state mismatch rejection")
	} else if !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("error = %q, want it to mention a state mismatch", err)
	}
}

func TestCallbackServerReportsAuthorizationDenial(t *testing.T) {
	port := freePort(t)
	redirect := "https://localhost:" + port + "/digikey_callback"

	s := &CallbackServer{RedirectURI: redirect, State: "s"}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	go func() {
		resp, err := insecureClient().Get(redirect + "?error=access_denied&error_description=nope")
		if err == nil {
			resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := s.Wait(ctx)
	if err == nil {
		t.Fatal("Wait() error = nil, want the denial reported")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error = %q, want it to include DigiKey's error code", err)
	}
}

func TestCallbackServerWaitRespectsContext(t *testing.T) {
	port := freePort(t)
	s := &CallbackServer{RedirectURI: "https://127.0.0.1:" + port + "/cb"}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := s.Wait(ctx); err == nil {
		t.Fatal("Wait() error = nil, want the context deadline to surface")
	}
}

func TestCallbackServerIgnoresOtherPaths(t *testing.T) {
	port := freePort(t)
	base := "https://localhost:" + port
	s := &CallbackServer{RedirectURI: base + "/digikey_callback", State: "s"}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	// A stray request (a favicon fetch, a probe) must not complete the flow.
	resp, err := insecureClient().Get(base + "/favicon.ico?code=sneaky")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status for an unrelated path = %d, want 404", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if got, err := s.Wait(ctx); err == nil {
		t.Errorf("Wait() returned %+v from an unrelated path, want it to keep waiting", got)
	}
}

func TestSelfSignedCertCoversLoopback(t *testing.T) {
	cert, err := selfSignedCert("localhost")
	if err != nil {
		t.Fatalf("selfSignedCert() error = %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("selfSignedCert() produced no certificate")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("certificate does not cover localhost: %v", err)
	}
	// Browsers may resolve localhost to either loopback address.
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("certificate does not cover 127.0.0.1: %v", err)
	}
	if !leaf.NotAfter.After(time.Now()) {
		t.Error("certificate is already expired")
	}
	if time.Until(leaf.NotAfter) > time.Hour {
		t.Error("certificate lifetime is longer than the login flow needs")
	}
}

func TestHTMLEscape(t *testing.T) {
	got := htmlEscape(`<script>alert("x")</script>`)
	if strings.Contains(got, "<script>") {
		t.Errorf("htmlEscape() = %q, want the markup neutralized", got)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{"127.0.0.1", true},
		{"127.0.0.53", true},
		{"::1", true},
		{"example.com", false},
		{"10.0.0.1", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isLoopbackHost(tt.host); got != tt.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
