package api

import (
	"net/http"

	"github.com/julienschmidt/httprouter"
	"telemetry.gosuda.org/telemetry/internal/types"
)

func IDzHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")

		id, err := is.GenerateIDString()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(id))
	}
}
