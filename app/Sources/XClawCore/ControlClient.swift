import Foundation
import os
#if canImport(Darwin)
import Darwin
#endif

/// Connects to xclawd's control bus over a Unix domain socket, sends command
/// envelopes, and exposes the inbound event stream. Roles are reversed from
/// Open Island's bridge (here the Go core is the server, this is the client).
///
/// Transport detail: a Unix-domain stream socket via POSIX `connect(2)`, with a
/// background read loop on a dispatch queue feeding a `LineFramer`. Decoded
/// event envelopes are published on an AsyncStream.
///
/// Thread safety: the only cross-thread mutable state (`fd`, `idCounter`) lives
/// behind an `OSAllocatedUnfairLock`; the `LineFramer` is local to the read
/// loop; the `AsyncStream.Continuation` is itself `Sendable`. So the type is
/// fully checked-`Sendable` — no `@unchecked`.
public final class ControlClient: Sendable {
    private let path: String
    private let queue = DispatchQueue(label: "app.xclaw.control.read")
    /// Dedicated write queue so a blocking `write(2)` never runs on the caller's
    /// thread (the main actor). Separate from the read queue, which is parked in
    /// a blocking `read(2)`. Writes are FIFO here, preserving command order.
    private let writeQueue = DispatchQueue(label: "app.xclaw.control.write")
    private let continuation: AsyncStream<Envelope>.Continuation
    public let events: AsyncStream<Envelope>

    /// Lock-protected mutable state (both fields are value types, so the lock's
    /// state is `Sendable`).
    private struct Mutable {
        var fd: Int32 = -1
        var idCounter = 0
        /// Bumped on each connect; the read loop captures its epoch and stops as
        /// soon as it no longer matches, so a loop from a previous connection can
        /// never keep reading (and can't touch a reused fd).
        var epoch = 0
    }
    private let mutable = OSAllocatedUnfairLock(initialState: Mutable())

    public init(path: String) {
        self.path = path
        var cont: AsyncStream<Envelope>.Continuation!
        self.events = AsyncStream { cont = $0 }
        self.continuation = cont
    }

    public enum ClientError: Error {
        case socketFailed(Int32)
        case connectFailed(Int32)
        case pathTooLong
        case notConnected
        case writeFailed(Int32)
    }

    /// Connects and starts the background read loop. Idempotent-ish: call once.
    public func connect() throws {
        let s = socket(AF_UNIX, SOCK_STREAM, 0)
        guard s >= 0 else { throw ClientError.socketFailed(errno) }

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = path.utf8CString
        guard pathBytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else {
            close(s)
            throw ClientError.pathTooLong
        }
        withUnsafeMutablePointer(to: &addr.sun_path) { dst in
            dst.withMemoryRebound(to: CChar.self, capacity: pathBytes.count) { p in
                for (i, b) in pathBytes.enumerated() { p[i] = b }
            }
        }
        let connectResult = withUnsafePointer(to: &addr) { ap in
            ap.withMemoryRebound(to: sockaddr.self, capacity: 1) { sp in
                Darwin.connect(s, sp, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard connectResult == 0 else {
            close(s)
            throw ClientError.connectFailed(errno)
        }
        mutable.withLock { $0.fd = s }
        let epoch = mutable.withLock { state -> Int in
            state.epoch += 1
            return state.epoch
        }
        startReadLoop(fd: s, epoch: epoch)
    }

    public func disconnect() {
        let fd = mutable.withLock { state -> Int32 in
            let current = state.fd
            state.fd = -1
            // Invalidate the running read loop's epoch so it stops before its
            // next read — it must not read a descriptor that may be reused by a
            // later connect().
            state.epoch += 1
            return current
        }
        if fd >= 0 { close(fd) }
        continuation.finish()
    }

    private func nextID() -> String {
        mutable.withLock { state in
            state.idCounter += 1
            return "c\(state.idCounter)"
        }
    }

    /// Sends a command and returns the id used (responses arrive as events with
    /// matching id on the stream).
    @discardableResult
    public func send<B: Encodable>(type: String, body: B) throws -> String {
        let fd = mutable.withLock { $0.fd }
        guard fd >= 0 else { throw ClientError.notConnected }
        let id = nextID()
        let env = try ControlCodec.command(id: id, type: type, body: body)
        let line = try ControlCodec.encode(env)
        try writeAll(line, fd: fd)
        return id
    }

    /// Like `send`, but performs the blocking socket write on `writeQueue` so the
    /// caller (typically the main actor) never blocks on `write(2)` if the kernel
    /// send buffer is full. Encoding + id allocation happen synchronously and the
    /// id is returned immediately. A write failure breaks the connection via the
    /// read loop (next `read` fails → stream finishes), which the UI reflects —
    /// hence no throw. Use this on the UI hot path; keep `send` for probe/tests.
    @discardableResult
    public func sendAsync<B: Encodable>(type: String, body: B) -> String {
        let id = nextID()
        let fd = mutable.withLock { $0.fd }
        guard fd >= 0 else { return id }
        do {
            let env = try ControlCodec.command(id: id, type: type, body: body)
            let line = try ControlCodec.encode(env)
            writeQueue.async { [weak self] in
                do { try self?.writeAll(line, fd: fd) }
                catch { Log.control.error("async write failed: \(error.localizedDescription, privacy: .public)") }
            }
        } catch {
            Log.control.error("sendAsync encode failed: \(error.localizedDescription, privacy: .public)")
        }
        return id
    }

    private func writeAll(_ data: Data, fd: Int32) throws {
        try data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) in
            guard let base = raw.bindMemory(to: UInt8.self).baseAddress else {
                return // empty payload; nothing to write
            }
            var off = 0
            while off < data.count {
                let n = Darwin.write(fd, base + off, data.count - off)
                if n <= 0 { throw ClientError.writeFailed(errno) }
                off += n
            }
        }
    }

    private func startReadLoop(fd localFD: Int32, epoch: Int) {
        // Capture only Sendable values; no `self`. The framer is loop-local.
        let mutable = self.mutable
        queue.async { [continuation] in
            let framer = LineFramer()
            var buf = [UInt8](repeating: 0, count: 64 * 1024)
            while true {
                // Stop if this connection has been superseded (disconnect/reconnect)
                // BEFORE reading, so we never read from a fd that may have been
                // closed and its number reused by a newer connection.
                if mutable.withLock({ $0.epoch }) != epoch { break }
                let n = Darwin.read(localFD, &buf, buf.count)
                if n <= 0 { break }
                if mutable.withLock({ $0.epoch }) != epoch { break }
                let chunk = Data(buf[0..<n])
                do {
                    for line in try framer.push(chunk) {
                        do {
                            continuation.yield(try ControlCodec.decode(line))
                        } catch {
                            Log.control.error("dropping undecodable envelope: \(error.localizedDescription, privacy: .public)")
                        }
                    }
                } catch {
                    Log.control.error("read loop stopped: \(error.localizedDescription, privacy: .public)")
                    break
                }
            }
            continuation.finish()
        }
    }
}
