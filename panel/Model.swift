import AppKit
import Combine
import Foundation
import SwiftUI

// MARK: - rexmod JSON

struct TargetDTO: Decodable, Equatable {
    var sessionId: String?
    var sessionLabel: String?
    var windowId: String?
    var focusedBlockId: String?
    var agentBlockId: String?
    var agent: String?              // Claude, Codex, OpenCode or Pi
    var agents: [AgentPaneDTO]?     // every agent pane of the window
    var editorBlockId: String?
    var cwd: String?
    var repo: String?
    var repoName: String?
    var branch: String?
    var gitDir: String?
    var host: String?               // the Rex host the session lives on; nil here
}

struct AgentPaneDTO: Decodable, Equatable, Identifiable {
    var blockId: String
    var agent: String
    var name: String
    var id: String { blockId }
}

struct FileChangeDTO: Decodable, Equatable {
    var path: String
    var oldPath: String?
    var status: String
    var add: Int
    var del: Int
    var binary: Bool?
    var staged: Bool? = nil
}

struct ChangesDTO: Decodable, Equatable {
    var mode: String
    var base: String?
    var baseRef: String?
    var running: Bool?
    var since: String?
    var note: String?
    var files: [FileChangeDTO]
    var add: Int
    var del: Int
}

struct CommentDTO: Decodable, Equatable, Identifiable {
    var id: String
    var file: String
    var line: Int
    var endLine: Int
    var text: String
}

/// `rexmod info`: what the Info tab shows beside the working directory.
struct InfoDTO: Decodable, Equatable {
    struct Agent: Decodable, Equatable {
        var name: String
        var blockId: String
        var status: String?
        var message: String?
        var sessionId: String?
        var resume: String?
    }
    struct Usage: Decodable, Equatable {
        struct Window: Decodable, Equatable {
            var label: String
            var used: Double
            var resetsAt: String?
        }
        var source: String
        var fetchedAt: String?
        var windows: [Window]
    }
    struct Proc: Decodable, Equatable, Identifiable {
        var pid: Int
        var name: String
        var seconds: Int
        var id: Int { pid }
    }
    struct Port: Decodable, Equatable, Identifiable {
        var port: Int
        var pid: Int
        var name: String
        var pane: String?
        var id: Int { port }
    }
    var agent: Agent?
    var usage: Usage?
    var processes: [Proc]
    var ports: [Port]
}

struct StateDTO: Decodable, Equatable {
    var target: TargetDTO?
    var changes: [String: ChangesDTO]?
    var pendingComments: Int
    var comments: [CommentDTO]? = nil
    var error: String?
}

private let decoder: JSONDecoder = {
    let d = JSONDecoder()
    d.keyDecodingStrategy = .convertFromSnakeCase
    return d
}()

// MARK: - Tree

final class TreeNode: Identifiable {
    let id: String // path relative to the repo
    var name: String
    let isDir: Bool
    var children: [TreeNode] = []
    var change: FileChangeDTO?

    init(id: String, name: String, isDir: Bool, change: FileChangeDTO? = nil) {
        self.id = id
        self.name = name
        self.isDir = isDir
        self.change = change
    }
}

struct TreeRow: Identifiable {
    let node: TreeNode
    let depth: Int
    var id: String { node.id }
}

enum Tree {
    static func build(_ items: [(String, FileChangeDTO?)], compress: Bool) -> [TreeNode] {
        let root = TreeNode(id: "", name: "", isDir: true)
        var dirs: [String: TreeNode] = ["": root]
        for (path, change) in items {
            let parts = path.split(separator: "/").map(String.init)
            guard !parts.isEmpty else { continue }
            var parent = root
            var acc = ""
            for (i, part) in parts.enumerated() {
                acc = acc.isEmpty ? part : acc + "/" + part
                if i == parts.count - 1 {
                    parent.children.append(TreeNode(id: acc, name: part, isDir: false, change: change))
                } else if let d = dirs[acc] {
                    parent = d
                } else {
                    let d = TreeNode(id: acc, name: part, isDir: true)
                    dirs[acc] = d
                    parent.children.append(d)
                    parent = d
                }
            }
        }
        sort(root)
        if compress { compressChains(root) }
        return root.children
    }

    private static func sort(_ n: TreeNode) {
        n.children.sort { a, b in
            if a.isDir != b.isDir { return a.isDir }
            return a.name.localizedStandardCompare(b.name) == .orderedAscending
        }
        n.children.forEach(sort)
    }

    /// Folds a folder whose only child is a folder into one row, "cmd/ryui".
    private static func compressChains(_ n: TreeNode) {
        for (i, c) in n.children.enumerated() where c.isDir {
            var cur = c
            var name = c.name
            while cur.children.count == 1, let only = cur.children.first, only.isDir {
                name += "/" + only.name
                cur = only
            }
            if cur !== c {
                let merged = TreeNode(id: cur.id, name: name, isDir: true)
                merged.children = cur.children
                n.children[i] = merged
            }
            compressChains(n.children[i])
        }
    }

    static func flatten(_ nodes: [TreeNode], collapsed: Set<String>, expandedByDefault: Bool,
                        expanded: Set<String> = [], depth: Int = 0) -> [TreeRow] {
        var rows: [TreeRow] = []
        for n in nodes {
            rows.append(TreeRow(node: n, depth: depth))
            if n.isDir {
                let open = expandedByDefault ? !collapsed.contains(n.id) : expanded.contains(n.id)
                if open {
                    rows += flatten(n.children, collapsed: collapsed, expandedByDefault: expandedByDefault,
                                    expanded: expanded, depth: depth + 1)
                }
            }
        }
        return rows
    }

    static func fuzzyMatch(_ query: String, _ text: String) -> Int? {
        let q = Array(query.lowercased()), t = Array(text.lowercased())
        var qi = 0, score = 0, last = -1
        for (i, ch) in t.enumerated() where qi < q.count && ch == q[qi] {
            score += (last == i - 1) ? 3 : 1
            if i == 0 || t[i - 1] == "/" || t[i - 1] == "_" || t[i - 1] == "-" || t[i - 1] == "." { score += 2 }
            last = i
            qi += 1
        }
        guard qi == q.count else { return nil }
        // Prefer matches in the file name.
        if let slash = text.lastIndex(of: "/") {
            let base = text[text.index(after: slash)...].lowercased()
            if base.contains(query.lowercased()) { score += 10 }
        }
        return score - t.count / 20
    }
}

// MARK: - Model

/// Three tabs: what the agent is (Info), what it changed and what you said
/// about it (Changes, with the pending comments on top), and the whole
/// repository (Files). A saved "comments" tab, from before the comments moved
/// into Changes, opens Changes.
enum PanelTab: String { case info, changes, files }

let panelTabs: [(tab: PanelTab, title: String, symbol: String)] = [
    (.info, "Info", "info.circle"),
    (.changes, "Changes", "arrow.triangle.branch"),
    (.files, "Files", "folder"),
]

/// A titled run of files in the Changes list.
struct ChangeGroup: Identifiable, Equatable {
    let title: String
    let files: [FileChangeDTO]
    var id: String { title }

    /// Uncommitted splits by the index, the way you'd commit it. The other
    /// modes compare two trees, so the index means nothing to them.
    static func groups(for c: ChangesDTO) -> [ChangeGroup] {
        guard c.mode == "uncommitted" else {
            return c.files.isEmpty ? [] : [ChangeGroup(title: "Changed", files: c.files)]
        }
        let staged = c.files.filter { $0.staged == true }
        let unstaged = c.files.filter { $0.staged != true }
        return [ChangeGroup(title: "Staged", files: staged), ChangeGroup(title: "Unstaged", files: unstaged)]
            .filter { !$0.files.isEmpty }
    }
}

let changeModes: [(id: String, title: String)] = [
    ("last-turn", "Last Turn"), ("uncommitted", "Uncommitted"), ("branch", "Branch"),
]

final class PanelModel: ObservableObject {
    @Published var state: StateDTO?
    @Published var tab: PanelTab {
        didSet {
            RM.defaults.set(tab.rawValue, forKey: "rexmod.tab")
            if tab == .files { refreshFiles() }
            if tab == .info { refreshInfo() }
        }
    }
    @Published var mode: String {
        didSet { RM.defaults.set(mode, forKey: "rexmod.mode") }
    }
    @Published var changesAsTree: Bool = RM.defaults.bool(forKey: "rexmod.changesAsTree") {
        didSet { RM.defaults.set(changesAsTree, forKey: "rexmod.changesAsTree") }
    }
    @Published var files: [String] = []
    @Published var filter: String = ""
    @Published var collapsed: Set<String> = []
    @Published var expandedFiles: Set<String> = []
    @Published var selected: String?
    @Published var loading = false
    @Published var flash: String?
    /// Not published: no view reads it, and a change would redraw the whole
    /// panel on the first frame of the edge animation.
    var panelVisible: Bool = RM.defaults.object(forKey: "rexmod.panelVisible") as? Bool ?? true {
        didSet { RM.defaults.set(panelVisible, forKey: "rexmod.panelVisible") }
    }

    private(set) var sessionID: String?
    private var lastTargetJSON: Data?
    private var watcher: FSWatcher?
    private var watchedRepo: String?
    /// A remote repository has no file events here. Its host's rexmod
    /// service says when it changes (rexmod remote-events); a slow timer
    /// covers a dropped stream.
    private var remoteEvents: Process?
    private var remoteHost: String?
    private var remoteBackoff: TimeInterval = 1
    private var safetyTimer: Timer?
    /// A change came while the panel was hidden: refresh once it shows.
    private var remoteDirty = false
    private var timers: [Timer] = []
    private var inflight = false
    private var pending = false
    private var debounce: DispatchWorkItem?
    private var filesInflight = false
    var isVisible: () -> Bool = { true }

    init() {
        tab = PanelTab(rawValue: RM.defaults.string(forKey: "rexmod.tab") ?? "") ?? .changes
        mode = RM.defaults.string(forKey: "rexmod.mode") ?? "uncommitted"
    }

    var target: TargetDTO? { state?.target }
    var currentChanges: ChangesDTO? { state?.changes?[mode] }

    func start() {
        sessionID = RM.focusedSessionID()
        refresh(reason: "start")
        // Tabs, focus and new agent panes change without file events.
        timers.append(Timer.scheduledTimer(withTimeInterval: 2.0, repeats: true) { [weak self] _ in
            guard let self = self, NSApp.isActive, self.isVisible() else { return }
            // A remote target costs a round trip to its host: not while the
            // panel is hidden. Showing it refreshes anyway.
            if self.target?.host != nil && !self.panelVisible { return }
            if self.remoteDirty {
                self.remoteDirty = false
                self.refresh(reason: "remote")
            }
            self.checkTarget()
        })
        // Processes come and go and the agent changes state without a file
        // event; read them while someone can see the Info tab.
        timers.append(Timer.scheduledTimer(withTimeInterval: 3.0, repeats: true) { [weak self] _ in
            guard let self = self, self.tab == .info, self.panelVisible, self.isVisible() else { return }
            self.refreshInfo()
        })
    }

    /// Called by the host with the session this window shows (an ID or label).
    func setSession(_ s: String?) {
        guard let s = s, s != sessionID else { return }
        sessionID = s
        lastTargetJSON = nil
        refresh(reason: "session changed")
    }

    func checkTarget() {
        guard let s = sessionID else { return }
        Shell.rexmod(["target", "--session", s]) { [weak self] status, out, _ in
            guard let self = self, status == 0 else { return }
            if out != self.lastTargetJSON {
                self.lastTargetJSON = out
                self.refresh(reason: "target changed")
            }
        }
    }

    func scheduleRefresh(after delay: TimeInterval = 0.25, reason: String) {
        debounce?.cancel()
        let w = DispatchWorkItem { [weak self] in self?.refresh(reason: reason) }
        debounce = w
        DispatchQueue.main.asyncAfter(deadline: .now() + delay, execute: w)
    }

    func refresh(reason: String) {
        guard let s = sessionID else {
            state = StateDTO(target: nil, changes: nil, pendingComments: 0, error: "No Rex session is focused.")
            return
        }
        // Hidden with a remote target: no round trip to its host (session
        // switches included) until the panel shows, which refreshes anyway.
        if target?.host != nil && !panelVisible && reason != "shown" {
            remoteDirty = true
            return
        }
        if inflight { pending = true; return }
        inflight = true
        loading = true
        Shell.rexmod(["state", "--session", s]) { [weak self] status, out, err in
            guard let self = self else { return }
            self.inflight = false
            self.loading = false
            if status == 0, let st = try? decoder.decode(StateDTO.self, from: out) {
                // Files arriving while the agent works, or comments leaving
                // once sent, fade in and out instead of jumping.
                if st != self.state { withAnimation(Motion.fade) { self.state = st } }
                self.updateWatcher()
                if self.tab == .files, self.files.isEmpty { self.refreshFiles() }
                // A new tab, agent or session: its Info right away, not in 3 s.
                if self.tab == .info, reason != "fs" { self.refreshInfo() }
            } else {
                RM.log("state failed (\(reason)): \(status) \(err)")
            }
            if self.pending {
                self.pending = false
                self.scheduleRefresh(after: 0.1, reason: "pending")
            }
        }
    }

    @Published var info: InfoDTO?
    private var infoInflight = false

    func refreshInfo() {
        guard let s = sessionID, !infoInflight else { return }
        infoInflight = true
        var args = ["info", "--session", s]
        if let w = target?.windowId { args += ["--window", w] }
        Shell.rexmod(args) { [weak self] status, out, _ in
            guard let self = self else { return }
            self.infoInflight = false
            guard status == 0, let i = try? decoder.decode(InfoDTO.self, from: out), i != self.info else { return }
            withAnimation(Motion.fade) { self.info = i }
        }
    }

    func refreshFiles() {
        guard let repo = target?.repo, !filesInflight else { return }
        filesInflight = true
        var args = ["files", "--repo", repo]
        if let host = target?.host { args += ["--host", host] }
        Shell.rexmod(args) { [weak self] status, out, _ in
            guard let self = self else { return }
            self.filesInflight = false
            guard status == 0, let s = String(data: out, encoding: .utf8) else { return }
            let list = s.split(separator: "\n").map(String.init)
            if list != self.files { self.files = list }
        }
    }

    private func updateWatcher() {
        // The same path on two machines is two repositories.
        let host = target?.host
        let key = target?.repo.map { (host.map { $0 + ":" } ?? "") + $0 }
        guard key != watchedRepo else { return }
        watchedRepo = key
        watcher = nil
        stopRemoteEvents()
        files = []
        collapsed = []
        expandedFiles = []
        guard key != nil, let path = target?.repo else { return }
        if let host = host {
            RM.log("following \(path) on Rex host \(host)")
            startRemoteEvents(host)
            safetyTimer = Timer.scheduledTimer(withTimeInterval: 20.0, repeats: true) { [weak self] _ in
                guard let self = self, self.panelVisible, self.isVisible() else { return }
                self.refresh(reason: "safety")
            }
            if tab == .files { refreshFiles() }
            return
        }
        let repo = path
        var paths = [repo]
        if let gd = target?.gitDir, !gd.hasPrefix(repo + "/") { paths.append(gd) }
        watcher = FSWatcher(paths: paths) { [weak self] changed in
            guard let self = self else { return }
            let relevant = changed.contains { p in
                !(p.contains("/.git/objects") || p.contains("/.git/logs") || p.hasSuffix(".lock")
                    || p.contains("/node_modules/") || p.contains("/.build/") || p.contains("/target/")
                    || p.contains("/DerivedData/") || p.contains("/.next/"))
            }
            guard relevant else { return }
            self.scheduleRefresh(after: 0.3, reason: "fs")
            if self.tab == .files { self.refreshFilesSoon() }
        }
        if tab == .files { refreshFiles() }
    }

    // MARK: remote change events

    private func startRemoteEvents(_ host: String) {
        remoteHost = host
        let p = Process()
        p.executableURL = URL(fileURLWithPath: RM.rexmodPath)
        p.arguments = ["remote-events", "--host", host]
        let out = Pipe()
        p.standardOutput = out
        // Its stdin: when this side goes (Rex quits or crashes), it sees EOF
        // and takes its rex events stream with it.
        p.standardInput = Pipe()
        p.standardError = FileHandle.nullDevice
        out.fileHandleForReading.readabilityHandler = { [weak self] h in
            let data = h.availableData
            guard !data.isEmpty, String(decoding: data, as: UTF8.self).contains("changed") else { return }
            DispatchQueue.main.async { self?.remoteChanged() }
        }
        p.terminationHandler = { [weak self] ended in
            out.fileHandleForReading.readabilityHandler = nil
            DispatchQueue.main.async {
                guard let self = self, self.remoteHost == host, self.remoteEvents === ended else { return }
                // The stream ended (network, host restart): again, backing off.
                let wait = self.remoteBackoff
                self.remoteBackoff = min(self.remoteBackoff * 2, 5)
                RM.log("remote events for \(host) ended; again in \(Int(wait)) s")
                DispatchQueue.main.asyncAfter(deadline: .now() + wait) { [weak self] in
                    guard let self = self, self.remoteHost == host, self.remoteEvents === ended else { return }
                    self.startRemoteEvents(host)
                }
            }
        }
        do {
            try p.run()
            remoteEvents = p
        } catch {
            RM.log("remote events for \(host) didn't start: \(error)")
        }
    }

    private func remoteChanged() {
        remoteBackoff = 1 // the stream works
        guard target?.host != nil else { return }
        if panelVisible && isVisible() {
            scheduleRefresh(after: 0.05, reason: "remote")
            if tab == .files { refreshFilesSoon() }
        } else {
            remoteDirty = true
        }
    }

    private func stopRemoteEvents() {
        if remoteHost != nil { RM.log("remote events stopped") }
        remoteHost = nil
        remoteEvents?.terminate()
        remoteEvents = nil
        safetyTimer?.invalidate()
        safetyTimer = nil
        remoteDirty = false
        remoteBackoff = 1
    }

    private var filesDebounce: DispatchWorkItem?
    private func refreshFilesSoon() {
        filesDebounce?.cancel()
        let w = DispatchWorkItem { [weak self] in self?.refreshFiles() }
        filesDebounce = w
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.0, execute: w)
    }

    // MARK: actions

    func open(_ relPath: String, line: Int = 0) {
        guard let t = target, let repo = t.repo, let s = t.sessionId else { return }
        selected = relPath
        var args = ["open", "--session", s]
        if let w = t.windowId { args += ["--window", w] }
        if line > 0 { args += ["--line", String(line)] }
        args.append(repo + "/" + relPath)
        Shell.rexmod(args) { [weak self] status, _, err in
            if status != 0 {
                self?.show(err.trimmingCharacters(in: .whitespacesAndNewlines).replacingOccurrences(of: "rexmod: ", with: ""))
            }
        }
    }

    func sendComments() {
        guard let t = target, let s = t.sessionId else { return }
        var args = ["comments", "send", "--session", s]
        if let w = t.windowId { args += ["--window", w] }
        Shell.rexmod(args) { [weak self] status, out, err in
            let msg = status == 0 ? String(data: out, encoding: .utf8) ?? "" : err
            self?.show(msg.trimmingCharacters(in: .whitespacesAndNewlines).replacingOccurrences(of: "rexmod: ", with: ""))
            self?.scheduleRefresh(after: 0.2, reason: "comments sent")
        }
    }

    /// Follows another agent pane of this tab: rexmod remembers it and focuses it.
    func follow(_ blockId: String) {
        guard let t = target, let s = t.sessionId, blockId != t.agentBlockId else { return }
        var args = ["follow", "--session", s, "--block", blockId]
        if let w = t.windowId { args += ["--window", w] }
        Shell.rexmod(args) { [weak self] status, _, err in
            if status != 0 { self?.show(err.replacingOccurrences(of: "rexmod: ", with: "")) }
            self?.scheduleRefresh(after: 0.05, reason: "follow")
        }
    }

    /// Pending comments per file and, summed, per folder above it.
    var commentCounts: [String: Int] {
        var m: [String: Int] = [:]
        for c in state?.comments ?? [] {
            m[c.file, default: 0] += 1
            var p = c.file
            while let slash = p.lastIndex(of: "/") {
                p = String(p[..<slash])
                m[p, default: 0] += 1
            }
        }
        return m
    }

    /// The followed agent's name, for messages.
    var agentName: String { target?.agent ?? "the agent" }

    func openPicker() {
        guard let t = target, let s = t.sessionId else { return }
        var args = ["pick", "--session", s]
        if let w = t.windowId { args += ["--window", w] }
        Shell.rexmod(args) { [weak self] status, _, err in
            if status != 0 { self?.show(err.replacingOccurrences(of: "rexmod: ", with: "")) }
        }
    }

    func show(_ msg: String) {
        guard !msg.isEmpty else { return }
        withAnimation(Motion.fade) { flash = msg }
        DispatchQueue.main.asyncAfter(deadline: .now() + 4) { [weak self] in
            if self?.flash == msg { withAnimation(Motion.fade) { self?.flash = nil } }
        }
    }

    func toggleFolder(_ id: String) {
        withAnimation(Motion.reflow) { toggleFolderNow(id) }
    }

    private func toggleFolderNow(_ id: String) {
        if tab == .changes {
            if collapsed.contains(id) { collapsed.remove(id) } else { collapsed.insert(id) }
        } else {
            if expandedFiles.contains(id) { expandedFiles.remove(id) } else { expandedFiles.insert(id) }
        }
    }

    // MARK: derived rows

    func changeGroups() -> [ChangeGroup] {
        currentChanges.map(ChangeGroup.groups(for:)) ?? []
    }

    func changeRows() -> [TreeRow] {
        guard let c = currentChanges else { return [] }
        let nodes = Tree.build(c.files.map { ($0.path, $0) }, compress: true)
        return Tree.flatten(nodes, collapsed: collapsed, expandedByDefault: true)
    }

    func fileRows() -> [TreeRow] {
        let changed = Dictionary(
            (state?.changes?["branch"]?.files ?? []).map { ($0.path, $0) }, uniquingKeysWith: { a, _ in a })
        let q = filter.trimmingCharacters(in: .whitespaces)
        if !q.isEmpty {
            let scored = files.compactMap { f -> (String, Int)? in
                Tree.fuzzyMatch(q, f).map { (f, $0) }
            }.sorted { $0.1 > $1.1 }.prefix(300)
            return scored.map { TreeRow(node: TreeNode(id: $0.0, name: $0.0, isDir: false, change: changed[$0.0]), depth: 0) }
        }
        let nodes = Tree.build(files.map { ($0, changed[$0]) }, compress: false)
        return Tree.flatten(nodes, collapsed: [], expandedByDefault: false, expanded: expandedFiles)
    }
}
