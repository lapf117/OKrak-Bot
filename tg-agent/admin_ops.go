package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Server-terminal management (tg-agent admin). Each operation returns plain
// text for the terminal. The same code runs inside the live bot (reached
// through the admin socket) or, when the bot is stopped, in the terminal
// process itself.

type adminOp struct {
	run      func(ctx context.Context, lang string, args []string) (string, error)
	needsBot bool // only meaningful inside the running bot (e.g. run a schedule now)
}

var adminOps map[string]adminOp

func init() {
	adminOps = map[string]adminOp{
		"status":            {run: opStatus},
		"users.list":        {run: opUsersList},
		"users.add":         {run: opUsersChange(true)},
		"users.del":         {run: opUsersChange(false)},
		"reg.list":          {run: opRegList},
		"reg.ids":           {run: opRegIDs},
		"reg.approve":       {run: opRegDecide("ok")},
		"reg.reject":        {run: opRegDecide("no")},
		"reg.ban":           {run: opRegDecide("ban")},
		"reg.mode":          {run: opRegMode},
		"notice.list":       {run: opNoticeList},
		"notice.recipients": {run: opNoticeRecipients},
		"notice.publish":    {run: opNoticePublish},
		"notice.del":        {run: opNoticeDel},
		"brand.list":        {run: opBrandShow},
		"brand.set":         {run: opBrandSet},
		"provider.list":     {run: opProviderList},
		"provider.use":      {run: opProviderUse},
		"provider.add":      {run: opProviderAdd},
		"provider.del":      {run: opProviderDel},
		"workspace.list":    {run: opWorkspaceList},
		"workspace.use":     {run: opWorkspaceUse},
		"workspace.add":     {run: opWorkspaceAdd},
		"workspace.del":     {run: opWorkspaceDel},
		"prompt.show":       {run: opPromptShow},
		"prompt.set":        {run: opPromptSet},
		"cron.list":         {run: opCronList},
		"cron.add":          {run: opCronAdd},
		"cron.del":          {run: opCronChange("del")},
		"cron.pause":        {run: opCronChange("pause")},
		"cron.resume":       {run: opCronChange("resume")},
		"cron.run":          {run: opCronChange("run"), needsBot: true},
		"cmd.list":          {run: opCmdList},
		"cmd.add":           {run: opCmdAdd},
		"cmd.del":           {run: opCmdDel},
		"skills.list":       {run: opSkillsList},
		"skills.install":    {run: opSkillsInstall},
		"skills.sync":       {run: opSkillsSync},
		"health":            {run: opHealth},
		"log":               {run: opLog},
		"config":            {run: opConfig},
		"runs.list":         {run: opRunsList},
		"runs.show":         {run: opRunsShow},
	}
}

// execAdminOp runs an operation in this process.
func execAdminOp(ctx context.Context, lang, name string, args []string) (string, error) {
	op, ok := adminOps[name]
	if !ok {
		return "", errors.New(tr(lang, "cli.unknown_op", name))
	}
	return op.run(ctx, lang, args)
}

// resolveAdminOp maps command-line words to an operation: "reg approve 1"
// → reg.approve [1]; "users" → users.list; "health" → health.
func resolveAdminOp(words []string) (string, []string, bool) {
	if len(words) == 0 {
		return "", nil, false
	}
	if len(words) >= 2 {
		if _, ok := adminOps[words[0]+"."+words[1]]; ok {
			return words[0] + "." + words[1], words[2:], true
		}
	}
	if _, ok := adminOps[words[0]]; ok {
		return words[0], words[1:], true
	}
	if _, ok := adminOps[words[0]+".list"]; ok {
		return words[0] + ".list", words[1:], true
	}
	return "", nil, false
}

// adminCommandLine is the command an admin runs on the server.
func adminCommandLine() string {
	exe := "./tg-agent"
	if p, err := os.Executable(); err == nil {
		exe = p
	}
	if configFile != "" {
		return exe + " --config " + configFile + " admin"
	}
	return exe + " admin"
}

func usageErr(lang, key string) error { return errors.New(tr(lang, key)) }

func joinArgs(args []string) string { return strings.TrimSpace(strings.Join(args, " ")) }

func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "#"), 10, 64)
	return id, err == nil && id > 0
}

// saveSetting applies a config change the same way everywhere.
func saveSetting(fn func(*Config)) error {
	uiMu.Lock()
	defer uiMu.Unlock()
	if err := updateConfig(fn); err != nil {
		return errors.New(redactSecrets(err.Error()))
	}
	return nil
}

func registrationLabel(lang, mode string) string { return tr(lang, "reg.mode."+mode) }

func userCount(c Config) int {
	ids := map[int64]bool{c.OwnerID: true}
	for _, list := range [][]int64{c.Admins, c.Friends} {
		for _, id := range list {
			if !containsID(c.Banned, id) {
				ids[id] = true
			}
		}
	}
	return len(ids)
}

// ---------------- overview ----------------

func opStatus(ctx context.Context, lang string, args []string) (string, error) {
	c := configSnapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "%s · v%s\n", appName(lang), version)
	fmt.Fprintf(&b, "%s\n", tr(lang, "cli.summary", userCount(c), len(pendingRegistrations()), len(listNotices())))
	fmt.Fprintf(&b, "%s %s · %s %s · %s %s · %s %s",
		tr(lang, "cli.registration"), registrationLabel(lang, registrationMode(c)),
		tr(lang, "menu.model"), c.Model, tr(lang, "btn.mode"), modeLabel(lang, approvalMode(c)),
		tr(lang, "menu.effort"), effortLabel(lang, effectiveEffort(c)))
	return b.String(), nil
}

// ---------------- users ----------------

func userLabel(id int64) string {
	if name := userName(id); name != "?" && name != "" {
		return fmt.Sprintf("%d %s", id, name)
	}
	return strconv.FormatInt(id, 10)
}

func opUsersList(ctx context.Context, lang string, args []string) (string, error) {
	c := configSnapshot()
	row := func(label string, ids []int64) string {
		var parts []string
		for _, id := range ids {
			if id > 0 {
				parts = append(parts, userLabel(id))
			}
		}
		if len(parts) == 0 {
			parts = []string{tr(lang, "cli.none")}
		}
		return padRight(label, 8) + strings.Join(parts, "、")
	}
	return strings.Join([]string{
		row(tr(lang, "role.owner"), []int64{c.OwnerID}),
		row(tr(lang, "list.admin"), c.Admins),
		row(tr(lang, "list.friend"), c.Friends),
		row(tr(lang, "list.ban"), c.Banned),
	}, "\n"), nil
}

func normalizeRole(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "admin", "admins", "管理员":
		return "admin"
	case "friend", "friends", "user", "朋友", "用户":
		return "friend"
	case "ban", "banned", "封禁":
		return "ban"
	}
	return ""
}

func opUsersChange(add bool) func(context.Context, string, []string) (string, error) {
	return func(ctx context.Context, lang string, args []string) (string, error) {
		if len(args) != 2 {
			return "", usageErr(lang, "cli.usage_users")
		}
		role := normalizeRole(args[0])
		id, ok := parseID(args[1])
		if role == "" || !ok || id == configSnapshot().OwnerID {
			return "", usageErr(lang, "list.bad_id")
		}
		err := saveSetting(func(c *Config) {
			switch role {
			case "admin":
				c.Admins = toggleID(c.Admins, id, add)
			case "friend":
				c.Friends = toggleID(c.Friends, id, add)
			case "ban":
				c.Banned = toggleID(c.Banned, id, add)
			}
		})
		if err != nil {
			return "", err
		}
		if role == "admin" {
			registerChatCommands(id, add) // admins get the settings commands in their Telegram menu
		}
		return tr(lang, "list.updated", tr(lang, "list."+role), id), nil
	}
}

// ---------------- registrations ----------------

func opRegList(ctx context.Context, lang string, args []string) (string, error) {
	mode := registrationMode(configSnapshot())
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — %s\n", tr(lang, "cli.registration"), registrationLabel(lang, mode), tr(lang, "reg.mode_hint."+mode))
	pending := pendingRegistrations()
	if len(pending) == 0 {
		b.WriteString(tr(lang, "reg.none"))
		return b.String(), nil
	}
	b.WriteString(tr(lang, "cli.pending", len(pending)))
	for i, r := range pending {
		fmt.Fprintf(&b, "\n  %d. %s (%d) · %s", i+1, regDisplay(r), r.ID, time.Unix(r.Created, 0).Format("01-02 15:04"))
		if r.Note != "" {
			b.WriteString(" · " + oneLine(r.Note, 60))
		}
	}
	return b.String(), nil
}

// opRegIDs lists pending applicant IDs in order, for picking by number.
func opRegIDs(ctx context.Context, lang string, args []string) (string, error) {
	var ids []string
	for _, r := range pendingRegistrations() {
		ids = append(ids, strconv.FormatInt(r.ID, 10))
	}
	return strings.Join(ids, " "), nil
}

func opRegDecide(decision string) func(context.Context, string, []string) (string, error) {
	return func(ctx context.Context, lang string, args []string) (string, error) {
		if len(args) != 1 {
			return "", usageErr(lang, "cli.usage_reg")
		}
		id, ok := parseID(args[0])
		if !ok {
			return "", usageErr(lang, "cli.usage_reg")
		}
		text, changed := decideRegistration(lang, id, decision)
		if !changed {
			return "", errors.New(text)
		}
		return text, nil
	}
}

func normalizeRegMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "close", "closed", "关闭", "1":
		return "off"
	case "approval", "review", "需审核", "审核", "2":
		return "approval"
	case "open", "开放", "3":
		return "open"
	}
	return ""
}

func opRegMode(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 1 || normalizeRegMode(args[0]) == "" {
		return "", usageErr(lang, "cli.usage_reg_mode")
	}
	mode := normalizeRegMode(args[0])
	if err := saveSetting(func(c *Config) { c.Registration = mode }); err != nil {
		return "", err
	}
	return tr(lang, "cli.registration") + " " + registrationLabel(lang, mode) + " — " + tr(lang, "reg.mode_hint."+mode), nil
}

// ---------------- announcements ----------------

func opNoticeList(ctx context.Context, lang string, args []string) (string, error) {
	list := listNotices()
	if len(list) == 0 {
		return tr(lang, "notice.empty"), nil
	}
	var b strings.Builder
	for i, n := range list {
		if i >= 10 {
			fmt.Fprintf(&b, "… +%d\n", len(list)-i)
			break
		}
		fmt.Fprintf(&b, "#%d · %s", n.ID, time.Unix(n.Created, 0).Format("2006-01-02 15:04"))
		if n.Total > 0 {
			b.WriteString(" · " + tr(lang, "notice.delivered", n.Sent, n.Total))
		}
		fmt.Fprintf(&b, "\n   %s\n", oneLine(n.Text, 80))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func opNoticeRecipients(ctx context.Context, lang string, args []string) (string, error) {
	return strconv.Itoa(len(noticeRecipients())), nil
}

func opNoticePublish(ctx context.Context, lang string, args []string) (string, error) {
	text := joinArgs(args)
	if text == "" {
		return "", usageErr(lang, "cli.usage_notice")
	}
	n, err := publishNotice(ctx, text, configSnapshot().OwnerID)
	if err != nil {
		return "", err
	}
	return tr(lang, "notice.published", n.ID, n.Sent, n.Total), nil
}

func opNoticeDel(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 1 {
		return "", usageErr(lang, "cli.usage_notice")
	}
	id, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	if err != nil || !deleteNotice(id) {
		return "", usageErr(lang, "notice.not_found")
	}
	return tr(lang, "notice.deleted", id), nil
}

// ---------------- name, author and support ----------------

func opBrandShow(ctx context.Context, lang string, args []string) (string, error) {
	c := configSnapshot()
	value := func(v string) string {
		if strings.TrimSpace(v) == "" {
			return tr(lang, "config.unset")
		}
		return oneLine(v, 100)
	}
	name := value(c.BotName)
	if strings.TrimSpace(c.BotName) == "" {
		name = tr(lang, "app.name") + tr(lang, "brand.default")
	}
	return strings.Join([]string{
		padRight(tr(lang, "brand.name"), 10) + name,
		padRight(tr(lang, "brand.author"), 10) + value(c.Author),
		padRight(tr(lang, "brand.support"), 10) + value(c.SupportText),
		padRight(tr(lang, "brand.link"), 10) + value(c.SupportURL),
	}, "\n"), nil
}

func normalizeBrandField(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "name", "名称", "名字":
		return "name"
	case "author", "作者":
		return "author"
	case "text", "support", "说明", "支持", "支持说明":
		return "text"
	case "link", "url", "链接", "支持链接":
		return "link"
	}
	return ""
}

func opBrandSet(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) < 2 || normalizeBrandField(args[0]) == "" {
		return "", usageErr(lang, "cli.usage_brand")
	}
	field, value := normalizeBrandField(args[0]), joinArgs(args[1:])
	if value == "-" {
		value = ""
	}
	err := saveSetting(func(c *Config) {
		switch field {
		case "name":
			c.BotName = value
		case "author":
			c.Author = value
		case "text":
			c.SupportText = value
		case "link":
			c.SupportURL = value
		}
	})
	if err != nil {
		return "", err
	}
	return tr(lang, "toast.saved"), nil
}

// ---------------- providers ----------------

func opProviderList(ctx context.Context, lang string, args []string) (string, error) {
	c := configSnapshot()
	if len(c.Provider) == 0 {
		return tr(lang, "provider.none"), nil
	}
	var lines []string
	for _, p := range c.Provider {
		markText := "  "
		if p.Name == c.ProviderName {
			markText = "● "
		}
		lines = append(lines, fmt.Sprintf("%s%s  %s  %s", markText, p.Name, p.BaseURL, strings.Join(p.Models, ", ")))
	}
	return strings.Join(lines, "\n"), nil
}

func opProviderUse(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 1 {
		return "", usageErr(lang, "cli.usage_provider")
	}
	var found *Provider
	for _, p := range configSnapshot().Provider {
		if p.Name == args[0] {
			p := p
			found = &p
		}
	}
	if found == nil {
		return "", usageErr(lang, "provider.unknown")
	}
	var model string
	if err := saveSetting(func(c *Config) { selectProvider(c, *found); model = c.Model }); err != nil {
		return "", err
	}
	return tr(lang, "provider.switched", found.Name, model), nil
}

func opProviderAdd(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 4 {
		return "", usageErr(lang, "cli.usage_provider")
	}
	var models []string
	for _, m := range strings.Split(args[3], ",") {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return "", usageErr(lang, "cli.usage_provider")
	}
	p := Provider{Name: args[0], BaseURL: args[1], APIKey: args[2], Models: models, DefaultModel: models[0]}
	if err := saveSetting(func(c *Config) { setProviderIn(c, p) }); err != nil {
		return "", err
	}
	return tr(lang, "provider.added", p.Name, len(models), p.Name), nil
}

func opProviderDel(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 1 {
		return "", usageErr(lang, "cli.usage_provider")
	}
	c := configSnapshot()
	if args[0] == c.ProviderName {
		return "", usageErr(lang, "provider.del_current")
	}
	exists := false
	for _, p := range c.Provider {
		exists = exists || p.Name == args[0]
	}
	if !exists {
		return "", usageErr(lang, "provider.unknown")
	}
	err := saveSetting(func(c *Config) {
		var keep []Provider
		for _, p := range c.Provider {
			if p.Name != args[0] {
				keep = append(keep, p)
			}
		}
		c.Provider = keep
	})
	if err != nil {
		return "", err
	}
	return tr(lang, "provider.deleted", args[0]), nil
}

// ---------------- workspace ----------------

func opWorkspaceList(ctx context.Context, lang string, args []string) (string, error) {
	c := configSnapshot()
	var b strings.Builder
	b.WriteString(tr(lang, "ws.current") + " " + c.Workspace)
	if len(c.Workspaces) == 0 {
		b.WriteString("\n" + tr(lang, "ws.empty"))
	}
	for i, w := range c.Workspaces {
		markText := "  "
		if w == c.Workspace {
			markText = "● "
		}
		fmt.Fprintf(&b, "\n%s%d. %s", markText, i+1, w)
	}
	return b.String(), nil
}

func opWorkspaceUse(ctx context.Context, lang string, args []string) (string, error) {
	target := joinArgs(args)
	c := configSnapshot()
	if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(c.Workspaces) {
		target = c.Workspaces[idx-1]
	}
	if !filepath.IsAbs(target) {
		return "", usageErr(lang, "ws.absolute")
	}
	if out, err := runShell("test -d "+shellQuote(target)+" && echo OK", 15*time.Second); err != nil || !strings.Contains(out, "OK") {
		return "", errors.New(tr(lang, "ws.missing", target))
	}
	if err := saveSetting(func(c *Config) { c.Workspace = target }); err != nil {
		return "", err
	}
	return tr(lang, "cli.ws_switched", target), nil
}

func opWorkspaceAdd(ctx context.Context, lang string, args []string) (string, error) {
	path := joinArgs(args)
	if !filepath.IsAbs(path) {
		return "", usageErr(lang, "ws.absolute")
	}
	if containsString(configSnapshot().Workspaces, path) {
		return "", usageErr(lang, "ws.exists")
	}
	if err := saveSetting(func(c *Config) { c.Workspaces = append(c.Workspaces, path) }); err != nil {
		return "", err
	}
	return tr(lang, "ws.added", path), nil
}

func opWorkspaceDel(ctx context.Context, lang string, args []string) (string, error) {
	idx, err := strconv.Atoi(joinArgs(args))
	c := configSnapshot()
	if err != nil || idx < 1 || idx > len(c.Workspaces) {
		return "", usageErr(lang, "ws.bad_index")
	}
	removed := c.Workspaces[idx-1]
	err = saveSetting(func(c *Config) {
		var keep []string
		for _, w := range c.Workspaces {
			if w != removed {
				keep = append(keep, w)
			}
		}
		c.Workspaces = keep
	})
	if err != nil {
		return "", err
	}
	return tr(lang, "ws.removed", removed), nil
}

// ---------------- system prompt and persona ----------------

func normalizePromptKind(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "system", "系统", "系统提示词", "提示词":
		return "system"
	case "persona", "人设":
		return "persona"
	}
	return ""
}

func opPromptShow(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 1 || normalizePromptKind(args[0]) == "" {
		return "", usageErr(lang, "cli.usage_prompt")
	}
	c := configSnapshot()
	cur := c.SystemPrompt
	if normalizePromptKind(args[0]) == "persona" {
		cur = c.Persona
	}
	if strings.TrimSpace(cur) == "" {
		return tr(lang, "prompt.unset"), nil
	}
	return cur, nil
}

func opPromptSet(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) < 2 || normalizePromptKind(args[0]) == "" {
		return "", usageErr(lang, "cli.usage_prompt")
	}
	kind, text := normalizePromptKind(args[0]), joinArgs(args[1:])
	if text == "-" {
		text = ""
	}
	err := saveSetting(func(c *Config) {
		if kind == "persona" {
			c.Persona = text
		} else {
			c.SystemPrompt = text
		}
	})
	if err != nil {
		return "", err
	}
	return tr(lang, kind+".updated"), nil
}

// ---------------- schedules ----------------

func opCronList(ctx context.Context, lang string, args []string) (string, error) {
	if s := strings.TrimRight(listCronText(lang, 0), "\n"); s != "" {
		return s, nil
	}
	return tr(lang, "cron.empty"), nil
}

// opCronAdd: rule words then the task, e.g. "daily 09:00 汇总服务器状态".
// Jobs created in the terminal run as the owner in the owner's chat.
func opCronAdd(ctx context.Context, lang string, args []string) (string, error) {
	c := configSnapshot()
	spec, used, err := parseScheduleFields(args, time.Now(), scheduleLocation(c))
	if err != nil {
		return "", errors.New(err.Error() + "\n" + tr(lang, "cron.usage"))
	}
	prompt := joinArgs(args[used:])
	job, err := addCronJob(c.OwnerID, c.OwnerID, spec, prompt, "terminal")
	if err != nil {
		return "", err
	}
	return tr(lang, "cron.created", job.ID, spec.describe(lang), formatRunTime(job.NextRun), oneLine(prompt, 60)), nil
}

func opCronChange(action string) func(context.Context, string, []string) (string, error) {
	return func(ctx context.Context, lang string, args []string) (string, error) {
		if len(args) != 1 {
			return "", usageErr(lang, "cron.usage")
		}
		id, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil {
			return "", usageErr(lang, "cron.usage")
		}
		switch action {
		case "del":
			err = deleteCronJob(id, 0)
		case "pause":
			err = setCronEnabled(id, 0, false)
		case "resume":
			err = setCronEnabled(id, 0, true)
		case "run":
			err = runCronNow(id)
		}
		if err != nil {
			return "", err
		}
		return tr(lang, "cron.updated", id), nil
	}
}

// ---------------- shortcuts ----------------

func opCmdList(ctx context.Context, lang string, args []string) (string, error) {
	return strings.TrimRight(quickList(lang), "\n"), nil
}

func opCmdAdd(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) < 2 || strings.ContainsAny(args[0], " \t\n") {
		return "", usageErr(lang, "cli.usage_cmd")
	}
	name, prompt := args[0], joinArgs(args[1:])
	err := saveSetting(func(c *Config) {
		var keep []QuickCmd
		for _, q := range c.QuickCommands {
			if q.Name != name {
				keep = append(keep, q)
			}
		}
		c.QuickCommands = append(keep, QuickCmd{Name: name, Prompt: prompt})
	})
	if err != nil {
		return "", err
	}
	return tr(lang, "quick.added", name, name), nil
}

func opCmdDel(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 1 {
		return "", usageErr(lang, "cli.usage_cmd")
	}
	if _, ok := findQuick(args[0]); !ok {
		return "", usageErr(lang, "quick.unknown")
	}
	err := saveSetting(func(c *Config) {
		var keep []QuickCmd
		for _, q := range c.QuickCommands {
			if q.Name != args[0] {
				keep = append(keep, q)
			}
		}
		c.QuickCommands = keep
	})
	if err != nil {
		return "", err
	}
	return tr(lang, "quick.deleted", args[0]), nil
}

// ---------------- skills ----------------

func opSkillsList(ctx context.Context, lang string, args []string) (string, error) {
	return strings.TrimSpace(stripTags(skillsPanel(lang))), nil
}

func opSkillsInstall(ctx context.Context, lang string, args []string) (string, error) {
	if len(args) != 1 {
		return "", usageErr(lang, "skills.usage")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return installSkill(ctx, configSnapshot(), args[0])
}

func opSkillsSync(ctx context.Context, lang string, args []string) (string, error) {
	c := configSnapshot()
	if len(c.SkillSources) == 0 {
		return tr(lang, "skills.no_sources"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var lines []string
	for _, source := range c.SkillSources {
		text, err := installSkill(ctx, c, source.Name)
		if err != nil {
			text = source.Name + ": " + err.Error()
		}
		lines = append(lines, strings.TrimSpace(text))
	}
	return strings.Join(lines, "\n"), nil
}

// ---------------- diagnostics ----------------

func opHealth(ctx context.Context, lang string, args []string) (string, error) {
	return strings.TrimSpace(stripTags(healthCheck(lang))), nil
}

func opLog(ctx context.Context, lang string, args []string) (string, error) {
	n := 40
	if len(args) == 1 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			n = min(v, 2000)
		}
	}
	return tailLog(n), nil
}

func opConfig(ctx context.Context, lang string, args []string) (string, error) {
	return strings.TrimSpace(stripTags(configOverview(lang))), nil
}

func opRunsList(ctx context.Context, lang string, args []string) (string, error) {
	items := recentRuns(20)
	if len(items) == 0 {
		return tr(lang, "runs.empty"), nil
	}
	var b strings.Builder
	for i, it := range items {
		fmt.Fprintf(&b, "%2d. %s · %.1f KB · %s\n", i+1, it.t.Format("01-02 15:04"), float64(it.size)/1024, it.name)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func opRunsShow(ctx context.Context, lang string, args []string) (string, error) {
	items := recentRuns(20)
	idx, err := strconv.Atoi(joinArgs(args))
	if err != nil || idx < 1 || idx > len(items) {
		return "", usageErr(lang, "cli.usage_runs")
	}
	data, err := os.ReadFile(filepath.Join(baseDir, "runs", items[idx-1].name))
	if err != nil {
		return "", err
	}
	return short(string(data), 60000), nil
}

// padRight pads to a display width so CJK labels line up in the terminal.
func padRight(s string, width int) string {
	if w := displayWidth(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s + " "
}
