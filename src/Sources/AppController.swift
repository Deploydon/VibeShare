import Foundation
import SwiftUI
import AppKit
import ServiceManagement

/// AppController is the single source of truth the SwiftUI views observe. It
/// boots the child processes, subscribes to the router's SSE event stream for
/// instant updates, and polls the control API as a slow fallback.
@MainActor
final class AppController: ObservableObject {
    @Published var status: RouterStatus?
    @Published var grants: [GrantView] = []
    @Published var connections: [ConnectionView] = []
    @Published var activity: [ActivityEntry] = []
    @Published var softError: String?
    @Published var lastUpdated: Date?

    /// True while any grant (host) or connection (guest) is actively routing a
    /// request. The same signal that drives the per-row "routing" badges; the
    /// menu bar controller uses it to show the status-bar dot.
    var isRouting: Bool {
        grants.contains { $0.routing } || connections.contains { $0.routing }
    }

    /// Invoked on the main actor after every refresh so the menu bar status
    /// item can update (e.g. start/stop the routing dot).
    var onUpdate: (() -> Void)?

    let processes = ProcessManager.shared
    let providers = ProviderManager()
    var client = RouterClient()

    static let shared = AppController()

    private var pollTask: Task<Void, Never>?
    private var eventTask: Task<Void, Never>?
    private var eventRefreshPending = false

    func start() {
        processes.startAll()
        startPolling()
        startEventStream()
    }

    /// Slow fallback poll. The SSE stream delivers the interesting transitions
    /// instantly; this catches anything that doesn't notify (relay health,
    /// upstream reachability) and recovers if the stream is down.
    private func startPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refresh()
                try? await Task.sleep(nanoseconds: 5_000_000_000)
            }
        }
    }

    /// Live updates: re-subscribe to GET /api/events forever (the router may be
    /// restarting), refreshing on every pushed change.
    private func startEventStream() {
        eventTask?.cancel()
        eventTask = Task { [weak self] in
            while !Task.isCancelled {
                if let client = self?.client {
                    try? await client.subscribeEvents {
                        Task { @MainActor in AppController.shared.scheduleEventRefresh() }
                    }
                }
                // Stream ended (router down/restarting) — retry shortly.
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }

    /// Coalesce event bursts (every routed chunk of work can notify) into one
    /// refresh ~0.15s later.
    private func scheduleEventRefresh() {
        guard !eventRefreshPending else { return }
        eventRefreshPending = true
        Task { [weak self] in
            try? await Task.sleep(nanoseconds: 150_000_000)
            self?.eventRefreshPending = false
            await self?.refresh()
        }
    }

    func refresh() async {
        do {
            async let s = client.status()
            async let g = client.grants()
            async let c = client.connections()
            async let a = client.activity()
            let (st, gr, co, ac) = try await (s, g, c, a)
            // Notify on interesting transitions before overwriting the snapshot.
            NotificationManager.shared.diff(
                oldGrants: grants, newGrants: gr,
                oldConnections: connections, newConnections: co)
            status = st
            grants = gr
            connections = co
            activity = ac
            softError = nil
            lastUpdated = Date()
        } catch {
            // The router may still be coming up; keep prior data, surface softly.
            softError = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
        }
        onUpdate?()
    }

    // MARK: Actions

    @discardableResult
    func createGrant(label: String, providers: [String], models: [String], tokenLimit: Int) async -> GrantView? {
        do {
            let g = try await client.createGrant(
                label: label, providers: providers, models: models, tokenLimit: tokenLimit)
            await refresh()
            return g
        } catch {
            softError = error.localizedDescription
            return nil
        }
    }

    /// Adjust or top-up a friend's token allotment after the code was issued.
    func setGrantLimit(_ id: String, tokenLimit: Int) async {
        do {
            try await client.updateGrantLimit(id: id, tokenLimit: tokenLimit)
            await refresh()
        } catch {
            softError = error.localizedDescription
        }
    }

    /// Temporarily stop (or resume) serving a friend without killing their code.
    func setGrantPaused(_ id: String, paused: Bool) async {
        do {
            try await client.setGrantPaused(id: id, paused: paused)
            await refresh()
        } catch {
            softError = error.localizedDescription
        }
    }

    func revokeGrant(_ id: String) async {
        try? await client.revokeGrant(id: id)
        await refresh()
    }

    @discardableResult
    func redeem(code: String, label: String) async -> Bool {
        do {
            _ = try await client.redeem(code: code, label: label)
            await refresh()
            return true
        } catch {
            softError = error.localizedDescription
            return false
        }
    }

    func removeConnection(_ id: String) async {
        try? await client.removeConnection(id: id)
        await refresh()
    }

    func connectProvider(_ provider: ProviderInfo) {
        providers.connect(provider)
    }

    /// Disconnect a provider after confirmation (it deletes the stored sign-in,
    /// and getting back requires the browser OAuth dance).
    func disconnectProvider(_ provider: ProviderInfo) {
        let files = providers.authFiles(provider)
        let alert = NSAlert()
        NSApp.activate(ignoringOtherApps: true)
        guard !files.isEmpty else {
            // "Connected" is inferred from the model list, so a provider can look
            // connected via another account's models (e.g. Antigravity ⇄ Gemini).
            alert.messageText = "No stored sign-in for \(provider.name)"
            alert.informativeText = "Its models are likely served by another connected provider's account, so there is nothing to disconnect here."
            alert.addButton(withTitle: "OK")
            alert.runModal()
            return
        }
        alert.alertStyle = .warning
        alert.messageText = "Disconnect \(provider.name)?"
        let accounts = files.count == 1 ? "the locally stored sign-in" : "\(files.count) locally stored sign-ins"
        alert.informativeText = "This removes \(accounts). Friends you share \(provider.name) models with lose them until you reconnect. Your subscription itself is untouched."
        alert.addButton(withTitle: "Disconnect")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        providers.disconnect(provider)
        Task { await refresh() }
    }

    func setIdentityName(_ name: String) async -> Bool {
        do {
            try await client.updateConfig(RouterConfigUpdate(identityName: name))
        } catch {
            softError = error.localizedDescription
            return false
        }
        await refresh()
        return true
    }

    func setSharingEnabled(_ enabled: Bool) async {
        do {
            try await client.updateConfig(RouterConfigUpdate(enableSharing: enabled))
        } catch {
            softError = error.localizedDescription
        }
        await refresh()
    }

    /// Force an immediate subscription-usage refetch for one provider (the
    /// refresh button), bypassing the router's 10-minute cache.
    func refreshUsage(_ provider: String) async {
        do {
            try await client.refreshUsage(provider: provider)
            await refresh()
        } catch {
            softError = error.localizedDescription
        }
    }

    /// Configure the session-usage reserve: when `enabled`, the host stops
    /// sharing a provider's models once that provider's 5-hour session window
    /// passes `100 - percent`, keeping `percent`% of each subscription in reserve.
    func setUsageReserve(enabled: Bool, percent: Int) async {
        do {
            try await client.updateConfig(RouterConfigUpdate(
                autoStopSharing: enabled,
                usageReservePercent: percent))
        } catch {
            softError = error.localizedDescription
        }
        await refresh()
    }

    /// Choose how much of your own subscription state friends see alongside the
    /// pause they can already observe: nothing, reset times, or live windows.
    func setUsageShareLevel(_ level: UsageShareLevel) async {
        do {
            try await client.updateConfig(RouterConfigUpdate(shareUsageLevel: level.rawValue))
        } catch {
            softError = error.localizedDescription
        }
        await refresh()
    }

    /// Replace the signaling relay set. Relays are read at router start, so the
    /// router is restarted afterwards (the UI shows it reconnecting).
    func setRelays(_ relays: [String]) async -> Bool {
        do {
            try await client.updateConfig(RouterConfigUpdate(nostrRelays: relays))
        } catch {
            softError = error.localizedDescription
            return false
        }
        processes.restartRouter()
        return true
    }

    // MARK: Launch at login

    /// SMAppService only works from a real .app bundle (not `swift build` runs).
    nonisolated static var launchAtLoginAvailable: Bool {
        Bundle.main.bundlePath.hasSuffix(".app")
    }

    var launchAtLogin: Bool {
        guard Self.launchAtLoginAvailable else { return false }
        return SMAppService.mainApp.status == .enabled
    }

    func setLaunchAtLogin(_ on: Bool) {
        guard Self.launchAtLoginAvailable else { return }
        do {
            if on {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
        } catch {
            softError = "Launch at login: \(error.localizedDescription)"
        }
        objectWillChange.send()
    }

    // MARK: Misc

    var endpointBase: String {
        "http://127.0.0.1:\(status?.frontPort ?? 8788)"
    }

    var endpointURL: String {
        endpointBase + "/v1"
    }

    func copyToPasteboard(_ text: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
    }

    /// Getting-started step 2 ("point a tool at the endpoint") is checked once
    /// the user copies the endpoint or any setup snippet.
    static let toolSetupSeenKey = "onboarding.toolSetupSeen"
    func markToolSetupSeen() {
        UserDefaults.standard.set(true, forKey: Self.toolSetupSeenKey)
        objectWillChange.send()
    }
    var toolSetupSeen: Bool {
        UserDefaults.standard.bool(forKey: Self.toolSetupSeenKey)
    }

    func openLogsFolder() {
        NSWorkspace.shared.open(AppPaths.stateDir)
    }

    func openLogFile(_ url: URL) {
        NSWorkspace.shared.open(url)
    }

    /// Quit, but warn first when friends are connected (quitting cuts them off).
    func requestQuit() {
        let affected = grants.filter { !$0.revoked && ($0.online || $0.routing) }
        if !affected.isEmpty {
            let alert = NSAlert()
            alert.alertStyle = .warning
            alert.messageText = "Quit VibeShare?"
            let names = affected.map(\.label).joined(separator: ", ")
            alert.informativeText = affected.count == 1
                ? "\(names) is connected right now and will lose access to your models while VibeShare is closed."
                : "\(names) are connected right now and will lose access to your models while VibeShare is closed."
            alert.addButton(withTitle: "Quit")
            alert.addButton(withTitle: "Cancel")
            NSApp.activate(ignoringOtherApps: true)
            guard alert.runModal() == .alertFirstButtonReturn else { return }
        }
        quit()
    }

    func quit() {
        ProcessManager.shared.stopAll()
        NSApplication.shared.terminate(nil)
    }
}
