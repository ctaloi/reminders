import Darwin
import Foundation

// A Unix socket in the user's Application Support, readable only by them.
// One JSON object per line each way. Requests may carry a "seq", which the
// answer repeats ("id" always means a reminder's id). A connection that sends {"op":"watch"} also gets
// {"event":"changed"} whenever Reminders changes.

let socketPath: String = {
    let dir = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Application Support/Reminders Bridge", isDirectory: true)
    try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true,
                                             attributes: [.posixPermissions: 0o700])
    return dir.appendingPathComponent("bridge.sock").path
}()

func makeAddress(_ path: String) -> sockaddr_un {
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8)
    withUnsafeMutableBytes(of: &addr.sun_path) { buf in
        for (i, b) in bytes.prefix(buf.count - 1).enumerated() { buf[i] = b }
    }
    return addr
}

final class Client {
    let fd: Int32
    var buffer = Data()
    var source: DispatchSourceRead?
    var watching = false
    var pending = 0
    var readClosed = false
    var closed = false
    weak var server: Server?

    init(fd: Int32) { self.fd = fd }

    func send(_ obj: Any) {
        guard !closed, JSONSerialization.isValidJSONObject(obj),
              var data = try? JSONSerialization.data(withJSONObject: obj, options: [.withoutEscapingSlashes])
        else { return }
        data.append(0x0A)
        let ok = data.withUnsafeBytes { raw -> Bool in
            var off = 0
            while off < raw.count {
                let n = write(fd, raw.baseAddress! + off, raw.count - off)
                if n < 0 {
                    if errno == EINTR { continue }
                    // Dispatch makes the socket non-blocking; a big answer
                    // fills it, so wait (up to 10s) until there's room.
                    if errno == EAGAIN {
                        var p = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
                        if poll(&p, 1, 10_000) > 0 { continue }
                    }
                    log("dropping a connection: \(String(cString: strerror(errno)))")
                    return false
                }
                off += n
            }
            return true
        }
        if !ok { close() }
    }

    func finishIfDone() {
        if readClosed && pending == 0 { close() }
    }

    func close() {
        guard !closed else { return }
        closed = true
        if !readClosed { source?.cancel() }
        Darwin.close(fd)
        server?.drop(self)
    }
}

final class Server {
    let store: Store
    private var listenFD: Int32 = -1
    private var listenSource: DispatchSourceRead?
    private var clients: [Int32: Client] = [:]

    init(store: Store) {
        self.store = store
        store.observe { [weak self] in self?.broadcastChanged() }
    }

    func start() throws {
        unlink(socketPath)
        listenFD = socket(AF_UNIX, SOCK_STREAM, 0)
        guard listenFD >= 0 else { throw Failure(code: "io", message: "socket: \(String(cString: strerror(errno)))") }
        var addr = makeAddress(socketPath)
        let len = socklen_t(MemoryLayout<sockaddr_un>.size)
        let rc = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listenFD, $0, len) } }
        guard rc == 0 else { throw Failure(code: "io", message: "bind \(socketPath): \(String(cString: strerror(errno)))") }
        chmod(socketPath, 0o600)
        guard listen(listenFD, 16) == 0 else { throw Failure(code: "io", message: "listen: \(String(cString: strerror(errno)))") }
        let src = DispatchSource.makeReadSource(fileDescriptor: listenFD, queue: .main)
        src.setEventHandler { [weak self] in self?.accept() }
        src.resume()
        listenSource = src
        log("listening on \(socketPath)")
    }

    private func accept() {
        let fd = Darwin.accept(listenFD, nil, nil)
        guard fd >= 0 else { return }
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        let c = Client(fd: fd)
        c.server = self
        let src = DispatchSource.makeReadSource(fileDescriptor: fd, queue: .main)
        src.setEventHandler { [weak self, weak c] in
            guard let self, let c else { return }
            self.read(c)
        }
        c.source = src
        clients[fd] = c
        src.resume()
    }

    func drop(_ c: Client) { clients.removeValue(forKey: c.fd) }

    private func read(_ c: Client) {
        var buf = [UInt8](repeating: 0, count: 65536)
        let n = Darwin.read(c.fd, &buf, buf.count)
        if n <= 0 {
            // Nothing more to read. Cancel the reader (never suspend it: a
            // suspended source that gets released kills the process), but
            // keep the fd open for answers still on their way.
            c.readClosed = true
            c.source?.cancel()
            c.finishIfDone()
            return
        }
        c.buffer.append(contentsOf: buf[0..<n])
        while let nl = c.buffer.firstIndex(of: 0x0A) {
            let line = c.buffer.subdata(in: c.buffer.startIndex..<nl)
            c.buffer.removeSubrange(c.buffer.startIndex...nl)
            if line.isEmpty { continue }
            handleLine(line, from: c)
        }
    }

    private func handleLine(_ line: Data, from c: Client) {
        guard let req = (try? JSONSerialization.jsonObject(with: line)) as? [String: Any] else {
            c.send(["ok": false, "code": "bad", "error": "not a JSON object"])
            return
        }
        let seq = req["seq"]
        if req["op"] as? String == "watch" {
            c.watching = true
            var o: [String: Any] = ["ok": true, "result": store.status()]
            if let seq { o["seq"] = seq }
            c.send(o)
            return
        }
        c.pending += 1
        store.handle(req) { [weak c] result in
            guard let c else { return }
            var o: [String: Any]
            switch result {
            case .success(let v): o = ["ok": true, "result": v]
            case .failure(let f): o = ["ok": false, "code": f.code, "error": f.message]
            }
            if let seq { o["seq"] = seq }
            c.send(o)
            c.pending -= 1
            c.finishIfDone()
        }
    }

    private func broadcastChanged() {
        let o: [String: Any] = ["event": "changed", "at": Codec.stamp(Date())]
        for c in clients.values where c.watching { c.send(o) }
    }
}
