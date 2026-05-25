package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"

	// Register decoders for the formats Immich serves previews in (JPEG or
	// WebP, depending on server config); GIF/PNG are included for completeness.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Client is a minimal Immich REST API client.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient returns a Client for the given Immich server. baseURL should be the
// server root (e.g. https://immich.example.com); the trailing slash is trimmed.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// asset is the subset of Immich's AssetResponseDto we care about. We pick
// images regardless of orientation, so dimensions aren't needed here.
type asset struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "IMAGE" or "VIDEO"
}

// albumResponse is the subset of Immich's AlbumResponseDto we care about.
type albumResponse struct {
	Assets []asset `json:"assets"`
}

func (c *Client) do(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/octet-stream, application/json")
	return c.http.Do(req)
}

// listImageAssets fetches the album and returns only its IMAGE-type assets.
func (c *Client) listImageAssets(ctx context.Context, albumID string) ([]asset, error) {
	resp, err := c.do(ctx, "/api/albums/"+albumID)
	if err != nil {
		return nil, fmt.Errorf("request album %s: %w", albumID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get album %s: %s", albumID, statusError(resp))
	}

	var album albumResponse
	if err := json.NewDecoder(resp.Body).Decode(&album); err != nil {
		return nil, fmt.Errorf("decode album %s: %w", albumID, err)
	}

	images := make([]asset, 0, len(album.Assets))
	for _, a := range album.Assets {
		if a.Type == "IMAGE" {
			images = append(images, a)
		}
	}
	return images, nil
}

// downloadPreview fetches and decodes the preview image for an asset. We use
// the preview (thumbnail) rather than the original because Immich bakes the
// camera's EXIF orientation into generated previews (so portrait shots come out
// upright), serves them as decodable JPEG/WebP even when the original is HEIC,
// and the endpoint only requires the asset.view permission. A preview is ~1440px
// — ample for a 400×480 half.
func (c *Client) downloadPreview(ctx context.Context, id string) (image.Image, error) {
	resp, err := c.do(ctx, "/api/assets/"+id+"/thumbnail?size=preview")
	if err != nil {
		return nil, fmt.Errorf("request asset %s: %w", id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download asset %s: %s", id, statusError(resp))
	}

	img, _, err := image.Decode(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decode asset %s: %w", id, err)
	}
	return img, nil
}

// statusError formats a non-2xx response into a concise error string,
// including a truncated body to aid debugging.
func statusError(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return resp.Status
	}
	return fmt.Sprintf("%s: %s", resp.Status, msg)
}
