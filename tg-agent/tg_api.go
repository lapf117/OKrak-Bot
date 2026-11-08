package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.telegram.org/bot"

var (
	httpClient      = &http.Client{Timeout: 120 * time.Second}
	uploadsDir      string
	telegramContext = context.Background()
)

type tgResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// tgError is a definitive Telegram rejection (not a transport failure).
type tgError struct {
	Code        int
	Description string
}

func (e *tgError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

var errNotModified = errors.New("message is not modified")

// Telegram calls are retried silently too: 4 attempts, waiting 1s, 2s, 4s
// (or the retry_after Telegram asks for). Tests shorten tgRetryBase.
var tgRetryBase = time.Second

const tgAttempts = 4

func isParseError(err error) bool {
	var te *tgError
	return errors.As(err, &te) && te.Code == 400 && strings.Contains(strings.ToLower(te.Description), "parse")
}

// tgDo calls a Bot API method with bounded, silent retries for transport
// errors and 429/5xx responses. Definitive 4xx errors are returned immediately.
func tgDo(ctx context.Context, method string, payload interface{}) (json.RawMessage, error) {
	c := configSnapshot()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	endpoint := apiBase + c.Token + "/" + method
	var lastErr error
	for attempt := 0; attempt < tgAttempts; attempt++ {
		wait := tgRetryBase << attempt
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, errors.New(redactSecrets(err.Error()))
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = errors.New(redactSecrets(err.Error()))
			if attempt == tgAttempts-1 {
				break
			}
			if waitContext(ctx, wait) != nil {
				return nil, ctx.Err()
			}
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		var r tgResp
		if json.Unmarshal(raw, &r) == nil && r.OK {
			return r.Result, nil
		}
		description := r.Description
		if description == "" {
			description = short(string(raw), 200)
		}
		description = redactSecrets(description)
		if strings.Contains(description, "message is not modified") {
			return nil, errNotModified
		}
		lastErr = &tgError{Code: resp.StatusCode, Description: description}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			logf("TG 鉴权失败: %s", description)
			return nil, lastErr
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			return nil, lastErr
		}
		if attempt == tgAttempts-1 {
			break
		}
		if r.Parameters.RetryAfter > 0 {
			wait = time.Duration(min(r.Parameters.RetryAfter, 30)) * time.Second
		}
		if waitContext(ctx, wait) != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// tgCallContext is the legacy helper: it logs failures and returns nil.
func tgCallContext(ctx context.Context, method string, payload interface{}) json.RawMessage {
	raw, err := tgDo(ctx, method, payload)
	if err != nil {
		if err != errNotModified && ctx.Err() == nil {
			logf("TG %s 失败: %s", method, short(err.Error(), 200))
		}
		return nil
	}
	return raw
}

func tgCall(method string, payload interface{}) json.RawMessage {
	return tgCallContext(telegramContext, method, payload)
}

func messageID(raw json.RawMessage) int64 {
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.MessageID
}

// sendMessage sends plain text, splitting long content across messages.
func sendMessage(chatID int64, text string, markup interface{}) int64 {
	return sendMessageContext(telegramContext, chatID, text, markup)
}

func sendMessageContext(ctx context.Context, chatID int64, text string, markup interface{}) int64 {
	text = redactSecrets(text)
	if strings.TrimSpace(text) == "" {
		text = "…"
	}
	chunks := splitTelegramText(text, 4000)
	var firstID int64
	for i, chunk := range chunks {
		if ctx.Err() != nil {
			break
		}
		p := map[string]interface{}{"chat_id": chatID, "text": chunk, "disable_web_page_preview": true}
		if markup != nil && i == len(chunks)-1 {
			p["reply_markup"] = markup
		}
		id := messageID(tgCallContext(ctx, "sendMessage", p))
		if i == 0 {
			firstID = id
		}
	}
	return firstID
}

// sendHTML sends one pre-sized HTML message, retrying as plain text when
// Telegram cannot parse the markup.
func sendHTML(ctx context.Context, chatID int64, htmlText, plain string, markup interface{}) int64 {
	htmlText, plain = redactSecrets(htmlText), redactSecrets(plain)
	p := map[string]interface{}{"chat_id": chatID, "disable_web_page_preview": true}
	if markup != nil {
		p["reply_markup"] = markup
	}
	if htmlText != "" && utf16Len(htmlText) <= 4096 {
		p["text"], p["parse_mode"] = htmlText, "HTML"
		raw, err := tgDo(ctx, "sendMessage", p)
		if err == nil {
			return messageID(raw)
		}
		if !isParseError(err) {
			if ctx.Err() == nil {
				logf("TG sendMessage 失败: %s", short(err.Error(), 200))
			}
			return 0
		}
		delete(p, "parse_mode")
	}
	if plain == "" {
		plain = stripTags(htmlText)
	}
	p["text"] = truncateTelegramText(plain, 4000)
	return messageID(tgCallContext(ctx, "sendMessage", p))
}

// sendRich renders Markdown and sends it as one or more formatted messages.
// The keyboard is attached to the last message. Returns the first message id.
func sendRich(ctx context.Context, chatID int64, markdown string, markup interface{}) int64 {
	chunks := markdownChunks(markdown, telegramChunk)
	var first int64
	for i, chunk := range chunks {
		if ctx.Err() != nil {
			break
		}
		var m interface{}
		if i == len(chunks)-1 {
			m = markup
		}
		id := sendHTML(ctx, chatID, chunk.HTML, chunk.Plain, m)
		if i == 0 {
			first = id
		}
	}
	return first
}

// sendAnswer sends a Markdown answer with an optional HTML statistics line
// appended to the last message (or sent on its own when it does not fit).
// It returns the id of the first message, 0 when nothing was delivered.
func sendAnswer(ctx context.Context, chatID int64, markdown, footerHTML string) int64 {
	chunks := markdownChunks(markdown, telegramChunk)
	if footerHTML != "" {
		last := &chunks[len(chunks)-1]
		footerPlain := stripTags(footerHTML)
		switch {
		case last.HTML != "" && utf16Len(last.HTML+"\n\n"+footerHTML) <= telegramChunk:
			last.HTML += "\n\n" + footerHTML
			last.Plain += "\n\n" + footerPlain
		case last.HTML == "" && utf16Len(last.Plain+"\n\n"+footerPlain) <= telegramChunk:
			last.Plain += "\n\n" + footerPlain
		default:
			chunks = append(chunks, richChunk{HTML: footerHTML, Plain: footerPlain})
		}
	}
	var first int64
	for _, chunk := range chunks {
		if ctx.Err() != nil {
			break
		}
		id := sendHTML(ctx, chatID, chunk.HTML, chunk.Plain, nil)
		if first == 0 {
			first = id
		}
	}
	return first
}

// deleteMessage removes one of the bot's own messages; failures are only logged.
func deleteMessage(ctx context.Context, chatID, msgID int64) {
	if msgID == 0 {
		return
	}
	tgCallContext(ctx, "deleteMessage", map[string]interface{}{"chat_id": chatID, "message_id": msgID})
}

// editHTML edits a message with HTML markup, falling back to plain text.
// "Not modified" counts as success.
func editHTML(ctx context.Context, chatID, msgID int64, htmlText, plain string, markup interface{}) bool {
	if msgID == 0 {
		return false
	}
	htmlText, plain = redactSecrets(htmlText), redactSecrets(plain)
	p := map[string]interface{}{"chat_id": chatID, "message_id": msgID, "disable_web_page_preview": true}
	if markup != nil {
		p["reply_markup"] = markup
	}
	if htmlText != "" && utf16Len(htmlText) <= 4096 {
		p["text"], p["parse_mode"] = htmlText, "HTML"
		_, err := tgDo(ctx, "editMessageText", p)
		if err == nil || err == errNotModified {
			return true
		}
		if !isParseError(err) {
			return false
		}
		delete(p, "parse_mode")
	}
	if plain == "" {
		plain = stripTags(htmlText)
	}
	p["text"] = truncateTelegramText(plain, 4000)
	_, err := tgDo(ctx, "editMessageText", p)
	return err == nil || err == errNotModified
}

func editMessage(chatID, msgID int64, text string, markup interface{}) bool {
	return editMessageContext(telegramContext, chatID, msgID, text, markup)
}

func editMessageContext(ctx context.Context, chatID, msgID int64, text string, markup interface{}) bool {
	p := map[string]interface{}{
		"chat_id":                  chatID,
		"message_id":               msgID,
		"text":                     truncateTelegramText(redactSecrets(text), 4000),
		"disable_web_page_preview": true,
	}
	if markup != nil {
		p["reply_markup"] = markup
	}
	_, err := tgDo(ctx, "editMessageText", p)
	return err == nil || err == errNotModified
}

// UI helpers: HTML panels for menus and settings.
func sendUI(chatID int64, htmlText string, markup interface{}) int64 {
	return sendHTML(telegramContext, chatID, htmlText, "", markup)
}

func editUI(chatID, msgID int64, htmlText string, markup interface{}) bool {
	return editHTML(telegramContext, chatID, msgID, htmlText, "", markup)
}

func answerCallback(id string) { answerCallbackText(id, "") }

func answerCallbackText(id, text string) {
	ctx, cancel := context.WithTimeout(telegramContext, 5*time.Second)
	defer cancel()
	p := map[string]interface{}{"callback_query_id": id}
	if text != "" {
		p["text"] = short(text, 180)
	}
	tgCallContext(ctx, "answerCallbackQuery", p)
}

func sendTyping(ctx context.Context, chatID int64) {
	tgCallContext(ctx, "sendChatAction", map[string]interface{}{"chat_id": chatID, "action": "typing"})
}

func waitContext(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
