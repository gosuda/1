package api

import (
	"net/http"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
	"telemetry.gosuda.org/telemetry/internal/core"
	"telemetry.gosuda.org/telemetry/internal/types"
)

var _status_ok = []byte(`{"status":"ok"}`)
var _status_err = []byte(`{"status":"error"}`)

// HealthzHandler returns a handler function that handles health check requests
func HealthzHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")

		err := core.DoHealthCheck(is)
		if err != nil {
			log.Error().Err(err).Msg("health check failed")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write(_status_err)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write(_status_ok)
	}
}
