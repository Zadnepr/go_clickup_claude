package httpapi

import (
	"encoding/json"
	"net/http"
	"time"
)

// newStatsHandler обрабатывает GET /api/stats?window=hour|day|all —
// агрегированная статистика и лог прогонов за период, включая потраченные
// токены и стоимость по каждой задаче и суммарно.
func newStatsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		windowParam := r.URL.Query().Get("window")
		since, windowName := resolveStatsWindow(windowParam)

		stats, err := deps.Store.Stats(r.Context(), since)
		if err != nil {
			deps.Logger.Error("failed to compute stats", "error", err.Error())
			http.Error(w, "failed to compute stats", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"window":     windowName,
			"since":      since,
			"total_runs": stats.TotalRuns,
			"by_status":  stats.ByStatus,
			"by_verdict": stats.ByVerdict,
			"tokens":     stats.Tokens,
			"runs":       stats.Runs,
		})
	}
}

func resolveStatsWindow(window string) (since time.Time, name string) {
	switch window {
	case "hour":
		return time.Now().Add(-time.Hour), "hour"
	case "day":
		return time.Now().Add(-24 * time.Hour), "day"
	default:
		return time.Time{}, "all"
	}
}
