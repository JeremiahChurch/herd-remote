package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Name sync: a herdr panel follows its agent's own session name.
//
// Claude, Codex and Muse write their session name into the terminal title, and herdr
// reports that title for every pane (Claude "✳ <name>", Codex "<folder> | <thread>",
// Muse "<name>"). A /rename inside the agent changes the title, and the loop below
// copies the new name onto the herdr workspace, agent and pane labels. Renaming from
// the phone goes the other way round: it types the agent's own rename command (see
// RenameSession), and this loop then has nothing left to do.
//
// Only a CHANGE of title renames a panel. Panes already open when the server starts
// are recorded as they are, so a restart never relabels every panel at once, and a
// name typed straight into herdr stays until the agent's name next changes.
//
// Hermes keeps its /title name out of the terminal title, so herdr cannot see it and
// Hermes panels are only renamed from the phone.

const nameSyncInterval = 5 * time.Second

// genericTitles are the titles an agent shows before it has a session name.
var genericTitles = map[string]bool{
	"claude code": true, "claude": true, "codex": true, "muse": true, "muse code": true, "hermes": true,
}

// reIDTitle matches an unnamed Codex thread, whose title part is the thread id.
var reIDTitle = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-`)

// syncName returns the panel name an agent title asks for, or "" when the title is not
// a session name: empty, a placeholder, a bare thread id, or just the folder name
// (Muse's title before a rename).
func syncName(s Session) string {
	t := strings.TrimSpace(s.Title)
	if t == "" || genericTitles[strings.ToLower(t)] || reIDTitle.MatchString(t) {
		return ""
	}
	if s.Cwd != "" && t == filepath.Base(s.Cwd) {
		return ""
	}
	return sanitizeLabel(t)
}

// renameAction is one panel the loop relabels.
type renameAction struct {
	Machine, WorkspaceID, PaneID, Label string
	Workspace                           bool // also rename the workspace (it holds no other agent)
}

type nameState struct {
	applied string // the name this panel last followed
	cand    string // a new name waiting for a second sighting
	retry   bool   // the last rename failed part way: send it even if one label matches
}

// nameSyncer remembers the last name seen for every pane. Not safe for concurrent
// ticks; the loop runs one at a time.
type nameSyncer struct {
	primed map[string]bool       // machines whose first listing has been taken as is
	panes  map[string]*nameState // machine + "/" + pane id
}

func newNameSyncer() *nameSyncer {
	return &nameSyncer{primed: map[string]bool{}, panes: map[string]*nameState{}}
}

// failed forgets a rename that did not fully go through, so the next ticks send it
// again. retry matters when the workspace label took but the agent or pane label did
// not: the listing then shows the new name, and without it the stale labels would stay.
func (ns *nameSyncer) failed(a renameAction) {
	if st := ns.panes[a.Machine+"/"+a.PaneID]; st != nil {
		st.applied, st.retry = "", true
	}
}

// tick takes one listing and returns the panels to relabel. A new name has to show
// on two listings in a row, so a title caught mid-redraw never renames anything.
// answered holds the machines that listed their sessions ("" = this box); a pane on a
// machine that did not answer is kept, so its return is not mistaken for a new pane.
// Priming is per machine: a machine that was offline at startup, or added later, has
// its panes taken as they are on its first answer, like this box's at startup.
func (ns *nameSyncer) tick(sessions []Session, answered map[string]bool) []renameAction {
	agentsIn := map[string]int{}
	for _, s := range sessions {
		agentsIn[s.Machine+"/"+s.WorkspaceID]++
	}
	seen := map[string]bool{}
	var out []renameAction
	for _, s := range sessions {
		key := s.Machine + "/" + s.PaneID
		seen[key] = true
		name := syncName(s)
		st := ns.panes[key]
		if st == nil {
			st = &nameState{}
			ns.panes[key] = st
			if !ns.primed[s.Machine] {
				st.applied = name // open when its machine first answered: take it as it is
				continue
			}
		}
		if name == "" || name == st.applied {
			st.cand = ""
			continue
		}
		if name != st.cand {
			st.cand = name
			continue
		}
		st.applied, st.cand = name, ""
		whole := agentsIn[s.Machine+"/"+s.WorkspaceID] == 1
		// Label is the workspace's name; in a shared workspace the pane's own herdr
		// name is the agent name.
		current := s.Label
		if !whole {
			current = s.Name
		}
		if name == current && !st.retry {
			continue
		}
		st.retry = false
		out = append(out, renameAction{
			Machine: s.Machine, WorkspaceID: s.WorkspaceID, PaneID: s.PaneID, Label: name,
			Workspace: whole,
		})
	}
	for key := range ns.panes {
		machine, _, _ := strings.Cut(key, "/")
		if !seen[key] && answered[machine] {
			delete(ns.panes, key)
		}
	}
	for m := range answered {
		ns.primed[m] = true
	}
	return out
}

// startNameSync runs the loop for the life of the server. HERD_REMOTE_NAME_SYNC=0
// turns it off.
func startNameSync() {
	if os.Getenv("HERD_REMOTE_NAME_SYNC") == "0" {
		log.Printf("name sync: off (HERD_REMOTE_NAME_SYNC=0)")
		return
	}
	go func() {
		ns := newNameSyncer()
		for {
			sessions, statuses, err := ListSessions()
			if err == nil {
				answered := map[string]bool{"": true}
				for _, m := range statuses {
					if m.Error == "" {
						answered[m.ID] = true
					}
				}
				for _, a := range ns.tick(sessions, answered) {
					if err := renamePanel(a); err != nil {
						ns.failed(a)
						log.Printf("name sync: %s %s -> %q: %v", a.Machine, a.PaneID, a.Label, err)
					} else {
						log.Printf("name sync: %s %s -> %q", a.Machine, a.PaneID, a.Label)
					}
				}
			}
			time.Sleep(nameSyncInterval)
		}
	}()
}

// renamePanel puts a name on the agent and pane labels, and on the workspace when the
// workspace holds only this agent (a shared workspace keeps its own name). Any failed
// call is an error, so the loop sends the whole rename again; each call is idempotent.
func renamePanel(a renameAction) error {
	if err := validateLabel(a.Label); err != nil {
		return err
	}
	if a.Workspace {
		if _, err := herdrOn(a.Machine, "workspace", "rename", a.WorkspaceID, a.Label); err != nil {
			return err
		}
	}
	if _, err := herdrOn(a.Machine, "agent", "rename", a.PaneID, a.Label); err != nil {
		return fmt.Errorf("agent label: %w", err)
	}
	if _, err := herdrOn(a.Machine, "pane", "rename", a.PaneID, a.Label); err != nil {
		return fmt.Errorf("pane label: %w", err)
	}
	return nil
}

// renameCommands is each agent's own command for naming its session. Every one was
// checked live: Claude, Codex and Muse take "/rename <name>" in one line and show the
// new name in the terminal title at once; Hermes takes "/title <name>" (saved with the
// session's first message if it has none yet).
var renameCommands = map[string]string{
	"claude": "/rename", "codex": "/rename", "muse": "/rename", "hermes": "/title",
}

// errLabelsPending means the agent took the new name but herdr's labels had not all
// changed when the phone was answered. The rename command is not typed again; the
// phone is told herdr may still show the old name.
type errLabelsPending struct{ err error }

func (e errLabelsPending) Error() string {
	return "renamed in the session, but herdr may still show the old name: " + e.err.Error()
}

// Labelling herdr after the agent took a name: up to labelTries attempts, labelGap
// apart, in the background. The phone waits labelWait at most, well inside its own
// 15s request timeout (a herdr call alone may take up to cliTimout), so a slow herdr
// shows as a warning rather than a timed-out request the user would repeat.
var (
	labelTries = 3
	labelGap   = 2 * time.Second
	labelWait  = 4 * time.Second
)

var errLabelsSlow = fmt.Errorf("herdr is slow to answer; still retrying")

// errSuperseded ends a label retry once a newer rename of the same pane has started,
// so an older, slower rename never writes its name over the newer one.
var errSuperseded = fmt.Errorf("superseded by a newer rename")

// labelGens counts renames per pane (machine + "/" + pane id).
var labelGens = struct {
	sync.Mutex
	m map[string]uint64
}{m: map[string]uint64{}}

// paneLocks serialize phone renames of one pane: typing the rename command and each
// attempt at herdr's labels hold it. A rename's generation is taken under the same
// lock as its command, so generation order is command order, and every label attempt
// rechecks its generation under the lock - an older rename can never write after a
// newer one has, even if its herdr calls were mid-flight when the newer one started.
var paneLocks sync.Map // machine + "/" + pane id -> *sync.Mutex

func paneLock(key string) *sync.Mutex {
	mu, _ := paneLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// errRenameRunning refuses a phone rename while another rename of the same pane
// still holds herdr up, so the request never queues past the phone's timeout.
var errRenameRunning = fmt.Errorf("another rename of this session is still running - try again in a moment")

// lockWithin takes mu if it frees up within d.
func lockWithin(mu *sync.Mutex, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !mu.TryLock() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}

// newLabelGen starts a rename of key and returns a check that stays true until the
// next rename of the same key starts.
func newLabelGen(key string) (current func() bool) {
	labelGens.Lock()
	labelGens.m[key]++
	gen := labelGens.m[key]
	labelGens.Unlock()
	return func() bool {
		labelGens.Lock()
		defer labelGens.Unlock()
		return labelGens.m[key] == gen
	}
}

// finishLabels runs label until it succeeds or runs out of tries, and returns nil if
// it succeeded within labelWait, errLabelsSlow if it was still going, or its last
// error. The retries carry on after a slow answer, but stop as soon as current()
// turns false; a final failure is logged.
func finishLabels(what string, current func() bool, label func() error) error {
	tries, gap, wait := labelTries, labelGap, labelWait // read once, outside the goroutine
	done := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < tries; i++ {
			if i > 0 {
				time.Sleep(gap)
			}
			if !current() {
				err = errSuperseded
				break
			}
			if err = label(); err == nil || err == errSuperseded {
				break
			}
		}
		if err != nil {
			log.Printf("rename: %s herdr labels: %v", what, err)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		return errLabelsSlow
	}
}

// errBusy means the agent is mid-turn or waiting on a question: typing the rename
// command now would queue behind the turn or answer the question.
var errBusy = fmt.Errorf("the session is busy - rename it when it is idle")

// RenameSession renames a session from the phone. It types the agent's own rename
// command, so the name lives in the session itself (resume lists, the agent's title),
// and also labels the herdr panel at once rather than waiting for the sync loop. A pane
// with no known agent only gets the herdr labels. Returns the agent it renamed, if any;
// with an agent, an errLabelsPending error means the session took the name and herdr
// did not.
func RenameSession(machine, workspaceID, paneID, label string) (string, error) {
	label = strings.TrimSpace(label)
	if err := validateLabel(label); err != nil {
		return "", err
	}
	agent, status := "", ""
	if paneID != "" {
		var err error
		// A failed lookup must not read as "no agent": that would skip the session
		// rename and fall back to herdr labels alone.
		if agent, status, err = paneAgentStatus(machine, paneID); err != nil {
			return "", fmt.Errorf("could not read the session's agent: %w", err)
		}
	}
	cmd := renameCommands[agent]
	if cmd == "" {
		return "", Rename(machine, workspaceID, paneID, label)
	}
	if status != "idle" && status != "done" {
		return "", errBusy
	}
	key := machine + "/" + paneID
	mu := paneLock(key)
	if !lockWithin(mu, agentLookupTimeout) {
		return "", errRenameRunning
	}
	current := newLabelGen(key)
	err := SendPrompt(machine, paneID, cmd+" "+label)
	mu.Unlock()
	if err != nil {
		return "", err
	}
	// The agent is renamed now. Name sync cannot repair the herdr labels for Hermes (its
	// title is invisible to herdr) or with the sync off, so every label must take - the
	// workspace, agent and pane (renamePanel fails on any of them, where Rename only
	// checks the workspace) - and anything short of that is reported as a partial rename.
	a := renameAction{Machine: machine, WorkspaceID: workspaceID, PaneID: paneID, Label: label, Workspace: true}
	attempt := func() error {
		mu.Lock()
		defer mu.Unlock()
		if !current() {
			return errSuperseded
		}
		return renamePanel(a)
	}
	if err := finishLabels(machine+" "+paneID, current, attempt); err != nil {
		return agent, errLabelsPending{err}
	}
	return agent, nil
}

// agentLookupTimeout bounds the status check before the rename command is typed, so
// a stuck herdr fails the request early instead of eating the phone's 15s timeout.
const agentLookupTimeout = 4 * time.Second

// paneAgentStatus reads a pane's agent and its live status. agent is "" when the pane
// has no agent; err means the lookup itself failed.
func paneAgentStatus(machine, pane string) (agent, status string, err error) {
	out, err := herdrOnT(agentLookupTimeout, machine, "agent", "get", pane)
	if err != nil {
		return "", "", err
	}
	var r struct {
		Result struct {
			Agent struct {
				Agent       string `json:"agent"`
				AgentStatus string `json:"agent_status"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", "", fmt.Errorf("parse agent get: %w", err)
	}
	return r.Result.Agent.Agent, r.Result.Agent.AgentStatus, nil
}
