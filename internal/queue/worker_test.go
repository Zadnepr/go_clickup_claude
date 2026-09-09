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
	specErr         error
	specExists      bool   // существует для любого запрошенного ID
	specExistsForID string // существует только для конкретного ID (приоритет над specExists)
	specUsage       review.TokenUsage
	result          review.Result
	reviewErr       error
	gitErr          error
	gotSpecPath     string // specPath, с которым реально вызвали RunReview
	reviewCalled    bool   // была ли вызвана RunReview (нужно проверять, что её пропустили)
}

func (f *fakeRunner) GitFetch(ctx context.Context) error { return f.gitErr }

func (f *fakeRunner) RunSpec(ctx context.Context, taskURL string) (string, review.TokenUsage, error) {
	return "spec-session", f.specUsage, f.specErr
}

func (f *fakeRunner) RunReview(ctx context.Context, taskURL, specPath string) (review.Result, error) {
	f.reviewCalled = true
	f.gotSpecPath = specPath
	return f.result, f.reviewErr
}

func (f *fakeRunner) SpecFilePath(taskID string) string {
	exists := f.specExists
	if f.specExistsForID != "" {
		exists = taskID == f.specExistsForID
	}
	if exists {
		// Существующий файл: сам тестовый бинарь на диске годится как заглушка.
		return filepath.Join(".", "worker_test.go")
	}
	return filepath.Join(string(filepath.Separator), "nonexistent", "spec-"+taskID+".md")
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
		specExists: true,
		result: review.Result{
			Output:    "Всё отлично.\nИТОГ: критичных=0 важных=0 минор=1 статус=pass",
			SessionID: "sess-1",
			Verdict:   review.Verdict{Status: review.StatusPass, Minor: 1, Raw: "ИТОГ: критичных=0 важных=0 минор=1 статус=pass"},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("1")

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
	q.processTask("2")

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
	q.processTask("3")

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
	q.processTask("4")

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
	q.processTask("5")

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

	q.processTask("6")

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
	q.processTask("4")

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

func TestProcessTask_DuplicateEvent_DoesNotProduceSecondRun(t *testing.T) {
	task := &clickup.Task{ID: "5", Name: "Task 5", URL: "https://app.clickup.com/t/5", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)

	q.processTask("5")
	q.processTask("5") // повторное событие по той же задаче

	if len(*calls) != 2 {
		t.Fatalf("expected exactly 2 slack notifications (started + finished) despite duplicate event, got %d", len(*calls))
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
		specExists: true,
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("6")

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.removedAssignees) != 1 || len(cu.removedAssignees[0]) != 2 {
		t.Fatalf("expected original assignees [7 8] to be removed before the check, got: %+v", cu.removedAssignees)
	}
}

func TestProcessTask_Fail_ReassignsOriginalAssigneesWhenNoOverrideConfigured(t *testing.T) {
	task := &clickup.Task{
		ID: "7", Name: "Task 7", URL: "https://app.clickup.com/t/7",
		ListID: "list1", Tags: []string{"ai"}, Status: "to check",
		Assignees: []int{7, 8}, CreatorID: 99,
	}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specExists: true,
		result: review.Result{
			Output:  "плохо\nИТОГ: критичных=1 важных=0 минор=0 статус=fail",
			Verdict: review.Verdict{Status: review.StatusFail, Critical: 1},
		},
	}

	cfg := testConfig(t)
	cfg.AssigneeOnFail = "" // без явного override — должны вернуться исходные исполнители
	deps, _, _ := newTestDeps(t, cu, runner, cfg)
	q := New(deps, 10)
	q.processTask("7")

	cu.mu.Lock()
	defer cu.mu.Unlock()
	if len(cu.assignees) != 1 || len(cu.assignees[0]) != 2 || cu.assignees[0][0] != 7 || cu.assignees[0][1] != 8 {
		t.Errorf("expected original assignees [7 8] to be reassigned on failure, got: %+v", cu.assignees)
	}
}

func TestProcessTask_RemovesTriggerTagAfterFinishing(t *testing.T) {
	task := &clickup.Task{ID: "8", Name: "Task 8", URL: "https://app.clickup.com/t/8", ListID: "list1", Tags: []string{"ai"}, Status: "to check"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{
		specExists: true,
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("8")

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
	q.processTask("9")

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
		specExistsForID: "PNL-4528", // /spec сохранила файл по человекочитаемому ID, а не нативному
		result: review.Result{
			Output:  "ok\nИТОГ: критичных=0 важных=0 минор=0 статус=pass",
			Verdict: review.Verdict{Status: review.StatusPass},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("10")

	want := filepath.Join("specs", "PNL-4528.md")
	if runner.gotSpecPath != want {
		t.Errorf("specPath passed to RunReview = %q, want %q", runner.gotSpecPath, want)
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
		specExists: true,
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

func TestResumeTask_AlreadyDone_SkipsWithoutReprocessing(t *testing.T) {
	task := &clickup.Task{ID: "12", Name: "Task 12", URL: "https://app.clickup.com/t/12", ListID: "list1"}
	cu := &fakeClickUp{task: task}
	runner := &fakeRunner{}

	deps, _, calls := newTestDeps(t, cu, runner, testConfig(t))
	deps.Store.MarkDone(context.Background(), mustEnqueue(t, deps, "12"), "pass", "s", store.Usage{})

	q := New(deps, 10)
	q.resumeTask("12")

	if len(*calls) != 0 {
		t.Fatalf("expected no reprocessing for a task that already has a done run, got %d slack calls", len(*calls))
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
		specExists: true,
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
		specExists: true,
		result: review.Result{
			Output:    "### Замечания\n\nЗамечаний нет.\n\n" + raw,
			SessionID: "sess-xyz",
			Verdict:   review.Verdict{Status: review.StatusPass, Raw: raw},
		},
	}

	deps, _, _ := newTestDeps(t, cu, runner, testConfig(t))
	q := New(deps, 10)
	q.processTask("14")

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
