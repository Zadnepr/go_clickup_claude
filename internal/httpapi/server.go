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

	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// Submitter — то немногое от очереди, что нужно HTTP-слою.
type Submitter interface {
	Submit(taskID string) bool
}

// DataStore — то немногое от Store, что нужно HTTP-слою: /readyz, /api/status
// и /api/stats.
type DataStore interface {
	Ping(ctx context.Context) error
	ListActive(ctx context.Context) ([]store.Run, error)
	Stats(ctx context.Context, since time.Time) (store.Stats, error)
}

// Deps — зависимости HTTP-слоя.
type Deps struct {
	Queue         Submitter
	Trigger       ManualRunner // ручной запуск через POST /api/run
	WebhookSecret string       // пусто -> эндпоинт вебхука не регистрируется
	Store         DataStore
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

	mux.HandleFunc("POST /api/run", newRunHandler(deps))
	mux.HandleFunc("GET /api/status", newStatusHandler(deps))
	mux.HandleFunc("GET /api/stats", newStatsHandler(deps))

	return mux
}
