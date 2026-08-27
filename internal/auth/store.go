package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcase/dk/internal/atomicfile"
)

// Token is a cached OAuth token. RefreshToken is only populated for user
// (3-legged) tokens; DigiKey does not issue refresh tokens to the client
// credentials grant.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope,omitempty"`
}

// Valid reports whether the token is usable at time now, leaving skew of
// headroom so a token does not expire mid-flight.
func (t *Token) Valid(now time.Time, skew time.Duration) bool {
	if t == nil || t.AccessToken == "" {
		return false
	}
	return now.Add(skew).Before(t.ExpiresAt)
}

// Tokens holds every token dk caches, keyed by environment so that switching
// between sandbox and production does not require re-authenticating.
type Tokens struct {
	// App holds client-credentials tokens, keyed by environment name.
	App map[string]*Token `json:"app,omitempty"`
	// User holds 3-legged tokens, keyed by environment name.
	User map[string]*Token `json:"user,omitempty"`
}

// Store persists Tokens to a 0600 file on disk. The zero value is not usable;
// construct one with NewStore.
type Store struct {
	path string

	mu     sync.Mutex
	loaded bool
	tokens Tokens
}

// NewStore returns a Store backed by the given file path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the file the store reads and writes.
func (s *Store) Path() string { return s.path }

// load reads the token file into memory. A missing file yields empty tokens.
// Callers must hold s.mu.
func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &s.tokens); err != nil {
			return fmt.Errorf("parse %s: %w", s.path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		s.tokens = Tokens{}
	default:
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	s.loaded = true
	return nil
}

// Get returns the cached token of the given kind for env, or nil if absent.
func (s *Store) Get(kind Kind, env string) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	m := s.bucket(kind)
	if m == nil {
		return nil, nil
	}
	tok, ok := m[env]
	if !ok || tok == nil {
		return nil, nil
	}
	// Return a copy so callers cannot mutate cached state.
	cp := *tok
	return &cp, nil
}

// Put stores a token and flushes the file to disk.
func (s *Store) Put(kind Kind, env string, tok *Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	switch kind {
	case KindApp:
		if s.tokens.App == nil {
			s.tokens.App = map[string]*Token{}
		}
		s.tokens.App[env] = tok
	case KindUser:
		if s.tokens.User == nil {
			s.tokens.User = map[string]*Token{}
		}
		s.tokens.User[env] = tok
	default:
		return fmt.Errorf("unknown token kind %q", kind)
	}
	return s.flush()
}

// Delete removes a token of the given kind for env and flushes to disk.
// Deleting an absent token is not an error.
func (s *Store) Delete(kind Kind, env string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if m := s.bucket(kind); m != nil {
		delete(m, env)
	}
	return s.flush()
}

// Clear removes every cached token.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = Tokens{}
	s.loaded = true
	return s.flush()
}

// bucket returns the map for a token kind. Callers must hold s.mu.
func (s *Store) bucket(kind Kind) map[string]*Token {
	switch kind {
	case KindApp:
		return s.tokens.App
	case KindUser:
		return s.tokens.User
	default:
		return nil
	}
}

// flush writes the in-memory tokens to disk. Callers must hold s.mu.
func (s *Store) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	data, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tokens: %w", err)
	}
	data = append(data, '\n')

	return atomicfile.Write(s.path, data, 0o600)
}
