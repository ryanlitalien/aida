package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	voyageAPI      = "https://api.voyageai.com/v1/embeddings"
	voyageModel    = "voyage-4-lite"
	voyageMaxBatch = 128
	// voyageOutputDimension pins Voyage 4's Matryoshka-learned output to
	// 512 -- the family defaults to 1024, but VectorDims (vectors.go) and
	// every stored blob assume 512. Explicit here rather than implicit
	// (voyage-3-lite's only option was 512, so earlier code never had to
	// ask): omitting this field would silently double every vector's size
	// and break DecodeVector against existing rows.
	voyageOutputDimension = 512
)

// EmbeddingClient generates text embeddings via Voyage AI.
type EmbeddingClient struct {
	apiKey string
	http   *http.Client
}

// NewEmbeddingClient creates a client. apiKeyEnv is the environment variable
// name holding the Voyage API key (e.g. "VOYAGE_API_KEY").
func NewEmbeddingClient(apiKeyEnv string) *EmbeddingClient {
	key := os.Getenv(apiKeyEnv)
	return &EmbeddingClient{
		apiKey: key,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Available reports whether the embedding client has an API key configured.
func (c *EmbeddingClient) Available() bool {
	return c.apiKey != ""
}

// EmbedQuery embeds a single query string (uses input_type "query").
func (c *EmbeddingClient) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.embed(ctx, []string{text}, "query")
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("voyage: empty response")
	}
	return vecs[0], nil
}

// EmbedDocument embeds a single string as a document (uses input_type
// "document", like EmbedDocuments). Voyage's "query" and "document" input
// types are asymmetric -- measured empirically, the SAME literal text
// embedded once as a query and once as a document lands at ~0.81 cosine
// similarity, not ~1.0. Comparing a query-type embedding against a
// document-type embedding is fine at the lenient 0.15/0.25 floors used
// elsewhere (RecallMemories, FindSimilarJarvisLessons) because those
// floors already tolerate a wide gap. It silently breaks a strict,
// verbatim-reuse comparison like the Tier-1 run_cache lookup, whose
// question_embedding column is populated via EmbedDocuments (mine.go) --
// so any code comparing against it must embed with this method, not
// EmbedQuery, or every match collapses toward a ~0.81 ceiling regardless
// of how similar the text actually is.
func (c *EmbeddingClient) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("voyage: empty response")
	}
	return vecs[0], nil
}

// EmbedDocuments embeds multiple documents (uses input_type "document").
// Automatically batches if len(texts) > voyageMaxBatch.
func (c *EmbeddingClient) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var all [][]float32
	for i := 0; i < len(texts); i += voyageMaxBatch {
		end := i + voyageMaxBatch
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := c.embed(ctx, texts[i:end], "document")
		if err != nil {
			return nil, fmt.Errorf("batch %d-%d: %w", i, end, err)
		}
		all = append(all, batch...)
	}
	return all, nil
}

type voyageRequest struct {
	Model           string   `json:"model"`
	Input           []string `json:"input"`
	InputType       string   `json:"input_type"`
	OutputDimension int      `json:"output_dimension"`
}

type voyageResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (c *EmbeddingClient) embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("voyage: no API key (set VOYAGE_API_KEY)")
	}

	body, err := json.Marshal(voyageRequest{
		Model:           voyageModel,
		Input:           texts,
		InputType:       inputType,
		OutputDimension: voyageOutputDimension,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", voyageAPI, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("voyage: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result voyageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("voyage: decode: %w", err)
	}

	vecs := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}
