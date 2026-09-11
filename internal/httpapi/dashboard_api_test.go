package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/queue"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

type fakeClickUpReader struct {
	task         *clickup.Task
	tasksByID    map[string]*clickup.Task
	taskErr      error
	members      []clickup.Member
	memErr       error
	otherTasks   []clickup.Task
	otherErr     error
	gotStatus    string
	queuedTasks  []clickup.Task
	queuedErr    error
	gotQueuedTag string
}

func (f *fakeClickUpReader) GetTask(ctx context.Context, taskID string) (*clickup.Task, error) {
	if f.tasksByID != nil {
		if t, ok := f.tasksByID[taskID]; ok {
			return t, nil
		}
	}
	return f.task, f.taskErr
}

func (f *fakeClickUpReader) GetTeamMembers(ctx context.Context) ([]clickup.Member, error) {
	return f.members, f.memErr
}

func (f *fakeClickUpReader) ListTasksByStatus(ctx context.Context, listID, status string) ([]clickup.Task, error) {
	f.gotStatus = status
	return f.otherTasks, f.otherErr
}

func (f *fakeClickUpReader) ListTasksByTagAndStatus(ctx context.Context, listID, tag, status string) ([]clickup.Task, error) {
	f.gotQueuedTag = tag
	return f.queuedTasks, f.queuedErr
}

// fakeRunnerControl — фейковая реализация RunnerControl.
type fakeRunnerControl struct {
	model, effort string
	setCalled     bool
}

func (f *fakeRunnerControl) SetModelEffort(model, effort string) {
	f.setCalled = true
	f.model, f.effort = model, effort
}

func (f *fakeRunnerControl) ModelEffort() (string, string) { return f.model, f.effort }

func TestConfigHandler_ReturnsSanitizedSettingsWithResolvedAssignee(t *testing.T) {
	cfg := &config.Config{
		TriggerTag: "ai", StatusTrigger: "to check", StatusRunning: "checking",
		StatusPass: "done", StatusFail: "rework", AssigneeOnFail: "81838052",
		WorkerConcurrency: 1, ReviewTimeout: 15 * time.Minute, ReconcileInterval: 5 * time.Minute,
		UsageLimitPause: 30 * time.Minute, ClaudeModel: "haiku", ClaudeEffort: "low",
		RepoPath: "/repo", CUListID: "list1",
	}
	cu := &fakeClickUpReader{members: []clickup.Member{{ID: 81838052, Username: "Sergey", Avatar: "https://x/y.jpg"}}}
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Cfg: cfg, ClickUp: cu, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["claude_model"] != "haiku" || body["claude_effort"] != "low" {
		t.Errorf("unexpected model/effort in response: %+v", body)
	}
	assignee, ok := body["assignee_on_fail"].(map[string]any)
	if !ok {
		t.Fatalf("expected assignee_on_fail to be an object, got: %+v", body["assignee_on_fail"])
	}
	if assignee["username"] != "Sergey" {
		t.Errorf("expected resolved username Sergey, got: %+v", assignee)
	}
}

func TestConfigHandler_NoConfig_Returns500(t *testing.T) {
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestQueueHandler_ReturnsActiveAndPending(t *testing.T) {
	// pending теперь опрашивается напрямую у ClickUp (тег+статус триггера),
	// а не из внутреннего буфера очереди (см. Queue.Pending) — так дашборд
	// не копит дубли, если сверка несколько раз подряд находит одну и ту же
	// ещё не взятую в обработку задачу, и не зависит от того, успел ли этот
	// процесс сам когда-либо положить task_id в свой буфер.
	sub := &fakeSubmitter{
		active: []queue.ActiveRunInfo{{TaskID: "t1", RunID: 5}},
	}
	st := &fakePinger{
		run:    &store.Run{ID: 5, TaskID: "t1", Status: "running", InputTokens: 10, OutputTokens: 20, CostUSD: 0.01},
		stages: []store.RunStage{{RunID: 5, Stage: "spec", Status: "done"}},
	}
	cu := &fakeClickUpReader{
		task: &clickup.Task{ID: "t1", Name: "Task One", URL: "https://x/t1"},
		queuedTasks: []clickup.Task{
			{ID: "t2", CustomID: "PNL-2", Name: "Task Two", URL: "https://x/t2"},
			// t1 тоже формально ещё числится с тегом+статусом триггера на
			// момент опроса ClickUp (короткое окно до SetStatus в setup) —
			// должна быть исключена из pending, раз уже показана в active.
			{ID: "t1", CustomID: "PNL-1", Name: "Task One", URL: "https://x/t1"},
		},
	}
	cfg := &config.Config{CUListID: "list1", TriggerTag: "ai", StatusTrigger: "to check"}
	deps := Deps{Queue: sub, Store: st, ClickUp: cu, Cfg: cfg, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Active []struct {
			TaskID   string           `json:"task_id"`
			RunID    int64            `json:"run_id"`
			TaskName string           `json:"task_name"`
			Status   string           `json:"status"`
			Stages   []store.RunStage `json:"stages"`
		} `json:"active"`
		Pending []struct {
			TaskID   string `json:"task_id"`
			CustomID string `json:"custom_id"`
			Name     string `json:"name"`
			URL      string `json:"url"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Active) != 1 || body.Active[0].TaskID != "t1" || body.Active[0].TaskName != "Task One" {
		t.Errorf("unexpected active: %+v", body.Active)
	}
	if body.Active[0].Status != "running" || len(body.Active[0].Stages) != 1 {
		t.Errorf("unexpected active run details: %+v", body.Active[0])
	}
	if len(body.Pending) != 1 || body.Pending[0].TaskID != "t2" {
		t.Errorf("expected only t2 in pending (t1 excluded as active), got: %+v", body.Pending)
	}
	if body.Pending[0].CustomID != "PNL-2" || body.Pending[0].Name != "Task Two" {
		t.Errorf("expected pending[0] resolved to custom_id/name, got %+v", body.Pending[0])
	}
	if cu.gotQueuedTag != "ai" {
		t.Errorf("expected ListTasksByTagAndStatus called with tag %q, got %q", "ai", cu.gotQueuedTag)
	}
}

func TestTaskControlHandler_CancelSuccess(t *testing.T) {
	sub := &fakeSubmitter{cancelResult: true}
	deps := Deps{Queue: sub, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/abc123/cancel", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if sub.cancelledID != "abc123" {
		t.Errorf("expected RequestCancel called with abc123, got %q", sub.cancelledID)
	}
}

func TestTaskControlHandler_PauseNotActive_Returns409(t *testing.T) {
	sub := &fakeSubmitter{pauseResult: false}
	deps := Deps{Queue: sub, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/abc123/pause", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestTaskControlHandler_Resume(t *testing.T) {
	sub := &fakeSubmitter{resumeResult: true}
	deps := Deps{Queue: sub, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/abc123/resume", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if sub.resumedID != "abc123" {
		t.Errorf("expected SubmitResume called with abc123, got %q", sub.resumedID)
	}
}

func TestRunDetailHandler_ReturnsRunStagesAndInvocations(t *testing.T) {
	fake := &fakePinger{
		run:         &store.Run{ID: 7, TaskID: "t7", Status: "done", Verdict: "pass"},
		stages:      []store.RunStage{{RunID: 7, Stage: "spec", Status: "done"}},
		invocations: []store.ClaudeInvocation{{ID: 1, RunID: 7, Stage: "spec", InputTokens: 5}},
	}
	deps := Deps{Queue: &fakeSubmitter{}, Store: fake, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/runs/7", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"session_id"`)) {
		t.Errorf("expected invocation fields in response, got: %s", rec.Body.String())
	}
}

func TestRunDetailHandler_InvalidID_Returns400(t *testing.T) {
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/runs/not-a-number", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestInvocationsHandler_ReturnsTotals(t *testing.T) {
	fake := &fakePinger{invocations: []store.ClaudeInvocation{
		{ID: 1, RunID: 1, Stage: "spec", InputTokens: 10, OutputTokens: 20, CostUSD: 0.01},
		{ID: 2, RunID: 1, Stage: "review", InputTokens: 30, OutputTokens: 40, CostUSD: 0.02},
	}}
	deps := Deps{Queue: &fakeSubmitter{}, Store: fake, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/invocations?window=day", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Totals struct {
			InputTokens  int64   `json:"input_tokens"`
			OutputTokens int64   `json:"output_tokens"`
			CostUSD      float64 `json:"cost_usd"`
			Count        int     `json:"count"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Totals.InputTokens != 40 || body.Totals.OutputTokens != 60 || body.Totals.Count != 2 {
		t.Errorf("unexpected totals: %+v", body.Totals)
	}
}

func TestDashboardHandler_ServesHTMLAtRoot(t *testing.T) {
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("<html")) {
		t.Errorf("expected HTML content, got: %s", rec.Body.String()[:min(200, rec.Body.Len())])
	}
}

func TestDashboardHandler_UnknownPath_Returns404(t *testing.T) {
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/some/unknown/path", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSetModelHandler_ChangesLiveRunnerAndPersists(t *testing.T) {
	runner := &fakeRunnerControl{model: "sonnet", effort: "high"}
	st := &fakePinger{}
	deps := Deps{Queue: &fakeSubmitter{}, Store: st, Runner: runner, Logger: discardLogger()}
	mux := NewMux(deps)

	body := bytes.NewReader([]byte(`{"model":"haiku","effort":"low"}`))
	req := httptest.NewRequest(http.MethodPut, "/api/config/model", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !runner.setCalled || runner.model != "haiku" || runner.effort != "low" {
		t.Errorf("expected live runner updated to haiku/low, got: %+v", runner)
	}
	if st.settingCalls["claude_model"] != "haiku" || st.settingCalls["claude_effort"] != "low" {
		t.Errorf("expected settings persisted, got: %+v", st.settingCalls)
	}
}

func TestSetModelHandler_InvalidEffort_Returns400(t *testing.T) {
	runner := &fakeRunnerControl{}
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Runner: runner, Logger: discardLogger()}
	mux := NewMux(deps)

	body := bytes.NewReader([]byte(`{"model":"haiku","effort":"turbo"}`))
	req := httptest.NewRequest(http.MethodPut, "/api/config/model", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if runner.setCalled {
		t.Error("expected the runner not to be updated for an invalid effort")
	}
}

func TestSetModelHandler_MissingModel_Returns400(t *testing.T) {
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, Runner: &fakeRunnerControl{}, Logger: discardLogger()}
	mux := NewMux(deps)

	body := bytes.NewReader([]byte(`{"effort":"low"}`))
	req := httptest.NewRequest(http.MethodPut, "/api/config/model", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestQueueHandler_OtherToCheck_ExcludesTaggedTasks(t *testing.T) {
	cfg := &config.Config{CUListID: "list1", StatusTrigger: "to check", TriggerTag: "ai"}
	cu := &fakeClickUpReader{otherTasks: []clickup.Task{
		{ID: "1", Name: "Tagged", Tags: []string{"AI"}},    // уже с тегом (регистр не важен) — не должен попасть в other_to_check
		{ID: "2", Name: "Untagged", Tags: []string{"bug"}}, // без нужного тега
	}}
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, ClickUp: cu, Cfg: cfg, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OtherToCheck []struct {
			TaskID string `json:"task_id"`
		} `json:"other_to_check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.OtherToCheck) != 1 || body.OtherToCheck[0].TaskID != "2" {
		t.Errorf("expected only the untagged task, got: %+v", body.OtherToCheck)
	}
	if cu.gotStatus != "to check" {
		t.Errorf("expected ListTasksByStatus called with the trigger status, got %q", cu.gotStatus)
	}
}

func TestQueueHandler_OtherToCheck_ResolvesAssigneeAvatars(t *testing.T) {
	cfg := &config.Config{CUListID: "list1", StatusTrigger: "to check", TriggerTag: "ai"}
	cu := &fakeClickUpReader{
		otherTasks: []clickup.Task{
			{ID: "1", Name: "Untagged", Assignees: []int{81838052, 999}},
		},
		members: []clickup.Member{{ID: 81838052, Username: "Sergey", Avatar: "https://x/y.jpg"}},
	}
	deps := Deps{Queue: &fakeSubmitter{}, Store: &fakePinger{}, ClickUp: cu, Cfg: cfg, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OtherToCheck []struct {
			Assignees []struct {
				ID       int    `json:"id"`
				Username string `json:"username"`
				Avatar   string `json:"avatar"`
			} `json:"assignees"`
		} `json:"other_to_check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.OtherToCheck) != 1 || len(body.OtherToCheck[0].Assignees) != 2 {
		t.Fatalf("unexpected response: %+v", body.OtherToCheck)
	}
	resolved := body.OtherToCheck[0].Assignees[0]
	if resolved.ID != 81838052 || resolved.Username != "Sergey" || resolved.Avatar != "https://x/y.jpg" {
		t.Errorf("expected first assignee resolved to Sergey, got: %+v", resolved)
	}
	unresolved := body.OtherToCheck[0].Assignees[1]
	if unresolved.ID != 999 || unresolved.Username != "" {
		t.Errorf("expected second assignee to be unresolved (no member found), got: %+v", unresolved)
	}
}

func TestRunHandler_PassesModelEffortToManualRunner(t *testing.T) {
	trigger := &fakeManualRunner{submitted: []string{"123"}}
	deps := Deps{Queue: &fakeSubmitter{}, Trigger: trigger, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/run", bytes.NewReader([]byte(`{"task_id":"123","model":"haiku","effort":"low"}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", rec.Code, rec.Body.String())
	}
	if trigger.modelArg != "haiku" || trigger.effortArg != "low" {
		t.Errorf("expected model/effort passed through to RunNow, got model=%q effort=%q", trigger.modelArg, trigger.effortArg)
	}
}

func TestRunHandler_InvalidEffort_Returns400(t *testing.T) {
	trigger := &fakeManualRunner{}
	deps := Deps{Queue: &fakeSubmitter{}, Trigger: trigger, Store: &fakePinger{}, Logger: discardLogger()}
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/run", bytes.NewReader([]byte(`{"task_id":"123","effort":"turbo"}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
