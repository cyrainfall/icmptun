#!/usr/bin/env bash
# Lightweight local launcher for the icmptun client (no launchd).
#
#   ICMPTUN_PEER=203.0.113.10 scripts/icmptun-ctl.sh start
#   scripts/icmptun-ctl.sh status | stop | restart | env
#
# For a supervised install on macOS use scripts/install-mac-lane.sh instead.
set -u
BIN="${ICMPTUN_BIN:-/tmp/icmptun-darwin}"
PEER="${ICMPTUN_PEER:-}"
SOCKS="${ICMPTUN_SOCKS:-127.0.0.1:1080}"
HTTP="${ICMPTUN_HTTP:-127.0.0.1:8118}"
LOG="${ICMPTUN_LOG:-/tmp/icmptun-client.log}"
ARGS="-mode client -peer $PEER -socks $SOCKS -http $HTTP -wnd 4096 -fec 8 -payload 1350 -pps 20000 -v"

asroot() {
  # prefer cached sudo; otherwise use SUDO_PASS via -S
  if sudo -n true 2>/dev/null; then sudo "$@";
  elif [[ -n ${SUDO_PASS:-} ]]; then printf '%s\n' "$SUDO_PASS" | sudo -S "$@" 2>/dev/null;
  else echo "need sudo (set SUDO_PASS or run from a terminal)"; exit 1; fi
}

require_peer() {
  [[ -n $PEER ]] || { echo "set ICMPTUN_PEER=<server-ip> first"; exit 2; }
}

start() {
  require_peer
  if pgrep -x "$(basename "$BIN")" >/dev/null; then echo "already running"; return 0; fi
  [[ -x $BIN ]] || { echo "missing $BIN"; return 1; }
  asroot bash -c "nohup $BIN $ARGS > $LOG 2>&1 &"
  sleep 2
  pgrep -x "$(basename "$BIN")" >/dev/null \
    && echo "started (socks $SOCKS, http $HTTP)" \
    || { echo "failed, see $LOG"; return 1; }
}

stop() {
  asroot pkill -x "$(basename "$BIN")"
  echo "stopped"
}

status() {
  if pgrep -x "$(basename "$BIN")" >/dev/null; then
    echo "running pid $(pgrep -x "$(basename "$BIN")")"
  else
    echo "not running"
  fi
  grep -E 'socks5 on|http proxy' "$LOG" 2>/dev/null | tail -2
  tail -1 "$LOG" 2>/dev/null
  curl -s -m 12 -x "socks5h://$SOCKS" -o /dev/null -w 'probe=%{http_code} %{time_total}s\n' \
    https://www.cloudflare.com/cdn-cgi/trace || true
}

case "${1:-status}" in
  start) start ;;
  stop) stop ;;
  restart) stop; sleep 1; start ;;
  status) status ;;
  env)
    echo "export ALL_PROXY=socks5h://$SOCKS"
    echo "export http_proxy=http://$HTTP https_proxy=http://$HTTP"
    echo "export no_proxy=127.0.0.1,localhost,192.168.0.0/16,10.0.0.0/8"
    ;;
  *) echo "usage: ICMPTUN_PEER=<ip> $0 {start|stop|restart|status|env}" ;;
esac
