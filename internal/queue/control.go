package queue

import (
	"context"
	"sync"
)

// activeRun — управление одним прогоном, обрабатываемым воркером прямо
// сейчас: cancel обрывает контекст (см. RequestCancel — "прервать" в
// веб-интерфейсе), pauseRequested проверяется между этапами runReview
// (см. RequestPause/consumePauseRequest — "поставить на паузу"). runID
// заполняется чуть позже регистрации, когда становится известен
// (см. setActiveRunID) — используется только для отображения на дашборде.
type activeRun struct {
	mu             sync.Mutex
	runID          int64
	cancel         context.CancelFunc
	pauseRequested bool
}

// ActiveRunInfo — то, что видно снаружи про один активный прогон
// (см. GET /api/queue).
type ActiveRunInfo struct {
	TaskID string
	RunID  int64
}

// registerActive регистрирует taskID как обрабатываемый прямо сейчас
// (run_id уже известен — вызывается после ReopenOrEnqueue/TryEnqueue), с
// cancel этого прогона. Снимается парным unregisterActive (defer).
func (q *Queue) registerActive(taskID string, runID int64, cancel context.CancelFunc) {
	q.activeMu.Lock()
	defer q.activeMu.Unlock()
	if q.active == nil {
		q.active = make(map[string]*activeRun)
	}
	q.active[taskID] = &activeRun{runID: runID, cancel: cancel}
}

// unregisterActive снимает регистрацию по завершении обработки (успешном,
// провальном или по прерыванию) — task_id перестаёт быть "текущей задачей".
func (q *Queue) unregisterActive(taskID string) {
	q.activeMu.Lock()
	defer q.activeMu.Unlock()
	delete(q.active, taskID)
}

// ActiveRuns возвращает задачи, обрабатываемые прямо сейчас, — "текущая
// задача (-и)" в терминах веб-интерфейса (обычно одна, если
// WORKER_CONCURRENCY=1).
func (q *Queue) ActiveRuns() []ActiveRunInfo {
	q.activeMu.Lock()
	defer q.activeMu.Unlock()
	out := make([]ActiveRunInfo, 0, len(q.active))
	for taskID, ar := range q.active {
		ar.mu.Lock()
		runID := ar.runID
		ar.mu.Unlock()
		out = append(out, ActiveRunInfo{TaskID: taskID, RunID: runID})
	}
	return out
}

// RequestCancel немедленно обрывает обработку задачи taskID, если она
// сейчас активна (см. "прервать" в веб-интерфейсе): отменяет контекст
// прогона, что убивает текущий подпроцесс claude (exec.CommandContext сам
// это делает при отмене ctx) — дальше прогон идёт по обычному пути реальной
// ошибки обработки (см. Queue.runReview) и помечается failed. Возвращает
// false, если по этому task_id прямо сейчас ничего не выполняется.
func (q *Queue) RequestCancel(taskID string) bool {
	q.activeMu.Lock()
	ar, ok := q.active[taskID]
	q.activeMu.Unlock()
	if !ok {
		return false
	}
	ar.cancel()
	return true
}

// RequestPause просит прогон остановиться после текущего этапа (см.
// "поставить на паузу" в веб-интерфейсе) — в отличие от RequestCancel, не
// обрывает уже идущий вызов claude резко, а даёт ему домолотить текущий шаг
// и остановиться на границе этапов (см. Queue.checkManualPause). Возвращает
// false, если по этому task_id прямо сейчас ничего не выполняется.
func (q *Queue) RequestPause(taskID string) bool {
	q.activeMu.Lock()
	ar, ok := q.active[taskID]
	q.activeMu.Unlock()
	if !ok {
		return false
	}
	ar.mu.Lock()
	ar.pauseRequested = true
	ar.mu.Unlock()
	return true
}

// consumePauseRequest проверяет и одновременно снимает флаг
// pauseRequested — вызывается между этапами runReview (см.
// Queue.checkManualPause). "Снимает" гарантирует, что повторный вызов не
// увидит тот же запрос ещё раз, если пауза почему-то не была применена.
func (q *Queue) consumePauseRequest(taskID string) bool {
	q.activeMu.Lock()
	ar, ok := q.active[taskID]
	q.activeMu.Unlock()
	if !ok {
		return false
	}
	ar.mu.Lock()
	requested := ar.pauseRequested
	ar.pauseRequested = false
	ar.mu.Unlock()
	return requested
}

// addPending/removePending/Pending — задачи, лежащие в буфере канала и ещё
// не взятые ни одним воркером (см. Submit/SubmitResume/Start). Это и есть
// "список задач в очереди" в терминах веб-интерфейса — то, что не видно по
// одним только записям в БД: там строка появляется только тогда, когда
// воркер реально приступил к TryEnqueue, а до этого момента задача может
// какое-то время просто ждать своей очереди в канале (например, если
// WORKER_CONCURRENCY=1 и воркер занят другой проверкой).
func (q *Queue) addPending(taskID string) {
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	q.pending = append(q.pending, taskID)
}

// alreadyQueuedOrActive проверяет, не лежит ли taskID уже в буфере или не
// обрабатывается ли воркером прямо сейчас — вызывается перед постановкой в
// очередь (см. Submit/SubmitWithOptions/SubmitResume). Без этой проверки
// повторный Submit того же task_id (типичный случай — сверка раз за разом
// находит одну и ту же ещё не взятую в обработку задачу, пока единственный
// воркер занят другой проверкой) копит в буфере дубли одного и того же
// task_id: они не портят корректность (ReopenOrEnqueue/TryEnqueue всё равно
// схлопнут повторную обработку в no-op), но зря съедают слоты буфера и
// засоряют "Очередь" в вебе повторами одной и той же задачи.
func (q *Queue) alreadyQueuedOrActive(taskID string) bool {
	q.pendingMu.Lock()
	for _, id := range q.pending {
		if id == taskID {
			q.pendingMu.Unlock()
			return true
		}
	}
	q.pendingMu.Unlock()

	q.activeMu.Lock()
	_, active := q.active[taskID]
	q.activeMu.Unlock()
	return active
}

func (q *Queue) removePending(taskID string) {
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	for i, id := range q.pending {
		if id == taskID {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return
		}
	}
}

// Pending возвращает копию списка задач, ожидающих в буфере очереди.
func (q *Queue) Pending() []string {
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	out := make([]string, len(q.pending))
	copy(out, q.pending)
	return out
}
