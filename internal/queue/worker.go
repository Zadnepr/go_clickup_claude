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

// requiredCommands — slash-команды, без которых /spec и /review не смогут
// отработать содержательно; проверяются перед запуском claude.
var requiredCommands = []string{"spec.md", "review.md"}

// ClickUp — часть API ClickUp, нужная воркеру очереди. Позволяет подменять
// реальный клиент фейком в тестах.
type ClickUp interface {
	GetTask(ctx context.Context, taskID string) (*clickup.Task, error)
	SetStatus(ctx context.Context, taskID, status string) error
	AddAssignees(ctx context.Context, taskID string, userIDs []int) error
	RemoveAssignees(ctx context.Context, taskID string, userIDs []int) error
	RemoveTag(ctx context.Context, taskID, tagName string) error
	AddComment(ctx context.Context, taskID, text string) error
}

// Runner — часть API запуска claude, нужная воркеру очереди.
type Runner interface {
	GitFetch(ctx context.Context) error
	RunSpec(ctx context.Context, taskURL string) (sessionID string, usage review.TokenUsage, err error)
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

// specFileCandidateIDs возвращает ID, по которым /spec могла сохранить файл
// ТЗ, в порядке приоритета. /spec называет файл `.claude/specs/<ID задачи>.md`,
// и на практике команда предпочитает человекочитаемый custom_id (например,
// "PNL-4528"), если он у задачи задан — иначе использует нативный ID ClickUp.
// Проверяем оба варианта, чтобы не промахнуться мимо реально созданного файла.
func specFileCandidateIDs(task *clickup.Task) []string {
	if task.CustomID != "" {
		return []string{task.CustomID, task.ID}
	}
	return []string{task.ID}
}

// missingCommands проверяет наличие .claude/commands/{spec,review}.md в
// рабочей копии репозитория — без них claude не сможет содержательно
// отработать, и запускать процесс нет смысла.
func missingCommands(repoPath string) []string {
	var missing []string
	for _, name := range requiredCommands {
		rel := filepath.Join(".claude", "commands", name)
		if _, err := os.Stat(filepath.Join(repoPath, rel)); err != nil {
			missing = append(missing, rel)
		}
	}
	return missing
}

// processTask прогоняет одну задачу через полный цикл: проверка условия,
// дедупликация, перевод в running, /spec+/review, переходы статуса и
// исполнителя, комментарий, уведомление в Slack и в лог.
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

// resumeTask доводит до конца прогон, прерванный крахом или убийством
// процесса (см. Store.RecoverFromRestart), — при старте сервиса, а не по
// событию. В отличие от processTask, условие триггера (тег/статус/список)
// не проверяется: ревью уже было начато для этой задачи, и его нужно
// завершить (перевести в нужную колонку, оставить комментарий) независимо
// от того, как сейчас выглядит карточка в ClickUp — например, она может
// застрять в STATUS_RUNNING без тега, если тот успел слететь раньше обрыва.
func (q *Queue) resumeTask(taskID string) {
	cfg := q.deps.Cfg
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ReviewTimeout)
	defer cancel()

	log := q.deps.Logger.With("task_id", taskID, "resumed", true)

	task, err := q.deps.ClickUp.GetTask(ctx, taskID)
	if err != nil {
		log.Error("failed to fetch task for resume, giving up until reconcile finds it again", "error", err.Error())
		return
	}

	runID, enqueued, err := q.deps.Store.TryEnqueue(ctx, taskID)
	if err != nil {
		log.Error("failed to record resumed run in store", "error", err.Error())
		return
	}
	if !enqueued {
		log.Debug("resumed task already has an active or completed run, skipping")
		return
	}

	log.Info("resuming review interrupted by a previous instance")
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

	// Снимаем текущих исполнителей на время проверки — запоминаем их, чтобы
	// при провале вернуть работу тому же человеку (или создателю, если
	// исполнителей не было). Успех переназначает задачу отдельному ревьюеру
	// (ASSIGNEE_ON_PASS), поэтому снятые здесь люди для успешного пути не нужны.
	originalAssignees := append([]int(nil), task.Assignees...)
	if len(originalAssignees) > 0 {
		if err := q.deps.ClickUp.RemoveAssignees(ctx, task.ID, originalAssignees); err != nil {
			log.Error("failed to remove assignees before check", "assignees", originalAssignees, "error", err.Error())
		}
	}

	q.notifyStarted(ctx, log, task)

	if missing := missingCommands(cfg.RepoPath); len(missing) > 0 {
		errMsg := fmt.Sprintf("не найдены обязательные команды: %s", strings.Join(missing, ", "))
		log.Error("required slash commands are missing, aborting run", "missing", missing)
		q.notifyServiceError(ctx, log, task, errMsg)
		q.markFailed(ctx, log, runID, "", errMsg, review.TokenUsage{})
		return
	}

	if err := q.deps.Runner.GitFetch(ctx); err != nil {
		errMsg := "git fetch failed: " + err.Error()
		log.Error("git fetch failed, aborting run", "error", err.Error())
		q.notifyServiceError(ctx, log, task, errMsg)
		q.markFailed(ctx, log, runID, "", errMsg, review.TokenUsage{})
		return
	}

	specSessionID, specUsage, err := q.deps.Runner.RunSpec(ctx, task.URL)
	if err != nil {
		log.Warn("/spec run failed, falling back to a single task link for /review",
			"error", err.Error(), "spec_session_id", specSessionID)
	}

	specPath := ""
	for _, id := range specFileCandidateIDs(task) {
		if _, statErr := os.Stat(q.deps.Runner.SpecFilePath(id)); statErr == nil {
			specPath = filepath.Join(".claude", "specs", id+".md")
			break
		}
	}
	if specPath == "" {
		log.Warn("/spec did not produce a spec file, running /review with the task link only")
	}

	result, reviewErr := q.deps.Runner.RunReview(ctx, task.URL, specPath)
	totalUsage := specUsage.Add(result.Usage)
	verdict := result.Verdict
	if reviewErr != nil {
		log.Error("/review run failed", "error", reviewErr.Error(), "stderr", truncateForLog(result.Stderr))
	} else if verdict.Raw == "" {
		log.Warn("review output had no parsable ИТОГ line, treating as blocked")
	}

	if strings.TrimSpace(result.Output) != "" {
		q.postComment(ctx, log, task.ID, result.SessionID, result.Output)
	}

	// Реальный сбой процесса (не смог запуститься, упал, protухший токен,
	// таймаут) — это не результат ревью, а инфраструктурная проблема Go/claude.
	// Задачу нельзя трогать так, будто её реально проверили: статус, тег и
	// исполнитель остаются как есть — карточка так и останется в running-
	// колонке до ручного возврата или повторного запуска, но не получит
	// ложный вердикт. Только настоящий ответ /review (даже blocked) меняет
	// статус/тег/исполнителя.
	var targetStatus string
	var assigneeIDs []int
	if reviewErr == nil {
		targetStatus, assigneeIDs = Decide(verdict, cfg, task.CreatorID, originalAssignees)

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

		if cfg.TriggerTag != "" {
			if err := q.deps.ClickUp.RemoveTag(ctx, task.ID, cfg.TriggerTag); err != nil {
				log.Error("failed to remove trigger tag", "tag", cfg.TriggerTag, "error", err.Error())
			}
		}
	}

	q.notifyReviewResult(ctx, log, task, verdict, cfg.StatusRunning, targetStatus, assigneeIDs, result.SessionID, reviewErr)

	if reviewErr != nil {
		errMsg := reviewErr.Error()
		if result.Stderr != "" {
			errMsg += "; stderr: " + truncateForLog(result.Stderr)
		}
		q.markFailed(ctx, log, runID, result.SessionID, errMsg, totalUsage)
		return
	}

	if err := q.deps.Store.MarkDone(ctx, runID, verdict.Status, result.SessionID, storeUsage(totalUsage)); err != nil {
		log.Error("failed to mark run done", "error", err.Error())
	}
}

func (q *Queue) markFailed(ctx context.Context, log *slog.Logger, runID int64, sessionID, errMsg string, usage review.TokenUsage) {
	if err := q.deps.Store.MarkFailed(ctx, runID, sessionID, errMsg, storeUsage(usage)); err != nil {
		log.Error("failed to mark run failed", "error", err.Error())
	}
}

func storeUsage(u review.TokenUsage) store.Usage {
	return store.Usage{
		InputTokens:  int64(u.InputTokens),
		OutputTokens: int64(u.OutputTokens),
		CostUSD:      u.CostUSD,
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

// notifyReviewResult шлёт в Slack и в консоль короткое сообщение о том, что
// сделано: задача и куда она перемещена. Для blocked и внутренних ошибок
// сервиса сообщение отдельное и содержит причину — молчаливый отказ хуже
// ложного срабатывания.
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
		text, blocks = slack.BuildShortResultMessage(n)
	}

	if err := q.deps.Slack.Send(ctx, text, blocks); err != nil {
		log.Error("failed to send slack notification", "error", err.Error())
	}

	log.Info(fmt.Sprintf("задача «%s» обработана: %s → %s", task.Name, fromStatus, toStatus),
		"verdict", verdict.Status, "assignees", assigneeIDs, "task_url", task.URL)
}

func (q *Queue) notifyStarted(ctx context.Context, log *slog.Logger, task *clickup.Task) {
	text, blocks := slack.BuildStartedMessage(task.Name, task.URL)
	if err := q.deps.Slack.Send(ctx, text, blocks); err != nil {
		log.Error("failed to send slack start notification", "error", err.Error())
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
