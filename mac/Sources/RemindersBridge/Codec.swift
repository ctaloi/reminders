import AppKit
import EventKit
import Foundation

// Turning EventKit's objects into JSON and back. Dates on the wire:
//   "2026-11-24"                     a day with no time (all-day)
//   "2026-11-24T09:00:00-05:00"      a moment, with the offset it was made in
// Timestamps (modifiedAt and friends) carry milliseconds, so a write can be
// refused when the reminder changed since it was read.

enum Codec {
    static let iso: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        f.timeZone = .current
        return f
    }()

    static let isoMillis: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        f.timeZone = .current
        return f
    }()

    static func stamp(_ d: Date?) -> Any { d.map { isoMillis.string(from: $0) } ?? NSNull() }

    static func parseInstant(_ s: String) -> Date? {
        isoMillis.date(from: s) ?? iso.date(from: s)
    }

    // MARK: due and start dates

    static func dateJSON(_ c: DateComponents?) -> Any {
        guard let c, let y = c.year, let m = c.month, let d = c.day else { return NSNull() }
        if c.hour == nil {
            return String(format: "%04d-%02d-%02d", y, m, d)
        }
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = c.timeZone ?? .current
        var cc = DateComponents(year: y, month: m, day: d, hour: c.hour, minute: c.minute ?? 0, second: c.second ?? 0)
        cc.timeZone = cal.timeZone
        guard let date = cal.date(from: cc) else { return NSNull() }
        return iso.string(from: date)
    }

    enum CodecError: Error, CustomStringConvertible {
        case bad(String)
        var description: String { if case .bad(let s) = self { return s }; return "bad value" }
    }

    static func components(from value: Any?) throws -> DateComponents? {
        guard let value, !(value is NSNull) else { return nil }
        guard let s = value as? String else { throw CodecError.bad("a date has to be a string") }
        let cal = Calendar(identifier: .gregorian)
        if s.count == 10 {
            let parts = s.split(separator: "-").compactMap { Int($0) }
            guard parts.count == 3 else { throw CodecError.bad("not a day: \(s)") }
            var c = DateComponents(year: parts[0], month: parts[1], day: parts[2])
            c.calendar = cal
            return c
        }
        guard let date = parseInstant(s) else { throw CodecError.bad("not a date: \(s)") }
        var c = cal.dateComponents(in: .current, from: date)
        c = DateComponents(calendar: cal, timeZone: .current, year: c.year, month: c.month, day: c.day,
                           hour: c.hour, minute: c.minute, second: c.second)
        return c
    }

    // MARK: colors

    static func hex(_ cg: CGColor?) -> Any {
        guard let cg, let srgb = CGColorSpace(name: CGColorSpace.sRGB),
              let c = cg.converted(to: srgb, intent: .defaultIntent, options: nil),
              let comps = c.components, comps.count >= 3 else { return NSNull() }
        func b(_ v: CGFloat) -> Int { max(0, min(255, Int((v * 255).rounded()))) }
        return String(format: "#%02x%02x%02x", b(comps[0]), b(comps[1]), b(comps[2]))
    }

    // MARK: alarms

    static func alarmJSON(_ a: EKAlarm) -> [String: Any] {
        if let loc = a.structuredLocation, a.proximity != .none {
            var o: [String: Any] = ["type": "location", "title": loc.title ?? "",
                                    "proximity": a.proximity == .enter ? "arrive" : "leave"]
            if let g = loc.geoLocation {
                o["lat"] = g.coordinate.latitude
                o["lon"] = g.coordinate.longitude
            }
            o["radius"] = loc.radius
            return o
        }
        if let d = a.absoluteDate {
            return ["type": "absolute", "at": iso.string(from: d)]
        }
        return ["type": "relative", "offset": Int(a.relativeOffset)]
    }

    static func alarm(from o: [String: Any]) throws -> EKAlarm {
        switch o["type"] as? String {
        case "absolute":
            guard let s = o["at"] as? String, let d = parseInstant(s) else { throw CodecError.bad("an absolute alarm needs at") }
            return EKAlarm(absoluteDate: d)
        case "relative":
            guard let n = o["offset"] as? NSNumber else { throw CodecError.bad("a relative alarm needs offset (seconds)") }
            return EKAlarm(relativeOffset: n.doubleValue)
        default:
            throw CodecError.bad("alarms here are absolute or relative; location alarms are made on the iPhone or Mac")
        }
    }

    // MARK: repeats

    static let freqNames: [EKRecurrenceFrequency: String] = [.daily: "daily", .weekly: "weekly", .monthly: "monthly", .yearly: "yearly"]

    static func ruleJSON(_ r: EKRecurrenceRule) -> [String: Any] {
        var o: [String: Any] = ["frequency": freqNames[r.frequency] ?? "unknown", "interval": r.interval]
        if let days = r.daysOfTheWeek, !days.isEmpty {
            o["daysOfWeek"] = days.map { d -> [String: Any] in
                var x: [String: Any] = ["day": d.dayOfTheWeek.rawValue] // 1 = Sunday
                if d.weekNumber != 0 { x["week"] = d.weekNumber }
                return x
            }
        }
        if let v = r.daysOfTheMonth, !v.isEmpty { o["daysOfMonth"] = v.map(\.intValue) }
        if let v = r.monthsOfTheYear, !v.isEmpty { o["monthsOfYear"] = v.map(\.intValue) }
        if let v = r.setPositions, !v.isEmpty { o["setPositions"] = v.map(\.intValue) }
        if let end = r.recurrenceEnd {
            if let d = end.endDate { o["until"] = iso.string(from: d) }
            else if end.occurrenceCount > 0 { o["count"] = end.occurrenceCount }
        }
        return o
    }

    static func rule(from o: [String: Any]) throws -> EKRecurrenceRule {
        guard let name = o["frequency"] as? String,
              let freq = freqNames.first(where: { $0.value == name })?.key else {
            throw CodecError.bad("frequency is daily, weekly, monthly or yearly")
        }
        let interval = (o["interval"] as? NSNumber)?.intValue ?? 1
        let days: [EKRecurrenceDayOfWeek]? = try (o["daysOfWeek"] as? [[String: Any]])?.map { d in
            guard let n = (d["day"] as? NSNumber)?.intValue, let wd = EKWeekday(rawValue: n) else {
                throw CodecError.bad("daysOfWeek[].day is 1 (Sunday) to 7")
            }
            let week = (d["week"] as? NSNumber)?.intValue ?? 0
            return week == 0 ? EKRecurrenceDayOfWeek(wd) : EKRecurrenceDayOfWeek(wd, weekNumber: week)
        }
        func nums(_ k: String) -> [NSNumber]? { (o[k] as? [NSNumber]).flatMap { $0.isEmpty ? nil : $0 } }
        var end: EKRecurrenceEnd?
        if let s = o["until"] as? String, let d = parseInstant(s) ?? (try? components(from: s))?.date {
            end = EKRecurrenceEnd(end: d)
        } else if let n = (o["count"] as? NSNumber)?.intValue, n > 0 {
            end = EKRecurrenceEnd(occurrenceCount: n)
        }
        return EKRecurrenceRule(recurrenceWith: freq, interval: interval, daysOfTheWeek: days,
                                daysOfTheMonth: nums("daysOfMonth"), monthsOfTheYear: nums("monthsOfYear"),
                                weeksOfTheYear: nil, daysOfTheYear: nil, setPositions: nums("setPositions"), end: end)
    }

    // MARK: a whole reminder

    static func reminderJSON(_ r: EKReminder) -> [String: Any] {
        var o: [String: Any] = [
            "id": r.calendarItemIdentifier,
            "externalId": r.calendarItemExternalIdentifier ?? NSNull(),
            "list": r.calendar?.calendarIdentifier ?? NSNull(),
            "title": r.title ?? "",
            "notes": r.notes ?? NSNull(),
            "url": r.url?.absoluteString ?? NSNull(),
            "priority": r.priority,
            "completed": r.isCompleted,
            "completedAt": stamp(r.completionDate),
            "due": dateJSON(r.dueDateComponents),
            "alarms": (r.alarms ?? []).map(alarmJSON),
            "recurrence": (r.recurrenceRules ?? []).map(ruleJSON),
            "createdAt": stamp(r.creationDate),
            "modifiedAt": stamp(r.lastModifiedDate),
            // Tags, flags, subtasks and sections: EventKit doesn't show them.
            // Filled later from the Reminders database, when the agent has
            // Full Disk Access; until then always empty.
            "extras": [String: Any](),
        ]
        if let loc = r.location, !loc.isEmpty { o["location"] = loc }
        return o
    }

    static func listJSON(_ c: EKCalendar, defaultId: String?) -> [String: Any] {
        [
            "id": c.calendarIdentifier,
            "title": c.title,
            "color": hex(c.cgColor),
            "source": c.source?.title ?? "",
            "writable": c.allowsContentModifications,
            "default": c.calendarIdentifier == defaultId,
        ]
    }
}
