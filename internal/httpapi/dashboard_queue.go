package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// activeRunView — одна "текущая задача" в ответе GET /api/queue: то, что
// обрабатывается прямо сейчас (обычно одна, если WORKER_CONCURRENCY=1),
// с прогрессом по этапам и расходом токенов в реальном времени (см.
// Store.UpdateRunningUsage).
type activeRunView struct {
	TaskID       string           `json:"task_id"`
	RunID        int64            `json:"run_id"`
	TaskName     string           `json:"task_name,omitempty"`
	TaskURL      string           `json:"task_url,omitempty"`
	Status       string           `json:"status"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	CostUSD      float64          `json:"cost_usd"`
	Stages       []store.RunStage `json:"stages"`
}

// newQueueHandler обрабатывает GET /api/queue: текущая(-ие) задача(-и) в
// обработке прямо сейчас — с прогрессом по этапам и живым расходом
// токенов, для управления (прервать/пауза) — и список задач, ожидающих
// своей очереди в буфере (см. Требование «список задач в очереди»).
func newQueueHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		active := make([]activeRunView, 0, len(deps.Queue.ActiveRuns()))

		for _, ar := range deps.Queue.ActiveRuns() {
			view := activeRunView{TaskID: ar.TaskID, RunID: ar.RunID}

			if run, err := deps.Store.GetRun(ctx, ar.RunID); err == nil {
				view.Status = run.Status
				view.InputTokens = run.InputTokens
				view.OutputTokens = run.OutputTokens
				view.CostUSD = run.CostUSD
			} else {
				deps.Logger.Warn("failed to load active run details", "run_id", ar.RunID, "error", err.Error())
			}

			if stages, err := deps.Store.ListStages(ctx, ar.RunID); err == nil {
				view.Stages = stages
			}

			if deps.ClickUp != nil {
				if task, err := deps.ClickUp.GetTask(ctx, ar.TaskID); err == nil {
					view.TaskName = task.Name
					view.TaskURL = task.URL
				}
			}

			active = append(active, view)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active":  active,
			"pending": deps.Queue.Pending(),
		})
	}
}
