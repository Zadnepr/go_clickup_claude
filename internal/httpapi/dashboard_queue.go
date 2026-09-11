package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// taskDetailsCacheTTL — сколько держать в кеше результат GetTask для одной
// задачи между опросами GET /api/queue. Дашборд опрашивает этот эндпоинт
// каждые 5 секунд (см. static/dashboard.html: refreshFast); без кеша
// каждый опрос добавляет по одному вызову ClickUp API на КАЖДУЮ задачу в
// «Очереди» и «Текущей задаче» — при глубокой очереди и нескольких
// открытых вкладках дашборда это быстро упирается в rate limit ClickUp API
// (наблюдалось на практике: каскад "clickup api rate limited", из-за
// которого проваливались параллельные операции — уведомление в Slack,
// сверка). Карточка задачи не обязана быть live-актуальной на дашборде
// ежесекундно — устаревание на несколько секунд незаметно и безопасно.
const taskDetailsCacheTTL = 20 * time.Second

// taskDetailsCache — общий на все запросы к одному инстансу mux (создаётся
// один раз в newQueueHandler, а не в каждом запросе) TTL-кеш GetTask.
type taskDetailsCache struct {
	mu   sync.Mutex
	byID map[string]cachedTask
}

type cachedTask struct {
	task *clickup.Task
	err  error
	at   time.Time
}

func newTaskDetailsCache() *taskDetailsCache {
	return &taskDetailsCache{byID: make(map[string]cachedTask)}
}

func (c *taskDetailsCache) Get(ctx context.Context, cu ClickUpReader, taskID string) (*clickup.Task, error) {
	c.mu.Lock()
	if e, ok := c.byID[taskID]; ok && time.Since(e.at) < taskDetailsCacheTTL {
		c.mu.Unlock()
		return e.task, e.err
	}
	c.mu.Unlock()

	task, err := cu.GetTask(ctx, taskID)

	c.mu.Lock()
	c.byID[taskID] = cachedTask{task: task, err: err, at: time.Now()}
	c.mu.Unlock()

	return task, err
}

// activeRunView — одна "текущая задача" в ответе GET /api/queue: то, что
// обрабатывается прямо сейчас (обычно одна, если WORKER_CONCURRENCY=1),
// с прогрессом по этапам и расходом токенов в реальном времени (см.
// Store.UpdateRunningUsage).
type activeRunView struct {
	TaskID       string           `json:"task_id"`
	RunID        int64            `json:"run_id"`
	TaskName     string           `json:"task_name,omitempty"`
	TaskURL      string           `json:"task_url,omitempty"`
	Status       string           `json:"status"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	CostUSD      float64          `json:"cost_usd"`
	Stages       []store.RunStage `json:"stages"`
}

// otherTaskView — одна задача из колонки-триггера без тега-триггера (см.
// Требование «список задач без тега для ручного запуска») — сверка её не
// возьмёт сама, но её можно запустить вручную через дашборд. Assignees —
// текущие исполнители задачи (см. Требование «отобразить аватарки того, кто
// назначен на задачи»), разрешённые в имя/аватар через ClickUp, если это
// удалось (см. loadMembersByID); если участник не нашёлся — отдаётся то,
// что есть (сам ID, без имени и аватара).
type otherTaskView struct {
	TaskID    string      `json:"task_id"`
	CustomID  string      `json:"custom_id,omitempty"`
	Name      string      `json:"name"`
	URL       string      `json:"url"`
	Assignees []memberRef `json:"assignees,omitempty"`
}

// newQueueHandler обрабатывает GET /api/queue: текущая(-ие) задача(-и) в
// обработке прямо сейчас — с прогрессом по этапам и живым расходом
// токенов, для управления (прервать/пауза) — список задач, ожидающих своей
// очереди в буфере (см. Требование «список задач в очереди»), и отдельно —
// остальные задачи в колонке-триггере, у которых просто нет тега-триггера
// (сверка их не подхватит сама, см. Требование «список задач без тега»).
func newQueueHandler(deps Deps) http.HandlerFunc {
	taskCache := newTaskDetailsCache()

	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		activeRuns := deps.Queue.ActiveRuns()
		active := make([]activeRunView, 0, len(activeRuns))
		activeTaskIDs := make(map[string]bool, len(activeRuns))

		for _, ar := range activeRuns {
			activeTaskIDs[ar.TaskID] = true
			view := activeRunView{TaskID: ar.TaskID, RunID: ar.RunID}

			if run, err := deps.Store.GetRun(ctx, ar.RunID); err == nil {
				view.Status = run.Status
				view.InputTokens = run.InputTokens
				view.OutputTokens = run.OutputTokens
				view.CostUSD = run.CostUSD
			} else {
				deps.Logger.Warn("failed to load active run details", "run_id", ar.RunID, "error", err.Error())
			}

			if stages, err := deps.Store.ListStages(ctx, ar.RunID); err == nil {
				view.Stages = stages
			}

			if deps.ClickUp != nil {
				if task, err := taskCache.Get(ctx, deps.ClickUp, ar.TaskID); err == nil {
					view.TaskName = task.Name
					view.TaskURL = task.URL
				}
			}

			active = append(active, view)
		}

		var members map[int]clickup.Member
		loadMembersOnce := func() map[int]clickup.Member {
			if members == nil {
				members = loadMembersByID(ctx, deps)
			}
			return members
		}

		// pending — опрашивается напрямую у ClickUp (тег-триггер + статус-
		// триггер), тем же способом, что и otherToCheck ниже, а не через
		// внутренний буфер очереди (Queue.Pending): тот отражает только то,
		// что этот процесс сам успел поставить в канал, и после рестарта
		// пуст, пока не отработает сверка (до RECONCILE_INTERVAL) — задача с
		// уже добавленным тегом при этом не видна в дашборде, хотя условие
		// триггера выполнено. Хуже того, буфер копил дубли одного и того же
		// task_id, если сверка находила его на нескольких циклах подряд, пока
		// единственный воркер был занят другой проверкой (см. Требование
		// «в очереди много дублей задач, такого не должно быть»). Живой
		// запрос к ClickUp свободен от обеих проблем: каждая задача
		// возвращается ровно один раз и точно отражает доску прямо сейчас.
		// Активная прямо сейчас задача исключается: она уже показана в
		// "Текущей задаче", даже если карточка ещё не успела перейти в
		// STATUS_RUNNING (короткое окно между взятием в обработку и
		// фактическим SetStatus в начале runReview).
		pending := []otherTaskView{}
		if deps.ClickUp != nil && deps.Cfg != nil && deps.Cfg.TriggerTag != "" && deps.Cfg.StatusTrigger != "" {
			tasks, err := deps.ClickUp.ListTasksByTagAndStatus(ctx, deps.Cfg.CUListID, deps.Cfg.TriggerTag, deps.Cfg.StatusTrigger)
			if err != nil {
				deps.Logger.Warn("failed to list queued tasks", "error", err.Error())
			} else {
				members := loadMembersOnce()
				for _, task := range tasks {
					if activeTaskIDs[task.ID] {
						continue
					}
					pending = append(pending, otherTaskView{
						TaskID: task.ID, CustomID: task.CustomID, Name: task.Name, URL: task.URL,
						Assignees: resolveAssigneeRefs(task.Assignees, members),
					})
				}
			}
		}

		otherToCheck := []otherTaskView{}
		if deps.ClickUp != nil && deps.Cfg != nil && deps.Cfg.StatusTrigger != "" {
			tasks, err := deps.ClickUp.ListTasksByStatus(ctx, deps.Cfg.CUListID, deps.Cfg.StatusTrigger)
			if err != nil {
				deps.Logger.Warn("failed to list other tasks in the trigger status", "error", err.Error())
			} else {
				members := loadMembersOnce()
				wantTag := config.NormalizeStatus(deps.Cfg.TriggerTag)
				for _, task := range tasks {
					if taskHasTag(task.Tags, wantTag) {
						continue
					}
					otherToCheck = append(otherToCheck, otherTaskView{
						TaskID: task.ID, CustomID: task.CustomID, Name: task.Name, URL: task.URL,
						Assignees: resolveAssigneeRefs(task.Assignees, members),
					})
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"active":         active,
			"pending":        pending,
			"other_to_check": otherToCheck,
		})
	}
}

func taskHasTag(tags []string, wantNormalized string) bool {
	for _, t := range tags {
		if config.NormalizeStatus(t) == wantNormalized {
			return true
		}
	}
	return false
}

// resolveAssigneeRefs превращает ID исполнителей задачи в memberRef
// (имя+аватар, если участник нашёлся в members — см. loadMembersByID).
// Не найденный участник — не ошибка, отдаётся с одним только ID.
func resolveAssigneeRefs(ids []int, members map[int]clickup.Member) []memberRef {
	if len(ids) == 0 {
		return nil
	}
	refs := make([]memberRef, 0, len(ids))
	for _, id := range ids {
		if m, ok := members[id]; ok {
			refs = append(refs, memberRef{ID: id, Username: m.Username, Avatar: m.Avatar})
		} else {
			refs = append(refs, memberRef{ID: id})
		}
	}
	return refs
}
