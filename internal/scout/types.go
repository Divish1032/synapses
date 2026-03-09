// Package scout is a fail-silent HTTP client for the synapses-scout sidecar.
// Every method returns zero values on failure — callers never need to handle
// scout errors, and the graph-only path always degrades gracefully.
package scout

// SearchRequest is the body sent to POST /v1/search.
type SearchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
	Region     string `json:"region,omitempty"`
	Timelimit  string `json:"timelimit,omitempty"`
}

// SearchHit is one search result entry.
type SearchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// SearchResponse is returned by POST /v1/search.
type SearchResponse struct {
	Query string      `json:"query"`
	Hits  []SearchHit `json:"hits"`
	Count int         `json:"count"`
}

// FetchRequest is the body sent to POST /v1/fetch.
// Input may be a URL or a plain-text query (scout auto-routes).
type FetchRequest struct {
	Input        string `json:"input"`
	ForceRefresh bool   `json:"force_refresh,omitempty"`
	Distill      *bool  `json:"distill,omitempty"`
	Region       string `json:"region,omitempty"`
	Timelimit    string `json:"timelimit,omitempty"`
	MaxResults   int    `json:"max_results,omitempty"`
}

// FetchResponse is returned by POST /v1/fetch (mirrors ScoutResult).
type FetchResponse struct {
	URL         string                 `json:"url"`
	ContentType string                 `json:"content_type"`
	Title       string                 `json:"title"`
	ContentMD   string                 `json:"content_md"`
	WordCount   int                    `json:"word_count"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	Fragment    *ScoutFragment         `json:"fragment,omitempty"`
	Cached      bool                   `json:"cached"`
	FetchedAt   string                 `json:"fetched_at"`
}

// ScoutFragment is the distilled LLM summary attached to a FetchResponse.
type ScoutFragment struct {
	Summary     string   `json:"summary"`
	Tags        []string `json:"tags"`
	DistilledBy string   `json:"distilled_by"`
}

// DeepSearchRequest is the body sent to POST /v1/deep-search.
type DeepSearchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
	Region     string `json:"region,omitempty"`
	Timelimit  string `json:"timelimit,omitempty"`
	Expand     *bool  `json:"expand,omitempty"`
}

// DeepSearchResponse is returned by POST /v1/deep-search.
type DeepSearchResponse struct {
	Query             string      `json:"query"`
	ExpandedQueries   []string    `json:"expanded_queries"`
	Hits              []SearchHit `json:"hits"`
	Count             int         `json:"count"`
	TotalRawHits      int         `json:"total_raw_hits"`
	DeduplicatedCount int         `json:"deduplicated"`
}

// LookupDocsRequest is the body sent to POST /v1/lookup-docs.
type LookupDocsRequest struct {
	Query    string `json:"query"`
	MaxChars int    `json:"max_chars,omitempty"`
}

// LookupDocsResponse is returned by POST /v1/lookup-docs.
type LookupDocsResponse struct {
	Query      string      `json:"query"`
	SourceURL  string      `json:"source_url"`
	Title      string      `json:"title"`
	Content    string      `json:"content"`
	Truncated  bool        `json:"truncated"`
	Cached     bool        `json:"cached"`
	Note       string      `json:"note,omitempty"`
	Error      string      `json:"error,omitempty"`
	SearchHits []SearchHit `json:"search_hits,omitempty"`
}

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Status                string      `json:"status"`
	Version               string      `json:"version"`
	IntelligenceAvailable bool        `json:"intelligence_available"`
	Cache                 interface{} `json:"cache,omitempty"`
}
