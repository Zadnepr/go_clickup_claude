package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// newRunDetailHandler обрабатывает GET /api/runs/{id} — детали одного
// прогона: сам прогон (статус, вердикт, суммарные токены), этапы
// (run_stages) и полный лог вызовов claude по каждой сессии (см.
// Требование «кол-во потраченных токенов по каждой сессии»).
func newRunDetailHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid run id", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		run, err := deps.Store.GetRun(ctx, id)
		if err != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}

		stages, err := deps.Store.ListStages(ctx, id)
		if err != nil {
			deps.Logger.Error("failed to list run stages", "run_id", id, "error", err.Error())
			http.Error(w, "failed to load run stages", http.StatusInternalServerError)
			return
		}

		invocations, err := deps.Store.ListInvocations(ctx, id)
		if err != nil {
			deps.Logger.Error("failed to list run invocations", "run_id", id, "error", err.Error())
			http.Error(w, "failed to load run invocations", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"run":         run,
			"stages":      stages,
			"invocations": invocations,
		})
	}
}
