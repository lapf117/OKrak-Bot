package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Long-term memory: short notes per user that survive /new. They are injected
// into the system prompt as low-trust preferences and can be reviewed with /memory.

const (
	maxMemoryItems = 50
	maxMemoryRunes = 500
)

type MemoryItem struct {
	ID      int    `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"`
}

var memoryMu sync.Mutex

func memoryPath(uid int64) string {
	return filepath.Join(baseDir, "memory", strconv.FormatInt(uid, 10)+".json")
}

func loadMemory(uid int64) []MemoryItem {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return loadMemoryLocked(uid)
}

func loadMemoryLocked(uid int64) []MemoryItem {
	raw, err := os.ReadFile(memoryPath(uid))
	if err != nil {
		return nil
	}
	var items []MemoryItem
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

func saveMemoryLocked(uid int64, items []MemoryItem) error {
	if len(items) == 0 {
		err := os.Remove(memoryPath(uid))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(memoryPath(uid), data, 0600)
}

func addMemory(uid int64, text string) (int, error) {
	text = strings.TrimSpace(redactSecrets(text))
	if text == "" {
		return 0, fmt.Errorf("备忘内容不能为空")
	}
	memoryMu.Lock()
	defer memoryMu.Unlock()
	items := loadMemoryLocked(uid)
	for _, it := range items {
		if it.Text == text {
			return it.ID, nil
		}
	}
	if len(items) >= maxMemoryItems {
		return 0, fmt.Errorf("备忘最多 %d 条，请先删除旧的", maxMemoryItems)
	}
	id := 1
	for _, it := range items {
		if it.ID >= id {
			id = it.ID + 1
		}
	}
	items = append(items, MemoryItem{ID: id, Text: short(text, maxMemoryRunes), Created: time.Now().Unix()})
	return id, saveMemoryLocked(uid, items)
}

func deleteMemory(uid int64, id int) (bool, error) {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	items := loadMemoryLocked(uid)
	var kept []MemoryItem
	for _, it := range items {
		if it.ID != id {
			kept = append(kept, it)
		}
	}
	if len(kept) == len(items) {
		return false, nil
	}
	return true, saveMemoryLocked(uid, kept)
}

func clearMemory(uid int64) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return saveMemoryLocked(uid, nil)
}

func renderMemoryList(items []MemoryItem) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "#%d %s\n", it.ID, it.Text)
	}
	return b.String()
}

// memoryPrompt is the block injected into the system prompt.
func memoryPrompt(uid int64) string {
	if uid == 0 {
		return ""
	}
	return short(renderMemoryList(loadMemory(uid)), 4000)
}

func toolMemoryContext(ctx context.Context, args map[string]interface{}) (string, error) {
	uid := executionUID(ctx)
	if uid == 0 {
		return "", fmt.Errorf("备忘需要用户身份")
	}
	switch strArg(args, "action") {
	case "add":
		id, err := addMemory(uid, strArg(args, "text"))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已记住 #%d。用户可用 /memory 查看或删除。", id), nil
	case "delete":
		ok, err := deleteMemory(uid, intArg(args, "id", 0, 0, 1<<30))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("没有这条备忘")
		}
		return "已删除。", nil
	case "list", "":
		if list := renderMemoryList(loadMemory(uid)); list != "" {
			return list, nil
		}
		return "（没有备忘）", nil
	}
	return "", fmt.Errorf("未知备忘操作，可用 add / list / delete")
}

// handleMemoryCommand implements /memory [add 文本 | del 编号 | clear].
func handleMemoryCommand(chatID, uid int64, rest string) bool {
	lang := langFor(uid)
	fields := strings.Fields(rest)
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	switch sub {
	case "add":
		text := argsAfter(rest, 1)
		if text == "" {
			sendMessage(chatID, tr(lang, "memory.usage"), nil)
			return true
		}
		id, err := addMemory(uid, text)
		if err != nil {
			sendMessage(chatID, "⚠️ "+err.Error(), nil)
			return true
		}
		sendMessage(chatID, tr(lang, "memory.added", id), nil)
	case "del", "delete", "rm":
		id, err := strconv.Atoi(argsAfter(rest, 1))
		if ok, e := deleteMemory(uid, id); err != nil || e != nil || !ok {
			sendMessage(chatID, tr(lang, "memory.not_found"), nil)
			return true
		}
		sendMessage(chatID, tr(lang, "memory.deleted", id), nil)
	case "clear":
		if err := clearMemory(uid); err != nil {
			sendMessage(chatID, tr(lang, "err.save", err.Error()), nil)
			return true
		}
		sendMessage(chatID, tr(lang, "memory.cleared"), nil)
	default:
		sendUI(chatID, memoryPanel(uid, lang), memoryMenu(lang))
	}
	return true
}

func memoryPanel(uid int64, lang string) string {
	items := loadMemory(uid)
	var b strings.Builder
	b.WriteString("🧠 <b>" + esc(tr(lang, "memory.title")) + "</b>\n\n")
	if len(items) == 0 {
		b.WriteString(esc(tr(lang, "memory.empty")) + "\n")
	} else {
		for _, it := range items {
			fmt.Fprintf(&b, "<code>#%d</code> %s\n", it.ID, esc(it.Text))
		}
	}
	b.WriteString("\n<i>" + esc(tr(lang, "memory.help")) + "</i>")
	return b.String()
}

func memoryMenu(lang string) interface{} {
	return keyboard([][]Btn{{btn(tr(lang, "btn.clear_memory"), "memory:clear"), btn(tr(lang, "btn.back"), "nav:root")}})
}
