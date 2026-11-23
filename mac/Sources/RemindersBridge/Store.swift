import EventKit
import Foundation

// Every request is answered here, on the main queue. A request is one JSON
// object with an "op"; the answer is {"ok":true,"result":…} or
// {"ok":false,"error":"…","code":"…"}. Codes the other side acts on:
//   access    the Reminders permission isn't granted
//   notFound  no reminder or list with that id (it may have been deleted)
//   conflict  it changed since it was read; read it again and retry
//   bad       the request itself is wrong

struct Failure: Error {
    let code: String
    let message: String
}

final class Store {
    let ek = EKEventStore()
    private var onChange: (() -> Void)?
    private var pendingChange: DispatchWorkItem?

    init() {
        // queue: nil, then hop to main ourselves. EventKit posts this from
        // the main queue and waits for observers; an observer that asks for
        // the main OperationQueue waits for it in turn, and neither moves.
        NotificationCenter.default.addObserver(forName: .EKEventStoreChanged, object: ek, queue: nil) { [weak self] _ in
            DispatchQueue.main.async { self?.changed() }
        }
        // iCloud pushes changes on its own; this nudges it now and then in
        // case a push was missed.
        Timer.scheduledTimer(withTimeInterval: 120, repeats: true) { [weak self] _ in
            self?.ek.refreshSourcesIfNecessary()
        }
    }

    func observe(_ f: @escaping () -> Void) { onChange = f }

    private func changed() {
        // EventKit says only that something changed, often several times in
        // a row; one word to the watchers is enough.
        pendingChange?.cancel()
        let w = DispatchWorkItem { [weak self] in
            self?.ek.reset()
            self?.onChange?()
        }
        pendingChange = w
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.4, execute: w)
    }

    static var authorization: String {
        switch EKEventStore.authorizationStatus(for: .reminder) {
        case .fullAccess: return "full"
        case .writeOnly: return "writeOnly"
        case .denied: return "denied"
        case .restricted: return "restricted"
        case .notDetermined: return "notDetermined"
        @unknown default: return "unknown"
        }
    }

    func requestAccess(_ done: @escaping (String) -> Void) {
        ek.requestFullAccessToReminders { _, err in
            if let err { log("access request: \(err.localizedDescription)") }
            DispatchQueue.main.async {
                self.ek.reset()
                done(Store.authorization)
            }
        }
    }

    // MARK: dispatch

    /// Answers one request; `reply` is called once, on the main queue.
    func handle(_ req: [String: Any], reply: @escaping (Result<Any, Failure>) -> Void) {
        let op = req["op"] as? String ?? ""
        if op == "status" {
            reply(.success(status()))
            return
        }
        if op == "authorize" {
            requestAccess { reply(.success(["authorization": $0])) }
            return
        }
        guard Store.authorization == "full" else {
            reply(.failure(Failure(code: "access", message: "This Mac hasn't given Reminders Bridge access to Reminders (\(Store.authorization)). Allow it in System Settings › Privacy & Security › Reminders.")))
            return
        }
        do {
            switch op {
            case "lists": reply(.success(lists()))
            case "reminders": fetch(req, reply: reply)
            case "get": reply(.success(Codec.reminderJSON(try find(req))))
            case "create": reply(.success(try create(req)))
            case "update": reply(.success(try update(req)))
            case "complete": reply(.success(try complete(req)))
            case "delete": reply(.success(try delete(req)))
            case "refresh":
                ek.refreshSourcesIfNecessary()
                reply(.success(true))
            default: throw Failure(code: "bad", message: "no such op: \(op)")
            }
        } catch let f as Failure {
            reply(.failure(f))
        } catch {
            reply(.failure(Failure(code: "bad", message: "\(error)")))
        }
    }

    // MARK: reads

    func status() -> [String: Any] {
        let auth = Store.authorization
        var o: [String: Any] = [
            "agent": agentVersion,
            "authorization": auth,
            "macos": ProcessInfo.processInfo.operatingSystemVersionString,
            "host": Host.current().localizedName ?? "",
        ]
        if auth == "full" { o["lists"] = ek.calendars(for: .reminder).count }
        return o
    }

    func lists() -> [[String: Any]] {
        let def = ek.defaultCalendarForNewReminders()?.calendarIdentifier
        return ek.calendars(for: .reminder).map { Codec.listJSON($0, defaultId: def) }
    }

    /// Everything not done, and what was finished in the last `completedDays`
    /// days (30 unless asked; 0 for none).
    func fetch(_ req: [String: Any], reply: @escaping (Result<Any, Failure>) -> Void) {
        let days = (req["completedDays"] as? NSNumber)?.intValue ?? 30
        var cals: [EKCalendar]?
        if let ids = req["lists"] as? [String] {
            cals = ids.compactMap { ek.calendar(withIdentifier: $0) }
        }
        let open = ek.predicateForIncompleteReminders(withDueDateStarting: nil, ending: nil, calendars: cals)
        ek.fetchReminders(matching: open) { [ek] openItems in
            let a = (openItems ?? []).map(Codec.reminderJSON)
            guard days > 0 else {
                DispatchQueue.main.async { reply(.success(a)) }
                return
            }
            let since = Calendar.current.date(byAdding: .day, value: -days, to: Date())
            let done = ek.predicateForCompletedReminders(withCompletionDateStarting: since, ending: nil, calendars: cals)
            ek.fetchReminders(matching: done) { doneItems in
                let b = (doneItems ?? []).map(Codec.reminderJSON)
                DispatchQueue.main.async { reply(.success(a + b)) }
            }
        }
    }

    // MARK: writes

    private func find(_ req: [String: Any]) throws -> EKReminder {
        guard let id = req["id"] as? String else { throw Failure(code: "bad", message: "id is missing") }
        guard let r = ek.calendarItem(withIdentifier: id) as? EKReminder else {
            throw Failure(code: "notFound", message: "That reminder is gone (deleted, or moved by iCloud).")
        }
        return r
    }

    /// Refuses when the caller's copy is older than what's here.
    private func checkUnchanged(_ r: EKReminder, _ req: [String: Any]) throws {
        guard let s = req["ifModified"] as? String else { return }
        let theirs = Codec.parseInstant(s)
        let ours = r.lastModifiedDate
        if let theirs, let ours, abs(theirs.timeIntervalSince(ours)) < 0.002 { return }
        if theirs == nil && ours == nil { return }
        throw Failure(code: "conflict", message: "It changed somewhere else since it was read.")
    }

    private func calendar(_ id: String) throws -> EKCalendar {
        guard let c = ek.calendar(withIdentifier: id) else { throw Failure(code: "notFound", message: "No such list.") }
        guard c.allowsContentModifications else { throw Failure(code: "bad", message: "\(c.title) can't be changed.") }
        return c
    }

    /// Sets each field present in `f`; null clears it.
    private func apply(_ f: [String: Any], to r: EKReminder) throws {
        func str(_ k: String) -> String? { f[k] as? String }
        if f.keys.contains("title") { r.title = str("title") ?? "" }
        if f.keys.contains("notes") { r.notes = str("notes") }
        if f.keys.contains("url") { r.url = str("url").flatMap(URL.init(string:)) }
        if f.keys.contains("location") { r.location = str("location") }
        if let p = f["priority"] as? NSNumber { r.priority = p.intValue }
        if f["priority"] is NSNull { r.priority = 0 }
        if let id = str("list") { r.calendar = try calendar(id) }
        if f.keys.contains("due") { r.dueDateComponents = try Codec.components(from: f["due"]) }
        if f.keys.contains("alarms") {
            // Location alarms can't be made here; keep the ones it has.
            let keep = (r.alarms ?? []).filter { $0.structuredLocation != nil && $0.proximity != .none }
            let new = try (f["alarms"] as? [[String: Any]] ?? []).map(Codec.alarm(from:))
            r.alarms = keep + new
        }
        if f.keys.contains("recurrence") {
            let rules = try (f["recurrence"] as? [[String: Any]] ?? []).map(Codec.rule(from:))
            if !rules.isEmpty && r.dueDateComponents == nil {
                throw Failure(code: "bad", message: "A repeating reminder needs a due date.")
            }
            r.recurrenceRules = rules
        }
        if let c = f["completed"] as? Bool { r.isCompleted = c }
    }

    private func save(_ r: EKReminder) throws -> [String: Any] {
        do {
            try ek.save(r, commit: true)
        } catch {
            throw Failure(code: "bad", message: "Reminders refused it: \(error.localizedDescription)")
        }
        return Codec.reminderJSON(r)
    }

    func create(_ req: [String: Any]) throws -> [String: Any] {
        let r = EKReminder(eventStore: ek)
        if let id = req["list"] as? String {
            r.calendar = try calendar(id)
        } else if let def = ek.defaultCalendarForNewReminders() {
            r.calendar = def
        } else {
            throw Failure(code: "bad", message: "No list to put it in.")
        }
        guard let title = req["title"] as? String, !title.trimmingCharacters(in: .whitespaces).isEmpty else {
            throw Failure(code: "bad", message: "A reminder needs a title.")
        }
        var f = req
        f.removeValue(forKey: "list")
        try apply(f, to: r)
        return try save(r)
    }

    func update(_ req: [String: Any]) throws -> [String: Any] {
        let r = try find(req)
        try checkUnchanged(r, req)
        var f = req
        for k in ["op", "id", "ifModified"] { f.removeValue(forKey: k) }
        try apply(f, to: r)
        return try save(r)
    }

    func complete(_ req: [String: Any]) throws -> [String: Any] {
        let r = try find(req)
        try checkUnchanged(r, req)
        r.isCompleted = (req["completed"] as? Bool) ?? true
        return try save(r)
    }

    func delete(_ req: [String: Any]) throws -> Bool {
        let r = try find(req)
        try checkUnchanged(r, req)
        do {
            try ek.remove(r, commit: true)
        } catch {
            throw Failure(code: "bad", message: "Reminders refused it: \(error.localizedDescription)")
        }
        return true
    }
}
