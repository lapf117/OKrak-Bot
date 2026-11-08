package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Session is the persisted conversation of one private chat.
type Session struct {
	Messages []LLMMessage
	Updated  time.Time
}

var (
	sessMu    sync.Mutex
	sessions  = map[int64]*Session{}
	lastThink = map[int64]string{}
)

func sessionPath(chatID int64) string {
	return filepath.Join(baseDir, "sessions", fmt.Sprintf("%d.json", chatID))
}

func getHistory(chatID int64) []LLMMessage {
	sessMu.Lock()
	defer sessMu.Unlock()
	if _, ok := sessions[chatID]; !ok {
		if f, err := os.Open(sessionPath(chatID)); err == nil {
			defer f.Close()
			var s Session
			if json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&s) == nil {
				clean := make([]LLMMessage, 0, len(s.Messages))
				for _, m := range s.Messages {
					if m.Role == "user" || m.Role == "assistant" || m.Role == "tool" {
						clean = append(clean, m)
					}
				}
				s.Messages = clean
				sessions[chatID] = &s
			}
		}
	}
	if s, ok := sessions[chatID]; ok {
		return append([]LLMMessage(nil), s.Messages...)
	}
	return nil
}

func setHistory(chatID int64, msgs []LLMMessage) {
	setHistoryLimit(chatID, msgs, configSnapshot().MaxHistory)
}

func setHistoryLimit(chatID int64, msgs []LLMMessage, limit int) {
	sessMu.Lock()
	defer sessMu.Unlock()
	s := &Session{Messages: trimHistoryLimit(msgs, limit), Updated: time.Now()}
	sessions[chatID] = s
	data, err := json.Marshal(s)
	if err != nil {
		logf("序列化会话失败: %v", err)
		return
	}
	if len(data) > 16<<20 {
		logf("会话过大，跳过持久化 chat=%d", chatID)
		return
	}
	if err := atomicWriteFile(sessionPath(chatID), data, 0600); err != nil {
		logf("保存会话失败: %v", err)
	}
}

func resetHistory(chatID int64) {
	sessMu.Lock()
	defer sessMu.Unlock()
	delete(sessions, chatID)
	delete(lastThink, chatID)
	_ = os.Remove(sessionPath(chatID))
}

func trimHistory(msgs []LLMMessage) []LLMMessage {
	return trimHistoryLimit(msgs, configSnapshot().MaxHistory)
}

// trimHistoryLimit keeps the last `limit` complete user turns.
func trimHistoryLimit(msgs []LLMMessage, limit int) []LLMMessage {
	if limit <= 0 {
		limit = 40
	}
	var starts []int
	for i, m := range msgs {
		if m.Role == "user" {
			starts = append(starts, i)
		}
	}
	if len(starts) <= limit {
		return msgs
	}
	return msgs[starts[len(starts)-limit]:]
}

func setLastThink(chatID int64, text string) {
	sessMu.Lock()
	lastThink[chatID] = text
	sessMu.Unlock()
}

func getLastThink(chatID int64) string {
	sessMu.Lock()
	defer sessMu.Unlock()
	return lastThink[chatID]
}

// historyTurns counts user turns for status displays.
func historyTurns(chatID int64) int {
	n := 0
	for _, m := range getHistory(chatID) {
		if m.Role == "user" {
			n++
		}
	}
	return n
}
