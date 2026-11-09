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

// Announcements are published from the server terminal (tg-agent admin):
// written, previewed, confirmed, then pushed to every user. Users read them
// later with the "Notices" button or /notice.

const (
	maxNotices      = 50
	maxNoticeRunes  = 3000
	noticeSendDelay = 60 * time.Millisecond // stays far below Telegram's broadcast limit
)

type Announcement struct {
	ID      int    `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"`
	By      int64  `json:"by"`
	Sent    int    `json:"sent"`
	Total   int    `json:"total"`
}

var noticeMu sync.Mutex

func noticePath() string { return filepath.Join(baseDir, "announcements.json") }

func loadNoticesLocked() []Announcement {
	raw, err := os.ReadFile(noticePath())
	if err != nil {
		return nil
	}
	var list []Announcement
	if json.Unmarshal(raw, &list) != nil {
		logf("公告文件损坏，已忽略")
		return nil
	}
	return list
}

func saveNoticesLocked(list []Announcement) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(noticePath(), data, 0600)
}

// listNotices returns announcements newest first.
func listNotices() []Announcement {
	noticeMu.Lock()
	defer noticeMu.Unlock()
	list := loadNoticesLocked()
	out := make([]Announcement, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i])
	}
	return out
}

func addNotice(text string, by int64) (Announcement, error) {
	noticeMu.Lock()
	defer noticeMu.Unlock()
	list := loadNoticesLocked()
	next := 1
	for _, n := range list {
		next = max(next, n.ID+1)
	}
	n := Announcement{ID: next, Text: text, Created: time.Now().Unix(), By: by}
	list = append(list, n)
	if len(list) > maxNotices {
		list = list[len(list)-maxNotices:]
	}
	return n, saveNoticesLocked(list)
}

func deleteNotice(id int) bool {
	noticeMu.Lock()
	defer noticeMu.Unlock()
	list := loadNoticesLocked()
	for i, n := range list {
		if n.ID == id {
			if saveNoticesLocked(append(list[:i], list[i+1:]...)) == nil {
				return true
			}
			return false
		}
	}
	return false
}

func recordNoticeDelivery(id, sent, total int) {
	noticeMu.Lock()
	defer noticeMu.Unlock()
	list := loadNoticesLocked()
	for i := range list {
		if list[i].ID == id {
			list[i].Sent, list[i].Total = sent, total
		}
	}
	_ = saveNoticesLocked(list)
}

// noticeRecipients is every user: owner, admins and friends, minus banned.
func noticeRecipients() []int64 {
	c := configSnapshot()
	var out []int64
	seenIDs := map[int64]bool{}
	for _, id := range append(append([]int64{c.OwnerID}, c.Admins...), c.Friends...) {
		if id > 0 && !seenIDs[id] && !containsID(c.Banned, id) {
			seenIDs[id] = true
			out = append(out, id)
		}
	}
	return out
}

// noticeHTML is one announcement as users receive it.
func noticeHTML(lang string, n Announcement) string {
	stamp := time.Unix(n.Created, 0).Format("2006-01-02 15:04")
	if n.ID > 0 { // drafts have no number yet
		stamp = "#" + strconv.Itoa(n.ID) + " · " + stamp
	}
	return "<b>▍" + esc(tr(lang, "notice.title")) + "</b> · " + esc(appName(lang)) + "\n\n" +
		markdownToHTML(n.Text) + "\n\n<i>" + stamp + "</i>"
}

// noticesPanel is the read-only list for users (latest three).
func noticesPanel(uid int64) (string, map[string]interface{}) {
	lang := langFor(uid)
	list := listNotices()
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "notice.title")) + "</b>\n")
	if len(list) == 0 {
		b.WriteString("\n" + esc(tr(lang, "notice.empty")))
	}
	for i, n := range list {
		if i >= 3 {
			b.WriteString("\n<i>" + esc(tr(lang, "notice.more", len(list)-i)) + "</i>")
			break
		}
		body := markdownToHTML(cutAtParagraph(n.Text, 600))
		if !balancedHTML(body) {
			body = esc(short(n.Text, 600))
		}
		fmt.Fprintf(&b, "\n<i>#%d · %s</i>\n%s\n", n.ID, time.Unix(n.Created, 0).Format("2006-01-02 15:04"), body)
	}
	text := strings.TrimRight(b.String(), "\n")
	if utf16Len(text) > telegramChunk {
		text = truncateTelegramText(stripTags(text), telegramChunk)
		text = esc(text)
	}
	return text, keyboard([][]Btn{{btn(tr(lang, "btn.back"), "nav:root")}})
}

// publishNotice stores an announcement and pushes it to every user, one by
// one, waiting for the deliveries. It returns the announcement with the
// delivery count. Used by the server terminal (tg-agent admin).
func publishNotice(ctx context.Context, text string, by int64) (Announcement, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Announcement{}, fmt.Errorf("公告内容不能为空")
	}
	if len([]rune(text)) > maxNoticeRunes {
		return Announcement{}, fmt.Errorf("公告最多 %d 字", maxNoticeRunes)
	}
	n, err := addNotice(text, by)
	if err != nil {
		return n, err
	}
	recipients := noticeRecipients()
	sent := 0
	for i, id := range recipients {
		if ctx.Err() != nil {
			break
		}
		if i > 0 {
			_ = waitContext(ctx, noticeSendDelay)
		}
		if sendHTML(ctx, id, noticeHTML(langFor(id), n), "", nil) != 0 {
			sent++
		}
	}
	n.Sent, n.Total = sent, len(recipients)
	recordNoticeDelivery(n.ID, sent, len(recipients))
	logf("公告 #%d 已推送 %d/%d", n.ID, sent, len(recipients))
	return n, nil
}
