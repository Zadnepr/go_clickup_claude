// Package slack отправляет уведомления в Slack через Incoming Webhook.
// Недоступность Slack не должна ронять обработку задачи — все ошибки этого
// пакета предназначены для логирования вызывающей стороной, а не для паники.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Notifier отправляет сообщения в Slack Incoming Webhook.
type Notifier struct {
	webhookURL string
	httpClient *http.Client
}

// NewNotifier создаёт Notifier для заданного webhook URL.
func NewNotifier(webhookURL string) *Notifier {
	return &Notifier{
		webhookURL: webhookURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Block — минимальный набор Block Kit блоков, нужный сервису.
type Block map[string]any

// message — тело запроса к Incoming Webhook.
type message struct {
	Blocks []Block `json:"blocks"`
	Text   string  `json:"text"` // fallback для уведомлений/превью
}

// Send отправляет набор блоков в канал. text используется как fallback-текст
// для пуш-уведомлений и превью ссылок.
func (n *Notifier) Send(ctx context.Context, text string, blocks []Block) error {
	body, err := json.Marshal(message{Blocks: blocks, Text: text})
	if err != nil {
		return fmt.Errorf("marshal slack message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send slack notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// Header — блок заголовка.
func Header(text string) Block {
	return Block{
		"type": "header",
		"text": Block{"type": "plain_text", "text": text, "emoji": true},
	}
}

// Section — блок с Markdown-текстом (mrkdwn).
func Section(markdown string) Block {
	return Block{
		"type": "section",
		"text": Block{"type": "mrkdwn", "text": markdown},
	}
}

// Divider — визуальный разделитель.
func Divider() Block {
	return Block{"type": "divider"}
}
