package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
)

// interestingEvents — события, по которым условие триггера (тег + статус)
// может стать истинным; из пейлоада нужен только task_id, само условие
// перепроверяется отдельным запросом к API (см. Требование 2).
var interestingEvents = map[string]bool{
	"taskStatusUpdated": true,
	"taskTagUpdated":    true,
	"taskCreated":       true,
	"taskMoved":         true,
}

type webhookPayload struct {
	Event  string `json:"event"`
	TaskID string `json:"task_id"`
}

// newWebhookHandler обрабатывает POST /webhook/clickup. Отвечает 200
// немедленно после проверки подписи и постановки в очередь: вся работа —
// в фоне, чтобы не копить счётчик неудач вебхука на стороне ClickUp.
func newWebhookHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
		if err != nil {
			deps.Logger.Error("failed to read webhook body", "error", err.Error())
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		sig := r.Header.Get("X-Signature")
		if !clickup.VerifySignature(deps.WebhookSecret, body, sig) {
			deps.Logger.Warn("webhook signature verification failed")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var payload webhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			deps.Logger.Error("failed to parse webhook payload", "error", err.Error())
			// Подпись верна, но тело не разобралось — отвечаем 200, чтобы не
			// портить статистику здоровья вебхука; сверка подберёт задачу.
			w.WriteHeader(http.StatusOK)
			return
		}

		if payload.TaskID == "" || !interestingEvents[payload.Event] {
			w.WriteHeader(http.StatusOK)
			return
		}

		deps.Queue.Submit(payload.TaskID)
		w.WriteHeader(http.StatusOK)
	}
}
