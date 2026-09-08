package queue

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/review"
	"github.com/Zadnepr/go_clickup_claude/internal/slack"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// ClickUp — часть API ClickUp, нужная воркеру очереди. Позволяет подменять
// реальный клиент фейком в тестах.
type ClickUp interface {
	GetTask(ctx context.Context, taskID string) (*clickup.Task, error)
	SetStatus(ctx context.Context, taskID, status string) error
	AddAssignees(ctx context.Context, taskID string, userIDs []int) error
	AddComment(ctx context.Context, taskID, text string) error
}

// Runner — часть API запуска claude, нужная воркеру очереди.
type Runner interface {
	GitFetch(ctx context.Context) error
	RunSpec(ctx context.Context, taskURL string) (sessionID string, err error)
	RunReview(ctx context.Context, taskURL, specPath string) (review.Result, error)
	SpecFilePath(taskID string) string
}

// Deps — зависимости, нужные очереди для обработки задач.
type Deps struct {
	ClickUp ClickUp
	Store   *store.Store
	Slack   *slack.Notifier
	Runner  Runner
	Cfg     *config.Config
	Logger  *slog.Logger
}

// isEligible проверяет условие «задача берётся в работу» (Требование 2) по
// свежей карточке задачи, полученной через GetTask, а не по данным события.
func isEligible(task *clickup.Task, cfg *config.Config) bool {
	if task.ListID != cfg.CUListID {
		return false
	}

	wantTag := config.NormalizeStatus(cfg.TriggerTag)
	hasTag := false
	for _, tag := range task.Tags {
		if config.NormalizeStatus(tag) == wantTag {
			hasTag = true
			break
		}
	}
	if !hasTag {
		return false
	}

	return config.NormalizeStatus(task.Status) == config.NormalizeStatus(cfg.StatusTrigger)
}

// processTask прогоняет одну задачу через полный цикл: проверка условия,
// дедупликация, перевод в running, /spec+/review, переходы статуса и
// исполнителя, комментарий, уведомление в Slack.
func (q *Queue) processTask(taskID string) {
	cfg := q.deps.Cfg
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ReviewTimeout)
	defer cancel()

	log := q.deps.Logger.With("task_id", taskID)

	task, err := q.deps.ClickUp.GetTask(ctx, taskID)
	if err != nil {
		log.Error("failed to fetch task, will retry on next event or reconcile", "error", err.Error())
		return
	}

	if !isEligible(task, cfg) {
		log.Debug("task does not meet trigger condition, skipping without a dedup record")
		return
	}

	runID, enqueued, err := q.deps.Store.TryEnqueue(ctx, taskID)
	if err != nil {
		log.Error("failed to record run in store", "error", err.Error())
		return
	}
	if !enqueued {
		log.Debug("task already has an active or completed run, skipping")
		return
	}

	q.runReview(ctx, log, task, runID)
}

func (q *Queue) runReview(ctx context.Context, log *slog.Logger, task *clickup.Task, runID int64) {
	cfg := q.deps.Cfg

	if cfg.StatusRunning != "" {
		if err := q.deps.ClickUp.SetStatus(ctx, task.ID, cfg.StatusRunning); err != nil {
			log.Error("failed to move task to running status",
				"attempted_status", cfg.StatusRunning,
				"available_statuses", task.AvailableStatuses,
				"error", err.Error())
		}
	}
	if err := q.deps.Store.MarkRunning(ctx, runID); err != nil {
		log.Error("failed to mark run as running", "error", err.Error())
	}

	if err := q.deps.Runner.GitFetch(ctx); err != nil {
		errMsg := "git fetch failed: " + err.Error()
		log.Error("git fetch failed, aborting run", "error", err.Error())
		q.notifyServiceError(ctx, log, task, errMsg)
		q.markFailed(ctx, log, runID, "", errMsg)
		return
	}

	specSessionID, err := q.deps.Runner.RunSpec(ctx, task.URL)
	if err != nil {
		log.Warn("/spec run failed, falling back to a single task link for /review",
			"error", err.Error(), "spec_session_id", specSessionID)
	}

	specPath := ""
	if _, statErr := os.Stat(q.deps.Runner.SpecFilePath(task.ID)); statErr == nil {
		specPath = filepath.Join(".claude", "specs", task.ID+".md")
	} else {
		log.Warn("/spec did not produce a spec file, running /review with the task link only")
	}

	result, reviewErr := q.deps.Runner.RunReview(ctx, task.URL, specPath)
	verdict := result.Verdict
	if reviewErr != nil {
		log.Error("/review run failed", "error", reviewErr.Error(), "stderr", truncateForLog(result.Stderr))
	} else if verdict.Raw == "" {
		log.Warn("review output had no parsable ИТОГ line, treating as blocked")
	}

	targetStatus, assigneeIDs := Decide(verdict, cfg, task.CreatorID)

	if targetStatus != "" {
		if err := q.deps.ClickUp.SetStatus(ctx, task.ID, targetStatus); err != nil {
			log.Error("failed to move task to target status",
				"attempted_status", targetStatus,
				"available_statuses", task.AvailableStatuses,
				"error", err.Error())
		}
	}

	if len(assigneeIDs) > 0 {
		if err := q.deps.ClickUp.AddAssignees(ctx, task.ID, assigneeIDs); err != nil {
			log.Error("failed to add assignees", "assignees", assigneeIDs, "error", err.Error())
		}
	}

	if strings.TrimSpace(result.Output) != "" {
		q.postComment(ctx, log, task.ID, result.SessionID, result.Output)
	}

	q.notifyReviewResult(ctx, log, task, verdict, cfg.StatusRunning, targetStatus, assigneeIDs, result.SessionID, reviewErr)

	if reviewErr != nil {
		errMsg := reviewErr.Error()
		if result.Stderr != "" {
			errMsg += "; stderr: " + truncateForLog(result.Stderr)
		}
		q.markFailed(ctx, log, runID, result.SessionID, errMsg)
		return
	}

	if err := q.deps.Store.MarkDone(ctx, runID, verdict.Status, result.SessionID); err != nil {
		log.Error("failed to mark run done", "error", err.Error())
	}
}

func (q *Queue) markFailed(ctx context.Context, log *slog.Logger, runID int64, sessionID, errMsg string) {
	if err := q.deps.Store.MarkFailed(ctx, runID, sessionID, errMsg); err != nil {
		log.Error("failed to mark run failed", "error", err.Error())
	}
}

// postComment публикует полный текст ревью, при необходимости разбивая его
// на несколько последовательных комментариев по границам разделов.
func (q *Queue) postComment(ctx context.Context, log *slog.Logger, taskID, sessionID, output string) {
	full := output
	if sessionID != "" {
		full = fmt.Sprintf("Сессия ревью: `%s`\n\n%s", sessionID, output)
	}

	chunks := SplitComment(full, maxCommentLen)
	for i, chunk := range chunks {
		if err := q.deps.ClickUp.AddComment(ctx, taskID, chunk); err != nil {
			log.Error("failed to post review comment", "part", i+1, "of", len(chunks), "error", err.Error())
			return
		}
	}
}

func (q *Queue) notifyReviewResult(ctx context.Context, log *slog.Logger, task *clickup.Task, verdict review.Verdict, fromStatus, toStatus string, assigneeIDs []int, sessionID string, reviewErr error) {
	n := slack.ReviewNotification{
		TaskName:   task.Name,
		TaskURL:    task.URL,
		Verdict:    verdict.Status,
		Critical:   verdict.Critical,
		Important:  verdict.Important,
		Minor:      verdict.Minor,
		FromStatus: fromStatus,
		ToStatus:   toStatus,
		Assignee:   formatAssignees(assigneeIDs),
		SessionID:  sessionID,
	}

	var text string
	var blocks []slack.Block
	switch {
	case reviewErr != nil:
		text, blocks = slack.BuildServiceErrorMessage(task.Name, task.URL, reviewErr.Error())
	case verdict.Status == review.StatusBlocked:
		reason := "строка ИТОГ отсутствует или не разобрана"
		if verdict.Raw != "" {
			reason = "ревью сообщило blocked, см. текст ревью"
		}
		text, blocks = slack.BuildBlockedMessage(n, reason)
	default:
		text, blocks = slack.BuildReviewMessage(n)
	}

	if err := q.deps.Slack.Send(ctx, text, blocks); err != nil {
		log.Error("failed to send slack notification", "error", err.Error())
	}
}

func (q *Queue) notifyServiceError(ctx context.Context, log *slog.Logger, task *clickup.Task, errMsg string) {
	text, blocks := slack.BuildServiceErrorMessage(task.Name, task.URL, errMsg)
	if err := q.deps.Slack.Send(ctx, text, blocks); err != nil {
		log.Error("failed to send slack service-error notification", "error", err.Error())
	}
}

func formatAssignees(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ", ")
}

func truncateForLog(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
