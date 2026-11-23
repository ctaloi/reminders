#!/bin/bash
# Builds Reminders Bridge on this Mac, makes it an app (so it can hold the
# Reminders permission), signs it, puts it in ~/Applications and starts it
# in the logged-in session with launchd.
#
#   ./install.sh               build, install, (re)start
#   ./install.sh --uninstall   stop it and remove the app and the agent
#
# Settings, all optional:
#   BUNDLE_ID      the app's id, which is also launchd's label (local.reminders-bridge)
#   SIGN_IDENTITY  a codesigning identity; by default the first Developer ID
#                  Application, then Apple Development, then ad hoc
#
# Signing with a real identity matters: macOS ties the Reminders permission
# to the signature, so an ad-hoc build asks again after every rebuild.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_ID="${BUNDLE_ID:-local.reminders-bridge}"
APP="$HOME/Applications/Reminders Bridge.app"
EXE="reminders-bridge"
PLIST="$HOME/Library/LaunchAgents/$BUNDLE_ID.plist"
LOGDIR="$HOME/Library/Logs/Reminders Bridge"
DOMAIN="gui/$(id -u)"

stop() { launchctl bootout "$DOMAIN/$BUNDLE_ID" 2>/dev/null || true; }

if [[ ${1:-} == --uninstall ]]; then
  stop
  rm -f "$PLIST"
  rm -rf "$APP"
  echo "Removed. The Reminders permission stays listed in System Settings until you remove it there."
  exit 0
fi

cd "$here"
swift build -c release 2>&1 | grep -v '^\[' || true
bin="$(swift build -c release --show-bin-path)/RemindersBridge"
[[ -x $bin ]] || { echo "build failed" >&2; exit 1; }

stage="$here/.build/stage/Reminders Bridge.app"
rm -rf "$stage"
mkdir -p "$stage/Contents/MacOS"
cp "$bin" "$stage/Contents/MacOS/$EXE"
cat > "$stage/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
  <key>CFBundleName</key><string>Reminders Bridge</string>
  <key>CFBundleDisplayName</key><string>Reminders Bridge</string>
  <key>CFBundleExecutable</key><string>$EXE</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$("$bin" version)</string>
  <key>CFBundleVersion</key><string>$("$bin" version)</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>LSUIElement</key><true/>
  <key>LSBackgroundOnly</key><true/>
  <key>NSRemindersFullAccessUsageDescription</key>
  <string>Reminders Bridge shares your reminders with your Linux desktop, over your own SSH connection.</string>
  <key>NSRemindersUsageDescription</key>
  <string>Reminders Bridge shares your reminders with your Linux desktop, over your own SSH connection.</string>
</dict>
</plist>
EOF

if [[ -z ${SIGN_IDENTITY:-} ]]; then
  ids="$(security find-identity -v -p codesigning 2>/dev/null || true)"
  SIGN_IDENTITY="$(sed -n 's/.*\(Developer ID Application: [^"]*\)".*/\1/p' <<<"$ids" | head -1)"
  [[ -z $SIGN_IDENTITY ]] && SIGN_IDENTITY="$(sed -n 's/.*\(Apple Development: [^"]*\)".*/\1/p' <<<"$ids" | head -1)"
  [[ -z $SIGN_IDENTITY ]] && SIGN_IDENTITY="-"
fi
if [[ $SIGN_IDENTITY == - ]]; then
  echo "Signing ad hoc: macOS will ask for Reminders access again after each rebuild."
fi
sign() { codesign --force --sign "$SIGN_IDENTITY" --identifier "$BUNDLE_ID" "$stage" 2>"$here/.build/codesign.err"; }
if ! sign; then
  # Over SSH the login keychain is locked, and an unlock only counts in the
  # session that made it, so it has to happen here, in this one.
  if grep -q errSecInternalComponent "$here/.build/codesign.err" && [[ -t 0 ]]; then
    echo "The keychain is locked in this SSH session; unlock it to sign (your Mac login password):"
    security unlock-keychain "$HOME/Library/Keychains/login.keychain-db"
    sign || { cat "$here/.build/codesign.err" >&2; exit 1; }
  else
    cat "$here/.build/codesign.err" >&2
    grep -q errSecInternalComponent "$here/.build/codesign.err" &&
      echo "The keychain is locked (common over SSH). Run this from a terminal, so it can ask to unlock it." >&2
    exit 1
  fi
fi

stop
mkdir -p "$HOME/Applications" "$LOGDIR" "$(dirname "$PLIST")"
rm -rf "$APP"
ditto "$stage" "$APP"

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$BUNDLE_ID</string>
  <key>ProgramArguments</key>
  <array><string>$APP/Contents/MacOS/$EXE</string><string>serve</string></array>
  <key>AssociatedBundleIdentifiers</key><string>$BUNDLE_ID</string>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardErrorPath</key><string>$LOGDIR/bridge.log</string>
  <key>StandardOutPath</key><string>$LOGDIR/bridge.log</string>
</dict>
</plist>
EOF
launchctl bootstrap "$DOMAIN" "$PLIST"

echo "Installed $APP ($("$APP/Contents/MacOS/$EXE" version), signed: $SIGN_IDENTITY)"
sleep 1
"$APP/Contents/MacOS/$EXE" call '{"op":"status"}'
