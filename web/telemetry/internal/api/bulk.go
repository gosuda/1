package api

import (
	"encoding/json"
	"net/http"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
	"telemetry.gosuda.org/telemetry/internal/core"
	"telemetry.gosuda.org/telemetry/internal/types"
)

// POST /counts/bulk
func BulkCountsHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=5, stale-while-revalidate=86400, must-revalidate")
		defer r.Body.Close()

		var req types.BulkCountsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Error().Err(err).Msg("failed to decode bulk counts request")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
			return
		}

		if len(req.Urls) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "urls list is required"})
			return
		}

		// Normalize and deduplicate URLs while preserving first-seen order
		// Also store a mapping from normalized URL back to original URL
		normalizedUrls := make([]string, 0, len(req.Urls))
		originalToNormalizedMap := make(map[string]string) // normalized URL -> original URL
		seenNormalized := make(map[string]struct{})

		for _, originalUrl := range req.Urls {
			n, err := core.NormalizeURL(originalUrl)
			if err != nil {
				log.Debug().Str("url", originalUrl).Err(err).Msg("failed to normalize url")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid url in list"})
				return
			}
			if _, ok := seenNormalized[n]; !ok {
				seenNormalized[n] = struct{}{}
				normalizedUrls = append(normalizedUrls, n)
				originalToNormalizedMap[n] = originalUrl
			}
		}

		rows, err := is.BulkCountsByUrls(r.Context(), normalizedUrls)
		if err != nil {
			log.Error().Err(err).Msg("failed to query bulk counts")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Build a map with default zero values so missing URLs return 0 counts
		resultsMap := make(map[string]types.BulkCountEntry, len(normalizedUrls))
		for _, nUrl := range normalizedUrls {
			resultsMap[nUrl] = types.BulkCountEntry{
				URL:       originalToNormalizedMap[nUrl], // Use original URL for the response
				ViewCount: 0,
				LikeCount: 0,
			}
		}
		for _, rr := range rows {
			if originalUrl, ok := originalToNormalizedMap[rr.URL]; ok {
				// Update with actual counts from DB, ensuring the original URL is retained
				resultsMap[rr.URL] = types.BulkCountEntry{
					URL:       originalUrl,
					ViewCount: rr.ViewCount,
					LikeCount: rr.LikeCount,
				}
			}
		}

		// Preserve original request order in response, using original URLs
		resp := types.BulkCountsResponse{
			Results: make([]types.BulkCountEntry, 0, len(req.Urls)),
		}
		// Iterate through the original request URLs to maintain order
		for _, originalUrl := range req.Urls {
			normalizedUrl, err := core.NormalizeURL(originalUrl)
			if err != nil {
				// This should ideally not happen again if it passed earlier validation
				log.Debug().Str("url", originalUrl).Err(err).Msg("failed to re-normalize url for response building")
				continue // Skip this URL if normalization fails here
			}
			if entry, ok := resultsMap[normalizedUrl]; ok {
				resp.Results = append(resp.Results, entry)
			}
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}
}
