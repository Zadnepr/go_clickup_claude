package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
)

// ManualRunner — ручной запуск проверки: конкретной задачи (если передан
// task_id) или немедленное пересканирование доски по тегу/статусу (если
// task_id пуст) — та же логика, что и у периодической сверки.
type ManualRunner interface {
	RunNow(ctx context.Context, taskID string) (submitted []string, err error)
}

type runRequest struct {
	TaskID string `json:"task_id"`
}

// newRunHandler обрабатывает POST /api/run. Постановка в очередь идёт через
// тот же Submit, что и вебхук/сверка — обычные условия (тег, статус, список,
// дедупликация) применяются как всегда: ручной запуск не обходит их.
func newRunHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req runRequest
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
		}

		submitted, err := deps.Trigger.RunNow(r.Context(), req.TaskID)
		if err != nil {
			deps.Logger.Error("manual run failed", "task_id", req.TaskID, "error", err.Error())
			http.Error(w, "manual run failed: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"submitted": submitted})
	}
}
