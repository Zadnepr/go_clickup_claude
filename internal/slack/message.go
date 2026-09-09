package slack

import (
	"fmt"
	"time"
)

// ReviewNotification — данные одного законченного прогона ревью, нужные
// для формирования Slack-сообщения. Собирается вызывающей стороной из
// вердикта, карточки задачи и переходов статуса/исполнителя — сам пакет
// slack смысл ревью не разбирает.
type ReviewNotification struct {
	TaskName   string
	TaskURL    string
	Verdict    string // pass | fail | blocked
	Critical   int
	Important  int
	Minor      int
	FromStatus string
	ToStatus   string
	Assignee   string // человекочитаемое имя/ID назначенного исполнителя, "" если не менялся
	SessionID  string
}

// BuildStartedMessage формирует сообщение о начале проверки задачи —
// отправляется сразу после того, как задача взята в работу (переведена в
// STATUS_RUNNING), до запуска /spec и /review.
func BuildStartedMessage(taskName, taskURL string) (text string, blocks []Block) {
	text = fmt.Sprintf("🔎 Проверка начата: %s — %s", taskName, taskURL)

	blocks = []Block{
		Header("🔎 Проверка начата"),
		Section(fmt.Sprintf("*Задача:* <%s|%s>", taskURL, taskName)),
	}
	return text, blocks
}

// BuildReviewMessage формирует Block Kit сообщение о завершённом прогоне.
// Критичный вердикт визуально отличим от успешного (эмодзи + заголовок).
func BuildReviewMessage(n ReviewNotification) (text string, blocks []Block) {
	emoji, verdictLabel := verdictPresentation(n.Verdict)

	text = fmt.Sprintf("%s %s: %s — %s", emoji, n.TaskName, verdictLabel, n.TaskURL)

	lines := fmt.Sprintf(
		"*Задача:* <%s|%s>\n*Вердикт:* %s %s\n*Замечания:* критичных=%d, важных=%d, минор=%d",
		n.TaskURL, n.TaskName, emoji, verdictLabel, n.Critical, n.Important, n.Minor,
	)
	if n.FromStatus != "" || n.ToStatus != "" {
		lines += fmt.Sprintf("\n*Колонка:* %s → %s", n.FromStatus, n.ToStatus)
	}
	if n.Assignee != "" {
		lines += fmt.Sprintf("\n*Исполнитель:* %s", n.Assignee)
	}
	if n.SessionID != "" {
		lines += fmt.Sprintf("\n*Сессия:* `%s`", n.SessionID)
	}

	blocks = []Block{
		Header(fmt.Sprintf("%s Ревью задачи: %s", emoji, verdictLabel)),
		Section(lines),
	}
	return text, blocks
}

// BuildShortResultMessage формирует короткое сообщение об итоге проверки:
// задача и куда она перемещена, без разбивки по уровням замечаний — этого
// достаточно, полный текст ревью уже опубликован комментарием в задаче.
func BuildShortResultMessage(n ReviewNotification) (text string, blocks []Block) {
	emoji, _ := verdictPresentation(n.Verdict)

	text = fmt.Sprintf("%s %s: %s → %s — %s", emoji, n.TaskName, n.FromStatus, n.ToStatus, n.TaskURL)

	line := fmt.Sprintf("%s *<%s|%s>*: `%s` → `%s`", emoji, n.TaskURL, n.TaskName, n.FromStatus, n.ToStatus)
	if n.Assignee != "" {
		line += fmt.Sprintf(" (исполнитель: %s)", n.Assignee)
	}

	blocks = []Block{Section(line)}
	return text, blocks
}

// BuildBlockedMessage формирует отдельное сообщение для случая, когда ревью
// не смогло выполниться (blocked) — молчаливый отказ хуже ложного срабатывания.
func BuildBlockedMessage(n ReviewNotification, reason string) (text string, blocks []Block) {
	text = fmt.Sprintf("⛔ Ревью заблокировано: %s — %s", n.TaskName, n.TaskURL)

	lines := fmt.Sprintf("*Задача:* <%s|%s>\n*Причина:* %s", n.TaskURL, n.TaskName, reason)
	if n.SessionID != "" {
		lines += fmt.Sprintf("\n*Сессия:* `%s`", n.SessionID)
	}

	blocks = []Block{
		Header("⛔ Ревью не выполнено (blocked)"),
		Section(lines),
	}
	return text, blocks
}

// BuildServiceErrorMessage формирует сообщение о внутренней ошибке сервиса
// (например, не удалось поменять статус или назначить исполнителя).
func BuildServiceErrorMessage(taskName, taskURL, errText string) (text string, blocks []Block) {
	text = fmt.Sprintf("🔥 Ошибка сервиса при обработке задачи: %s — %s", taskName, taskURL)

	blocks = []Block{
		Header("🔥 Внутренняя ошибка сервиса ревью"),
		Section(fmt.Sprintf("*Задача:* <%s|%s>\n*Ошибка:* %s", taskURL, taskName, errText)),
	}
	return text, blocks
}

// BuildPausedMessage формирует сообщение о том, что проверка задачи
// поставлена на паузу из-за исчерпанного лимита использования claude — это
// не ошибка и не результат ревью, задача вернётся в очередь автоматически.
func BuildPausedMessage(taskName, taskURL string, pauseFor time.Duration, reason string) (text string, blocks []Block) {
	text = fmt.Sprintf("⏸️ Проверка на паузе (исчерпан лимит claude): %s — %s", taskName, taskURL)

	lines := fmt.Sprintf(
		"*Задача:* <%s|%s>\n*Причина:* %s\n*Повтор:* автоматически через сверку, не раньше чем через %s",
		taskURL, taskName, reason, pauseFor.Round(time.Second),
	)

	blocks = []Block{
		Header("⏸️ Ревью на паузе — исчерпан лимит claude"),
		Section(lines),
	}
	return text, blocks
}

func verdictPresentation(verdict string) (emoji, label string) {
	switch verdict {
	case "pass":
		return "✅", "нет критичных замечаний"
	case "fail":
		return "❌", "есть критичные замечания"
	case "blocked":
		return "⛔", "заблокировано"
	default:
		return "❔", verdict
	}
}
