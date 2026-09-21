#!/usr/bin/env bash
# scripts/server.sh — the local server's lifecycle, shared by the justfile recipes.
#
#   server.sh start    stop any ezBookkeeping server already on the port (whoever started it:
#                      `just run`, `ezbk up`, or a foreground run), start the current build in the
#                      background, wait until it is healthy, and verify the machine plane is armed
#   server.sh fg       the same stop, then run in the foreground (logs to the terminal; Ctrl+C stops)
#   server.sh stop     stop it
#   server.sh status   one line: up or down, pid, write/admin tiers
#   server.sh logs     follow the server log
#
# The pid file and the log are the ones `ezbk up` / `ezbk stop` use, so the two tools interoperate.
# It never stops a process that is not this app's server: a foreign process on the port is an error.
#
# Environment (the justfile passes these):
#   ROOT   the checkout            PORT   the port (EBK_PORT, default 8080)
#   STATE  the state dir (EZBK_STATE_DIR, default ~/T/_ezbookkeeping)
#   ALLOW_WRITE / ALLOW_ADMIN  1 or 0 — the machine plane's write and admin tiers (apis.mdx §9.1)

set -euo pipefail

ROOT="${ROOT:?ROOT is required}"
PORT="${PORT:-8080}"
STATE="${STATE:-$HOME/T/_ezbookkeeping}"
ALLOW_WRITE="${ALLOW_WRITE:-1}"
ALLOW_ADMIN="${ALLOW_ADMIN:-0}"
BIN="$ROOT/ezbookkeeping"
PIDFILE="$STATE/server.pid"
LOG="$STATE/server.log"
URL="http://localhost:$PORT/"
HEALTH="http://127.0.0.1:$PORT/healthz.json"
WAIT_SECONDS="${WAIT_SECONDS:-120}"

say() { printf '%s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

# is_ours PID — true when PID is this app's server process (any checkout's binary, any launcher)
is_ours() {
  local cmd
  cmd="$(ps -p "$1" -o command= 2>/dev/null || true)"
  [[ "$cmd" == *"ezbookkeeping --conf-path"*"server run"* ]]
}

healthy() {
  curl -fsS -m 2 "$HEALTH" 2>/dev/null | grep -q '"status":"ok"'
}

# listen_ports PID — the TCP ports PID listens on, space-separated
listen_ports() {
  lsof -nP -a -p "$1" -iTCP -sTCP:LISTEN -Fn 2>/dev/null | sed -n 's/^n.*:\([0-9]*\)$/\1/p' | sort -u | tr '\n' ' '
}

# find_pids — fills PIDS with every server of ours for this PORT: whatever of ours holds the port,
# plus the recorded pid while it is still starting (alive, ours, listening nowhere yet). A recorded
# server on ANOTHER port shares this state dir — the same database — so it is refused, not stopped.
# A foreign process on the port is refused. Runs in this shell (not a subshell) so die() exits.
PIDS=()
find_pids() {
  PIDS=()
  local p ports
  for p in $(lsof -nP -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true); do
    if is_ours "$p"; then
      PIDS+=("$p")
    else
      die "port $PORT is held by pid $p ($(ps -p "$p" -o command= 2>/dev/null)), which is not an ezBookkeeping server. Stop it yourself, or use another port: EBK_PORT=$((PORT + 10)) just run"
    fi
  done
  if [[ -s "$PIDFILE" ]]; then
    p="$(tr -dc '0-9' < "$PIDFILE")"
    if [[ -n "$p" ]] && kill -0 "$p" 2>/dev/null && is_ours "$p" && [[ " ${PIDS[*]-} " != *" $p "* ]]; then
      ports="$(listen_ports "$p")"
      if [[ -z "${ports// /}" ]]; then
        PIDS+=("$p")
      else
        die "another ezBookkeeping server (pid $p, port ${ports% }) is running on the same state dir $STATE — the same database. Two servers on one database is unsafe: stop it first (EBK_PORT=${ports%% *} just stop), or give this one its own EZBK_STATE_DIR."
      fi
    fi
  fi
}

do_stop() {
  local pids=() p i
  find_pids
  pids=("${PIDS[@]-}")
  [[ -z "${pids[0]-}" ]] && pids=()
  if [[ ${#pids[@]} -eq 0 ]]; then
    rm -f "$PIDFILE"
    say "==> No ezBookkeeping server was running on port $PORT."
    return 0
  fi
  say "==> Stopping ezBookkeeping (pid ${pids[*]})..."
  for p in "${pids[@]}"; do
    kill -TERM -- "-$p" 2>/dev/null || true   # its process group, when it leads one (ezbk up, just run)
    kill -TERM "$p" 2>/dev/null || true
  done
  for i in $(seq 1 100); do
    local alive=0
    for p in "${pids[@]}"; do kill -0 "$p" 2>/dev/null && alive=1; done
    [[ $alive -eq 0 ]] && break
    sleep 0.1
  done
  for p in "${pids[@]}"; do
    if kill -0 "$p" 2>/dev/null; then
      say "    pid $p did not exit in 10s; killing it"
      kill -KILL -- "-$p" 2>/dev/null || true
      kill -KILL "$p" 2>/dev/null || true
    fi
  done
  # wait for the port to be released so the new server can bind it
  for i in $(seq 1 50); do
    lsof -nP -tiTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 0.1
  done
  rm -f "$PIDFILE"
  say "    stopped."
}

# prepare — build present, state dirs, the one-time move of a db out of the repo, upstream's secret_key
prepare() {
  [[ -x "$BIN" ]] || die "backend not built. Run: just build"
  [[ -f "$ROOT/dist/index.html" ]] || die "frontend not built. Run: just build"
  mkdir -p "$STATE/data" "$STATE/log" "$STATE/storage"
  chmod 700 "$STATE" "$STATE/data" 2>/dev/null || true
  local f
  for f in ezbookkeeping.db .secret_key; do
    if [[ -f "$ROOT/data/$f" && ! -e "$STATE/data/$f" ]]; then
      mv "$ROOT/data/$f" "$STATE/data/$f"
      say "Moved data/$f out of the repo to $STATE/data/"
    fi
  done
  if [[ ! -s "$STATE/data/.secret_key" ]]; then
    (umask 077; openssl rand -hex 24 | tr -d '\n' > "$STATE/data/.secret_key")
    say "Generated $STATE/data/.secret_key"
  fi
  # a stale build is the commonest "my change is not there": say so, never guess silently
  local newest
  newest="$(cd "$ROOT" && git ls-files -z -- '*.go' ':!:cli/**' ':!:scripts/**' ':!:*_test.go' 2>/dev/null | xargs -0 stat -f '%m' 2>/dev/null | sort -n | tail -1 || true)"
  if [[ -n "$newest" && "$newest" -gt "$(stat -f '%m' "$BIN")" ]]; then
    say "Warning: Go sources are newer than ./ezbookkeeping — run 'just build' first to serve your changes."
  fi
}

server_env() {
  export EBK_WORK_DIR="$ROOT" \
    EBK_SERVER_HTTP_ADDR=127.0.0.1 \
    EBK_SERVER_HTTP_PORT="$PORT" \
    EBK_SERVER_DOMAIN=localhost \
    EBK_SERVER_STATIC_ROOT_PATH=dist \
    EBKCFP_SECURITY_SECRET_KEY="$STATE/data/.secret_key" \
    EBK_DATABASE_DB_PATH="$STATE/data/ezbookkeeping.db" \
    EBK_LOG_LOG_PATH="$STATE/log/ezbookkeeping.log" \
    EBK_STORAGE_LOCAL_FILESYSTEM_PATH="$STATE/storage/" \
    EZBK_MACHINE_ALLOW_WRITE="$ALLOW_WRITE" \
    EZBK_MACHINE_ALLOW_ADMIN="$ALLOW_ADMIN"
}

tiers() {
  local w=off a=off
  [[ "$ALLOW_WRITE" == 1 ]] && w=ON
  [[ "$ALLOW_ADMIN" == 1 ]] && a=ON
  printf 'writes %s, admin %s' "$w" "$a"
}

do_start() {
  prepare
  do_stop
  server_env
  cd "$ROOT"
  printf '\n==== %s  just run (port %s, %s) ====\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$PORT" "$(tiers)" >> "$LOG"
  say "==> Starting ezBookkeeping ($(tiers))..."
  # its own process group (so `ezbk stop` and this script stop it and nothing else), detached from
  # this terminal, stdout/stderr appended to the same log `ezbk up` writes
  nohup perl -e 'setpgrp(0, 0); exec @ARGV or die "exec: $!"' \
    "$BIN" --conf-path conf/ezbookkeeping.ini server run >> "$LOG" 2>&1 < /dev/null &
  local pid=$!
  echo "$pid" > "$PIDFILE"
  chmod 600 "$PIDFILE" "$LOG" 2>/dev/null || true

  local i
  for i in $(seq 1 $((WAIT_SECONDS * 2))); do
    if healthy; then break; fi
    if ! kill -0 "$pid" 2>/dev/null; then
      [[ "$(cat "$PIDFILE" 2>/dev/null)" == "$pid" ]] && rm -f "$PIDFILE"
      say "The server exited during start-up. Last lines of $LOG:" >&2
      tail -n 30 "$LOG" >&2
      exit 1
    fi
    sleep 0.5
  done
  if ! healthy; then
    say "The server did not become healthy within ${WAIT_SECONDS}s. Last lines of $LOG:" >&2
    tail -n 30 "$LOG" >&2
    exit 1
  fi

  # the machine plane is what ezbk and the MCP talk to: prove it armed with the tiers asked for
  local ping
  ping="$(curl -sS -m 3 "http://127.0.0.1:$PORT/machine/v1/ping" 2>/dev/null || true)"
  if [[ "$ping" != *'"code":"unauthorized"'* && "$ping" != *'"ok":true'* ]]; then
    say "Warning: the web app is up but the machine plane did not answer (got: ${ping:-nothing}). Read $LOG." >&2
  fi
  if [[ -x "$ROOT/cli/bin/ezbk" ]]; then
    local st
    st="$("$ROOT/cli/bin/ezbk" status --no-bringup 2>&1 || true)"
    grep -E '^[[:space:]]+(server|plane|user) ' <<< "$st" | sed 's/^/  /' || true
    if [[ "$ALLOW_WRITE" == 1 ]] && ! grep -q 'writes on' <<< "$st"; then
      say "Warning: writes were requested but the plane reports them off:" >&2
      grep -E 'plane' <<< "$st" >&2 || true
    fi
  fi

  say ""
  say "=============================================="
  say "  ezBookkeeping: $URL   (pid $pid, $(tiers))"
  say "=============================================="
  say "  logs: just logs     stop: just stop     status: just status"
  say "  The MCP server runs inside Claude Code: restart Claude Code to load a new MCP build."
}

do_fg() {
  prepare
  do_stop
  server_env
  cd "$ROOT"
  say ""
  say "=============================================="
  say "  ezBookkeeping: $URL   (foreground, $(tiers); Ctrl+C stops)"
  say "=============================================="
  say ""
  echo $$ > "$PIDFILE"
  exec "$BIN" --conf-path conf/ezbookkeeping.ini server run
}

do_status() {
  local pids=()
  find_pids
  pids=("${PIDS[@]-}")
  [[ -z "${pids[0]-}" ]] && pids=()
  if [[ ${#pids[@]} -eq 0 ]]; then
    say "ezBookkeeping: down (nothing of ours on port $PORT)"
    return 0
  fi
  if healthy; then
    say "ezBookkeeping: UP at $URL (pid ${pids[*]})"
  else
    say "ezBookkeeping: process running (pid ${pids[*]}) but $HEALTH is not answering"
  fi
  if [[ -x "$ROOT/cli/bin/ezbk" ]]; then
    "$ROOT/cli/bin/ezbk" status --no-bringup 2>&1 | grep -E '^[[:space:]]+plane ' || true
  fi
}

case "${1:-}" in
  start) do_start ;;
  fg) do_fg ;;
  stop) do_stop ;;
  status) do_status ;;
  logs) touch "$LOG"; exec tail -n 50 -F "$LOG" ;;
  *) die "usage: server.sh start|fg|stop|status|logs" ;;
esac
