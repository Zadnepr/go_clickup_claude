// Package clickup — тонкий клиент ClickUp API v2: карточка задачи, смена
// статуса, назначение исполнителей, комментарии, список задач, вебхуки.
// Никакой содержательной логики ревью здесь нет и быть не должно.
package clickup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.clickup.com/api/v2"

// Client — клиент ClickUp API v2.
type Client struct {
	baseURL    string
	apiToken   string
	teamID     string
	httpClient *http.Client
	limiter    *limiter
	logger     *slog.Logger
}

// Option настраивает Client при создании.
type Option func(*Client)

// WithBaseURL переопределяет базовый URL API (используется в тестах против httptest.Server).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// WithHTTPClient задаёт свой http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithLogger задаёт логгер.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// WithRateLimit задаёт лимит запросов в минуту.
func WithRateLimit(perMinute int) Option {
	return func(c *Client) { c.limiter = newLimiter(perMinute) }
}

// NewClient создаёт клиент ClickUp API.
func NewClient(apiToken, teamID string, opts ...Option) *Client {
	c := &Client{
		baseURL:    defaultBaseURL,
		apiToken:   apiToken,
		teamID:     teamID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		limiter:    newLimiter(100),
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Close останавливает фоновые горутины клиента (лимитер).
func (c *Client) Close() {
	c.limiter.Close()
}

// Task — плоское представление карточки задачи ClickUp, содержащее только
// поля, нужные Go-сервису для принятия решений и сантехники.
type Task struct {
	ID                string
	CustomID          string // человекочитаемый ID вида "PNL-4528", если задан в ClickUp
	Name              string
	URL               string
	Status            string
	AvailableStatuses []string
	Tags              []string
	ListID            string
	CreatorID         int
	Assignees         []int
	// DeveloperIDs — значение custom field "Developer" (тип "users" в
	// ClickUp), если такое поле есть на задаче и заполнено. Приоритетный
	// источник исполнителя при провале ревью (см. queue.Decide) — это
	// реальный разработчик, а не тот, кто был назначен на задачу до снятия
	// исполнителей на время проверки.
	DeveloperIDs []int
}

type apiError struct {
	statusCode int
	err        string
	ecode      string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("clickup api: http %d %s (%s)", e.statusCode, e.err, e.ecode)
}

// doRequest выполняет HTTP-запрос к ClickUp API с учётом лимитера и
// экспоненциальным повтором при 429.
func (c *Client) doRequest(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	const maxAttempts = 5
	backoff := time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}

		var reqBody io.Reader
		if bodyReader != nil {
			b, _ := io.ReadAll(bodyReader)
			bodyReader = bytes.NewReader(b)
			reqBody = bytes.NewReader(b)
		}

		req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", c.apiToken)
		if reqBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("clickup api request: %w", err)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read clickup api response: %w", readErr)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			wait := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					wait = time.Duration(secs) * time.Second
				}
			}
			c.logger.Warn("clickup api rate limited, retrying", "attempt", attempt, "wait", wait.String())
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			backoff *= 2
			continue
		}

		if resp.StatusCode >= 400 {
			var parsed struct {
				Err   string `json:"err"`
				ECode string `json:"ECODE"`
			}
			_ = json.Unmarshal(respBody, &parsed)
			return respBody, &apiError{statusCode: resp.StatusCode, err: parsed.Err, ecode: parsed.ECode}
		}

		return respBody, nil
	}

	return nil, fmt.Errorf("clickup api: exceeded %d retry attempts due to rate limiting", maxAttempts)
}

// rawTask отражает нужные нам поля ответа GET /task/{id}.
type rawTask struct {
	ID       string `json:"id"`
	CustomID string `json:"custom_id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Tags     []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Status struct {
		Status string `json:"status"`
	} `json:"status"`
	List struct {
		ID string `json:"id"`
	} `json:"list"`
	Creator struct {
		ID int `json:"id"`
	} `json:"creator"`
	Assignees []struct {
		ID int `json:"id"`
	} `json:"assignees"`
	CustomFields []rawCustomField `json:"custom_fields"`
}

// rawCustomField отражает один custom field из ответа GET /task/{id}. Value
// не разбирается здесь целиком: форма поля зависит от его типа (дата — число
// строкой, dropdown — индекс, users — массив объектов и т.д.), поэтому
// парсится только там, где по имени/типу поля точно известно, чего ожидать
// (см. developerIDsFromCustomFields).
type rawCustomField struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// developerFieldName — имя custom field в ClickUp, откуда берётся реальный
// исполнитель при провале ревью (см. queue.Decide). Сравнивается без учёта
// регистра — то же соглашение, что и для статусов/тегов (см.
// config.NormalizeStatus).
const developerFieldName = "developer"

// developerIDsFromCustomFields ищет среди custom fields задачи поле
// "Developer" (тип "users") и возвращает id всех выбранных в нём
// пользователей. Поля другого типа с тем же именем или отсутствие поля —
// пустой результат, не ошибка: не на каждой задаче/списке оно обязано быть.
func developerIDsFromCustomFields(fields []rawCustomField) []int {
	for _, f := range fields {
		if strings.ToLower(strings.TrimSpace(f.Name)) != developerFieldName || f.Type != "users" || len(f.Value) == 0 {
			continue
		}
		var users []struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(f.Value, &users); err != nil {
			continue
		}
		ids := make([]int, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		return ids
	}
	return nil
}

// GetTask загружает полную карточку задачи по её ID.
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/task/"+taskID, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", taskID, err)
	}

	var raw rawTask
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse task %s response: %w", taskID, err)
	}

	task := &Task{
		ID:           raw.ID,
		CustomID:     raw.CustomID,
		Name:         raw.Name,
		URL:          raw.URL,
		Status:       raw.Status.Status,
		ListID:       raw.List.ID,
		CreatorID:    raw.Creator.ID,
		DeveloperIDs: developerIDsFromCustomFields(raw.CustomFields),
	}
	for _, t := range raw.Tags {
		task.Tags = append(task.Tags, t.Name)
	}
	for _, a := range raw.Assignees {
		task.Assignees = append(task.Assignees, a.ID)
	}

	if statuses, err := c.ListStatuses(ctx, task.ListID); err == nil {
		task.AvailableStatuses = statuses
	} else {
		c.logger.Warn("failed to load available statuses for list", "list_id", task.ListID, "error", err.Error())
	}

	return task, nil
}

// Member — участник ClickUp-воркспейса (см. GetTeamMembers). Используется
// для отображения имени/аватара по ID пользователя (ASSIGNEE_ON_FAIL/
// ASSIGNEE_ON_PASS, custom field Developer, текущие исполнители) в
// веб-интерфейсе — сам сервис по имени/аватару ничего не решает.
type Member struct {
	ID       int
	Username string
	Email    string
	Color    string
	Avatar   string
}

// GetTeamMembers возвращает участников воркспейса (team, заданного при
// создании клиента). У ClickUp API v2 нет эндпоинта для одной команды по
// id — приходится запрашивать список всех команд, доступных токену,
// и выбирать нужную.
func (c *Client) GetTeamMembers(ctx context.Context) ([]Member, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/team", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}

	var raw struct {
		Teams []struct {
			ID      string `json:"id"`
			Members []struct {
				User struct {
					ID             int    `json:"id"`
					Username       string `json:"username"`
					Email          string `json:"email"`
					Color          string `json:"color"`
					ProfilePicture string `json:"profilePicture"`
				} `json:"user"`
			} `json:"members"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse teams response: %w", err)
	}

	for _, t := range raw.Teams {
		if t.ID != c.teamID {
			continue
		}
		members := make([]Member, 0, len(t.Members))
		for _, m := range t.Members {
			members = append(members, Member{
				ID:       m.User.ID,
				Username: m.User.Username,
				Email:    m.User.Email,
				Color:    m.User.Color,
				Avatar:   m.User.ProfilePicture,
			})
		}
		return members, nil
	}
	return nil, fmt.Errorf("team %s not found in /team response", c.teamID)
}

// ListStatuses возвращает названия статусов, доступных в списке (для диагностики
// ошибок смены статуса — несуществующее имя колонки частая ошибка конфигурации).
func (c *Client) ListStatuses(ctx context.Context, listID string) ([]string, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/list/"+listID, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("get list %s: %w", listID, err)
	}

	var raw struct {
		Statuses []struct {
			Status string `json:"status"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse list %s response: %w", listID, err)
	}

	statuses := make([]string, 0, len(raw.Statuses))
	for _, s := range raw.Statuses {
		statuses = append(statuses, s.Status)
	}
	return statuses, nil
}

// SetStatus переводит задачу в указанную колонку.
func (c *Client) SetStatus(ctx context.Context, taskID, status string) error {
	_, err := c.doRequest(ctx, http.MethodPut, "/task/"+taskID, nil, map[string]any{
		"status": status,
	})
	if err != nil {
		return fmt.Errorf("set status %q on task %s: %w", status, taskID, err)
	}
	return nil
}

// AddAssignees добавляет исполнителей задаче, не трогая существующих.
func (c *Client) AddAssignees(ctx context.Context, taskID string, userIDs []int) error {
	if len(userIDs) == 0 {
		return nil
	}
	_, err := c.doRequest(ctx, http.MethodPut, "/task/"+taskID, nil, map[string]any{
		"assignees": map[string]any{
			"add": userIDs,
			"rem": []int{},
		},
	})
	if err != nil {
		return fmt.Errorf("add assignees %v to task %s: %w", userIDs, taskID, err)
	}
	return nil
}

// RemoveAssignees снимает указанных исполнителей с задачи, не трогая
// остальных.
func (c *Client) RemoveAssignees(ctx context.Context, taskID string, userIDs []int) error {
	if len(userIDs) == 0 {
		return nil
	}
	_, err := c.doRequest(ctx, http.MethodPut, "/task/"+taskID, nil, map[string]any{
		"assignees": map[string]any{
			"add": []int{},
			"rem": userIDs,
		},
	})
	if err != nil {
		return fmt.Errorf("remove assignees %v from task %s: %w", userIDs, taskID, err)
	}
	return nil
}

// RemoveTag снимает тег с задачи.
func (c *Client) RemoveTag(ctx context.Context, taskID, tagName string) error {
	_, err := c.doRequest(ctx, http.MethodDelete, "/task/"+taskID+"/tag/"+url.PathEscape(tagName), nil, nil)
	if err != nil {
		return fmt.Errorf("remove tag %q from task %s: %w", tagName, taskID, err)
	}
	return nil
}

// AddComment публикует комментарий в задаче. notify_all всегда выключен.
func (c *Client) AddComment(ctx context.Context, taskID, text string) error {
	_, err := c.doRequest(ctx, http.MethodPost, "/task/"+taskID+"/comment", nil, map[string]any{
		"comment_text": text,
		"notify_all":   false,
	})
	if err != nil {
		return fmt.Errorf("add comment to task %s: %w", taskID, err)
	}
	return nil
}

// ListTasksByTagAndStatus возвращает задачи списка с заданным тегом и статусом
// (используется сверкой, см. Требование 1.2).
func (c *Client) ListTasksByTagAndStatus(ctx context.Context, listID, tag, status string) ([]Task, error) {
	q := url.Values{}
	q.Add("tags[]", tag)
	q.Add("statuses[]", status)
	q.Add("include_closed", "true")

	body, err := c.doRequest(ctx, http.MethodGet, "/list/"+listID+"/task", q, nil)
	if err != nil {
		return nil, fmt.Errorf("list tasks for list %s: %w", listID, err)
	}

	var raw struct {
		Tasks []rawTask `json:"tasks"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse list tasks response: %w", err)
	}

	tasks := make([]Task, 0, len(raw.Tasks))
	for _, rt := range raw.Tasks {
		t := Task{
			ID:        rt.ID,
			CustomID:  rt.CustomID,
			Name:      rt.Name,
			URL:       rt.URL,
			Status:    rt.Status.Status,
			ListID:    rt.List.ID,
			CreatorID: rt.Creator.ID,
		}
		for _, tag := range rt.Tags {
			t.Tags = append(t.Tags, tag.Name)
		}
		for _, a := range rt.Assignees {
			t.Assignees = append(t.Assignees, a.ID)
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// CreateWebhook регистрирует вебхук в ClickUp и возвращает его ID и секрет
// для проверки подписи входящих запросов.
func (c *Client) CreateWebhook(ctx context.Context, endpointURL string, events []string) (id, secret string, err error) {
	body, err := c.doRequest(ctx, http.MethodPost, "/team/"+c.teamID+"/webhook", nil, map[string]any{
		"endpoint": endpointURL,
		"events":   events,
	})
	if err != nil {
		return "", "", fmt.Errorf("create webhook: %w", err)
	}

	var raw struct {
		ID      string `json:"id"`
		Webhook struct {
			Secret string `json:"secret"`
		} `json:"webhook"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", "", fmt.Errorf("parse create webhook response: %w", err)
	}

	return raw.ID, raw.Webhook.Secret, nil
}
