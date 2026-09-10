package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
)

// memberRef — короткая ссылка на пользователя ClickUp для отображения в
// дашборде (аватар/имя рядом с ID). Если участник не нашёлся (токен не
// настроен, ID не резолвится) — отдаётся то, что есть (сам ID).
type memberRef struct {
	ID       int    `json:"id"`
	Username string `json:"username,omitempty"`
	Avatar   string `json:"avatar,omitempty"`
}

// newConfigHandler обрабатывает GET /api/config — текущие настройки
// сервиса в виде, удобном для отображения (без токенов/секретов):
// условие триггера, переходы статусов, приоритет исполнителей, модель и
// уровень усилий claude и т.д. ID исполнителей, где возможно, разрешаются
// в имя и аватар через ClickUp.
func newConfigHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := deps.Cfg
		if cfg == nil {
			http.Error(w, "config not available", http.StatusInternalServerError)
			return
		}

		members := loadMembersByID(r.Context(), deps)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"trigger_tag":        cfg.TriggerTag,
			"status_trigger":     cfg.StatusTrigger,
			"status_running":     cfg.StatusRunning,
			"status_pass":        cfg.StatusPass,
			"status_fail":        cfg.StatusFail,
			"assignee_on_fail":   resolveAssignee(cfg.AssigneeOnFail, members),
			"assignee_on_pass":   resolveAssignee(cfg.AssigneeOnPass, members),
			"worker_concurrency": cfg.WorkerConcurrency,
			"review_timeout":     cfg.ReviewTimeout.String(),
			"reconcile_interval": cfg.ReconcileInterval.String(),
			"usage_limit_pause":  cfg.UsageLimitPause.String(),
			"claude_model":       cfg.ClaudeModel,
			"claude_effort":      cfg.ClaudeEffort,
			"repo_path":          cfg.RepoPath,
			"cu_list_id":         cfg.CUListID,
			"cu_team_id":         cfg.CUTeamID,
			"webhook_enabled":    deps.WebhookSecret != "",
			"port":               cfg.Port,
		})
	}
}

// loadMembersByID отдаёт участников воркспейса, проиндексированных по ID —
// используется и для /api/config (ASSIGNEE_ON_*), и для /api/queue (текущий
// исполнитель задачи). Ошибку (например, ClickUp недоступен) не считает
// фатальной для всей страницы — просто аватары/имена не резолвятся.
func loadMembersByID(ctx context.Context, deps Deps) map[int]clickup.Member {
	if deps.ClickUp == nil {
		return nil
	}
	members, err := deps.ClickUp.GetTeamMembers(ctx)
	if err != nil {
		deps.Logger.Warn("failed to load clickup team members for display", "error", err.Error())
		return nil
	}
	byID := make(map[int]clickup.Member, len(members))
	for _, m := range members {
		byID[m.ID] = m
	}
	return byID
}

// resolveAssignee превращает строковый ID из конфигурации (ASSIGNEE_ON_FAIL/
// ASSIGNEE_ON_PASS) в memberRef, если это число и участник нашёлся. Пустая
// строка (не задано) -> nil.
func resolveAssignee(idStr string, members map[int]clickup.Member) *memberRef {
	if idStr == "" {
		return nil
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return nil
	}
	if m, ok := members[id]; ok {
		return &memberRef{ID: id, Username: m.Username, Avatar: m.Avatar}
	}
	return &memberRef{ID: id}
}
