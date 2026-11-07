package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
	"gosuda.org/randflake"
	"telemetry.gosuda.org/telemetry/internal/core"
	"telemetry.gosuda.org/telemetry/internal/types"
)

// LikeRequest represents a like request with client credentials
type LikeRequest struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	URL         string `json:"url"`
}

// LikeResponse represents a generic response for like operations
type LikeResponse struct {
	Status string `json:"status"`
}

// POST /client/like
func ClientLikeHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		defer r.Body.Close()

		likeRequest := LikeRequest{}
		err := json.NewDecoder(r.Body).Decode(&likeRequest)
		if err != nil {
			log.Error().Err(err).Msg("failed to decode like request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		log.Debug().
			Str("client_id", likeRequest.ClientID).
			Str("client_token", likeRequest.ClientToken).
			Str("url", likeRequest.URL).
			Msg("Like Request Received")

		// Normalize URL (host + pathname)
		normalizedURL, err := core.NormalizeURL(likeRequest.URL)
		if err != nil {
			log.Debug().
				Str("url", likeRequest.URL).
				Err(err).
				Msg("failed to normalize url")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid url"})
			return
		}

		// Verify client credentials
		clientID, err := randflake.DecodeString(likeRequest.ClientID)
		if err != nil {
			log.Debug().
				Str("client_id", likeRequest.ClientID).
				Err(err).
				Msg("Failed to decode client ID")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		ok, err := is.ClientVerifyToken(context.Background(), clientID, likeRequest.ClientToken)
		if err != nil {
			log.Error().Err(err).Msg("failed to verify client token")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if !ok {
			log.Debug().
				Str("client_id", likeRequest.ClientID).
				Str("client_token", likeRequest.ClientToken).
				Msg("Client token verification failed")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(LikeResponse{Status: "unauthorized"})
			return
		}

		// Generate ID for the like
		likeID, err := is.GenerateID()
		if err != nil {
			log.Error().Err(err).Msg("failed to generate like ID")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Look up or create URL
		var urlID int64
		urlRecord, err := is.UrlLookupByUrl(context.Background(), normalizedURL)
		if err != nil {
			// URL doesn't exist, create it
			urlID, err = is.GenerateID()
			if err != nil {
				log.Error().Err(err).Msg("failed to generate URL ID")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			err = is.UrlInsert(context.Background(), urlID, normalizedURL)
			if err != nil {
				log.Error().Err(err).Msg("failed to insert URL")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		} else {
			// URL exists, use its ID
			urlID = urlRecord.ID
		}

		// Generate ID for like count (in case we need to create one)
		likeCountID, err := is.GenerateID()
		if err != nil {
			log.Error().Err(err).Msg("failed to generate like count ID")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Insert the like and update the count in a transaction
		err = is.LikeInsertWithCount(context.Background(), likeID, urlID, clientID, likeCountID)
		if err != nil {
			log.Error().Err(err).Msg("failed to insert like and update count")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(LikeResponse{Status: "ok"})
	}
}

// LikeCountResponse represents the response to a like count lookup request
type LikeCountResponse struct {
	URL   string `json:"url"`
	Count int64  `json:"count"`
}

// GET /like/count?url=<url>
func LikeCountHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=5, stale-while-revalidate=86400, must-revalidate")

		// Get URL parameter
		rawURL := r.URL.Query().Get("url")
		if rawURL == "" {
			log.Debug().Msg("URL parameter is required")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "url parameter is required"})
			return
		}

		normalizedURL, err := core.NormalizeURL(rawURL)
		if err != nil {
			log.Debug().
				Str("url", rawURL).
				Err(err).
				Msg("failed to normalize url")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid url"})
			return
		}

		log.Debug().
			Str("url", normalizedURL).
			Msg("Like Count Request Received")

		// Look up URL
		urlRecord, err := is.UrlLookupByUrl(context.Background(), normalizedURL)
		if err != nil {
			log.Debug().
				Str("url", normalizedURL).
				Err(err).
				Msg("URL not found")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "URL not found"})
			return
		}

		// Look up like count
		likeCount, err := is.LikeCountLookup(context.Background(), urlRecord.ID)
		if err != nil {
			log.Debug().
				Str("url", normalizedURL).
				Int64("url_id", urlRecord.ID).
				Err(err).
				Msg("Like count not found")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(LikeCountResponse{
				URL:   normalizedURL,
				Count: 0,
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(LikeCountResponse{
			URL:   normalizedURL,
			Count: likeCount.Count,
		})
	}
}
