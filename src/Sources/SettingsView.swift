import SwiftUI

// MARK: - Settings

struct SettingsTab: View {
    @EnvironmentObject var controller: AppController
    @State private var name: String = ""
    @State private var nameSaved = false
    @State private var sharing: Bool = true
    @State private var reserve: Double = 20
    @State private var notifyFriends = NotificationManager.shared.friendEventsEnabled
    @State private var notifyAllotment = NotificationManager.shared.allotmentEnabled
    @State private var editingRelays = false
    @State private var relaysDraft = ""
    @State private var relaysRestarting = false
    @FocusState private var relaysFieldFocused: Bool

    private var upstreamOK: Bool { controller.status?.upstream.reachable ?? false }

    private func saveName() {
        Task {
            if await controller.setIdentityName(name) {
                nameSaved = true
                DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { nameSaved = false }
            }
        }
    }

    var body: some View {
        identitySection
        Divider().padding(.vertical, 4)
        sessionReserveSection
        Divider().padding(.vertical, 4)
        notificationsSection
        Divider().padding(.vertical, 4)
        relaysSection
        Divider().padding(.vertical, 4)
        engineSection
    }

    // MARK: Identity & behavior

    @ViewBuilder
    private var identitySection: some View {
        SectionTitle("Identity", subtitle: "The name friends see when you're online.")
        HStack {
            TextField("Your name", text: $name)
                .textFieldStyle(.roundedBorder)
                .onSubmit { saveName() } // Return saves, no extra click
            Button(action: { saveName() }) {
                // Fixed width so Save → Saved ✓ doesn't resize the text field.
                Text(nameSaved ? "Saved ✓" : "Save").frame(width: 52)
            }
            .controlSize(.small)
            .disabled(nameSaved)
        }
        .onAppear {
            name = controller.status?.identityName ?? ""
            sharing = controller.status?.sharingEnabled ?? true
            reserve = Double(controller.status?.usageReserve ?? 20)
        }

        Toggle("Enable peer-to-peer sharing", isOn: $sharing)
            .onChange(of: sharing) { newValue in
                Task { await controller.setSharingEnabled(newValue) }
            }
            .font(.subheadline)

        if AppController.launchAtLoginAvailable {
            Toggle("Start VibeShare at login", isOn: Binding(
                get: { controller.launchAtLogin },
                set: { controller.setLaunchAtLogin($0) }
            ))
            .font(.subheadline)
            .help("Friends can only use your models while VibeShare is running.")
        } else {
            HStack(spacing: 6) {
                Toggle("Start VibeShare at login", isOn: .constant(false))
                    .font(.subheadline).disabled(true)
            }
            Text("Available when running the packaged VibeShare.app (make app).")
                .font(.caption2).foregroundStyle(.secondary)
        }
    }

    // MARK: Session reserve

    private var autoStopOn: Bool { controller.status?.autoStopSharingOn ?? false }

    @ViewBuilder
    private var sessionReserveSection: some View {
        SectionTitle("Session reserve",
                     subtitle: "Keep a buffer of each provider's subscription for yourself.")
        Toggle("Stop sharing a provider when its session runs low", isOn: Binding(
            get: { autoStopOn },
            set: { on in Task { await controller.setUsageReserve(enabled: on, percent: Int(reserve)) } }
        ))
        .font(.subheadline)
        .help("Tracks each provider's 5-hour session window separately, so a busy Claude window stops Claude sharing while Codex keeps flowing.")
        // Re-seed the slider whenever the server's value changes (e.g. it first
        // loads after this view appeared) so toggling on never writes a stale
        // reserve. It only fires on a real change — never mid-drag, which commits
        // only on release — so it can't fight an active drag.
        .onChange(of: controller.status?.usageReserve) { reserve = Double($0 ?? 20) }

        if autoStopOn {
            HStack {
                Text("Reserve \(Int(reserve))% for me").font(.subheadline)
                Spacer()
                Text("stops at \(max(0, 100 - Int(reserve)))% used")
                    .font(.caption2).foregroundStyle(.secondary)
            }
            Slider(value: $reserve, in: 5...80, step: 5) { editing in
                if !editing {
                    Task { await controller.setUsageReserve(enabled: true, percent: Int(reserve)) }
                }
            }
            Text("When a provider's session passes \(max(0, 100 - Int(reserve)))% used, its models stop being shared and resume automatically once the window resets.")
                .font(.caption2).foregroundStyle(.secondary)
            sessionUsageReadout
            usageSharePicker
        }
    }

    /// How much of your own subscription friends see. Friends are always told a
    /// provider is paused; this dial is only about how much context comes with it.
    @ViewBuilder
    private var usageSharePicker: some View {
        let level = controller.status?.usageShareLevel ?? .resets
        VStack(alignment: .leading, spacing: 4) {
            Picker("Tell friends", selection: Binding(
                get: { level },
                set: { new in
                    guard new != level else { return }
                    Task { await controller.setUsageShareLevel(new) }
                })) {
                    ForEach(UsageShareLevel.allCases) { l in
                        Text(l.title).tag(l)
                    }
                }
                .pickerStyle(.menu)
                .font(.subheadline)
            Text(level.detail)
                .font(.caption2).foregroundStyle(.secondary)
        }
    }

    /// Shows where each tracked provider's session currently sits, highlighting any
    /// already past the stop threshold so the effect of the slider is visible.
    @ViewBuilder
    private var sessionUsageReadout: some View {
        let usage = controller.status?.providerUsage ?? [:]
        let threshold = max(0, 100 - Int(reserve))
        let rows: [(name: String, window: UsageWindow)] = [("Claude", "claude"), ("Codex", "codex")]
            .compactMap { name, key in
                guard let w = usage[key]?.shownWindows
                    .first(where: { $0.label.lowercased().hasPrefix("session") }) else { return nil }
                return (name, w)
            }
        if !rows.isEmpty {
            VStack(alignment: .leading, spacing: 2) {
                ForEach(rows, id: \.name) { row in
                    // Compare the raw utilization (not the rounded percent) so this
                    // indicator matches the Go gate's `util >= threshold` exactly.
                    let paused = row.window.utilization >= Double(threshold)
                    HStack {
                        Text(row.name).font(.caption2)
                        Spacer()
                        Text(paused
                             ? "\(row.window.percentUsed)% used · sharing paused"
                             : "\(row.window.percentUsed)% used")
                            .font(.caption2)
                            .foregroundStyle(paused ? Color.orange : Color.secondary)
                    }
                }
            }
            .padding(.top, 2)
        }
    }

    // MARK: Notifications

    @ViewBuilder
    private var notificationsSection: some View {
        SectionTitle("Notifications", subtitle: "Heads-up while the popover is closed.")
        if NotificationManager.available {
            Toggle("Friends connecting, going online, or pausing", isOn: $notifyFriends)
                .onChange(of: notifyFriends) { NotificationManager.shared.friendEventsEnabled = $0 }
                .font(.subheadline)
            Toggle("Token allotments at 90% and used up", isOn: $notifyAllotment)
                .onChange(of: notifyAllotment) { NotificationManager.shared.allotmentEnabled = $0 }
                .font(.subheadline)
        } else {
            Text("Available when running the packaged VibeShare.app (make app).")
                .font(.caption2).foregroundStyle(.secondary)
        }
    }

    // MARK: Relays

    @ViewBuilder
    private var relaysSection: some View {
        HStack {
            SectionTitle("Signaling relays",
                         subtitle: "Public Nostr relays (used only for encrypted connection setup).")
            Spacer()
            if !editingRelays {
                Button("Edit") {
                    relaysDraft = (controller.status?.nostr.relays ?? [])
                        .map(\.url).joined(separator: "\n")
                    editingRelays = true
                }
                .controlSize(.small)
            }
        }
        if editingRelays {
            TextField("wss://relay.example.com (one per line)", text: $relaysDraft, axis: .vertical)
                .textFieldStyle(.roundedBorder)
                .font(.system(.caption2, design: .monospaced))
                .lineLimit(3...8)
                .focused($relaysFieldFocused)
                .onAppear {
                    DispatchQueue.main.async { relaysFieldFocused = true }
                }
            HStack {
                Button("Cancel") { editingRelays = false }
                    .controlSize(.small)
                Spacer()
                Button(relaysRestarting ? "Restarting…" : "Save & restart engine") {
                    let relays = relaysDraft
                        .split(separator: "\n")
                        .map { $0.trimmingCharacters(in: .whitespaces) }
                        .filter { $0.contains("://") }
                    guard !relays.isEmpty else { return }
                    relaysRestarting = true
                    Task {
                        _ = await controller.setRelays(relays)
                        relaysRestarting = false
                        editingRelays = false
                    }
                }
                .controlSize(.small)
                .disabled(relaysRestarting)
            }
            Text("Relays only apply on engine start, so saving restarts the router (a second or two).")
                .font(.caption2).foregroundStyle(.secondary)
        } else {
            ForEach(controller.status?.nostr.relays ?? []) { r in
                HStack {
                    StatusDot(on: r.connected)
                    Text(r.url).font(.system(.caption2, design: .monospaced))
                    Spacer()
                }
            }
        }
    }

    // MARK: Engine

    @ViewBuilder
    private var engineSection: some View {
        HStack {
            Image(systemName: upstreamOK ? "checkmark.seal.fill" : "exclamationmark.triangle.fill")
                .foregroundStyle(upstreamOK ? Color.green : Color.orange)
            Text(upstreamOK ? "Provider engine reachable" : "Provider engine starting…")
                .font(.caption)
        }
        HStack {
            Button {
                controller.openLogsFolder()
            } label: { Label("Open logs folder", systemImage: "folder") }
                .controlSize(.small)
            Button {
                ProcessManager.shared.restartAll()
            } label: { Label("Restart engine", systemImage: "arrow.clockwise") }
                .controlSize(.small)
                .help("Restarts both bundled engines (router + provider)")
        }
        Text("VibeShare 1.0").font(.caption2).foregroundStyle(.secondary)
    }
}
