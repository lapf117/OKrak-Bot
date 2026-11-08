package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

// updateDispatcher runs handlers in bounded lanes. Ordinary work can wait for
// approvals for minutes; its admission never blocks polling, and a separate
// lane keeps approvals and stop controls usable.
type updateDispatcher struct {
	ordinary   chan struct{}
	control    chan struct{}
	notices    chan struct{}
	handlers   sync.WaitGroup
	onMessage  func(Message)
	onCallback func(CallbackQuery)
}

func newUpdateDispatcher(ordinaryLimit, controlLimit int, onMessage func(Message), onCallback func(CallbackQuery)) *updateDispatcher {
	return &updateDispatcher{
		ordinary:   make(chan struct{}, max(ordinaryLimit, 1)),
		control:    make(chan struct{}, max(controlLimit, 1)),
		notices:    make(chan struct{}, 4),
		onMessage:  onMessage,
		onCallback: onCallback,
	}
}

func (d *updateDispatcher) launch(lane chan struct{}, work func()) bool {
	select {
	case lane <- struct{}{}:
		d.handlers.Add(1)
		go func() { defer d.handlers.Done(); defer func() { <-lane }(); work() }()
		return true
	default:
		return false
	}
}

func privateControlAllowed(chat Chat, uid int64) bool {
	return chat.ID > 0 && chat.ID == uid && (chat.Type == "" || chat.Type == "private") && allowed(uid)
}

func stopMessage(m Message) bool {
	text := m.Text
	if strings.TrimSpace(text) == "" {
		text = m.Caption
	}
	fields := strings.Fields(normalizeBotCommand(strings.TrimSpace(text)))
	return len(fields) > 0 && strings.EqualFold(fields[0], "/stop")
}

func (d *updateDispatcher) dispatch(update Update) bool {
	if update.CallbackQuery != nil {
		callback := *update.CallbackQuery
		lane := d.ordinary
		if callback.Message != nil && privateControlAllowed(callback.Message.Chat, callback.From.ID) {
			if callback.Data == "act:stop" {
				// Cancellation is local and must work even if all network handlers are busy.
				stopTask(callback.Message.Chat.ID)
				lane = d.control
			} else if isApprovalCallback(callback.Data) {
				lane = d.control
			}
		}
		return d.launch(lane, func() { d.onCallback(callback) })
	}
	if update.Message != nil {
		message := *update.Message
		lane := d.ordinary
		if stopMessage(message) && privateControlAllowed(message.Chat, message.From.ID) {
			stopTask(message.Chat.ID)
			lane = d.control
		}
		return d.launch(lane, func() { d.onMessage(message) })
	}
	return true
}

// reject tells the user about overload. Notifications are bounded too, so a
// slow Telegram connection cannot stall polling.
func (d *updateDispatcher) reject(update Update) {
	d.launch(d.notices, func() {
		ctx, cancel := context.WithTimeout(telegramContext, 3*time.Second)
		defer cancel()
		if update.CallbackQuery != nil {
			lang := langFor(update.CallbackQuery.From.ID)
			tgCallContext(ctx, "answerCallbackQuery", map[string]interface{}{"callback_query_id": update.CallbackQuery.ID, "text": tr(lang, "busy.callback")})
		} else if m := update.Message; m != nil && privateControlAllowed(m.Chat, m.From.ID) {
			sendMessageContext(ctx, m.Chat.ID, tr(langFor(m.From.ID), "busy.message"), nil)
		}
	})
}

func (d *updateDispatcher) wait() { d.handlers.Wait() }

func normalizeBotCommand(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	end := strings.IndexAny(text, " \t\r\n")
	if end < 0 {
		end = len(text)
	}
	command := text[:end]
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}
	return command + text[end:]
}
