import AppKit
import SwiftUI

struct WorktreeActivity: Decodable {
    let agent: String
    let title: String
    let last: String
    let instruction: String
}

struct WorktreeRow: Decodable, Identifiable {
    let path: String
    let branch: String
    let main: Bool
    let agent: String?
    let ahead: Int
    let behind: Int
    let changed: Int
    let subject: String
    let missing: Bool
    let activity: WorktreeActivity?

    var id: String { path }
    var folder: String { URL(fileURLWithPath: path).lastPathComponent }
}

struct WorktreesReport: Decodable {
    let main: String
    let worktrees: [WorktreeRow]
}

struct WorktreeAdded: Decodable { let start: String }

/// Optional worktree mode: each agent works in its own checkout and branch,
/// so they never overwrite each other's files.
struct WorktreesView: View {
    @EnvironmentObject var model: AppModel
    @AppStorage("projectDir") private var dir = ""
    @State private var report: WorktreesReport?
    @State private var loading = false
    @State private var notice = ""
    @State private var removing: WorktreeRow?

    private var agentsWithout: [AgentStatus] {
        let taken = Set((report?.worktrees ?? []).compactMap(\.agent))
        return (model.status?.agents ?? []).filter { $0.installed && $0.enabled && !taken.contains($0.id) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Image(systemName: "folder")
                Text(dir.isEmpty ? tr("请选择一个 git 项目") : dir).lineLimit(1).truncationMode(.middle)
                Spacer()
                Button("选择项目…") { choose() }
            }
            .padding(12)
            Text("每个 agent 在自己的 worktree 和分支上工作，互不覆盖文件；进度、记忆和任务看板仍然共享。完成后把分支合并回来即可。")
                .font(.callout).foregroundStyle(.secondary).padding(.horizontal, 12).padding(.bottom, 10)
            Divider()
            if dir.isEmpty {
                ContentUnavailableMessage(title: "还没有选择项目", detail: "选择一个 git 仓库后，可以为每个 agent 创建独立的 worktree。")
            } else if loading && report == nil {
                ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if let r = report {
                List {
                    ForEach(r.worktrees) { w in row(w, main: r.main) }
                }
            }
            if !notice.isEmpty {
                Divider()
                HStack {
                    Text(notice).font(.callout).textSelection(.enabled).lineLimit(3)
                    Spacer()
                    Button("复制") { copy(notice.components(separatedBy: "\n").last ?? notice) }
                }.padding(12)
            }
        }
        .navigationTitle("Worktree")
        .toolbar {
            if !dir.isEmpty {
                Menu {
                    ForEach(agentsWithout) { a in
                        Button(a.displayName) { add(a) }
                    }
                } label: { Label("为 agent 创建 worktree", systemImage: "plus") }
                .disabled(agentsWithout.isEmpty)
            }
            Button { Task { await reload() } } label: { Label("刷新", systemImage: "arrow.clockwise") }
        }
        .confirmationDialog("删除这个 worktree？", isPresented: Binding(get: { removing != nil }, set: { if !$0 { removing = nil } })) {
            if let w = removing {
                if w.changed > 0 {
                    Button("仍然删除（丢弃未提交的改动）", role: .destructive) { remove(w, force: true) }
                } else {
                    Button("删除", role: .destructive) { remove(w, force: false) }
                }
            }
        } message: {
            if let w = removing {
                Text(w.changed > 0 ? tr("%@ 有 %lld 个未提交的改动，删除后会丢失。分支 %@ 和已提交的内容会保留。", w.path, w.changed, w.branch)
                     : tr("会删除目录 %@。分支 %@ 和已提交的内容会保留。", w.path, w.branch))
            }
        }
        .task(id: dir) { await reload() }
    }

    private func row(_ w: WorktreeRow, main: String) -> some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: w.main ? "house" : "arrow.triangle.branch").foregroundStyle(.secondary).frame(width: 18)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(w.branch.isEmpty ? tr("（分离的 HEAD）") : w.branch).font(.headline)
                    if let a = w.agent { Text(Names.agent(a)).font(.caption).padding(.horizontal, 6).padding(.vertical, 1).background(.quaternary, in: Capsule()) }
                    if w.main { Text("主目录").font(.caption).foregroundStyle(.secondary) }
                }
                Text(w.path).font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                if w.missing {
                    Label("目录已不存在", systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange)
                } else {
                    HStack(spacing: 10) {
                        if !w.main { Text(tr("领先 %lld · 落后 %lld", w.ahead, w.behind)) }
                        Text(w.changed > 0 ? tr("%lld 个未提交的改动", w.changed) : tr("没有未提交的改动"))
                            .foregroundStyle(w.changed > 0 ? Color.orange : Color.secondary)
                        Text(w.subject).lineLimit(1).foregroundStyle(.secondary)
                    }.font(.caption)
                }
                if let a = w.activity {
                    Text("\(Names.agent(a.agent)) · \(a.instruction.isEmpty ? a.title : a.instruction)")
                        .font(.caption).lineLimit(1).help(a.instruction)
                }
            }
            Spacer()
            if !w.main {
                VStack(alignment: .trailing, spacing: 4) {
                    if let a = w.agent, !w.missing {
                        Button("复制启动命令") { copy(startCommand(a, w.path)) }.controlSize(.small)
                    }
                    if w.ahead > 0 {
                        Button("复制合并命令") { copy("git -C \(shellQuote(main)) merge \(w.branch)") }.controlSize(.small)
                    }
                    Button("删除…", role: .destructive) { removing = w }.controlSize(.small)
                }
            }
        }
        .padding(.vertical, 4)
    }

    private func startCommand(_ agent: String, _ path: String) -> String {
        let bin = ["claude-code": "claude", "codex": "codex", "pi": "pi", "dsh": "dsh web"][agent]
        return "cd \(shellQuote(path))" + (bin.map { " && \($0)" } ?? "")
    }

    private func shellQuote(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }

    private func reload() async {
        guard !dir.isEmpty else { report = nil; return }
        loading = true
        defer { loading = false }
        do { report = try await Backend.api(WorktreesReport.self, ["worktrees", "--dir", dir]) } catch {
            report = nil
            notice = error.localizedDescription
        }
    }

    private func add(_ a: AgentStatus) {
        Task {
            await model.perform {
                let r = try await Backend.api(WorktreeAdded.self, ["worktree-add", a.id, "--dir", dir])
                notice = tr("已为 %@ 创建 worktree，在终端里运行：", a.displayName) + "\n" + r.start
            }
            await reload()
        }
    }

    private func remove(_ w: WorktreeRow, force: Bool) {
        Task {
            await model.perform { try await Backend.call(["worktree-remove", w.path, "--dir", dir] + (force ? ["--force"] : [])) }
            notice = ""
            await reload()
        }
    }

    private func choose() {
        let p = NSOpenPanel()
        p.canChooseFiles = false
        p.canChooseDirectories = true
        p.prompt = tr("选择")
        if p.runModal() == .OK, let u = p.url { dir = u.path }
    }
}
