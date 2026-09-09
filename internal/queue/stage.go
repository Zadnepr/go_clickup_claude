package queue

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/Zadnepr/go_clickup_claude/internal/review"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// Имена этапов одного прогона ревью (см. Queue.runReview) — в порядке
// выполнения. Каждый — не более одной строки в run_stages на run_id
// (см. Store.FinishStage): при возобновлении прерванного/поставленного на
// паузу прогона (тот же run_id — см. Store.ReopenOrEnqueue) уже готовые
// этапы не выполняются заново, а берутся из БД.
const (
	stageSetup         = "setup"          // перевод в running-статус, снятие исполнителей, уведомление о начале
	stageCommandsCheck = "commands_check" // наличие .claude/commands/{spec,review}.md
	stageGitFetch      = "git_fetch"      // обновление рабочей копии репозитория(-ев)
	stageSpec          = "spec"           // /spec — сбор уточнённого ТЗ
	stageReview        = "review"         // /review — само ревью
	stageDecide        = "decide"         // перевод в колонку по вердикту, назначение исполнителя, снятие тега
	stageComment       = "comment"        // публикация текста ревью комментарием в задаче
)

// setupData — то, что нужно помнить о самом начале прогона: список
// исполнителей, снятых на время проверки. Критично для возобновления —
// после снятия исполнителей свежий GetTask вернёт уже пустой список, и без
// сохранённого значения провал ревью после возобновления вернул бы работу
// не тому (или никому).
type setupData struct {
	OriginalAssignees []int `json:"original_assignees"`
}

// specStageData — результат /spec, сохранённый в run_stages: id сессии
// claude, путь к файлу ТЗ (пусто, если /spec не смогла его создать) и
// потраченные токены. При возобновлении /spec не запускается повторно —
// используется этот сохранённый результат.
type specStageData struct {
	SessionID string            `json:"session_id"`
	SpecPath  string            `json:"spec_path"`
	Usage     review.TokenUsage `json:"usage"`
}

// reviewStageData — результат /review. Вердикт отдельно не хранится —
// восстанавливается из Output через review.ParseVerdict (чистая функция,
// без внешних вызовов), одинаково что для свежего прогона, что для
// восстановленного из БД.
type reviewStageData struct {
	SessionID string            `json:"session_id"`
	Output    string            `json:"output"`
	Stderr    string            `json:"stderr,omitempty"`
	Usage     review.TokenUsage `json:"usage"`
}

// decideStageData — итог Decide(): куда переводить задачу и кого назначать.
type decideStageData struct {
	TargetStatus string `json:"target_status"`
	AssigneeIDs  []int  `json:"assignee_ids"`
}

// stageRun выполняет один этап прогона runID с логированием в run_stages
// (Требование: «каждый этап выполняется поэтапно и логируется в базе»).
//
// Если этап уже отмечен готовым (это и есть возобновление прогона после
// паузы/обрыва — см. Store.ReopenOrEnqueue), fn не вызывается вообще:
// результат берётся из сохранённых данных. Иначе fn выполняется, и по
// результату этап отмечается готовым (с сериализованными данными) или
// проваленным — проваленный этап не считается «уже готовым» и при
// следующей попытке выполняется заново.
//
// Ошибки самого журнала (не удалось прочитать/записать run_stages)
// намеренно не прерывают прогон — тогда стадия просто выполняется заново,
// как будто журнала нет; это доступность важнее точности лога.
func stageRun[T any](ctx context.Context, q *Queue, log *slog.Logger, runID int64, name string, fn func() (T, error)) (data T, reused bool, err error) {
	if existing, ok, getErr := q.deps.Store.GetStage(ctx, runID, name); getErr != nil {
		log.Error("failed to read stage state, running it fresh", "stage", name, "error", getErr.Error())
	} else if ok && existing.Status == store.StageStatusDone {
		var out T
		if unmarshalErr := json.Unmarshal(existing.Data, &out); unmarshalErr != nil {
			log.Error("failed to parse stored stage data, running it fresh", "stage", name, "error", unmarshalErr.Error())
		} else {
			log.Info("stage already completed, reusing stored result", "stage", name)
			return out, true, nil
		}
	}

	if startErr := q.deps.Store.StartStage(ctx, runID, name); startErr != nil {
		log.Error("failed to record stage start", "stage", name, "error", startErr.Error())
	}

	result, fnErr := fn()
	if fnErr != nil {
		if failErr := q.deps.Store.FailStage(ctx, runID, name, fnErr.Error()); failErr != nil {
			log.Error("failed to record stage failure", "stage", name, "error", failErr.Error())
		}
		return result, false, fnErr
	}

	if raw, marshalErr := json.Marshal(result); marshalErr != nil {
		log.Error("failed to marshal stage data", "stage", name, "error", marshalErr.Error())
	} else if finishErr := q.deps.Store.FinishStage(ctx, runID, name, raw); finishErr != nil {
		log.Error("failed to record stage finish", "stage", name, "error", finishErr.Error())
	}
	return result, false, nil
}
