package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	registry "github.com/moby/moby/api/types/registry"
)

const dockerHubSearchURL = "https://index.docker.io/v1/search"

// ErrRegistrySearch is returned when the registry search endpoint fails.
var ErrRegistrySearch = errors.New("image search: registry error")

// Search queries the Docker Hub registry for images matching the term.
func Search(ctx context.Context, term string, limit int) ([]registry.SearchResult, error) {
	if limit <= 0 {
		limit = 25
	}

	u := fmt.Sprintf("%s?q=%s&n=%d", dockerHubSearchURL, url.QueryEscape(term), limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("image search: build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image search: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

		return nil, fmt.Errorf("%w: %s: %s", ErrRegistrySearch, resp.Status, body)
	}

	var results registry.SearchResults

	if decErr := json.NewDecoder(resp.Body).Decode(&results); decErr != nil {
		return nil, fmt.Errorf("image search: decode: %w", decErr)
	}

	return results.Results, nil
}
