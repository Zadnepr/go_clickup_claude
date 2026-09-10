package httpapi

import (
	"encoding/json"
	"net/http"
)

// taskAction — какое ручное действие над задачей запрошено (см.
// newTaskControlHandler): прервать (жёсткая остановка прямо сейчас),
// поставить на паузу (остановить на границе этапов, ждать явного
// продолжения) или продолжить ранее остановленный/прерванный прогон.
type taskAction int

const (
	taskActionCancel taskAction = iota
	taskActionPause
	taskActionResume
)

// newTaskControlHandler обрабатывает POST /api/tasks/{task_id}/{cancel,pause,resume}
// — управление текущей задачей из дашборда (см. Требование «текущая задача
// с возможностью её прервать, поставить на паузу, продолжить»).
func newTaskControlHandler(deps Deps, action taskAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := r.PathValue("task_id")
		if taskID == "" {
			http.Error(w, "task_id is required", http.StatusBadRequest)
			return
		}

		var ok bool
		switch action {
		case taskActionCancel:
			ok = deps.Queue.RequestCancel(taskID)
		case taskActionPause:
			ok = deps.Queue.RequestPause(taskID)
		case taskActionResume:
			// SubmitResume не проверяет, есть ли реально что возобновлять —
			// ReopenOrEnqueue сам решит: найдётся paused/interrupted прогон
			// по этому task_id — переоткроет его, не найдётся — ничего не
			// произойдёт на этапе TryEnqueue (см. Store.ReopenOrEnqueue).
			ok = deps.Queue.SubmitResume(taskID)
		}

		if !ok {
			http.Error(w, "task is not currently active", http.StatusConflict)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"task_id": taskID, "accepted": true})
	}
}
