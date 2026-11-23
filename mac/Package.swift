// swift-tools-version:5.10
// The Mac half of Reminders: a bridge that holds the Reminders
// permission and answers over a Unix socket. Swift 5 mode on purpose:
// EKEventStore isn't Sendable, and everything here runs on the main queue.
import PackageDescription

let package = Package(
    name: "RemindersBridge",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(
            name: "RemindersBridge",
            path: "Sources/RemindersBridge",
            linkerSettings: [.linkedFramework("EventKit"), .linkedFramework("AppKit")]
        ),
    ]
)
