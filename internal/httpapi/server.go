// Package httpapi поднимает HTTP-эндпоинты сервиса: приём вебхука ClickUp,
// эндпоинты состояния /healthz и /readyz, JSON API для ручного управления
// и веб-дашборд (см. dashboard.go).
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/queue"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// Ключи в таблице settings (см. store.GetSetting/SetSetting) для модели и
// effort claude, изменяемых через PUT /api/config/model — main.go читает их
// при старте, чтобы применить сохранённое значение к Runner ещё до первого
// прогона (см. Требование «менять модель и effort в веб-интерфейсе»).
const (
	SettingClaudeModel  = "claude_model"
	SettingClaudeEffort = "claude_effort"
)

// Submitter — то немногое от очереди, что нужно вебхуку/сверке.
type Submitter interface {
	Submit(taskID string) bool
}

// QueueControl — то, что нужно ручному управлению из дашборда: текущая
// задача (прервать/поставить на паузу/продолжить) и список задач в буфере
// очереди. Встраивает Submitter — набор целиком реализует одна и та же
// *queue.Queue.
type QueueControl interface {
	Submitter
	ActiveRuns() []queue.ActiveRunInfo
	Pending() []string
	RequestCancel(taskID string) bool
	RequestPause(taskID string) bool
	SubmitResume(taskID string) bool
}

// DataStore — то немногое от Store, что нужно HTTP-слою: /readyz,
// /api/status, /api/stats, /api/runs/{id}, /api/invocations и сохранение
// настроек, изменённых через веб-интерфейс (см. /api/config/model).
type DataStore interface {
	Ping(ctx context.Context) error
	ListActive(ctx context.Context) ([]store.Run, error)
	Stats(ctx context.Context, since time.Time) (store.Stats, error)
	GetRun(ctx context.Context, runID int64) (*store.Run, error)
	ListStages(ctx context.Context, runID int64) ([]store.RunStage, error)
	ListInvocations(ctx context.Context, runID int64) ([]store.ClaudeInvocation, error)
	ListInvocationsSince(ctx context.Context, since time.Time) ([]store.ClaudeInvocation, error)
	SetSetting(ctx context.Context, key, value string) error
}

// ClickUpReader — то немногое от ClickUp API, что нужно дашборду для
// отображения текущей задачи, разрешения ID исполнителей в имя/аватар и
// списка остальных задач в колонке-триггере без нужного тега.
type ClickUpReader interface {
	GetTask(ctx context.Context, taskID string) (*clickup.Task, error)
	GetTeamMembers(ctx context.Context) ([]clickup.Member, error)
	ListTasksByStatus(ctx context.Context, listID, status string) ([]clickup.Task, error)
}

// RunnerControl — то, что нужно веб-интерфейсу, чтобы менять модель/effort
// claude на лету (см. Требование «менять модель и effort в веб-интерфейсе»),
// без перезапуска сервиса. Реализуется тем же *review.Runner, что и очередь
// использует для реальных вызовов claude — смена применяется сразу.
type RunnerControl interface {
	SetModelEffort(model, effort string)
	ModelEffort() (model, effort string)
}

// Deps — зависимости HTTP-слоя.
type Deps struct {
	Queue         QueueControl
	Trigger       ManualRunner // ручной запуск через POST /api/run
	WebhookSecret string       // пусто -> эндпоинт вебхука не регистрируется
	Store         DataStore
	ClickUp       ClickUpReader
	Runner        RunnerControl
	Cfg           *config.Config // для GET /api/config — секреты (токены) в ответ не идут
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
	mux.HandleFunc("GET /api/config", newConfigHandler(deps))
	mux.HandleFunc("PUT /api/config/model", newSetModelHandler(deps))
	mux.HandleFunc("GET /api/queue", newQueueHandler(deps))
	mux.HandleFunc("GET /api/runs/{id}", newRunDetailHandler(deps))
	mux.HandleFunc("GET /api/invocations", newInvocationsHandler(deps))
	mux.HandleFunc("POST /api/tasks/{task_id}/cancel", newTaskControlHandler(deps, taskActionCancel))
	mux.HandleFunc("POST /api/tasks/{task_id}/pause", newTaskControlHandler(deps, taskActionPause))
	mux.HandleFunc("POST /api/tasks/{task_id}/resume", newTaskControlHandler(deps, taskActionResume))

	// "/{$}", а не просто "/": голый "/" в net/http регистрируется как
	// подкаталог и матчит ЛЮБОЙ путь без более специфичного обработчика —
	// тогда, например, POST /webhook/clickup с выключенным вебхуком отвечал
	// бы 405 (путь "существует" для GET), а не ожидаемым 404. "/{$}" матчит
	// только точный путь "/".
	mux.HandleFunc("GET /{$}", newDashboardHandler())

	return mux
}
