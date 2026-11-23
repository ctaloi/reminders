import Darwin
import Foundation

// reminders-bridge serve     the agent itself (launchd runs this)
// reminders-bridge relay     stdin/stdout to the agent's socket; what
//                                Omarchy runs over ssh. Never touches
//                                Reminders, so it needs no permission.
// reminders-bridge call JSON one request through the socket, for a person

let agentVersion = "0.1.0"

func log(_ s: String) {
    FileHandle.standardError.write(("\(Codec.isoMillis.string(from: Date())) \(s)\n").data(using: .utf8)!)
}

func connectToAgent() -> Int32 {
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    var addr = makeAddress(socketPath)
    let rc = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    if rc != 0 {
        FileHandle.standardError.write("reminders-bridge isn't running on this Mac (\(socketPath): \(String(cString: strerror(errno))))\n".data(using: .utf8)!)
        exit(3)
    }
    return fd
}

func pump(from: Int32, to: Int32) -> Bool {
    var buf = [UInt8](repeating: 0, count: 65536)
    while true {
        let n = read(from, &buf, buf.count)
        if n < 0 && errno == EINTR { continue }
        if n <= 0 { return true }
        let ok = buf.withUnsafeBytes { raw -> Bool in
            var off = 0
            while off < n {
                let w = write(to, raw.baseAddress! + off, n - off)
                if w < 0 { if errno == EINTR { continue }; return false }
                off += w
            }
            return true
        }
        if !ok { return false }
    }
}

func relay() -> Never {
    signal(SIGPIPE, SIG_IGN)
    let fd = connectToAgent()
    let up = Thread {
        _ = pump(from: STDIN_FILENO, to: fd)
        shutdown(fd, SHUT_WR) // done asking; answers still come back
    }
    up.start()
    _ = pump(from: fd, to: STDOUT_FILENO)
    exit(0)
}

func call(_ json: String) -> Never {
    signal(SIGPIPE, SIG_IGN)
    let fd = connectToAgent()
    let line = json + "\n"
    _ = line.withCString { write(fd, $0, strlen($0)) }
    shutdown(fd, SHUT_WR)
    _ = pump(from: fd, to: STDOUT_FILENO)
    exit(0)
}

func serve() -> Never {
    signal(SIGPIPE, SIG_IGN)
    setvbuf(stderr, nil, _IONBF, 0)
    let store = Store()
    let server = Server(store: store)
    do {
        try server.start()
    } catch {
        log("can't start: \(error)")
        exit(1)
    }
    log("agent \(agentVersion), Reminders access: \(Store.authorization)")
    if Store.authorization == "notDetermined" {
        store.requestAccess { log("Reminders access now: \($0)") }
    }
    withExtendedLifetime(server) { RunLoop.main.run() }
    exit(0)
}

let args = CommandLine.arguments.dropFirst()
switch args.first ?? "serve" {
case "serve": serve()
case "relay": relay()
case "call":
    guard args.count == 2 else { print("usage: reminders-bridge call '{\"op\":\"status\"}'"); exit(2) }
    call(args[args.index(after: args.startIndex)])
case "--version", "version": print(agentVersion); exit(0)
default:
    print("usage: reminders-bridge [serve | relay | call JSON | version]")
    exit(2)
}
