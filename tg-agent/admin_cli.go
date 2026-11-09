package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The server terminal is where the bot is managed: `tg-agent admin` opens a
// numbered menu, `tg-agent admin <command>` runs one command. While the bot
// runs, every operation is sent to it over a private Unix socket in the data
// directory (mode 0600), so changes apply at once and never race with the
// bot's own writes. When the bot is stopped, the terminal edits the files
// itself and the bot picks them up on its next start.

const adminSocketName = "admin.sock"

func adminSocketPath() string { return filepath.Join(baseDir, adminSocketName) }

type adminRequest struct {
	Op   string   `json:"op"`
	Args []string `json:"args"`
	Lang string   `json:"lang"`
}

type adminResponse struct {
	OK    bool   `json:"ok"`
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}

// startAdminSocket serves terminal requests inside the running bot. It
// returns a function that closes the socket.
func startAdminSocket(ctx context.Context) func() {
	path := adminSocketPath()
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		logf("终端管理通道 %s 已被另一个实例占用，本实例不提供", path)
		return func() {}
	}
	_ = os.Remove(path) // stale socket from a crash
	ln, err := net.Listen("unix", path)
	if err != nil {
		logf("终端管理通道启动失败（终端将直接修改文件）: %v", err)
		return func() {}
	}
	_ = os.Chmod(path, 0600)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveAdminConn(ctx, conn)
		}
	}()
	return func() {
		ln.Close()
		_ = os.Remove(path)
	}
}

func serveAdminConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Minute))
	var req adminRequest
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&req); err != nil {
		return
	}
	lang := langZH
	if req.Lang == langEN {
		lang = langEN
	}
	logf("终端管理：%s", req.Op) // arguments may contain keys; only the operation is logged
	text, err := execAdminOp(ctx, lang, req.Op, req.Args)
	resp := adminResponse{OK: err == nil, Text: text}
	if err != nil {
		resp.Error = redactSecrets(err.Error())
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// botRunning reports whether a live bot answers on the admin socket.
func botRunning() bool {
	conn, err := net.DialTimeout("unix", adminSocketPath(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// callAdmin runs an operation in the live bot when it is reachable,
// otherwise in this process on the files.
func callAdmin(ctx context.Context, lang, name string, args []string) (string, error) {
	if conn, err := net.DialTimeout("unix", adminSocketPath(), 2*time.Second); err == nil {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(15 * time.Minute))
		if err := json.NewEncoder(conn).Encode(adminRequest{Op: name, Args: args, Lang: lang}); err != nil {
			return "", err
		}
		var resp adminResponse
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			return "", fmt.Errorf("%s: %v", tr(lang, "cli.socket_error"), err)
		}
		if !resp.OK {
			return resp.Text, errors.New(resp.Error)
		}
		return resp.Text, nil
	}
	if op, ok := adminOps[name]; ok && op.needsBot {
		return "", errors.New(tr(lang, "cli.need_bot"))
	}
	// The files may have changed since this terminal session started.
	if err := reloadConfigIfChanged(); err != nil {
		return "", err
	}
	if strings.HasPrefix(name, "cron.") {
		loadCron()
	}
	return execAdminOp(ctx, lang, name, args)
}

func isAdminSubcommand(s string) bool {
	switch s {
	case "admin", "console", "manage":
		return true
	}
	return false
}

// cliLang follows config language; Chinese unless English is chosen.
func cliLang() string {
	if configSnapshot().Language == langEN {
		return langEN
	}
	return langZH
}

// runAdminCLI is `tg-agent admin [command...]`.
func runAdminCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	previousLog, previousCtx := logToStdout.Load(), telegramContext
	logToStdout.Store(false)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	telegramContext = ctx
	defer func() {
		cancel()
		telegramContext = previousCtx
		logToStdout.Store(previousLog)
	}()
	lang := cliLang()
	call := func(op string, a ...string) (string, error) { return callAdmin(ctx, lang, op, a) }
	if len(args) == 0 {
		newAdminUI(stdin, stdout, lang, call, botRunning).run()
		return 0
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, tr(lang, "cli.usage", "tg-agent"))
		return 0
	}
	name, rest, ok := resolveAdminOp(args)
	if !ok {
		fmt.Fprintln(stderr, tr(lang, "cli.unknown_op", strings.Join(args, " ")))
		return 2
	}
	// "notice publish -" reads the announcement from standard input.
	if name == "notice.publish" && len(rest) == 1 && rest[0] == "-" {
		data, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
		rest = []string{strings.TrimSpace(string(data))}
	}
	text, err := call(name, rest...)
	if text != "" {
		fmt.Fprintln(stdout, text)
	}
	if err != nil {
		fmt.Fprintln(stderr, "✗ "+err.Error())
		return 1
	}
	return 0
}

// ---------------- interactive menu ----------------

type adminUI struct {
	in      *bufio.Scanner
	out     io.Writer
	lang    string
	call    func(op string, args ...string) (string, error)
	running func() bool
	color   bool
	eof     bool
}

func newAdminUI(in io.Reader, out io.Writer, lang string, call func(string, ...string) (string, error), running func() bool) *adminUI {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	color := false
	if f, ok := out.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
			color = true
		}
	}
	return &adminUI{in: scanner, out: out, lang: lang, call: call, running: running, color: color}
}

func (u *adminUI) t(key string, args ...interface{}) string { return tr(u.lang, key, args...) }

func (u *adminUI) printf(format string, a ...interface{}) { fmt.Fprintf(u.out, format, a...) }

func (u *adminUI) bold(s string) string {
	if u.color {
		return "\033[1m" + s + "\033[0m"
	}
	return s
}

func (u *adminUI) dim(s string) string {
	if u.color {
		return "\033[2m" + s + "\033[0m"
	}
	return s
}

// ask reads one line; false at end of input.
func (u *adminUI) ask(label string) (string, bool) {
	if u.eof {
		return "", false
	}
	u.printf("%s > ", label)
	if !u.in.Scan() {
		u.eof = true
		u.printf("\n")
		return "", false
	}
	return strings.TrimSpace(u.in.Text()), true
}

// askLines reads text until a line with a single "." (or end of input).
func (u *adminUI) askLines(label string) (string, bool) {
	if u.eof {
		return "", false
	}
	u.printf("%s\n", label)
	var lines []string
	for u.in.Scan() {
		line := u.in.Text()
		if strings.TrimSpace(line) == "." {
			return strings.TrimSpace(strings.Join(lines, "\n")), true
		}
		lines = append(lines, line)
	}
	u.eof = true
	return strings.TrimSpace(strings.Join(lines, "\n")), len(lines) > 0
}

// show runs an operation and prints its result.
func (u *adminUI) show(op string, args ...string) bool {
	text, err := u.call(op, args...)
	if text != "" {
		u.printf("%s\n", text)
	}
	if err != nil {
		u.printf("✗ %s\n", err.Error())
		return false
	}
	return true
}

func (u *adminUI) section(titleKey string) {
	u.printf("\n%s\n", u.bold("▍"+u.t(titleKey)))
}

// choose prints numbered entries in two columns plus 0 for back/exit and
// returns the number picked; 0 also on end of input.
func (u *adminUI) choose(keys []string, zeroKey string) int {
	width := 0
	labels := make([]string, len(keys))
	for i, k := range keys {
		labels[i] = fmt.Sprintf("%2d  %s", i+1, u.t(k))
		width = max(width, displayWidth(labels[i]))
	}
	u.printf("\n")
	for i := 0; i < len(labels); i += 2 {
		line := "  " + labels[i]
		if i+1 < len(labels) {
			line = "  " + padRight(labels[i], width+4) + labels[i+1]
		}
		u.printf("%s\n", line)
	}
	u.printf("  %s\n", u.dim(fmt.Sprintf("%2d  %s", 0, u.t(zeroKey))))
	for {
		answer, ok := u.ask(u.t("cli.choose"))
		if !ok {
			return 0
		}
		if n, err := strconv.Atoi(answer); err == nil && n >= 0 && n <= len(keys) {
			return n
		}
		u.printf("%s\n", u.t("cli.invalid"))
	}
}

var adminMenuKeys = []string{
	"btn.users", "btn.registrations", "btn.announce", "btn.brand", "btn.provider", "btn.workspace",
	"btn.system", "btn.persona", "btn.cron", "btn.quick", "btn.skills", "btn.health", "btn.log", "btn.config", "cli.runs",
}

func (u *adminUI) header() {
	line := strings.Repeat("━", 36)
	u.printf("\n%s\n", line)
	status, _ := u.call("status")
	first, rest, _ := strings.Cut(status, "\n")
	u.printf(" %s\n", u.bold(first+" · "+u.t("cli.title")))
	if u.running() {
		u.printf(" ● %s\n", u.t("cli.running"))
	} else {
		u.printf(" ○ %s\n", u.t("cli.offline"))
	}
	u.printf("%s\n", line)
	for _, l := range strings.Split(rest, "\n") {
		if l != "" {
			u.printf(" %s\n", l)
		}
	}
}

func (u *adminUI) run() {
	handlers := []func(){
		u.users, u.registrations, u.notices, u.brand, u.providers, u.workspace,
		func() { u.prompt("system") }, func() { u.prompt("persona") }, u.cron, u.quick, u.skills,
		func() { u.section("btn.health"); u.printf("%s\n", u.t("health.running")); u.show("health") },
		u.logs,
		func() { u.section("btn.config"); u.show("config") },
		u.runs,
	}
	for {
		u.header()
		choice := u.choose(adminMenuKeys, "cli.exit")
		if choice == 0 {
			u.printf("%s\n", u.t("cli.bye"))
			return
		}
		handlers[choice-1]()
	}
}

func (u *adminUI) users() {
	actions := []struct{ op, role string }{{"users.add", "admin"}, {"users.del", "admin"}, {"users.add", "friend"}, {"users.del", "friend"}, {"users.add", "ban"}, {"users.del", "ban"}}
	for !u.eof {
		u.section("btn.users")
		u.show("users.list")
		choice := u.choose([]string{"cli.act.add_admin", "cli.act.del_admin", "cli.act.add_friend", "cli.act.del_friend", "cli.act.ban", "cli.act.unban"}, "cli.back")
		if choice == 0 {
			return
		}
		if id, ok := u.ask(u.t("cli.input_id")); ok && id != "" {
			u.show(actions[choice-1].op, actions[choice-1].role, id)
		}
	}
}

// applicant turns "1" (position in the pending list) or an ID into an ID.
func (u *adminUI) applicant(v string) string {
	ids, _ := u.call("reg.ids")
	list := strings.Fields(ids)
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(list) {
		return list[n-1]
	}
	return v
}

func (u *adminUI) registrations() {
	for !u.eof {
		u.section("btn.registrations")
		u.show("reg.list")
		choice := u.choose([]string{"cli.act.approve", "cli.act.reject", "cli.act.reg_ban", "cli.act.reg_mode"}, "cli.back")
		switch choice {
		case 0:
			return
		case 1, 2, 3:
			v, ok := u.ask(u.t("cli.input_index_or_id"))
			if !ok || v == "" {
				continue
			}
			u.show([]string{"reg.approve", "reg.reject", "reg.ban"}[choice-1], u.applicant(v))
		case 4:
			if m, ok := u.ask(u.t("cli.input_mode")); ok && m != "" {
				u.show("reg.mode", m)
			}
		}
	}
}

func (u *adminUI) notices() {
	for !u.eof {
		u.section("btn.announce")
		u.show("notice.list")
		choice := u.choose([]string{"cli.act.publish", "cli.act.notice_del"}, "cli.back")
		switch choice {
		case 0:
			return
		case 1:
			text, ok := u.askLines(u.t("cli.multiline"))
			if !ok || text == "" {
				u.printf("%s\n", u.t("cli.cancelled"))
				continue
			}
			count, _ := u.call("notice.recipients")
			u.printf("\n%s\n%s\n%s\n", u.dim("┈┈┈┈ "+u.t("cli.preview")+" ┈┈┈┈"), text, u.dim(strings.Repeat("┈", 16)))
			if answer, _ := u.ask(u.t("cli.confirm_publish", count)); strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") || answer == "是" {
				u.printf("%s\n", u.t("cli.sending"))
				u.show("notice.publish", text)
			} else {
				u.printf("%s\n", u.t("cli.cancelled"))
			}
		case 2:
			if id, ok := u.ask("#"); ok && id != "" {
				u.show("notice.del", id)
			}
		}
	}
}

func (u *adminUI) brand() {
	fields := []string{"name", "author", "text", "link"}
	for !u.eof {
		u.section("btn.brand")
		u.show("brand.list")
		choice := u.choose([]string{"brand.edit_name", "brand.edit_author", "brand.edit_support", "brand.edit_link"}, "cli.back")
		if choice == 0 {
			return
		}
		var value string
		var ok bool
		if fields[choice-1] == "text" {
			value, ok = u.askLines(u.t("cli.multiline_clear"))
		} else {
			value, ok = u.ask(u.t("cli.input_value"))
		}
		if ok && value != "" {
			u.show("brand.set", fields[choice-1], value)
		}
	}
}

func (u *adminUI) providers() {
	for !u.eof {
		u.section("btn.provider")
		u.show("provider.list")
		switch u.choose([]string{"cli.act.use", "cli.act.add", "cli.act.del"}, "cli.back") {
		case 0:
			return
		case 1:
			if name, ok := u.ask(u.t("cli.input_name")); ok && name != "" {
				u.show("provider.use", name)
			}
		case 2:
			name, ok1 := u.ask(u.t("cli.input_name"))
			url, ok2 := u.ask(u.t("cli.input_url"))
			key, ok3 := u.ask(u.t("cli.input_key"))
			models, ok4 := u.ask(u.t("cli.input_models"))
			if ok1 && ok2 && ok3 && ok4 && name != "" {
				u.show("provider.add", name, url, key, models)
			}
		case 3:
			if name, ok := u.ask(u.t("cli.input_name")); ok && name != "" {
				u.show("provider.del", name)
			}
		}
	}
}

func (u *adminUI) workspace() {
	for !u.eof {
		u.section("btn.workspace")
		u.show("workspace.list")
		choice := u.choose([]string{"cli.act.ws_use", "cli.act.ws_add", "cli.act.ws_del"}, "cli.back")
		if choice == 0 {
			return
		}
		label := u.t("cli.input_path")
		if choice == 3 {
			label = u.t("cli.input_index")
		}
		if v, ok := u.ask(label); ok && v != "" {
			u.show([]string{"workspace.use", "workspace.add", "workspace.del"}[choice-1], v)
		}
	}
}

func (u *adminUI) prompt(kind string) {
	title := map[string]string{"system": "btn.system", "persona": "btn.persona"}[kind]
	for !u.eof {
		u.section(title)
		u.show("prompt.show", kind)
		switch u.choose([]string{"cli.act.edit", "cli.act.clear"}, "cli.back") {
		case 0:
			return
		case 1:
			if text, ok := u.askLines(u.t("cli.multiline")); ok && text != "" {
				u.show("prompt.set", kind, text)
			}
		case 2:
			u.show("prompt.set", kind, "-")
		}
	}
}

func (u *adminUI) cron() {
	for !u.eof {
		u.section("btn.cron")
		u.show("cron.list")
		choice := u.choose([]string{"cli.act.cron_add", "cli.act.cron_del", "cli.act.cron_pause", "cli.act.cron_resume", "cli.act.cron_run"}, "cli.back")
		switch choice {
		case 0:
			return
		case 1:
			rule, ok1 := u.ask(u.t("cli.input_rule"))
			task, ok2 := u.ask(u.t("cli.input_task"))
			if ok1 && ok2 && rule != "" && task != "" {
				u.show("cron.add", append(strings.Fields(rule), task)...)
			}
		default:
			if id, ok := u.ask("#"); ok && id != "" {
				u.show([]string{"cron.del", "cron.pause", "cron.resume", "cron.run"}[choice-2], id)
			}
		}
	}
}

func (u *adminUI) quick() {
	for !u.eof {
		u.section("btn.quick")
		u.show("cmd.list")
		switch u.choose([]string{"cli.act.cmd_add", "cli.act.cmd_del"}, "cli.back") {
		case 0:
			return
		case 1:
			name, ok1 := u.ask(u.t("cli.input_name"))
			prompt, ok2 := u.ask(u.t("cli.input_task"))
			if ok1 && ok2 && name != "" && prompt != "" {
				u.show("cmd.add", name, prompt)
			}
		case 2:
			if name, ok := u.ask(u.t("cli.input_name")); ok && name != "" {
				u.show("cmd.del", name)
			}
		}
	}
}

func (u *adminUI) skills() {
	for !u.eof {
		u.section("btn.skills")
		u.show("skills.list")
		switch u.choose([]string{"cli.act.install", "cli.act.sync"}, "cli.back") {
		case 0:
			return
		case 1:
			if name, ok := u.ask(u.t("cli.input_name")); ok && name != "" {
				u.show("skills.install", name)
			}
		case 2:
			u.show("skills.sync")
		}
	}
}

func (u *adminUI) logs() {
	u.section("btn.log")
	n, ok := u.ask(u.t("cli.input_lines"))
	if !ok {
		return
	}
	if n == "" {
		n = "40"
	}
	u.show("log", n)
}

func (u *adminUI) runs() {
	for !u.eof {
		u.section("cli.runs")
		u.show("runs.list")
		v, ok := u.ask(u.t("cli.input_run"))
		if !ok || v == "" || v == "0" {
			return
		}
		u.show("runs.show", v)
	}
}
