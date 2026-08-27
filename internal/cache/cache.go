// Package cache stores DigiKey API responses on disk so that repeating a read
// does not spend another request against a rate-limited quota.
//
// dk is a one-shot process, so "repeating a read" is the common case rather
// than the exception: a query run once to look at and again to pipe somewhere
// else is two identical requests, and a caller that pages through a list twice
// pays for every page twice. Nothing about that is visible from inside a single
// invocation, which is why the cache lives on disk rather than in memory.
//
// An entry is one file holding the response body verbatim, named for a hash of
// the caller's key. Freshness is the file's modification time measured against
// the current TTL — deliberately not an expiry baked in at write time, so that
// lowering --cache-ttl takes effect immediately instead of after the entries
// written under the old value age out.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcase/dk/internal/atomicfile"
)

// Cache is a TTL-bounded store of response bodies, partitioned into scopes so
// that a write to one part of the API can invalidate only what it affects.
//
// A nil *Cache is usable and does nothing: every read misses and every write is
// dropped. That is what lets the caller treat "caching disabled" as an ordinary
// value rather than a branch at each call site.
//
// A zero TTL is not the same thing as a nil Cache. Reading and writing are off,
// but Invalidate still works, because dropping what an earlier run stored is an
// obligation of the write rather than a feature of the cache — see New.
type Cache struct {
	dir string
	ttl time.Duration
	// now is overridable for tests; nil means time.Now.
	now func() time.Time
}

// New returns a Cache rooted at dir, or nil when there is no directory to root
// it in.
//
// A ttl of zero or less turns off both halves of caching — every Get misses and
// every Put is dropped — but still returns a usable value, because Invalidate
// has to keep working. Whether this invocation reads the cache is a preference;
// dropping what an earlier invocation stored for a list this one just changed
// is a correctness obligation, and gating it on the preference is how
// `dk list add --cache-ttl 0` used to leave the next `dk list show` serving the
// contents from before the add.
func New(dir string, ttl time.Duration) *Cache {
	if dir == "" {
		return nil
	}
	return &Cache{dir: dir, ttl: ttl}
}

// Dir returns the cache root, or "" when caching is disabled.
func (c *Cache) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// TTL returns the freshness window, or 0 when caching is disabled.
func (c *Cache) TTL() time.Duration {
	if c == nil {
		return 0
	}
	return c.ttl
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Get returns the cached body for key, if one exists and is still fresh.
//
// It never reports an error. A cache that cannot be read is a cache miss: the
// caller's next step is to ask DigiKey, which is the correct outcome for an
// unreadable entry, a truncated file, or a cache directory someone deleted
// mid-run. Failing the command instead would make dk less reliable than it was
// before the cache existed.
func (c *Cache) Get(scope, key string) ([]byte, bool) {
	// A zero TTL means reading is off. Nothing is fresh within a zero window,
	// so this changes no answer — it is here so that "off" costs no syscalls
	// rather than a stat per read that can only miss.
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	path, err := c.entryPath(scope, key)
	if err != nil {
		return nil, false
	}

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	if !c.fresh(info.ModTime()) {
		return nil, false
	}

	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	return body, true
}

// fresh reports whether an entry stamped at mod is still within the TTL.
func (c *Cache) fresh(mod time.Time) bool {
	return isFresh(c.clock(), mod, c.ttl)
}

// isFresh reports whether an entry stamped at mod is still within ttl as of now.
//
// A modification time in the future means the clock moved backwards (or the
// file was touched by something else); treating that as stale costs one request
// and cannot serve a response from an unknown point in time.
func isFresh(now, mod time.Time, ttl time.Duration) bool {
	age := now.Sub(mod)
	return age >= 0 && age < ttl
}

// Put stores body under key. An empty body is not stored: there is nothing to
// serve from it, and a zero-length file would read as a valid entry.
func (c *Cache) Put(scope, key string, body []byte) error {
	// A zero TTL stores nothing: Get would never serve the entry, so writing it
	// only costs disk. Get needs no such guard — nothing is fresh within a zero
	// window, so it misses on its own.
	if c == nil || c.ttl <= 0 || len(body) == 0 {
		return nil
	}
	path, err := c.entryPath(scope, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	// 0600, matching the token and config files: with a 3-legged token in play
	// these responses carry account-specific pricing.
	if err := atomicfile.Write(path, body, 0o600); err != nil {
		return err
	}
	// Stamp the entry from the cache's own clock rather than leaving it at
	// whatever the filesystem recorded, so that the time an entry claims and the
	// time it is measured against come from one source. In production the two
	// are the same instant; a stamp that failed to apply leaves the write time,
	// which is still the right answer.
	if now := c.clock(); !now.IsZero() {
		_ = os.Chtimes(path, now, now)
	}
	// Sweeping the scope just written keeps the directory proportional to one
	// TTL of traffic. Every distinct search is a new entry, so without this the
	// product scope would grow for the life of the machine.
	c.prune(scope)
	return nil
}

// Invalidate drops every entry in a scope. Callers use it after a write that
// changes what a read in that scope would return.
func (c *Cache) Invalidate(scope string) error {
	if c == nil {
		return nil
	}
	dir, err := c.scopeDir(scope)
	if err != nil {
		return err
	}
	if err := removeEntries(dir); err != nil {
		return fmt.Errorf("invalidate cache scope %s: %w", scope, err)
	}
	return nil
}

// isEntryName reports whether name is a file this package wrote: the hex digest
// of a key, plus .json. Clear, Invalidate, Stat, and prune all confine
// themselves to these and to the temporaries isTempName recognizes — the cache
// root can be any directory the user named through DK_CACHE_DIR, and nothing dk
// did not write is dk's to delete.
func isEntryName(name string) bool {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok || len(base) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for _, r := range base {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

// isTempName reports whether name is a leftover from atomicfile.Write, which
// creates "<entry>.tmp*" beside the entry and renames it into place. A crash
// between the two leaves one behind.
//
// It is a removal test, not an entry test: a half-written file is not a cached
// response, so Stat must not count it as one, but nothing else in this package
// would ever look at it again either. Unlike the config and token files, cache
// entries are written on nearly every command, so these accumulate.
func isTempName(name string) bool {
	i := strings.LastIndex(name, ".tmp")
	return i >= 0 && isEntryName(name[:i])
}

// isRemovable reports whether this package wrote name and may delete it: a
// cache entry, or the temporary file an interrupted write left beside one.
func isRemovable(e fs.DirEntry) bool {
	return !e.IsDir() && (isEntryName(e.Name()) || isTempName(e.Name()))
}

// removeEntries deletes every cache entry directly inside dir, then the
// directory itself if that emptied it. A directory holding anything else is
// left in place, along with whatever that was.
func removeEntries(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	removed := 0
	for _, e := range entries {
		if !isRemovable(e) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed++
	}
	// Only once something of ours has actually gone. An empty directory under
	// the cache root whose name merely looks like a scope may never have been
	// ours — the root is whatever DK_CACHE_DIR named — and rmdir'ing it would
	// be this package deleting a directory it did not fill.
	//
	// Non-recursive on purpose: this succeeds only when the directory is now
	// empty, and fails harmlessly when something that is not ours is still in it.
	if removed > 0 {
		_ = os.Remove(dir)
	}
	return nil
}

// Clear removes every entry in every scope.
//
// It takes a directory rather than a *Cache because clearing has to work when
// caching is switched off — turning the cache off does not delete what it
// already wrote, and that is exactly when someone wants to reclaim it.
//
// Deliberately not RemoveAll(dir): the root is user-supplied through
// DK_CACHE_DIR, so only the scope directories this package created are removed.
// Anything else living under that path was not dk's to delete, and `dk cache
// clear` must not be a way to lose it.
func Clear(dir string) error {
	if dir == "" {
		return nil
	}
	scopes, err := scopeNames(dir)
	if err != nil {
		return fmt.Errorf("clear cache: %w", err)
	}
	for _, scope := range scopes {
		if err := removeEntries(filepath.Join(dir, scope)); err != nil {
			return fmt.Errorf("clear cache: %w", err)
		}
	}
	return nil
}

// scopeNames lists the directories under a cache root that could hold entries.
// Clear and Stat share it so that the count reported before a clear is the
// count the clear actually removes.
//
// The name test is necessary rather than sufficient — "lists" is a plausible
// name for a directory of someone's own — which is why what happens inside one
// is decided per file by isEntryName rather than by the directory alone.
//
// A missing root is not an error: it is what an empty cache looks like.
func scopeNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && validScope(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Stats describes what is currently on disk.
type Stats struct {
	// Entries counts every file, Fresh only those still within the TTL. The
	// difference is what a sweep would remove.
	Entries int   `json:"entries"`
	Fresh   int   `json:"fresh_entries"`
	Bytes   int64 `json:"bytes"`
}

// Stat walks a cache directory. A missing directory is not an error: it is
// what an empty cache looks like before anything has been written.
//
// Like Clear, it takes a directory so that it still reports what is on disk
// when caching is off. A ttl of zero or less leaves Fresh at zero, which is
// accurate: with the cache disabled, no entry would be served.
//
// Also like Clear, it looks only inside the scope directories, and only one
// level down, which is where Put writes and the only level Clear, Invalidate,
// and prune read. `dk cache clear` counts with this and then deletes with that,
// so counting a foreign or nested file here would report a removal that never
// happens.
func Stat(dir string, ttl time.Duration) (Stats, error) {
	var s Stats
	if dir == "" {
		return s, nil
	}
	scopes, err := scopeNames(dir)
	if err != nil {
		return s, fmt.Errorf("read cache dir: %w", err)
	}
	for _, scope := range scopes {
		entries, err := os.ReadDir(filepath.Join(dir, scope))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return s, fmt.Errorf("read cache dir: %w", err)
		}
		for _, e := range entries {
			// Entries only. A leftover temporary file is removable but is not a
			// response anyone can be served.
			if e.IsDir() || !isEntryName(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			s.Entries++
			s.Bytes += info.Size()
			if ttl > 0 && isFresh(time.Now(), info.ModTime(), ttl) {
				s.Fresh++
			}
		}
	}
	return s, nil
}

// prune removes expired entries from one scope. Failures are ignored: pruning
// is housekeeping, and a cache directory that cannot be tidied is not a reason
// to fail the request that just succeeded.
func (c *Cache) prune(scope string) {
	dir, err := c.scopeDir(scope)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !isRemovable(e) {
			continue
		}
		// The freshness test covers the leftovers too, and has to: a temporary
		// file another process is writing right now is fresh, and yanking it
		// would fail that write's rename.
		info, err := e.Info()
		if err != nil || c.fresh(info.ModTime()) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func (c *Cache) scopeDir(scope string) (string, error) {
	if err := validScope(scope); err != nil {
		return "", err
	}
	return filepath.Join(c.dir, scope), nil
}

func (c *Cache) entryPath(scope, key string) (string, error) {
	dir, err := c.scopeDir(scope)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

// validScope rejects anything that is not a plain name. Scopes are compile-time
// constants today, but they name a directory that Invalidate passes to
// RemoveAll, and a separator or a ".." reaching that call would delete
// somewhere else entirely.
func validScope(scope string) error {
	if scope == "" {
		return errors.New("cache: empty scope")
	}
	for _, r := range scope {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			continue
		}
		return fmt.Errorf("cache: invalid scope %q", scope)
	}
	return nil
}
