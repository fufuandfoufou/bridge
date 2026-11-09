# 外部适配器协议（版本 1）

外部适配器是一个可执行文件，任何语言都可以写。它在 stdin / stdout 上收发 JSON-RPC 2.0，每行一条消息。日志写到 stderr。

## 发现与信任

bridge 从两个位置查找适配器：

- PATH 中名为 `bridge-adapter-<id>` 的可执行文件。
- 配置项 `[adapters.<id>] path = "..."`。

适配器视为不可信代码，首次启用需要运行 `bridge adapter trust <id>`。信任绑定文件内容的 hash，文件变化后要重新确认。

进程按需启动，空闲 60 秒后退出。单次调用超时 30 秒。

## 方法

| 方法 | 参数 | 返回 |
| --- | --- | --- |
| `initialize` | `{protocol_version, agent_version}` | `{protocol_version, capabilities}` |
| `detect` | `{}` | `{installed, version, data_dir}` |
| `sessions/list` | `{filter: {cwd, since, limit}}` | `{sessions: [SessionRef]}` |
| `sessions/read` | `{ref, from: Cursor}` | `{events: [Event], cursor: Cursor}` |
| `sessions/write` | `{session, events}` | `{id, path, resume_command}` |
| `hooks/install` | `{command, mcp_command}` | `{}` |
| `hooks/uninstall` | `{}` | `{}` |

不支持的方法返回错误码 `-32601`。协议版本不一致时，该适配器按无能力处理。

## 数据形状

字段定义以 `internal/model` 和 `pkg/adapterkit` 的 JSON 标签为准。

- `capabilities`：`session_read`、`session_write`、`live_tail`、`hooks`、`mcp`、`rules_files`、`skills_dirs`、`context_window`、`tools`。
- `Event`：`seq`、`kind`、`role`、`content`、`tool`、`file`、`ts`。`kind` 取 `message`、`tool_call`、`tool_result`、`file_change`、`reasoning_stub`、`meta`。
- `tool.canonical_name` 必须来自统一工具词汇，无法归类时用 `opaque`。
- `Cursor` 由适配器维护：`seq` 必须递增，用同一游标再次读取不能返回已经读过的事件。

`rules_files` 和 `skills_dirs` 的每一项必须声明 `format`（`symlink`、`generate` 或 `source`），未声明的不会同步。

写入时适配器收到的是已脱敏的事件，不含原始记录。

## 验证

```bash
bridge adapter test <id>           # 只读检查
bridge adapter test <id> --write   # 另外写入一条测试会话，读回校验后删除
```
