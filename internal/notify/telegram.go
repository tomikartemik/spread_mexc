package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sender описывает общий интерфейс уведомлений.
type Sender interface {
	Notify(ctx context.Context, message string) error
}

// TelegramSender отправляет сообщения в Telegram ботом.
type TelegramSender struct {
	botToken   string
	chatID     string
	httpClient *http.Client
}

// NewTelegram создаёт TelegramSender с дефолтным HTTP-клиентом.
func NewTelegram(botToken, chatID string) *TelegramSender {
	return &TelegramSender{
		botToken: botToken,
		chatID:   chatID,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Notify отправляет текстовое сообщение в указанный чат.
func (t *TelegramSender) Notify(ctx context.Context, message string) error {
	if t == nil {
		return nil
	}
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	data := url.Values{}
	data.Set("chat_id", t.chatID)
	data.Set("text", message)
	data.Set("disable_web_page_preview", "true")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram send failed: %s", resp.Status)
	}
	return nil
}
