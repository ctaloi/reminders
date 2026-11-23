# Reminders

Apple Reminders on Linux, through a Mac you own.

Apple offers no way into Reminders from anywhere but its own devices: since
iOS 13, iCloud keeps them where CalDAV can't reach. A Mac can, though, so
this puts a small bridge on one, and talks to it over SSH:

```
Linux                                          Mac
reminders (service, CLI)  ──ssh──▶  Reminders Bridge  ──EventKit──▶  iCloud
  ~/.cache/reminders/                 (a signed app, run by launchd)
  desktop notifications
```

- **Reminders Bridge** (`mac/`, Swift) is an app that holds the Reminders
  permission and answers on a Unix socket. It never listens on the network:
  the only way in is SSH to your Mac, then its socket.
- **reminders** (`engine/`, Go) is a user service that keeps one SSH
  connection open, mirrors every list into `~/.cache/reminders/`, and
  rings desktop notifications for alarms. It's also the command line.

Changes show up both ways within a moment: the bridge tells the service when
anything in Reminders changes (on the phone, the Mac, anywhere), and the
service reads it again. It also reads everything every five minutes, in
case.

## Setup

You need SSH from Linux to the Mac with a key (no password prompt), and the
Mac's Command Line Tools (`xcode-select --install`).

1. **The Mac.** From Linux:

   ```sh
   bin/install-mac my-mac        # my-mac: the Mac, as ssh knows it
   ```

   It copies `mac/` over, builds the bridge, signs it, installs it in
   `~/Applications` and starts it. If the Mac's keychain is locked (normal
   over SSH), it asks for the Mac's login password to sign.

   Then, **at the Mac**, click **Allow** when "Reminders Bridge" asks for
   Reminders. (Later: System Settings › Privacy & Security › Reminders.)

   Signing: it uses your Developer ID if the Mac has one, then an Apple
   Development identity, then ad hoc. macOS ties the permission to the
   signature, so an ad-hoc build asks again after every rebuild.

2. **Linux.** Arch (and Omarchy):

   ```sh
   mkdir -p ~/.config/reminders
   echo 'host = "my-mac"' > ~/.config/reminders/config.toml
   bin/install                   # the package, and the user service, started
   reminders status
   ```

   Elsewhere: `bin/build`, put `build/reminders` on your PATH, and run
   `reminders serve` however your system runs user services
   (`pkgbuild/reminders.service` is the systemd one).

## The command line

```
reminders status                       connected? how many open, due today, overdue
reminders list [LIST] [--all]          what's open (--all: and what was finished lately)
reminders lists
reminders show REF
reminders add Buy milk --list Groceries --due "tomorrow 9am" --priority high
reminders add Bins out --due "thu 7pm" --repeat weekly
reminders done REF                     (undone REF puts it back)
reminders edit REF --due "fri 3pm"     --title --notes --url --list --priority --repeat; "none" clears
reminders delete REF
reminders refresh
reminders call '{"op":"lists"}'       one bridge request as is, for programs (prints {ok, result | code, error})
```

- **REF** is an id, the start of one (as `list` shows it), or a title.
- **When** reads the way you'd say it: `today`, `tomorrow 9am`, `fri 3pm`,
  `next mon`, `nov 28`, `11/28 5pm`, `in 2h`, `in 3 days`, `2026-12-01`.
- Every command takes `--json`, for scripts and agents.
- `list`, `lists`, `show` and `status` read the cache, so they answer offline.
  The others go through the service, or straight to the Mac when the service
  isn't running.
- An `edit` from an out-of-date copy is refused rather than overwriting a
  change made elsewhere: `reminders refresh`, then again.

## Notifications

A reminder rings:
- at each of its alerts;
- with no alert, at its due time;
- due on a day with no time, at 9:00, like Reminders' Today notification.

Each notification has **Done** and **In 1 hour**, which adds an alert an hour
from now. Location alerts can't ring here; they stay on the phone. One missed
while the computer slept still rings if it's under an hour late.

## config.toml

```toml
host = "my-mac"             # the Mac, as ssh knows it ($REMINDERS_HOST overrides)
completed_days = 30         # how far back finished reminders are kept

[notify]
enabled = true
all_day_at = "09:00"        # "" never rings for day-only reminders
open = ["my-app", "--reminder", "{id}"]   # adds an Open button
```

`$REMINDERS_OPEN` sets `open` too (words split on spaces), so a package can
add the button with a systemd drop-in instead of touching this file.

`command = ["ssh", …]` replaces the whole command that reaches the bridge,
for anything unusual.

## For a window, or a bar

The service publishes, in `~/.cache/reminders/`:
- `reminders.json`: every list and reminder.
- `state.json`: connected or not, why not, when it last synced, and counts
  (open, due today, overdue). It's rewritten on every change, so it's the
  one file to watch.

Writes go to the service's socket, `engine.sock` there: `POST /call` with a
bridge request (below), `GET /state` for the state.

## The bridge's protocol

One JSON object per line, each way. A request has an `op`, and may carry a
`seq` that its answer repeats. Answers are `{"ok":true,"result":…}` or
`{"ok":false,"code":"…","error":"…"}`.

| op | takes | gives |
|---|---|---|
| `status` | | bridge version, authorization, macOS |
| `watch` | | status; then `{"event":"changed"}` whenever Reminders changes |
| `lists` | | `[{id,title,color,source,writable,default}]` |
| `reminders` | `completedDays` (30), `lists` | every open reminder, and those finished in the window |
| `get` | `id` | one reminder |
| `create` | `title`, `list`, and any field below | the reminder |
| `update` | `id`, `ifModified`, fields (`null` clears) | the reminder |
| `complete` | `id`, `completed` (true), `ifModified` | the reminder |
| `delete` | `id`, `ifModified` | true |
| `refresh` | | asks iCloud for news |

Fields: `title`, `notes`, `url`, `location`, `priority` (0 none, 1 high,
5 medium, 9 low), `list`, `due` (`"2026-11-24"` for a day,
RFC 3339 for a time), `alarms` (`{type:"absolute",at}`,
`{type:"relative",offset}` in seconds from due; location alarms are read
only), `recurrence` (`{frequency,interval,daysOfWeek,daysOfMonth,
monthsOfYear,setPositions,until|count}`), `completed`.

Codes: `access` (no Reminders permission), `notFound`, `conflict`
(`ifModified` doesn't match: read it again), `bad`.

Worth knowing:
- Completing a repeating reminder works like it does in Reminders: the
  reminder moves to its next date, and a finished copy is kept with a new id.
- EventKit shows nothing of tags, flags, subtasks or sections. Each reminder
  has an `extras` object, kept empty for now, where those will go.

To try it by hand on the Mac:
`~/Applications/Reminders\ Bridge.app/Contents/MacOS/reminders-bridge call '{"op":"lists"}'`

## Development

- `bin/test` runs the engine's tests against a fake bridge; no Mac needed.
- `bin/install-mac HOST` rebuilds and restarts the bridge.
- The bridge logs to `~/Library/Logs/Reminders Bridge/bridge.log` on the Mac;
  the service to the journal (`journalctl --user -u reminders`).
- `mac/install.sh --uninstall` (run on the Mac) removes the bridge.
