package main

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
)

// Backend messages are written in Chinese. For an English UI they are
// translated where they leave the JSON API, so messages injected into agents
// are unaffected. Only whole, known messages match, which leaves user content
// such as session titles alone.
var messagesEN = map[string]string{
	"原生写回": "Native write-back", "生成交接文档，用户手动引用": "Handoff document, referenced by hand", "生成交接文档，启动时注入": "Handoff document, injected at start",
	"hook 注入增量 + MCP 查询": "Hook injection + MCP queries", "hook 启动注入 + MCP 检索": "Injected at start + MCP search", "hook 注入增量": "Hook injection", "hook 启动注入": "Injected at start",
	"仅 MCP 按需查询": "MCP queries only", "写入规则文件的托管区块 + MCP": "Managed block in rule files + MCP", "不支持": "Not supported", "仅写入规则文件托管区块": "Managed block in rule files only",
	"这个会话没有可同步的迁移记录。": "This session has no migration to sync with.", "两边已经是最新的。": "Both sides are up to date.", "这个项目被隐私规则排除，不能同步": "This project is excluded by privacy rules and can't be synced.",
	"当前配置不允许这个方向的同步。": "The current configuration doesn't allow syncing in this direction.", "新增内容超出目标 agent 的上下文预算，没有写入。": "The new content exceeds the target agent's context budget. Nothing was written.",
	"目标会话的存储格式暂不支持追加；可以用“迁移”生成新的副本。": "The target session's storage format can't be appended to yet. Use Migrate to create a new copy.",
	"目标会话末尾有未写完的内容，已停止，请稍后重试":        "The target session ends with an unfinished write. Stopped; please try again later.", "写入后读回校验失败，已回滚": "Read-back verification failed. The write was rolled back.",
	"agent 未启用（agents.enabled）": "Agent is disabled (agents.enabled)", "规则同步已关闭（sync.rules）": "Rule sync is off (sync.rules)", "被 sync.exclude_files 排除": "Excluded by sync.exclude_files", "这个文件就是规则源": "This file is the rule source",
	"规则源不存在": "Rule source doesn't exist", "未声明": "undeclared", "副本不存在，创建软链接": "No copy yet; will create a symlink", "软链接已指向规则源": "Symlink already points to the source", "已有指向别处的软链接": "A symlink here points elsewhere",
	"内容与源相同，替换为软链接": "Same content as the source; will replace with a symlink", "已有内容不同的目录": "A directory with different content exists", "已有内容不同的文件，不自动覆盖": "A file with different content exists; it won't be overwritten",
	"副本不存在，生成副本": "No copy yet; will generate one", "副本只有托管区块，生成副本": "Copy holds only managed blocks; will generate it", "已有用户手写的内容，不自动覆盖": "Contains hand-written content; it won't be overwritten", "规则源已变化，重新生成": "Rule source changed; will regenerate", "副本与源一致": "Copy matches the source",
	"skill 源是指向源目录之外的软链接，拒绝同步": "Skill source is a symlink out of the source directory; refused", "skill 同步已关闭（sync.skills）": "Skill sync is off (sync.skills)", "被 sync.skills.exclude 排除": "Excluded by sync.skills.exclude",
	"规则源和副本都被修改过，两边都已保留；请手工合并后再同步": "Both the source and the copy changed. Both were kept; merge them by hand, then sync.", "已把副本内容合回规则源": "Copy merged back into the source",
	"这个 agent 不支持自动检测，只能手动启用": "This agent has no automatic check and can only be enabled by hand", "客户端未能继续探测会话": "The client couldn't continue the probe session", "客户端未能在限定时间内继续探测会话": "The client didn't continue the probe session in time", "客户端加载了探测会话，但历史内容缺失": "The client loaded the probe session, but its history is missing",
	"缺少目标 agent": "No target agent specified", "项目记忆需要先选择项目目录": "Choose a project folder for project memories first", "记忆内容不能为空": "Memory content can't be empty", "只能编辑生效中或待审核的记忆": "Only active or pending memories can be edited", "无法确定该 agent 的版本": "Can't determine this agent's version",
	"新增 MCP server": "New MCP server", "与源一致": "Matches the source", "源已更新": "Source updated", "已从源中删除": "Removed from the source",
	"被手工修改过；可合回源，或以源为准覆盖":                    "Edited by hand. Merge it into the source, or overwrite it from the source.",
	"已有同名但内容不同的配置，不会自动覆盖；可合回源，或以源为准覆盖":       "A different server with the same name exists and won't be overwritten. Merge it into the source, or overwrite it from the source.",
	"已在这个 agent 中被删除；可以以源为准重新写入，或合回源（从源中删除）": "Deleted in this agent. Write it again from the source, or merge back (remove it from the source).",
	"已从源中删除，但这里被手工修改过；可合回源，或以源为准删除":          "Removed from the source, but edited here by hand. Merge it back, or delete it as the source says.",
	"这个 agent 不支持同步 MCP 配置":                  "This agent can't sync MCP configuration", "这个 agent 没有该范围的 MCP 配置": "This agent has no MCP configuration for this scope",
	"MCP 配置同步已关闭（sync.mcp_configs）": "MCP configuration sync is off (sync.mcp_configs)", "Claude Code 的 MCP 配置不支持 cwd": "Claude Code's MCP configuration doesn't support cwd",
	"Codex 不支持 SSE 传输的 MCP server": "Codex doesn't support MCP servers over SSE",
	"记忆同步已关闭（sync.memory），不能导入":    "Memory sync is off (sync.memory), so nothing can be imported", "同步已暂停，恢复后再导入": "Sync is paused. Resume it before importing.",
	"已有一个历史导入正在进行": "An import is already running",
	"任务需要先选择项目目录":  "Choose a project folder for the task first", "任务标题不能为空": "Task title can't be empty", "not found": "Not found",
	"数据库可打开": "Database opens", "数据库权限为 0600": "Database permissions are 0600", "守护进程在线": "Daemon is online",
}

var patternsEN = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`^恢复命令：(.*)$`), "Resume command: $1"},
	{regexp.MustCompile(`^只在 (.*) 中配置；可合回源后同步给其他 agent$`), "Configured only in $1. Merge it into the source to sync it to the other agents."},
	{regexp.MustCompile(`^还没有 MCP 源（(.*)），两边也都没有可同步的 server$`), "No MCP source yet ($1), and no servers to sync on either side"},
	{regexp.MustCompile(`(?s)^目标 agent 不支持原生写回（版本 (.*)），已生成交接文档：(.*)\n请在目标 agent 中引用这个文件。$`), "The target agent doesn't support native write-back (version $1). Handoff document created: $2\nReference this file in the target agent."},
	{regexp.MustCompile(`(?s)^目标 agent 不支持原生写回（版本 (.*)），已生成交接文档：(.*)\n下次在该项目启动 (.*) 时会自动注入。$`), "The target agent doesn't support native write-back (version $1). Handoff document created: $2\nIt will be injected the next time $3 starts in this project."},
	{regexp.MustCompile(`^上次同步被中断，且 (.*) 之后又发生了变化，已停止同步，请手动检查$`), "The last sync was interrupted and $1 has changed since. Syncing stopped; please check it by hand."},
	{regexp.MustCompile(`^(.*) 不支持更新已有会话。$`), "$1 can't update existing sessions."},
	{regexp.MustCompile(`^(.*) 版本 (.*) 未验证写入，请先在概览页检测或启用。$`), "$1 version $2 isn't verified for writing. Check or enable it on the Overview page first."},
	{regexp.MustCompile(`^(.*) 的这个会话正在使用，退出后再更新，否则可能造成分叉。$`), "This $1 session is in use. Quit it before updating, or the history may fork."},
	{regexp.MustCompile(`^已把 (.*) 的 (\d+) 条新内容更新到 (.*)。$`), "Updated $3 with $2 new items from $1."},
	{regexp.MustCompile(`^已把 (.*) 的 (\d+) 条新内容更新到 (.*)。两边都有新内容，已以 (.*) 为准；(.*) 自己的新内容保留在原处，不会同步给 (.*)。$`), "Updated $3 with $2 new items from $1. Both sides had changed, so $4 was used; the new content in $5 stays there and isn't sent to $6."},
	{regexp.MustCompile(`^agent 同步方向为 (.*)，不接收同步$`), "Agent direction is $1, so it doesn't receive syncs"},
	{regexp.MustCompile(`^规则源不存在，但这里已有内容；可用 bridge sync --adopt (.*) 把它作为源$`), "No rule source, but this file has content; use bridge sync --adopt $1 to make it the source"},
	{regexp.MustCompile(`^格式未经验证（(.*)），不同步$`), "Unverified format ($1); not synced"},
	{regexp.MustCompile(`^副本被手工修改（hash 不符）.*$`), "The copy was edited by hand (hash mismatch)"},
	{regexp.MustCompile(`^读取现有会话失败：(.*)$`), "Couldn't read an existing session: $1"},
	{regexp.MustCompile(`^现有会话 (.*) 解析不出内容$`), "Existing session $1 yielded no content"},
	{regexp.MustCompile(`^本机没有找到版本 (.*) 的客户端$`), "No installed client reports version $1"},
	{regexp.MustCompile(`^24 小时内已经迁移过这个会话（(.*)）。$`), "This session was already migrated in the last 24 hours ($1)."},
	{regexp.MustCompile(`^(.*) (\S+) 已验证写入$`), "$1 $2 is verified for writing"},
	{regexp.MustCompile(`^(.*) hooks 已安装$`), "$1 hooks are installed"},
	{regexp.MustCompile(`^(.*) hooks 已被信任$`), "$1 hooks are trusted"},
	{regexp.MustCompile(`^外部适配器 (.*) 已信任$`), "External adapter $1 is trusted"},
	{regexp.MustCompile(`^无法连接后台服务：(.*)$`), "Can't reach the background service: $1"},
	{regexp.MustCompile(`^不支持修改配置项 (.*)$`), "Setting $1 can't be changed here"},
}

func translateMessage(s string) string {
	suffix := ""
	for _, tail := range []struct{ zh, en string }{{"（只能作为接收方）", " (receive only)"}, {"（对方看不到它的进展）", " (others can't see its progress)"}, {"（无法从它提取记忆）", " (memories can't be extracted from it)"}} {
		if n := len(s) - len(tail.zh); n > 0 && s[n:] == tail.zh {
			s, suffix = s[:n], tail.en
			break
		}
	}
	if en, ok := messagesEN[s]; ok {
		return en + suffix
	}
	for _, p := range patternsEN {
		if p.re.MatchString(s) {
			return p.re.ReplaceAllString(s, p.with) + suffix
		}
	}
	return s + suffix
}

func walkStrings(v any) any {
	switch x := v.(type) {
	case string:
		return translateMessage(x)
	case []any:
		for i := range x {
			x[i] = walkStrings(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = walkStrings(x[k])
		}
	}
	return v
}

// localizeAPI translates a response when the app runs in English.
func localizeAPI(out map[string]any) any {
	if os.Getenv("BRIDGE_LANG") != "en" {
		return out
	}
	b, err := json.Marshal(out)
	if err != nil {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return out
	}
	return walkStrings(v)
}
