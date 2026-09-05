import Foundation

// Codable mirrors of the router's control-API JSON. Field names match exactly.

struct RouterStatus: Codable {
    var running: Bool
    var frontPort: Int
    var controlPort: Int
    var identityName: String
    var sharingEnabled: Bool
    var autoStopSharing: Bool?
    var usageReservePercent: Int?
    var shareUsageLevel: String?
    var upstream: UpstreamStatus
    var nostr: NostrStatus
    var localModels: [String]?
    var providerUsage: [String: ProviderUsage]?

    struct UpstreamStatus: Codable {
        var url: String
        var reachable: Bool
    }
    struct NostrStatus: Codable {
        var relays: [RelayStatus]
    }
    struct RelayStatus: Codable, Identifiable {
        var url: String
        var connected: Bool
        var id: String { url }
    }

    var models: [String] { localModels ?? [] }
    var connectedRelayCount: Int { nostr.relays.filter { $0.connected }.count }

    /// Session-reserve gate state (host keeps a buffer of each provider's quota).
    var autoStopSharingOn: Bool { autoStopSharing ?? false }
    var usageReserve: Int { usageReservePercent ?? 20 }
    /// How much of your own subscription friends see: "off" | "resets" | "windows".
    /// An absent or unrecognized value means the router's default, "resets".
    var usageShareLevel: UsageShareLevel { UsageShareLevel(rawValue: shareUsageLevel ?? "") ?? .resets }
}

/// One provider's subscription window state, fetched by the router from that
/// provider's usage endpoint and keyed by provider in RouterStatus.providerUsage.
/// `hasData` is false until the router's first successful poll.
struct ProviderUsage: Codable {
    var updatedAt: Int64 = 0
    var error: String?
    var windows: [UsageWindow]?

    var hasData: Bool { updatedAt > 0 }
    var shownWindows: [UsageWindow] { windows ?? [] }
}

/// A single rate-limit window (e.g. "Session (5h)", "Weekly") for a provider.
struct UsageWindow: Codable, Identifiable {
    var label: String
    var utilization: Double // percent used, 0...100
    var resetsAt: String    // RFC3339 timestamp, "" if unknown

    var id: String { label }
    var percentUsed: Int { Int(min(max(utilization, 0), 100).rounded()) }

    /// "resets in 2h 13m", or nil if the timestamp can't be parsed.
    var resetText: String? {
        guard let d = Self.parseISO(resetsAt) else { return nil }
        let secs = max(0, d.timeIntervalSinceNow)
        let f = DateComponentsFormatter()
        f.allowedUnits = secs >= 3600 ? [.day, .hour, .minute] : [.minute]
        f.unitsStyle = .abbreviated
        f.maximumUnitCount = 2
        return f.string(from: secs).map { "resets in \($0)" }
    }

    /// Parses an RFC3339 timestamp, tolerating microsecond fractions.
    static func parseISO(_ s: String) -> Date? {
        if s.isEmpty { return nil }
        let iso = ISO8601DateFormatter()
        iso.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = iso.date(from: s) { return d }
        iso.formatOptions = [.withInternetDateTime]
        let stripped = s.replacingOccurrences(of: #"\.\d+"#, with: "", options: .regularExpression)
        return iso.date(from: stripped)
    }
}

/// How much of the host's own subscription state rides along with presence.
/// Ordered by disclosure so the picker reads as a single dial.
enum UsageShareLevel: String, CaseIterable, Identifiable {
    case off, resets, windows

    var id: String { rawValue }

    var title: String {
        switch self {
        case .off: return "Nothing"
        case .resets: return "When a paused provider is back"
        case .windows: return "Live session usage"
        }
    }

    var detail: String {
        switch self {
        case .off:
            return "Friends still see that a provider is paused — they just aren't told when it lifts."
        case .resets:
            return "Adds the time a paused provider comes back. Never how much of your subscription is left."
        case .windows:
            return "Shares the percentage used of each session window you're sharing, so friends can see a squeeze coming instead of hitting it mid-task."
        }
    }
}

/// One provider's session window as a host chose to disclose it. `utilization`
/// is nil when the host shares reset times only, which is why this isn't just a
/// UsageWindow — "no number shared" has to be distinguishable from "0% used".
struct ProviderShare: Codable, Identifiable {
    var provider: String
    var limited: Bool
    var utilization: Double?
    var resetsAt: String

    var id: String { provider }
    var percentUsed: Int? { utilization.map { Int(min(max($0, 0), 100).rounded()) } }

    /// "resets in 1h 20m", or nil when no usable timestamp came across.
    var resetText: String? { UsageWindow(label: "", utilization: 0, resetsAt: resetsAt).resetText }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        provider = try c.decodeIfPresent(String.self, forKey: .provider) ?? ""
        limited = try c.decodeIfPresent(Bool.self, forKey: .limited) ?? false
        utilization = try c.decodeIfPresent(Double.self, forKey: .utilization)
        resetsAt = try c.decodeIfPresent(String.self, forKey: .resetsAt) ?? ""
    }
}

struct GrantView: Codable, Identifiable {
    var id: String
    var label: String
    var code: String
    var providers: [String]
    var models: [String]
    var createdAt: Int64
    var revoked: Bool
    var paused: Bool
    var online: Bool
    var routing: Bool
    var usageLimited: Bool // fully auto-paused: reserve gate hid ALL shared models
    var limitedProviders: [String] // providers the reserve gate paused (partial or full)
    var advertisedModels: [String]
    var totalReqs: Int
    var inputTokens: Int
    var outputTokens: Int
    var tokenLimit: Int // 0 = unlimited

    var totalTokens: Int { inputTokens + outputTokens }

    /// Whether this friend has a capped allotment.
    var hasLimit: Bool { tokenLimit > 0 }
    /// Tokens left in the allotment (host side: the host's own tally is the
    /// authoritative meter). Only meaningful when `hasLimit`.
    var remaining: Int { max(0, tokenLimit - totalTokens) }

    // Tolerant decoding: a null or missing field defaults instead of throwing,
    // so create vs. list response shapes can never crash the UI.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        label = try c.decodeIfPresent(String.self, forKey: .label) ?? ""
        code = try c.decodeIfPresent(String.self, forKey: .code) ?? ""
        providers = try c.decodeIfPresent([String].self, forKey: .providers) ?? []
        models = try c.decodeIfPresent([String].self, forKey: .models) ?? []
        createdAt = try c.decodeIfPresent(Int64.self, forKey: .createdAt) ?? 0
        revoked = try c.decodeIfPresent(Bool.self, forKey: .revoked) ?? false
        paused = try c.decodeIfPresent(Bool.self, forKey: .paused) ?? false
        online = try c.decodeIfPresent(Bool.self, forKey: .online) ?? false
        routing = try c.decodeIfPresent(Bool.self, forKey: .routing) ?? false
        usageLimited = try c.decodeIfPresent(Bool.self, forKey: .usageLimited) ?? false
        limitedProviders = try c.decodeIfPresent([String].self, forKey: .limitedProviders) ?? []
        advertisedModels = try c.decodeIfPresent([String].self, forKey: .advertisedModels) ?? []
        totalReqs = try c.decodeIfPresent(Int.self, forKey: .totalReqs) ?? 0
        inputTokens = try c.decodeIfPresent(Int.self, forKey: .inputTokens) ?? 0
        outputTokens = try c.decodeIfPresent(Int.self, forKey: .outputTokens) ?? 0
        tokenLimit = try c.decodeIfPresent(Int.self, forKey: .tokenLimit) ?? 0
    }
}

struct ConnectionView: Codable, Identifiable {
    var id: String
    var label: String
    var redeemedAt: Int64
    var online: Bool
    var routing: Bool
    var revoked: Bool // host permanently revoked this share
    var paused: Bool // host paused sharing (still online)
    var pausedReason: String // why the host is paused (e.g. usage limit), "" if not given
    var limitedProviders: [String] // providers the host auto-paused by reserve (partial or full)
    var usage: [ProviderShare] // host's session windows, as far as they chose to share
    var hostName: String
    var models: [String]
    var totalReqs: Int
    var inputTokens: Int
    var outputTokens: Int
    var tokenLimit: Int // host-advertised allotment, 0 = unlimited
    var tokensUsed: Int // host-authoritative usage, drives remaining

    var totalTokens: Int { inputTokens + outputTokens }

    /// Whether the host capped this connection's allotment.
    var hasLimit: Bool { tokenLimit > 0 }
    /// The limited-provider notice, e.g. "Codex paused (their session limit,
    /// resets in 1h 20m)". Falls back to the plain wording when the host doesn't
    /// share reset times or the timestamp can't be parsed.
    var limitedText: String? {
        guard !limitedProviders.isEmpty else { return nil }
        let names = limitedProviders.joined(separator: " & ")
        let resets = usage
            .filter { $0.limited }
            .compactMap { $0.resetText }
        // Only show a single shared time; two providers rarely reset together and
        // naming which is which would need more room than this one line has.
        if resets.count == 1, let only = resets.first {
            return "\(names) paused (their session limit, \(only)) — other models still available."
        }
        return "\(names) paused (their session limit) — other models still available."
    }

    /// The session windows worth drawing a bar for — only providers whose host
    /// shared an actual number (the "windows" level). Empty at every other level,
    /// which is what keeps the borrow card unchanged for hosts who didn't opt in.
    var sessionBars: [ProviderShare] { usage.filter { $0.utilization != nil } }

    /// Tokens left in your allotment (guest side: uses the host's authoritative
    /// tally, which is what the host actually enforces). Only meaningful when
    /// `hasLimit`.
    var remaining: Int { max(0, tokenLimit - tokensUsed) }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        label = try c.decodeIfPresent(String.self, forKey: .label) ?? ""
        redeemedAt = try c.decodeIfPresent(Int64.self, forKey: .redeemedAt) ?? 0
        online = try c.decodeIfPresent(Bool.self, forKey: .online) ?? false
        routing = try c.decodeIfPresent(Bool.self, forKey: .routing) ?? false
        revoked = try c.decodeIfPresent(Bool.self, forKey: .revoked) ?? false
        paused = try c.decodeIfPresent(Bool.self, forKey: .paused) ?? false
        pausedReason = try c.decodeIfPresent(String.self, forKey: .pausedReason) ?? ""
        limitedProviders = try c.decodeIfPresent([String].self, forKey: .limitedProviders) ?? []
        usage = try c.decodeIfPresent([ProviderShare].self, forKey: .usage) ?? []
        hostName = try c.decodeIfPresent(String.self, forKey: .hostName) ?? ""
        models = try c.decodeIfPresent([String].self, forKey: .models) ?? []
        totalReqs = try c.decodeIfPresent(Int.self, forKey: .totalReqs) ?? 0
        inputTokens = try c.decodeIfPresent(Int.self, forKey: .inputTokens) ?? 0
        outputTokens = try c.decodeIfPresent(Int.self, forKey: .outputTokens) ?? 0
        tokenLimit = try c.decodeIfPresent(Int.self, forKey: .tokenLimit) ?? 0
        tokensUsed = try c.decodeIfPresent(Int.self, forKey: .tokensUsed) ?? 0
    }
}

struct RouterConfigUpdate: Encodable {
    var nostrRelays: [String]? = nil
    var identityName: String? = nil
    var enableSharing: Bool? = nil
    var autoStopSharing: Bool? = nil
    var usageReservePercent: Int? = nil
    var shareUsageLevel: String? = nil
}

/// One routed request in the live activity feed (mirrors the router's
/// ActivityEntry; in-memory on the router, so it resets with it).
struct ActivityEntry: Codable, Identifiable {
    var id: Int64
    var ts: Int64
    var direction: String // "hosted" | "borrowed"
    var peer: String
    var model: String
    var status: String // "ok" | "error"
    var error: String
    var inputTokens: Int
    var outputTokens: Int
    var durationMs: Int64

    var hosted: Bool { direction == "hosted" }
    var ok: Bool { status == "ok" }
    var date: Date { Date(timeIntervalSince1970: TimeInterval(ts)) }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(Int64.self, forKey: .id)
        ts = try c.decodeIfPresent(Int64.self, forKey: .ts) ?? 0
        direction = try c.decodeIfPresent(String.self, forKey: .direction) ?? ""
        peer = try c.decodeIfPresent(String.self, forKey: .peer) ?? ""
        model = try c.decodeIfPresent(String.self, forKey: .model) ?? ""
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? ""
        error = try c.decodeIfPresent(String.self, forKey: .error) ?? ""
        inputTokens = try c.decodeIfPresent(Int.self, forKey: .inputTokens) ?? 0
        outputTokens = try c.decodeIfPresent(Int.self, forKey: .outputTokens) ?? 0
        durationMs = try c.decodeIfPresent(Int64.self, forKey: .durationMs) ?? 0
    }
}
