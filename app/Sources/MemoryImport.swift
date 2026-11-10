import SwiftUI

struct ImportPlanReport: Decodable {
    let sessions: Int
    let calls: Int
    let tokens: Int
    let skipped: [String: Int]
    let extractor: String
}

struct ImportStatus: Decodable {
    let state: String
    let total: Int
    let done: Int
    let added: Int
    let current: String
    let error: String?

    var running: Bool { state == "running" }
}

/// Extracts memories from past sessions. Nothing is sent before the user
/// sees the estimate and confirms; results wait in the review queue.
struct ImportHistorySheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    let dir: String
    let onReview: () -> Void
    @State private var allProjects = false
    @State private var since = "90d"
    @State private var plan: ImportPlanReport?
    @State private var status: ImportStatus?
    @State private var estimating = false
    @State private var planError = ""

    private var scopeArgs: [String] {
        (allProjects || dir.isEmpty ? ["--all"] : ["--dir", dir]) + ["--since", since]
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("导入历史会话").font(.title2.weight(.semibold))
            if let s = status, s.running || ["done", "failed", "cancelled", "interrupted"].contains(s.state) {
                progress(s)
            } else {
                form
            }
        }
        .padding(20)
        .frame(width: 460)
        .task {
            await refreshStatus()
            if status?.running != true { await estimate() }
            // Follow a run, including one started earlier or elsewhere.
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 1_500_000_000)
                if status?.running == true { await refreshStatus() }
            }
        }
    }

    private var form: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("从已有的会话里提取记忆，适合刚开始使用、或之前没开自动提取的时候。")
                .font(.callout).foregroundStyle(.secondary)
            Picker("范围", selection: $allProjects) {
                Text(dir.isEmpty ? tr("当前项目（未选择）") : tr("当前项目：%@", URL(fileURLWithPath: dir).lastPathComponent)).tag(false)
                Text("所有项目").tag(true)
            }
            .disabled(dir.isEmpty)
            Picker("时间", selection: $since) {
                Text("最近 30 天").tag("30d")
                Text("最近 90 天").tag("90d")
                Text("最近一年").tag("365d")
                Text("全部").tag("all")
            }
            GroupBox {
                VStack(alignment: .leading, spacing: 6) {
                    if estimating {
                        HStack { ProgressView().controlSize(.small); Text("正在估算…") }
                    } else if !planError.isEmpty {
                        Text(planError).foregroundStyle(.red).textSelection(.enabled)
                    } else if let p = plan {
                        Text(tr("将处理 %lld 个会话，调用 %@ %lld 次，约 %lld 个输入 token。", p.sessions, Names.agent(p.extractor), p.calls, p.tokens))
                        ForEach(p.skipped.sorted(by: { $0.key < $1.key }), id: \.key) { reason, n in
                            Text(tr("跳过 %lld 个：%@", n, tr(reason))).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }.frame(maxWidth: .infinity, alignment: .leading)
            }
            Label("脱敏后的对话文本会发送给提取用的 agent，并消耗你的额度；工具输出不会发送。导入的记忆都会先进入待审核，通过后才生效。", systemImage: "info.circle")
                .font(.callout).foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("取消") { dismiss() }.keyboardShortcut(.cancelAction)
                Button("开始导入") { start() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(estimating || (plan?.sessions ?? 0) == 0)
            }
        }
        .onChange(of: allProjects) { _ in Task { await estimate() } }
        .onChange(of: since) { _ in Task { await estimate() } }
    }

    private func progress(_ s: ImportStatus) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            if s.running {
                ProgressView(value: Double(s.done), total: Double(max(s.total, 1)))
                Text(tr("正在处理 %lld/%lld：%@", min(s.done + 1, s.total), s.total, s.current)).lineLimit(1).truncationMode(.middle)
                Text("可以关闭这个窗口，导入会在后台继续。").font(.callout).foregroundStyle(.secondary)
            } else {
                Label(resultText(s), systemImage: s.state == "done" ? "checkmark.circle" : "exclamationmark.triangle")
                    .foregroundStyle(s.state == "failed" ? Color.red : Color.primary)
                if let e = s.error, !e.isEmpty {
                    Text(e).font(.callout).foregroundStyle(.secondary).textSelection(.enabled).lineLimit(4)
                }
            }
            HStack {
                if s.running {
                    Button("停止导入") { Task { await model.perform { try await Backend.call(["memory-import-cancel"]) }; await refreshStatus() } }
                } else {
                    Button("再导入一次…") { status = nil; Task { await estimate() } }
                }
                Spacer()
                if !s.running && s.added > 0 {
                    Button("查看待审核") { onReview(); dismiss() }.keyboardShortcut(.defaultAction)
                }
                Button(s.running ? "在后台继续" : "完成") { dismiss() }
            }
        }
    }

    private func resultText(_ s: ImportStatus) -> String {
        switch s.state {
        case "done": return tr("已完成 %lld 个会话，新增 %lld 条记忆，等待审核。", s.done, s.added)
        case "cancelled", "interrupted": return tr("已停止：完成 %lld/%lld 个会话，新增 %lld 条。再次导入会从中断处继续。", s.done, s.total, s.added)
        default: return tr("导入失败：完成 %lld/%lld 个会话，新增 %lld 条。", s.done, s.total, s.added)
        }
    }

    private func refreshStatus() async {
        status = try? await Backend.api(ImportStatus.self, ["memory-import-status"])
    }

    private func estimate() async {
        estimating = true
        planError = ""
        defer { estimating = false }
        do { plan = try await Backend.api(ImportPlanReport.self, ["memory-import-plan"] + scopeArgs) } catch {
            plan = nil
            planError = error.localizedDescription
        }
    }

    private func start() {
        Task {
            await model.perform {
                try await Backend.call(["memory-import-start"] + scopeArgs)
                try? await Task.sleep(nanoseconds: 300_000_000)
                await refreshStatus()
            }
        }
    }
}
