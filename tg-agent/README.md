# 大肥鱼 Telegram Agent 2.4

> 2.4.0：管理功能从 Telegram 移到 **服务器终端**：在服务器上运行 `tg-agent admin` 打开中文编号菜单（用户、注册审核、公告、名称与作者、服务商、工作区、提示词、定时任务、快捷指令、技能、自检、日志、配置、执行记录），也可直接执行单条命令。Telegram 里只保留用户菜单与管理员的“模型 / 执行模式 / 推理”设置。
>
> 2.3.0：菜单精简——设置只保留模型、执行模式、推理；人设、系统提示词、高级、工作区、定时任务、快捷指令、技能等移入仅管理员可见的管理后台（`/admin`）。新增用户注册与审核、公告发布（后台预览后推送）、支持作者页与可自定义名称。回复下方的统计行默认不再显示余额。
>
> 2.2.0：回复下方不再附带“新对话 / 重试 / 菜单 / 执行记录”按钮；模型接口报错在后台静默重试；过程面板重新设计（计划、思考、步骤、耗时），回复末尾显示耗时、调用、工具、命令、Token、余额统计，每一项都能用 `/show` 单独开关。全程禁用 emoji，只用 ✓ ✗ ▸ ● ▍ 等文本符号与粗体排版。
>
> 2.1.1：纯文本界面（无表情），对话时无停止按钮（用 /stop 停止），普通用户只见使用、模型与推理选择，其余设置在管理员菜单与 config.json。

> English: a bilingual (中文/English) Telegram AI agent in pure Go. Send a task; it plans, calls tools with a stated intent for each step, shows live progress, and replies in formatted Markdown. Switch language with `/lang en`.

一个用 Go 标准库实现的 Telegram AI 工作助手。直接发送任务，即可搜索资料、读写代码、执行命令、维护计划并调用子智能体。主菜单保留常用入口，模型、执行策略、工作区、自动化与权限管理放在二级设置中。

本版参考并实际阅读了 [Codex CLI 官方源码](docs/CODEX_REFERENCE.md)，使用原生 Go 实现审批、取消、上下文压缩和技能机制。这里的执行权限是应用层控制；主机命令使用运行服务的系统用户权限。

## 快速开始

需要 Go 1.24+、bash 与 Linux 常用命令工具（包括 base64、mktemp、GNU chmod）；Git、SSH、ripgrep 按所需工具安装。构建无需下载第三方 Go 依赖。

```bash
go build -trimpath -o tg-agent .
cp config.example.json config.json
mkdir -p workspace
chmod 600 config.json
```

编辑 `config.json`：填入自己的 Telegram **用户 ID**（`owner_id`）、模型服务的 `base_url` 与真实模型名称。示例的 `YOUR_MODEL_NAME` 必须替换。设置凭据，然后验证与启动：

```bash
export TG_BOT_TOKEN='你的 BotFather token'
export LLM_API_KEY='你的模型 API key'
./tg-agent --config ./config.json --check
./tg-agent --config ./config.json
```

`--check` 只验证配置，不连接 Telegram 或模型。也支持 `--version`、`--help`。配置中的密钥支持 `env:变量名` 和 `${变量名}`；保存设置会保留环境变量引用。`TG_BOT_TOKEN` 可覆盖配置中的 token。不要提交实际配置或密钥。

此版只接受 **私聊**，用户 ID 必须在主人、管理员或朋友名单内。群聊会话不会启动任务。每个私聊同时运行一个任务；不同私聊可独立运行。任务启动时冻结模型、目录、执行模式与限制，更改设置从下一个任务生效。

## 菜单与操作

直接发送文字、图片、文件、视频或语音。语音转写需配置 `asr`；远端 SSH 工作区使用附件时，需要先把机器人主机的上传文件传到远端。

**用户看到的（前台）**

| 入口 | 用途 |
| --- | --- |
| `/menu` | 主菜单：新对话、显示、帮助、公告、支持作者（管理员另有用量、模型、推理、设置） |
| `/new` | 新对话，不会自动终止运行中的任务 |
| `/stop` | 取消模型请求、正在执行的本地命令及等待的审批 |
| `/continue` | 重新执行上一个任务；可能重复之前产生的操作 |
| `/show` | 显示设置：思考、步骤、统计行各项开关与预设 |
| `/notice` | 查看公告 |
| `/about` | 支持作者：作者、支持说明与链接 |
| `/memory` `/lang` | 长期备忘、切换语言 |

管理员在 Telegram 里只有“设置”（`/settings`：**模型、执行模式、推理**）和“用量”。Telegram 命令菜单对普通用户只列出 menu、new、stop、show、notice、about、help；管理员的私聊里额外列出 settings、usage。

## 服务器终端管理

其余管理都在服务器的 Linux 终端完成，不出现在 Telegram 的菜单或命令里。登录服务器后运行：

```bash
# systemd 部署（以服务用户运行，文件归属不变；用 sudo 以 root 运行也可以）
sudo -u tgagent /opt/tg-agent/tg-agent --config /opt/tg-agent/config.json admin
# 容器部署
docker compose exec agent tg-agent --config /app/config.json admin
```

会打开一个编号菜单：

```text
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
 AI Agent 云端 · v2.4.0 · 终端管理
 ● 机器人运行中，修改立即生效
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
 用户 4 人 · 待审核 1 · 公告 1 条
 注册方式 需审核 · 模型 deepseek-chat · 执行模式 人工确认 · 推理 默认（不发送）

   1  用户管理       2  注册审核
   3  发布公告       4  名称与作者
   5  服务商         6  工作区
   7  系统提示词     8  人设
   9  定时任务      10  快捷指令
  11  技能          12  自检
  13  运行日志      14  配置总览
  15  执行记录
   0  退出
请选择 >
```

| 菜单 | 内容 |
| --- | --- |
| 用户管理 | 添加 / 移除管理员、用户（朋友），封禁 / 解封 |
| 注册审核 | 查看待审核申请，按序号或 ID 通过、拒绝、封禁；切换注册方式 关闭 / 需审核 / 开放 |
| 发布公告 | 输入内容（多行，单独一行 `.` 结束）→ 预览 → 确认后推送给全部用户并显示送达人数；删除旧公告 |
| 名称与作者 | 机器人名称、作者、支持说明（Markdown）、支持链接，输入 `-` 清空 |
| 服务商 / 工作区 | 切换、添加、删除服务商；切换、收藏、移除工作区 |
| 系统提示词 / 人设 | 查看、修改（多行）、清空 |
| 定时任务 / 快捷指令 | 添加（如 `daily 09:00` + 任务内容）、删除、暂停、恢复、立即运行；添加、删除快捷指令 |
| 技能 / 自检 / 运行日志 / 配置总览 / 执行记录 | 技能列表、安装与同步；模型与执行后端自检；查看日志、配置与最近的执行记录 |

每一项也能直接作为命令执行，方便脚本或快速操作（`tg-agent admin help` 查看全部）：

```bash
tg-agent admin status
tg-agent admin reg approve 5005
tg-agent admin users add admin 123456789
tg-agent admin brand set name AI Agent 云端
tg-agent admin notice publish 今晚 22:00 维护        # 直接发布，不预览
echo "多行公告" | tg-agent admin notice publish -     # 从标准输入读取
tg-agent admin cron add daily 09:00 汇总服务器状态
tg-agent admin log 100
```

**如何生效**：机器人运行时会在数据目录创建仅服务用户可读写的 `admin.sock`，终端的每个操作都发给运行中的机器人执行，立即生效，也不会和机器人自己的写入冲突。机器人未运行时，终端直接修改 `config.json` 等文件，下次启动生效；以 root 运行时新文件的属主会沿用原文件，服务用户仍可读取。在机器人运行时手动编辑了 `config.json`，下一次修改设置前会先重新读取它；文件有语法错误时不会被覆盖。

技能由 AI 自动匹配与加载（`list_skills` / `load_skill`），只能在终端里查看、安装与同步。

**用户注册**：`registration` 为 `approval`（默认）时，陌生人发送任意消息会看到注册页，点“申请注册”或发送 `/register 备注` 后，所有管理员会在 Telegram 收到一条提醒（不带按钮，附带审核用的终端命令），在服务器终端的“注册审核”里处理；通过后对方成为朋友（受 `friend_tools` 限制）并收到欢迎消息，被拒绝后 24 小时内不能再申请。`open` 为注册即可用（会消耗你的模型额度，谨慎开启），`off` 恢复为只能由管理员按 ID 添加。

**公告**：在服务器终端发布，确认后逐个推送给主人、管理员和朋友（跳过封禁用户），完成后显示送达人数；用户随时可在菜单“公告”或 `/notice` 查看最近的公告。公告保存在 `announcements.json`，注册申请保存在 `registrations.json`。

## 显示与统计

每个任务有一条实时更新的**过程面板**，完成后收起；答复单独发送，末尾附一行统计。两者都不带按钮。

```text
◐ 执行中 · 0:42
调用 3 · 工具 4 · 命令 1 · Token 11.3k

▍计划  1/3
✓ 定位超时配置
▸ 修改默认值
○ 运行测试验证

▍思考
│ 用户要把命令超时改为 300 秒，先找到默认值……（可展开）
默认值在 config.go，先改这里再跑测试。

▍执行  4 步
✓ 01 搜索内容 CommandTimeoutSeconds 1.2s
   ╰ 定位超时字段
◌ 04 执行命令 go test ./...
   ╰ 运行测试验证改动
```

答复末尾的统计行：

```text
耗时 1m23s · 调用 5 · 工具 6 · 命令 2 · Token 17.3k
```

- **思考**：模型随工具调用给出的说明，以及服务商返回的 `reasoning_content`（如 deepseek-reasoner），折叠在可展开引用中。
- **步骤**：每次工具调用的序号、名称、参数摘要、用途和耗时；完成后收起为“工具 × 次数”与可展开的步骤清单。
- **统计**：耗时、模型调用次数、工具调用次数、shell 命令数、Token、余额（默认关闭；仅管理员可开启，服务商支持 `/user/balance` 时显示，查询失败自动隐藏）、模型名。为 0 的项不显示。

所有项都能单独开关，从下一条回复起生效，按用户分别保存在 `prefs.json`：

```text
/show                    打开开关面板（也可从 /menu 的“显示”进入）
/show token off          隐藏 Token
/show 余额 关            中文项目名与开/关同样可用
/show 耗时 on 调用 off   一次改多项
/show 思考               不写开关即切换
/show 简洁               预设：只显示耗时与 Token
/show 纯净               预设：只保留答复正文，完成后自动删除过程面板
/show 全部 | 默认        全部显示 | 恢复默认
```

| 项目 | 键 | 默认 |
| --- | --- | --- |
| 思考过程 | `think` | 开 |
| 工具步骤 | `trace` | 开 |
| 输出预览 | `output` | 关（同 `/steps`、`show_steps`） |
| 保留面板 | `keep` | 开；关闭后答复发出即删除过程面板 |
| 耗时 / 调用 / 工具 / 命令 / Token | `time` `calls` `tools` `cmds` `tokens` | 开 |
| 余额 | `balance` | 关，仅管理员可开启 |
| 模型名 | `model` | 关 |

管理员可在 `config.json` 的 `display` 中修改全员默认值，用户自己的选择优先。答复中的 emoji 会被自动替换为文本符号（✅ → ✓、➡️ → →）或删除，代码块与行内代码保持原样；系统提示词也要求模型只用粗体、标题、列表和文本符号排版。

Telegram 中保留 `/model`、`/effort`、`/mode`（与“设置”相同）以及运行快捷指令的 `/cmd 名称`。`/provider`、`/system`、`/persona`、`/ws`、`/cron`、`/skills`、`/health`、`/runs`、`/file`、`/admin`、`/friend`、`/ban` 已移至服务器终端，管理员在 Telegram 中发送这些命令会收到终端命令提示。`/think` 展示最近的可见答复摘要。

## 三种执行模式

通过“设置 → 执行策略”或 `/mode` 切换。

| 模式 | 行为 |
| --- | --- |
| `manual`（默认） | 任意 shell、文件修改、撤销、Git 写操作、技能安装及子任务启动前，发送一次性批准/拒绝按钮。子任务每次写操作继续审核。 |
| `auto` | 自动连续执行工具，受用户身份、任务预算、命令与任务超时限制。 |
| `read-only` | 服务端拒绝任意 shell、写文件、撤销、Git 写入、技能安装和子任务启动。 |

人工审批只由本次任务发起者在同一个私聊确认，批准仅适用于当前完整参数，重复点击和过期按钮失效。AI 不会替人点击批准。为保证审核内容完整可读，人工模式单次工具参数 JSON 超过 2200 字节会被拒绝；模型应拆分成较小的编辑。

朋友默认只有只读工具，路径限制在工作区内；模型伪造未列出的工具调用也会被服务端检查。`friend_tools: "all"` 会授予可信朋友包括任意 shell 和文件写入的主机操作权限，仍遵守执行策略；只读工具的路径检查不构成 shell 或写入沙箱。只读 Git 查询可以使用，Git 写操作需审批；文件和媒体回传限管理员。请把工作区放在数据目录之外或独立子目录中，避免在工作区存放凭据。

`auto` 能执行系统用户有权执行的命令。部署时建议使用专用普通用户、容器或独立工作主机；不会伪称提供 Codex 原生操作系统沙箱。SSH 连接关闭能结束客户端，但远端脱离会话的后台进程可能继续运行。

## 技能自动获取

本地自动发现：

```text
skills/code-task.md
skills/testing/SKILL.md
<workspace>/.skills/<name>.md
<workspace>/.agents/skills/<name>/SKILL.md
```

项目 `.agents/skills` 优先于 `.skills`，项目技能优先于全局同名技能。按技能名称、描述、触发条件和中英关键词为当前任务匹配最多三个技能；也支持模型用 `list_skills` / `load_skill` 主动加载。技能是辅助文本，不能覆盖权限和审批策略。自带编码、调研、运维、安全审查、发布检查及数据分析技能。

```markdown
---
name: testing
description: 测试与回归验证，test regression
when: 修改代码或修复 bug 后
---
先确定受影响行为，再运行最小必要验证，报告真实结果。
```

远程获取使用管理员配置的目录，而非运行网页里的安装脚本。将原始 Markdown URL、固定 SHA256 与主机白名单加入配置：

```json
{
  "auto_load_skills": true,
  "auto_fetch_skills": false,
  "skill_allowed_hosts": ["raw.githubusercontent.com"],
  "skill_sources": [
    {
      "name": "my-skill",
      "url": "https://raw.githubusercontent.com/YOUR_ORG/YOUR_REPO/COMMIT/skills/my-skill.md",
      "sha256": "替换为文件的64位SHA256十六进制摘要",
      "description": "代码测试、审查 test review"
    }
  ]
}
```

从审核过的文件运行 `sha256sum my-skill.md` 获取摘要；示例地址与摘要是占位值。下载必须使用 HTTPS、匹配精确主机白名单、通过 SHA256 校验，最多 128 KiB；重定向也会检查。安装只保存 Markdown 文本。

```bash
tg-agent admin skills                    # 列出本地技能
tg-agent admin skills install my-skill
tg-agent admin skills sync
```

管理员在服务器终端执行 `install` / `sync` 就是本次安装授权；模型发起的 `install_skill` 仍受审批。只有同时配置 `auto_fetch_skills: true`、`auto_load_skills: true` 且当前为管理员的 `auto` 模式，才会按任务自动下载匹配的可信技能。首次启动不需要远程技能源。

SSH 后端只从机器人主机的 `skills_dir` 加载技能；需要使用远端项目技能时，请复制到机器人技能目录或通过可信目录分发。项目 `AGENTS.md` 在执行后端读取。

## 工具与任务可靠性

22 个工具（每次调用都需填写 intent 用途说明）：plan 计划、memory 记忆、schedule 定时、send_file 发送文件，以及命令、文件分页、精确编辑、项目地图、搜索、Git、撤销、任务清单、网络检测、网页检索、技能和子智能体。工具按依赖顺序执行。读写与撤销使用独立备份，备份失败会中止写入；精确编辑拒绝截断的原文。子任务继承用户、目录、执行策略、取消信号和总预算，不会提升为主人。

上下文按完整用户轮次裁剪，过长时压缩为交接摘要。私聊历史保存到 `sessions/`，重启后按需恢复。达到执行预算会返回未完成状态与下一步；错误、取消、成功都会记录。任务清单按任务隔离。

| 配置项 | 默认 |
| --- | --- |
| `max_steps` | 40 次模型调用预算，主任务与子任务共享 |
| `max_subagents` | 最多 4 个子任务；深度最多 3 层 |
| `command_timeout_seconds` | 120 秒 |
| `task_timeout_seconds` | 1800 秒 |
| `approval_timeout_seconds` | 300 秒 |
| `max_tool_output` | 24000 字节，超长结果截断并提示 |
| `max_history` | 30 轮 |
| `show_steps` | false，隐藏滚动详情，执行记录仍保存 |
| `display` | 显示开关的全员默认值，见“显示与统计” |
| `llm_retries` | 6，模型接口失败后的静默重试次数（0 为默认，-1 关闭，最多 20） |
| `registration` | `approval`，用户注册方式：`off` / `approval` / `open` |
| `bot_name` `author` `support_text` `support_url` | 名称、作者、支持说明与链接；也可在服务器终端修改 |
| `providers[].efforts` | 可选，该服务商接受的推理档位，如 `["low","medium","high"]`；不填则按模型名自动匹配 |

**推理档位按 API 提供的选项选择**：“推理档位”面板和 `/effort` 只列出当前服务商/模型支持的值。来源依次是 `providers[].efforts`、内置的模型名对照（DeepSeek、GPT-4o、Qwen 等不支持该参数，只显示“off”，DeepSeek 用 deepseek-reasoner 开启思考；GPT-5 为 minimal/low/medium/high，GPT-5.1 为 low/medium/high，GPT-5.2 起含 xhigh，o 系列 low/medium/high，grok-3-mini low/high，Gemini 2.5+ low/medium/high），未识别的模型列出常见档位。请求只携带当前模型支持的值；服务商返回“不支持 reasoning_effort”时会去掉该参数静默重发，并在本次运行内不再发送。

模型接口遇到连接失败、超时、408/409/425/429/5xx、网关返回的非 JSON 页面、空结果或临时性报错时，会在后台按 1s、2s、4s… 最长 30s 的间隔（含少量抖动，优先遵循 `Retry-After`）静默重试，用户只看到任务继续进行；每次重试写入 `agent.log`，次数记入执行记录。鉴权失败、请求参数错误、上下文超长、余额不足等重试无济于事的错误会立即结束任务，并提示可用 `/continue` 重跑。Telegram 接口的发送与编辑同样在后台重试（最多 4 次）。

任务到达预算时可另有一次不带工具的总结请求。工具输出截断后仍可能影响模型判断；应分页读取大文件。`read-only` 是应用层权限策略，不防御恶意项目里的可执行 Git 扩展或被攻陷的宿主工具；可信工作区仍是必要前提。

## 定时任务、快捷指令与 SSH

在服务器终端管理（结果发到主人的私聊）：

```bash
tg-agent admin cron add daily 09:00 汇总服务器状态
tg-agent admin cron add weekdays 18:00 检查未提交改动
tg-agent admin cron add in 45m 提醒我开会
tg-agent admin cron pause 1        # resume 1 | run 1 | del 1
tg-agent admin cmd add review 检查当前项目的未提交改动并给出问题
```

在 Telegram 里发送 `/cmd review` 运行快捷指令；对 AI 说“每天 9 点提醒我…”，它也会自己创建定时任务。定时任务最短间隔 60 秒，使用创建者身份，并在触发前重新检查管理员权限。已有任务仍在运行时，不排队叠加同一私聊的新任务。旧版没有创建者 ID 的定时任务会停用，需要重新创建。

远程执行配置 `backend: "ssh"`、`ssh.host/user/port/key_path` 和远端绝对 `workspace`。先用服务用户执行一次正常 SSH 登录，核验主机指纹并写入 `known_hosts`；程序开启严格主机密钥校验，不再关闭校验。远端需有 bash、base64、mktemp 与 GNU coreutils；路径检查使用 `realpath`。

## 部署

推荐 Linux 普通用户。systemd 模板在 [deploy/tg-agent.service](deploy/tg-agent.service)，密钥环境文件放在 `/etc/tg-agent.env`，权限 `0600`，把服务用户和 `/opt/tg-agent` 目录权限设置好。模板将可写范围限制在该目录；需操作其他目录时按实际工作范围调整。

容器方式：

```bash
mkdir -p data/workspace
cp config.example.json data/config.json
cp -R skills data/skills
cp .env.example .env
# 编辑 .env 与 data/config.json
sudo chown -R 10001:10001 data
chmod 600 .env data/config.json
docker compose up -d --build
```

`data` 包含所有持久化文件；容器以 UID 10001 运行。备份实际配置、`cron.json`、`sessions/`、`runs/`、`skills/` 和工作区；备份含会话内容，应私密保存。执行日志默认不做自动轮转，长期运行请配置保留与清理策略。

## 验证与改动

```bash
go vet ./...
go test -race ./...
go build ./...
```

可用 `make release` 构建 Linux amd64 / arm64 单文件程序。本包的验证结果见 [docs/VALIDATION.md](docs/VALIDATION.md)，主要修复见 [CHANGELOG.md](CHANGELOG.md)，旧版迁移见 [升级说明](docs/UPGRADE.md)。没有实际 Telegram token / 模型 API key 的离线测试不会声称完成真实服务联调。

## 授权

本包没有替原始上传代码指定新的许可证。你需要确认对原代码、所选模型服务和第三方技能的使用与分发权利；不能仅凭本次改造保证所有商业使用权。[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 记录了 Codex 参考范围及上游许可信息。
