package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

const webSearchTimeout = 15 * time.Second

// WebSearchAdapter performs web searches using the Brave Search API
// or falls back to a simple DuckDuckGo HTML scrape.
type WebSearchAdapter struct{}

func (a *WebSearchAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	query := strings.TrimSpace(command)
	if query == "" {
		return SourceResult{Source: "web-search", Status: "empty", Summary: "empty query"}, nil
	}

	start := time.Now()

	// Try Brave Search API if key is available
	if key := os.Getenv("BRAVE_API_KEY"); key != "" {
		result, err := braveSearch(ctx, query, key)
		if err == nil {
			result.Duration = time.Since(start)
			return result, nil
		}
		// Fall through to DuckDuckGo on error
	}

	// Fallback: DuckDuckGo Instant Answer API (free, no key needed)
	result, err := ddgSearch(ctx, query)
	if err != nil {
		return SourceResult{
			Source:   "web-search",
			Status:   "error",
			Summary:  fmt.Sprintf("web search failed: %s", err),
			Duration: time.Since(start),
		}, nil
	}
	result.Duration = time.Since(start)
	return result, nil
}

func (a *WebSearchAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	return []Artifact{{Type: "page", ID: "web-result", Timestamp: time.Now().Format(time.RFC3339), Snippet: string(raw)}}, nil
}

func braveSearch(ctx context.Context, query, apiKey string) (SourceResult, error) {
	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	u := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=5", url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return SourceResult{}, err
	}
	req.Header.Set("X-Subscription-Token", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return SourceResult{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return SourceResult{}, fmt.Errorf("brave API status %d", resp.StatusCode)
	}

	var braveResp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &braveResp); err != nil {
		return SourceResult{}, err
	}

	retrievedAt := time.Now().Format(time.RFC3339)
	var artifacts []Artifact
	for i, r := range braveResp.Web.Results {
		artifacts = append(artifacts, Artifact{
			Type:      "page",
			ID:        fmt.Sprintf("result-%d", i+1),
			Timestamp: retrievedAt,
			Snippet:   fmt.Sprintf("[%s](%s)\n%s", r.Title, r.URL, r.Description),
		})
	}

	status := "success"
	if len(artifacts) == 0 {
		status = "empty"
	}

	return SourceResult{
		Source:    "web-search",
		Status:    status,
		Summary:   fmt.Sprintf("%d web results for: %s", len(artifacts), query),
		Artifacts: artifacts,
		Data:      body,
		Command:   query,
	}, nil
}

func ddgSearch(ctx context.Context, query string) (SourceResult, error) {
	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	u := fmt.Sprintf("https://api.duckduckgo.com/?q=%s&format=json&no_html=1", url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return SourceResult{}, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return SourceResult{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return SourceResult{}, fmt.Errorf("ddg API status %d", resp.StatusCode)
	}

	var ddgResp struct {
		Abstract      string `json:"Abstract"`
		AbstractURL   string `json:"AbstractURL"`
		Answer        string `json:"Answer"`
		RelatedTopics []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
		} `json:"RelatedTopics"`
	}
	if err := json.Unmarshal(body, &ddgResp); err != nil {
		return SourceResult{}, err
	}

	retrievedAt := time.Now().Format(time.RFC3339)
	var artifacts []Artifact

	// Instant answer
	if ddgResp.Answer != "" {
		artifacts = append(artifacts, Artifact{
			Type: "page", ID: "answer",
			Timestamp: retrievedAt,
			Snippet:   ddgResp.Answer,
		})
	}

	// Abstract
	if ddgResp.Abstract != "" {
		snippet := ddgResp.Abstract
		if ddgResp.AbstractURL != "" {
			snippet = fmt.Sprintf("%s\nSource: %s", snippet, ddgResp.AbstractURL)
		}
		artifacts = append(artifacts, Artifact{
			Type: "page", ID: "abstract",
			Timestamp: retrievedAt,
			Snippet:   snippet,
		})
	}

	// Related topics (max 5)
	for i, rt := range ddgResp.RelatedTopics {
		if i >= 5 || rt.Text == "" {
			break
		}
		snippet := rt.Text
		if rt.FirstURL != "" {
			snippet = fmt.Sprintf("%s\n%s", snippet, rt.FirstURL)
		}
		artifacts = append(artifacts, Artifact{
			Type: "page", ID: fmt.Sprintf("related-%d", i+1),
			Timestamp: retrievedAt,
			Snippet:   snippet,
		})
	}

	status := "success"
	if len(artifacts) == 0 {
		// DuckDuckGo instant answer API often returns empty for complex queries
		// Return a minimal result so synthesis knows we tried
		status = "empty"
		var buf bytes.Buffer
		buf.WriteString("No instant answer available. ")
		buf.WriteString("Try searching directly: https://duckduckgo.com/?q=")
		buf.WriteString(url.QueryEscape(query))
		artifacts = append(artifacts, Artifact{
			Type: "page", ID: "no-result",
			Timestamp: retrievedAt,
			Snippet:   buf.String(),
		})
	}

	return SourceResult{
		Source:    "web-search",
		Status:    status,
		Summary:   fmt.Sprintf("DuckDuckGo results for: %s", query),
		Artifacts: artifacts,
		Command:   query,
	}, nil
}

func init() {
	RegisterAdapter("web-search", &WebSearchAdapter{})
}
