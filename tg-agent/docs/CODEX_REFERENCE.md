# Codex CLI 源码参考记录

本次改造实际阅读了 [openai/codex](https://github.com/openai/codex) 的官方公开源码。
为便于复核，以下链接固定到提交
[`57a808a3920f05f2d4b9eab2d0493e1df136ae6b`](https://github.com/openai/codex/commit/57a808a3920f05f2d4b9eab2d0493e1df136ae6b)，
阅读日期为 2026-11-07。

本项目仍是基于原有 tg-agent 的 Go 实现，没有内嵌 Codex CLI，也没有移植其 Rust 模块。
下表记录参考的具体机制及在 Telegram 场景中的落点，便于后续维护时核对设计来源。

| 官方源码 | 参考机制 | 本项目中的实现方式 |
| --- | --- | --- |
| [core/src/tools/orchestrator.rs](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/core/src/tools/orchestrator.rs) | 执行前集中判断审批要求，再交给工具运行层；拒绝与失败保留为结果 | Agent 工具入口统一执行权限检查；审批与 shell 执行分开 |
| [core/src/tools/approvals.rs](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/core/src/tools/approvals.rs) | 审批动作包含命令、工作目录和权限；明确区分人工批准与自动审查 | Telegram 审批绑定当前任务及具体工具请求，确认按钮不能代替用户身份校验 |
| [core/src/tools/runtimes/unified_exec.rs](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/core/src/tools/runtimes/unified_exec.rs) | 进程执行具有独立的生命周期、超时和取消信号；审批上下文保留执行环境 | 任务取消贯穿模型请求、审批等待和本地进程；子任务沿用原任务执行配置 |
| [ext/skills/src/loader/discovery.rs](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/ext/skills/src/loader/discovery.rs) | 目录扫描有深度、目录数和条目数上限，并明确处理符号链接 | 技能扫描限定可信目录，支持 `SKILL.md`，限制文件大小和遍历范围 |
| [ext/skills/src/selection.rs](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/ext/skills/src/selection.rs) | 显式技能引用优先按路径或名称解析，按来源和包去重 | 技能按名称稳定索引；显式选择与任务相关性选择共同用于加载 |
| [ext/skills/src/render.rs](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/ext/skills/src/render.rs) | 技能目录与完整提示分层，按上下文预算限制注入量 | 提供简短技能目录；仅加载与当前任务相关的少量完整技能 |

## 审核模式的含义

官方审批实现明确保护必须由人确认的审批点。因此，本项目的人工审核模式不会让
模型自行点击“允许”，也不会因技能或网页中的指令跳过审批。界面可以折叠高级设置，
但审批发生时必须展示需要确认的具体动作。

自动模式表示管理员已经选择了自动执行策略，运行时按该策略执行。
只读模式由服务端限制工具调用；仅在系统提示词中写“只读”不足以形成权限边界。
这三个模式与 Codex 的具体策略名称没有一一对应关系，也不意味着本项目具有
Codex 的操作系统沙箱、网络代理隔离或完整权限系统。

## 技能下载的边界

自动发现本地技能、按任务自动加载技能、从远程安装技能是三个独立步骤。
本项目的远程安装采用管理员配置的目录、HTTPS 来源、主机白名单和 SHA-256 内容校验，
下载对象为 Markdown 技能说明。下载技能不会自行执行其中附带的安装脚本。
这部分来源校验是适用于本项目的独立设计，不宣称是官方 Codex 的完整技能分发实现。

## 许可与兼容性

上游固定提交的 [LICENSE](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/LICENSE)
为 Apache-2.0，附带 [NOTICE](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/NOTICE)。
本次没有将上游源文件或代码片段收入 Go 程序，具体范围见
[THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)。
后续若直接引入上游源码，应重新检查对应文件的许可、保留必要声明并记录修改。
不应把“参考 Codex 设计”表述为 OpenAI 官方产品或官方授权服务。
