package queue

import (
	"sync"
	"time"
)

// RunOptions — переопределения для одного конкретного прогона поверх
// текущих значений по умолчанию (см. review.Runner.SetModelEffort) —
// например, ручной запуск конкретной задачи с другой моделью/effort через
// веб-интерфейс (см. Требование «выбрать модель для текущей задачи»).
// Пустая строка — не переопределять, использовать значение по умолчанию.
type RunOptions struct {
	Model  string
	Effort string
	// SkipEligibility — не проверять тег/статус триггера (см. isEligible),
	// проверяется только принадлежность задачи настроенному списку.
	// Ставится только при явном ручном запуске конкретного task_id (см.
	// httpapi ManualRunner.RunNow) — в частности, для задач из «Остальных
	// задач в колонке-триггере без тега»: у них по определению нет
	// TRIGGER_TAG, и обычная проверка молча отбросила бы прогон (это и
	// была причина, по которой ручной запуск для таких задач ничего не
	// делал). Вебхук и сверка эту опцию не используют — для них условие
	// триггера остаётся обязательным, как и раньше.
	SkipEligibility bool
}

// queueItem — один элемент буферизованного канала: task_id, признак, что
// это восстановленный после рестарта прогон (см. SubmitResume), и
// переопределения модели/effort на этот конкретный запуск (см. RunOptions).
type queueItem struct {
	taskID string
	resume bool
	opts   RunOptions
}

// Queue — буферизованный канал task_id плюс пул воркеров (WORKER_CONCURRENCY).
// Переполнение буфера не теряет событие безвозвратно: сверка (Требование 1.2)
// подберёт задачу позже.
type Queue struct {
	deps Deps
	ch   chan queueItem
	wg   sync.WaitGroup

	// pauseMu/pausedUntil/pauseReason — общая пауза всей очереди на случай
	// исчерпанного лимита использования claude (см. review.ErrUsageLimit).
	// Пока pausedUntil в будущем, processTask/resumeTask пропускают задачи,
	// не трогая ClickUp и не создавая запись в store — сверка (тикер
	// ReconcileInterval) сама повторит попытку, когда пауза закончится,
	// без отдельного таймера здесь.
	pauseMu     sync.Mutex
	pausedUntil time.Time
	pauseReason string

	// activeMu/active — задачи, обрабатываемые прямо сейчас (см. control.go:
	// RequestCancel/RequestPause/ActiveRuns).
	activeMu sync.Mutex
	active   map[string]*activeRun

	// pendingMu/pending — задачи, лежащие в буфере ch, но ещё не взятые ни
	// одним воркером (см. control.go: Pending).
	pendingMu sync.Mutex
	pending   []string
}

// New создаёт очередь с заданным размером буфера.
func New(deps Deps, bufferSize int) *Queue {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &Queue{deps: deps, ch: make(chan queueItem, bufferSize)}
}

// Submit кладёт task_id в очередь без блокировки. При переполненном буфере
// событие отбрасывается с предупреждением в лог — это безопасно благодаря
// сверке. Вызывается и вебхуком, и тикером сверки, и ручным /api/run — все
// три источника ведут в одну функцию постановки в очередь.
func (q *Queue) Submit(taskID string) bool {
	return q.SubmitWithOptions(taskID, RunOptions{})
}

// SubmitWithOptions — как Submit, но с переопределением модели/effort для
// этого конкретного запуска (см. RunOptions) — используется ручным запуском
// из веб-интерфейса, когда для задачи явно выбрана другая модель/effort.
func (q *Queue) SubmitWithOptions(taskID string, opts RunOptions) bool {
	select {
	case q.ch <- queueItem{taskID: taskID, opts: opts}:
		q.addPending(taskID)
		return true
	default:
		q.deps.Logger.Warn("queue buffer is full, dropping event; reconcile will pick it up later", "task_id", taskID)
		return false
	}
}

// SubmitResume ставит в очередь задачу, прогон которой прервался (контейнер
// упал или был убит сигналом посреди ревью — см. Store.RecoverFromRestart).
// В отличие от Submit, обработка этой задачи **не проверяет условие
// триггера** (тег/статус/список): раз ревью уже было начато, оно доводится
// до конца независимо от того, как сейчас выглядит карточка в ClickUp
// (например, она может застрять в колонке STATUS_RUNNING без тега).
func (q *Queue) SubmitResume(taskID string) bool {
	select {
	case q.ch <- queueItem{taskID: taskID, resume: true}:
		q.addPending(taskID)
		return true
	default:
		q.deps.Logger.Warn("queue buffer is full, dropping resumed task; it will stay stuck until manually retried",
			"task_id", taskID)
		return false
	}
}

// pauseFor ставит всю очередь на паузу минимум до now+d: новые вызовы
// processTask/resumeTask до этого момента становятся no-op. Повторный вызов
// с меньшей длительностью паузу не сокращает — например, если /spec-go и
// /review-go одной и той же задачи оба упёрлись в лимит, действует более
// поздний срок.
func (q *Queue) pauseFor(d time.Duration, reason string) {
	q.pauseMu.Lock()
	defer q.pauseMu.Unlock()
	if until := time.Now().Add(d); until.After(q.pausedUntil) {
		q.pausedUntil = until
		q.pauseReason = reason
	}
}

// pausedFor возвращает, стоит ли очередь на паузе прямо сейчас, и на сколько
// ещё, — для лога при пропуске задачи.
func (q *Queue) pausedFor() (bool, time.Duration, string) {
	q.pauseMu.Lock()
	defer q.pauseMu.Unlock()
	remaining := time.Until(q.pausedUntil)
	if remaining <= 0 {
		return false, 0, ""
	}
	return true, remaining, q.pauseReason
}

// Start запускает workers воркеров, разбирающих канал.
func (q *Queue) Start(workers int) {
	if workers <= 0 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for item := range q.ch {
				q.removePending(item.taskID)
				if item.resume {
					q.resumeTask(item.taskID)
				} else {
					q.processTask(item.taskID, item.opts)
				}
			}
		}()
	}
}

// Shutdown закрывает приём новых задач и ждёт завершения текущих прогонов
// не дольше grace. Вызывающая сторона должна прекратить приём вебхуков и
// остановить сверку до вызова Shutdown, чтобы в канал не летели новые задачи.
func (q *Queue) Shutdown(grace time.Duration) {
	close(q.ch)

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(grace):
		q.deps.Logger.Warn("shutdown grace period elapsed while runs were still in progress")
	}
}
