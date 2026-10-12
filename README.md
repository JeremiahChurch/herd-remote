# herd-remote

A tiny phone-friendly web control surface for [herdr](https://herdr.dev) sessions.

Spawn a new agent session in any folder, see which sessions are **idle / working /
blocked**, read a session's screen, and do the minimal interactions that matter from
your phone: approve/deny a prompt, interrupt a hang, `/clear`, send a prompt, rename,
or kill a session. Built for the case where you're away from the keyboard and a Codex,
Claude, or Hermes session is stuck waiting on you.

It's a single Go binary that shells out to the `herdr` socket-API CLI (and the
`herd-spawn` helper). No daemon of its own beyond an HTTP server; herdr does the work.

## What it does

- **Session list** - every agent-bearing pane, colored by status, blocked ones first
  (blocked pulses red). Auto-refreshes.
- **Detail view** - live scrollback (last ~50 lines, polled) plus control buttons:
  - `Enter` (approve default), `Esc` (deny/cancel), `C-c` (kick a hang)
  - `▲ ▼` to navigate a Codex approval menu, `y` / `n` / `1` `2` `3` for direct answers
  - `/clear`, free-text prompt box, rename, and close (kill).
  - a **command / key picker** (dropdown) for less-common inputs (`/compact`, `/context`,
    `/usage`, `Space`, `Tab`, `Backspace`) so the button grid stays small. Its
    contents follow the agent: Hermes gets `/queue`, `/steer`, `/busy queue`,
    `/compress`, and `/reasoning <level>` in place of the Claude/Codex set.
  - a **Queue** button beside Send on Hermes sessions. It sends `/queue <your text>`,
    which hands Hermes the prompt for its next turn instead of interrupting the one
    already running - the phone equivalent of not talking over it.
  - scrollback reads `--source recent`, so you can scroll well past the visible screen.
  - a **⧉ Copy** toggle freezes the scrollback into a plain read-only textarea (native,
    well-behaved mobile text selection) and **pauses polling** so nothing shifts under your
    finger while you select. Tap **✓ Done** to return to the live colored view.
  - the agent's full-width input-box rules (`───`/`━━━`) are collapsed to a short divider
    so the prompt line stays near the bottom instead of wrapping off-screen on a phone.
- **Spawn** - optional session name, pick a folder under `$HOME` (filterable), optional
  first prompt, and a **model dropdown that also picks the agent** (Claude: Opus/Sonnet/
  Fable/Haiku via Claude Code's aliases, which always resolve to the newest model in
  each family; Codex: read live from codex's own `~/.codex/models_cache.json`, plus a
  reasoning-effort picker with that model's levels; Hermes: one entry, since it reads
  model and reasoning from `~/.hermes/config.yaml`). New models appear without a
  herd-remote release. One **Launch** button, tinted
  to the chosen agent. The spawn name is passed to Claude's own `--name` so it shows
  inside Claude too, not just in herdr. After spawning it jumps straight into the new
  session and clears the form.
- **Resume** - lists past sessions read straight from disk (Claude
  `~/.claude/projects/*/<uuid>.jsonl`, titled by the `ai-title` record; Codex
  `~/.codex/session_index.jsonl`; Hermes `~/.hermes/state.db`), most-recent first,
  filterable by folder/name and by a **Claude / Codex / Hermes** segmented toggle. Tap
  one to relaunch it (`claude --resume <id>` / `codex resume <id>` /
  `hermes --resume <id>`) in a fresh herdr workspace.

## Remote machines

herdr 0.9 can save SSH machines (`herdr machine add <ssh-host> --label "Shop PC"`).
herd-remote picks up every **enabled** saved machine on its own - no config here.

- **Session list** shows sessions from this box and every saved machine. A remote
  session carries a yellow machine tag (local ones carry none). If a machine doesn't
  answer within a few seconds its sessions are left out and a "unreachable" line says so,
  so an offline machine never blanks the list.
- **Detail view** works the same on a remote session: scrollback, keys, prompts, rename
  and kill all go through `herdr --machine <id> ...`, over herdr's own SSH link.
- **Spawn** gets a Local / <machine> picker once a machine is saved. Picking a machine
  lists that machine's folders and launches with that machine's own `herd-spawn`. Both go
  over plain `ssh <target>` (herdr doesn't forward helper scripts), so the machine needs
  `~/.local/bin/herd-spawn` and an SSH key this box can use non-interactively. The Launch
  button flags an agent CLI the machine doesn't appear to have.
- **Resume** is local only: it reads this box's session files.

Pane and workspace ids are per herdr server (both machines can have a `w2:p1`), so every
session call carries `?machine=<id>`; the server only accepts ids of enabled saved
machines.

## Hermes notes

Hermes works like the other two, with a few differences worth knowing:

- **Install the herdr plugin once:** `herdr integration install hermes`. That is what
  makes a Hermes pane report idle/working/blocked, so it shows up in the session list at
  all - and it's what herd-spawn waits on before typing a seed prompt.
- **Seed prompts are typed, not passed.** Hermes' REPL takes no positional first prompt
  (`-q`/`-z` are non-interactive one-shots), so herd-spawn waits for the session to come
  up and then types it in.
- **Submitting takes a separate Enter.** `herdr pane run` writes the text and the Enter
  in one go; Hermes reads that as a paste, so the Enter lands as a newline and the prompt
  sits unsent. Prompts to a Hermes pane are sent as `pane send-text` followed by a
  `send-keys Enter`, which is what actually submits.
- Sessions launch in Hermes' classic REPL (its default without `--tui`): line-based
  output reads far better through the phone's scrollback than a full-screen TUI.

All control keys are validated against an allowlist server-side, so the HTTP surface
can only send known-safe tokens to a pane.

## Security model

- Binds `127.0.0.1` only. It is **not** on the LAN until you expose it.
- LAN reachability comes from the existing `expose-port`/WSLExpose hop
  (`10.10.69.99:PORT -> 127.0.0.1:PORT`), which is **house-LAN only, never the tailnet**.
  `herd-remote-expose.service` re-applies that hop at every boot (see below).
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

### Boot behavior (why the LAN port needs re-applying)

`setup.sh` installs **two** user units:

| Unit | Role |
|---|---|
| `herd-remote.service` | runs the binary on `127.0.0.1:PORT` |
| `herd-remote-expose.service` | oneshot; re-applies the LAN portproxy once that socket is up |

The second one exists because the `WSLExpose` scheduled task is triggered at Windows
**logon** - before WSL boots and before herd-remote binds. At that point
`wsl-expose.ps1`'s loopback probe finds nothing answering and falls back to its default
`v4tov6 -> ::1:PORT` mapping. herd-remote binds IPv4-only, so that mapping never
connects and the dash is dead on the LAN until someone re-runs `expose-port add` by hand.

`herd-remote-expose.service` waits for the listener plus WSL interop, re-runs the
reconcile so the probe succeeds (`v4tov4 -> 127.0.0.1:PORT`), then verifies the mapping
and retries if the elevated task lags. It is ordered `After=herd-remote.service` and
pulled in by `Wants=`, so it also re-runs on a manual restart.

```bash
systemctl --user status herd-remote-expose      # did boot exposure work?
journalctl --user -u herd-remote-expose -n 20   # why not
systemctl --user restart herd-remote-expose     # re-apply by hand
```

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
| GET | `/api/history?dir=&limit=` | past sessions (all agents) for resume |
| POST | `/api/spawn` | `{dir,name,prompt,model,effort,agent,resume,background}` |

## Requirements

- `herdr` and `herd-spawn` on `PATH` (this repo assumes `~/.local/bin`).
- Go 1.18+ to build.
- `python3` on `PATH` for the Hermes resume list - its sessions live in a WAL-mode
  SQLite store, and reading it through python3's stdlib keeps this binary dependency-free.
- WSL2 with the WSLExpose hop installed (`expose-port install`) for LAN access.

### Agent launchers (how sessions actually start)

Spawns/resumes run through `herd-spawn`, which launches your agent CLIs. By default it uses
the raw CLIs with full-auto permissions, so **no personal aliases are required**:

| Agent | Env override | Default |
|---|---|---|
| Claude | `HERD_CLAUDE_CMD` | `claude --dangerously-skip-permissions` |
| Codex | `HERD_CODEX_CMD` | `codex --yolo` |

Point these at your own wrapper if you have one - e.g. Jeremiah's boxes use `ccd` / `codexd`
aliases for the same commands:

```bash
export HERD_CLAUDE_CMD='ccd'      # your bypass-perms Claude wrapper
export HERD_CODEX_CMD='codexd'    # your full-auto Codex wrapper
```

These grant the agent full permissions in the spawned session by design - only spawn into
folders you trust, same as running the CLI yourself.
