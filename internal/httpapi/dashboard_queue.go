package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
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

// otherTaskView — одна задача из колонки-триггера без тега-триггера (см.
// Требование «список задач без тега для ручного запуска») — сверка её не
// возьмёт сама, но её можно запустить вручную через дашборд. Assignees —
// текущие исполнители задачи (см. Требование «отобразить аватарки того, кто
// назначен на задачи»), разрешённые в имя/аватар через ClickUp, если это
// удалось (см. loadMembersByID); если участник не нашёлся — отдаётся то,
// что есть (сам ID, без имени и аватара).
type otherTaskView struct {
	TaskID    string      `json:"task_id"`
	CustomID  string      `json:"custom_id,omitempty"`
	Name      string      `json:"name"`
	URL       string      `json:"url"`
	Assignees []memberRef `json:"assignees,omitempty"`
}

// newQueueHandler обрабатывает GET /api/queue: текущая(-ие) задача(-и) в
// обработке прямо сейчас — с прогрессом по этапам и живым расходом
// токенов, для управления (прервать/пауза) — список задач, ожидающих своей
// очереди в буфере (см. Требование «список задач в очереди»), и отдельно —
// остальные задачи в колонке-триггере, у которых просто нет тега-триггера
// (сверка их не подхватит сама, см. Требование «список задач без тега»).
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

		otherToCheck := []otherTaskView{}
		if deps.ClickUp != nil && deps.Cfg != nil && deps.Cfg.StatusTrigger != "" {
			tasks, err := deps.ClickUp.ListTasksByStatus(ctx, deps.Cfg.CUListID, deps.Cfg.StatusTrigger)
			if err != nil {
				deps.Logger.Warn("failed to list other tasks in the trigger status", "error", err.Error())
			} else {
				members := loadMembersByID(ctx, deps)
				wantTag := config.NormalizeStatus(deps.Cfg.TriggerTag)
				for _, task := range tasks {
					if taskHasTag(task.Tags, wantTag) {
						continue
					}
					otherToCheck = append(otherToCheck, otherTaskView{
						TaskID: task.ID, CustomID: task.CustomID, Name: task.Name, URL: task.URL,
						Assignees: resolveAssigneeRefs(task.Assignees, members),
					})
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active":         active,
			"pending":        deps.Queue.Pending(),
			"other_to_check": otherToCheck,
		})
	}
}

func taskHasTag(tags []string, wantNormalized string) bool {
	for _, t := range tags {
		if config.NormalizeStatus(t) == wantNormalized {
			return true
		}
	}
	return false
}

// resolveAssigneeRefs превращает ID исполнителей задачи в memberRef
// (имя+аватар, если участник нашёлся в members — см. loadMembersByID).
// Не найденный участник — не ошибка, отдаётся с одним только ID.
func resolveAssigneeRefs(ids []int, members map[int]clickup.Member) []memberRef {
	if len(ids) == 0 {
		return nil
	}
	refs := make([]memberRef, 0, len(ids))
	for _, id := range ids {
		if m, ok := members[id]; ok {
			refs = append(refs, memberRef{ID: id, Username: m.Username, Avatar: m.Avatar})
		} else {
			refs = append(refs, memberRef{ID: id})
		}
	}
	return refs
}
