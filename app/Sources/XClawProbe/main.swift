import Foundation
import XClawCore

// xclaw-probe — connects to a running xclawd control socket, sends one
// session.send command, and prints the event stream for a few seconds. Proves
// the Swift control client talks to the Go core end-to-end.
//
//   xclaw-probe <socket-path> [uid] [text]

let args = CommandLine.arguments
guard args.count >= 2 else {
    FileHandle.standardError.write(Data("usage: xclaw-probe <socket-path> [uid] [text]\n".utf8))
    exit(2)
}
let sockPath = args[1]
let uid = args.count >= 3 ? args[2] : "probe-user"
let text = args.count >= 4 ? args[3] : "hello from swift"
// Probe lifetime in seconds (env override, default 6) — bump it to watch a long
// streamed reply arrive token-by-token.
let runSecs = Double(ProcessInfo.processInfo.environment["XCLAW_PROBE_SECS"] ?? "") ?? 6
let t0 = Date()
func stamp() -> String { String(format: "+%5dms", Int(Date().timeIntervalSince(t0) * 1000)) }

let client = ControlClient(path: sockPath)
do {
    try client.connect()
} catch {
    FileHandle.standardError.write(Data("connect failed: \(error)\n".utf8))
    exit(1)
}
print("connected to \(sockPath)")

// Thread-safe collector for session keys seen (the detached consumer can't
// touch main-actor state).
final class SeenBox: @unchecked Sendable {
    private let lock = NSLock()
    private var keys = Set<String>()
    func add(_ k: String) { lock.lock(); keys.insert(k); lock.unlock() }
    var sorted: [String] { lock.lock(); defer { lock.unlock() }; return keys.sorted() }
}
let seen = SeenBox()

// Fire a health check then a session.send.
do {
    try client.send(type: "health", body: [String: String]())
    try client.send(type: "session.send", body: SessionSendBody(uid: uid, text: text))
    print("sent: health + session.send(uid=\(uid))")
} catch {
    FileHandle.standardError.write(Data("send failed: \(error)\n".utf8))
    exit(1)
}

// Drain events on a background queue (the read loop yields to the AsyncStream);
// a semaphore bounds the probe's lifetime without blocking the cooperative
// executor that runs the consumer.
let done = DispatchSemaphore(value: 0)
Task.detached {
    for await env in client.events {
        switch env.kind {
        case .response:
            print("  [response] id=\(env.id ?? "?") type=\(env.type)")
        case .event:
            switch env.type {
            case "session.text":
                if let b = env.decodeBody(SessionTextBody.self) { seen.add(b.sessionKey); print("  \(stamp()) [text] \(b.delta)") }
            case "session.tool":
                if let b = env.decodeBody(SessionToolBody.self) { seen.add(b.sessionKey); print("  \(stamp()) [tool] 🔧 \(b.name)(\(b.params))") }
            case "session.activity":
                if let b = env.decodeBody(SessionActivityBody.self) { seen.add(b.sessionKey); print("  \(stamp()) [activity] \(b.kind)") }
            case "session.reply":
                if let b = env.decodeBody(SessionReplyBody.self) { seen.add(b.sessionKey); print("  \(stamp()) [reply] 💬 \(b.text.prefix(80))…") }
            case "session.usage":
                if let b = env.decodeBody(SessionUsageBody.self) { seen.add(b.sessionKey); print("  \(stamp()) [usage] in=\(b.inputTokens) out=\(b.outputTokens)") }
            case "error":
                if let b = env.decodeBody(ErrorBody.self) { print("  [error] \(b.message)") }
            default:
                print("  [\(env.type)]")
            }
        case .command:
            break
        }
    }
    done.signal()
}

// Bound the probe lifetime; close the connection to end the stream.
_ = done.wait(timeout: .now() + runSecs)
client.disconnect()
_ = done.wait(timeout: .now() + 1)
print("── probe done ──")
print("sessions seen: \(seen.sorted)")
