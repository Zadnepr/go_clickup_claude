package clickup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("pk_test", "team1", WithBaseURL(srv.URL), WithRateLimit(1000))
	t.Cleanup(c.Close)
	return c, srv
}

func TestGetTask(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/task/123":
			if got := r.Header.Get("Authorization"); got != "pk_test" {
				t.Errorf("Authorization header = %q, want pk_test", got)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id":        "123",
				"custom_id": "PNL-4528",
				"name":      "Fix bug",
				"url":       "https://app.clickup.com/t/123",
				"tags":      []map[string]string{{"name": "ai"}, {"name": "urgent"}},
				"status":    map[string]string{"status": "to check"},
				"list":      map[string]string{"id": "list1"},
				"creator":   map[string]int{"id": 42},
				"assignees": []map[string]int{{"id": 7}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/list/list1":
			json.NewEncoder(w).Encode(map[string]any{
				"statuses": []map[string]string{{"status": "to check"}, {"status": "checking"}, {"status": "done"}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	task, err := c.GetTask(context.Background(), "123")
	if err != nil {
		t.Fatalf("GetTask error: %v", err)
	}
	if task.Name != "Fix bug" || task.Status != "to check" || task.ListID != "list1" || task.CreatorID != 42 {
		t.Errorf("unexpected task: %+v", task)
	}
	if task.CustomID != "PNL-4528" {
		t.Errorf("CustomID = %q, want PNL-4528", task.CustomID)
	}
	if len(task.Tags) != 2 || task.Tags[0] != "ai" {
		t.Errorf("unexpected tags: %+v", task.Tags)
	}
	if len(task.Assignees) != 1 || task.Assignees[0] != 7 {
		t.Errorf("unexpected assignees: %+v", task.Assignees)
	}
	if len(task.AvailableStatuses) != 3 {
		t.Errorf("unexpected available statuses: %+v", task.AvailableStatuses)
	}
}

func TestGetTask_ExtractsDeveloperCustomField(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/task/123":
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "123",
				"name":   "Fix bug",
				"status": map[string]string{"status": "to check"},
				"list":   map[string]string{"id": "list1"},
				"custom_fields": []map[string]any{
					{"name": "Date developing", "type": "date", "value": "1788224400000"},
					{"name": "Developer", "type": "users", "value": []map[string]any{
						{"id": 81838052, "username": "Sergey Ponomarev"},
					}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/list/list1":
			json.NewEncoder(w).Encode(map[string]any{"statuses": []map[string]string{}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	task, err := c.GetTask(context.Background(), "123")
	if err != nil {
		t.Fatalf("GetTask error: %v", err)
	}
	if len(task.DeveloperIDs) != 1 || task.DeveloperIDs[0] != 81838052 {
		t.Errorf("DeveloperIDs = %+v, want [81838052]", task.DeveloperIDs)
	}
}

func TestGetTask_NoDeveloperCustomField(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/task/123":
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "123",
				"name":   "Fix bug",
				"status": map[string]string{"status": "to check"},
				"list":   map[string]string{"id": "list1"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/list/list1":
			json.NewEncoder(w).Encode(map[string]any{"statuses": []map[string]string{}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	task, err := c.GetTask(context.Background(), "123")
	if err != nil {
		t.Fatalf("GetTask error: %v", err)
	}
	if len(task.DeveloperIDs) != 0 {
		t.Errorf("expected no DeveloperIDs when the task has no custom fields, got: %+v", task.DeveloperIDs)
	}
}

func TestSetStatus_Success(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/task/123" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["status"] != "checking" {
			t.Errorf("unexpected body: %+v", body)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"id": "123"})
	})

	if err := c.SetStatus(context.Background(), "123", "checking"); err != nil {
		t.Fatalf("SetStatus error: %v", err)
	}
}

func TestSetStatus_InvalidStatus(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"err": "Status not found", "ECODE": "STATUS_002"})
	})

	err := c.SetStatus(context.Background(), "123", "not-a-real-status")
	if err == nil {
		t.Fatal("expected error for invalid status")
	}
}

func TestAddAssignees_OnlyAdds(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Assignees struct {
				Add []int `json:"add"`
				Rem []int `json:"rem"`
			} `json:"assignees"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Assignees.Add) != 1 || body.Assignees.Add[0] != 99 {
			t.Errorf("unexpected add list: %+v", body.Assignees.Add)
		}
		if len(body.Assignees.Rem) != 0 {
			t.Errorf("expected empty rem list, got: %+v", body.Assignees.Rem)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{})
	})

	if err := c.AddAssignees(context.Background(), "123", []int{99}); err != nil {
		t.Fatalf("AddAssignees error: %v", err)
	}
}

func TestRemoveAssignees_OnlyRemoves(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Assignees struct {
				Add []int `json:"add"`
				Rem []int `json:"rem"`
			} `json:"assignees"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Assignees.Rem) != 2 || body.Assignees.Rem[0] != 7 || body.Assignees.Rem[1] != 8 {
			t.Errorf("unexpected rem list: %+v", body.Assignees.Rem)
		}
		if len(body.Assignees.Add) != 0 {
			t.Errorf("expected empty add list, got: %+v", body.Assignees.Add)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{})
	})

	if err := c.RemoveAssignees(context.Background(), "123", []int{7, 8}); err != nil {
		t.Fatalf("RemoveAssignees error: %v", err)
	}
}

func TestRemoveAssignees_EmptyListIsNoop(t *testing.T) {
	called := false
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	if err := c.RemoveAssignees(context.Background(), "123", nil); err != nil {
		t.Fatalf("RemoveAssignees error: %v", err)
	}
	if called {
		t.Fatal("expected no HTTP request for an empty assignee list")
	}
}

func TestRemoveTag(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := "/task/123/tag/ai"
		if r.Method != http.MethodDelete || r.URL.Path != want {
			t.Fatalf("unexpected request: %s %s, want DELETE %s", r.Method, r.URL.Path, want)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{})
	})

	if err := c.RemoveTag(context.Background(), "123", "ai"); err != nil {
		t.Fatalf("RemoveTag error: %v", err)
	}
}

func TestAddComment_NotifyAllOff(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["notify_all"] != false {
			t.Errorf("expected notify_all=false, got %+v", body["notify_all"])
		}
		if body["comment_text"] != "review text" {
			t.Errorf("unexpected comment_text: %+v", body["comment_text"])
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{})
	})

	if err := c.AddComment(context.Background(), "123", "review text"); err != nil {
		t.Fatalf("AddComment error: %v", err)
	}
}

func TestListTasksByTagAndStatus(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tags[]") != "ai" || r.URL.Query().Get("statuses[]") != "to check" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tasks": []map[string]any{
				{"id": "1", "name": "A", "status": map[string]string{"status": "to check"}, "list": map[string]string{"id": "list1"}},
				{"id": "2", "name": "B", "status": map[string]string{"status": "to check"}, "list": map[string]string{"id": "list1"}},
			},
		})
	})

	tasks, err := c.ListTasksByTagAndStatus(context.Background(), "list1", "ai", "to check")
	if err != nil {
		t.Fatalf("ListTasksByTagAndStatus error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestCreateWebhook(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/team/team1/webhook" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var body struct {
			Endpoint string   `json:"endpoint"`
			Events   []string `json:"events"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Endpoint != "https://example.com/webhook/clickup" {
			t.Errorf("unexpected endpoint: %s", body.Endpoint)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "wh_1",
			"webhook": map[string]any{
				"id":     "wh_1",
				"secret": "generated-secret",
			},
		})
	})

	id, secret, err := c.CreateWebhook(context.Background(), "https://example.com/webhook/clickup", []string{"taskStatusUpdated"})
	if err != nil {
		t.Fatalf("CreateWebhook error: %v", err)
	}
	if id != "wh_1" || secret != "generated-secret" {
		t.Errorf("unexpected result: id=%q secret=%q", id, secret)
	}
}

func TestRateLimit_RetriesOn429(t *testing.T) {
	attempts := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task/123" {
			// побочный запрос GetTask за списком доступных статусов — не считаем
			json.NewEncoder(w).Encode(map[string]any{"statuses": []map[string]string{}})
			return
		}
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"err": "rate limited"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "123", "name": "ok", "status": map[string]string{"status": "to check"}, "list": map[string]string{"id": ""}})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	task, err := c.GetTask(ctx, "123")
	if err != nil {
		t.Fatalf("GetTask error after retries: %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
	if task.Name != "ok" {
		t.Errorf("unexpected task: %+v", task)
	}
}
