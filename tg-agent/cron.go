package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------- scheduled tasks ----------------

const (
	cronEvery    = "every"
	cronDaily    = "daily"
	cronWeekdays = "weekdays"
	cronOnce     = "once"

	maxCronJobs        = 50
	maxCronJobsPerChat = 20
)

type CronJob struct {
	ID      int    `json:"id"`
	ChatID  int64  `json:"chat_id"`
	UserID  int64  `json:"user_id"`
	Kind    string `json:"kind,omitempty"` // every | daily | weekdays | once; empty = every (v2.0)
	Every   int64  `json:"every"`          // seconds, for every
	At      string `json:"at,omitempty"`   // HH:MM, for daily/weekdays
	Prompt  string `json:"prompt"`
	NextRun int64  `json:"next_run"`
	Enabled bool   `json:"enabled"`
	Source  string `json:"source,omitempty"` // user | agent
	Created int64  `json:"created,omitempty"`
	LastRun int64  `json:"last_run,omitempty"`
	Runs    int    `json:"runs,omitempty"`
	Done    bool   `json:"done,omitempty"` // a one-shot job that already ran
}

func (j CronJob) kind() string {
	if j.Kind == "" {
		return cronEvery
	}
	return j.Kind
}

var (
	cronMu     sync.Mutex
	cronJobs   []CronJob
	cronSeq    int
	cronSaveMu sync.Mutex
)

func cronPath() string { return filepath.Join(baseDir, "cron.json") }

func loadCron() {
	b, err := os.ReadFile(cronPath())
	if err != nil {
		return
	}
	cronMu.Lock()
	defer cronMu.Unlock()
	if err := json.Unmarshal(b, &cronJobs); err != nil {
		logf("定时任务配置损坏: %v", err)
		return
	}
	for i := range cronJobs {
		j := &cronJobs[i]
		// Jobs without a creator must never run with the owner's identity.
		if j.UserID == 0 || (j.kind() == cronEvery && j.Every < 60) {
			j.Enabled = false
		}
		if j.ID > cronSeq {
			cronSeq = j.ID
		}
	}
}

func saveCron() {
	cronSaveMu.Lock()
	defer cronSaveMu.Unlock()
	cronMu.Lock()
	data, err := json.MarshalIndent(cronJobs, "", "  ")
	cronMu.Unlock()
	if err == nil {
		err = atomicWriteFile(cronPath(), data, 0600)
	}
	if err != nil {
		logf("定时任务保存失败: %v", err)
	}
}

// ---------------- schedule grammar ----------------

// scheduleSpec is a parsed time rule:
//
//	30m | every 2h        repeat
//	daily 09:00           every day
//	weekdays 09:00        Monday–Friday
//	at 18:30              once, next occurrence
//	at 2026-12-01 09:00   once, explicit date
//	in 45m                once, after a delay
type scheduleSpec struct {
	Kind  string
	Every int64
	At    string
	Once  time.Time
}

var clockRe = regexp.MustCompile(`^([01]?\d|2[0-3])[:：]([0-5]\d)$`)

func parseClock(s string) (int, int, error) {
	m := clockRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, fmt.Errorf("时间格式应为 HH:MM，例如 09:00")
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h, mi, nil
}

// parseEvery accepts 90s/30m/2h/1d/1w and Go durations like 1h30m (60s..366d).
func parseEvery(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid interval")
	}
	var seconds int64
	units := map[byte]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 7 * 86400}
	if mult, ok := units[s[len(s)-1]]; ok {
		if n, err := strconv.ParseInt(s[:len(s)-1], 10, 64); err == nil {
			if n <= 0 || n > 366*86400/mult {
				return 0, fmt.Errorf("invalid interval")
			}
			seconds = n * mult
		}
	}
	if seconds == 0 {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("invalid interval")
		}
		seconds = int64(d / time.Second)
	}
	if seconds < 60 || seconds > 366*86400 {
		return 0, fmt.Errorf("interval must be between 60 seconds and 366 days")
	}
	return seconds, nil
}

// parseScheduleFields consumes a schedule rule from the start of fields and
// reports how many fields it used.
func parseScheduleFields(fields []string, now time.Time, loc *time.Location) (scheduleSpec, int, error) {
	if len(fields) == 0 {
		return scheduleSpec{}, 0, fmt.Errorf("缺少时间规则")
	}
	head := strings.ToLower(fields[0])
	need := func(n int) error {
		if len(fields) < n {
			return fmt.Errorf("时间规则不完整")
		}
		return nil
	}
	switch head {
	case "every", "每":
		if err := need(2); err != nil {
			return scheduleSpec{}, 0, err
		}
		sec, err := parseEvery(fields[1])
		return scheduleSpec{Kind: cronEvery, Every: sec}, 2, err
	case "daily", "每天", "weekdays", "工作日":
		if err := need(2); err != nil {
			return scheduleSpec{}, 0, err
		}
		h, m, err := parseClock(fields[1])
		kind := cronDaily
		if head == "weekdays" || head == "工作日" {
			kind = cronWeekdays
		}
		return scheduleSpec{Kind: kind, At: fmt.Sprintf("%02d:%02d", h, m)}, 2, err
	case "in", "after", "过":
		if err := need(2); err != nil {
			return scheduleSpec{}, 0, err
		}
		sec, err := parseEvery(fields[1])
		return scheduleSpec{Kind: cronOnce, Once: now.Add(time.Duration(sec) * time.Second)}, 2, err
	case "at", "于":
		if err := need(2); err != nil {
			return scheduleSpec{}, 0, err
		}
		now = now.In(loc)
		if h, m, err := parseClock(fields[1]); err == nil {
			t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, loc)
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
			return scheduleSpec{Kind: cronOnce, Once: t}, 2, nil
		}
		for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
			if t, err := time.ParseInLocation(layout, fields[1], loc); err == nil {
				return checkOnce(t, now, 2)
			}
		}
		if len(fields) >= 3 {
			if t, err := time.ParseInLocation("2006-01-02 15:04", fields[1]+" "+strings.ReplaceAll(fields[2], "：", ":"), loc); err == nil {
				return checkOnce(t, now, 3)
			}
		}
		return scheduleSpec{}, 0, fmt.Errorf("at 后应为 HH:MM 或 YYYY-MM-DD HH:MM")
	}
	sec, err := parseEvery(fields[0])
	if err != nil {
		return scheduleSpec{}, 0, fmt.Errorf("无法识别时间规则 %q", fields[0])
	}
	return scheduleSpec{Kind: cronEvery, Every: sec}, 1, nil
}

func checkOnce(t, now time.Time, used int) (scheduleSpec, int, error) {
	if !t.After(now) {
		return scheduleSpec{}, 0, fmt.Errorf("时间 %s 已经过去", t.Format("2006-01-02 15:04"))
	}
	if t.Sub(now) > 366*24*time.Hour {
		return scheduleSpec{}, 0, fmt.Errorf("最多提前一年")
	}
	return scheduleSpec{Kind: cronOnce, Once: t}, used, nil
}

// parseScheduleText parses a whole rule (used by the schedule tool).
func parseScheduleText(text string, loc *time.Location) (scheduleSpec, error) {
	fields := strings.Fields(text)
	spec, used, err := parseScheduleFields(fields, time.Now(), loc)
	if err != nil {
		return spec, err
	}
	if used != len(fields) {
		return spec, fmt.Errorf("时间规则有多余内容: %s", strings.Join(fields[used:], " "))
	}
	return spec, nil
}

func (s scheduleSpec) describe(lang string) string {
	switch s.Kind {
	case cronDaily:
		return tr(lang, "cron.daily", s.At)
	case cronWeekdays:
		return tr(lang, "cron.weekdays", s.At)
	case cronOnce:
		return tr(lang, "cron.once", s.Once.Format("01-02 15:04"))
	}
	return tr(lang, "cron.every", humanEvery(s.Every, lang))
}

func jobSpec(j CronJob) scheduleSpec {
	return scheduleSpec{Kind: j.kind(), Every: j.Every, At: j.At, Once: time.Unix(j.NextRun, 0)}
}

func humanEvery(sec int64, lang string) string {
	type unit struct {
		size   int64
		zh, en string
	}
	for _, u := range []unit{{7 * 86400, "周", "w"}, {86400, "天", "d"}, {3600, "小时", "h"}, {60, "分钟", "min"}} {
		if sec%u.size == 0 {
			if lang == langEN {
				return fmt.Sprintf("%d%s", sec/u.size, u.en)
			}
			return fmt.Sprintf("%d %s", sec/u.size, u.zh)
		}
	}
	if lang == langEN {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%d 秒", sec)
}

// nextRunAfter computes the next fire time strictly after t.
func nextRunAfter(j CronJob, t time.Time, loc *time.Location) (int64, bool) {
	switch j.kind() {
	case cronOnce:
		return 0, false
	case cronDaily, cronWeekdays:
		h, m, err := parseClock(j.At)
		if err != nil {
			return 0, false
		}
		local := t.In(loc)
		next := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, loc)
		for !next.After(local) || (j.kind() == cronWeekdays && (next.Weekday() == time.Saturday || next.Weekday() == time.Sunday)) {
			next = next.AddDate(0, 0, 1)
		}
		return next.Unix(), true
	}
	if j.Every < 60 {
		return 0, false
	}
	return t.Unix() + j.Every, true
}

func formatRunTime(unix int64) string {
	if unix <= 0 {
		return "-"
	}
	return time.Unix(unix, 0).In(scheduleLocation(configSnapshot())).Format("01-02 15:04")
}

// ---------------- job management ----------------

func addCron(chatID int64, every int64, prompt string) int {
	return addCronFor(chatID, configSnapshot().OwnerID, every, prompt)
}

func addCronFor(chatID, uid int64, every int64, prompt string) int {
	job, err := addCronJob(chatID, uid, scheduleSpec{Kind: cronEvery, Every: every}, prompt, "user")
	if err != nil {
		return 0
	}
	return job.ID
}

func addCronJob(chatID, uid int64, spec scheduleSpec, prompt, source string) (CronJob, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return CronJob{}, fmt.Errorf("任务内容不能为空")
	}
	now := time.Now()
	job := CronJob{ChatID: chatID, UserID: uid, Kind: spec.Kind, Every: spec.Every, At: spec.At, Prompt: short(prompt, 4000), Enabled: true, Source: source, Created: now.Unix()}
	switch spec.Kind {
	case cronOnce:
		job.NextRun = spec.Once.Unix()
	case cronEvery:
		if spec.Every < 60 {
			return CronJob{}, fmt.Errorf("间隔至少 1 分钟")
		}
		job.NextRun = now.Unix() + spec.Every
	default:
		next, ok := nextRunAfter(job, now, scheduleLocation(configSnapshot()))
		if !ok {
			return CronJob{}, fmt.Errorf("无效时间规则")
		}
		job.NextRun = next
	}
	cronMu.Lock()
	active, perChat := 0, 0
	for _, j := range cronJobs {
		if !j.Done {
			active++
			if j.ChatID == chatID {
				perChat++
			}
		}
	}
	if active >= maxCronJobs || perChat >= maxCronJobsPerChat {
		cronMu.Unlock()
		return CronJob{}, fmt.Errorf("定时任务数量已达上限（每个会话 %d 个，总计 %d 个）", maxCronJobsPerChat, maxCronJobs)
	}
	cronSeq++
	job.ID = cronSeq
	cronJobs = append(cronJobs, job)
	cronMu.Unlock()
	saveCron()
	return job, nil
}

// deleteCronJob removes a job; chatID 0 means any chat (admin console).
func deleteCronJob(id int, chatID int64) error {
	cronMu.Lock()
	found := false
	var keep []CronJob
	for _, j := range cronJobs {
		if j.ID == id && (chatID == 0 || j.ChatID == chatID) {
			found = true
			continue
		}
		keep = append(keep, j)
	}
	cronJobs = keep
	cronMu.Unlock()
	if !found {
		return fmt.Errorf("没有定时任务 #%d", id)
	}
	saveCron()
	return nil
}

func delCron(id int) bool { return deleteCronJob(id, 0) == nil }

func setCronEnabled(id int, chatID int64, enabled bool) error {
	cronMu.Lock()
	var err error = fmt.Errorf("没有定时任务 #%d", id)
	for i := range cronJobs {
		j := &cronJobs[i]
		if j.ID != id || (chatID != 0 && j.ChatID != chatID) {
			continue
		}
		switch {
		case enabled && j.UserID == 0:
			err = fmt.Errorf("旧版任务没有创建者，请删除后重新创建")
		case enabled && j.Done:
			err = fmt.Errorf("一次性任务已执行完毕")
		default:
			j.Enabled = enabled
			if enabled && j.NextRun <= time.Now().Unix() {
				if next, ok := nextRunAfter(*j, time.Now(), scheduleLocation(configSnapshot())); ok {
					j.NextRun = next
				} else {
					j.NextRun = time.Now().Unix() + 60
				}
			}
			err = nil
		}
	}
	cronMu.Unlock()
	if err == nil {
		saveCron()
	}
	return err
}

func cronSnapshot() []CronJob {
	cronMu.Lock()
	defer cronMu.Unlock()
	out := append([]CronJob(nil), cronJobs...)
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}

// listCronText renders jobs as plain text; chatID 0 lists every chat.
func listCronText(lang string, chatID int64) string {
	var b strings.Builder
	for _, j := range cronSnapshot() {
		if chatID != 0 && j.ChatID != chatID {
			continue
		}
		state := "✅"
		switch {
		case j.Done:
			state = "☑️"
		case !j.Enabled:
			state = "⏸"
		}
		fmt.Fprintf(&b, "%s #%d · %s · %s %s", state, j.ID, jobSpec(j).describe(lang), tr(lang, "cron.next"), formatRunTime(j.NextRun))
		if j.Done {
			b.WriteString(" · " + tr(lang, "cron.done"))
		}
		if j.Runs > 0 {
			b.WriteString(" · " + tr(lang, "cron.runs", j.Runs))
		}
		if j.Source == "agent" {
			b.WriteString(" · 🤖")
		}
		fmt.Fprintf(&b, "\n   %s\n", oneLine(j.Prompt, 80))
	}
	return b.String()
}

func listCron() string {
	if s := listCronText(langZH, 0); s != "" {
		return s
	}
	return tr(langZH, "cron.empty")
}

// ---------------- scheduler ----------------

func startCron() {
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-telegramContext.Done():
				return
			case <-t.C:
			}
			for _, job := range dueCronJobs(time.Now()) {
				go fireCron(job)
			}
		}
	}()
}

// dueCronJobs advances due jobs and returns those to fire now. A job whose
// chat is busy is postponed by a minute instead of interrupting the user.
func dueCronJobs(now time.Time) []CronJob {
	loc := scheduleLocation(configSnapshot())
	cronMu.Lock()
	var due []CronJob
	changed := false
	kept := cronJobs[:0]
	for _, j := range cronJobs {
		// Finished one-shot jobs are kept for three days for reference.
		if j.Done && now.Unix()-j.LastRun > 3*86400 {
			changed = true
			continue
		}
		if j.Enabled && j.UserID != 0 && j.NextRun <= now.Unix() {
			changed = true
			if isBusy(j.ChatID) {
				j.NextRun = now.Unix() + 60
			} else {
				j.LastRun = now.Unix()
				j.Runs++
				if next, ok := nextRunAfter(j, now, loc); ok {
					j.NextRun = next
				} else {
					j.Enabled, j.Done = false, true
				}
				due = append(due, j)
			}
		}
		kept = append(kept, j)
	}
	cronJobs = kept
	cronMu.Unlock()
	if changed {
		saveCron()
	}
	return due
}

func fireCron(job CronJob) {
	logf("定时任务 #%d 触发: %s", job.ID, short(job.Prompt, 60))
	if !allowed(job.UserID) || !isAdmin(job.UserID) {
		logf("定时任务 #%d 创建者已无管理员权限，跳过", job.ID)
		return
	}
	origin := fmt.Sprintf("⏰ #%d", job.ID)
	if !runUserTaskOpts(job.ChatID, job.UserID, job.Prompt, nil, taskOptions{Origin: origin, Quiet: true}) {
		postponeCron(job.ID)
	}
}

// postponeCron retries a job in a minute when its chat became busy.
func postponeCron(id int) {
	cronMu.Lock()
	for i := range cronJobs {
		if cronJobs[i].ID == id {
			cronJobs[i].NextRun = time.Now().Unix() + 60
			cronJobs[i].Runs--
			if cronJobs[i].Done {
				cronJobs[i].Done, cronJobs[i].Enabled = false, true
			}
		}
	}
	cronMu.Unlock()
	saveCron()
}

// runCronNow fires a job immediately without changing its schedule.
func runCronNow(id int) error {
	for _, j := range cronSnapshot() {
		if j.ID == id {
			if j.UserID == 0 {
				return fmt.Errorf("旧版任务没有创建者，请删除后重新创建")
			}
			if isBusy(j.ChatID) {
				return fmt.Errorf("当前已有任务在运行")
			}
			go runUserTaskOpts(j.ChatID, j.UserID, j.Prompt, nil, taskOptions{Origin: fmt.Sprintf("⏰ #%d", j.ID)})
			return nil
		}
	}
	return fmt.Errorf("没有定时任务 #%d", id)
}
