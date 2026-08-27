package digikey

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jacobcase/dk/internal/cache"
)

// recorder counts the requests a test server actually received. The count is
// the assertion in every test here: the cache exists to make the second
// identical read cost nothing, and only the server can say whether it did.
type recorder struct {
	mu     sync.Mutex
	total  int
	byPath map[string]int
}

func (r *recorder) note(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	r.byPath[path]++
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

func (r *recorder) at(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byPath[path]
}

// cachingServer returns a server that answers everything with body, and a
// recorder of what it was asked.
func cachingServer(t *testing.T, body string) (string, *recorder) {
	t.Helper()
	return statusServer(t, func(int, *http.Request) (int, string) { return http.StatusOK, body })
}

// statusServer lets a test vary the response by request number and by the
// request itself, which is how the "an error is never cached" case and the
// read-then-write list cases are set up.
func statusServer(t *testing.T, reply func(n int, r *http.Request) (int, string)) (string, *recorder) {
	t.Helper()
	rec := &recorder{byPath: map[string]int{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		rec.note(r.URL.Path)
		status, body := reply(rec.count(), r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

// cachedClient builds a client sharing dir as its response cache. Separate
// clients over one directory stand in for what dk actually does: each command
// is a new process reading what the last one wrote.
func cachedClient(t *testing.T, dir, baseURL string, opts ...func(*Options)) *Client {
	t.Helper()

	o := Options{
		BaseURL:  baseURL,
		ClientID: "test-client-id",
		Locale:   Locale{Site: "US", Language: "en", Currency: "USD"},
		Tokens:   &staticTokens{app: "app-tok", user: "user-tok"},
		Cache:    cache.New(dir, time.Hour),
	}
	for _, opt := range opts {
		opt(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

const cachedSearchBody = `{"ProductsCount":1,"Products":[{"ManufacturerProductNumber":"GRM188R71C104KA01D"}]}`

func TestIdenticalReadIsServedFromCache(t *testing.T) {
	url, rec := cachingServer(t, cachedSearchBody)
	dir := t.TempDir()
	req := KeywordRequest{Keywords: "0.1uF 0603", Limit: 5}

	first, err := cachedClient(t, dir, url).KeywordSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}
	// A second process, same query. This is the case the cache exists for: an
	// agent that runs a search, then runs it again to pipe the output somewhere.
	second, err := cachedClient(t, dir, url).KeywordSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}

	if rec.count() != 1 {
		t.Errorf("server saw %d requests, want 1: the repeat read should have come from the cache", rec.count())
	}
	// A cached answer that decodes differently from the live one would be worse
	// than no cache at all, so compare the decoded results, not just the count.
	if len(second.Products) != len(first.Products) ||
		second.Products[0].ManufacturerProductNumber != first.Products[0].ManufacturerProductNumber {
		t.Errorf("cached response decoded to %+v, want the same as the live one %+v", second, first)
	}
}

func TestRawAndTypedReadsShareOneEntry(t *testing.T) {
	url, rec := cachingServer(t, cachedSearchBody)
	dir := t.TempDir()
	req := KeywordRequest{Keywords: "0.1uF 0603", Limit: 5}

	raw, err := cachedClient(t, dir, url).RawKeywordSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("RawKeywordSearch() error = %v", err)
	}
	if _, err := cachedClient(t, dir, url).KeywordSearch(context.Background(), req); err != nil {
		t.Fatalf("KeywordSearch() error = %v", err)
	}

	// The cache stores response bytes, not a decoded struct, so --raw and the
	// view path are the same entry. Storing decoded structs instead would both
	// double the traffic and re-introduce the field loss --raw exists to avoid.
	if rec.count() != 1 {
		t.Errorf("server saw %d requests, want 1: --raw and the typed read should share an entry", rec.count())
	}
	if !json.Valid(raw) {
		t.Errorf("RawKeywordSearch() returned invalid json: %s", raw)
	}
}

func TestCacheSurvivesATokenRefresh(t *testing.T) {
	url, rec := cachingServer(t, cachedSearchBody)
	dir := t.TempDir()
	req := KeywordRequest{Keywords: "0.1uF"}

	// DigiKey's client_credentials token lives 600 seconds against a 10m
	// default TTL, so a caller straddles a refresh constantly. Keying on the
	// token bytes rotated the whole namespace at that cadence and made the
	// cache close to useless for a search-only caller; the grant is what
	// actually changes the answer.
	mustSearch(t, cachedClient(t, dir, url, func(o *Options) {
		o.Tokens = &staticTokens{app: "first-token"}
	}), req)
	mustSearch(t, cachedClient(t, dir, url, func(o *Options) {
		o.Tokens = &staticTokens{app: "second-token"}
	}), req)

	if rec.count() != 1 {
		t.Errorf("server saw %d requests, want 1: a refreshed token is the same grant asking the same question", rec.count())
	}
}

func TestDistinctRequestsGetDistinctEntries(t *testing.T) {
	tests := []struct {
		name string
		call func(t *testing.T, dir, url string)
		why  string
	}{
		{
			name: "different keywords",
			call: func(t *testing.T, dir, url string) {
				c := cachedClient(t, dir, url)
				mustSearch(t, c, KeywordRequest{Keywords: "first"})
				mustSearch(t, c, KeywordRequest{Keywords: "second"})
			},
			why: "two searches are two questions",
		},
		{
			name: "different page",
			call: func(t *testing.T, dir, url string) {
				c := cachedClient(t, dir, url)
				mustSearch(t, c, KeywordRequest{Keywords: "same", Offset: 0})
				mustSearch(t, c, KeywordRequest{Keywords: "same", Offset: 50})
			},
			why: "page two is not page one; sharing an entry would silently truncate a paged read",
		},
		{
			name: "different token grant",
			call: func(t *testing.T, dir, url string) {
				app := cachedClient(t, dir, url, func(o *Options) {
					o.Tokens = &staticTokens{app: "app-tok"}
				})
				mustSearch(t, app, KeywordRequest{Keywords: "same"})

				// Once a 3-legged token is cached it answers product reads as
				// well, and it returns account-specific pricing: the same query
				// under the two grants is two documents.
				user := cachedClient(t, dir, url, func(o *Options) {
					o.Tokens = &staticTokens{app: "app-tok", user: "user-tok", preferUser: true}
				})
				mustSearch(t, user, KeywordRequest{Keywords: "same"})
			},
			why: "account pricing differs by grant, so entries must not be shared across them",
		},
		{
			name: "different currency",
			call: func(t *testing.T, dir, url string) {
				usd := cachedClient(t, dir, url)
				mustSearch(t, usd, KeywordRequest{Keywords: "same"})

				eur := cachedClient(t, dir, url, func(o *Options) {
					o.Locale = Locale{Site: "DE", Language: "de", Currency: "EUR"}
				})
				mustSearch(t, eur, KeywordRequest{Keywords: "same"})
			},
			why: "serving a USD response to a EUR request would misprice a whole BOM",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, rec := cachingServer(t, cachedSearchBody)
			tc.call(t, t.TempDir(), url)
			if rec.count() != 2 {
				t.Errorf("server saw %d requests, want 2: %s", rec.count(), tc.why)
			}
		})
	}
}

// listPartsOrAdd answers a GET of a list's parts with an empty list and a POST
// to the same path with the ids AddParts expects.
func listPartsOrAdd(_ int, r *http.Request) (int, string) {
	if r.Method == http.MethodPost {
		return http.StatusOK, `["uid-1"]`
	}
	return http.StatusOK, `{"TotalParts":0,"PartsList":[]}`
}

func mustSearch(t *testing.T, c *Client, req KeywordRequest) {
	t.Helper()
	if _, err := c.KeywordSearch(context.Background(), req); err != nil {
		t.Fatalf("KeywordSearch(%q) error = %v", req.Keywords, err)
	}
}

func TestFailedResponsesAreNotCached(t *testing.T) {
	url, rec := statusServer(t, func(n int, _ *http.Request) (int, string) {
		if n == 1 {
			return http.StatusTooManyRequests, `{"ErrorMessage":"slow down"}`
		}
		return http.StatusOK, cachedSearchBody
	})
	dir := t.TempDir()
	req := KeywordRequest{Keywords: "0.1uF"}

	if _, err := cachedClient(t, dir, url).KeywordSearch(context.Background(), req); err == nil {
		t.Fatal("KeywordSearch() error = nil, want the 429 to surface")
	}
	// A cached rate-limit or expired-token response would outlive the condition
	// that produced it and turn a transient failure into a sticky one.
	if _, err := cachedClient(t, dir, url).KeywordSearch(context.Background(), req); err != nil {
		t.Fatalf("the retry after a 429 failed: %v", err)
	}
	if rec.count() != 2 {
		t.Errorf("server saw %d requests, want 2: the failure must not have been stored", rec.count())
	}
}

func TestUndecodableSuccessIsNotCached(t *testing.T) {
	url, rec := statusServer(t, func(n int, _ *http.Request) (int, string) {
		if n == 1 {
			return http.StatusOK, `<html><body>checking your browser</body></html>`
		}
		return http.StatusOK, cachedSearchBody
	})
	dir := t.TempDir()
	req := KeywordRequest{Keywords: "0.1uF"}

	if _, err := cachedClient(t, dir, url).KeywordSearch(context.Background(), req); err == nil {
		t.Fatal("KeywordSearch() error = nil, want the undecodable body to surface")
	}
	// A proxy interstitial or a WAF challenge served with a 200 is a failed
	// response wearing a successful status. Stored, it replays its decode error
	// from disk for the whole TTL and no retry can clear it — the same sticky
	// failure a cached 429 would be.
	if _, err := cachedClient(t, dir, url).KeywordSearch(context.Background(), req); err != nil {
		t.Fatalf("the retry after an undecodable 200 failed: %v", err)
	}
	if rec.count() != 2 {
		t.Errorf("server saw %d requests, want 2: a body that did not decode must not have been stored", rec.count())
	}
}

func TestListWriteInvalidatesWhenItsReplyNeverArrives(t *testing.T) {
	const parts = "/mylists/v1/lists/abc/parts"

	tests := []struct {
		name  string
		write func(http.ResponseWriter)
		why   string
	}{
		{
			name:  "server error",
			write: func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
			why:   "a 5xx can follow a mutation that already landed",
		},
		{
			name:  "connection dropped",
			write: func(http.ResponseWriter) { panic(http.ErrAbortHandler) },
			why:   "a lost reply says nothing about whether the write arrived",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{byPath: map[string]int{}}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				rec.note(r.URL.Path)
				if r.Method == http.MethodPost {
					tc.write(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"TotalParts":0,"PartsList":[]}`)
			}))
			t.Cleanup(srv.Close)

			ctx := context.Background()
			dir := t.TempDir()
			c := cachedClient(t, dir, srv.URL)
			if _, err := c.ListParts(ctx, "abc", 0, 0, Locale{}); err != nil {
				t.Fatalf("ListParts() error = %v", err)
			}
			if _, err := c.AddParts(ctx, "abc", []RequestedPart{{RequestedPartNumber: "490-1532-1-ND"}}); err == nil {
				t.Fatal("AddParts() error = nil, want the failed write to surface")
			}
			if _, err := cachedClient(t, dir, srv.URL).ListParts(ctx, "abc", 0, 0, Locale{}); err != nil {
				t.Fatalf("ListParts() after a failed write: %v", err)
			}

			// The scope is dropped on the attempt, not on the reply. A list
			// read left cached after a write that did land is the one
			// staleness this cache promises never to produce, and a failed
			// reply is no evidence that the write did not land.
			if got := rec.at(parts); got != 3 {
				t.Errorf("server saw %d calls to %s, want 3 (read, write, re-read): %s", got, parts, tc.why)
			}
		})
	}
}

func TestListWriteInvalidatesListReads(t *testing.T) {
	url, rec := statusServer(t, listPartsOrAdd)
	dir := t.TempDir()
	const parts = "/mylists/v1/lists/abc/parts"
	ctx := context.Background()

	c := cachedClient(t, dir, url)
	if _, err := c.ListParts(ctx, "abc", 0, 0, Locale{}); err != nil {
		t.Fatalf("ListParts() error = %v", err)
	}
	if _, err := c.AddParts(ctx, "abc", []RequestedPart{{RequestedPartNumber: "490-1532-1-ND"}}); err != nil {
		t.Fatalf("AddParts() error = %v", err)
	}
	if _, err := cachedClient(t, dir, url).ListParts(ctx, "abc", 0, 0, Locale{}); err != nil {
		t.Fatalf("ListParts() after a write: %v", err)
	}

	// One GET before the write, one after: `dk list show` must never report the
	// list as it stood before the `dk list add` that just ran.
	if got := rec.at(parts); got != 3 {
		t.Errorf("server saw %d calls to %s, want 3 (read, write, re-read)", got, parts)
	}
}

func TestListWriteLeavesCatalogEntriesAlone(t *testing.T) {
	url, rec := statusServer(t, func(n int, r *http.Request) (int, string) {
		if r.Method == http.MethodPost && r.URL.Path != "/products/v4/search/keyword" {
			return http.StatusOK, `["uid-1"]`
		}
		return http.StatusOK, cachedSearchBody
	})
	dir := t.TempDir()
	ctx := context.Background()
	req := KeywordRequest{Keywords: "0.1uF"}

	c := cachedClient(t, dir, url)
	mustSearch(t, c, req)
	if _, err := c.AddParts(ctx, "abc", []RequestedPart{{RequestedPartNumber: "x"}}); err != nil {
		t.Fatalf("AddParts() error = %v", err)
	}
	mustSearch(t, cachedClient(t, dir, url), req)

	// Scoping is what keeps the cache useful in the workflow it exists for:
	// search, add, search again. A write that dropped everything would make the
	// second search pay full price.
	if got := rec.at("/products/v4/search/keyword"); got != 1 {
		t.Errorf("server saw %d searches, want 1: a list write must not drop catalog entries", got)
	}
}

func TestCacheRefreshIgnoresEntriesButStillStoresThem(t *testing.T) {
	url, rec := cachingServer(t, cachedSearchBody)
	dir := t.TempDir()
	req := KeywordRequest{Keywords: "0.1uF"}

	refresh := func(o *Options) { o.CacheRefresh = true }
	mustSearch(t, cachedClient(t, dir, url, refresh), req)
	mustSearch(t, cachedClient(t, dir, url, refresh), req)

	if rec.count() != 2 {
		t.Errorf("server saw %d requests, want 2: --no-cache must not serve a stored entry", rec.count())
	}

	// The other half of what --no-cache promises: the fresh reply replaces the
	// stale entry, so the next ordinary read benefits from the refresh.
	mustSearch(t, cachedClient(t, dir, url), req)
	if rec.count() != 2 {
		t.Errorf("server saw %d requests, want 2: a refreshed response should have been stored", rec.count())
	}
}

func TestUncachedEndpointAlwaysAsks(t *testing.T) {
	url, rec := cachingServer(t, `"Bench PSU rev B"`)
	dir := t.TempDir()
	ctx := context.Background()

	c := cachedClient(t, dir, url)
	for range 2 {
		if _, err := c.SuggestListName(ctx, "Bench PSU rev A"); err != nil {
			t.Fatalf("SuggestListName() error = %v", err)
		}
	}

	// A GET, but not a cacheable one: it answers "is this name free right now",
	// and --auto-rename acts on the answer immediately.
	if rec.count() != 2 {
		t.Errorf("server saw %d requests, want 2: name validation must not be cached", rec.count())
	}
}

func TestClientWithoutACacheStillWorks(t *testing.T) {
	url, rec := cachingServer(t, cachedSearchBody)
	req := KeywordRequest{Keywords: "0.1uF"}

	none := func(o *Options) { o.Cache = nil }
	mustSearch(t, cachedClient(t, "", url, none), req)
	mustSearch(t, cachedClient(t, "", url, none), req)

	// Caching off has to mean every read reaches DigiKey, not that reads fail.
	if rec.count() != 2 {
		t.Errorf("server saw %d requests, want 2 with the cache disabled", rec.count())
	}
}
