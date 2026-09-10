package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/review"
	"github.com/Zadnepr/go_clickup_claude/internal/slack"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// fakeClickUp — фейковая реализация интерфейса ClickUp для тестов воркера.
type fakeClickUp struct {
	mu               sync.Mutex
	task             *clickup.Task
	statuses         []string
	assignees        [][]int
	removedAssignees [][]int
	removedTags      []string
	comments         []string
}

func (f *fakeClickUp) GetTask(ctx context.Context, taskID string) (*clickup.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.task
	return &cp, nil
}

func (f *fakeClickUp) SetStatus(ctx context.Context, taskID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeClickUp) AddAssignees(ctx context.Context, taskID string, userIDs []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assignees = append(f.assignees, userIDs)
	return nil
}

func (f *fakeClickUp) RemoveAssignees(ctx context.Context, taskID string, userIDs []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedAssignees = append(f.removedAssignees, userIDs)
	return nil
}

func (f *fakeClickUp) RemoveTag(ctx context.Context, taskID, tagName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedTags = append(f.removedTags, tagName)
	return nil
}

func (f *fakeClickUp) AddComment(ctx context.Context, taskID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, text)
	return nil
}

// fakeRunner — фейковая реализация интерфейса Runner.
type fakeRunner struct {
	specErr       error
	specContent   string // то, что вернёт RunSpec как собранное ТЗ ("" — /spec не собрала ТЗ)
	specUsage     review.TokenUsage
	result        review.Result
	reviewErr     error
	gitErr        error
	gotSpecPath   string // specPath, с которым реально вызвали RunReview
	reviewCalled  bool   // была ли вызвана RunReview (нужно проверять, что её пропустили)
	specCalled    bool   // была ли вызвана RunSpec (нужно проверять, что её пропустили при возобновлении)
	afterSpec     func() // если задано, вызывается в конце RunSpec — используется, чтобы смоделировать RequestPause/RequestCancel "между этапами" в тестах
	gotSpecOpts   review.CallOptions
	gotReviewOpts review.CallOptions

	modelMu sync.Mutex
	model   string
	effort  string

	specDirOnce sync.Once
	specDir     string
}

func (f *fakeRunner) GitFetch(ctx context.Context) error { return f.gitErr }

func (f *fakeRunner) RunSpec(ctx context.Context, taskURL string, opts review.CallOptions) (review.SpecResult, error) {
	f.specCalled = true
	f.gotSpecOpts = opts
	if f.afterSpec != nil {
		f.afterSpec()
	}
	// Как и настоящий Runner (exec.CommandContext убивает подпроцесс claude
	// при отмене ctx, что возвращается как ошибка) — если ctx уже отменён
	// (см. Queue.RequestCancel), вызов должен считаться неуспешным.
	if err := ctx.Err(); err != nil {
		return review.SpecResult{}, err
	}
	return review.SpecResult{SessionID: "spec-session", Content: f.specContent, Usage: f.specUsage}, f.specErr
}

func (f *fakeRunner) RunReview(ctx context.Context, taskURL, specPath string, opts review.CallOptions) (review.Result, error) {
	f.reviewCalled = true
	f.gotSpecPath = specPath
	f.gotReviewOpts = opts
	if err := ctx.Err(); err != nil {
		return review.Result{}, err
	}
	return f.result, f.reviewErr
}

func (f *fakeRunner) SetModelEffort(model, effort string) {
	f.modelMu.Lock()
	defer f.modelMu.Unlock()
	f.model, f.effort = model, effort
}

func (f *fakeRunner) ModelEffort() (string, string) {
	f.modelMu.Lock()
	defer f.modelMu.Unlock()
	return f.model, f.effort
}

// SpecFilePath отдаёт путь во временном каталоге, приватном для этого
// fakeRunner, — воркер пишет туда файл ТЗ по-настоящему (Queue.ensureSpecFile
// делает реальный os.WriteFile), поэтому путь должен существовать и быть
// доступным для записи, а не просто различаться "существует/не существует",
// как было при прежней (файловой) схеме.
func (f *fakeRunner) SpecFilePath(taskID string) string {
	f.specDirOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fake-spec")
		if err != nil {
			panic(err)
		}
		f.specDir = dir
	})
	return filepath.Join(f.specDir, taskID+".md")
}

func newTestDeps(t *testing.T, cu *fakeClickUp, runner *fakeRunner, cfg *config.Config) (Deps, *httptest.Server, *[]slackCall) {
	t.Helper()

	var calls []slackCall
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text   string        `json:"text"`
			Blocks []slack.Block `json:"blocks"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, slackCall{Text: body.Text, Blocks: body.Blocks})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	deps := Deps{
		ClickUp: cu,
		Store:   st,
		Slack:   slack.NewNotifier(srv.URL),
		Runner:  runner,
		Cfg:     cfg,
		Logger:  slog.New(slog.NewTextHandler(nil2Writer{}, nil)),
	}
	return deps, srv, &calls
}

type slackCall struct {
	Text   string
	Blocks []slack.Block
}

// nil2Writer отбрасывает лог-вывод в тестах, чтобы не засорять их вывод.
type nil2Writer struct{}

func (nil2Writer) Write(p []byte) (int, error) { return len(p), nil }

// repoWithCommands создаёт временный каталог с .claude/commands/{spec,review}.md,
// чтобы missingCommands не блокировал прогон в тестах, которым это не нужно.
func repoWithCommands(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatalf("mkdir commands dir: %v", err)
	}
	for _, name := range requiredCommands {
		if err := os.WriteFile(filepath.Join(cmdDir, name), []byte("# stub"), 0o644); err != nil {
			t.Fatalf("write stub command %s: %v", name, err)
		}
	}
	return dir
}

func testConfig(t *testing.T) *config.Config {
	return &config.Config{
		CUListID:          "list1",
		TriggerTag:        "ai",
		StatusTrigger:     "to check",
		StatusRunning:     "checking",
		StatusPass:        "done",
		StatusFail:        "to fix",
		AssigneeOnFail:    "20",
		WorkerConcurrency: 1,
		ReviewTimeout:     10 * time.Second,
		UsageLimitPause:   time.Minute,
		RepoPath:          repoWithCommands(t),
	}
}

func TestProcessTask_PassVerdict_FullPipeline(t *testing.T) {
	task := &clickup.Task{ID: "1", Name: "Task 1", URL: "https://app.clickup.com/t/1", ListID: "list1", Tags: []string{"ai"}, Status: "to check", CreatorID: 5}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:    "Всё отлично.\nИТОГ: критичных=0 важных=0 минор=1 статус=pass",
			SessionID: "sess-1",
			Verdict:   review.Verdict{Status: review.StatusPass, Minor: 1, Raw: "ИТОГ: критичных=0 важных=0 минор=1 статус=pass"},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("1", RunOptions{})

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.statuses) != 2 || cu.statuses[0] != "checking" || cu.statuses[1] != "done" {
		t.Errorf("unexpected status transitions: %+v", cu.statuses)
	}
	if len(cu.assignees) != 0 {
		t.Errorf("expected no assignee change on pass, got: %+v", cu.assignees)
	}
	if len(cu.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(cu.comments))
	}

	if len(*calls) != 2 {
		t.Fatalf("expected 2 slack notifications (started + finished), got %d", len(*calls))
	}
}

func TestProcessTask_FailVerdict_AssignsConfiguredUser(t *testing.T) {
	task := &clickup.Task{ID: "2", Name: "Task 2", URL: "https://app.clickup.com/t/2", ListID: "list1", Tags: []string{"ai"}, Status: "to check", CreatorID: 5}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		result: review.Result{
			Output:    "Есть проблема.\nИТОГ: критичных=1 важных=0 минор=0 статус=fail",
			SessionID: "sess-2",
			Verdict:   review.Verdict{Status: review.StatusFail, Critical: 1, Raw: "x"},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("2", RunOptions{})

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.statuses) != 2 || cu.statuses[1] != "to fix" {
		t.Errorf("unexpected status transitions: %+v", cu.statuses)
	}
	if len(cu.assignees) != 1 || len(cu.assignees[0]) != 1 || cu.assignees[0][0] != 20 {
		t.Errorf("expected assignee 20 (ASSIGNEE_ON_FAIL), got: %+v", cu.assignees)
	}
}

func TestProcessTask_ReviewProcessError_MarksRunFailedAndNotifies(t *testing.T) {
	task := &clickup.Task{
		ID: "3", Name: "Task 3", URL: "https://app.clickup.com/t/3",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		result:    review.Result{Verdict: review.Verdict{Status: review.StatusBlocked}},
		reviewErr: context.DeadlineExceeded,
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("3", RunOptions{})

	if len(*calls) != 2 {
		t.Fatalf("expected 2 slack notifications (started + service error), got %d", len(*calls))
	}

	// Реальный сбой процесса — не результат ревью: статус (кроме перехода
	// в running в начале), тег и исполнителя трогать нельзя, иначе задача
	// получит ложный вердикт из-за инфраструктурной проблемы (протухший
	// токен, таймаут и т.п.), а не реальной проверки кода.
	cu.mu.Lock()
	if len(cu.statuses) != 1 || cu.statuses[0] != "checking" {
		t.Errorf("expected only the running-status transition, got: %+v", cu.statuses)
	}
	if len(cu.assignees) != 0 {
		t.Errorf("expected no assignee change on a process error, got: %+v", cu.assignees)
	}
	if len(cu.removedTags) != 0 {
		t.Errorf("expected trigger tag to stay on a process error, got: %+v", cu.removedTags)
	}
	cu.mu.Unlock()

	// Прогон должен быть отмечен как failed и не блокировать повторную постановку.
	runID, enqueued, err := deps.Store.TryEnqueue(context.Background(), "3")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if !enqueued {
		t.Fatal("expected task to be re-enqueueable after a failed run")
	}
	_ = runID
}

func TestProcessTask_UsageLimitFromReview_PausesQueueAndRestoresState(t *testing.T) {
	task := &clickup.Task{
		ID: "4", Name: "Task 4", URL: "https://app.clickup.com/t/4",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check", Assignees: []int{7},
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		reviewErr: fmt.Errorf("/review run failed: %w: usage limit reached", review.ErrUsageLimit),
	}

	cfg := testConfig(t)
	deps, _, calls := newTestDeps(t, cu, runner, cfg)
	q := New(deps, 10)
	q.processTask("4", RunOptions{})

	// Карточка должна вернуться в исходное состояние: статус — обратно на
	// триггерный, снятый на время проверки исполнитель — восстановлен.
	// Иначе задача застрянет в running-колонке до ручного вмешательства,
	// хотя лимит освободится сам.
	cu.mu.Lock()
	if len(cu.statuses) != 2 || cu.statuses[0] != cfg.StatusRunning || cu.statuses[1] != cfg.StatusTrigger {
		t.Errorf("expected running then back to trigger status, got: %+v", cu.statuses)
	}
	if len(cu.assignees) != 1 || len(cu.assignees[0]) != 1 || cu.assignees[0][0] != 7 {
		t.Errorf("expected original assignee [7] restored, got: %+v", cu.assignees)
	}
	if len(cu.removedTags) != 0 {
		t.Errorf("trigger tag must stay on a usage-limit pause, got: %+v", cu.removedTags)
	}
	cu.mu.Unlock()

	if paused, remaining, _ := q.pausedFor(); !paused || remaining <= 0 {
		t.Errorf("expected queue to be paused, paused=%v remaining=%v", paused, remaining)
	}

	// Failed/paused-прогон не блокирует повторную постановку — сверка
	// подберёт задачу снова, когда пауза очереди закончится.
	if _, enqueued, err := deps.Store.TryEnqueue(context.Background(), "4"); err != nil || !enqueued {
		t.Fatalf("expected task to be re-enqueueable after a usage-limit pause: enqueued=%v err=%v", enqueued, err)
	}

	foundPausedNotice := false
	for _, c := range *calls {
		if strings.Contains(c.Text, "паузе") {
			foundPausedNotice = true
		}
	}
	if !foundPausedNotice {
		t.Errorf("expected a slack notification about the pause, got: %+v", *calls)
	}
}

func TestProcessTask_UsageLimitFromSpec_SkipsReviewAndPauses(t *testing.T) {
	task := &clickup.Task{
		ID: "5", Name: "Task 5", URL: "https://app.clickup.com/t/5",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specErr: fmt.Errorf("/spec run failed: %w: usage limit reached", review.ErrUsageLimit),
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("5", RunOptions{})

	if runner.reviewCalled {
		t.Error("expected /review not to run when /spec already hit the usage limit")
	}
	if paused, _, _ := q.pausedFor(); !paused {
		t.Error("expected queue to be paused after /spec usage-limit error")
	}
}

func TestProcessTask_SkippedWhilePaused(t *testing.T) {
	task := &clickup.Task{
		ID: "6", Name: "Task 6", URL: "https://app.clickup.com/t/6",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		result: review.Result{Verdict: review.Verdict{Status: review.StatusPass}},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.pauseFor(time.Minute, "test pause")

	q.processTask("6", RunOptions{})

	cu.mu.Lock()
	if len(cu.statuses) != 0 {
		t.Errorf("expected no ClickUp interaction while paused, got statuses: %+v", cu.statuses)
	}
	cu.mu.Unlock()
	if len(*calls) != 0 {
		t.Errorf("expected no slack notifications while paused, got: %+v", *calls)
	}

	// Пропуск не должен создавать dedup-запись — иначе после окончания паузы
	// сверка не сможет поставить задачу в очередь заново.
	if _, enqueued, err := deps.Store.TryEnqueue(context.Background(), "6"); err != nil || !enqueued {
		t.Fatalf("expected task to remain enqueueable while paused: enqueued=%v err=%v", enqueued, err)
	}
}

func TestProcessTask_NotEligible_SkipsWithoutDedupRecord(t *testing.T) {
	task := &clickup.Task{ID: "4", Name: "Task 4", URL: "https://app.clickup.com/t/4", ListID: "list1", Tags: []string{"other"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("4", RunOptions{})

	if len(*calls) != 0 {
		t.Fatalf("expected no slack notification for ineligible task, got %d", len(*calls))
	}

	// Не должно остаться дедуп-записи: постановка должна быть возможна снова.
	_, enqueued, err := deps.Store.TryEnqueue(context.Background(), "4")
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	if !enqueued {
		t.Fatal("expected no dedup record to have been left for an ineligible task")
	}
}

func TestProcessTask_SkipEligibility_ProcessesTaskWithoutTriggerTag(t *testing.T) {
	// Задачи из "Остальные задачи в колонке-триггере без тега" (дашборд) —
	// по определению без TRIGGER_TAG; раньше ручной запуск для них молча
	// ничего не делал (processTask отбрасывал их на проверке isEligible),
	// хотя HTTP-ответ уже успевал вернуть 202 (см. отзыв: "ручной запуск...
	// не работает"). SkipEligibility — явное исключение именно для этого.
	task := &clickup.Task{
		ID: "40", Name: "Task 40", URL: "https://app.clickup.com/t/40",
		ListID: "list1", Tags: []string{"unrelated"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("40", RunOptions{SkipEligibility: true})

	if !runner.reviewCalled {
		t.Fatal("expected the run to proceed despite the missing trigger tag")
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 slack notifications (started + finished), got %d", len(*calls))
	}
}

func TestProcessTask_SkipEligibility_StillRefusesWrongList(t *testing.T) {
	task := &clickup.Task{
		ID: "41", Name: "Task 41", URL: "https://app.clickup.com/t/41",
		ListID: "some-other-list", Tags: []string{"unrelated"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("41", RunOptions{SkipEligibility: true})

	if runner.specCalled || runner.reviewCalled {
		t.Fatal("expected a task from a different list to be refused even with SkipEligibility")
	}
	if len(*calls) != 0 {
		t.Fatalf("expected no slack notifications, got %d", len(*calls))
	}
}

func TestProcessTask_SkipsWhileAlreadyRunning(t *testing.T) {
	// Настоящая защита от дубликата события (два вебхука на одно и то же
	// изменение почти одновременно) — active-статус (queued/running) уже
	// занятого прогона, а не сам факт, что задача когда-либо проверялась
	// (см. TestProcessTask_ReprocessesAfterPreviousRunDone).
	task := &clickup.Task{ID: "5", Name: "Task 5", URL: "https://app.clickup.com/t/5", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	runID := mustEnqueue(t, deps, "5")
	if err := deps.Store.MarkRunning(context.Background(), runID); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}

	q := New(deps, 10)
	q.processTask("5", RunOptions{})

	if len(*calls) != 0 {
		t.Fatalf("expected no processing while a run for this task is already active, got %d slack calls", len(*calls))
	}
	if runner.reviewCalled {
		t.Error("expected RunReview not to be called for an already-running task")
	}
}

func TestProcessTask_ReprocessesAfterPreviousRunDone(t *testing.T) {
	// Тег-триггер снимается только при успешном decide — то есть done
	// означает, что тег уже был снят. Если тег/статус на задаче снова
	// совпали с условием (в тесте — статический fakeClickUp, что
	// эквивалентно человеку, перетегировавшему задачу заново), это
	// осознанный повторный запрос, а не эхо старого события: done не
	// должен блокировать постановку навсегда (см. Store.TryEnqueue).
	task := &clickup.Task{ID: "5", Name: "Task 5", URL: "https://app.clickup.com/t/5", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)

	q.processTask("5", RunOptions{})
	q.processTask("5", RunOptions{}) // тег/статус выставлены заново после первой проверки

	if len(*calls) != 4 {
		t.Fatalf("expected 2 full runs worth of notifications (started+finished twice), got %d", len(*calls))
	}
}

func TestProcessTask_RemovesAssigneesBeforeCheck(t *testing.T) {
	task := &clickup.Task{
		ID: "6", Name: "Task 6", URL: "https://app.clickup.com/t/6",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
		Assignees: []int{7, 8},
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("6", RunOptions{})

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.removedAssignees) != 1 || len(cu.removedAssignees[0]) != 2 {
		t.Fatalf("expected original assignees [7 8] to be removed before the check, got: %+v", cu.removedAssignees)
	}
}

func TestProcessTask_Fail_FallsBackToCreatorWhenNothingConfigured(t *testing.T) {
	task := &clickup.Task{
		ID: "7", Name: "Task 7", URL: "https://app.clickup.com/t/7",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
		Assignees: []int{7, 8}, CreatorID: 99,
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "плохо\nИТОГ: критичных=1 важных=0 минор=0 статус=fail",
			Verdict: review.Verdict{Status: review.StatusFail, Critical: 1},
		},
	}

	cfg := testConfig(t)
	// Ни custom field Developer, ни ASSIGNEE_ON_FAIL/ASSIGNEE_ON_PASS не
	// заданы — снятые на время проверки исходные исполнители больше не
	// восстанавливаются автоматически (см. Decide): последним резервом
	// остаётся создатель задачи.
	cfg.AssigneeOnFail = ""
	deps, _, _ := newTestDeps(t, cu, runner, cfg)
	q := New(deps, 10)
	q.processTask("7", RunOptions{})

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.assignees) != 1 || len(cu.assignees[0]) != 1 || cu.assignees[0][0] != 99 {
		t.Errorf("expected fallback to the task creator (99), got: %+v", cu.assignees)
	}
}

func TestProcessTask_Fail_PrefersDeveloperCustomFieldOverAssigneeOnFail(t *testing.T) {
	task := &clickup.Task{
		ID: "22", Name: "Task 22", URL: "https://app.clickup.com/t/22",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
		Assignees: []int{7, 8}, CreatorID: 99, DeveloperIDs: []int{81838052},
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "плохо\nИТОГ: критичных=1 важных=0 минор=0 статус=fail",
			Verdict: review.Verdict{Status: review.StatusFail, Critical: 1},
		},
	}

	// AssigneeOnFail сконфигурирован (testConfig задаёт "20"), но custom
	// field Developer на задаче важнее — именно он должен победить.
	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("22", RunOptions{})

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.assignees) != 1 || len(cu.assignees[0]) != 1 || cu.assignees[0][0] != 81838052 {
		t.Errorf("expected custom field Developer (81838052) to take priority over ASSIGNEE_ON_FAIL, got: %+v", cu.assignees)
	}
}

func TestProcessTask_RemovesTriggerTagAfterFinishing(t *testing.T) {
	task := &clickup.Task{ID: "8", Name: "Task 8", URL: "https://app.clickup.com/t/8", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("8", RunOptions{})

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.removedTags) != 1 || cu.removedTags[0] != "ai" {
		t.Fatalf("expected trigger tag 'ai' to be removed after finishing, got: %+v", cu.removedTags)
	}
}

func TestProcessTask_MissingRequiredCommands_AbortsWithoutRunningReview(t *testing.T) {
	task := &clickup.Task{ID: "9", Name: "Task 9", URL: "https://app.clickup.com/t/9", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{} // не должен быть вызван вовсе

	cfg := testConfig(t)
	cfg.RepoPath = t.TempDir() // пустой репозиторий, без .claude/commands
	deps, _, calls := newTestDeps(t, cu, runner, cfg)
	q := New(deps, 10)
	q.processTask("9", RunOptions{})

	cu.mu.Lock()
	commentsPosted := len(cu.comments)
	tagsRemoved := len(cu.removedTags)
	cu.mu.Unlock()

	if commentsPosted != 0 {
		t.Errorf("expected no comment when required commands are missing, got %d", commentsPosted)
	}
	if tagsRemoved != 0 {
		t.Errorf("expected trigger tag to stay when the run never reached a real verdict, got %d removals", tagsRemoved)
	}
	// started + служебная ошибка.
	if len(*calls) != 2 {
		t.Fatalf("expected 2 slack notifications (started + service error), got %d", len(*calls))
	}

	// failed-прогон не должен блокировать повторную постановку.
	if _, enqueued, err := deps.Store.TryEnqueue(context.Background(), "9"); err != nil || !enqueued {
		t.Fatalf("expected task to be re-enqueueable after missing-commands failure: enqueued=%v err=%v", enqueued, err)
	}
}

func TestProcessTask_UsesCustomIDForSpecFileWhenPresent(t *testing.T) {
	task := &clickup.Task{
		ID: "869d9kt6a", CustomID: "PNL-4528", Name: "Task 10", URL: "https://app.clickup.com/t/869d9kt6a",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content", // проверяем, что имя файла берётся из CustomID задачи
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("10", RunOptions{})

	want := filepath.Join("specs", "PNL-4528.md")
	if runner.gotSpecPath != want {
		t.Errorf("specPath passed to RunReview = %q, want %q", runner.gotSpecPath, want)
	}

	run, err := deps.Store.GetRun(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.CustomID != "PNL-4528" {
		t.Errorf("expected the run to record CustomID PNL-4528, got: %+v", run)
	}
}

func TestResumeTask_BypassesEligibility(t *testing.T) {
	// Статус "checking", а не STATUS_TRIGGER ("to check") — обычный
	// processTask пропустил бы такую задачу. resumeTask обязана довести её
	// до конца: ревью уже было начато, а прогон прервался крахом/сигналом.
	task := &clickup.Task{
		ID: "11", Name: "Task 11", URL: "https://app.clickup.com/t/11",
		ListID: "list1", Tags: []string{"ai"}, Status: "checking",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.resumeTask("11")

	if len(*calls) != 2 {
		t.Fatalf("expected 2 slack notifications (started + finished) for a resumed task, got %d", len(*calls))
	}

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.statuses) != 2 || cu.statuses[1] != "done" {
		t.Errorf("expected resumed task to reach STATUS_PASS despite not matching trigger condition, got: %+v", cu.statuses)
	}
}

func TestResumeTask_AlreadyRunning_SkipsWithoutReprocessing(t *testing.T) {
	// SubmitResume/resumeTask вызывается только для задач, найденных
	// Store.RecoverFromRestart (застряли в running) — здесь моделируется
	// именно занятое активное состояние, а не устаревшее предположение
	// «done блокирует постановку навсегда» (см. TryEnqueue).
	task := &clickup.Task{ID: "12", Name: "Task 12", URL: "https://app.clickup.com/t/12", ListID: "list1"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	runID := mustEnqueue(t, deps, "12")
	if err := deps.Store.MarkRunning(context.Background(), runID); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}

	q := New(deps, 10)
	q.resumeTask("12")

	if len(*calls) != 0 {
		t.Fatalf("expected no reprocessing for a task that is already actively running, got %d slack calls", len(*calls))
	}
}

func TestResumeTask_ReusesCompletedSpecStage_DoesNotCallRunSpecAgain(t *testing.T) {
	task := &clickup.Task{
		ID: "20", Name: "Task 20", URL: "https://app.clickup.com/t/20",
		ListID: "list1", Tags: []string{"ai"}, Status: "checking",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content", // не важно: RunSpec не должна вызываться повторно
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	ctx := context.Background()
	runID := mustEnqueue(t, deps, "20")
	if err := deps.Store.MarkRunning(ctx, runID); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	mustFinishStage(t, deps, runID, "setup", setupData{})
	mustFinishStage(t, deps, runID, "spec", specStageData{
		SessionID: "cached-spec-session",
		SpecID:    "cached",
		Content:   "cached spec content",
		Usage:     review.TokenUsage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.01},
	})
	// Прогон "прервался" сразу после /spec — сервис перезапустился.
	if _, err := deps.Store.RecoverFromRestart(ctx); err != nil {
		t.Fatalf("RecoverFromRestart error: %v", err)
	}

	q := New(deps, 10)
	q.resumeTask("20")

	if runner.specCalled {
		t.Error("expected /spec not to run again for a stage already marked done")
	}
	if runner.gotSpecPath != "specs/cached.md" {
		t.Errorf("expected /review to use the cached spec path, got %q", runner.gotSpecPath)
	}

	run, err := deps.Store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.InputTokens != 10 || run.OutputTokens != 5 {
		t.Errorf("expected cached /spec usage to be included in the total, got: %+v", run)
	}
}

func TestResumeTask_ReusesCompletedReviewStage_DoesNotRepostComment(t *testing.T) {
	task := &clickup.Task{
		ID: "21", Name: "Task 21", URL: "https://app.clickup.com/t/21",
		ListID: "list1", Tags: []string{"ai"}, Status: "checking",
	}
	cu := &fakeClickUp{task: task}
	// Если бы /review реально вызвалась заново, вердикт был бы другим —
	// тест это заметит через несовпадение целевого статуса.
	runner := &fakeRunner{
		reviewErr: fmt.Errorf("must not be called: stage already done"),
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	ctx := context.Background()
	runID := mustEnqueue(t, deps, "21")
	if err := deps.Store.MarkRunning(ctx, runID); err != nil {
		t.Fatalf("MarkRunning error: %v", err)
	}
	mustFinishStage(t, deps, runID, "setup", setupData{})
	mustFinishStage(t, deps, runID, "commands_check", struct{}{})
	mustFinishStage(t, deps, runID, "git_fetch", struct{}{})
	mustFinishStage(t, deps, runID, "spec", specStageData{SessionID: "spec-sess"})
	mustFinishStage(t, deps, runID, "review", reviewStageData{
		SessionID: "cached-review-session",
		Output:    "Всё отлично.\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
	})
	mustFinishStage(t, deps, runID, "comment", struct{}{})
	// Прогон "прервался" после публикации комментария, до перевода в колонку.
	if _, err := deps.Store.RecoverFromRestart(ctx); err != nil {
		t.Fatalf("RecoverFromRestart error: %v", err)
	}

	q := New(deps, 10)
	q.resumeTask("21")

	if runner.reviewCalled {
		t.Error("expected /review not to run again for a stage already marked done")
	}
	cu.mu.Lock()
	if len(cu.comments) != 0 {
		t.Errorf("expected the comment stage to be skipped (no duplicate comment), got: %+v", cu.comments)
	}
	if len(cu.statuses) != 1 || cu.statuses[0] != "done" {
		t.Errorf("expected the task to still reach the decide stage and move to STATUS_PASS, got: %+v", cu.statuses)
	}
	cu.mu.Unlock()

	run, err := deps.Store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != store.StatusDone || run.Verdict != "pass" {
		t.Errorf("expected the run to finish done/pass using the cached review, got: %+v", run)
	}
	if len(*calls) == 0 {
		t.Error("expected a slack notification for the finished review")
	}
}

func TestProcessTask_ManualPause_StopsBeforeReviewAndCanBeResumed(t *testing.T) {
	task := &clickup.Task{
		ID: "30", Name: "Task 30", URL: "https://app.clickup.com/t/30",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	// Имитирует оператора, нажавшего "поставить на паузу" через веб-интерфейс
	// прямо во время выполнения /spec — RequestPause должен остановить
	// прогон на границе перед /review, не вызывая его вовсе.
	runner.afterSpec = func() { q.RequestPause("30") }

	q.processTask("30", RunOptions{})

	if runner.reviewCalled {
		t.Fatal("expected RunReview not to be called once a pause was requested before it")
	}
	run, err := deps.Store.GetRun(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != store.StatusPaused {
		t.Fatalf("expected the run to be paused, got status %q", run.Status)
	}

	// "Продолжить": SubmitResume должен переоткрыть тот же прогон и
	// довести его до конца — на этот раз включая /review.
	q.resumeTask("30")

	if !runner.reviewCalled {
		t.Fatal("expected RunReview to run after an explicit resume")
	}
	run, err = deps.Store.GetRun(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != store.StatusDone || run.Verdict != "pass" {
		t.Errorf("expected the resumed run to finish done/pass, got: %+v", run)
	}
}

func TestProcessTask_ManualCancel_StopsRunAndMarksFailed(t *testing.T) {
	task := &clickup.Task{
		ID: "31", Name: "Task 31", URL: "https://app.clickup.com/t/31",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{specContent: "spec content"}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	// Имитирует оператора, нажавшего "прервать" во время /spec — в отличие
	// от паузы, это должно оборвать контекст прогона немедленно.
	runner.afterSpec = func() {
		if !q.RequestCancel("31") {
			t.Error("expected RequestCancel to find the active task")
		}
	}

	q.processTask("31", RunOptions{})

	run, err := deps.Store.GetRun(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetRun error: %v", err)
	}
	if run.Status != store.StatusFailed {
		t.Fatalf("expected the run to be marked failed after cancellation, got status %q", run.Status)
	}
	if len(q.ActiveRuns()) != 0 {
		t.Error("expected no active runs left registered after the run finished")
	}
}

func TestProcessTask_PerRunModelEffortOverride_PassedToRunnerCalls(t *testing.T) {
	task := &clickup.Task{
		ID: "32", Name: "Task 32", URL: "https://app.clickup.com/t/32",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}
	runner.SetModelEffort("sonnet", "high") // дефолт очереди — не должен использоваться для этого прогона

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("32", RunOptions{Model: "haiku", Effort: "low"})

	if runner.gotSpecOpts.Model != "haiku" || runner.gotSpecOpts.Effort != "low" {
		t.Errorf("expected RunSpec to receive the per-run override, got: %+v", runner.gotSpecOpts)
	}
	if runner.gotReviewOpts.Model != "haiku" || runner.gotReviewOpts.Effort != "low" {
		t.Errorf("expected RunReview to receive the per-run override, got: %+v", runner.gotReviewOpts)
	}
}

func mustFinishStage(t *testing.T, deps Deps, runID int64, stage string, data any) {
	t.Helper()
	if err := deps.Store.StartStage(context.Background(), runID, stage); err != nil {
		t.Fatalf("StartStage(%s) error: %v", stage, err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal stage data for %s: %v", stage, err)
	}
	if err := deps.Store.FinishStage(context.Background(), runID, stage, raw); err != nil {
		t.Fatalf("FinishStage(%s) error: %v", stage, err)
	}
}

func mustEnqueue(t *testing.T, deps Deps, taskID string) int64 {
	t.Helper()
	id, _, err := deps.Store.TryEnqueue(context.Background(), taskID)
	if err != nil {
		t.Fatalf("TryEnqueue error: %v", err)
	}
	return id
}

func TestQueue_SubmitResume_BypassesEligibilityThroughRealDispatch(t *testing.T) {
	task := &clickup.Task{
		ID: "13", Name: "Task 13", URL: "https://app.clickup.com/t/13",
		ListID: "list1", Tags: []string{"other"}, Status: "checking", // не проходит isEligible
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.Start(1)
	q.SubmitResume("13")

	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(*calls) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for resumed task to be processed via real dispatch, got %d slack calls", len(*calls))
		}
		time.Sleep(10 * time.Millisecond)
	}

	q.Shutdown(time.Second)
}

func TestStripVerdictLine(t *testing.T) {
	output := "## PR 1 — задача\n\n### Замечания\n\n1. [Критично] foo.php:1 — bug.\n\nИТОГ: критичных=1 важных=0 минор=0 статус=fail"
	raw := "ИТОГ: критичных=1 важных=0 минор=0 статус=fail"

	got := stripVerdictLine(output, raw)
	if strings.Contains(got, "ИТОГ:") {
		t.Errorf("expected ИТОГ line to be stripped, got: %q", got)
	}
	if !strings.Contains(got, "1. [Критично] foo.php:1 — bug.") {
		t.Errorf("expected findings to remain, got: %q", got)
	}
}

func TestStripVerdictLine_EmptyRawIsNoop(t *testing.T) {
	output := "some text without a verdict line"
	if got := stripVerdictLine(output, ""); got != output {
		t.Errorf("expected output unchanged when raw is empty, got: %q", got)
	}
}

func TestProcessTask_CommentExcludesSessionLineAndVerdictLine(t *testing.T) {
	task := &clickup.Task{ID: "14", Name: "Task 14", URL: "https://app.clickup.com/t/14", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	raw := "ИТОГ: критичных=0 важных=0 минор=0 статус=pass"
	runner := &fakeRunner{
		specContent: "spec content",
		result: review.Result{
			Output:    "### Замечания\n\nЗамечаний нет.\n\n" + raw,
			SessionID: "sess-xyz",
			Verdict:   review.Verdict{Status: review.StatusPass, Raw: raw},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("14", RunOptions{})

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(cu.comments))
	}
	comment := cu.comments[0]
	if strings.Contains(comment, "Сессия") || strings.Contains(comment, "sess-xyz") {
		t.Errorf("expected no session line in the comment, got: %q", comment)
	}
	if strings.Contains(comment, "ИТОГ:") {
		t.Errorf("expected no ИТОГ line in the comment, got: %q", comment)
	}
	if !strings.Contains(comment, "Замечаний нет.") {
		t.Errorf("expected findings text to remain, got: %q", comment)
	}
}
