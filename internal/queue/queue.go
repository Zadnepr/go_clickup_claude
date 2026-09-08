package queue

import (
	"sync"
	"time"
)

// Queue — буферизованный канал task_id плюс пул воркеров (WORKER_CONCURRENCY).
// Переполнение буфера не теряет событие безвозвратно: сверка (Требование 1.2)
// подберёт задачу позже.
type Queue struct {
	deps Deps
	ch   chan string
	wg   sync.WaitGroup
}

// New создаёт очередь с заданным размером буфера.
func New(deps Deps, bufferSize int) *Queue {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &Queue{deps: deps, ch: make(chan string, bufferSize)}
}

// Submit кладёт task_id в очередь без блокировки. При переполненном буфере
// событие отбрасывается с предупреждением в лог — это безопасно благодаря
// сверке. Вызывается и вебхуком, и тикером сверки — оба ведут в одну функцию.
func (q *Queue) Submit(taskID string) bool {
	select {
	case q.ch <- taskID:
		return true
	default:
		q.deps.Logger.Warn("queue buffer is full, dropping event; reconcile will pick it up later", "task_id", taskID)
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
			for taskID := range q.ch {
				q.processTask(taskID)
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
