package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Announcements are written in the admin console (or with /announce), shown
// as a preview, and only pushed to every user after the admin confirms.
// Users read them later with the "Announcements" button or /notice.

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

// noticeAdminPanel lists announcements with delete buttons and a publish button.
func noticeAdminPanel(lang string) (string, map[string]interface{}) {
	list := listNotices()
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "btn.announce")) + "</b>\n")
	b.WriteString("<i>" + esc(tr(lang, "notice.admin_hint", len(noticeRecipients()))) + "</i>\n")
	var rows [][]Btn
	rows = append(rows, []Btn{btn(tr(lang, "notice.new"), "adm:in:announce")})
	var del []Btn
	for i, n := range list {
		if i >= 6 {
			break
		}
		delivery := ""
		if n.Total > 0 {
			delivery = " · " + tr(lang, "notice.delivered", n.Sent, n.Total)
		}
		fmt.Fprintf(&b, "\n<b>#%d</b> <i>%s%s</i>\n%s\n", n.ID, time.Unix(n.Created, 0).Format("01-02 15:04"), esc(delivery), esc(oneLine(n.Text, 80)))
		del = append(del, btn(tr(lang, "notice.del", n.ID), "ntc:del:"+strconv.Itoa(n.ID)))
		if len(del) == 3 {
			rows = append(rows, del)
			del = nil
		}
	}
	if len(del) > 0 {
		rows = append(rows, del)
	}
	if len(list) == 0 {
		b.WriteString("\n" + esc(tr(lang, "notice.empty")))
	}
	rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "adm:root")})
	return strings.TrimRight(b.String(), "\n"), keyboard(rows)
}

// ---------------- drafts and publishing ----------------

var noticeDrafts = struct {
	sync.Mutex
	m map[int64]string
}{m: map[int64]string{}}

// showNoticeDraft keeps the text as the admin's draft and shows a preview
// with confirm / cancel buttons. Nothing is sent before confirmation.
func showNoticeDraft(chatID, uid int64, text string) {
	lang := langFor(uid)
	text = strings.TrimSpace(text)
	if text == "" {
		sendMessage(chatID, tr(lang, "notice.usage"), nil)
		return
	}
	if len([]rune(text)) > maxNoticeRunes {
		sendMessage(chatID, tr(lang, "notice.too_long", maxNoticeRunes), nil)
		return
	}
	noticeDrafts.Lock()
	noticeDrafts.m[uid] = text
	noticeDrafts.Unlock()
	preview := noticeHTML(lang, Announcement{ID: 0, Text: text, Created: time.Now().Unix()})
	header := "<b>" + esc(tr(lang, "notice.preview", len(noticeRecipients()))) + "</b>\n\n"
	sendUI(chatID, header+preview, keyboard([][]Btn{{btn("✓ "+tr(lang, "notice.confirm"), "ntc:pub"), btn("✗ "+tr(lang, "notice.cancel"), "ntc:cancel")}}))
}

// publishNotice stores the admin's draft and pushes it to every user in the
// background, then reports the delivery count on the preview message.
func publishNotice(chatID, msgID, uid int64) string {
	lang := langFor(uid)
	noticeDrafts.Lock()
	text, ok := noticeDrafts.m[uid]
	delete(noticeDrafts.m, uid)
	noticeDrafts.Unlock()
	if !ok {
		return tr(lang, "notice.no_draft")
	}
	n, err := addNotice(text, uid)
	if err != nil {
		return tr(lang, "err.save", err.Error())
	}
	recipients := noticeRecipients()
	editUI(chatID, msgID, "<b>"+esc(tr(lang, "notice.sending", n.ID, len(recipients)))+"</b>", nil)
	go func() {
		sent := 0
		for i, id := range recipients {
			if telegramContext.Err() != nil {
				break
			}
			if i > 0 {
				_ = waitContext(telegramContext, noticeSendDelay)
			}
			if sendUI(id, noticeHTML(langFor(id), n), nil) != 0 {
				sent++
			}
		}
		recordNoticeDelivery(n.ID, sent, len(recipients))
		logf("公告 #%d 已推送 %d/%d", n.ID, sent, len(recipients))
		editUI(chatID, msgID, "<b>"+esc(tr(lang, "notice.published", n.ID, sent, len(recipients)))+"</b>", keyboard([][]Btn{{btn(tr(lang, "btn.announce"), "adm:notice")}}))
	}()
	return tr(lang, "notice.confirmed")
}

// handleNoticeCallback serves ntc:* buttons from admins.
func handleNoticeCallback(chatID, msgID, uid int64, data string) string {
	lang := langFor(uid)
	switch {
	case data == "ntc:pub":
		return publishNotice(chatID, msgID, uid)
	case data == "ntc:cancel":
		noticeDrafts.Lock()
		delete(noticeDrafts.m, uid)
		noticeDrafts.Unlock()
		editUI(chatID, msgID, esc(tr(lang, "notice.cancelled")), nil)
		return tr(lang, "notice.cancelled")
	case strings.HasPrefix(data, "ntc:del:"):
		id, err := strconv.Atoi(strings.TrimPrefix(data, "ntc:del:"))
		if err != nil || !deleteNotice(id) {
			return tr(lang, "notice.not_found")
		}
		text, kb := noticeAdminPanel(lang)
		editUI(chatID, msgID, text, kb)
		return tr(lang, "notice.deleted", id)
	}
	return ""
}

// handleNoticeCommand: /notice shows announcements; admins can also use
// /notice del <id> and /announce <text> (preview first).
func handleNoticeCommand(chatID, uid int64, cmd, rest string) {
	lang := langFor(uid)
	if cmd == "/announce" {
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return
		}
		if strings.TrimSpace(rest) == "" {
			expectAdminInput(uid, "announce")
			sendUI(chatID, "<b>▍"+esc(tr(lang, "input.title.announce"))+"</b>\n"+esc(tr(lang, "input.prompt.announce"))+"\n\n<i>"+esc(tr(lang, "input.cancel_hint"))+"</i>", nil)
			return
		}
		showNoticeDraft(chatID, uid, rest)
		return
	}
	fields := strings.Fields(rest)
	if len(fields) >= 2 && (fields[0] == "del" || fields[0] == "rm") && isAdmin(uid) {
		id, _ := strconv.Atoi(strings.TrimPrefix(fields[1], "#"))
		if deleteNotice(id) {
			sendMessage(chatID, tr(lang, "notice.deleted", id), nil)
		} else {
			sendMessage(chatID, tr(lang, "notice.not_found"), nil)
		}
		return
	}
	text, kb := noticesPanel(uid)
	sendUI(chatID, text, kb)
}
