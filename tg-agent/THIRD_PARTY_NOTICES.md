# 第三方来源与许可说明

## OpenAI Codex：设计参考

- 项目：[openai/codex](https://github.com/openai/codex)
- 参考提交：`57a808a3920f05f2d4b9eab2d0493e1df136ae6b`
- 上游版权声明：Copyright 2025 OpenAI
- 上游许可：[Apache License, Version 2.0](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/LICENSE)
- 上游声明文件：[NOTICE](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/NOTICE)

本交付中的 Codex 参考范围是审批协调、工具运行生命周期和技能发现的设计研究。
Go 功能代码在用户提供的 tg-agent 基础上独立改写，没有随包分发 Codex 的 Rust 源文件、
二进制、图标、示例技能或其他上游资源。具体阅读文件见
[源码参考记录](docs/CODEX_REFERENCE.md)。

上游 NOTICE 另外注明其包含来自 Ratatui 的 MIT 许可代码，相关版权属于
Florian Dehau（2016–2022）及 The Ratatui Developers（2023–2025）。本项目没有引入
这些终端界面代码；该说明仅用于准确记录所阅读的上游来源。

Apache-2.0 允许在遵守许可条件的前提下商业使用及修改、分发。
如果后续直接引入 Codex 代码，分发时需附带适用许可、保留必要版权和 NOTICE 声明，
并在修改文件中标明修改。Apache-2.0 不授予 OpenAI 或 Codex 商标使用权。

## Go 标准库与外部服务

程序使用 Go 标准库；本项目的 `go.mod` 没有第三方 Go 模块依赖。
构建和分发 Go 工具链或运行时相关组件时，适用
[Go 项目的许可](https://go.dev/LICENSE)。
Telegram Bot API、OpenAI 兼容模型接口及搜索服务属于运行时连接的外部服务，
没有将这些服务本身的软件分发到源码包中。运营者应自行配置服务账号与密钥，
并遵守相应服务条款。

## 原始 tg-agent 代码

用户提供的原始源码包没有顶层 LICENSE 文件。本次改造不替原作者重新授权，
也不将上游 Codex 的 Apache-2.0 许可套用到原始 tg-agent 全部代码。
原始代码及本次交付的使用、分发权应由具有相应权利的项目所有者确定。
