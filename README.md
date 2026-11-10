<p align="center"><img src="app/logo.png" width="120" alt="Bridge logo"></p>

<h1 align="center">Bridge</h1>

<p align="center">让编程 agent 共享同一份上下文：在 Claude Code 和 Codex 之间接续会话、互通进展、共享记忆和规则。</p>

<p align="center"><strong>简体中文</strong> · <a href="README.en.md">English</a></p>

---

同时用多个编程 agent 时，每个 agent 都是孤岛。在 Claude Code 里调试了 30 轮，换到 Codex 就得从头交代；纠正过的编码偏好，另一个 agent 不知道；`CLAUDE.md` 和 `AGENTS.md` 内容大同小异，改一处忘一处；两个 agent 同时工作，互相不知道对方改了什么。

Bridge 是一个本地运行的工具，用一个统一模型解决这些问题。数据默认留在本机。

## 和同类项目相比的优势

现有项目大多只解决其中一个方面。Bridge 把**会话、记忆、文件、实时协作**放在同一个统一事件模型上。

| 项目 | 原生会话迁移 | 实时协作 | 共享记忆 | 规则 / Skill 同步 | 新增 agent 的方式 |
| --- | :---: | :---: | :---: | :---: | --- |
| **Bridge** | ✅ | ✅ | ✅ | ✅ | 实现一个适配器，任意语言 |
| continues | 交接文档注入 | — | — | — | 覆盖多种 agent |
| cc-sync | ✅ 仅两家 | — | — | — | 两两转换 |
| rses | 上下文注入 | — | — | — | 内置三家 |
| [agentmemory](https://github.com/rohitg00/agentmemory) | — | — | ✅ | — | MCP + hooks |
| [Memorix](https://github.com/AVIDS2/memorix) | — | — | ✅ | ✅ | 不做原生会话迁移 |
| agentsync | — | — | — | ✅ 软链接 | 只管文件 |

> 表格依据各项目的公开介绍整理，这些项目可能已经更新，欢迎提 issue 指正。

### 1. 原生迁移，而不是一份摘要
会话会转换成目标 agent 自己的格式，出现在它的 `resume` 列表里，打开就是真实的对话历史和工具调用记录。无法对应的工具会明确降级成文本，迁移结果会报告省略、压缩和截断了什么，不会把有损说成无损。

### 2. 迁移后还能互相更新
迁移不是一次性的。Bridge 记录两边的同步边界，之后任一边有新内容，都可以把增量追加到另一边，会话 ID 不变。两边同时有更新时，由你选择以哪边为准。

### 3. 实时互通进展
hook 在每次发消息时，把其他 agent 的最新进展注入当前 agent：改了哪些文件、用户的最新指令、对方的最新回复。按已读水位只注入增量，有 token 上限。这已在真实的 Claude Code 和 Codex 上验证双向生效。

### 4. Hub 架构，新增 agent 不用两两适配
所有 agent 经适配器转成统一事件，N 个 agent 只需要 N 个适配器。外部适配器用 JSON-RPC over stdio，任何语言都能写，并提供一致性测试。

### 5. 把兼容性当作一等问题
agent 的会话文件不是公开接口，版本升级可能改变格式。Bridge 的做法是：

- 未验证的版本默认只读，迁移自动降级为交接文档，不会静默写坏。
- 发现新版本时自动检测：让**真实客户端**加载一条探测会话，通过才放行。检测在临时目录进行，不联网、不使用凭据。
- 也可以在界面上对当前版本手动启用，并随时撤销。

### 6. 写入安全
写入前备份，临时文件加原子重命名，写完读回校验，失败回滚。跨文件的写入有操作日志，进程在任何一步被杀都能恢复。卸载时会保留你后来手动改过的内容。

### 7. 隐私默认收紧
- 脱敏在写入数据库之前完成，内置常见密钥规则，推理内容不入库。
- 路径排除是安全底线，项目配置不能取消全局排除，软链接也绕不过去。
- 不开放 TCP 端口，只用权限 0600 的 Unix socket。
- 记忆自动提取默认关闭，因为它会把对话片段发给模型服务，开启前会明确告知。

## 功能

| 模块 | 能力 |
| --- | --- |
| 会话迁移 | 索引和筛选会话、原生迁移、工具映射、超长裁剪、交接文档降级、迁移后双向更新 |
| 实时协作 | 增量采集、hook 注入进展、MCP 查询（`session_list`、`session_events`、`memory_search`）、文件认领提醒、工作区改动提示 |
| 任务看板 | 项目内共享任务，`task_add` / `task_claim` / `task_update` / `task_done`，认领是原子的，只有认领者能更新；新会话开始时注入未完成任务 |
| Worktree 模式（可选） | 每个 agent 在自己的 git worktree 和分支上工作，互不覆盖文件；同一仓库的所有 worktree 共享进度、记忆和任务看板；汇总各 worktree 的提交、未提交改动和最近活动 |
| 记忆 | 全局偏好和项目记忆、自动提取（可选）、从历史会话导入、冲突取代、审核、删除后不再提取 |
| 文件同步 | 规则与 Skill 以一份为源、软链接或生成副本、漂移检测、合回源或以源为准；MCP server 配置按条目同步（默认关闭） |
| 配置 | 四层合并、按类别和 agent 开关、同步方向、`why` 解释生效来源 |
| 桌面应用 | macOS 原生界面和状态栏：查看状态、迁移、暂停同步、管理记忆和设置 |

## 快速开始

### 下载安装（推荐）

需要 macOS 13+，支持 Apple Silicon 和 Intel。

1. 从 [Releases](https://github.com/fufuandfoufou/bridge/releases/latest) 下载 `Bridge-<版本>.dmg`，把 Bridge 拖到"应用程序"。
2. 当前版本**没有经过 Apple 签名和公证**，macOS 默认会阻止打开，需要在终端放行一次：

   ```bash
   xattr -cr /Applications/Bridge.app
   ```

3. 打开 Bridge，点击"接入 agent"。接入后不要移动应用。
4. Codex 要求用户信任 hooks 后才会执行，接入后请在 Codex 的 `/hooks` 中审核一次。

### 从源码构建

需要 Go 1.27+；构建桌面应用还需要 Xcode。

```bash
git clone https://github.com/fufuandfoufou/bridge.git && cd bridge
go build -o bin/bridge ./cmd/bridge

./bin/bridge init          # 探测 agent，安装 hooks 与 MCP，注册后台服务
./bin/bridge status        # 查看各 agent 实际生效的通道
./bin/bridge sessions      # 列出会话
./bin/bridge handoff <会话 ID> --to codex
```

构建桌面应用：

```bash
./scripts/build-app.sh     # 生成 dist/Bridge-<版本>.dmg
```

## 常用命令

| 命令 | 作用 |
| --- | --- |
| `init` / `uninstall` | 接入 / 卸载并还原 |
| `status` / `doctor` | 接入状态 / 自检 |
| `sessions` / `handoff` | 列出 / 迁移会话 |
| `plan` / `sync` | 预演 / 执行规则与 Skill 同步 |
| `why <路径或记忆 ID>` | 解释同步结果及生效的配置 |
| `memory list/review/add/edit/forget/extract` | 管理记忆 |
| `memory import [--all] [--since 90d] [--dry-run]` | 从历史会话提取记忆：先估算、确认后才发送，结果进入待审核 |
| `task list/add/reopen/delete` | 管理当前项目的任务看板 |
| `worktree add <agent>` / `list` / `remove` | 为 agent 创建独立 worktree（分支 `bridge/<agent>`）、查看汇总、删除（分支保留） |
| `adapter list/trust/test` | 外部适配器与一致性测试 |

任何命令都可以用 `--set key=value` 覆盖配置。配置分四层：内置默认 → `~/.config/bridge/config.toml` → `<repo>/.bridge.toml` → `--set`。

## 兼容性

| Agent | 内置可写版本 | 其他版本 |
| --- | --- | --- |
| Claude Code | 2.1.x | 自动检测，或手动启用 |
| Codex | 0.144.x、0.166.x | 自动检测，或手动启用 |

已在真实客户端上验证过的：Claude Code 2.1.317 ↔ Codex CLI 0.144.1 的原生迁移和恢复，Codex 0.166.0-alpha.6 的恢复，以及两个方向的 hook 注入。

## 当前状态与局限

这是早期项目，请先在不重要的会话上试用。

- 迁移不是字节级副本：推理内容不迁移，无法映射的工具转为文本，超长会话会压缩。
- 追加的历史要重新打开会话才生效，正在运行的客户端不会感知。Codex 没有运行状态登记，只能按文件最近是否变化来判断是否在使用。
- Codex 的原生分页会话暂不支持追加更新，只能生成新副本。
- 文件认领可设为提醒或拦截，只约束接入的 agent 的编辑工具；Shell 命令、手工编辑不受保护，后台离线时不拦截。
- 后台会监听最近两小时活跃的 git 项目目录（跳过隐藏目录、node_modules、vendor、dist、build、target 等），把其他来源的文件改动告诉 agent。来源只是推测：附近有某个 agent 的编辑工具改过同一文件时标为“可能来自”，否则标为未知。
- MCP 配置同步默认关闭，开启方式是 `bridge config` 或设置页里的 `sync.mcp_configs`。源文件是 `~/.config/bridge/mcp.toml`（全局）和项目的 `.agents/mcp.toml`，按条目写入 `~/.claude.json`、`.mcp.json` 和 Codex 的 `config.toml`。已有的同名配置、被手工改过的条目不会被覆盖。Codex 只读取已信任项目里的 `.codex/config.toml`，也不支持 SSE 传输。详见 [docs/mcp-sync.md](docs/mcp-sync.md)。
- 导入历史会话会把脱敏后的对话文本发给提取用的 agent，并消耗你的额度。导入前会先显示会话数、调用次数和 token 估算，迁移生成的副本不会重复提取，结果全部进入待审核。中断后再次导入会从中断处继续。
- Worktree 模式不会自动合并分支，合并由你决定（界面里可以复制合并命令）。删除 worktree 时有未提交的改动会先拒绝，分支和已提交的内容始终保留。
- 目前只支持 macOS。

## 文档与贡献

- [外部适配器协议](docs/adapter-protocol.md)
- [适配器示例](examples/bridge-adapter-demo)

运行测试：

```bash
go test ./...
```

验收测试位于 `test/acceptance`，全部在隔离的 HOME 中运行，不会接触真实的 agent 数据。

欢迎为新的 agent 贡献适配器，先用 `bridge adapter test <id>` 跑通一致性测试。

## 许可证

本项目基于 [Apache License 2.0](LICENSE) 开源。
