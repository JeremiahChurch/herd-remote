#!/usr/bin/env bash
# setup.sh - build herd-remote, set a password, install the user service,
# and expose it on the LAN via the existing WSLExpose hop.
set -euo pipefail

PORT="${PORT:-8787}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
CFG="${XDG_CONFIG_HOME:-$HOME/.config}/herd-remote"

echo ">> building"
( cd "$REPO" && go build -o herd-remote . )

echo ">> password"
mkdir -p "$CFG"; chmod 700 "$CFG"
if [[ ! -s "$CFG/password" ]]; then
  pw=""
  while [[ -z "$pw" ]]; do
    read -rsp "Set access password: " pw; echo
    [[ -z "$pw" ]] && echo "   password cannot be empty"
  done
  printf '%s' "$pw" > "$CFG/password"
  echo "   saved to $CFG/password"
else
  echo "   using existing $CFG/password"
fi
chmod 600 "$CFG/password"   # always enforce, even on a pre-existing file

echo ">> installing systemd --user services"
mkdir -p "$HOME/.config/systemd/user"
chmod +x "$REPO/deploy/expose-on-boot.sh"
for unit in herd-remote.service herd-remote-expose.service; do
  sed "s#127.0.0.1:8787#127.0.0.1:${PORT}#" "$REPO/deploy/$unit" \
    > "$HOME/.config/systemd/user/$unit"
done
systemctl --user daemon-reload
systemctl --user enable --now herd-remote.service
loginctl enable-linger "$USER" 2>/dev/null || true
echo "   systemctl --user status herd-remote   # to check"

# herd-remote-expose runs the WSLExpose reconcile *after* the socket is up, at every
# boot. Doing it here too would race the service start, so just restart it and let
# the unit's own wait loop do the work. It is intentionally not enabled: it has no
# [Install] section and is pulled in by herd-remote.service's Wants=.
echo ">> exposing port ${PORT} on the LAN (Windows host 10.10.69.99)"
if command -v expose-port >/dev/null; then
  # Clear a stale default.target.wants symlink from the pre-cycle-fix layout.
  rm -f "$HOME/.config/systemd/user/default.target.wants/herd-remote-expose.service"
  systemctl --user daemon-reload
  systemctl --user restart herd-remote-expose.service \
    || echo "   (run 'expose-port install' once if this failed; journalctl --user -u herd-remote-expose)"
else
  echo "   expose-port not found; skip"
fi

echo
echo "Done.  Phone/laptop:  http://10.10.69.99:${PORT}"
echo
echo "DEVICE LOCK (recommended) - restrict the firewall rule to your two fixed IPs."
echo "Run this in an ELEVATED PowerShell on Windows (replace with your device IPs):"
echo "  Set-NetFirewallRule -DisplayName 'WSL-Expose ${PORT}' -RemoteAddress '10.10.69.AA','10.10.69.BB'"
