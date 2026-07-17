# herd-remote

A tiny phone-friendly web control surface for [herdr](https://herdr.dev) sessions.

Spawn a new agent session in any folder, see which sessions are **idle / working /
blocked**, read a session's screen, and do the minimal interactions that matter from
your phone: approve/deny a prompt, interrupt a hang, `/clear`, send a prompt, rename,
or kill a session. Built for the case where you're away from the keyboard and a Codex
or Claude session is stuck waiting on you.

It's a single Go binary that shells out to the `herdr` socket-API CLI (and the
`herd-spawn` helper). No daemon of its own beyond an HTTP server; herdr does the work.

## What it does

- **Session list** - every agent-bearing pane, colored by status, blocked ones first
  (blocked pulses red). Auto-refreshes.
- **Detail view** - live scrollback (last ~50 lines, polled) plus control buttons:
  - `Enter` (approve default), `Esc` (deny/cancel), `C-c` (kick a hang)
  - `▲ ▼` to navigate a Codex approval menu, `y` / `n` / `1` `2` `3` for direct answers
  - `/clear`, free-text prompt box, rename, and close (kill).
- **Spawn** - optional session name, pick a folder under `$HOME` (filterable), optional
  first prompt + model, fires `herd-spawn`. A name labels the herdr window + this list
  (and the pane border). Claude defaults to Opus 4.8; Codex defaults to `gpt-5.6-sol`.
  After spawning it jumps straight into the new session and clears the form.

All control keys are validated against an allowlist server-side, so the HTTP surface
can only send known-safe tokens to a pane.

## Security model

- Binds `127.0.0.1` only. It is **not** on the LAN until you expose it.
- LAN reachability comes from the existing `expose-port`/WSLExpose hop
  (`10.10.69.99:PORT -> 127.0.0.1:PORT`), which is **house-LAN only, never the tailnet**.
- **Device lock:** scope the `WSL-Expose <port>` Windows firewall rule to your two
  fixed device IPs (see below). That is the real "only my phone + laptop" enforcement,
  done at the firewall before traffic reaches the app.
- **App auth:** one shared password -> a 30-day HMAC-signed HttpOnly cookie
  ("super long login session"). Password comes from `HERD_REMOTE_PASSWORD` or
  `~/.config/herd-remote/password` (mode 0600).
- **Caveat - plain HTTP:** the LAN hop is unencrypted HTTP, so the password and
  session cookie are visible to anyone who can sniff your LAN. The device-IP
  firewall scope above is the mitigation. If you want encryption, front it with a
  reverse proxy that terminates TLS (or run it behind Tailscale) and set the cookie
  `Secure`. Authenticated users have full terminal authority by design - only give
  the password to people you'd hand a keyboard.

## Quick start

```bash
go build -o herd-remote .
# option A: one-shot installer (build + password + service + expose)
./deploy/setup.sh
# option B: run it manually
HERD_REMOTE_PASSWORD=yourpass ./herd-remote        # http://127.0.0.1:8787
expose-port add 8787                                # publish to the house LAN
```

Then on your phone/laptop: `http://10.10.69.99:8787`

### Lock it to your two devices (recommended)

In an **elevated** PowerShell on Windows (fixed LAN IPs of your phone + laptop):

```powershell
Set-NetFirewallRule -DisplayName 'WSL-Expose 8787' -RemoteAddress '10.10.69.AA','10.10.69.BB'
```

## Config

| Setting | Env | Default |
|---|---|---|
| Listen address | `HERD_REMOTE_ADDR` | `127.0.0.1:8787` |
| Password | `HERD_REMOTE_PASSWORD` | else `~/.config/herd-remote/password` |

`~/.config/herd-remote/secret` is a generated HMAC key; deleting it logs everyone out.

## API (all under a session cookie except `/api/login`)

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/login` | `{password}` -> sets cookie |
| GET | `/api/sessions` | list agent sessions |
| GET | `/api/sessions/{pane}/read?lines=N` | screen text |
| POST | `/api/sessions/{pane}/prompt` | `{text}` typed + Enter |
| POST | `/api/sessions/{pane}/key` | `{key}` one allowed control key |
| POST | `/api/sessions/{wid}/rename` | `{label}` nav label |
| POST | `/api/sessions/{wid}/close` | kill workspace |
| GET | `/api/folders` | spawn target folders |
| POST | `/api/spawn` | `{dir,name,prompt,model,agent,background}` |

## Requirements

- `herdr` and `herd-spawn` on `PATH` (this repo assumes `~/.local/bin`).
- Go 1.18+ to build.
- WSL2 with the WSLExpose hop installed (`expose-port install`) for LAN access.
