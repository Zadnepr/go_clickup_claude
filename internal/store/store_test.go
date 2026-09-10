package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTryEnqueue_Deduplicates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id1, ok1, err := s.TryEnqueue(ctx, "task-1")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if !ok1 {
		t.Fatal("expected first enqueue to succeed")
	}

	// Повторное событие по той же задаче, пока прогон queued — не должно
	// породить второй прогон.
	id2, ok2, err := s.TryEnqueue(ctx, "task-1")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if ok2 {
		t.Fatalf("expected second enqueue to be deduplicated, got new run id %d", id2)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero run id")
	}
}

func TestTryEnqueue_BlockedWhileRunning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, ok, err := s.TryEnqueue(ctx, "task-2")
	if err != nil || !ok {
		t.Fatalf("initial enqueue failed: ok=%v err=%v", ok, err)
	}

	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if _, ok, err := s.TryEnqueue(ctx, "task-2"); err != nil || ok {
		t.Fatalf("expected enqueue blocked while running: ok=%v err=%v", ok, err)
	}
}

// TestTryEnqueue_AllowsReenqueueAfterDone проверяет ключевое свойство:
// задача, уже доведённая до done, не заблокирована навсегда. Тег-триггер
// снимается только на успешном decide (см. Queue.runReview) — то есть уже к
// моменту done тега на задаче нет, и она не может совпасть условием
// триггера сама по себе. Если тег/статус снова совпали — это осознанный
// повторный запрос (человек перетегировал задачу руками после доработки),
// и сверка обязана его подхватить, а не отбросить молча из-за старой
// дедуп-записи.
func TestTryEnqueue_AllowsReenqueueAfterDone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, ok, err := s.TryEnqueue(ctx, "task-2")
	if err != nil || !ok {
		t.Fatalf("initial enqueue failed: ok=%v err=%v", ok, err)
	}
	if err := s.MarkDone(ctx, id, "fail", "session-1", Usage{}); err != nil {
		t.Fatalf("MarkDone error: %v", err)
	}

	newID, ok, err := s.TryEnqueue(ctx, "task-2")
	if err != nil || !ok {
		t.Fatalf("expected a fresh enqueue to be allowed after done: ok=%v err=%v", ok, err)
	}
	if newID == id {
		t.Errorf("expected a new run id distinct from the done run, got the same id %d", id)
	}
}

func TestTryEnqueue_ReopensAfterFailed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, ok, err := s.TryEnqueue(ctx, "task-3")
	if err != nil || !ok {
		t.Fatalf("initial enqueue failed: ok=%v err=%v", ok, err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if err := s.MarkFailed(ctx, id, "session-1", "boom", Usage{}); err != nil {
		t.Fatalf("MarkFailed error: %v", err)
	}

	// Failed не должен блокировать повторную постановку.
	newID, ok, err := s.TryEnqueue(ctx, "task-3")
	if err != nil {
		t.Fatalf("TryEnqueue after failed error: %v", err)
	}
	if !ok {
		t.Fatal("expected enqueue to succeed after previous run failed")
	}
	if newID == id {
		t.Fatal("expected a new run id, not reuse of the failed one")
	}
}

func TestTryEnqueue_ReopensAfterPaused(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, ok, err := s.TryEnqueue(ctx, "task-usage-limit")
	if err != nil || !ok {
		t.Fatalf("initial enqueue failed: ok=%v err=%v", ok, err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if err := s.MarkPaused(ctx, id, "session-1", "usage limit reached", Usage{}); err != nil {
		t.Fatalf("MarkPaused error: %v", err)
	}

	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != StatusPaused {
		t.Errorf("Status = %q, want %q", run.Status, StatusPaused)
	}

	// Paused, как и failed, не должен блокировать повторную постановку —
	// сверка подберёт задачу снова, когда лимит освободится.
	newID, ok, err := s.TryEnqueue(ctx, "task-usage-limit")
	if err != nil {
		t.Fatalf("TryEnqueue after paused error: %v", err)
	}
	if !ok {
		t.Fatal("expected enqueue to succeed after previous run was paused")
	}
	if newID == id {
		t.Fatal("expected a new run id, not reuse of the paused one")
	}
}

func TestRecoverFromRestart(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-4")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}

	recovered, err := s.RecoverFromRestart(ctx)
	if err != nil {
		t.Fatalf("RecoverFromRestart error: %v", err)
	}
	if len(recovered) != 1 || recovered[0].TaskID != "task-4" || recovered[0].RunID != id {
		t.Fatalf("expected [{RunID:%d TaskID:task-4}] recovered, got %+v", id, recovered)
	}

	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != StatusInterrupted {
		t.Errorf("run status = %q, want %q", run.Status, StatusInterrupted)
	}

	// Interrupted — возобновляемый статус: ReopenOrEnqueue должен
	// переоткрыть именно этот run_id, а не создать новый (см.
	// TestReopenOrEnqueue_ReopensInterruptedRun).
	if _, ok, err := s.TryEnqueue(ctx, "task-4"); err != nil || !ok {
		t.Fatalf("expected enqueue to succeed after restart recovery: ok=%v err=%v", ok, err)
	}
}

func TestRecoverFromRestart_MultipleRunningTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, taskID := range []string{"task-a", "task-b"} {
		id, _, err := s.TryEnqueue(ctx, taskID)
		if err != nil {
			t.Fatalf("TryEnqueue(%s) error: %v", taskID, err)
		}
		if err := s.MarkRunning(ctx, id); err != nil {
			t.Fatalf("MarkRunning(%s) error: %v", taskID, err)
		}
	}

	recovered, err := s.RecoverFromRestart(ctx)
	if err != nil {
		t.Fatalf("RecoverFromRestart error: %v", err)
	}
	got := map[string]bool{}
	for _, r := range recovered {
		got[r.TaskID] = true
	}
	if len(recovered) != 2 || !got["task-a"] || !got["task-b"] {
		t.Fatalf("expected [task-a task-b] recovered, got %+v", recovered)
	}
}

func TestPing(t *testing.T) {
	s := openTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping error: %v", err)
	}
}

func TestMarkDone_StoresTokenUsage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-tokens")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkDone(ctx, id, "pass", "sess-1", Usage{InputTokens: 1000, OutputTokens: 200, CostUSD: 0.05}); err != nil {
		t.Fatalf("MarkDone error: %v", err)
	}

	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.InputTokens != 1000 || run.OutputTokens != 200 || run.CostUSD != 0.05 {
		t.Errorf("unexpected token usage on run: %+v", run)
	}
}

func TestListActive_ReturnsOnlyQueuedAndRunning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	queuedID, _, _ := s.TryEnqueue(ctx, "task-a")
	runningID, _, _ := s.TryEnqueue(ctx, "task-b")
	s.MarkRunning(ctx, runningID)
	doneID, _, _ := s.TryEnqueue(ctx, "task-c")
	s.MarkDone(ctx, doneID, "pass", "s", Usage{})

	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive error: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active runs, got %d: %+v", len(active), active)
	}
	ids := map[int64]bool{active[0].ID: true, active[1].ID: true}
	if !ids[queuedID] || !ids[runningID] {
		t.Errorf("expected queued(%d) and running(%d) runs in active list, got %+v", queuedID, runningID, active)
	}
}

func TestStats_AggregatesCountsAndTokens(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id1, _, _ := s.TryEnqueue(ctx, "task-x")
	s.MarkDone(ctx, id1, "pass", "s1", Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.01})

	id2, _, _ := s.TryEnqueue(ctx, "task-y")
	s.MarkDone(ctx, id2, "fail", "s2", Usage{InputTokens: 200, OutputTokens: 20, CostUSD: 0.02})

	id3, _, _ := s.TryEnqueue(ctx, "task-z")
	s.MarkFailed(ctx, id3, "s3", "boom", Usage{InputTokens: 50, OutputTokens: 5, CostUSD: 0.005})

	stats, err := s.Stats(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Stats error: %v", err)
	}
	if stats.TotalRuns != 3 {
		t.Fatalf("expected 3 runs, got %d", stats.TotalRuns)
	}
	if stats.ByStatus[StatusDone] != 2 || stats.ByStatus[StatusFailed] != 1 {
		t.Errorf("unexpected ByStatus: %+v", stats.ByStatus)
	}
	if stats.ByVerdict["pass"] != 1 || stats.ByVerdict["fail"] != 1 {
		t.Errorf("unexpected ByVerdict: %+v", stats.ByVerdict)
	}
	if stats.Tokens.InputTokens != 350 || stats.Tokens.OutputTokens != 35 {
		t.Errorf("unexpected token totals: %+v", stats.Tokens)
	}
	if stats.Tokens.CostUSD < 0.0349 || stats.Tokens.CostUSD > 0.0351 {
		t.Errorf("unexpected cost total: %v", stats.Tokens.CostUSD)
	}
	if len(stats.Runs) != 3 {
		t.Errorf("expected 3 runs in log, got %d", len(stats.Runs))
	}
}

func TestStats_ExcludesRunsBeforeSince(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, _ := s.TryEnqueue(ctx, "task-old")
	s.MarkDone(ctx, id, "pass", "s", Usage{})

	stats, err := s.Stats(ctx, time.Now().Add(time.Hour)) // since в будущем
	if err != nil {
		t.Fatalf("Stats error: %v", err)
	}
	if stats.TotalRuns != 0 {
		t.Errorf("expected 0 runs when since is in the future, got %d", stats.TotalRuns)
	}
}

func TestReopenOrEnqueue_NoExistingRun_BehavesLikeTryEnqueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, ok, err := s.ReopenOrEnqueue(ctx, "task-fresh")
	if err != nil || !ok {
		t.Fatalf("ReopenOrEnqueue error: %v ok=%v", err, ok)
	}
	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != StatusQueued {
		t.Errorf("status = %q, want %q", run.Status, StatusQueued)
	}
}

func TestReopenOrEnqueue_ReopensInterruptedRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-interrupted")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if _, err := s.RecoverFromRestart(ctx); err != nil {
		t.Fatalf("RecoverFromRestart error: %v", err)
	}

	// Этап, пройденный до обрыва — должен пережить переоткрытие того же run_id.
	if err := s.StartStage(ctx, id, "spec"); err != nil {
		t.Fatalf("StartStage error: %v", err)
	}
	if err := s.FinishStage(ctx, id, "spec", []byte(`{"session_id":"s1"}`)); err != nil {
		t.Fatalf("FinishStage error: %v", err)
	}

	reopenedID, ok, err := s.ReopenOrEnqueue(ctx, "task-interrupted")
	if err != nil || !ok {
		t.Fatalf("ReopenOrEnqueue error: %v ok=%v", err, ok)
	}
	if reopenedID != id {
		t.Fatalf("expected the same run_id %d to be reopened, got %d", id, reopenedID)
	}

	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != StatusRunning {
		t.Errorf("status = %q, want %q", run.Status, StatusRunning)
	}

	stage, ok, err := s.GetStage(ctx, id, "spec")
	if err != nil || !ok {
		t.Fatalf("GetStage error: %v ok=%v", err, ok)
	}
	if stage.Status != StageStatusDone {
		t.Errorf("stage status = %q, want %q — the completed stage must survive reopening", stage.Status, StageStatusDone)
	}
}

func TestReopenOrEnqueue_ReopensPausedRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-paused")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if err := s.MarkPaused(ctx, id, "sess", "usage limit reached", Usage{}); err != nil {
		t.Fatalf("MarkPaused error: %v", err)
	}

	reopenedID, ok, err := s.ReopenOrEnqueue(ctx, "task-paused")
	if err != nil || !ok {
		t.Fatalf("ReopenOrEnqueue error: %v ok=%v", err, ok)
	}
	if reopenedID != id {
		t.Fatalf("expected the same run_id %d to be reopened, got %d", id, reopenedID)
	}
}

func TestReopenOrEnqueue_DoesNotReopenGenuinelyFailedRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-failed")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	if err := s.MarkFailed(ctx, id, "sess", "git auth failure", Usage{}); err != nil {
		t.Fatalf("MarkFailed error: %v", err)
	}

	// Настоящая ошибка обработки — это не повод молча переиспользовать
	// состояние сломанной попытки: новый прогон должен начаться с чистого
	// листа (новый run_id), а не переоткрыть failed-прогон.
	newID, ok, err := s.ReopenOrEnqueue(ctx, "task-failed")
	if err != nil || !ok {
		t.Fatalf("ReopenOrEnqueue error: %v ok=%v", err, ok)
	}
	if newID == id {
		t.Fatal("expected a new run_id, not reuse of a genuinely failed run")
	}
}

func TestStage_StartFinishGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-stage")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}

	if _, ok, err := s.GetStage(ctx, id, "spec"); err != nil || ok {
		t.Fatalf("expected no stage yet: ok=%v err=%v", ok, err)
	}

	if err := s.StartStage(ctx, id, "spec"); err != nil {
		t.Fatalf("StartStage error: %v", err)
	}
	stage, ok, err := s.GetStage(ctx, id, "spec")
	if err != nil || !ok {
		t.Fatalf("GetStage error: %v ok=%v", err, ok)
	}
	if stage.Status != StageStatusRunning {
		t.Errorf("status = %q, want %q", stage.Status, StageStatusRunning)
	}

	if err := s.FinishStage(ctx, id, "spec", []byte(`{"session_id":"abc"}`)); err != nil {
		t.Fatalf("FinishStage error: %v", err)
	}
	stage, ok, err = s.GetStage(ctx, id, "spec")
	if err != nil || !ok {
		t.Fatalf("GetStage error: %v ok=%v", err, ok)
	}
	if stage.Status != StageStatusDone {
		t.Errorf("status = %q, want %q", stage.Status, StageStatusDone)
	}
	if string(stage.Data) != `{"session_id":"abc"}` {
		t.Errorf("data = %s, want the stored JSON", stage.Data)
	}
}

func TestFailStage_MarksFailedNotDone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.TryEnqueue(ctx, "task-stage-fail")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.StartStage(ctx, id, "review"); err != nil {
		t.Fatalf("StartStage error: %v", err)
	}
	if err := s.FailStage(ctx, id, "review", "usage limit reached"); err != nil {
		t.Fatalf("FailStage error: %v", err)
	}

	stage, ok, err := s.GetStage(ctx, id, "review")
	if err != nil || !ok {
		t.Fatalf("GetStage error: %v ok=%v", err, ok)
	}
	if stage.Status != StageStatusFailed {
		t.Errorf("status = %q, want %q", stage.Status, StageStatusFailed)
	}
	if stage.Error != "usage limit reached" {
		t.Errorf("error = %q, want %q", stage.Error, "usage limit reached")
	}
}

func TestRecordInvocation_StoresFullRequestAndResponse(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	runID, _, err := s.TryEnqueue(ctx, "task-invocation")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}

	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	finished := time.Now().UTC().Truncate(time.Second)
	inv := ClaudeInvocation{
		RunID: runID, Stage: "spec", SessionID: "sess-1", Model: "haiku", Effort: "low",
		Prompt: "/spec\nhttps://...", Output: "собранное ТЗ", Stderr: "", Subtype: "success", IsError: false,
		InputTokens: 100, OutputTokens: 20, CostUSD: 0.005, StartedAt: started, FinishedAt: finished,
	}
	id, err := s.RecordInvocation(ctx, inv)
	if err != nil {
		t.Fatalf("RecordInvocation error: %v", err)
	}
	if id == 0 {
		t.Fatal("expected a non-zero invocation id")
	}

	invocations, err := s.ListInvocations(ctx, runID)
	if err != nil {
		t.Fatalf("ListInvocations error: %v", err)
	}
	if len(invocations) != 1 {
		t.Fatalf("expected 1 invocation, got %d", len(invocations))
	}
	got := invocations[0]
	if got.SessionID != "sess-1" || got.Model != "haiku" || got.Effort != "low" {
		t.Errorf("unexpected metadata: %+v", got)
	}
	if got.Prompt != inv.Prompt || got.Output != inv.Output {
		t.Errorf("expected full prompt/output to round-trip, got: %+v", got)
	}
	if got.InputTokens != 100 || got.OutputTokens != 20 || got.CostUSD != 0.005 {
		t.Errorf("unexpected token usage: %+v", got)
	}
	if !got.StartedAt.Equal(started) || !got.FinishedAt.Equal(finished) {
		t.Errorf("unexpected timestamps: started=%v finished=%v", got.StartedAt, got.FinishedAt)
	}
}

func TestListInvocations_OrderedChronologically(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	runID, _, err := s.TryEnqueue(ctx, "task-invocation-order")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}

	for _, stage := range []string{"spec", "review"} {
		if _, err := s.RecordInvocation(ctx, ClaudeInvocation{
			RunID: runID, Stage: stage, StartedAt: time.Now(), FinishedAt: time.Now(),
		}); err != nil {
			t.Fatalf("RecordInvocation(%s) error: %v", stage, err)
		}
	}

	invocations, err := s.ListInvocations(ctx, runID)
	if err != nil {
		t.Fatalf("ListInvocations error: %v", err)
	}
	if len(invocations) != 2 || invocations[0].Stage != "spec" || invocations[1].Stage != "review" {
		t.Fatalf("expected [spec review] in order, got: %+v", invocations)
	}
}

func TestListInvocationsSince_ExcludesOlderInvocations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	runA, _, err := s.TryEnqueue(ctx, "task-old")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if _, err := s.RecordInvocation(ctx, ClaudeInvocation{RunID: runA, Stage: "spec", StartedAt: old, FinishedAt: old}); err != nil {
		t.Fatalf("RecordInvocation error: %v", err)
	}

	runB, _, err := s.TryEnqueue(ctx, "task-new")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	recent := time.Now()
	if _, err := s.RecordInvocation(ctx, ClaudeInvocation{RunID: runB, Stage: "spec", StartedAt: recent, FinishedAt: recent}); err != nil {
		t.Fatalf("RecordInvocation error: %v", err)
	}

	invocations, err := s.ListInvocationsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListInvocationsSince error: %v", err)
	}
	if len(invocations) != 1 || invocations[0].RunID != runB {
		t.Fatalf("expected only the recent invocation, got: %+v", invocations)
	}
}

func TestUpdateRunningUsage_UpdatesWithoutChangingStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	runID, _, err := s.TryEnqueue(ctx, "task-live-usage")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if err := s.MarkRunning(ctx, runID); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}

	if err := s.UpdateRunningUsage(ctx, runID, Usage{InputTokens: 1000, OutputTokens: 200, CostUSD: 0.02}); err != nil {
		t.Fatalf("UpdateRunningUsage error: %v", err)
	}

	run, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != StatusRunning {
		t.Errorf("expected status to stay running, got %q", run.Status)
	}
	if run.InputTokens != 1000 || run.OutputTokens != 200 || run.CostUSD != 0.02 {
		t.Errorf("unexpected live usage: %+v", run)
	}
}

func TestGetSetting_NotSet_ReturnsOkFalse(t *testing.T) {
	s := openTestStore(t)
	_, ok, err := s.GetSetting(context.Background(), "claude_model")
	if err != nil {
		t.Fatalf("GetSetting error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for an unset setting")
	}
}

func TestSetSetting_ThenGetSetting_RoundTrips(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetSetting(ctx, "claude_model", "haiku"); err != nil {
		t.Fatalf("SetSetting error: %v", err)
	}
	value, ok, err := s.GetSetting(ctx, "claude_model")
	if err != nil {
		t.Fatalf("GetSetting error: %v", err)
	}
	if !ok || value != "haiku" {
		t.Errorf("value = %q, ok = %v, want haiku/true", value, ok)
	}
}

func TestSetSetting_OverwritesExistingValue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SetSetting(ctx, "claude_effort", "low"); err != nil {
		t.Fatalf("SetSetting error: %v", err)
	}
	if err := s.SetSetting(ctx, "claude_effort", "high"); err != nil {
		t.Fatalf("SetSetting error: %v", err)
	}
	value, ok, err := s.GetSetting(ctx, "claude_effort")
	if err != nil {
		t.Fatalf("GetSetting error: %v", err)
	}
	if !ok || value != "high" {
		t.Errorf("value = %q, ok = %v, want high/true", value, ok)
	}
}

func TestMigrateTokenColumns_IdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open error: %v", err)
	}
	id, _, _ := s1.TryEnqueue(context.Background(), "task-1")
	s1.MarkDone(context.Background(), id, "pass", "s", Usage{InputTokens: 42})
	s1.Close()

	// Переоткрытие уже существующей базы не должно падать на повторной миграции.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open error: %v", err)
	}
	defer s2.Close()

	run, err := s2.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.InputTokens != 42 {
		t.Errorf("expected token data to survive reopen, got: %+v", run)
	}
}
