package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/review"
	"github.com/Zadnepr/go_clickup_claude/internal/slack"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// maxLoggedOutput — предел размера ответа claude в структурированном логе
// (Требование: «писать ответы claude в лог»). Полный ответ без обрезки
// всегда доступен в БД (см. store.ClaudeInvocation) — предел здесь только
// для того, чтобы один ответ ревью на большой PR не забивал журнал сервиса.
const maxLoggedOutput = 8000

// requiredCommands — slash-команды, без которых /spec-go и /review-go не
// смогут отработать содержательно; проверяются перед запуском claude. Это
// версии команд для этого пайплайна — не путать с ручными /spec и /review
// (те сохраняют файл и рассчитаны на интерактивную сессию человека).
var requiredCommands = []string{"spec-go.md", "review-go.md"}

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
	RunSpec(ctx context.Context, taskURL string, opts review.CallOptions) (review.SpecResult, error)
	RunReview(ctx context.Context, taskURL, specPath string, opts review.CallOptions) (review.Result, error)
	// SpecFilePath — путь, по которому нужно сохранить (не сама /spec-go —
	// см. Queue.runReview) содержимое собранного ТЗ, чтобы /review-go могла
	// прочитать его тулом Read. Источник истины для этого содержимого —
	// БД (specStageData.Content), файл — лишь производный от неё артефакт.
	SpecFilePath(taskID string) string
	// SetModelEffort/ModelEffort — модель/effort по умолчанию для вызовов
	// без собственного переопределения (см. RunOptions в control.go) —
	// меняются на лету через веб-интерфейс (см. httpapi.RunnerControl).
	SetModelEffort(model, effort string)
	ModelEffort() (model, effort string)
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
	if !hasTriggerTag(task, cfg) {
		return false
	}
	return config.NormalizeStatus(task.Status) == config.NormalizeStatus(cfg.StatusTrigger)
}

// hasTriggerTag проверяет только наличие TRIGGER_TAG на задаче, без статуса
// и списка — общая часть isEligible и resumable.
func hasTriggerTag(task *clickup.Task, cfg *config.Config) bool {
	wantTag := config.NormalizeStatus(cfg.TriggerTag)
	for _, tag := range task.Tags {
		if config.NormalizeStatus(tag) == wantTag {
			return true
		}
	}
	return false
}

// resumable проверяет, что прерванную падением/перезапуском сервиса
// проверку (см. resumeTask, Store.RecoverFromRestart) вообще стоит
// доводить до конца: пока сервис был недоступен, задачу могли обработать
// вручную — проверить, перевести статус или снять тег. Условие мягче
// isEligible (не требует STATUS_TRIGGER жёстко): прогон мог прерваться уже
// после того, как setup перевёл карточку в STATUS_RUNNING, поэтому
// допустимы обе колонки — исходная триггерная и рабочая. Тег обязателен
// в обоих случаях: его снимает только успешно отработавший decide, значит
// его отсутствие — надёжный признак того, что кто-то (человек или более
// ранний прогон) уже закрыл вопрос по этой задаче без нас.
func resumable(task *clickup.Task, cfg *config.Config) bool {
	if !hasTriggerTag(task, cfg) {
		return false
	}
	status := config.NormalizeStatus(task.Status)
	return status == config.NormalizeStatus(cfg.StatusTrigger) || status == config.NormalizeStatus(cfg.StatusRunning)
}

// staleAssignees возвращает тех из current, кого нет среди want — то, что
// осталось на задаче не по воле этого прогона (например, человек, который
// вручную проверял задачу параллельно, пока она лежала в STATUS_RUNNING) и
// должно быть снято при переходе по вердикту (см. decide в runReview).
func staleAssignees(current, want []int) []int {
	if len(current) == 0 {
		return nil
	}
	wantSet := make(map[int]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	var stale []int
	for _, id := range current {
		if !wantSet[id] {
			stale = append(stale, id)
		}
	}
	return stale
}

// belongsToConfiguredList проверяет только принадлежность задачи
// настроенному списку (CU_LIST_ID) — без тега и статуса. Используется
// вместо полного isEligible при явном ручном запуске конкретного task_id
// (см. RunOptions.SkipEligibility): само подтверждение "запустить именно
// эту задачу" уже пришло от оператора, но задача из совершенно другого
// списка — почти наверняка результат опечатки в ID, а не намеренный выбор.
func belongsToConfiguredList(task *clickup.Task, cfg *config.Config) bool {
	return task.ListID == cfg.CUListID
}

// specFileID возвращает ID задачи, под которым сохраняется файл ТЗ
// (`specs/<ID>.md`, см. Runner.SpecFilePath): человекочитаемый custom_id
// (например, "PNL-4528"), если он у задачи задан, иначе нативный ID ClickUp.
// В отличие от прежней версии (когда /spec-go сама писала файл и приходилось
// угадывать, каким именем она его назвала) Go теперь пишет этот файл сам
// из содержимого, сохранённого в БД, — поэтому имя выбирается детерминированно,
// без перебора кандидатов.
func specFileID(task *clickup.Task) string {
	if task.CustomID != "" {
		return task.CustomID
	}
	return task.ID
}

// missingCommands проверяет наличие .claude/commands/{spec-go,review-go}.md
// в рабочей копии репозитория — без них claude не сможет содержательно
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
// дедупликация, перевод в running, /spec-go+/review-go, переходы статуса и
// исполнителя, комментарий, уведомление в Slack и в лог. opts — переопределение
// модели/effort claude на этот конкретный прогон (см. RunOptions), обычно
// нулевое (использовать текущее значение по умолчанию).
func (q *Queue) processTask(taskID string, opts RunOptions) {
	cfg := q.deps.Cfg
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ReviewTimeout)
	defer cancel()

	log := q.deps.Logger.With("task_id", taskID)

	if paused, remaining, reason := q.pausedFor(); paused {
		log.Debug("queue is paused, skipping until the pause ends", "remaining", remaining.Round(time.Second), "reason", reason)
		return
	}

	task, err := q.deps.ClickUp.GetTask(ctx, taskID)
	if err != nil {
		log.Error("failed to fetch task, will retry on next event or reconcile", "error", err.Error())
		return
	}

	if opts.SkipEligibility {
		if !belongsToConfiguredList(task, cfg) {
			log.Warn("manual run refused: task does not belong to the configured list", "list_id", task.ListID)
			return
		}
	} else if !isEligible(task, cfg) {
		log.Debug("task does not meet trigger condition, skipping without a dedup record")
		return
	}

	runID, enqueued, err := q.deps.Store.ReopenOrEnqueue(ctx, taskID)
	if err != nil {
		log.Error("failed to record run in store", "error", err.Error())
		return
	}
	if !enqueued {
		log.Debug("task already has an active or completed run, skipping")
		return
	}
	if err := q.deps.Store.SetRunTaskInfo(ctx, runID, task.CustomID, task.Name); err != nil {
		log.Error("failed to record task info for run", "error", err.Error())
	}

	q.registerActive(taskID, runID, cancel)
	defer q.unregisterActive(taskID)
	q.runReview(ctx, log, task, runID, opts)
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

	// Пауза очереди (см. runReview) здесь намеренно не проверяется:
	// SubmitResume вызывается один раз при старте для задач, которые
	// зависли конкретно в этом виде (см. Store.RecoverFromRestart) и не
	// обязательно эффективны для сверки (карточка может не соответствовать
	// условию триггера) — пропустить эту попытку значит рискнуть потерять
	// задачу до ручного вмешательства. Один лишний запуск на паузу не
	// критичен: он тут же попадёт в ту же ветку обработки ErrUsageLimit.
	log := q.deps.Logger.With("task_id", taskID, "resumed", true)

	task, err := q.deps.ClickUp.GetTask(ctx, taskID)
	if err != nil {
		log.Error("failed to fetch task for resume, giving up until reconcile finds it again", "error", err.Error())
		return
	}

	runID, enqueued, err := q.deps.Store.ReopenOrEnqueue(ctx, taskID)
	if err != nil {
		log.Error("failed to record resumed run in store", "error", err.Error())
		return
	}
	if !enqueued {
		log.Debug("resumed task already has an active or completed run, skipping")
		return
	}
	if err := q.deps.Store.SetRunTaskInfo(ctx, runID, task.CustomID, task.Name); err != nil {
		log.Error("failed to record task info for run", "error", err.Error())
	}

	// Пока сервис был недоступен, карточку могли обработать вручную —
	// проверить, перевести статус или снять тег-триггер. Дожимать прерванную
	// проверку в этом случае не нужно: её результат уже никого не интересует,
	// а по итогу можно ещё и наложить устаревший вердикт поверх того, что
	// сделал человек (см. Требование «синхронизировать состояние
	// восстановленной задачи»). Сама карточка не трогается — ни статус, ни
	// тег, ни исполнитель: раз её кто-то обработал сам, вмешиваться не надо.
	if !resumable(task, cfg) {
		msg := fmt.Sprintf(
			"задача больше не в колонке %q/%q с тегом %q — похоже, её обработали вручную, пока сервис был недоступен; возобновление отменено",
			cfg.StatusTrigger, cfg.StatusRunning, cfg.TriggerTag)
		log.Info("task no longer matches trigger tag/status after restart, abandoning resume",
			"status", task.Status, "tags", task.Tags)
		q.markFailed(ctx, log, runID, "", msg, review.TokenUsage{})
		return
	}

	log.Info("resuming review interrupted by a previous instance")
	q.registerActive(taskID, runID, cancel)
	defer q.unregisterActive(taskID)
	q.runReview(ctx, log, task, runID, RunOptions{})
}

// runReview прогоняет одну задачу через полный цикл поэтапно (см. stage.go):
// setup → commands_check → git_fetch → spec → review → decide → comment.
// Каждый этап логируется в run_stages (Store.StartStage/FinishStage/
// FailStage) — если runID уже переоткрыт после паузы/обрыва процесса (см.
// Store.ReopenOrEnqueue), уже пройденные этапы берутся из БД, а не
// выполняются заново: /spec-go и /review-go — самые дорогие вызовы во всём цикле,
// и именно их результат («результат /spec-go сохранялся в табличку») не нужно
// терять при возобновлении.
func (q *Queue) runReview(ctx context.Context, log *slog.Logger, task *clickup.Task, runID int64, opts RunOptions) {
	cfg := q.deps.Cfg

	if err := q.deps.Store.MarkRunning(ctx, runID); err != nil {
		log.Error("failed to mark run as running", "error", err.Error())
	}

	setup, _, _ := stageRun(ctx, q, log, runID, stageSetup, func() (setupData, error) {
		if cfg.StatusRunning != "" {
			if err := q.deps.ClickUp.SetStatus(ctx, task.ID, cfg.StatusRunning); err != nil {
				log.Error("failed to move task to running status",
					"attempted_status", cfg.StatusRunning,
					"available_statuses", task.AvailableStatuses,
					"error", err.Error())
			}
		}

		// Снимаем текущих исполнителей на время проверки — запоминаем их в
		// data этого этапа, чтобы при провале вернуть работу тому же
		// человеку (или создателю, если исполнителей не было). Критично для
		// возобновления: свежий GetTask после снятия вернёт уже пустой
		// список — источником истины здесь может быть только сохранённый
		// результат этого этапа, а не текущее состояние карточки.
		original := append([]int(nil), task.Assignees...)
		if len(original) > 0 {
			if err := q.deps.ClickUp.RemoveAssignees(ctx, task.ID, original); err != nil {
				log.Error("failed to remove assignees before check", "assignees", original, "error", err.Error())
			}
		}

		q.notifyStarted(ctx, log, task)
		return setupData{OriginalAssignees: original}, nil
	})
	originalAssignees := setup.OriginalAssignees

	if _, _, err := stageRun(ctx, q, log, runID, stageCommandsCheck, func() (struct{}, error) {
		if missing := missingCommands(cfg.RepoPath); len(missing) > 0 {
			return struct{}{}, fmt.Errorf("не найдены обязательные команды: %s", strings.Join(missing, ", "))
		}
		return struct{}{}, nil
	}); err != nil {
		log.Error("required slash commands are missing, aborting run", "error", err.Error())
		q.notifyServiceError(ctx, log, task, err.Error())
		q.markFailed(ctx, log, runID, "", err.Error(), review.TokenUsage{})
		return
	}

	if _, _, err := stageRun(ctx, q, log, runID, stageGitFetch, func() (struct{}, error) {
		return struct{}{}, q.deps.Runner.GitFetch(ctx)
	}); err != nil {
		errMsg := "git fetch failed: " + err.Error()
		log.Error("git fetch failed, aborting run", "error", err.Error())
		q.notifyServiceError(ctx, log, task, errMsg)
		q.markFailed(ctx, log, runID, "", errMsg, review.TokenUsage{})
		return
	}

	if q.checkManualPause(log, task, runID, "", review.TokenUsage{}) {
		return
	}

	spec, _, specErr := stageRun(ctx, q, log, runID, stageSpec, func() (specStageData, error) {
		sr, err := q.deps.Runner.RunSpec(ctx, task.URL, review.CallOptions{
			Model: opts.Model, Effort: opts.Effort,
			OnUsage: func(u review.TokenUsage) {
				q.deps.Store.UpdateRunningUsage(ctx, runID, storeUsage(u))
			},
		})
		q.logAndRecordInvocation(ctx, log, runID, stageSpec, invocationLog{
			SessionID: sr.SessionID, Prompt: sr.Prompt, Output: sr.Content, Stderr: sr.Stderr,
			Subtype: sr.Subtype, IsError: sr.IsError, Usage: sr.Usage, Model: sr.Model, Effort: sr.Effort,
			StartedAt: sr.StartedAt, FinishedAt: sr.FinishedAt,
		})
		if err != nil {
			return specStageData{SessionID: sr.SessionID, Usage: sr.Usage}, err
		}

		content := strings.TrimSpace(sr.Content)
		specID := ""
		if content != "" {
			specID = specFileID(task)
		} else {
			log.Warn("/spec-go did not return any ТЗ content, running /review-go with the task link only")
		}
		return specStageData{SessionID: sr.SessionID, SpecID: specID, Content: content, Usage: sr.Usage}, nil
	})
	if errors.Is(specErr, review.ErrUsageLimit) {
		q.pauseAndRestore(log, task, runID, originalAssignees, spec.SessionID, specErr, spec.Usage)
		return
	}
	if specErr != nil {
		log.Warn("/spec-go run failed, falling back to a single task link for /review-go",
			"error", specErr.Error(), "spec_session_id", spec.SessionID)
	}

	// ТЗ хранится в БД (specStageData.Content), а не в файле — файл здесь
	// лишь производный артефакт, который читает /review-go тулом Read.
	// Перезаписывается из БД перед КАЖДЫМ запуском /review-go, в том числе при
	// возобновлении прогона (см. Store.ReopenOrEnqueue): рабочая копия
	// репозитория в новом контейнере может не содержать файла, который был
	// записан предыдущим (см. ensureSpecFile).
	specPath := q.ensureSpecFile(log, spec)

	if q.checkManualPause(log, task, runID, spec.SessionID, spec.Usage) {
		return
	}

	reviewData, _, reviewErr := stageRun(ctx, q, log, runID, stageReview, func() (reviewStageData, error) {
		result, err := q.deps.Runner.RunReview(ctx, task.URL, specPath, review.CallOptions{
			Model: opts.Model, Effort: opts.Effort,
			OnUsage: func(u review.TokenUsage) {
				q.deps.Store.UpdateRunningUsage(ctx, runID, storeUsage(spec.Usage.Add(u)))
			},
		})
		q.logAndRecordInvocation(ctx, log, runID, stageReview, invocationLog{
			SessionID: result.SessionID, Prompt: result.Prompt, Output: result.Output, Stderr: result.Stderr,
			Subtype: result.Subtype, IsError: result.IsError, Usage: result.Usage, Model: result.Model, Effort: result.Effort,
			StartedAt: result.StartedAt, FinishedAt: result.FinishedAt,
		})
		return reviewStageData{SessionID: result.SessionID, Output: result.Output, Stderr: result.Stderr, Usage: result.Usage}, err
	})
	totalUsage := spec.Usage.Add(reviewData.Usage)
	if errors.Is(reviewErr, review.ErrUsageLimit) {
		q.pauseAndRestore(log, task, runID, originalAssignees, reviewData.SessionID, reviewErr, totalUsage)
		return
	}

	// Вердикт не хранится отдельным полем — восстанавливается из Output
	// чистой функцией ParseVerdict, одинаково что для свежего прогона, что
	// для взятого из run_stages при возобновлении.
	verdict := review.ParseVerdict(reviewData.Output)
	if reviewErr != nil {
		log.Error("/review-go run failed", "error", reviewErr.Error(), "stderr", truncateForLog(reviewData.Stderr))
	} else if verdict.Raw == "" {
		log.Warn("review output had no parsable ИТОГ line, treating as blocked")
	}

	// Всё, что идёт дальше (комментарий, перевод статуса/исполнителя,
	// уведомление в Slack), сознательно переходит на независимый от ctx
	// прогона контекст (см. wrapUpCtx). spec+review обычно и съедают
	// большую часть общего ReviewTimeout (в этом сервисе на практике —
	// частые 12-17 минут вдвоём при бюджете в 15), так что к этому месту
	// от исходного ctx может остаться доли секунды или он уже истёк. Без
	// отдельного контекста это утаскивает за собой ровно ту работу, которая
	// обязана произойти после успешного ревью: карточка так и остаётся в
	// STATUS_RUNNING с тегом-триггером, а уведомление о готовом результате
	// не уходит — снаружи это неотличимо от «задача зависла», хотя ревью
	// уже реально отработало и стоило денег.
	wctx, wcancel := wrapUpCtx()
	defer wcancel()

	commentText := strings.TrimSpace(stripVerdictLine(reviewData.Output, verdict.Raw))
	if commentText != "" {
		// Комментарий — единственный этап без данных для восстановления:
		// важен сам факт «уже опубликован», иначе возобновление продублирует
		// его в задаче, а ClickUp такие дубли не схлопывает.
		stageRun(wctx, q, log, runID, stageComment, func() (struct{}, error) {
			q.postComment(wctx, log, task.ID, commentText)
			return struct{}{}, nil
		})
	}

	// Реальный сбой процесса (не смог запуститься, упал, protухший токен,
	// таймаут) — это не результат ревью, а инфраструктурная проблема Go/claude.
	// Задачу нельзя трогать так, будто её реально проверили: статус, тег и
	// исполнитель остаются как есть — карточка так и останется в running-
	// колонке до ручного возврата или повторного запуска, но не получит
	// ложный вердикт. Только настоящий ответ /review-go (даже blocked) меняет
	// статус/тег/исполнителя.
	var targetStatus string
	var assigneeIDs []int
	if reviewErr == nil {
		decide, _, _ := stageRun(wctx, q, log, runID, stageDecide, func() (decideStageData, error) {
			ts, ids := Decide(verdict, cfg, task.CreatorID, task.DeveloperIDs)

			if ts != "" {
				if err := q.deps.ClickUp.SetStatus(wctx, task.ID, ts); err != nil {
					log.Error("failed to move task to target status",
						"attempted_status", ts,
						"available_statuses", task.AvailableStatuses,
						"error", err.Error())
				}
			}

			// Свежий список исполнителей: setup снял тех, кто был назначен ДО
			// начала проверки, но пока задача лежала в STATUS_RUNNING, на неё
			// мог назначиться кто-то ещё вручную — например, человек, который
			// параллельно сам её проверял. После вердикта на задаче должен
			// остаться только целевой исполнитель (ids), а не любой, кто
			// оказался назначен по ходу (см. Требование «после переноса в
			// rework должен остаться только исполнитель, проверяющий должен
			// сняться»). task.Assignees — то, что было на момент начала этого
			// прогона, поэтому берём карточку заново, а не полагаемся на неё.
			if fresh, err := q.deps.ClickUp.GetTask(wctx, task.ID); err != nil {
				log.Error("failed to fetch fresh assignees before deciding, skipping stale-assignee cleanup", "error", err.Error())
			} else if stale := staleAssignees(fresh.Assignees, ids); len(stale) > 0 {
				if err := q.deps.ClickUp.RemoveAssignees(wctx, task.ID, stale); err != nil {
					log.Error("failed to remove stale assignees", "assignees", stale, "error", err.Error())
				}
			}

			if len(ids) > 0 {
				if err := q.deps.ClickUp.AddAssignees(wctx, task.ID, ids); err != nil {
					log.Error("failed to add assignees", "assignees", ids, "error", err.Error())
				}
			}

			if cfg.TriggerTag != "" {
				if err := q.deps.ClickUp.RemoveTag(wctx, task.ID, cfg.TriggerTag); err != nil {
					log.Error("failed to remove trigger tag", "tag", cfg.TriggerTag, "error", err.Error())
				}
			}
			return decideStageData{TargetStatus: ts, AssigneeIDs: ids}, nil
		})
		targetStatus = decide.TargetStatus
		assigneeIDs = decide.AssigneeIDs
	}

	q.notifyReviewResult(wctx, log, runID, task, verdict, cfg.StatusRunning, targetStatus, assigneeIDs, reviewData.SessionID, reviewErr)

	if reviewErr != nil {
		errMsg := reviewErr.Error()
		if reviewData.Stderr != "" {
			errMsg += "; stderr: " + truncateForLog(reviewData.Stderr)
		}
		q.markFailed(ctx, log, runID, reviewData.SessionID, errMsg, totalUsage)
		return
	}

	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.deps.Store.MarkDone(fctx, runID, verdict.Status, reviewData.SessionID, storeUsage(totalUsage)); err != nil {
		log.Error("failed to mark run done", "error", err.Error())
	}
}

// pauseAndRestore обрабатывает review.ErrUsageLimit: исчерпанный лимит
// использования claude — это не результат ревью и не сбой процесса, а
// временное состояние аккаунта. В отличие от обычной ошибки (см. комментарий
// перед reviewErr == nil в runReview), карточку возвращаем в исходный вид —
// статус триггера и снятых исполнителей — чтобы сверка подобрала задачу
// заново сама, без ручного вмешательства, когда лимит освободится.
func (q *Queue) pauseAndRestore(log *slog.Logger, task *clickup.Task, runID int64, originalAssignees []int, sessionID string, causeErr error, usage review.TokenUsage) {
	cfg := q.deps.Cfg
	errMsg := causeErr.Error()

	// Дальше — исключительно завершающие действия ("вернуть карточку и
	// зафиксировать паузу"), не зависящие от ctx самого прогона (см.
	// finalizeCtx): usage limit обычно не связан с истечением ReviewTimeout,
	// но если оба всё же совпали по времени, откат карточки и запись паузы
	// не должны провалиться вместе с исходным ctx.
	fctx, cancel := finalizeCtx()
	defer cancel()

	if cfg.StatusTrigger != "" {
		if err := q.deps.ClickUp.SetStatus(fctx, task.ID, cfg.StatusTrigger); err != nil {
			log.Error("failed to move task back to trigger status after usage limit pause",
				"attempted_status", cfg.StatusTrigger, "error", err.Error())
		}
	}
	if len(originalAssignees) > 0 {
		if err := q.deps.ClickUp.AddAssignees(fctx, task.ID, originalAssignees); err != nil {
			log.Error("failed to restore assignees after usage limit pause", "assignees", originalAssignees, "error", err.Error())
		}
	}

	if err := q.deps.Store.MarkPaused(fctx, runID, sessionID, errMsg, storeUsage(usage)); err != nil {
		log.Error("failed to mark run paused", "error", err.Error())
	}

	q.pauseFor(cfg.UsageLimitPause, errMsg)

	log.Warn("claude usage limit reached, pausing queue and returning task to trigger state",
		"pause_for", cfg.UsageLimitPause, "error", errMsg)

	text, blocks := slack.BuildPausedMessage(task.Name, task.URL, cfg.UsageLimitPause, errMsg)
	if err := q.deps.Slack.Send(fctx, text, blocks); err != nil {
		log.Error("failed to send slack paused notification", "error", err.Error())
	}
}

// checkManualPause проверяет, не попросили ли остановить именно эту задачу
// через RequestPause (см. control.go и "поставить на паузу" в
// веб-интерфейсе), и если да — останавливает прогон на этой границе этапов
// (см. pauseManually). true означает, что runReview должен завершиться
// прямо сейчас, не запуская следующий этап.
func (q *Queue) checkManualPause(log *slog.Logger, task *clickup.Task, runID int64, sessionID string, usage review.TokenUsage) bool {
	if !q.consumePauseRequest(task.ID) {
		return false
	}
	q.pauseManually(log, task, runID, sessionID, usage)
	return true
}

// pauseManually останавливает прогон по запросу оператора. В отличие от
// pauseAndRestore (пауза из-за исчерпанного лимита claude — временное
// состояние аккаунта, а не запроса) — не трогает статус/исполнителей
// карточки и не ставит на паузу всю очередь, только этот конкретный
// прогон. Карточка намеренно остаётся как есть (не в STATUS_TRIGGER): её
// возврат туда означал бы, что обычная сверка попробует запустить всё
// заново раньше, чем оператор явно нажмёт «продолжить» — см. httpapi'шный
// эндпоинт resume → Queue.SubmitResume → Store.ReopenOrEnqueue находит
// этот же run_id по task_id и доводит прогон до конца, используя уже
// пройденные этапы.
func (q *Queue) pauseManually(log *slog.Logger, task *clickup.Task, runID int64, sessionID string, usage review.TokenUsage) {
	fctx, cancel := finalizeCtx()
	defer cancel()

	const reason = "остановлено оператором через веб-интерфейс"
	if err := q.deps.Store.MarkPaused(fctx, runID, sessionID, reason, storeUsage(usage)); err != nil {
		log.Error("failed to mark run paused", "error", err.Error())
	}
	log.Info("run paused by operator request, waiting for an explicit resume")

	text, blocks := slack.BuildManualPauseMessage(task.Name, task.URL)
	if err := q.deps.Slack.Send(fctx, text, blocks); err != nil {
		log.Error("failed to send slack manual-pause notification", "error", err.Error())
	}
}

// markFailed помечает прогон проваленным. ctx прогона не используется для
// самой записи (см. finalizeCtx) — если он уже отменён (истёк ReviewTimeout
// или сработал RequestCancel, см. control.go), запись итогового статуса не
// должна проваливаться вместе с ним: иначе прогон навсегда зависнет в
// статусе running, и исправить это сможет только ручное вмешательство в БД.
func (q *Queue) markFailed(ctx context.Context, log *slog.Logger, runID int64, sessionID, errMsg string, usage review.TokenUsage) {
	fctx, cancel := finalizeCtx()
	defer cancel()
	if err := q.deps.Store.MarkFailed(fctx, runID, sessionID, errMsg, storeUsage(usage)); err != nil {
		log.Error("failed to mark run failed", "error", err.Error())
	}
}

// finalizeCtx — независимый от ctx прогона контекст для завершающей записи
// результата (MarkDone/MarkFailed/MarkPaused, см. markFailed/pauseAndRestore/
// pauseManually и финальный MarkDone в runReview).
func finalizeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// wrapUpCtx — независимый от ctx прогона контекст для всего, что должно
// произойти ПОСЛЕ успешного /review-go (комментарий, перевод статуса и
// исполнителя, уведомление в Slack — см. использование в runReview). Эта
// работа не должна зависеть от того, сколько бюджета ReviewTimeout уже
// потратили /spec-go и /review-go: они регулярно вдвоём занимают большую
// часть таймаута, и с общим ctx завершающие шаги получали бы то, что от
// него осталось — иногда доли секунды, — из-за чего задача формально
// «проверена», но так и остаётся висеть в running-колонке с тегом-триггером
// и без уведомления. Таймаут здесь на порядок щедрее finalizeCtx (10с),
// потому что тут не одна короткая запись в SQLite, а несколько
// последовательных вызовов ClickUp API (каждый — со своими попытками при
// rate limit, см. internal/clickup) плюс Slack.
func wrapUpCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Minute)
}

func storeUsage(u review.TokenUsage) store.Usage {
	return store.Usage{
		InputTokens:  int64(u.InputTokens),
		OutputTokens: int64(u.OutputTokens),
		CostUSD:      u.CostUSD,
	}
}

// ensureSpecFile перезаписывает файл ТЗ на диске из содержимого, сохранённого
// в БД (specStageData.Content), и возвращает относительный путь для передачи
// /review-go, либо "" если содержимого нет. БД — источник истины (Требование:
// «ТЗ сохранялось в базу и использовалось из базы»); файл — восстанавливаемый
// из неё артефакт, нужный только затем, что /review-go читает его тулом Read.
// Вызывается перед каждым запуском /review-go, в том числе при возобновлении —
// рабочая копия репозитория в новом контейнере могла не унаследовать файл,
// записанный предыдущим экземпляром сервиса.
func (q *Queue) ensureSpecFile(log *slog.Logger, spec specStageData) string {
	if spec.Content == "" || spec.SpecID == "" {
		return ""
	}

	absPath := q.deps.Runner.SpecFilePath(spec.SpecID)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		log.Error("failed to create specs directory, running /review-go with the task link only", "error", err.Error())
		return ""
	}
	if err := os.WriteFile(absPath, []byte(spec.Content), 0o644); err != nil {
		log.Error("failed to write spec file from stored content, running /review-go with the task link only", "error", err.Error())
		return ""
	}
	return filepath.Join("specs", spec.SpecID+".md")
}

// invocationLog — общий вид одного вызова claude (что для /spec-go, что для
// /review-go), нужный только для логирования и сохранения в БД (см.
// Queue.logAndRecordInvocation) — отдельно от review.Result/SpecResult,
// чтобы не завязывать это на конкретный из двух типов.
type invocationLog struct {
	SessionID string
	Prompt    string
	Output    string
	Stderr    string
	Subtype   string
	IsError   bool
	Usage     review.TokenUsage
	// Model/Effort — то, что было реально использовано для этого вызова
	// (см. review.Result.Model/Effort), а не текущая конфигурация: та могла
	// уже смениться (см. RunnerControl.SetModelEffort) к моменту записи.
	Model      string
	Effort     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// logAndRecordInvocation пишет полный ответ claude в лог сервиса (обрезая
// длинный текст — сам ответ без обрезки всегда доступен в БД) и сохраняет
// вызов целиком в claude_invocations (Требование: «лог процесса работы
// claude ... с токенами на этот этап. И чтобы ответ всей сессии тоже
// писался в таблицу»). Ошибки самой записи не прерывают прогон — потерять
// строку лога менее важно, чем результат ревью.
func (q *Queue) logAndRecordInvocation(ctx context.Context, log *slog.Logger, runID int64, stage string, inv invocationLog) {
	log.Info("claude response",
		"stage", stage, "session_id", inv.SessionID, "subtype", inv.Subtype, "is_error", inv.IsError,
		"model", inv.Model, "effort", inv.Effort,
		"input_tokens", inv.Usage.InputTokens, "output_tokens", inv.Usage.OutputTokens, "cost_usd", inv.Usage.CostUSD,
		"duration", inv.FinishedAt.Sub(inv.StartedAt).Round(time.Second),
		"output", truncateOutputForLog(inv.Output))

	if _, err := q.deps.Store.RecordInvocation(ctx, store.ClaudeInvocation{
		RunID: runID, Stage: stage, SessionID: inv.SessionID, Model: inv.Model, Effort: inv.Effort,
		Prompt: inv.Prompt, Output: inv.Output, Stderr: inv.Stderr, Subtype: inv.Subtype, IsError: inv.IsError,
		InputTokens: int64(inv.Usage.InputTokens), OutputTokens: int64(inv.Usage.OutputTokens), CostUSD: inv.Usage.CostUSD,
		StartedAt: inv.StartedAt, FinishedAt: inv.FinishedAt,
	}); err != nil {
		log.Error("failed to record claude invocation", "stage", stage, "error", err.Error())
	}
}

func truncateOutputForLog(s string) string {
	if len(s) <= maxLoggedOutput {
		return s
	}
	return s[:maxLoggedOutput] + "...(truncated, полный текст — в таблице claude_invocations)"
}

// postComment публикует полный текст ревью, при необходимости разбивая его
// на несколько последовательных комментариев по границам разделов.
func (q *Queue) postComment(ctx context.Context, log *slog.Logger, taskID, output string) {
	chunks := SplitComment(output, maxCommentLen)
	for i, chunk := range chunks {
		if err := q.deps.ClickUp.AddComment(ctx, taskID, chunk); err != nil {
			log.Error("failed to post review comment", "part", i+1, "of", len(chunks), "error", err.Error())
			return
		}
	}
}

// stripVerdictLine убирает служебную строку "ИТОГ: ..." из текста, который
// публикуется комментарием в задаче — читателю в ClickUp она не нужна,
// это чисто машинный контракт для Go. Это единственное разрешённое
// исключение из правила "Go не разбирает текст ревью": строка уже была
// найдена регулярным выражением при разборе вердикта (verdict.Raw),
// здесь просто вырезается её точное вхождение, без анализа остального текста.
func stripVerdictLine(output, raw string) string {
	if raw == "" {
		return output
	}
	idx := strings.LastIndex(output, raw)
	if idx == -1 {
		return output
	}
	return strings.TrimRight(output[:idx], "\n \t")
}

// notifyReviewResult шлёт в Slack и в консоль короткое сообщение о том, что
// сделано: задача и куда она перемещена. Для blocked и внутренних ошибок
// сервиса сообщение отдельное и содержит причину — молчаливый отказ хуже
// ложного срабатывания. Текст сообщения также сохраняется на прогоне (см.
// Store.SetRunSlackMessage), чтобы дашборд мог показать его по клику из
// истории (см. Требование «посмотреть коммент, отправленный в slack»).
func (q *Queue) notifyReviewResult(ctx context.Context, log *slog.Logger, runID int64, task *clickup.Task, verdict review.Verdict, fromStatus, toStatus string, assigneeIDs []int, sessionID string, reviewErr error) {
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
	if err := q.deps.Store.SetRunSlackMessage(ctx, runID, text); err != nil {
		log.Error("failed to record slack message for run", "error", err.Error())
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
