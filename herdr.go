package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// herdr.go wraps the `herdr` socket-API CLI and the `herd-spawn` helper.
// Every call shells out to the already-running herdr server, so this process
// needs no privileged access beyond the user's own herdr socket.

const (
	herdrBin  = "herdr"      // resolved from PATH (~/.local/bin/herdr)
	spawnBin  = "herd-spawn" // resolved from PATH
	cliTimout = 15 * time.Second
)

// Session is the flattened view the UI consumes: one agent-bearing pane.
// Pane and workspace ids are scoped to one herdr server, so a session is identified by
// (Machine, PaneID): the Shop PC and this box can both have a w2:p1.
type Session struct {
	Machine      string `json:"machine"`       // saved herdr machine id; "" = this box
	MachineLabel string `json:"machine_label"` // that machine's label, e.g. "Shop PC"
	WorkspaceID  string `json:"workspace_id"`
	PaneID       string `json:"pane_id"`
	Label        string `json:"label"`   // workspace label = what shows in herdr's nav
	Name         string `json:"name"`    // agent-reported name (often folder/worktree)
	Agent        string `json:"agent"`   // claude | codex | ...
	Status       string `json:"status"`  // idle | working | blocked | done | unknown
	Cwd          string `json:"cwd"`     // foreground cwd
	Number       int    `json:"number"`  // stable-ish nav number
	Focused      bool   `json:"focused"` // currently focused in the TUI
}

// run executes a herdr subcommand and returns raw stdout.
func run(bin string, args ...string) ([]byte, error) {
	return runT(cliTimout, bin, args...)
}

// runT is run with an explicit timeout.
func runT(timeout time.Duration, bin string, args ...string) ([]byte, error) {
	ctx, cancel := contextWithTimeout(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("%s %s: %s", bin, strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return out, nil
}

// --- remote machines ---
//
// herdr 0.9 saves SSH machines (`herdr machine add`) and forwards any API command to one
// with the global `--machine <id>` prefix, over its own persistent SSH link. Every pane
// operation here goes through that, so a Shop PC pane is driven exactly like a local one.
// Two things herdr does not forward: listing folders and running herd-spawn. Those go
// over plain ssh to the machine's saved target (see sshRun).

// Machine is one enabled saved herdr machine.
type Machine struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Target string `json:"-"` // ssh destination (an ~/.ssh/config Host); never sent to the phone
}

// remoteListTimeout bounds one machine's part of the session list, so an offline Shop PC
// costs the phone a few seconds instead of the full CLI timeout on every refresh.
const remoteListTimeout = 6 * time.Second

var machinesCache = struct {
	sync.Mutex
	at   time.Time
	list []Machine
}{}

// Machines lists enabled saved machines. Cached briefly: it is a local config read, but
// it sits in front of every remote request for id validation.
func Machines() []Machine {
	machinesCache.Lock()
	defer machinesCache.Unlock()
	if machinesCache.list != nil && time.Since(machinesCache.at) < 30*time.Second {
		return machinesCache.list
	}
	list := []Machine{}
	if out, err := run(herdrBin, "machine", "list", "--json"); err == nil {
		var raw []struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			Target  string `json:"target"`
			Enabled bool   `json:"enabled"`
		}
		if json.Unmarshal(out, &raw) == nil {
			for _, m := range raw {
				if m.Enabled && reMachineID.MatchString(m.ID) {
					list = append(list, Machine{ID: m.ID, Label: m.Label, Target: m.Target})
				}
			}
		}
	}
	machinesCache.at, machinesCache.list = time.Now(), list
	return list
}

// reMachineID: herdr machine ids are hex digests. Checked before an id becomes argv.
var reMachineID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// reSSHTarget guards the ssh destination; no leading '-' so it can never read as a flag.
var reSSHTarget = regexp.MustCompile(`^[A-Za-z0-9_.@][A-Za-z0-9_.@-]*$`)

// lookupMachine resolves a machine id from the phone. "" is this box and always valid;
// anything else must be an enabled saved machine, so the HTTP layer can't point
// `--machine` or ssh at an arbitrary host.
func lookupMachine(id string) (Machine, error) {
	if id == "" {
		return Machine{}, nil
	}
	for _, m := range Machines() {
		if m.ID == id {
			return m, nil
		}
	}
	return Machine{}, fmt.Errorf("unknown machine: %q", id)
}

// herdrOn runs a herdr API command on a machine ("" = this box).
func herdrOn(machine string, args ...string) ([]byte, error) {
	return herdrOnT(cliTimout, machine, args...)
}

func herdrOnT(timeout time.Duration, machine string, args ...string) ([]byte, error) {
	if machine != "" {
		args = append([]string{"--machine", machine}, args...)
	}
	return runT(timeout, herdrBin, args...)
}

// shellQuote single-quotes s for the remote shell ssh hands its command line to.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshRun runs a command line on a machine's ssh target. BatchMode so a missing key or
// an unapproved 1Password prompt fails fast instead of hanging the request.
func sshRun(m Machine, timeout time.Duration, cmdline string) ([]byte, error) {
	if !reSSHTarget.MatchString(m.Target) {
		return nil, fmt.Errorf("machine %q has no usable ssh target", m.Label)
	}
	return runT(timeout, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", m.Target, cmdline)
}

// MachineStatus reports whether a machine's sessions made it into the list.
type MachineStatus struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Error string `json:"error,omitempty"`
}

// --- JSON shapes emitted by herdr (only the fields we use) ---

type agentListResp struct {
	Result struct {
		Agents []struct {
			Agent         string `json:"agent"`
			AgentStatus   string `json:"agent_status"`
			Cwd           string `json:"cwd"`
			ForegroundCwd string `json:"foreground_cwd"`
			Focused       bool   `json:"focused"`
			Name          string `json:"name"`
			PaneID        string `json:"pane_id"`
			WorkspaceID   string `json:"workspace_id"`
		} `json:"agents"`
	} `json:"result"`
}

type workspaceListResp struct {
	Result struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
			Number      int    `json:"number"`
			AgentStatus string `json:"agent_status"`
			Focused     bool   `json:"focused"`
		} `json:"workspaces"`
	} `json:"result"`
}

type readResp struct {
	Result struct {
		Read struct {
			Text   string `json:"text"`
			PaneID string `json:"pane_id"`
		} `json:"read"`
	} `json:"result"`
}

// ListSessions gathers agent sessions from this box and every enabled saved machine.
// A local failure is an error (herdr itself is down); a remote failure only drops that
// machine's sessions and is reported in the returned statuses, so an offline Shop PC
// never blanks the phone's list.
func ListSessions() ([]Session, []MachineStatus, error) {
	machines := Machines()
	type res struct {
		sessions []Session
		err      error
	}
	results := make([]res, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func(i int, m Machine) {
			defer wg.Done()
			ss, err := listOn(m, remoteListTimeout)
			results[i] = res{ss, err}
		}(i, m)
	}
	out, err := listOn(Machine{}, cliTimout)
	wg.Wait()
	if err != nil {
		return nil, nil, err
	}
	statuses := make([]MachineStatus, 0, len(machines))
	for i, m := range machines {
		st := MachineStatus{ID: m.ID, Label: m.Label}
		if results[i].err != nil {
			st.Error = results[i].err.Error()
		} else {
			out = append(out, results[i].sessions...)
		}
		statuses = append(statuses, st)
	}

	// Sort: blocked first (needs attention), then working, then local before remote,
	// then by nav number.
	rank := func(st string) int {
		switch st {
		case "blocked":
			return 0
		case "working":
			return 1
		case "idle":
			return 2
		case "done":
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].Status), rank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		if out[i].Machine != out[j].Machine {
			return out[i].Machine < out[j].Machine // "" (local) sorts first
		}
		return out[i].Number < out[j].Number
	})
	return out, statuses, nil
}

// listOn merges `herdr agent list` (agent-bearing panes) with `herdr workspace list`
// (nav labels + numbers) for one machine, keyed by workspace_id.
func listOn(m Machine, timeout time.Duration) ([]Session, error) {
	aOut, err := herdrOnT(timeout, m.ID, "agent", "list")
	if err != nil {
		return nil, err
	}
	var al agentListResp
	if err := json.Unmarshal(aOut, &al); err != nil {
		return nil, fmt.Errorf("parse agent list: %w", err)
	}
	if m.ID != "" {
		cacheAgents(m.ID, al)
	}

	// Workspace labels/numbers (best-effort; UI still works without them).
	labels := map[string]string{}
	numbers := map[string]int{}
	if wOut, err := herdrOnT(timeout, m.ID, "workspace", "list"); err == nil {
		var wl workspaceListResp
		if json.Unmarshal(wOut, &wl) == nil {
			for _, w := range wl.Result.Workspaces {
				labels[w.WorkspaceID] = w.Label
				numbers[w.WorkspaceID] = w.Number
			}
		}
	}

	out := make([]Session, 0, len(al.Result.Agents))
	for _, a := range al.Result.Agents {
		cwd := a.ForegroundCwd
		if cwd == "" {
			cwd = a.Cwd
		}
		s := Session{
			Machine:      m.ID,
			MachineLabel: m.Label,
			WorkspaceID:  a.WorkspaceID,
			PaneID:       a.PaneID,
			Label:        labels[a.WorkspaceID],
			Name:         a.Name,
			Agent:        a.Agent,
			Status:       a.AgentStatus,
			Cwd:          cwd,
			Number:       numbers[a.WorkspaceID],
			Focused:      a.Focused,
		}
		if s.Label == "" {
			s.Label = a.Name
		}
		if s.Label == "" {
			s.Label = filepath.Base(cwd)
		}
		out = append(out, s)
	}
	return out, nil
}

// ReadPane returns the visible screen text for a pane.
func ReadPane(machine, pane string, lines int) (string, error) {
	if lines <= 0 {
		lines = 40
	}
	// --source recent pulls scrollback history (not just the visible screen), so the
	// phone can scroll well past the current viewport; --format ansi keeps SGR colors.
	out, err := herdrOn(machine, "agent", "read", pane, "--source", "recent", "--lines", fmt.Sprint(lines), "--format", "ansi")
	if err != nil {
		return "", err
	}
	// herdr 0.9 prints the pane text raw; older servers wrapped it in a JSON envelope.
	if !bytes.HasPrefix(bytes.TrimSpace(out), []byte(`{"id"`)) {
		return string(out), nil
	}
	var r readResp
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("parse read: %w", err)
	}
	return r.Result.Read.Text, nil
}

// SendPrompt types text into a pane and submits it (the submit primitive).
//
// claude/codex take `herdr pane run`, which writes the text and the Enter in one go.
// Hermes' input reads that as a paste: the trailing Enter becomes a literal newline and
// the prompt just sits in the composer unsent. For hermes, write the text and then send
// Enter as its own key event - that is what actually submits.
func SendPrompt(machine, pane, text string) error {
	if paneAgent(machine, pane) == "hermes" {
		if _, err := herdrOn(machine, "pane", "send-text", pane, text); err != nil {
			return err
		}
		time.Sleep(400 * time.Millisecond) // let the composer settle before the submit key
		_, err := herdrOn(machine, "pane", "send-keys", pane, "Enter")
		return err
	}
	_, err := herdrOn(machine, "pane", "run", pane, text)
	return err
}

// paneAgent resolves which agent owns a pane, so SendPrompt can pick the right submit
// shape. Best-effort: "" when herdr is unreachable or the pane has no agent, which lands
// on the claude/codex path (what every pane used before hermes existed).
//
// Cached briefly because a pane's agent doesn't change under a live session, and this
// sits in front of every prompt send - one `agent list` per prompt would otherwise stack
// on top of the two the list view already makes.
//
// Keyed per machine, since pane ids repeat across herdr servers.
type agentCache struct {
	at   time.Time
	byID map[string]string
}

var paneAgentCache = struct {
	sync.Mutex
	m map[string]agentCache
}{m: map[string]agentCache{}}

const paneAgentTTL = 30 * time.Second

func paneAgent(machine, pane string) string {
	paneAgentCache.Lock()
	c, ok := paneAgentCache.m[machine]
	paneAgentCache.Unlock()
	if ok && time.Since(c.at) < paneAgentTTL {
		return c.byID[pane]
	}
	out, err := herdrOn(machine, "agent", "list")
	if err != nil {
		return ""
	}
	var al agentListResp
	if json.Unmarshal(out, &al) != nil {
		return ""
	}
	return cacheAgents(machine, al)[pane]
}

// cacheAgents records a machine's pane -> agent map from an agent list we already have.
func cacheAgents(machine string, al agentListResp) map[string]string {
	byID := make(map[string]string, len(al.Result.Agents))
	for _, a := range al.Result.Agents {
		byID[a.PaneID] = a.Agent
	}
	paneAgentCache.Lock()
	paneAgentCache.m[machine] = agentCache{at: time.Now(), byID: byID}
	paneAgentCache.Unlock()
	return byID
}

// SendKey sends a single verified control key to a pane.
// Allowed set is enforced here so the HTTP layer can't inject arbitrary tokens.
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "C-c": true,
	"Up": true, "Down": true, "Left": true, "Right": true,
	// No "Shift+Tab": herdr accepts the name but sends a plain \t, so it never cycles
	// the permission mode. Sending Escape,[,Z by hand doesn't work either - herdr writes
	// each key separately, so the agent sees ESC then a literal "[Z" rather than CSI Z.
	"Tab": true, "Backspace": true, "Space": true,
	"y": true, "n": true, "1": true, "2": true, "3": true, "4": true, "5": true,
}

func SendKey(machine, pane, key string) error {
	if !allowedKeys[key] {
		return fmt.Errorf("key not allowed: %q", key)
	}
	_, err := herdrOn(machine, "pane", "send-keys", pane, key)
	return err
}

// Rename updates every place herdr shows a name for a session: the workspace
// label (left-nav), the agent label, and the pane label (both shown on pane
// borders / the agents panel). herdr has no way to rename an agent's *internal*
// session (Claude/Codex don't expose that), but this keeps all herdr surfaces
// consistent instead of leaving the stale folder name on the pane border.
func Rename(machine, workspaceID, paneID, label string) error {
	label = strings.TrimSpace(label)
	if err := validateLabel(label); err != nil {
		return err
	}
	// Workspace label is the authoritative nav name; fail if it errors.
	if _, err := herdrOn(machine, "workspace", "rename", workspaceID, label); err != nil {
		return err
	}
	// Agent + pane labels are best-effort (a pane may have no live agent).
	if paneID != "" {
		_, _ = herdrOn(machine, "agent", "rename", paneID, label)
		_, _ = herdrOn(machine, "pane", "rename", paneID, label)
	}
	return nil
}

// CloseWorkspace tears down a workspace (kills the session).
func CloseWorkspace(machine, workspaceID string) error {
	_, err := herdrOn(machine, "workspace", "close", workspaceID)
	return err
}

// codexEfforts is the allowlist for the codex reasoning-effort passthrough, so the
// HTTP layer can only hand herd-spawn a known-good value (it becomes a shell token).
// It is the union of every level codex's model list has offered; the per-model subset
// comes from CodexModels, so this only needs a new entry when codex invents a level.
var codexEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true, "ultra": true}

// CodexModel is one entry in the phone's Codex model dropdown.
type CodexModel struct {
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort"`
}

// codexFallbackModels is used only when codex's own cache is missing or unreadable.
var codexFallbackModels = []CodexModel{
	{Slug: "gpt-6.1-sol", Name: "GPT-6.1-Sol", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"},
	{Slug: "gpt-6-luna", Name: "GPT-6-Luna", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"},
}

// CodexModels lists the models codex itself would show in its /model picker. Codex
// refreshes ~/.codex/models_cache.json from OpenAI on its own, so reading it keeps the
// dropdown current with no herd-remote release. Hidden models are skipped; order is
// codex's own priority. Efforts are filtered through codexEfforts so the UI never
// offers a level the spawn path would reject.
func CodexModels() []CodexModel {
	home, err := os.UserHomeDir()
	if err != nil {
		return codexFallbackModels
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "models_cache.json"))
	if err != nil {
		return codexFallbackModels
	}
	var cache struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
			Priority    int    `json:"priority"`
			Default     string `json:"default_reasoning_level"`
			Levels      []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &cache) != nil {
		return codexFallbackModels
	}
	sort.SliceStable(cache.Models, func(i, j int) bool { return cache.Models[i].Priority < cache.Models[j].Priority })
	var out []CodexModel
	for _, m := range cache.Models {
		if m.Visibility != "list" || !reModelSlug.MatchString(m.Slug) {
			continue
		}
		cm := CodexModel{Slug: m.Slug, Name: m.DisplayName, DefaultEffort: m.Default}
		for _, l := range m.Levels {
			if codexEfforts[l.Effort] {
				cm.Efforts = append(cm.Efforts, l.Effort)
			}
		}
		if cm.Name == "" {
			cm.Name = m.Slug
		}
		out = append(out, cm)
	}
	if len(out) == 0 {
		return codexFallbackModels
	}
	return out
}

// reModelSlug guards a model id before it becomes a herd-spawn -m shell token.
var reModelSlug = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// reSessionID guards a resume target before it becomes a herd-spawn -r shell token.
// Claude ids are UUIDs and codex thread ids are UUID-ish (hex + dashes); hermes ids are
// timestamp-stamped like 20260824_155337_c0c98b, hence the underscore.
var reSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// Spawn creates a new workspace in dir and launches the chosen agent, optionally
// seeded. agent is "claude" (default) or "codex". name, when set, becomes the
// workspace label (herdr nav + this app's list) via herd-spawn's -l. effort is a
// codex-only reasoning-effort override (ignored for claude). resumeID, when set,
// resumes that existing session instead of starting fresh (model/effort are then the
// session's own, so they aren't re-sent).
//
// On a saved machine it runs that machine's own herd-spawn over ssh: herdr forwards API
// calls but not helper scripts, and herd-spawn has to check the folder and resolve the
// launcher on the box the agent will run on.
func Spawn(m Machine, dir, prompt, model, effort, agent, name, resumeID string, background bool) (string, error) {
	if agent == "" {
		agent = "claude"
	}
	if agent != "claude" && agent != "codex" && agent != "hermes" {
		return "", fmt.Errorf("unknown agent: %q", agent)
	}
	if resumeID != "" && !reSessionID.MatchString(resumeID) {
		return "", fmt.Errorf("bad resume id")
	}
	// A blank codex model is deliberate: codex then uses ~/.codex/config.toml's default,
	// same as a terminal launch.
	if model != "" && !reModelSlug.MatchString(model) {
		return "", fmt.Errorf("bad model: %q", model)
	}
	if effort != "" && !codexEfforts[effort] {
		return "", fmt.Errorf("unknown effort: %q", effort)
	}
	args := []string{"-a", agent}
	if background {
		args = append(args, "-b")
	}
	if resumeID != "" {
		args = append(args, "-r", resumeID)
	} else {
		if model != "" {
			args = append(args, "-m", model)
		}
		// effort maps to codex's model_reasoning_effort and hermes' --reasoning; claude
		// has no launch-time equivalent, so never send it there.
		if effort != "" && agent != "claude" {
			args = append(args, "-e", effort)
		}
	}
	if name = strings.TrimSpace(name); name != "" {
		args = append(args, "-l", name)
	}
	// `--` stops flag parsing so a dir like "--list" is treated as a path, not an option.
	args = append(args, "--", dir)
	if prompt != "" {
		args = append(args, prompt)
	}
	if m.ID == "" {
		out, err := run(spawnBin, args...)
		return strings.TrimSpace(string(out)), err
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	// ssh's non-interactive shell skips ~/.profile, so name the binary by path.
	out, err := sshRun(m, cliTimout, "~/.local/bin/herd-spawn "+strings.Join(quoted, " "))
	return strings.TrimSpace(string(out)), err
}

// Folders lists candidate spawn targets: immediate + one-level-deep dirs under
// $HOME (skipping dotdirs and junk), so the phone UI can present a picker.
func Folders() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{
		"node_modules": true, ".git": true, "vendor": true,
		"AppData": true, "Android": true, "Downloads": true, "downloads": true,
	}
	var dirs []string
	add := func(p string) { dirs = append(dirs, p) }

	top, err := os.ReadDir(home)
	if err != nil {
		return nil, err
	}
	for _, e := range top {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || skip[e.Name()] {
			continue
		}
		p := filepath.Join(home, e.Name())
		add(p)
		// one level deeper, so worktree/monorepo subdirs are reachable
		if sub, err := os.ReadDir(p); err == nil {
			for _, se := range sub {
				if !se.IsDir() || strings.HasPrefix(se.Name(), ".") || skip[se.Name()] {
					continue
				}
				add(filepath.Join(p, se.Name()))
			}
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// remoteFoldersSh prints the remote $HOME, then the same two-level folder listing Folders
// builds locally (dotdirs pruned; the junk-name skip happens in Go), then one
// "agent:<name>" line per agent CLI found, so the spawn form can flag a missing one.
const remoteFoldersSh = `echo "$HOME"
find "$HOME" -mindepth 1 -maxdepth 2 -name '.*' -prune -o -mindepth 1 -maxdepth 2 -type d -print 2>/dev/null
for a in claude codex hermes; do
  if [ -x "$HOME/.local/bin/$a" ] || command -v "$a" >/dev/null 2>&1; then echo "agent:$a"; fi
done`

// RemoteFolders lists spawn targets on a saved machine, plus its home dir (for ~ display)
// and which agent CLIs it has.
func RemoteFolders(m Machine) (dirs []string, home string, agents []string, err error) {
	out, err := sshRun(m, 10*time.Second, remoteFoldersSh)
	if err != nil {
		return nil, "", nil, err
	}
	skip := map[string]bool{
		"node_modules": true, "vendor": true,
		"AppData": true, "Android": true, "Downloads": true, "downloads": true,
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "/") {
		return nil, "", nil, fmt.Errorf("unexpected folder listing from %s", m.Label)
	}
	home = lines[0]
	for _, l := range lines[1:] {
		switch {
		case strings.HasPrefix(l, "agent:"):
			agents = append(agents, strings.TrimPrefix(l, "agent:"))
		case strings.HasPrefix(l, home+"/"):
			rel := strings.Split(strings.TrimPrefix(l, home+"/"), "/")
			bad := false
			for _, part := range rel {
				if skip[part] {
					bad = true
				}
			}
			if !bad {
				dirs = append(dirs, l)
			}
		}
	}
	sort.Strings(dirs)
	return dirs, home, agents, nil
}

// --- session history (resume) ---
//
// Both agents persist every session to disk, so we can list resumable past sessions
// without touching either TUI:
//   claude: ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl - the uuid IS the resume id;
//           name = last "ai-title" record; cwd = from the transcript. (herdr's
//           agent_session.value for a live claude pane equals this uuid.)
//   codex:  ~/.codex/session_index.jsonl - a prebuilt {id, thread_name, updated_at}
//           index. cwd isn't in it, so codex folder-filtering is best-effort.
//   hermes: ~/.hermes/state.db - a SQLite session store (id, title, cwd, last_activity_at).
//           Read through a python3 one-liner rather than a Go SQLite driver: this binary
//           has no module dependencies and the store is WAL-mode, so a hand-rolled reader
//           would miss anything still sitting in the -wal file.

// HistoryEntry is one resumable past session from an agent's on-disk store.
type HistoryEntry struct {
	Agent    string `json:"agent"`    // claude | codex | hermes
	ID       string `json:"id"`       // resume target
	Name     string `json:"name"`     // ai-title (claude) / thread_name (codex) / title (hermes)
	Cwd      string `json:"cwd"`      // claude + hermes: authoritative; codex: usually ""
	Modified int64  `json:"modified"` // unix seconds, for recency sort
}

// historyScanCap bounds how many of the most-recent claude transcripts we open to
// extract titles per request (each open is a file scan, so we cap the cost).
const historyScanCap = 80

// HistorySessions returns resumable past sessions from every agent, most-recent first.
// filter, when set (case-insensitive), matches on cwd for claude/hermes and on name for
// codex (codex has no cwd in its index). limit caps the returned list.
func HistorySessions(filter string, limit int) []HistoryEntry {
	if limit <= 0 {
		limit = 60
	}
	all := append(claudeHistory(), codexHistory()...)
	all = append(all, hermesHistory()...)
	if filter = strings.ToLower(strings.TrimSpace(filter)); filter != "" {
		kept := all[:0]
		for _, e := range all {
			hay := e.Cwd
			if e.Agent == "codex" {
				hay = e.Name
			}
			if strings.Contains(strings.ToLower(hay), filter) {
				kept = append(kept, e)
			}
		}
		all = kept
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Modified > all[j].Modified })
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// claudeHistory scans the most-recent transcripts under ~/.claude/projects.
func claudeHistory() []HistoryEntry {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".claude", "projects")
	proj, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type ref struct {
		path  string
		mtime time.Time
	}
	var refs []ref
	for _, pd := range proj {
		if !pd.IsDir() {
			continue
		}
		dir := filepath.Join(root, pd.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			refs = append(refs, ref{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	// newest first, then only open the top N (title extraction is a per-file scan)
	sort.Slice(refs, func(i, j int) bool { return refs[i].mtime.After(refs[j].mtime) })
	if len(refs) > historyScanCap {
		refs = refs[:historyScanCap]
	}
	out := make([]HistoryEntry, 0, len(refs))
	for _, r := range refs {
		name, cwd := claudeTitleAndCwd(r.path)
		out = append(out, HistoryEntry{
			Agent:    "claude",
			ID:       strings.TrimSuffix(filepath.Base(r.path), ".jsonl"),
			Name:     name,
			Cwd:      cwd,
			Modified: r.mtime.Unix(),
		})
	}
	return out
}

// claudeTitleAndCwd pulls the last "ai-title" and the first "cwd" from a transcript,
// scanning line-by-line so a multi-MB file isn't held fully parsed in memory.
func claudeTitleAndCwd(path string) (title, cwd string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // attachment lines can be large
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(`"ai-title"`)) {
			var t struct {
				AiTitle string `json:"aiTitle"`
			}
			if json.Unmarshal(line, &t) == nil && t.AiTitle != "" {
				title = t.AiTitle // keep the last one (most recent title)
			}
		} else if cwd == "" && bytes.Contains(line, []byte(`"cwd":`)) {
			var c struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &c) == nil {
				cwd = c.Cwd
			}
		}
	}
	return title, cwd
}

// codexHistory reads the prebuilt ~/.codex/session_index.jsonl index.
func codexHistory() []HistoryEntry {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".codex", "session_index.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []HistoryEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for sc.Scan() {
		var rec struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
			UpdatedAt  string `json:"updated_at"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.ID == "" {
			continue
		}
		var mod int64
		if t, err := time.Parse(time.RFC3339, rec.UpdatedAt); err == nil {
			mod = t.Unix()
		}
		out = append(out, HistoryEntry{
			Agent: "codex", ID: rec.ID, Name: rec.ThreadName, Modified: mod,
		})
	}
	return out
}

// hermesStatePy dumps ~/.hermes/state.db's resumable sessions as JSON lines.
//
// Archived/hidden rows and empty shells are skipped, as is source "tool" (hermes' own
// `sessions list` hides those too - they're subagent scratch, not something to resume).
// The read is read-only; the temp-copy fallback covers the case where SQLite refuses a
// read-only open because the WAL needs recovery and no hermes process is live to do it.
const hermesStatePy = `
import json, os, shutil, sqlite3, sys, tempfile

db = os.path.expanduser("~/.hermes/state.db")
if not os.path.exists(db):
    sys.exit(0)

Q = ("select id, coalesce(title, ''), coalesce(cwd, ''), "
     "coalesce(last_activity_at, started_at, 0) from sessions "
     "where coalesce(archived, 0) = 0 and coalesce(hidden, 0) = 0 "
     "and coalesce(message_count, 0) > 0 and coalesce(source, '') != 'tool' "
     "order by 4 desc limit 200")

def rows(path, readonly=True):
    uri = "file:%s%s" % (path, "?mode=ro" if readonly else "")
    con = sqlite3.connect(uri, uri=True, timeout=2)
    try:
        return con.execute(Q).fetchall()
    finally:
        con.close()

try:
    found = rows(db)
except Exception:
    tmp = tempfile.mkdtemp()
    try:
        for ext in ("", "-wal", "-shm"):
            if os.path.exists(db + ext):
                shutil.copy2(db + ext, os.path.join(tmp, "state.db" + ext))
        found = rows(os.path.join(tmp, "state.db"), readonly=False)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

for sid, title, cwd, ts in found:
    print(json.dumps({"id": sid, "title": title, "cwd": cwd, "modified": int(ts or 0)}))
`

// hermesHistory lists resumable hermes sessions. Best-effort: any failure (no hermes
// installed, no python3, unreadable store) yields no hermes entries rather than an error,
// same as the other two readers.
func hermesHistory() []HistoryEntry {
	out, err := run("python3", "-c", hermesStatePy)
	if err != nil {
		return nil
	}
	var entries []HistoryEntry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for sc.Scan() {
		var rec struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Cwd      string `json:"cwd"`
			Modified int64  `json:"modified"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.ID == "" {
			continue
		}
		entries = append(entries, HistoryEntry{
			Agent: "hermes", ID: rec.ID, Name: rec.Title, Cwd: rec.Cwd, Modified: rec.Modified,
		})
	}
	return entries
}
