package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

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

		// Модель/effort — из живого Runner (см. RunnerControl), а не из
		// стартовой конфигурации: их можно поменять на лету через
		// PUT /api/config/model, не перезапуская сервис (см. Требование
		// «менять модель и effort в веб-интерфейсе»), и cfg.ClaudeModel/
		// ClaudeEffort к этому моменту могут уже не отражать реальность.
		model, effort := cfg.ClaudeModel, cfg.ClaudeEffort
		if deps.Runner != nil {
			model, effort = deps.Runner.ModelEffort()
		}

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
			"claude_model":       model,
			"claude_effort":      effort,
			"repo_path":          cfg.RepoPath,
			"cu_list_id":         cfg.CUListID,
			"cu_team_id":         cfg.CUTeamID,
			"webhook_enabled":    deps.WebhookSecret != "",
			"port":               cfg.Port,
		})
	}
}

// claudeEfforts — допустимые значения флага --effort claude (см.
// review.CallOptions/Runner.SetModelEffort). Используется только для
// валидации ввода из веб-интерфейса — сам Runner ничего не проверяет,
// просто передаёт effort в claude как есть.
var claudeEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

type setModelRequest struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// newSetModelHandler обрабатывает PUT /api/config/model — меняет модель/
// effort claude по умолчанию на лету (см. Требование «менять модель и
// effort в веб-интерфейсе»): сохраняет в БД (переживает перезапуск) и сразу
// применяет к живому Runner (см. RunnerControl), без перезапуска сервиса.
func newSetModelHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req setModelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		req.Model = strings.TrimSpace(req.Model)
		req.Effort = strings.TrimSpace(req.Effort)
		if req.Model == "" {
			http.Error(w, "model is required", http.StatusBadRequest)
			return
		}
		if req.Effort != "" && !claudeEfforts[req.Effort] {
			http.Error(w, "effort must be one of: low, medium, high, xhigh, max", http.StatusBadRequest)
			return
		}
		if deps.Runner == nil {
			http.Error(w, "runner control not available", http.StatusInternalServerError)
			return
		}

		if err := deps.Store.SetSetting(r.Context(), SettingClaudeModel, req.Model); err != nil {
			deps.Logger.Error("failed to persist claude_model setting", "error", err.Error())
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
		if err := deps.Store.SetSetting(r.Context(), SettingClaudeEffort, req.Effort); err != nil {
			deps.Logger.Error("failed to persist claude_effort setting", "error", err.Error())
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
		deps.Runner.SetModelEffort(req.Model, req.Effort)
		deps.Logger.Info("claude model/effort changed via web interface", "model", req.Model, "effort", req.Effort)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "effort": req.Effort})
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
