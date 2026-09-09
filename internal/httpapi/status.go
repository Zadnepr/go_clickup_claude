package httpapi

import (
	"encoding/json"
	"net/http"
)

// newStatusHandler обрабатывает GET /api/status — список прогонов, которые
// сейчас в очереди или выполняются.
func newStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		active, err := deps.Store.ListActive(r.Context())
		if err != nil {
			deps.Logger.Error("failed to list active runs", "error", err.Error())
			http.Error(w, "failed to list active runs", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active": active,
			"count":  len(active),
		})
	}
}
