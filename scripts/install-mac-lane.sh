#!/bin/bash
# Install / upgrade the icmptun client on macOS (launchd daemon).
#
#   sudo scripts/install-mac-lane.sh <server-ip> [linkid]
#
# The launchd plist is rendered from scripts/com.cyx.icmptun.plist, so the peer
# address is never baked into the repository. linkid defaults to 0 and must match
# the -linkid of a server instance (a "lane").
#
# Needs root: run under sudo, or pass --elevate and let it re-exec itself through
# `osascript ... with administrator privileges`.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO/dist/icmptun-darwin-arm64"
TEMPLATE="$REPO/scripts/com.cyx.icmptun.plist"
DEST_BIN=/opt/icmptun/icmptun-darwin
DEST_PLIST=/Library/LaunchDaemons/com.cyx.icmptun.plist
LABEL=com.cyx.icmptun

if [ "${1:-}" = "--elevate" ]; then
  shift
  exec osascript -e "do shell script \"$0 $*\" with administrator privileges"
fi

PEER="${1:-${ICMPTUN_PEER:-}}"
LINKID="${2:-0}"

if [ -z "$PEER" ]; then
  echo "usage: $0 <server-ip> [linkid]" >&2
  echo "   or: ICMPTUN_PEER=<server-ip> $0" >&2
  exit 2
fi
printf '%s' "$PEER" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' \
  || { echo "peer must be an IPv4 address, got: $PEER" >&2; exit 2; }
case "$LINKID" in ''|*[!0-9]*) echo "linkid must be 0-255" >&2; exit 2 ;; esac
[ "$LINKID" -le 255 ] || { echo "linkid must be 0-255" >&2; exit 2; }

[ -x "$BIN" ] || { echo "missing $BIN (build it first)" >&2; exit 1; }
[ -f "$TEMPLATE" ] || { echo "missing $TEMPLATE" >&2; exit 1; }
[ "$(id -u)" = 0 ] || { echo "run as root (sudo, or pass --elevate)" >&2; exit 1; }

RENDERED="$(mktemp -t icmptun-plist)"
trap 'rm -f "$RENDERED"' EXIT
sed -e "s/__PEER__/$PEER/" -e "s/__LINKID__/$LINKID/" "$TEMPLATE" > "$RENDERED"
plutil -lint "$RENDERED" >/dev/null

xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true

mkdir -p /opt/icmptun
install -m 755 -o root -g wheel "$BIN" "$DEST_BIN.new"
mv -f "$DEST_BIN.new" "$DEST_BIN"
install -m 644 -o root -g wheel "$RENDERED" "$DEST_PLIST"

launchctl bootout "system/$LABEL" 2>/dev/null || true
sleep 1
launchctl bootstrap system "$DEST_PLIST"
sleep 4

if launchctl print "system/$LABEL" >/dev/null 2>&1; then
  echo "installed: $DEST_BIN"
  echo "peer:      $PEER  (linkid $LINKID)"
  launchctl print "system/$LABEL" | grep -E 'state|pid' | head -4
else
  echo "bootstrap failed; check /var/log/icmptun-client.log" >&2
  exit 1
fi
