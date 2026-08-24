package ouca

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTileCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tc, err := NewTileCache(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tc.Get("http://x/1"); ok {
		t.Fatal("expected cache miss")
	}
	if err := tc.Put("http://x/1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, ok := tc.Get("http://x/1")
	if !ok || string(data) != "hello" {
		t.Fatalf("bad cache hit: %q %v", data, ok)
	}
}

func TestTileCacheTTL(t *testing.T) {
	dir := t.TempDir()
	tc, err := NewTileCache(dir, -time.Hour) // negative disables expiry
	if err != nil {
		t.Fatal(err)
	}
	if err := tc.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, ok := tc.Get("k"); !ok {
		t.Fatal("negative ttl should disable expiry")
	}

	tc2, err := NewTileCache(dir, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok := tc2.Get("k"); ok {
		t.Fatal("entry should have expired")
	}
}

// TestTileCacheConcurrentWriters simulates multiple processes writing the
// same key at once: all writes must succeed and readers must never see a
// partial tile.
func TestTileCacheConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	const writers = 8
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tc, err := NewTileCache(dir, 0) // each "process" opens its own handle
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
				return
			}
			for j := range 50 {
				data := fmt.Appendf(nil, "payload-%d-%d", i, j)
				if err := tc.Put("shared", data); err != nil {
					t.Errorf("writer %d put: %v", i, err)
					return
				}
				if got, ok := tc.Get("shared"); ok && len(got) == 0 {
					t.Error("reader observed empty (partial) tile")
				}
			}
		}(i)
	}
	wg.Wait()

	// No temp files may leak.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("leaked temp file %s", e.Name())
		}
	}
}

func TestDefaultCacheDir(t *testing.T) {
	d, err := DefaultCacheDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if filepath.Base(d) != "ouca" {
		t.Fatalf("unexpected default cache dir %s", d)
	}
}

// TestReverseUsesCache serves one tile and verifies the second Reverse call
// is served from disk without an HTTP round trip.
func TestReverseUsesCache(t *testing.T) {
	hits := 0
	mvt := buildTestMVT() // minimal valid MVT with one road
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tilejson" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tiles":["%s/{z}/{x}/{y}.pbf"]}`, srv.URL)
			return
		}
		hits++
		w.Write(mvt)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	ix := NewIndex(
		WithProvider(srv.URL+"/tilejson"),
		WithCacheDir(cacheDir),
	)
	ctx := context.Background()

	if _, err := ix.Reverse(ctx, 52.5163, 13.3777); err != nil {
		t.Fatalf("first reverse: %v", err)
	}
	firstHits := hits
	if firstHits == 0 {
		t.Fatal("expected at least one network hit")
	}
	if _, err := ix.Reverse(ctx, 52.5163, 13.3777); err != nil {
		t.Fatalf("second reverse: %v", err)
	}
	if hits != firstHits {
		t.Fatalf("expected cached tiles to avoid network hits: %d -> %d", firstHits, hits)
	}

	// The cache files must exist in the requested dir.
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected cache files in %s: %v", cacheDir, err)
	}
}

func TestWithoutCache(t *testing.T) {
	ix := NewIndex(WithoutCache(), WithCacheDir(t.TempDir()))
	if ix.client.cache != nil {
		t.Fatal("cache should be disabled")
	}
}
