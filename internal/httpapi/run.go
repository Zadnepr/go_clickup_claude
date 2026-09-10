package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// ManualRunner — ручной запуск проверки: конкретной задачи (если передан
// task_id) или немедленное пересканирование доски по тегу/статусу (если
// task_id пуст) — та же логика, что и у периодической сверки. model/effort
// — переопределение на этот конкретный запуск (см. Требование «выбрать
// модель для текущей задачи»); пусто — использовать текущее значение по
// умолчанию. Применяются только при непустом taskID — для массового
// пересканирования переопределение не имеет смысла (см. newRunHandler).
type ManualRunner interface {
	RunNow(ctx context.Context, taskID, model, effort string) (submitted []string, err error)
}

type runRequest struct {
	TaskID string `json:"task_id"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
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
		req.Effort = strings.TrimSpace(req.Effort)
		if req.Effort != "" && !claudeEfforts[req.Effort] {
			http.Error(w, "effort must be one of: low, medium, high, xhigh, max", http.StatusBadRequest)
			return
		}

		submitted, err := deps.Trigger.RunNow(r.Context(), req.TaskID, strings.TrimSpace(req.Model), req.Effort)
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
