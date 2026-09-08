// Package queue — буферизованная очередь и воркеры, которые прогоняют
// задачу через полный цикл: проверка условия, git fetch, /spec, /review,
// переходы статуса, назначение исполнителя, комментарий, уведомление в Slack.
package queue

import (
	"strconv"
	"strings"

	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/review"
)

// Decide вычисляет целевую колонку и список добавляемых исполнителей по
// вердикту ревью (см. Требования 3 и 4). Критичные замечания и blocked
// (ревью не смогло выполниться) — оба случая уходят в STATUS_FAIL: п.3
// Требования 3 явно объединяет их в одном переходе.
func Decide(v review.Verdict, cfg *config.Config, creatorID int) (targetStatus string, assigneeIDs []int) {
	if v.Status == review.StatusPass {
		if id, ok := parseUserID(cfg.AssigneeOnPass); ok {
			assigneeIDs = []int{id}
		}
		return cfg.StatusPass, assigneeIDs
	}

	if id, ok := parseUserID(cfg.AssigneeOnFail); ok {
		assigneeIDs = []int{id}
	} else if creatorID != 0 {
		assigneeIDs = []int{creatorID}
	}
	return cfg.StatusFail, assigneeIDs
}

func parseUserID(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return id, true
}

// maxCommentLen — консервативный лимит длины одного комментария ClickUp.
const maxCommentLen = 8000

// SplitComment режет длинный текст ревью на несколько комментариев по
// границам разделов (пустая строка между абзацами markdown), сохраняя
// порядок. Абзац, который сам по себе длиннее лимита, режется жёстко.
func SplitComment(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = maxCommentLen
	}
	if len(text) <= maxLen {
		return []string{text}
	}

	paragraphs := strings.Split(text, "\n\n")
	var chunks []string
	var current strings.Builder

	appendParagraph := func(p string) {
		for len(p) > maxLen {
			chunks = append(chunks, p[:maxLen])
			p = p[maxLen:]
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(p)
	}

	for _, p := range paragraphs {
		sep := 0
		if current.Len() > 0 {
			sep = 2 // разделитель "\n\n"
		}
		if current.Len()+sep+len(p) > maxLen && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		appendParagraph(p)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}
