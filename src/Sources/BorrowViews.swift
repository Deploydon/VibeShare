import SwiftUI

// MARK: - Borrow (guest / connections)

struct BorrowTab: View {
    @EnvironmentObject var controller: AppController
    @State private var redeeming = false

    var body: some View {
        if redeeming {
            RedeemView(onClose: { redeeming = false })
        } else {
            HStack {
                SectionTitle("Friends sharing with you",
                             subtitle: "Enter a code a friend gave you to use their models.")
                Spacer()
                Button {
                    redeeming = true
                } label: { Label("Enter code", systemImage: "plus") }
                    .controlSize(.small)
            }

            if controller.connections.isEmpty {
                EmptyHint(icon: "square.and.arrow.down",
                          text: "No connections yet. Ask a friend for a VibeShare code.")
            }

            ForEach(controller.connections) { c in
                ConnectionRow(connection: c)
            }
        }
    }
}

struct ConnectionRow: View {
    @EnvironmentObject var controller: AppController
    let connection: ConnectionView

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Image(systemName: "person.crop.circle")
                Text(displayName).font(.subheadline).bold()
                Spacer()
                if connection.routing { Badge(text: "routing", color: .blue) }
                if connection.revoked {
                    Badge(text: "revoked", color: .red)
                } else if connection.paused && connection.online {
                    Badge(text: "paused", color: .orange)
                } else {
                    Badge(text: connection.online ? "online" : "offline",
                          color: connection.online ? .green : .secondary)
                    // Some (not all) of this friend's providers hit their reserve.
                    if connection.online && !connection.limitedProviders.isEmpty {
                        Badge(text: "limited", color: .orange)
                    }
                }
            }
            if connection.revoked {
                Text("\(displayName) revoked this share. It will not be used for routing; remove it when you're done.")
                    .font(.caption2).foregroundStyle(.red)
            } else if connection.paused && connection.online {
                Text(connection.pausedReason.isEmpty
                     ? "\(displayName) paused sharing — their models are hidden until they resume."
                     : "\(displayName) \(connection.pausedReason)")
                    .font(.caption2).foregroundStyle(.orange)
            } else if !connection.models.isEmpty {
                Text(connection.models.prefix(4).joined(separator: ", ")
                     + (connection.models.count > 4 ? "…" : ""))
                    .font(.caption2).foregroundStyle(.secondary)
                    .lineLimit(2)
            } else {
                Text(connection.online ? "Waiting for shared model list…" : "Friend is offline")
                    .font(.caption2).foregroundStyle(.secondary)
            }
            // Partial limit: friend is online with some models, but a provider is
            // auto-paused by their reserve. (Full pause shows above via `paused`.)
            if connection.online, !connection.revoked, !connection.paused,
               let limited = connection.limitedText {
                Text(limited)
                    .font(.caption2).foregroundStyle(.orange)
            }
            // Host shares live session usage: show where each provider stands, so a
            // squeeze is visible before it turns into a failed request. Only ever
            // populated when the host chose the "windows" level.
            if connection.online, !connection.revoked {
                ForEach(connection.sessionBars) { bar in
                    sessionBar(bar)
                }
            }
            if !connection.revoked, connection.hasLimit {
                VStack(alignment: .leading, spacing: 3) {
                    HStack(spacing: 6) {
                        Image(systemName: "chart.bar.fill").font(.caption2)
                        Text(allotmentText).font(.caption2)
                        Spacer()
                    }
                    .foregroundStyle(allotmentColor)
                    ProgressView(value: min(Double(connection.tokensUsed), Double(connection.tokenLimit)),
                                 total: Double(connection.tokenLimit))
                        .progressViewStyle(.linear).tint(allotmentColor)
                }
            }
            HStack {
                Text("\(connection.models.count) models").font(.caption2).foregroundStyle(.secondary)
                Spacer()
                Text("\(connection.totalReqs) reqs · \(compact(connection.totalTokens)) tok")
                    .font(.caption2).foregroundStyle(.secondary)
                    .help("↑ \(connection.inputTokens) in · ↓ \(connection.outputTokens) out tokens")
                Button(role: .destructive) {
                    Task { await controller.removeConnection(connection.id) }
                } label: { Text("Remove").font(.caption2) }
                    .buttonStyle(.borderless)
            }
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.05)))
    }

    /// One provider's session window on the friend's side: "Codex 87% used ·
    /// resets in 41m" over a bar that turns orange as it approaches their cutoff.
    @ViewBuilder
    private func sessionBar(_ bar: ProviderShare) -> some View {
        let pct = bar.percentUsed ?? 0
        let tint: Color = bar.limited ? .orange : (pct >= 80 ? .yellow : .secondary)
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 6) {
                Text(bar.provider).font(.caption2)
                Text("\(pct)% used").font(.caption2).foregroundStyle(tint)
                Spacer()
                if let reset = bar.resetText {
                    Text(reset).font(.caption2).foregroundStyle(.secondary)
                }
            }
            ProgressView(value: Double(pct), total: 100)
                .progressViewStyle(.linear).tint(tint)
        }
    }

    private var allotmentText: String {
        if connection.remaining == 0 {
            return "Allotment used up (\(compact(connection.tokenLimit)) tok)"
        }
        return "\(compact(connection.remaining)) of \(compact(connection.tokenLimit)) tokens left"
    }

    private var allotmentColor: Color {
        if connection.remaining == 0 { return .red }
        if Double(connection.remaining) < Double(connection.tokenLimit) * 0.1 { return .orange }
        return .secondary
    }

    private var displayName: String {
        if !connection.hostName.isEmpty { return connection.hostName }
        return connection.label
    }
}

// MARK: - Redeem (inline form)

struct RedeemView: View {
    @EnvironmentObject var controller: AppController
    var onClose: () -> Void

    @State private var code = ""
    @State private var label = ""
    @State private var working = false
    @State private var error: String?
    @FocusState private var codeFieldFocused: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Button { onClose() } label: {
                    Image(systemName: "chevron.left")
                }.buttonStyle(.borderless)
                Text("Enter a friend's code").font(.headline)
                Spacer()
            }
            TextField("VS-XXXX-XXXX-…", text: $code)
                .textFieldStyle(.roundedBorder)
                .font(.system(.callout, design: .monospaced))
                .focused($codeFieldFocused)
                .onAppear {
                    DispatchQueue.main.async { codeFieldFocused = true }
                }
            TextField("Label", text: $label)
                .textFieldStyle(.roundedBorder)
            if let error {
                Text(error).font(.caption).foregroundStyle(.red)
            }
            HStack {
                Button("Cancel") { onClose() }
                Spacer()
                Button(working ? "Linking…" : "Link up") {
                    working = true
                    error = nil
                    Task {
                        let ok = await controller.redeem(code: code, label: label)
                        working = false
                        if ok { onClose() } else { error = controller.softError ?? "Invalid code" }
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(working || code.isEmpty)
            }
        }
    }
}
