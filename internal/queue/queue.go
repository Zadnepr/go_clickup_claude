package queue

import (
	"sync"
	"time"
)

// queueItem — один элемент буферизованного канала: task_id и признак,
// что это восстановленный после рестарта прогон (см. SubmitResume).
type queueItem struct {
	taskID string
	resume bool
}

// Queue — буферизованный канал task_id плюс пул воркеров (WORKER_CONCURRENCY).
// Переполнение буфера не теряет событие безвозвратно: сверка (Требование 1.2)
// подберёт задачу позже.
type Queue struct {
	deps Deps
	ch   chan queueItem
	wg   sync.WaitGroup
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
	select {
	case q.ch <- queueItem{taskID: taskID}:
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
		return true
	default:
		q.deps.Logger.Warn("queue buffer is full, dropping resumed task; it will stay stuck until manually retried",
			"task_id", taskID)
		return false
	}
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
				if item.resume {
					q.resumeTask(item.taskID)
				} else {
					q.processTask(item.taskID)
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
