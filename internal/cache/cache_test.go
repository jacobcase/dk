package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// entryFixture is a name isEntryName accepts: the hex digest of a key, plus
// .json. Tests that plant a file by hand rather than through Put use it.
var entryFixture = strings.Repeat("a", 64) + ".json"

// scopeFixture is a valid scope name for tests that do not care which one.
const scopeFixture = "test"

// at returns a cache whose clock is pinned, so freshness is decided by the test
// rather than by how long the test took to run.
func at(t *testing.T, ttl time.Duration, now *time.Time) *Cache {
	t.Helper()
	c := New(t.TempDir(), ttl)
	if c == nil {
		t.Fatalf("New(dir, %v) = nil, want a cache", ttl)
	}
	c.now = func() time.Time { return *now }
	return c
}

func TestPutThenGetReturnsTheStoredBody(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c := at(t, time.Minute, &now)

	if err := c.Put(scopeFixture, "key", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	got, ok := c.Get(scopeFixture, "key")
	if !ok {
		t.Fatal("Get() reported a miss for an entry just written")
	}
	if string(got) != `{"a":1}` {
		t.Errorf("Get() = %q, want the body that was stored", got)
	}
}

func TestFreshness(t *testing.T) {
	const ttl = 10 * time.Minute

	tests := []struct {
		name    string
		elapsed time.Duration
		want    bool
		why     string
	}{
		{"just written", 0, true, "an entry written this instant must be served"},
		{"within ttl", ttl - time.Second, true, "an entry inside the window must be served"},
		{"exactly at ttl", ttl, false, "the TTL is the age at which an entry stops being fresh"},
		{"past ttl", ttl + time.Second, false, "an expired entry must not be served"},
		// A clock that moved backwards leaves entries stamped in the future.
		// Serving those would mean answering from an unknown point in time.
		{"clock moved backwards", -time.Hour, false, "an entry stamped in the future must not be served"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
			c := at(t, ttl, &now)
			if err := c.Put(scopeFixture, "key", []byte("body")); err != nil {
				t.Fatal(err)
			}

			now = now.Add(tc.elapsed)
			if _, ok := c.Get(scopeFixture, "key"); ok != tc.want {
				t.Errorf("Get() after %v = %t, want %t: %s", tc.elapsed, ok, tc.want, tc.why)
			}
		})
	}
}

func TestScopesDoNotShareEntries(t *testing.T) {
	now := time.Now()
	c := at(t, time.Minute, &now)

	if err := c.Put("product", "same-key", []byte("product body")); err != nil {
		t.Fatal(err)
	}
	// The same key in another scope is a different entry. If scopes shared a
	// namespace, invalidating list writes would drop catalog reads too.
	if _, ok := c.Get("lists", "same-key"); ok {
		t.Error("a key written in one scope was readable from another")
	}
}

func TestInvalidateDropsOnlyItsOwnScope(t *testing.T) {
	now := time.Now()
	c := at(t, time.Minute, &now)

	if err := c.Put("product", "k", []byte("catalog")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("lists", "k", []byte("list")); err != nil {
		t.Fatal(err)
	}
	if err := c.Invalidate("lists"); err != nil {
		t.Fatalf("Invalidate() error = %v", err)
	}

	if _, ok := c.Get("lists", "k"); ok {
		t.Error("Invalidate left an entry behind; a list write would then serve what it changed")
	}
	// The whole point of scoping: `dk list add` must not throw away the search
	// results the caller is working from.
	if _, ok := c.Get("product", "k"); !ok {
		t.Error("Invalidate on one scope dropped another scope's entry")
	}
}

func TestInvalidScopeIsRejectedRatherThanResolved(t *testing.T) {
	now := time.Now()
	c := at(t, time.Minute, &now)

	// A scope names a directory that Invalidate hands to RemoveAll. A separator
	// or a ".." reaching that call would delete somewhere else entirely, so the
	// guard matters more than the error message.
	for _, scope := range []string{"", "..", "../..", "a/b", "Product", `a\b`} {
		if err := c.Put(scope, "k", []byte("x")); err == nil {
			t.Errorf("Put(%q) accepted an invalid scope", scope)
		}
		if err := c.Invalidate(scope); err == nil {
			t.Errorf("Invalidate(%q) accepted an invalid scope", scope)
		}
		if _, ok := c.Get(scope, "k"); ok {
			t.Errorf("Get(%q) reported a hit for an invalid scope", scope)
		}
	}
}

func TestClearRemovesOnlyWhatThisPackageWrote(t *testing.T) {
	dir := t.TempDir()
	c := New(dir, time.Minute)
	if err := c.Put(scopeFixture, "k", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// The cache root comes from DK_CACHE_DIR, which can name any directory the
	// user has. `dk cache clear` deletes that path, so everything in it that
	// dk did not write has to survive.
	foreign := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(foreign, []byte("not dk's"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "important")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "data.txt"), []byte("also not dk's"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Counted before the clear, and it has to count the same things the clear
	// removes: a foreign file included here would be reported as deleted.
	stats, err := Stat(dir, time.Minute)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if stats.Entries != 1 {
		t.Errorf("Stat() counted %d entries, want 1: only cache entries belong in the count", stats.Entries)
	}

	if err := Clear(dir); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if _, ok := c.Get(scopeFixture, "k"); ok {
		t.Error("Clear() left a cache entry behind")
	}
	for _, path := range []string{foreign, filepath.Join(nested, "data.txt")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("Clear() deleted %s, which dk did not write: %v", path, err)
		}
	}
}

func TestClearRemovesAnInterruptedWrite(t *testing.T) {
	dir := t.TempDir()
	c := New(dir, time.Minute)
	if err := c.Put(scopeFixture, "k", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// What a crash between atomicfile.Write's CreateTemp and its Rename leaves
	// behind. Entries are written on nearly every command, unlike the config
	// and token files, so a name nothing here recognized would accumulate and
	// keep the scope directory from ever being reclaimed.
	scope := filepath.Join(dir, scopeFixture)
	leftover := filepath.Join(scope, entryFixture+".tmp1234567")
	if err := os.WriteFile(leftover, []byte("half a response"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Removable, but never counted: a half-written file is not a response
	// anyone can be served.
	stats, err := Stat(dir, time.Minute)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if stats.Entries != 1 {
		t.Errorf("Stat() counted %d entries, want 1: an interrupted write is not an entry", stats.Entries)
	}

	if err := Clear(dir); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Clear() left %s behind; nothing in this package would ever look at it again", leftover)
	}
	if _, err := os.Stat(scope); !errors.Is(err, fs.ErrNotExist) {
		t.Error("Clear() left the scope directory behind after emptying it")
	}
}

func TestClearLeavesAnEmptyDirectoryItDidNotFill(t *testing.T) {
	dir := t.TempDir()

	// A plausible scope name is not proof of ownership. The root is whatever
	// DK_CACHE_DIR named, so an empty directory under it may well be the
	// user's, and rmdir'ing it is this package deleting what it never wrote.
	foreign := filepath.Join(dir, "notes")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := Clear(dir); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("Clear() removed %s, an empty directory dk never wrote to: %v", foreign, err)
	}
}

func TestStatCountsOnlyTheLevelClearRemovesFrom(t *testing.T) {
	dir := t.TempDir()
	c := New(dir, time.Minute)
	if err := c.Put(scopeFixture, "k", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// Put writes one level down and Clear reads one level down, so an
	// entry-named file any deeper — a restored backup, a synced directory —
	// cannot be removed. Counting it would have `dk cache clear` report a
	// removal that never happened.
	nested := filepath.Join(dir, scopeFixture, "restored")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	buried := filepath.Join(nested, entryFixture)
	if err := os.WriteFile(buried, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := Stat(dir, time.Minute)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if stats.Entries != 1 {
		t.Errorf("Stat() counted %d entries, want 1: only what Clear can remove belongs in the count", stats.Entries)
	}
	if err := Clear(dir); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if _, err := os.Stat(buried); err != nil {
		t.Errorf("Clear() removed %s, which is below the level it reads: %v", buried, err)
	}
}

func TestNilCacheIsAWorkingNoOp(t *testing.T) {
	// New reports "caching is off" by returning nil, so every method has to be
	// safe on a nil receiver — otherwise --cache-ttl 0 would panic.
	var c *Cache

	if _, ok := c.Get(scopeFixture, "k"); ok {
		t.Error("a nil cache reported a hit")
	}
	if err := c.Put(scopeFixture, "k", []byte("x")); err != nil {
		t.Errorf("Put() on a nil cache: %v", err)
	}
	if err := c.Invalidate(scopeFixture); err != nil {
		t.Errorf("Invalidate() on a nil cache: %v", err)
	}
	if c.Dir() != "" || c.TTL() != 0 {
		t.Errorf("nil cache reported dir %q ttl %v, want empty and zero", c.Dir(), c.TTL())
	}
}

func TestNewReturnsNilOnlyWithoutADirectory(t *testing.T) {
	if c := New("", time.Minute); c != nil {
		t.Error("New(\"\", ttl) returned a cache, want nil for an unset directory")
	}
	// A zero or negative TTL still yields a usable value. Reading and writing
	// are off, but Invalidate has to keep working: a list write made with the
	// cache switched off must still drop what an earlier run stored, or the
	// next run serves the contents from before the write.
	for _, ttl := range []time.Duration{0, -time.Second} {
		if c := New(t.TempDir(), ttl); c == nil {
			t.Errorf("New(dir, %v) returned nil, want a cache that invalidates", ttl)
		}
	}
}

func TestZeroTTLReadsAndWritesNothingButStillInvalidates(t *testing.T) {
	dir := t.TempDir()

	// Something for an earlier run to have left behind.
	warm := New(dir, time.Minute)
	if err := warm.Put(scopeFixture, "k", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	off := New(dir, 0)
	if _, ok := off.Get(scopeFixture, "k"); ok {
		t.Error("a zero-TTL cache served an entry; caching is supposed to be off")
	}
	if err := off.Put(scopeFixture, "other", []byte(`{"b":2}`)); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if _, ok := warm.Get(scopeFixture, "other"); ok {
		t.Error("a zero-TTL cache stored an entry; nothing would ever serve it")
	}

	if err := off.Invalidate(scopeFixture); err != nil {
		t.Fatalf("Invalidate() error = %v", err)
	}
	if _, ok := warm.Get(scopeFixture, "k"); ok {
		t.Error("invalidating through a zero-TTL cache left the earlier entry in place")
	}
}

func TestEmptyBodyIsNotStored(t *testing.T) {
	now := time.Now()
	c := at(t, time.Minute, &now)

	if err := c.Put(scopeFixture, "k", nil); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	// A zero-length file would read back as a valid entry and decode into
	// nothing, which is indistinguishable from a successful empty response.
	if _, ok := c.Get(scopeFixture, "k"); ok {
		t.Error("an empty body was stored and served as a cache hit")
	}
}

func TestEntriesAreNotReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}
	now := time.Now()
	c := at(t, time.Minute, &now)

	if err := c.Put(scopeFixture, "k", []byte("body")); err != nil {
		t.Fatal(err)
	}

	var checked int
	err := filepath.WalkDir(c.Dir(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		checked++
		// With a 3-legged token in play these bodies carry account-specific
		// pricing, so they get the same 0600 the token file does.
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no cache files found to check")
	}
}

func TestPutPrunesExpiredEntries(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c := at(t, time.Minute, &now)

	if err := c.Put(scopeFixture, "old", []byte("body")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := c.Put(scopeFixture, "new", []byte("body")); err != nil {
		t.Fatal(err)
	}

	// Without a sweep the product scope would grow for the life of the machine:
	// every distinct search is a new entry that nothing ever revisits.
	stats, err := Stat(c.Dir(), c.TTL())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entries != 1 {
		t.Errorf("entries = %d, want 1: the expired entry should have been swept on write", stats.Entries)
	}
}

func TestStatSeparatesFreshFromExpired(t *testing.T) {
	// Stat reports what is on disk right now for `dk cache status`, so it reads
	// the real clock rather than an injected one. Stamp the entries accordingly.
	now := time.Now()
	c := at(t, time.Hour, &now)

	if err := c.Put("product", "a", []byte("aaaa")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("lists", "b", []byte("bb")); err != nil {
		t.Fatal(err)
	}

	stats, err := Stat(c.Dir(), c.TTL())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if stats.Entries != 2 || stats.Fresh != 2 {
		t.Errorf("Stat() = %+v, want 2 entries, 2 fresh", stats)
	}
	if stats.Bytes != 6 {
		t.Errorf("Stat().Bytes = %d, want 6", stats.Bytes)
	}

	// With caching off nothing would be served, so nothing counts as fresh —
	// but the files are still there, which is what `dk cache clear` reclaims.
	off, err := Stat(c.Dir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if off.Entries != 2 || off.Fresh != 0 {
		t.Errorf("Stat(dir, 0) = %+v, want 2 entries, 0 fresh", off)
	}
}

func TestStatAndClearTolerateAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-written")

	// An empty cache and a cache that has never been written to are the same
	// thing to a caller, and neither is a failure.
	stats, err := Stat(dir, time.Minute)
	if err != nil {
		t.Fatalf("Stat() on a missing dir: %v", err)
	}
	if stats.Entries != 0 {
		t.Errorf("Stat() = %+v, want zero entries", stats)
	}
	if err := Clear(dir); err != nil {
		t.Errorf("Clear() on a missing dir: %v", err)
	}
}

func TestClearRemovesEveryScope(t *testing.T) {
	now := time.Now()
	c := at(t, time.Minute, &now)

	if err := c.Put("product", "a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("lists", "b", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := Clear(c.Dir()); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}

	stats, err := Stat(c.Dir(), c.TTL())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entries != 0 {
		t.Errorf("Stat() after Clear() = %+v, want zero entries", stats)
	}
}
