package main

import "testing"

func TestSyncName(t *testing.T) {
	cases := []struct {
		title, cwd, want string
	}{
		{"reddit-edisco", "/home/jc/compliance", "reddit-edisco"},
		{"Claude Code", "/x", ""},
		{"01a2186b-c800-7ad0-b136-ed6559b16d0b", "/x", ""}, // unnamed Codex thread
		{"tt", "/tmp/tt", ""},                              // Muse before a rename
		{"", "/x", ""},
		{"Fix the log-in bug!", "/x", "Fix the log-in bug"},
	}
	for _, c := range cases {
		if got := syncName(Session{Title: c.title, Cwd: c.cwd}); got != c.want {
			t.Errorf("syncName(%q, %q) = %q, want %q", c.title, c.cwd, got, c.want)
		}
	}
}

func sess(machine, ws, pane, label, title string) Session {
	return Session{Machine: machine, WorkspaceID: ws, PaneID: pane, Label: label, Name: label, Title: title, Cwd: "/home/jc/repo"}
}

var local = map[string]bool{"": true}

func TestNameSyncLeavesStartupPanesAlone(t *testing.T) {
	ns := newNameSyncer()
	list := []Session{sess("", "w1", "w1:p1", "old-label", "agent-name")}
	for i := 0; i < 3; i++ {
		if got := ns.tick(list, local); len(got) != 0 {
			t.Fatalf("tick %d renamed a pane that was open at startup: %+v", i, got)
		}
	}
}

func TestNameSyncFollowsARename(t *testing.T) {
	ns := newNameSyncer()
	ns.tick([]Session{sess("", "w1", "w1:p1", "old", "old")}, local)
	renamed := []Session{sess("", "w1", "w1:p1", "old", "new-name")}
	if got := ns.tick(renamed, local); len(got) != 0 {
		t.Fatalf("renamed on the first sighting: %+v", got)
	}
	got := ns.tick(renamed, local)
	if len(got) != 1 || got[0].Label != "new-name" || got[0].PaneID != "w1:p1" || !got[0].Workspace {
		t.Fatalf("want one workspace rename to new-name, got %+v", got)
	}
	// Done once: later listings with the same name do nothing, even if herdr was
	// renamed by hand in the meantime.
	if got := ns.tick([]Session{sess("", "w1", "w1:p1", "typed-in-herdr", "new-name")}, local); len(got) != 0 {
		t.Fatalf("renamed again: %+v", got)
	}
}

func TestNameSyncNewPaneGetsItsName(t *testing.T) {
	ns := newNameSyncer()
	ns.tick(nil, local)
	p := []Session{sess("", "w2", "w2:p1", "spawn-label", "Codex thread name")}
	ns.tick(p, local)
	if got := ns.tick(p, local); len(got) != 1 || got[0].Label != "Codex thread name" {
		t.Fatalf("new pane not renamed: %+v", got)
	}
}

func TestNameSyncSkipsMatchingLabelAndPlaceholders(t *testing.T) {
	ns := newNameSyncer()
	ns.tick(nil, local)
	p := []Session{
		sess("", "w3", "w3:p1", "same", "same"),
		sess("", "w4", "w4:p1", "x", "Claude Code"),
	}
	ns.tick(p, local)
	if got := ns.tick(p, local); len(got) != 0 {
		t.Fatalf("want no renames, got %+v", got)
	}
}

func TestNameSyncSharedWorkspaceKeepsItsName(t *testing.T) {
	ns := newNameSyncer()
	ns.tick(nil, local)
	p := []Session{
		sess("", "w5", "w5:p1", "team", "first agent"),
		sess("", "w5", "w5:p2", "team", "team"), // its agent name is already "team"
	}
	ns.tick(p, local)
	got := ns.tick(p, local)
	if len(got) != 1 || got[0].Workspace {
		t.Fatalf("want a pane-only rename in a shared workspace, got %+v", got)
	}
}

func TestNameSyncMachineOfflineAtStartup(t *testing.T) {
	ns := newNameSyncer()
	ns.tick(nil, local) // m1 did not answer at startup
	box := []Session{sess("m1", "w1", "w1:p1", "hand-set", "agent-name")}
	up := map[string]bool{"": true, "m1": true}
	for i := 0; i < 3; i++ {
		if got := ns.tick(box, up); len(got) != 0 {
			t.Fatalf("a machine answering late had its panes renamed: %+v", got)
		}
	}
}

func TestNameSyncRetriesAFailedRename(t *testing.T) {
	ns := newNameSyncer()
	ns.tick(nil, local)
	p := []Session{sess("", "w1", "w1:p1", "old", "new")}
	ns.tick(p, local)
	got := ns.tick(p, local)
	if len(got) != 1 {
		t.Fatalf("want a rename, got %+v", got)
	}
	ns.failed(got[0])
	ns.tick(p, local)
	if got := ns.tick(p, local); len(got) != 1 {
		t.Fatalf("a failed rename was not tried again: %+v", got)
	}
}

func TestNameSyncOfflineMachineIsNotNew(t *testing.T) {
	ns := newNameSyncer()
	box := []Session{sess("m1", "w1", "w1:p1", "label", "agent-name")}
	ns.tick(box, map[string]bool{"": true, "m1": true})
	ns.tick(nil, map[string]bool{"": true}) // m1 did not answer
	ns.tick(box, map[string]bool{"": true, "m1": true})
	if got := ns.tick(box, map[string]bool{"": true, "m1": true}); len(got) != 0 {
		t.Fatalf("a machine coming back renamed its panes: %+v", got)
	}
}
