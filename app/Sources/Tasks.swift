import AppKit
import SwiftUI

struct BoardTask: Decodable, Identifiable {
    let id: String
    let project: String
    let title: String
    let detail: String
    let status: String
    let owner: String
    let note: String
    let updatedAt: String

    var folder: String { project.isEmpty ? "" : URL(fileURLWithPath: project).lastPathComponent }
    var statusName: String {
        switch status {
        case "open": return tr("待认领")
        case "claimed": return tr("进行中")
        case "done": return tr("已完成")
        default: return status
        }
    }
    var statusColor: Color {
        switch status {
        case "open": return .blue
        case "claimed": return .orange
        default: return .secondary
        }
    }
    /// "codex#1234" → "Codex"
    var ownerName: String {
        guard !owner.isEmpty else { return "" }
        return Names.agent(String(owner.split(separator: "#").first ?? ""))
    }
}

struct TasksReport: Decodable { let tasks: [BoardTask] }

struct TasksView: View {
    @EnvironmentObject var model: AppModel
    @AppStorage("projectDir") private var dir = ""
    @State private var tasks: [BoardTask] = []
    @State private var loading = true
    @State private var showDone = false
    @State private var title = ""
    @State private var detail = ""
    @State private var deleting: BoardTask?

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Text("同一项目里的 agent 共用这块看板：认领后其他 agent 不能再认领，只有认领者能更新或完成。新会话开始时会看到未完成的任务。")
                .font(.callout).foregroundStyle(.secondary).padding(12)
            addRow.padding(.horizontal, 12).padding(.bottom, 12)
            Divider()
            if loading {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if tasks.isEmpty {
                ContentUnavailableMessage(title: "看板上没有任务", detail: "在上方添加任务，或让 agent 用 bridge MCP 的 task_add 添加。")
            } else {
                Table(tasks) {
                    TableColumn("任务") { t in
                        VStack(alignment: .leading, spacing: 1) {
                            Text(t.title).lineLimit(1)
                            if !t.detail.isEmpty || !t.note.isEmpty {
                                Text([t.detail, t.note].filter { !$0.isEmpty }.joined(separator: " · "))
                                    .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                            }
                        }.help(t.title + (t.detail.isEmpty ? "" : "\n" + t.detail) + (t.note.isEmpty ? "" : "\n" + t.note))
                    }
                    TableColumn("项目") { Text($0.folder).lineLimit(1).help($0.project) }.width(110)
                    TableColumn("状态") { t in
                        HStack(spacing: 4) {
                            Circle().fill(t.statusColor).frame(width: 7, height: 7)
                            Text(t.statusName)
                        }
                    }.width(80)
                    TableColumn("认领者") { Text($0.ownerName).help($0.owner) }.width(100)
                    TableColumn("") { t in
                        HStack {
                            if t.status == "claimed" {
                                Button("退回") { Task { await act(["task-reopen", t.id]) } }
                                    .help(tr("认领它的会话已结束或卡住时，把任务放回待认领"))
                            }
                            Button(role: .destructive) { deleting = t } label: { Image(systemName: "trash") }
                                .buttonStyle(.borderless).help(tr("删除"))
                        }.controlSize(.small)
                    }.width(110)
                }
            }
        }
        .navigationTitle("任务看板")
        .toolbar {
            Toggle("显示已完成", isOn: $showDone).onChange(of: showDone) { _ in Task { await refresh() } }
            Button { Task { await refresh() } } label: { Label("刷新", systemImage: "arrow.clockwise") }
        }
        .confirmationDialog("删除这个任务？", isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } })) {
            Button("删除", role: .destructive) {
                if let t = deleting { Task { await act(["task-delete", t.id]) } }
            }
        } message: {
            Text(deleting?.title ?? "")
        }
        .task { await refresh() }
    }

    private var addRow: some View {
        HStack {
            Button { choose() } label: {
                Label(dir.isEmpty ? tr("选择项目…") : URL(fileURLWithPath: dir).lastPathComponent, systemImage: "folder")
            }.help(dir.isEmpty ? tr("任务属于某个项目") : dir)
            TextField("新任务标题", text: $title).textFieldStyle(.roundedBorder).onSubmit(add)
            TextField("说明（可选）", text: $detail).textFieldStyle(.roundedBorder).frame(maxWidth: 220).onSubmit(add)
            Button("添加", action: add).disabled(title.trimmingCharacters(in: .whitespaces).isEmpty)
        }
    }

    private func add() {
        let t = title.trimmingCharacters(in: .whitespaces)
        guard !t.isEmpty else { return }
        if dir.isEmpty { choose() }
        guard !dir.isEmpty else { return }
        Task {
            await act(["task-add", t, "--dir", dir] + (detail.isEmpty ? [] : ["--detail", detail]))
            title = ""
            detail = ""
        }
    }

    private func act(_ args: [String]) async {
        await model.perform { try await Backend.call(args) }
        await refresh()
    }

    private func refresh() async {
        loading = tasks.isEmpty
        defer { loading = false }
        await model.perform { tasks = try await Backend.api(TasksReport.self, ["tasks"] + (showDone ? ["--all"] : [])).tasks }
    }

    private func choose() {
        let p = NSOpenPanel()
        p.canChooseFiles = false
        p.canChooseDirectories = true
        p.prompt = tr("选择")
        if p.runModal() == .OK, let u = p.url { dir = u.path }
    }
}
