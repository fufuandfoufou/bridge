import AppKit
import SwiftUI

enum MenuBarIcon {
    /// A template image, so the system tints it for the menu bar's appearance.
    static let image: NSImage = {
        let url = Bundle.main.url(forResource: "MenuBarIcon", withExtension: "png")
        let image = url.flatMap(NSImage.init(contentsOf:)) ?? NSImage(systemSymbolName: "circle.hexagongrid", accessibilityDescription: "Bridge")!
        image.size = NSSize(width: 18, height: 18)
        image.isTemplate = true
        return image
    }()
}

struct MenuPanel: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.openWindow) private var openWindow
    @AppStorage("projectDir") private var dir = ""
    @State private var busy = false
    @State private var notice = ""
    @State private var copyable = ""
    @State private var repeatable: (SessionRow, AgentStatus)?

    private var recent: [SessionRow] { Array(model.sessions.prefix(4)) }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            Divider()
            agents
            Divider()
            sessions
            if !notice.isEmpty { noticeRow }
            Divider()
            actions
        }
        .padding(12)
        .frame(width: 340)
        .task {
            try? await Backend.call(["ensure-daemon"])
            await model.refreshStatus()
            await model.refreshSessions()
        }
    }

    private var statusText: String {
        if model.status?.paused == true { return tr("同步已暂停") }
        return model.status?.daemon == true ? tr("后台同步运行中") : tr("后台同步未运行")
    }

    private var statusColor: Color {
        if model.status?.paused == true { return .orange }
        return model.status?.daemon == true ? .green : .secondary
    }

    private var header: some View {
        HStack(spacing: 8) {
            Image(nsImage: MenuBarIcon.image)
            Text("Bridge").font(.headline)
            Spacer()
            Circle().fill(statusColor).frame(width: 8, height: 8)
            Text(statusText).font(.callout).foregroundStyle(.secondary)
        }
    }

    private var agents: some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach((model.status?.agents ?? []).filter(\.installed)) { a in
                HStack {
                    Text(a.displayName)
                    Text(a.version).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    Spacer()
                    Text(summary(a)).font(.caption).foregroundStyle(needsAttention(a) ? Color.orange : Color.secondary)
                }
            }
        }
    }

    private func needsAttention(_ a: AgentStatus) -> Bool { unverified(a) || (a.hooks && !a.hooksTrusted) }

    private func unverified(_ a: AgentStatus) -> Bool { !a.writable && a.nativeWrite != false }

    private func summary(_ a: AgentStatus) -> String {
        if a.hooks && !a.hooksTrusted { return tr("hooks 信任待确认") }
        if unverified(a) { return tr("版本未验证") }
        if a.nativeWrite == false { return tr("只读取会话") }
        return a.hooks ? tr("已接入") : tr("未接入")
    }

    private var sessions: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("最近会话").font(.caption).foregroundStyle(.secondary)
                if model.loadingSessions { ProgressView().controlSize(.mini) }
            }
            if !model.sessionsWarning.isEmpty {
                Label("部分会话未能读取：\(model.sessionsWarning)", systemImage: "exclamationmark.triangle")
                    .font(.caption).foregroundStyle(.orange).textSelection(.enabled)
            }
            if recent.isEmpty && !model.loadingSessions {
                Text("还没有会话").font(.callout).foregroundStyle(.secondary)
            }
            ForEach(recent) { s in
                HStack(spacing: 8) {
                    VStack(alignment: .leading, spacing: 1) {
                        Text(s.displayTitle).lineLimit(1).truncationMode(.tail)
                        Text("\(s.agentName) · \(s.folder)").font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    Spacer(minLength: 4)
                    if s.diverged {
                        Menu("两边都有更新") {
                            Button("以 \(Names.agent(s.agent)) 为准") { syncSession(s, prefer: s.agent) }
                            Button("以 \(Names.agent(s.syncOther)) 为准") { syncSession(s, prefer: s.syncOther) }
                        }.controlSize(.small).disabled(busy).fixedSize()
                    } else if s.needsSync {
                        Button("更新到最新") { syncSession(s) }.controlSize(.small).disabled(busy).fixedSize()
                    } else if let target = model.receivers.first(where: { $0.id != s.agent }) {
                        Button("迁移到 \(target.displayName)") { handoff(s, target) }
                            .controlSize(.small).disabled(busy).fixedSize()
                    }
                }
            }
        }
    }

    private var noticeRow: some View {
        HStack {
            Text(notice).font(.caption).foregroundStyle(.secondary).lineLimit(3).textSelection(.enabled)
            Spacer()
            if let (s, t) = repeatable {
                Button("再次迁移") { handoff(s, t, again: true) }.controlSize(.small).fixedSize().disabled(busy)
            }
            if !copyable.isEmpty {
                Button("复制恢复命令") { copy(copyable) }.controlSize(.small).fixedSize()
            }
        }
    }

    private var actions: some View {
        VStack(alignment: .leading, spacing: 2) {
            if model.status?.paused == true {
                MenuRow("恢复同步", "play.fill") { togglePause("resume") }
            } else {
                MenuRow("暂停同步", "pause.fill") { togglePause("pause") }
            }
            MenuRow(dir.isEmpty ? tr("同步全局规则与 Skill") : tr("同步 %@ 的规则与 Skill", URL(fileURLWithPath: dir).lastPathComponent), "arrow.triangle.2.circlepath") { syncFiles() }
            MenuRow("打开 Bridge", "macwindow") {
                openWindow(id: "main")
                NSApp.activate(ignoringOtherApps: true)
            }
            Divider().padding(.vertical, 4)
            MenuRow("退出界面", "power") { NSApp.terminate(nil) }
            Text("退出界面后，后台同步仍会继续。").font(.caption2).foregroundStyle(.tertiary).padding(.leading, 6)
        }
        .disabled(busy)
    }

    private func report(_ text: String, copy command: String = "") {
        notice = text
        copyable = command
    }

    private func handoff(_ s: SessionRow, _ target: AgentStatus, again: Bool = false) {
        guard !busy else { return }
        busy = true
        repeatable = nil
        report(tr("正在迁移到 %@…", target.displayName))
        Task {
            defer { busy = false }
            do {
                let r = try await Backend.api(HandoffResult.self, ["handoff", s.id, "--to", target.id] + (again ? ["--again"] : []))
                if r.mode == "duplicate" {
                    report(r.message, copy: r.resumeCommand)
                    repeatable = (s, target)
                } else if r.mode == "native" {
                    report(tr("已迁移到 %@", target.displayName), copy: r.resumeCommand)
                } else {
                    report(tr("已生成交接文档：%@", r.documentPath))
                }
                await model.refreshSessions()
            } catch { report(error.localizedDescription) }
        }
    }

    private func syncSession(_ s: SessionRow, prefer: String = "") {
        guard !busy else { return }
        busy = true
        repeatable = nil
        report(tr("正在更新…"))
        Task {
            defer { busy = false }
            do {
                report(try await Backend.api(SyncSessionResult.self, ["sync-session", s.id] + (prefer.isEmpty ? [] : ["--prefer", prefer])).message)
                await model.refreshSessions()
            } catch { report(error.localizedDescription) }
        }
    }

    private func togglePause(_ command: String) {
        Task {
            await model.perform { try await Backend.call([command]) }
            await model.refreshStatus()
            report(command == "pause" ? tr("已暂停：不采集、不注入、不同步文件") : tr("已恢复同步"))
        }
    }

    private func syncFiles() {
        Task {
            busy = true
            defer { busy = false }
            do {
                let r = try await Backend.api(SyncReport.self, ["sync"] + (dir.isEmpty ? [] : ["--dir", dir]))
                report(tr("已同步 %lld 项", r.applied.count) + (r.blocked.isEmpty ? "" : tr("，%lld 项需要在主窗口处理", r.blocked.count)))
            } catch { report(error.localizedDescription) }
        }
    }
}

struct MenuRow: View {
    let title: String
    let icon: String
    let action: () -> Void
    @State private var hovering = false

    init(_ title: String, _ icon: String, action: @escaping () -> Void) {
        self.title = title
        self.icon = icon
        self.action = action
    }

    var body: some View {
        Button(action: action) {
            HStack(spacing: 8) {
                Image(systemName: icon).frame(width: 16)
                Text(LocalizedStringKey(title)).lineLimit(1)
                Spacer()
            }
            .padding(.horizontal, 6)
            .padding(.vertical, 4)
            .contentShape(Rectangle())
            .background(hovering ? Color.primary.opacity(0.08) : .clear, in: RoundedRectangle(cornerRadius: 5))
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
    }
}
