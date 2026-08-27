package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runIn is run() with the config directory supplied by the caller instead of a
// fresh one. Sharing a directory across invocations is the whole point: dk is a
// one-shot process, so the cache only pays off between runs, and a test that
// used a new directory each time would be testing nothing.
func runIn(t *testing.T, dir string, m *mockDigiKey, args ...string) result {
	t.Helper()

	res := tryIn(t, dir, m, args...)
	if res.Code != ExitOK {
		t.Fatalf("dk %s exited %d\nstderr: %s", strings.Join(args, " "), res.Code, res.Stderr)
	}
	return res
}

// tryIn is runIn without the exit-code assertion, for the tests whose subject
// is a command that is supposed to refuse.
func tryIn(t *testing.T, dir string, m *mockDigiKey, args ...string) result {
	t.Helper()

	t.Setenv("DK_CONFIG_DIR", dir)
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	t.Setenv("DIGIKEY_ENV", "production")
	if m != nil {
		t.Setenv("DIGIKEY_API_BASE_URL", m.server.URL)
	}

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	return result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

// apiCalls counts the requests that reached the mock API, ignoring the OAuth
// token endpoint — a token comes from dk's own cache, not from this one.
func (m *mockDigiKey) apiCalls(path string) int {
	n := 0
	for _, r := range m.requests {
		if r.Path == path {
			n++
		}
	}
	return n
}

func TestRepeatedSearchCostsOneAPICall(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)
	dir := t.TempDir()

	// The behavior this whole layer exists for: a caller runs a search, decides
	// it wanted the output filtered, and runs the same search again.
	first := runIn(t, dir, m, "search", "0.1uF 0603", "--output", "json")
	second := runIn(t, dir, m, "search", "0.1uF 0603", "--output", "json")

	if got := m.apiCalls("/products/v4/search/keyword"); got != 1 {
		t.Errorf("the API saw %d searches, want 1: the repeat should have been served from the cache", got)
	}
	if first.Stdout != second.Stdout {
		t.Errorf("cached run printed different output:\nfirst:  %s\nsecond: %s", first.Stdout, second.Stdout)
	}
}

func TestNoCacheAsksAgainAndRefreshesTheEntry(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)
	dir := t.TempDir()

	runIn(t, dir, m, "search", "0.1uF", "--output", "json")
	runIn(t, dir, m, "search", "0.1uF", "--no-cache", "--output", "json")
	if got := m.apiCalls("/products/v4/search/keyword"); got != 2 {
		t.Fatalf("the API saw %d searches, want 2: --no-cache must not serve a stored entry", got)
	}

	// --no-cache means what Cache-Control: no-cache means — revalidate, then
	// keep the answer. A run that only bypassed would leave the stale entry in
	// place for the next caller, which is the opposite of what someone reaching
	// for the flag wants.
	runIn(t, dir, m, "search", "0.1uF", "--output", "json")
	if got := m.apiCalls("/products/v4/search/keyword"); got != 2 {
		t.Errorf("the API saw %d searches, want 2: --no-cache should have replaced the entry", got)
	}
}

func TestCacheCanBeTurnedOff(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "flag", args: []string{"--cache-ttl", "0"}},
		{name: "environment", env: map[string]string{"DK_CACHE_TTL": "0"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockDigiKey(t)
			m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)
			dir := t.TempDir()
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			args := append([]string{"search", "0.1uF", "--output", "json"}, tc.args...)
			runIn(t, dir, m, args...)
			runIn(t, dir, m, args...)

			if got := m.apiCalls("/products/v4/search/keyword"); got != 2 {
				t.Errorf("the API saw %d searches, want 2 with caching off", got)
			}
			entries, _ := os.ReadDir(filepath.Join(dir, "cache"))
			if len(entries) != 0 {
				t.Errorf("caching is off but %d cache entries were written", len(entries))
			}
		})
	}
}

func TestCacheTTLExpiresEntries(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)
	dir := t.TempDir()

	// A TTL short enough that the second run is already past it. Stock and
	// price move, so an entry has to stop being served eventually.
	runIn(t, dir, m, "search", "0.1uF", "--cache-ttl", "1ns", "--output", "json")
	runIn(t, dir, m, "search", "0.1uF", "--cache-ttl", "1ns", "--output", "json")

	if got := m.apiCalls("/products/v4/search/keyword"); got != 2 {
		t.Errorf("the API saw %d searches, want 2: an expired entry must not be served", got)
	}
}

func TestNegativeCacheTTLIsAUsageError(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	res := run(t, m, "search", "0.1uF", "--cache-ttl", "-5m")
	if res.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d for a negative --cache-ttl\nstderr: %s", res.Code, ExitUsage, res.Stderr)
	}
	if p := res.ErrorJSON(t); p.Error.Code != CodeUsage {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeUsage)
	}
}

func TestUnparseableCacheTTLIsAConfigError(t *testing.T) {
	m := newMockDigiKey(t)
	t.Setenv("DK_CACHE_TTL", "ten minutes")

	res := run(t, m, "search", "0.1uF")
	if res.Code != ExitConfig {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", res.Code, ExitConfig, res.Stderr)
	}
	// The exit code and the JSON code have to agree, or a caller branching on
	// .error.code reaches a different conclusion than one branching on $?.
	p := res.ErrorJSON(t)
	if p.Error.Code != CodeConfig {
		t.Errorf("error code = %q, want %q", p.Error.Code, CodeConfig)
	}
	if !strings.Contains(p.Error.Hint, "cache_ttl") {
		t.Errorf("hint should name the setting to fix: %q", p.Error.Hint)
	}
}

func TestUnparseableCacheTTLStillLeavesTheRepairsWorking(t *testing.T) {
	// The commands that get a caller out of this state: the one the hint above
	// names, and the one that empties the cache. Validating the TTL for every
	// command would take both out and leave hand-editing config.json as the
	// only way back.
	tests := [][]string{
		{"config", "set", "cache_ttl", "10m"},
		{"cache", "clear"},
		{"config", "show"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DK_CACHE_TTL", "ten minutes")

			res := tryIn(t, dir, nil, append(args, "--output", "json")...)
			if res.Code != ExitOK {
				t.Fatalf("dk %s exited %d with a bad cache_ttl, want %d\nstderr: %s", strings.Join(args, " "), res.Code, ExitOK, res.Stderr)
			}
		})
	}
}

func TestListWriteDropsTheCachedListRead(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("POST", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `["uid-9"]`)

	dir := t.TempDir()
	loggedIn(t, dir)

	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")
	runIn(t, dir, m, "list", "add", "Bench PSU rev A", "296-6501-1-ND:5", "--output", "json")
	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")

	// Reporting a BOM as it stood before the add that just ran is the one
	// staleness this cache must never produce.
	if got := m.apiCalls("/mylists/v1/lists/aaa-111/parts"); got != 3 {
		t.Errorf("the API saw %d calls to the parts endpoint, want 3 (read, write, re-read)", got)
	}
}

// emptyPartsBody is a list with nothing in it, as the parts endpoint reports it.
const emptyPartsBody = `{"TotalParts":0,"PartsList":[]}`

func TestListDeleteGuardIsNotAnsweredFromTheCache(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/bbb-222/parts", http.StatusOK, emptyPartsBody)

	dir := t.TempDir()
	loggedIn(t, dir)

	// A read that caches the empty list...
	runIn(t, dir, m, "list", "show", "Audio Amp", "--limit", "1", "--output", "json")
	// ...and parts arriving from somewhere dk cannot see: digikey.com, another
	// machine, another tool.
	m.handle("GET", "/mylists/v1/lists/bbb-222/parts", http.StatusOK, listPartsBody)

	res := tryIn(t, dir, m, "list", "delete", "Audio Amp", "--output", "json")

	// The guard exists so a permanent delete is never decided by a stale count.
	// Answering it from the cache would hand it exactly that.
	if res.Code != ExitUsage {
		t.Fatalf("list delete exited %d, want %d: the guard read a cached part count and deleted a filled list\nstdout: %s", res.Code, ExitUsage, res.Stdout)
	}
	for _, r := range m.requests {
		if r.Method == http.MethodDelete {
			t.Fatalf("list delete issued %s %s despite the list holding parts", r.Method, r.Path)
		}
	}
}

func TestListWritesResolveIDsLive(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("DELETE", "/mylists/v1/lists/aaa-111/parts/uid-1", http.StatusOK, `{}`)

	dir := t.TempDir()
	loggedIn(t, dir)

	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")
	runIn(t, dir, m, "list", "rm", "Bench PSU rev A", "490-1532-1-ND", "--output", "json")

	// `dk list rm` turns a part number into the unique id it then deletes. A
	// cached id is not a stale figure on a screen; it is a delete aimed at a
	// line that may no longer be the one the caller named.
	if got := m.apiCalls("/mylists/v1/lists/aaa-111/parts"); got != 2 {
		t.Errorf("the API saw %d parts reads, want 2: the read behind a write must not come from the cache", got)
	}
}

// renamedListsBody is listsBody after the name "Bench PSU rev A" has moved to a
// different list — renamed and recreated on digikey.com, which dk cannot see.
const renamedListsBody = `[
  {"Id":"aaa-111","ListName":"Bench PSU rev A (old)","TotalParts":2,"DateModified":"2026-08-01T10:00:00Z"},
  {"Id":"ccc-333","ListName":"Bench PSU rev A","TotalParts":0,"DateModified":"2026-08-02T10:00:00Z"}
]`

func TestListWritesResolveTheNameLive(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	dir := t.TempDir()
	loggedIn(t, dir)

	// A read that caches the listing, and with it the name-to-id mapping...
	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")
	// ...and then the name moving to another list, on digikey.com or from
	// another machine.
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, renamedListsBody)
	m.handle("GET", "/mylists/v1/lists/ccc-333/parts", http.StatusOK, emptyPartsBody)
	m.handle("DELETE", "/mylists/v1/lists/ccc-333", http.StatusOK, `{}`)

	runIn(t, dir, m, "list", "delete", "Bench PSU rev A", "--output", "json")

	// The part-count guard is Live, so a delete resolved from the cached
	// listing would still be checked — against the wrong list, and then aimed
	// at it. Nothing about a permanent delete may come from a stored answer.
	for _, r := range m.requests {
		if r.Method == http.MethodDelete && r.Path != "/mylists/v1/lists/ccc-333" {
			t.Fatalf("list delete issued %s %s: the name was resolved from the cached listing", r.Method, r.Path)
		}
	}
	// Two GETs per walk: AllLists cannot trust a short page as the end, so it
	// confirms with one more request. Two live walks is therefore 4.
	if got := m.apiCalls("/mylists/v1/lists"); got != 4 {
		t.Errorf("the API saw %d listing reads, want 4 (2 walks x 2): the name a write resolves must not come from the cache", got)
	}
}

func TestListShowStillResolvesTheNameFromTheCache(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)

	dir := t.TempDir()
	loggedIn(t, dir)

	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")
	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")

	// The counterpart to the test above: reads are what the cache is for, and
	// making every list command resolve live would give it nothing to do in the
	// workflow it was built for.
	// One walk is 2 GETs (see above); the second run adds none, which is the
	// point — both of the first walk's requests came back from the cache.
	if got := m.apiCalls("/mylists/v1/lists"); got != 2 {
		t.Errorf("the API saw %d listing reads, want 2 (1 walk x 2): a read must still be served from the cache", got)
	}
}

func TestListWriteWithCachingOffStillDropsTheCachedRead(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("GET", "/mylists/v1/lists", http.StatusOK, listsBody)
	m.handle("GET", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, listPartsBody)
	m.handle("POST", "/mylists/v1/lists/aaa-111/parts", http.StatusOK, `["uid-9"]`)

	dir := t.TempDir()
	loggedIn(t, dir)

	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")
	// The add runs with the cache switched off for that invocation alone, which
	// says nothing about the entry the previous run wrote.
	runIn(t, dir, m, "list", "add", "Bench PSU rev A", "296-6501-1-ND:5", "--cache-ttl", "0", "--output", "json")
	runIn(t, dir, m, "list", "show", "Bench PSU rev A", "--output", "json")

	if got := m.apiCalls("/mylists/v1/lists/aaa-111/parts"); got != 3 {
		t.Errorf("the API saw %d calls to the parts endpoint, want 3: a write must invalidate whether or not this run reads the cache", got)
	}
}

func TestNoCacheSetOnTheAppSurvivesSetup(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	t.Setenv("DK_CONFIG_DIR", t.TempDir())
	t.Setenv("DIGIKEY_CLIENT_ID", "test-id")
	t.Setenv("DIGIKEY_CLIENT_SECRET", "test-secret")
	t.Setenv("DIGIKEY_API_BASE_URL", m.server.URL)

	// NoCache is part of App's surface, and App is what lets the whole CLI run
	// in-process against an httptest server. A hook that overwrote the field
	// from a flag nobody passed would make it write-only from out here.
	for range 2 {
		app := &App{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, NoCache: true}
		root := NewRootCommand(app)
		root.SetArgs([]string{"search", "0.1uF", "--output", "json"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("search: %v", err)
		}
	}

	if got := m.apiCalls("/products/v4/search/keyword"); got != 2 {
		t.Errorf("the API saw %d searches, want 2: App.NoCache was reset by the flag it was never given", got)
	}
}

func TestLogoutDropsCachedResponses(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)

	dir := t.TempDir()
	loggedIn(t, dir)

	runIn(t, dir, m, "search", "0.1uF", "--output", "json")
	runIn(t, dir, m, "auth", "logout", "--output", "json")
	// A token back in the store is what a subsequent `dk auth login` looks like
	// from here, and it may well be a different DigiKey account. The read is
	// under the same grant either way, so nothing in the cache key tells the
	// two apart — the entries have to have gone at logout.
	loggedIn(t, dir)
	runIn(t, dir, m, "search", "0.1uF", "--output", "json")

	if got := m.apiCalls("/products/v4/search/keyword"); got != 2 {
		t.Errorf("the API saw %d searches, want 2: logging out must drop the responses read under that login", got)
	}
}

func TestCacheStatusAndClear(t *testing.T) {
	m := newMockDigiKey(t)
	m.handle("POST", "/products/v4/search/keyword", http.StatusOK, searchResponseBody)
	dir := t.TempDir()

	runIn(t, dir, m, "search", "0.1uF", "--output", "json")

	var status cacheStatusView
	runIn(t, dir, m, "cache", "status", "--output", "json").JSON(t, &status)
	if !status.Enabled {
		t.Error("cache status reports disabled, want enabled by default")
	}
	if status.Entries != 1 || status.Fresh != 1 {
		t.Errorf("status = %+v, want 1 entry, 1 fresh after one search", status)
	}
	// enabled and ttl_seconds describe one setting and must never disagree: a
	// caller reading 0 seconds would take it for a disabled cache.
	if status.Enabled != (status.TTLSeconds > 0) {
		t.Errorf("status = %+v, want enabled and ttl_seconds to agree", status)
	}
	// DK_CONFIG_DIR is dk's isolation lever; a cache that escaped it would let
	// a test run read and pollute the real user cache. Fatal rather than an
	// error: the next thing this test does is `dk cache clear`, which would act
	// on whatever directory the check just found wrong.
	if !strings.HasPrefix(status.Dir, dir) {
		t.Fatalf("cache dir = %q, want it under the configured dir %q", status.Dir, dir)
	}

	var cleared cacheClearView
	runIn(t, dir, m, "cache", "clear", "--output", "json").JSON(t, &cleared)
	if cleared.Removed != 1 {
		t.Errorf("clear removed %d entries, want 1", cleared.Removed)
	}

	// After a clear the same search has to reach the API again.
	runIn(t, dir, m, "search", "0.1uF", "--output", "json")
	if got := m.apiCalls("/products/v4/search/keyword"); got != 2 {
		t.Errorf("the API saw %d searches, want 2: `dk cache clear` should have dropped the entry", got)
	}
}

func TestSubSecondTTLDoesNotReportAsDisabled(t *testing.T) {
	dir := t.TempDir()

	var status cacheStatusView
	runIn(t, dir, nil, "cache", "status", "--cache-ttl", "500ms", "--output", "json").JSON(t, &status)

	if !status.Enabled {
		t.Fatalf("status = %+v, want enabled for a 500ms TTL", status)
	}
	// Truncating to an int reported 0 here, which is the value that means
	// "off" everywhere else in this feature.
	if status.TTLSeconds <= 0 {
		t.Errorf("ttl_seconds = %v alongside enabled=true; the two fields contradict each other", status.TTLSeconds)
	}
}

func TestCacheStatusPrintsNothingToStdoutButTheResult(t *testing.T) {
	dir := t.TempDir()
	// `dk cache status` reads no API, so it must work without credentials or a
	// server, and its stdout has to stay parseable like every other command's.
	res := runIn(t, dir, nil, "cache", "status", "--output", "json")
	var status cacheStatusView
	res.JSON(t, &status)
	if status.Dir == "" {
		t.Error("cache status reported no directory")
	}
}
