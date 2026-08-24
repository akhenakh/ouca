package ouca

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akhenakh/mvtgo"
)

const (
	DefaultProvider = "https://tiles.openfreemap.org/planet"
	DefaultZoom     = 14
)

// tileClient resolves the provider TileJSON and downloads vector tiles.
type tileClient struct {
	http    *http.Client
	tileURL string // resolved {z}/{x}/{y}.pbf template
}

func newTileClient(provider string, timeout time.Duration) *tileClient {
	if !strings.Contains(provider, "{z}") {
		provider = strings.TrimRight(provider, "/") + "/{z}/{x}/{y}.pbf"
	}
	return &tileClient{
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 8,
			},
		},
		tileURL: provider,
	}
}

// resolveTileJSON follows the OpenMapTiles convention where the provider URL
// points to a TileJSON describing the real tile template
// (e.g. https://tiles.openfreemap.org/planet -> .../planet/20240101_pt/{z}/{x}/{y}.pbf).
func (c *tileClient) resolveTileJSON(ctx context.Context) error {
	u := strings.TrimSuffix(c.tileURL, ".pbf")
	u = strings.TrimSuffix(u, "/{z}/{x}/{y}")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building tilejson request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetching tilejson %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tilejson %s: status %d", u, resp.StatusCode)
	}
	var tj struct {
		Tiles []string `json:"tiles"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err := json.Unmarshal(body, &tj); err != nil || len(tj.Tiles) == 0 {
		// Not a TileJSON; assume the original URL is already a template.
		return nil
	}
	tpl := tj.Tiles[0]
	if _, err := url.Parse(tpl); err != nil {
		return fmt.Errorf("invalid tile template %q: %w", tpl, err)
	}
	c.tileURL = tpl
	return nil
}

// fetchTile downloads and decodes a single vector tile.
func (c *tileClient) fetchTile(ctx context.Context, z, x, y int) ([]mvtgo.Layer, error) {
	u := strings.NewReplacer(
		"{z}", fmt.Sprint(z),
		"{x}", fmt.Sprint(x),
		"{y}", fmt.Sprint(y),
	).Replace(c.tileURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building tile request: %w", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "ouca/0.1 (+https://github.com/akhenakh/ouca)")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching tile %d/%d/%d: %w", z, x, y, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusNoContent:
		return nil, nil // empty tile is not an error
	default:
		return nil, fmt.Errorf("tile %d/%d/%d: status %d", z, x, y, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading tile %d/%d/%d: %w", z, x, y, err)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "gzip") || hasGzipMagic(data) {
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gunzipping tile %d/%d/%d: %w", z, x, y, err)
		}
		data, err = io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("gunzipping tile %d/%d/%d: %w", z, x, y, err)
		}
	}
	if len(data) == 0 {
		return nil, nil
	}
	layers, err := mvtgo.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decoding tile %d/%d/%d: %w", z, x, y, err)
	}
	return layers, nil
}

func hasGzipMagic(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}
