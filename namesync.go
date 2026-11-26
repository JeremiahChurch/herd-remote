package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
}

// nameSyncer remembers the last name seen for every pane. Not safe for concurrent
// ticks; the loop runs one at a time.
type nameSyncer struct {
	primed bool
	panes  map[string]*nameState // machine + "/" + pane id
}

func newNameSyncer() *nameSyncer { return &nameSyncer{panes: map[string]*nameState{}} }

// tick takes one listing and returns the panels to relabel. A new name has to show
// on two listings in a row, so a title caught mid-redraw never renames anything.
// answered holds the machines that listed their sessions ("" = this box); a pane on a
// machine that did not answer is kept, so its return is not mistaken for a new pane.
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
			if !ns.primed {
				st.applied = name // open at startup: take it as it is
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
		if name == s.Label {
			continue
		}
		out = append(out, renameAction{
			Machine: s.Machine, WorkspaceID: s.WorkspaceID, PaneID: s.PaneID, Label: name,
			Workspace: agentsIn[s.Machine+"/"+s.WorkspaceID] == 1,
		})
	}
	for key := range ns.panes {
		machine, _, _ := strings.Cut(key, "/")
		if !seen[key] && answered[machine] {
			delete(ns.panes, key)
		}
	}
	ns.primed = true
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
// workspace holds only this agent (a shared workspace keeps its own name).
func renamePanel(a renameAction) error {
	if err := validateLabel(a.Label); err != nil {
		return err
	}
	if a.Workspace {
		if _, err := herdrOn(a.Machine, "workspace", "rename", a.WorkspaceID, a.Label); err != nil {
			return err
		}
	}
	_, _ = herdrOn(a.Machine, "agent", "rename", a.PaneID, a.Label)
	_, _ = herdrOn(a.Machine, "pane", "rename", a.PaneID, a.Label)
	return nil
}

// renameCommands is each agent's own command for naming its session. Every one was
// checked live: Claude, Codex and Muse take "/rename <name>" in one line and show the
// new name in the terminal title at once; Hermes takes "/title <name>" (saved with the
// session's first message if it has none yet).
var renameCommands = map[string]string{
	"claude": "/rename", "codex": "/rename", "muse": "/rename", "hermes": "/title",
}

// errBusy means the agent is mid-turn or waiting on a question: typing the rename
// command now would queue behind the turn or answer the question.
var errBusy = fmt.Errorf("the session is busy - rename it when it is idle")

// RenameSession renames a session from the phone. It types the agent's own rename
// command, so the name lives in the session itself (resume lists, the agent's title),
// and also labels the herdr panel at once rather than waiting for the sync loop. A pane
// with no known agent only gets the herdr labels. Returns the agent it renamed, if any.
func RenameSession(machine, workspaceID, paneID, label string) (string, error) {
	label = strings.TrimSpace(label)
	if err := validateLabel(label); err != nil {
		return "", err
	}
	agent, status := "", ""
	if paneID != "" {
		agent, status = paneAgentStatus(machine, paneID)
	}
	cmd := renameCommands[agent]
	if cmd != "" {
		if status != "idle" && status != "done" {
			return "", errBusy
		}
		if err := SendPrompt(machine, paneID, cmd+" "+label); err != nil {
			return "", err
		}
	}
	if err := Rename(machine, workspaceID, paneID, label); err != nil {
		return "", err
	}
	if cmd == "" {
		return "", nil
	}
	return agent, nil
}

// paneAgentStatus reads a pane's agent and its live status ("" when it has no agent).
func paneAgentStatus(machine, pane string) (agent, status string) {
	out, err := herdrOn(machine, "agent", "get", pane)
	if err != nil {
		return "", ""
	}
	var r struct {
		Result struct {
			Agent struct {
				Agent       string `json:"agent"`
				AgentStatus string `json:"agent_status"`
			} `json:"agent"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &r) != nil {
		return "", ""
	}
	return r.Result.Agent.Agent, r.Result.Agent.AgentStatus
}
