import AppKit
import SwiftUI

enum Page: String, CaseIterable, Identifiable {
    case overview = "概览", sessions = "会话迁移", files = "规则、Skill 与 MCP", claims = "文件认领", tasks = "任务看板", memory = "记忆", settings = "设置"
    var id: String { rawValue }
    var icon: String {
        switch self {
        case .overview: return "gauge.with.dots.needle.33percent"
        case .sessions: return "arrow.left.arrow.right"
        case .files: return "doc.on.doc"
        case .claims: return "person.badge.key"
        case .tasks: return "checklist"
        case .memory: return "brain"
        case .settings: return "gearshape"
        }
    }
}

struct RootView: View {
    @EnvironmentObject var model: AppModel
    @State var page: Page? = .overview

    var body: some View {
        NavigationSplitView {
            List(Page.allCases, selection: $page) { p in
                Label(LocalizedStringKey(p.rawValue), systemImage: p.icon).tag(p)
            }
            .navigationSplitViewColumnWidth(min: 170, ideal: 190)
        } detail: {
            switch page ?? .overview {
            case .overview: OverviewView()
            case .sessions: SessionsView()
            case .files: FilesView()
            case .claims: ClaimsView()
            case .tasks: TasksView()
            case .memory: MemoryView()
            case .settings: SettingsView()
            }
        }
        .task {
            try? await Backend.call(["ensure-daemon"])
            await model.refreshStatus()
        }
        .alert("操作失败", isPresented: Binding(get: { model.error != nil }, set: { if !$0 { model.error = nil } })) {
            Button("好") {}
        } message: {
            Text(model.error ?? "")
        }
    }
}

struct FileClaim: Decodable, Identifiable {
    let project: String
    let path: String
    let agent: String
    let session: String
    let claimedAt: String
    let expiresAt: String
    var id: String { project + path }
}

struct ClaimsReport: Decodable { let claims: [FileClaim] }

struct ClaimsView: View {
    @EnvironmentObject var model: AppModel
    @State private var claims: [FileClaim] = []
    @State private var loading = true

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Text("agent 的编辑工具会自动认领文件，冲突时按设置提醒或拦截。Shell 命令、手工编辑和未接入的 agent 不受保护。")
                .font(.callout).foregroundStyle(.secondary).padding(12)
            Divider()
            if loading {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if claims.isEmpty {
                ContentUnavailableMessage(title: "当前没有认领的文件", detail: "Agent 开始使用受支持的文件编辑工具后，会显示在这里。")
            } else {
                Table(claims) {
                    TableColumn("文件") { Text($0.path).lineLimit(1).help($0.path) }
                    TableColumn("Agent") { Text(Names.agent($0.agent)) }.width(100)
                    TableColumn("会话") { Text(String($0.session.prefix(8))) }.width(80)
                    TableColumn("认领时间") { Text($0.claimedAt) }.width(150)
                }
            }
        }
        .navigationTitle("文件认领")
        .toolbar { Button { Task { await refresh() } } label: { Label("刷新", systemImage: "arrow.clockwise") } }
        .task { await refresh() }
    }

    private func refresh() async {
        loading = true
        defer { loading = false }
        await model.perform { claims = try await Backend.api(ClaimsReport.self, ["claims"]).claims }
    }
}

struct StateDot: View {
    let on: Bool
    var body: some View { Circle().fill(on ? Color.green : Color.secondary.opacity(0.5)).frame(width: 8, height: 8) }
}

struct OverviewView: View {
    @EnvironmentObject var model: AppModel
    @State private var busy = false
    @State private var confirmConnect = false
    @State private var confirmRemove = false
    @State private var checks: [DoctorCheck] = []

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                banner
                ForEach(model.status?.agents ?? []) { AgentCard(agent: $0) }
                if !checks.isEmpty { checksList }
            }
            .padding(24)
            .frame(maxWidth: 760, alignment: .leading)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .navigationTitle("概览")
        .toolbar {
            Button { Task { await model.refreshStatus() } } label: { Label("刷新", systemImage: "arrow.clockwise") }
            Button("运行自检") {
                Task { await model.perform { checks = try await Backend.api(DoctorReport.self, ["doctor"]).checks } }
            }
        }
        .confirmationDialog("接入已安装的 agent？", isPresented: $confirmConnect) {
            Button("取消", role: .cancel) {}
            Button("接入") { Task { await runCLI(["init", "--yes"]) } }
        } message: {
            Text("将修改 Claude Code 和 Codex 的 hooks 与 MCP 配置（修改前自动备份），并注册开机自启的后台服务。之后请不要移动本应用。")
        }
        .confirmationDialog("断开所有 agent？", isPresented: $confirmRemove) {
            Button("取消", role: .cancel) {}
            Button("断开并还原", role: .destructive) { Task { await runCLI(["uninstall", "--yes"]) } }
        } message: {
            Text("将移除 hooks、MCP 配置、软链接和后台服务。用户改动过的文件会保留，已索引的数据不会删除。")
        }
    }

    private var banner: some View {
        HStack(spacing: 14) {
            StateDot(on: model.status?.daemon ?? false)
            VStack(alignment: .leading, spacing: 2) {
                Text(model.status?.daemon == true ? "后台同步运行中" : "后台同步未运行").font(.headline)
                Text(model.connected ? "已接入，新会话和进展会自动同步。" : "接入后，agent 之间才会自动同步进展和记忆。")
                    .font(.callout).foregroundStyle(.secondary)
            }
            Spacer()
            if busy { ProgressView().controlSize(.small) }
            if model.connected {
                Button("断开…") { confirmRemove = true }.disabled(busy)
            } else {
                Button("接入 agent…") { confirmConnect = true }.buttonStyle(.borderedProminent).disabled(busy)
            }
        }
        .padding(16)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 10))
    }

    private var checksList: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("自检结果").font(.headline)
            ForEach(checks) { c in
                Label(c.name, systemImage: c.ok ? "checkmark.circle.fill" : "xmark.circle.fill")
                    .foregroundStyle(c.ok ? Color.green : Color.red)
            }
        }
    }

    private func runCLI(_ args: [String]) async {
        busy = true
        defer { busy = false }
        await model.perform { _ = try await Backend.run(args) }
        // launchd needs a moment to bring the daemon up.
        try? await Task.sleep(nanoseconds: 800_000_000)
        await model.refreshStatus()
    }
}

struct AgentCard: View {
    @EnvironmentObject var model: AppModel
    let agent: AgentStatus
    @State private var busy = false
    @State private var confirmApprove = false

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(agent.displayName).font(.title3.weight(.semibold))
                if !agent.version.isEmpty { Text(agent.version).font(.callout).foregroundStyle(.secondary) }
                Spacer()
                Text(badge).font(.caption).padding(.horizontal, 8).padding(.vertical, 3)
                    .background(.quaternary, in: Capsule())
            }
            if agent.installed {
                versionNotice
                if agent.hooks && !agent.hooksTrusted {
                    Label("无法确认 hooks 的信任状态；请打开 \(agent.displayName)，审核并信任 bridge 的 hooks。", systemImage: "exclamationmark.triangle")
                        .foregroundStyle(.orange).font(.callout)
                }
                Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 4) {
                    row("会话迁移", agent.handoff)
                    row("实时协作", agent.live)
                    row("记忆注入", agent.memory)
                    row("已索引", tr("%lld 个会话，%lld 条事件", agent.sessions, agent.events))
                    if !agent.last.isEmpty { row("最近活动", agent.last) }
                }
                .font(.callout)
            }
        }
        .padding(16)
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(.quaternary))
    }

    @ViewBuilder private var versionNotice: some View {
        if !agent.writable {
            VStack(alignment: .leading, spacing: 8) {
                Label(agent.versionState == "failed" ? "此版本未通过自动检测，迁移会生成交接文档。" : "此版本尚未验证，迁移会生成交接文档。", systemImage: "exclamationmark.triangle")
                    .foregroundStyle(.orange).font(.callout)
                if !agent.versionError.isEmpty {
                    Text(agent.versionError).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                }
                HStack {
                    Button("重新检测") { act(["verify-version", agent.id]) }.disabled(busy)
                    Button("仍然启用…") { confirmApprove = true }.disabled(busy)
                    if busy { ProgressView().controlSize(.small) }
                }
            }
            .confirmationDialog("仍然启用 \(agent.displayName) \(agent.version) 的原生写入？", isPresented: $confirmApprove) {
                Button("取消", role: .cancel) {}
                Button("仍然启用", role: .destructive) { act(["approve-version", agent.id]) }
            } message: {
                Text("这个版本没有通过验证，写入的会话可能无法打开。写入不影响原会话，失败时会尝试回滚。只对当前这个版本生效。")
            }
        } else if agent.versionState == "approved" {
            HStack {
                Label("此版本由你手动启用，未通过验证。", systemImage: "hand.raised").font(.callout).foregroundStyle(.secondary)
                Button("撤销") { act(["revoke-version", agent.id]) }.disabled(busy)
            }
        } else if agent.versionState == "verified" {
            Label("此版本已通过本机自动检测。", systemImage: "checkmark.seal").font(.callout).foregroundStyle(.secondary)
        }
    }

    private func act(_ args: [String]) {
        Task {
            busy = true
            defer { busy = false }
            await model.perform { _ = try await Backend.run(["api"] + args) }
            await model.refreshStatus()
        }
    }

    private var badge: String {
        if !agent.installed { return tr("未安装") }
        if !agent.enabled { return tr("已停用") }
        if agent.hooks && !agent.hooksTrusted { return Names.direction(agent.direction) + " · " + tr("信任待确认") }
        return Names.direction(agent.direction) + " · " + (agent.hooks ? tr("已接入") : tr("未接入"))
    }

    private func row(_ k: String, _ v: String) -> some View {
        GridRow {
            Text(LocalizedStringKey(k)).foregroundStyle(.secondary)
            Text(v).textSelection(.enabled)
        }
    }
}

struct SessionsView: View {
    @EnvironmentObject var model: AppModel
    @State private var query = ""
    @State private var agent = "全部"
    @State private var selection: SessionRow.ID?
    @State private var migrating: SessionRow?
    @State private var syncing = false
    @State private var syncMessage: String?

    private var rows: [SessionRow] {
        model.sessions.filter { s in
            (agent == "全部" || s.agent == agent) &&
                (query.isEmpty || s.title.localizedCaseInsensitiveContains(query) || s.cwd.localizedCaseInsensitiveContains(query))
        }
    }

    var body: some View {
        VStack(spacing: 0) {
            if !model.sessionsWarning.isEmpty {
                Label("部分会话未能读取：\(model.sessionsWarning)", systemImage: "exclamationmark.triangle")
                    .foregroundStyle(.orange).textSelection(.enabled).padding(10)
            }
        Group {
            if model.sessions.isEmpty && model.loadingSessions {
                ProgressView("正在索引会话，首次可能需要一分钟…").frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if model.sessions.isEmpty {
                ContentUnavailableMessage(title: "还没有会话", detail: "使用 Claude Code 或 Codex 后，会话会出现在这里。")
            } else {
                Table(rows, selection: $selection) {
                    TableColumn("标题") { Text($0.displayTitle).lineLimit(1).help($0.displayTitle) }
                    TableColumn("Agent") { Text($0.agentName) }.width(90)
                    TableColumn("项目") { Text($0.folder).lineLimit(1).help($0.cwd) }.width(150)
                    TableColumn("同步") { s in Text(s.syncLabel).foregroundStyle(s.needsSync ? Color.orange : Color.secondary) }.width(70)
                    TableColumn("最近活动") { Text($0.last) }.width(120)
                }
            }
        }
        }
        .navigationTitle("会话迁移")
        .searchable(text: $query, prompt: "搜索标题或目录")
        .toolbar {
            Picker("Agent", selection: $agent) {
                Text("全部").tag("全部")
                ForEach(model.status?.agents ?? []) { Text($0.displayName).tag($0.id) }
            }
            Button { Task { await model.refreshSessions() } } label: { Label("刷新", systemImage: "arrow.clockwise") }
                .disabled(model.loadingSessions)
            if let s = model.sessions.first(where: { $0.id == selection }), s.needsSync {
                if s.diverged {
                    Menu("两边都有更新") {
                        Button("以 \(Names.agent(s.agent)) 为准") { Task { await sync(s, prefer: s.agent) } }
                        Button("以 \(Names.agent(s.syncOther)) 为准") { Task { await sync(s, prefer: s.syncOther) } }
                    }.disabled(syncing)
                } else {
                    Button("更新到最新") { Task { await sync(s) } }.disabled(syncing)
                }
            }
            Button("迁移…") { migrating = model.sessions.first { $0.id == selection } }
                .buttonStyle(.borderedProminent)
                .disabled(selection == nil)
        }
        .task { if model.sessions.isEmpty { await model.refreshSessions() } }
        .sheet(item: $migrating) { HandoffSheet(session: $0) }
        .alert("会话同步", isPresented: Binding(get: { syncMessage != nil }, set: { if !$0 { syncMessage = nil } })) {
            Button("好") {}
        } message: { Text(syncMessage ?? "") }
    }

    private func sync(_ s: SessionRow, prefer: String = "") async {
        guard !syncing else { return }
        syncing = true
        defer { syncing = false }
        await model.perform {
            syncMessage = try await Backend.api(SyncSessionResult.self, ["sync-session", s.id] + (prefer.isEmpty ? [] : ["--prefer", prefer])).message
        }
        await model.refreshSessions()
    }
}

struct ContentUnavailableMessage: View {
    let title: String
    let detail: String
    var body: some View {
        VStack(spacing: 6) {
            Text(LocalizedStringKey(title)).font(.title3.weight(.semibold))
            Text(LocalizedStringKey(detail)).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

struct HandoffSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    let session: SessionRow
    @State private var target = ""
    @State private var document = false
    @State private var busy = false
    @State private var result: HandoffResult?
    @State private var failure: String?
    @State private var duplicate: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("迁移会话").font(.title2.weight(.semibold))
            Text(session.displayTitle).lineLimit(2)
            Text("\(session.agentName) · \(session.cwd)").font(.callout).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
            if let r = result { resultView(r) } else { form }
            if let failure { Text(failure).foregroundStyle(.red).textSelection(.enabled) }
            if let duplicate { Text(duplicate).foregroundStyle(.orange) }
            HStack {
                Spacer()
                if result != nil {
                    Button("完成") { dismiss() }.keyboardShortcut(.defaultAction)
                } else {
                    Button("取消") { dismiss() }.keyboardShortcut(.cancelAction).disabled(busy)
                    if busy { ProgressView().controlSize(.small) }
                    Button(duplicate == nil ? "开始迁移" : "再次迁移") { Task { await start() } }.keyboardShortcut(.defaultAction).disabled(target.isEmpty || busy)
                }
            }
        }
        .padding(22)
        .frame(width: 500)
        .onAppear {
            target = model.receivers.first { $0.id != session.agent }?.id ?? ""
        }
    }

    private var form: some View {
        VStack(alignment: .leading, spacing: 10) {
            Picker("迁移到", selection: $target) {
                ForEach(model.receivers.filter { $0.id != session.agent }) { Text($0.displayName).tag($0.id) }
            }
            Toggle("生成交接文档，而不是原生会话", isOn: $document)
            Text("推理内容不会迁移，无法对应的工具调用会转成文本。原会话不受影响。").font(.callout).foregroundStyle(.secondary)
        }
    }

    private func resultView(_ r: HandoffResult) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Label(r.mode == "native" ? "已写入目标 agent" : "已生成交接文档", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
            Text("约 \(r.tokens) tokens · 省略推理 \(r.reasoning) 段 · 工具转文本 \(r.degraded) 个 · 压缩 \(r.summarized) 轮 · 截断 \(r.truncated) 项")
                .font(.callout).foregroundStyle(.secondary)
            if r.mode == "native" {
                Text(r.resumeCommand).font(.system(.callout, design: .monospaced)).textSelection(.enabled)
                    .padding(8).frame(maxWidth: .infinity, alignment: .leading)
                    .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
                Button("复制恢复命令") { copy(r.resumeCommand) }
            } else {
                Text(r.message).textSelection(.enabled)
                Button("在访达中显示") { NSWorkspace.shared.selectFile(r.documentPath, inFileViewerRootedAtPath: "") }
            }
        }
    }

    private func start() async {
        guard !busy else { return }
        busy = true
        failure = nil
        defer { busy = false }
        do {
            var args = ["handoff", session.id, "--to", target]
            if document { args.append("--document") }
            if duplicate != nil { args.append("--again") }
            let r = try await Backend.api(HandoffResult.self, args)
            if r.mode == "duplicate" {
                duplicate = r.message + tr("可以用已有的会话，或点“再次迁移”生成新的一份。")
                return
            }
            result = r
            await model.refreshSessions()
        } catch { failure = error.localizedDescription }
    }
}

func copy(_ s: String) {
    NSPasteboard.general.clearContents()
    NSPasteboard.general.setString(s, forType: .string)
}

struct FilesView: View {
    @EnvironmentObject var model: AppModel
    @AppStorage("projectDir") private var dir = ""
    @State private var actions: [SyncAction] = []
    @State private var busy = false
    @State private var notice = ""
    @State private var confirmForce = false

    private var changes: Int { actions.filter(\.willChange).count }
    private var attention: [SyncAction] { actions.filter(\.needsAttention) }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Image(systemName: "folder")
                Text(dir.isEmpty ? tr("仅全局规则与 skill") : dir).lineLimit(1).truncationMode(.middle)
                Spacer()
                Button("选择项目…") { choose() }
                if !dir.isEmpty { Button("清除") { dir = ""; reload() } }
            }
            .padding(12)
            Divider()
            if actions.isEmpty {
                ContentUnavailableMessage(title: "没有需要同步的内容", detail: "在项目根目录放一份 AGENTS.md，或在 .agents/skills 下添加 skill；MCP server 写在 .agents/mcp.toml。")
            } else {
                Table(actions) {
                    TableColumn("状态") { a in Text(a.kindName).foregroundStyle(a.needsAttention ? Color.orange : Color.primary) }.width(80)
                    TableColumn("Agent") { Text(Names.agent($0.agent)) }.width(90)
                    TableColumn("目标") { Text($0.displayTarget).lineLimit(1).truncationMode(.middle).help($0.displayTarget) }
                    TableColumn("说明") { Text($0.displayReason).lineLimit(1).help($0.displayReason) }
                }
            }
            if !attention.isEmpty || !notice.isEmpty {
                Divider()
                HStack {
                    Text(notice.isEmpty ? tr("有 %lld 项需要你决定，内容不会被自动覆盖。", attention.count) : notice).font(.callout)
                    Spacer()
                    if !attention.isEmpty {
                        Menu("把副本合回源") {
                            ForEach(Array(Set(attention.map(\.agent))).sorted(), id: \.self) { a in
                                Button(Names.agent(a)) { sync(["--adopt", a]) }
                            }
                        }.fixedSize()
                        Button("以源为准覆盖…") { confirmForce = true }
                    }
                }
                .padding(12)
            }
        }
        .navigationTitle("规则、Skill 与 MCP")
        .toolbar {
            if busy { ProgressView().controlSize(.small) }
            Button { reload() } label: { Label("刷新", systemImage: "arrow.clockwise") }
            Button(changes > 0 ? "同步 \(changes) 项" : "已是最新") { sync([]) }
                .buttonStyle(.borderedProminent).disabled(changes == 0 || busy)
        }
        .confirmationDialog("以源文件覆盖所有冲突项？", isPresented: $confirmForce) {
            Button("取消", role: .cancel) {}
            Button("覆盖", role: .destructive) { sync(["--force"]) }
        } message: {
            ForEachText(attention.map(\.displayTarget))
        }
        .task {
            // The remembered project may have been moved or deleted.
            if !dir.isEmpty && !FileManager.default.fileExists(atPath: dir) { dir = "" }
            reload()
        }
    }

    private func dirArgs() -> [String] { dir.isEmpty ? [] : ["--dir", dir] }

    private func reload() {
        Task { await model.perform { actions = try await Backend.api(PlanReport.self, ["plan"] + dirArgs()).actions } }
    }

    private func sync(_ extra: [String]) {
        Task {
            busy = true
            defer { busy = false }
            await model.perform {
                let r = try await Backend.api(SyncReport.self, ["sync"] + dirArgs() + extra)
                notice = tr("已同步 %lld 项", r.applied.count) + (r.blocked.isEmpty ? "" : tr("，%lld 项未处理", r.blocked.count))
                actions = try await Backend.api(PlanReport.self, ["plan"] + dirArgs()).actions
            }
        }
    }

    private func choose() {
        let p = NSOpenPanel()
        p.canChooseFiles = false
        p.canChooseDirectories = true
        p.prompt = tr("选择")
        if p.runModal() == .OK, let u = p.url {
            dir = u.path
            notice = ""
            reload()
        }
    }
}

struct ForEachText: View {
    let lines: [String]
    init(_ l: [String]) { lines = l }
    var body: some View { Text(tr("覆盖前会自动备份：") + "\n" + lines.joined(separator: "\n")) }
}

struct MemoryView: View {
    @EnvironmentObject var model: AppModel
    @AppStorage("projectDir") private var dir = ""
    @State private var items: [MemoryItem] = []
    @State private var filter = "active"
    @State private var selection: MemoryItem.ID?
    @State private var adding = false
    @State private var editing: MemoryItem?
    @State private var confirmForget = false

    private var rows: [MemoryItem] { items.filter { $0.status == filter } }
    private var pending: Int { items.filter { $0.status == "pending" }.count }

    var body: some View {
        Group {
            if rows.isEmpty {
                ContentUnavailableMessage(title: filter == "pending" ? "待审核区是空的" : "还没有记忆", detail: "可以手动添加，或在设置里开启自动提取。")
            } else {
                Table(rows, selection: $selection) {
                    TableColumn("内容") { Text($0.content).lineLimit(2).help($0.content) }
                    TableColumn("范围") { Text($0.scope == "global" ? "全局" : "项目") }.width(50)
                    TableColumn("类别") { Text($0.category) }.width(100)
                    TableColumn("置信度") { Text(String(format: "%.2f", $0.confidence)) }.width(60)
                }
            }
        }
        .navigationTitle("记忆")
        .toolbar {
            Picker("状态", selection: $filter) {
                Text("生效中").tag("active")
                Text(pending > 0 ? "待审核 (\(pending))" : "待审核").tag("pending")
                Text("已取代").tag("superseded")
            }.pickerStyle(.segmented)
            if filter == "pending" {
                Button("通过") { act("memory-approve") }.disabled(selection == nil)
            }
            Button("编辑…") { editing = rows.first { $0.id == selection } }
                .disabled(selection == nil || filter == "superseded")
            Button("删除…", role: .destructive) { confirmForget = true }.disabled(selection == nil)
            Button { adding = true } label: { Label("添加", systemImage: "plus") }
        }
        .confirmationDialog("删除这条记忆？", isPresented: $confirmForget) {
            Button("取消", role: .cancel) {}
            Button("删除", role: .destructive) { act("memory-forget") }
        } message: { Text("删除后不可恢复，并且不会再被自动提取。") }
        .sheet(isPresented: $adding, onDismiss: reload) { AddMemorySheet(dir: dir) }
        .sheet(item: $editing, onDismiss: reload) { EditMemorySheet(item: $0) }
        .task { reload() }
    }

    private func reload() {
        Task { await model.perform { items = try await Backend.api(MemoriesReport.self, ["memories"]).memories } }
    }

    private func act(_ command: String) {
        guard let id = selection else { return }
        Task {
            await model.perform {
                try await Backend.call([command, id])
                selection = nil
                items = try await Backend.api(MemoriesReport.self, ["memories"]).memories
            }
        }
    }
}

struct EditMemorySheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    let item: MemoryItem
    @State private var content = ""
    @State private var busy = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("编辑记忆").font(.title2.weight(.semibold))
            TextEditor(text: $content).frame(height: 120)
                .overlay(RoundedRectangle(cornerRadius: 5).strokeBorder(.quaternary))
            Text("保存后会更新共享记忆；范围和审核状态保持不变。")
                .font(.callout).foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("取消") { dismiss() }.keyboardShortcut(.cancelAction).disabled(busy)
                Button("保存") {
                    busy = true
                    Task {
                        await model.perform {
                            try await Backend.call(["memory-edit", item.id, content])
                            dismiss()
                        }
                        busy = false
                    }
                }.keyboardShortcut(.defaultAction)
                    .disabled(busy || content.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }.padding(22).frame(width: 440)
            .onAppear { content = item.content }
    }
}

struct AddMemorySheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    let dir: String
    @State private var content = ""
    @State private var scope = "global"

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("添加记忆").font(.title2.weight(.semibold))
            TextEditor(text: $content).font(.body).frame(height: 90).overlay(RoundedRectangle(cornerRadius: 5).strokeBorder(.quaternary))
            Picker("范围", selection: $scope) {
                Text("全局偏好").tag("global")
                Text("当前项目").tag("project")
            }.pickerStyle(.segmented)
            if scope == "project" {
                Text(dir.isEmpty ? tr("请先在“规则、Skill 与 MCP”页选择项目。") : dir).font(.callout).foregroundStyle(.secondary).lineLimit(1)
            }
            HStack {
                Spacer()
                Button("取消") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("添加") { save() }.keyboardShortcut(.defaultAction)
                    .disabled(content.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || (scope == "project" && dir.isEmpty))
            }
        }
        .padding(22)
        .frame(width: 440)
    }

    private func save() {
        Task {
            await model.perform {
                var args = ["memory-add", content, "--scope", scope]
                if !dir.isEmpty { args += ["--dir", dir] }
                try await Backend.call(args)
                dismiss()
            }
        }
    }
}

struct SettingsView: View {
    @AppStorage("appLanguage") private var language = "system"
    @EnvironmentObject var model: AppModel
    @State private var s: Settings?
    @State private var newExclude = ""
    @State private var confirmExtract = false

    var body: some View {
        Group {
            if let cfg = s { form(cfg) } else { ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity) }
        }
        .navigationTitle("设置")
        .task { await load() }
        .confirmationDialog("开启记忆自动提取？", isPresented: $confirmExtract) {
            Button("取消", role: .cancel) {}
            Button("开启") { set("memory.auto_extract", "true") }
        } message: {
            Text("开启后，脱敏后的对话文本会发送给 \(Names.agent(s?.extractor ?? "")) 背后的模型服务，并消耗你的额度。工具输出不会发送。")
        }
    }

    private func form(_ c: Settings) -> some View {
        Form {
            Section("语言") {
                Picker("界面语言", selection: Binding(get: { language }, set: setLanguage)) {
                    Text("跟随系统").tag("system")
                    Text("简体中文").tag("zh-Hans")
                    Text("English").tag("en")
                }
                Text("重新打开 Bridge 后生效。").font(.callout).foregroundStyle(.secondary)
            }
            Section("同步内容") {
                toggle("会话", c.sessions, "sync.sessions")
                toggle("记忆", c.memory, "sync.memory")
                toggle("全局个人偏好", c.globalMemory, "sync.global_memory").disabled(!c.memory)
                toggle("项目记忆", c.projectMemory, "sync.project_memory").disabled(!c.memory)
                toggle("规则文件", c.rules, "sync.rules")
                toggle("Skill", c.skills, "sync.skills.enabled")
                toggle("MCP 配置", c.mcpConfigs, "sync.mcp_configs")
                if c.mcpConfigs {
                    Text("MCP server 以 ~/.config/bridge/mcp.toml（全局）和项目的 .agents/mcp.toml 为源，写入各 agent 自己的配置。已有的同名配置不会被覆盖。")
                        .font(.callout).foregroundStyle(.secondary)
                }
            }
            Section("Agent") {
                ForEach(model.status?.agents.filter(\.installed) ?? []) { a in
                    HStack {
                        Toggle(a.displayName, isOn: Binding(get: { c.enabled.contains(a.id) }, set: { on in setEnabled(a.id, on, c) }))
                        Spacer()
                        Picker("方向", selection: Binding(get: { c.directions[a.id] ?? "both" }, set: { set("agents.\(a.id).direction", tomlString($0)) })) {
                            Text("双向").tag("both")
                            Text("只发送").tag("send-only")
                            Text("只接收").tag("receive-only")
                        }.labelsHidden().fixedSize().disabled(!c.enabled.contains(a.id))
                    }
                }
            }
            Section("实时协作") {
                toggle("每次发消息时注入其他 agent 的进展", c.injectOnPrompt, "live.inject_on_prompt")
                Picker("文件冲突时", selection: Binding(get: { c.conflictPolicy }, set: { set("live.conflict_policy", tomlString($0)) })) {
                    Text("提醒，仍允许编辑").tag("warn")
                    Text("拦截编辑").tag("block")
                }
                Stepper("注入上限：\(c.injectTokenBudget) tokens", value: Binding(get: { c.injectTokenBudget }, set: { set("live.inject_token_budget", String($0)) }), in: 100...8000, step: 100)
            }
            Section("迁移") {
                Stepper("保留最近 \(c.keepRecentTurns) 轮原文", value: Binding(get: { c.keepRecentTurns }, set: { set("handoff.keep_recent_turns", String($0)) }), in: 1...200)
            }
            Section("记忆提取") {
                Toggle("自动提取记忆（会联网）", isOn: Binding(get: { c.autoExtract }, set: { on in
                    if on { confirmExtract = true } else { set("memory.auto_extract", "false") }
                }))
                Picker("提取方式", selection: Binding(get: { c.extractor }, set: { set("memory.extractor", tomlString($0)) })) {
                    Text("Claude Code").tag("claude-code")
                    Text("Codex").tag("codex")
                }
            }
            Section {
                ForEach(c.excludeProjects, id: \.self) { p in
                    HStack {
                        Text(p).font(.system(.body, design: .monospaced))
                        Spacer()
                        Button("移除") { set("privacy.exclude_projects", tomlArray(c.excludeProjects.filter { $0 != p })) }
                    }
                }
                HStack {
                    TextField("路径规则，例如 ~/work/**", text: $newExclude)
                    Button("添加") {
                        set("privacy.exclude_projects", tomlArray(c.excludeProjects + [newExclude]))
                        newExclude = ""
                    }.disabled(newExclude.isEmpty || c.excludeProjects.contains(newExclude))
                }
            } header: {
                Text("隐私：排除的项目")
            } footer: {
                Text("这些路径下的会话和记忆完全不采集。").font(.callout).foregroundStyle(.secondary)
            }
            Section("仅包含的项目") {
                PatternList(values: c.includeProjects, placeholder: "例如 ~/work/**", onSave: { set("privacy.include_projects", tomlArray($0)) })
                Text("留空时不限制项目；排除规则仍然生效。") .font(.callout).foregroundStyle(.secondary)
            }
            Section("排除的文件") {
                PatternList(values: c.excludeFiles, placeholder: "例如 **/private/**", onSave: { set("sync.exclude_files", tomlArray($0)) })
            }
            Section("排除的 Skill") {
                PatternList(values: c.excludeSkills, placeholder: "例如 private-*", onSave: { set("sync.skills.exclude", tomlArray($0)) })
            }
            if c.mcpConfigs {
                Section("排除的 MCP server") {
                    PatternList(values: c.mcpExclude, placeholder: "例如 node_repl", onSave: { set("sync.mcp_exclude", tomlArray($0)) })
                }
            }
            Section("自定义脱敏规则") {
                PatternList(values: c.redact, placeholder: "正则表达式", onSave: { set("privacy.redact", tomlArray($0)) })
                Text("使用 Go 正则表达式；内置密钥脱敏规则始终生效。") .font(.callout).foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
    }

    private func setLanguage(_ value: String) {
        language = value
        if value == "system" {
            UserDefaults.standard.removeObject(forKey: "AppleLanguages")
        } else {
            UserDefaults.standard.set([value], forKey: "AppleLanguages")
        }
    }

    private func toggle(_ title: String, _ value: Bool, _ key: String) -> some View {
        Toggle(LocalizedStringKey(title), isOn: Binding(get: { value }, set: { set(key, $0 ? "true" : "false") }))
    }

    private func setEnabled(_ id: String, _ on: Bool, _ c: Settings) {
        let next = on ? c.enabled + [id] : c.enabled.filter { $0 != id }
        set("agents.enabled", tomlArray(next))
    }

    private func load() async {
        await model.perform { s = try await Backend.api(Settings.self, ["config"]) }
    }

    private func set(_ key: String, _ value: String) {
        Task {
            await model.perform { try await Backend.call(["config-set", "--key", key, "--value", value]) }
            await load()
            await model.refreshStatus()
        }
    }
}

struct PatternList: View {
    let values: [String]
    let placeholder: String
    let onSave: ([String]) -> Void
    @State private var input = ""

    var body: some View {
        ForEach(Array(values.enumerated()), id: \.offset) { index, value in
            HStack {
                Text(value).font(.system(.body, design: .monospaced)).textSelection(.enabled)
                Spacer()
                Button("移除") { var next = values; next.remove(at: index); onSave(next) }
            }
        }
        HStack {
            TextField(LocalizedStringKey(placeholder), text: $input)
            Button("添加") { onSave(values + [input]); input = "" }
                .disabled(input.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || values.contains(input))
        }
    }
}
