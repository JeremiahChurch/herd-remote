package main

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidSpawnAgent(t *testing.T) {
	for in, want := range map[string]string{"": "claude", "claude": "claude", "codex": "codex", "hermes": "hermes", "muse": "muse"} {
		got, err := validSpawnAgent(in)
		if err != nil || got != want {
			t.Errorf("validSpawnAgent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"bash", "Muse", "muse ", "muse;id", "-a"} {
		if _, err := validSpawnAgent(bad); err == nil {
			t.Errorf("validSpawnAgent(%q) accepted", bad)
		}
	}
}

// Spawn must refuse bad input before it runs herd-spawn, so these never touch herdr.
func TestSpawnRejectsBadMuseInput(t *testing.T) {
	cases := []struct{ agent, model, effort, resume, want string }{
		{"mus", "", "", "", "unknown agent"},
		{"muse", "muse spark", "", "", "bad model"},
		{"muse", "", "minimal", "", "unknown effort"}, // Muse has it, herd-spawn does not
		{"muse", "", "none", "", "unknown effort"},
		{"muse", "", "", "../../etc", "bad resume id"},
	}
	for _, c := range cases {
		_, err := Spawn(Machine{}, "/nonexistent", "", c.model, c.effort, c.agent, "", c.resume, true)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Spawn(agent=%q model=%q effort=%q resume=%q) err = %v; want %q", c.agent, c.model, c.effort, c.resume, err, c.want)
		}
	}
}

func TestRemoteFoldersProbesMuse(t *testing.T) {
	if !strings.Contains(remoteFoldersSh, "for a in claude codex hermes muse; do") {
		t.Fatal("remote agent probe does not look for muse")
	}
}

func TestParseHistoryLines(t *testing.T) {
	out := []byte(`{"id":"a1b2c3d4-0000","title":"x: y","cwd":"/w","modified":12}
not json
{"id":"","title":"no id"}
{"id":"e5f6a7b8-1111","title":"","cwd":"","modified":0}
`)
	got := parseHistoryLines("muse", out)
	want := []HistoryEntry{
		{Agent: "muse", ID: "a1b2c3d4-0000", Name: "x: y", Cwd: "/w", Modified: 12},
		{Agent: "muse", ID: "e5f6a7b8-1111"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// makeMuseIndex builds a fixture session index with python3's sqlite3, using the
// columns museIndexPy reads from the real (1.4.14) schema.
func makeMuseIndex(t *testing.T, ddl string, inserts []string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	db := filepath.Join(t.TempDir(), "session-index.db")
	script := "import sqlite3, sys\ncon = sqlite3.connect(sys.argv[1])\ncon.execute(" + pyStr(ddl) + ")\n"
	for _, ins := range inserts {
		script += "con.execute(" + pyStr(ins) + ")\n"
	}
	script += "con.commit()\ncon.close()\n"
	if out, err := exec.Command("python3", "-c", script, db).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, out)
	}
	return db
}

func pyStr(s string) string { return `"""` + s + `"""` }

func TestMuseHistoryAt(t *testing.T) {
	ddl := `create table sessions (session_id text primary key, title text not null,
	  effective_long_title text, first_user_prompt text, session_name text, logged_session_name text,
	  workspace_root text, prompt_count integer not null default 0, msp_kind text,
	  msp_spawned_by_parent_session_id text, msp_last_activity_at_us integer,
	  updated_at_us integer, created_at_us integer)`
	long := strings.Repeat("word ", 40)
	db := makeMuseIndex(t, ddl, []string{
		// plain session: logged name + first-prompt title, activity time wins over updated
		`insert into sessions values ('01a1ada7-388f-7b81-b152-931edbf274b1', 'say ready', null, 'say ready', null, 'lemon-photon', '/home/u/herd-msg', 2, 'root', null, 3000000000, 1000000000, 500000000)`,
		// manual name beats the logged one; generated description beats the title
		`insert into sessions values ('01a1ad56-aebc-7d03-ac6f-375ac3d6b9af', 'first prompt', 'Fix the flaky test', 'first prompt', 'flaky-fix', 'grove-astraea', '/home/u/app', 4, 'root', null, null, 2000000000, 100)`,
		// long title is trimmed and whitespace collapsed
		`insert into sessions values ('01a1ad58-bfdd-71e3-aa3f-b7cec59bc204', '` + long + `', null, null, null, 'rust-halo', '', 1, null, null, null, null, 1500000000)`,
		// skipped: never prompted, a fork, a subagent
		`insert into sessions values ('01a1ad4b-5b1b-7a63-bd83-2e30c5263765', 'New session', null, null, null, 'wild-triton', '/home/u', 0, 'root', null, null, 9000000000, 1)`,
		`insert into sessions values ('01a1ad54-de44-7853-8baa-91280089def5', 'forked', null, null, null, 'porcelain-mira', '/home/u', 3, 'fork', null, null, 9000000000, 1)`,
		`insert into sessions values ('01a1ad53-dc25-7c83-b076-42713840583e', 'child', null, null, null, 'cream-transit', '/home/u', 3, 'root', '01a1ada7-388f-7b81-b152-931edbf274b1', null, 9000000000, 1)`,
		// skipped: an id the spawn path would refuse
		`insert into sessions values ('bad id!', 'odd', null, null, null, 'odd', '/home/u', 3, 'root', null, null, 9000000000, 1)`,
	})
	got := museHistoryAt(db)
	want := []HistoryEntry{
		{Agent: "muse", ID: "01a1ada7-388f-7b81-b152-931edbf274b1", Name: "lemon-photon: say ready", Cwd: "/home/u/herd-msg", Modified: 3000},
		{Agent: "muse", ID: "01a1ad56-aebc-7d03-ac6f-375ac3d6b9af", Name: "flaky-fix: Fix the flaky test", Cwd: "/home/u/app", Modified: 2000},
		{Agent: "muse", ID: "01a1ad58-bfdd-71e3-aa3f-b7cec59bc204", Name: "rust-halo: " + strings.TrimSpace(strings.Repeat("word ", 18)) + "...", Modified: 1500},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// An index without the optional msp_* and name columns still lists, falling back to the
// title and updated time.
func TestMuseHistoryAtOlderSchema(t *testing.T) {
	db := makeMuseIndex(t, `create table sessions (session_id text primary key, title text not null,
	  workspace_root text, updated_at_us integer)`, []string{
		`insert into sessions values ('01a1ad56-aebc-7d03-ac6f-375ac3d6b9af', 'review the diff', '/w', 7000000)`,
	})
	got := museHistoryAt(db)
	want := []HistoryEntry{{Agent: "muse", ID: "01a1ad56-aebc-7d03-ac6f-375ac3d6b9af", Name: "review the diff", Cwd: "/w", Modified: 7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestMuseHistoryAtMissing(t *testing.T) {
	if got := museHistoryAt(filepath.Join(t.TempDir(), "nope.db")); got != nil {
		t.Fatalf("missing index gave %+v", got)
	}
}

func TestParseMuseCatalogs(t *testing.T) {
	a := []byte(`{"rows":[
	  {"model_id":"muse-wonder-1.0","display_label":"muse-wonder-1.0","visibility":"visible","is_default":false,
	   "reasoning_effort_variants":[{"tier":"high"},{"tier":"low"},{"tier":"max"},{"tier":"minimal"}]},
	  {"model_id":"muse-wonder-1.0-contributor","visibility":"hidden","is_default":false},
	  {"model_id":"muse-spark-1.3","display_label":"","visibility":"visible","is_default":true,
	   "reasoning_effort_variants":[{"tier":"xhigh"},{"tier":"medium"},{"tier":"none"}]},
	  {"model_id":"bad model","visibility":"visible"}]}`)
	b := []byte(`{"rows":[{"model_id":"muse-spark-1.3","display_label":"dupe","visibility":"visible"},
	  {"model_id":"muse-spark-1.2","display_label":"Spark 1.2","visibility":"visible"}]}`)
	got := parseMuseCatalogs([][]byte{a, []byte("not json"), b})
	want := []MuseModel{
		{Slug: "muse-spark-1.3", Name: "muse-spark-1.3", Efforts: []string{"medium", "xhigh"}},
		{Slug: "muse-wonder-1.0", Name: "muse-wonder-1.0", Efforts: []string{"low", "high", "max"}},
		{Slug: "muse-spark-1.2", Name: "Spark 1.2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if got := parseMuseCatalogs(nil); len(got) != 0 {
		t.Fatalf("empty input gave %+v", got)
	}
}
