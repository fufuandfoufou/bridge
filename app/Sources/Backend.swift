import Foundation
import SwiftUI

// All product logic lives in the bundled `bridge` binary; the app only calls
// its JSON interface, so the UI and the core can't drift apart.

/// Localizes text that isn't a literal handed straight to a SwiftUI view.
func tr(_ key: String, _ args: CVarArg...) -> String {
    let format = NSLocalizedString(key, comment: "")
    return args.isEmpty ? format : String(format: format, arguments: args)
}

struct BackendError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

enum Backend {
    static var binary: String {
        if let p = ProcessInfo.processInfo.environment["BRIDGE_BIN"] { return p }
        return Bundle.main.resourceURL?.appendingPathComponent("bridge").path ?? "bridge"
    }

    static func run(_ args: [String]) async throws -> String {
        try await Task.detached(priority: .userInitiated) {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: binary)
            p.arguments = args
            // Apps launched from Finder get a minimal PATH, without Homebrew.
            var env = ProcessInfo.processInfo.environment
            env["BRIDGE_LANG"] = Bundle.main.preferredLocalizations.first?.hasPrefix("en") == true ? "en" : "zh"
            env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:" + (env["PATH"] ?? "/usr/bin:/bin")
            p.environment = env
            let out = Pipe()
            p.standardOutput = out
            p.standardError = out
            p.standardInput = FileHandle.nullDevice
            try p.run()
            let data = out.fileHandleForReading.readDataToEndOfFile()
            p.waitUntilExit()
            let text = String(decoding: data, as: UTF8.self)
            if p.terminationStatus != 0 {
                throw BackendError(message: text.trimmingCharacters(in: .whitespacesAndNewlines))
            }
            return text
        }.value
    }

    private struct Envelope<T: Decodable>: Decodable {
        let ok: Bool
        let data: T?
        let error: String?
    }

    static func api<T: Decodable>(_ type: T.Type, _ args: [String]) async throws -> T {
        let text = try await run(["api"] + args)
        // The envelope is the last line; warnings may precede it on stderr.
        let line = text.split(separator: "\n").last.map(String.init) ?? ""
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        let env: Envelope<T>
        do {
            env = try decoder.decode(Envelope<T>.self, from: Data(line.utf8))
        } catch {
            throw BackendError(message: tr("无法解析后台返回：") + text.prefix(300))
        }
        guard env.ok, let data = env.data else {
            throw BackendError(message: env.error ?? tr("未知错误"))
        }
        return data
    }

    /// For calls whose result carries no data.
    static func call(_ args: [String]) async throws {
        let text = try await run(["api"] + args)
        let line = text.split(separator: "\n").last.map(String.init) ?? ""
        struct Status: Decodable { let ok: Bool; let error: String? }
        let s = try JSONDecoder().decode(Status.self, from: Data(line.utf8))
        if !s.ok { throw BackendError(message: s.error ?? tr("未知错误")) }
    }
}

struct AgentStatus: Decodable, Identifiable {
    let id: String
    let installed: Bool
    let version: String
    let enabled: Bool
    let direction: String
    let writable: Bool
    /// False for agents bridge only reads; they have no version to verify.
    let nativeWrite: Bool?
    let versionState: String
    let versionError: String
    let hooks: Bool
    let hooksTrusted: Bool
    let handoff: String
    let live: String
    let memory: String
    let sessions: Int
    let events: Int
    let last: String

    var displayName: String { Names.agent(id) }
}

struct StatusReport: Decodable {
    let daemon: Bool
    let paused: Bool
    let agents: [AgentStatus]
    let warnings: [String]?
}

struct SessionRow: Decodable, Identifiable, Hashable {
    let id: String
    let agent: String
    let title: String
    let cwd: String
    let model: String
    let last: String
    let parent: String
    let syncState: String
    let syncOther: String

    var needsSync: Bool { ["source_ahead", "target_ahead", "diverged"].contains(syncState) }
    var diverged: Bool { syncState == "diverged" }
    var syncLabel: String {
        switch syncState {
        case "current": return tr("已同步")
        case "diverged": return tr("两边有更新")
        case "source_ahead", "target_ahead": return tr("有更新")
        default: return ""
        }
    }

    var displayTitle: String { title.isEmpty ? tr("（未命名会话）") : title }
    var folder: String { URL(fileURLWithPath: cwd).lastPathComponent }
    var agentName: String { Names.agent(agent) }
}

struct SyncSessionResult: Decodable {
    let state: String
    let message: String
}

struct SessionsReport: Decodable {
    let sessions: [SessionRow]
    let warning: String
}

struct HandoffResult: Decodable {
    let mode: String
    let message: String
    let resumeCommand: String
    let documentPath: String
    let tokens: Int
    let reasoning: Int
    let degraded: Int
    let summarized: Int
    let truncated: Int
}

struct SyncAction: Decodable, Identifiable {
    var id: String { kind + agent + target + (name ?? "") }
    let kind: String
    let category: String
    let agent: String
    let source: String
    let target: String
    let scope: String
    let reason: String
    let name: String?

    /// Where the change lands; MCP servers are entries inside a config file.
    var displayTarget: String { name.map { "\(target) · \($0)" } ?? target }

    var kindName: String {
        switch kind {
        case "noop": return tr("已同步")
        case "skip": return tr("跳过")
        case "link": return tr("创建链接")
        case "relink": return tr("替换为链接")
        case "generate": return tr("生成副本")
        case "drift": return tr("副本被修改")
        case "conflict": return tr("内容冲突")
        case "need_source": return category == "mcp" ? tr("只在一边") : tr("缺少源文件")
        case "remove": return tr("删除")
        default: return kind
        }
    }

    var displayReason: String {
        switch kind {
        case "need_source" where category == "mcp": return tr("只在 %@ 中配置；可用下方“把副本合回源”加入源，再同步给其他 agent", Names.agent(agent))
        case "need_source": return tr("还没有规则源，这里已有内容；可用下方“把副本合回源”把它作为源")
        case "drift": return tr("副本被手工修改；可合回源，或以源为准覆盖")
        default: return reason
        }
    }

    var needsAttention: Bool { ["drift", "conflict", "need_source"].contains(kind) }
    var willChange: Bool { ["link", "relink", "generate", "remove"].contains(kind) }
}

struct PlanReport: Decodable { let actions: [SyncAction] }
struct SyncReport: Decodable { let applied: [SyncAction]; let blocked: [SyncAction] }

struct MemoryItem: Decodable, Identifiable {
    let id: String
    let scope: String
    let category: String
    let content: String
    let confidence: Double
    let status: String

    var statusName: String {
        switch status {
        case "active": return tr("生效中")
        case "pending": return tr("待审核")
        case "superseded": return tr("已取代")
        case "forgotten": return tr("已删除")
        default: return status
        }
    }
}

struct MemoriesReport: Decodable { let memories: [MemoryItem] }

struct DoctorCheck: Decodable, Identifiable {
    var id: String { name }
    let ok: Bool
    let name: String
}

struct DoctorReport: Decodable { let checks: [DoctorCheck] }

struct Settings: Decodable {
    var sessions: Bool
    var memory: Bool
    var globalMemory: Bool
    var projectMemory: Bool
    var rules: Bool
    var skills: Bool
    var mcpConfigs: Bool
    var mcpExclude: [String]
    var autoExtract: Bool
    var extractor: String
    var injectOnPrompt: Bool
    var injectTokenBudget: Int
    var conflictPolicy: String
    var keepRecentTurns: Int
    var excludeProjects: [String]
    var includeProjects: [String]
    var excludeFiles: [String]
    var excludeSkills: [String]
    var redact: [String]
    var enabled: [String]
    var directions: [String: String]
    /// Whether each agent receives other agents' progress automatically.
    var injects: [String: Bool]?
}

enum Names {
    static func agent(_ id: String) -> String {
        switch id {
        case "claude-code": return "Claude Code"
        case "codex": return "Codex"
        case "pi": return "pi"
        case "dsh": return "DeepSeek Harness"
        default: return id
        }
    }

    static func direction(_ d: String) -> String {
        switch d {
        case "send-only": return tr("只发送")
        case "receive-only": return tr("只接收")
        default: return tr("双向")
        }
    }
}

func tomlString(_ s: String) -> String {
    let data = try! JSONEncoder().encode(s)
    return String(decoding: data, as: UTF8.self).replacingOccurrences(of: "\\/", with: "/")
}

func tomlArray(_ a: [String]) -> String { "[" + a.map(tomlString).joined(separator: ", ") + "]" }

/// Shared app state.
@MainActor
final class AppModel: ObservableObject {
    @Published var status: StatusReport?
    @Published var sessions: [SessionRow] = []
    @Published var sessionsWarning = ""
    @Published var loadingSessions = false
    @Published var error: String?

    func refreshStatus() async {
        do { status = try await Backend.api(StatusReport.self, ["status"]) } catch { self.error = error.localizedDescription }
    }

    func refreshSessions() async {
        loadingSessions = true
        defer { loadingSessions = false }
        do {
            let r = try await Backend.api(SessionsReport.self, ["sessions"])
            sessions = r.sessions
            sessionsWarning = r.warning
        } catch { self.error = error.localizedDescription }
    }

    var receivers: [AgentStatus] { (status?.agents ?? []).filter { $0.installed && $0.enabled && $0.direction != "send-only" } }

    /// Whether the user has connected any agent.
    var connected: Bool { status?.agents.contains { $0.hooks } ?? false }

    /// Installed, enabled agents still waiting to be connected, such as one
    /// installed after the others were connected.
    var unconnected: [AgentStatus] { (status?.agents ?? []).filter { $0.installed && $0.enabled && !$0.hooks } }

    /// Runs an action and surfaces any failure in the shared alert.
    func perform(_ action: () async throws -> Void) async {
        do { try await action() } catch { self.error = error.localizedDescription }
    }
}
