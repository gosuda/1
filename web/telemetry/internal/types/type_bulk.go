package types

// BulkCountsRequest represents a POST body containing a list of normalized URLs
type BulkCountsRequest struct {
	Urls []string `json:"urls"`
}

// BulkCountEntry represents counts for a single URL
type BulkCountEntry struct {
	URL       string `json:"url"`
	ViewCount int64  `json:"view_count"`
	LikeCount int64  `json:"like_count"`
}

// BulkCountsResponse is returned by the bulk counts API
type BulkCountsResponse struct {
	Results []BulkCountEntry `json:"results"`
}
