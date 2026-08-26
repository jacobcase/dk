package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CallbackResult is what a completed OAuth redirect yields.
type CallbackResult struct {
	Code  string
	State string
}

// NewState returns a random, URL-safe CSRF state value.
func NewState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// CallbackServer waits for DigiKey to redirect the browser back to the loopback
// interface. DigiKey requires redirect URIs to use TLS, so this serves HTTPS
// with a short-lived self-signed certificate; the browser will warn once and
// the user clicks through.
type CallbackServer struct {
	// RedirectURI must match the URI registered on developer.digikey.com
	// exactly. Its host determines the listen address and its path the route.
	RedirectURI string
	// State is the CSRF value expected back from the authorization server.
	State string

	listener net.Listener
	server   *http.Server
	results  chan CallbackResult
	failures chan error
}

// Start binds the listener and begins serving. The caller must call Close.
// Binding before printing the authorization URL guarantees the port is actually
// available before the user is sent to their browser.
func (c *CallbackServer) Start() error {
	u, err := url.Parse(c.RedirectURI)
	if err != nil {
		return fmt.Errorf("parse redirect uri %q: %w", c.RedirectURI, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("redirect uri must use https (DigiKey rejects plain http), got %q", c.RedirectURI)
	}

	host := u.Hostname()
	if !isLoopbackHost(host) {
		return fmt.Errorf("redirect uri host %q is not loopback; use --manual for a non-local redirect uri", host)
	}

	port := u.Port()
	if port == "" {
		port = "443"
	}

	cert, err := selfSignedCert(host)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("listen on %s: %w (is another dk auth login running?)", net.JoinHostPort(host, port), err)
	}

	c.results = make(chan CallbackResult, 1)
	c.failures = make(chan error, 1)

	path := u.Path
	if path == "" {
		path = "/"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		c.handle(w, r)
	})

	c.listener = tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	c.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := c.server.Serve(c.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case c.failures <- fmt.Errorf("callback server: %w", err):
			default:
			}
		}
	}()
	return nil
}

// handle processes the single redirect request the flow expects.
func (c *CallbackServer) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if errCode := q.Get("error"); errCode != "" {
		desc := q.Get("error_description")
		msg := errCode
		if desc != "" {
			msg = errCode + ": " + desc
		}
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed", msg)
		c.fail(fmt.Errorf("authorization denied: %s", msg))
		return
	}

	code := q.Get("code")
	if code == "" {
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed", "The redirect did not include an authorization code.")
		c.fail(errors.New("redirect contained no authorization code"))
		return
	}

	if got := q.Get("state"); c.State != "" && got != c.State {
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed", "State mismatch; the request may have been tampered with.")
		c.fail(errors.New("state mismatch in oauth redirect"))
		return
	}

	writeCallbackPage(w, http.StatusOK, "Authorized", "You can close this tab and return to your terminal.")

	select {
	case c.results <- CallbackResult{Code: code, State: q.Get("state")}:
	default:
	}
}

func (c *CallbackServer) fail(err error) {
	select {
	case c.failures <- err:
	default:
	}
}

// Wait blocks until the redirect arrives, the context is cancelled, or the
// server fails.
func (c *CallbackServer) Wait(ctx context.Context) (CallbackResult, error) {
	select {
	case res := <-c.results:
		return res, nil
	case err := <-c.failures:
		return CallbackResult{}, err
	case <-ctx.Done():
		return CallbackResult{}, ctx.Err()
	}
}

// Close shuts the listener down.
func (c *CallbackServer) Close() error {
	if c.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.server.Shutdown(ctx)
}

// Addr reports the address the callback server is listening on, which is useful
// when the redirect URI omits an explicit port.
func (c *CallbackServer) Addr() string {
	if c.listener == nil {
		return ""
	}
	return c.listener.Addr().String()
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// selfSignedCert mints a short-lived certificate for the loopback callback.
// It is never trusted by anything but the user's one-time click-through, and
// the private key never leaves memory.
func selfSignedCert(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"dk cli"}, CommonName: host},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(30 * time.Minute),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}

// ParseAuthorizationCode extracts the code from user-supplied input, accepting
// either a full redirect URL, a bare query string, or the code by itself. This
// backs `dk auth login --manual`, where the user pastes whatever their browser
// landed on.
func ParseAuthorizationCode(input string) (CallbackResult, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return CallbackResult{}, errors.New("no authorization code provided")
	}

	// A bare code has no URL or query syntax in it.
	if !strings.ContainsAny(s, "?&=/") {
		return CallbackResult{Code: s}, nil
	}

	raw := s
	if i := strings.Index(s, "?"); i >= 0 {
		raw = s[i+1:]
	}
	// Drop any fragment; DigiKey does not use one, but browsers may append it.
	if i := strings.Index(raw, "#"); i >= 0 {
		raw = raw[:i]
	}

	q, err := url.ParseQuery(raw)
	if err != nil {
		return CallbackResult{}, fmt.Errorf("parse redirect url: %w", err)
	}
	if errCode := q.Get("error"); errCode != "" {
		if desc := q.Get("error_description"); desc != "" {
			return CallbackResult{}, fmt.Errorf("authorization denied: %s: %s", errCode, desc)
		}
		return CallbackResult{}, fmt.Errorf("authorization denied: %s", errCode)
	}
	code := q.Get("code")
	if code == "" {
		return CallbackResult{}, errors.New("redirect url contained no ?code= parameter")
	}
	return CallbackResult{Code: code, State: q.Get("state")}, nil
}

func writeCallbackPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>dk &mdash; %[1]s</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;line-height:1.5">
<h1>%[1]s</h1><p>%[2]s</p>
</body></html>
`, htmlEscape(title), htmlEscape(detail))
}

// htmlEscape is a minimal escaper for the two interpolated strings above, both
// of which originate from DigiKey query parameters.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
