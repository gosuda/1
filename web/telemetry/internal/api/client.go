package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
	"gosuda.org/randflake"
	"telemetry.gosuda.org/telemetry/internal/types"
)

// ClientIdentity represents a registered client's credentials
type ClientIdentity struct {
	ID    string `json:"id"`    // Client's unique identifier
	Token string `json:"token"` // Authentication token for the client
}

// POST /client/register
func ClientRegisterHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")

		clientID, err := is.GenerateID()
		if err != nil {
			log.Error().Err(err).Msg("failed to generate client ID")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		token, err := is.GenerateIDString()
		if err != nil {
			log.Error().Err(err).Msg("failed to generate client token")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = is.ClientRegister(context.Background(), clientID, token)
		if err != nil {
			log.Error().Err(err).Msg("failed to register client")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		clientIdentity := ClientIdentity{
			ID:    randflake.EncodeString(clientID),
			Token: token,
		}
		log.Debug().
			Str("client_id", clientIdentity.ID).
			Str("client_token", clientIdentity.Token).
			Msg("Client Registered")

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(&clientIdentity)
	}
}

// POST /client/status
func ClientStatusHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		defer r.Body.Close()

		clientIdentity := ClientIdentity{}
		err := json.NewDecoder(r.Body).Decode(&clientIdentity)
		if err != nil {
			log.Error().Err(err).Msg("failed to decode client passport")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		log.Debug().
			Str("client_id", clientIdentity.ID).
			Str("client_token", clientIdentity.Token).
			Msg("Client Status Request Received")

		clientID, err := randflake.DecodeString(clientIdentity.ID)
		if err != nil {
			log.Debug().
				Str("client_id", clientIdentity.ID).
				Err(err).
				Msg("Failed to decode client ID")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		ok, err := is.ClientVerifyToken(context.Background(), clientID, clientIdentity.Token)
		if err != nil {
			log.Error().Err(err).Msg("failed to verify client token")
		}
		log.Debug().
			Bool("ok", ok).
			Msg("Client Token Verification")

		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"status": "unauthorized"})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// ClientPassport contains client authentication and environment details
type ClientPassport struct {
	ClientID      string `json:"client_id"`    // Unique client identifier
	ClientToken   string `json:"client_token"` // Authentication token
	ClientVersion string `json:"version"`      // Client software version
	FPVersion     int    `json:"fpv"`          // Fingerprint version
	Fingerprint   string `json:"fp"`           // Browser fingerprint hash
	UserAgent     string `json:"ua"`           // Raw User-Agent string
	UserAgentData string `json:"uad"`          // Structured User-Agent data (JSON)
}

// POST /client/checkin
func ClientCheckinHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		defer r.Body.Close()

		passport := ClientPassport{}
		err := json.NewDecoder(r.Body).Decode(&passport)
		if err != nil {
			log.Error().Err(err).Msg("failed to decode client passport")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		log.Debug().
			Str("client_id", passport.ClientID).
			Str("client_token", passport.ClientToken).
			Str("client_version", passport.ClientVersion).
			Int("fp_version", passport.FPVersion).
			Str("fingerprint", passport.Fingerprint).
			Str("user_agent", passport.UserAgent).
			Str("user_agent_data", passport.UserAgentData).
			Msg("Client Checkin Request Received")

		if passport.FPVersion != 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		clientID, err := randflake.DecodeString(passport.ClientID)
		if err != nil {
			log.Debug().
				Str("client_id", passport.ClientID).
				Err(err).
				Msg("Failed to decode client ID")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		ok, err := is.ClientVerifyToken(context.Background(), clientID, passport.ClientToken)
		if err != nil {
			log.Error().Err(err).Msg("failed to verify client token")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !ok {
			log.Debug().
				Str("client_id", passport.ClientID).
				Str("client_token", passport.ClientToken).
				Msg("Client token verification failed")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		fpid, err := is.GenerateID()
		if err != nil {
			log.Error().Err(err).Msg("failed to generate fingerprint ID")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = is.ClientRegisterFingerprint(
			context.Background(), fpid, clientID, passport.UserAgent, passport.UserAgentData, int32(passport.FPVersion), passport.Fingerprint)
		if err != nil {
			log.Error().Err(err).Msg("failed to register fingerprint")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
