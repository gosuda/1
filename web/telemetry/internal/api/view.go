package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
	"gosuda.org/randflake"
	"telemetry.gosuda.org/telemetry/internal/core"
	"telemetry.gosuda.org/telemetry/internal/types"
)

// ViewRequest represents a page view request with client credentials
type ViewRequest struct {
	ClientID    string `json:"client_id"`    // Client's unique identifier
	ClientToken string `json:"client_token"` // Authentication token for the client
	URL         string `json:"url"`          // URL being viewed
}

// ViewResponse represents the response to a page view request
type ViewResponse struct {
	Status string `json:"status"`
}

// POST /client/view
func ClientViewHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		defer r.Body.Close()

		viewRequest := ViewRequest{}
		err := json.NewDecoder(r.Body).Decode(&viewRequest)
		if err != nil {
			log.Error().Err(err).Msg("failed to decode view request")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid request body"}`))
			return
		}

		log.Debug().
			Str("client_id", viewRequest.ClientID).
			Str("client_token", viewRequest.ClientToken).
			Str("url", viewRequest.URL).
			Msg("View Request Received")

		// Normalize URL (host + pathname)
		normalizedURL, err := core.NormalizeURL(viewRequest.URL)
		if err != nil {
			log.Debug().
				Str("url", viewRequest.URL).
				Err(err).
				Msg("failed to normalize url")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid url"}`))
			return
		}

		// Verify client credentials
		clientID, err := randflake.DecodeString(viewRequest.ClientID)
		if err != nil {
			log.Debug().
				Str("client_id", viewRequest.ClientID).
				Err(err).
				Msg("Failed to decode client ID")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid client_id"}`))
			return
		}

		ok, err := is.ClientVerifyToken(context.Background(), clientID, viewRequest.ClientToken)
		if err != nil {
			log.Error().Err(err).Msg("failed to verify client token")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if !ok {
			log.Debug().
				Str("client_id", viewRequest.ClientID).
				Str("client_token", viewRequest.ClientToken).
				Msg("Client token verification failed")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"status":"unauthorized"}`))
			return
		}

		// Generate ID for the view
		viewID, err := is.GenerateID()
		if err != nil {
			log.Error().Err(err).Msg("failed to generate view ID")
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

		// Generate ID for view count (in case we need to create one)
		viewCountID, err := is.GenerateID()
		if err != nil {
			log.Error().Err(err).Msg("failed to generate view count ID")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Insert the view and update the count in a transaction
		err = is.ViewInsertWithCount(context.Background(), viewID, urlID, clientID, viewCountID)
		if err != nil {
			log.Error().Err(err).Msg("failed to insert view and update count")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}
}

// ViewCountResponse represents the response to a view count lookup request
type ViewCountResponse struct {
	URL   string `json:"url"`
	Count int64  `json:"count"`
}

// GET /view/count?url=<url>
func ViewCountHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=5, stale-while-revalidate=86400, must-revalidate")

		// Get URL parameter
		rawURL := r.URL.Query().Get("url")
		if rawURL == "" {
			log.Debug().Msg("URL parameter is required")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"url parameter is required"}`))
			return
		}

		normalizedURL, err := core.NormalizeURL(rawURL)
		if err != nil {
			log.Debug().
				Str("url", rawURL).
				Err(err).
				Msg("failed to normalize url")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid url"}`))
			return
		}

		log.Debug().
			Str("url", normalizedURL).
			Msg("View Count Request Received")

		// Look up URL
		urlRecord, err := is.UrlLookupByUrl(context.Background(), normalizedURL)
		if err != nil {
			log.Debug().
				Str("url", normalizedURL).
				Err(err).
				Msg("URL not found")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"URL not found"}`))
			return
		}

		// Look up view count
		viewCount, err := is.ViewCountLookup(context.Background(), urlRecord.ID)
		if err != nil {
			log.Debug().
				Str("url", normalizedURL).
				Int64("url_id", urlRecord.ID).
				Err(err).
				Msg("View count not found")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf(`{"url":"%s","count":0}`, normalizedURL)))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fmt.Sprintf(`{"url":"%s","count":%d}`, normalizedURL, viewCount.Count)))
	}
}
