#!/usr/bin/env bash
# expose-on-boot.sh - (re)apply the LAN portproxy for herd-remote once it is
# actually listening.
#
# Why this exists: the WSLExpose scheduled task is triggered at Windows *logon*,
# which happens before WSL boots and before herd-remote binds its socket. At that
# moment wsl-expose.ps1's loopback probe finds nothing answering on either family
# and falls back to its default `v4tov6 -> ::1:PORT` mapping. herd-remote binds
# IPv4-only (127.0.0.1), so that mapping never connects and the LAN URL is dead
# until someone re-runs `expose-port add` by hand.
#
# Running the same reconcile *after* the listener is up makes the probe succeed,
# so it installs the correct `v4tov4 -> 127.0.0.1:PORT` mapping.
#
# Usage: expose-on-boot.sh [port]      (default: port from $HERD_REMOTE_ADDR, else 8787)
set -uo pipefail

PORT="${1:-}"
if [[ -z "$PORT" ]]; then
  addr="${HERD_REMOTE_ADDR:-127.0.0.1:8787}"
  PORT="${addr##*:}"
fi
[[ "$PORT" =~ ^[0-9]+$ ]] || { echo "bad port: $PORT" >&2; exit 2; }

HOST_IP="${EXPOSE_HOST_IP:-10.10.69.99}"   # Windows LAN IP the proxy listens on
LISTEN_WAIT="${LISTEN_WAIT:-60}"           # seconds to wait for herd-remote's socket
INTEROP_WAIT="${INTEROP_WAIT:-60}"         # seconds to wait for WSL<->Windows interop
ATTEMPTS="${ATTEMPTS:-5}"                  # reconcile tries before giving up

# systemd --user runs with a minimal PATH; interop needs System32 for netsh/schtasks
# and ~/.local/bin for expose-port itself.
export PATH="$HOME/.local/bin:$PATH:/mnt/c/Windows/System32"

log() { echo "expose-on-boot[$PORT]: $*"; }

# Wait until something is listening on the loopback port inside WSL.
wait_listening() {
  local i
  for (( i = 0; i < LISTEN_WAIT; i++ )); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}

# Wait until we can actually shell out to Windows (binfmt interop can lag WSL boot).
wait_interop() {
  local i
  for (( i = 0; i < INTEROP_WAIT; i++ )); do
    if netsh.exe interface portproxy show v4tov4 >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

# True only for the mapping that actually works for an IPv4-only listener:
#   <HOST_IP> <PORT>  ->  127.0.0.1 <PORT>   (v4tov4)
# A v4tov6 -> ::1 row is the broken fallback we are here to correct, so it must not count.
mapping_ok() {
  local ip_re="${HOST_IP//./\\.}"
  netsh.exe interface portproxy show v4tov4 2>/dev/null | tr -d '\r' \
    | grep -qE "^[[:space:]]*${ip_re}[[:space:]]+${PORT}[[:space:]]+127\.0\.0\.1[[:space:]]+${PORT}[[:space:]]*$"
}

command -v expose-port >/dev/null || { log "expose-port not found; skipping"; exit 0; }

wait_listening || { log "nothing listening on 127.0.0.1:$PORT after ${LISTEN_WAIT}s; skipping"; exit 1; }
wait_interop   || { log "Windows interop unavailable after ${INTEROP_WAIT}s; skipping"; exit 1; }

if mapping_ok; then
  log "already mapped ${HOST_IP}:${PORT} -> 127.0.0.1:${PORT}"
  exit 0
fi

# `expose-port add` is idempotent: it keeps PORT in the desired list (self-healing if
# it was ever removed) and triggers the elevated task to reconcile. The task is async,
# so verify the result ourselves and retry - this also re-corrects the case where a
# late-firing logon sync clobbers us back to ::1.
for (( n = 1; n <= ATTEMPTS; n++ )); do
  expose-port add "$PORT" >/dev/null 2>&1
  # The elevated task takes ~5-10s to spin up PowerShell and reconcile, and repeat
  # triggers queue rather than coalesce - so wait generously before firing another.
  for (( i = 0; i < 15; i++ )); do
    mapping_ok && { log "exposed ${HOST_IP}:${PORT} -> 127.0.0.1:${PORT} (attempt $n)"; exit 0; }
    sleep 1
  done
  log "attempt $n did not produce a v4tov4 mapping; retrying"
  sleep 2
done

log "FAILED to expose port $PORT; check 'expose-port list'"
exit 1
