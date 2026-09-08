// Package httpapi поднимает HTTP-эндпоинты сервиса: приём вебхука ClickUp
// и эндпоинты состояния /healthz и /readyz. Вся содержательная обработка
// задачи делегируется очереди (internal/queue) — здесь только сантехника
// разбора запроса и постановки в очередь.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Submitter — то немногое от очереди, что нужно HTTP-слою.
type Submitter interface {
	Submit(taskID string) bool
}

// Pinger проверяет доступность зависимости (используется в /readyz).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps — зависимости HTTP-слоя.
type Deps struct {
	Queue         Submitter
	WebhookSecret string // пусто -> эндпоинт вебхука не регистрируется
	Store         Pinger
	RepoPath      string
	ClaudeBinary  string // по умолчанию "claude"
	Logger        *slog.Logger
	ReadyzTimeout time.Duration
}

// NewMux собирает http.ServeMux со всеми эндпоинтами сервиса.
func NewMux(deps Deps) *http.ServeMux {
	if deps.ClaudeBinary == "" {
		deps.ClaudeBinary = "claude"
	}
	if deps.ReadyzTimeout <= 0 {
		deps.ReadyzTimeout = 5 * time.Second
	}

	mux := http.NewServeMux()

	if deps.WebhookSecret != "" {
		mux.HandleFunc("POST /webhook/clickup", newWebhookHandler(deps))
	} else {
		deps.Logger.Warn("CU_WEBHOOK_SECRET is not set: /webhook/clickup is disabled, running on reconcile only")
	}

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", newReadyzHandler(deps))

	return mux
}
