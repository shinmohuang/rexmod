package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpcanaydin/rexmod/internal/editor/rex"
)

// fakeServeRex is a Rex server as the service sees it: connected clients
// (ours included, as self), and what the service sent.
type fakeServeRex struct {
	mu       sync.Mutex
	self     string
	clients  []rex.ClientInfo
	labels   []string
	declared []rex.Action
	replies  map[string]any
	errors   map[string]string // call id -> "code: message"
	detached [][2]string
	onLabel  func(label string) // runs as a label is set, lock held
	// detachFails makes the first n DetachBlock calls fail.
	detachFails int
	sessions    []rex.SessionInfo
	blocks      map[string][]string // session -> its blocks
	destroyed   []string
}

func (f *fakeServeRex) Sessions() ([]rex.SessionInfo, error) { return f.sessions, nil }

func (f *fakeServeRex) SessionBlocks(s string) ([]string, error) { return f.blocks[s], nil }

func (f *fakeServeRex) DestroySession(s string) error {
	f.destroyed = append(f.destroyed, s)
	return nil
}

func newFakeServeRex(self string, others ...rex.ClientInfo) *fakeServeRex {
	f := &fakeServeRex{self: self, replies: map[string]any{}, errors: map[string]string{}}
	f.clients = append(others, rex.ClientInfo{ID: self, Name: "rexmod", Kind: "peer", UID: 1})
	return f
}

func (f *fakeServeRex) Clients() ([]rex.ClientInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rex.ClientInfo(nil), f.clients...), nil
}

func (f *fakeServeRex) SetLabel(l string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = append(f.labels, l)
	if f.onLabel != nil {
		f.onLabel(l)
	}
	for i := range f.clients {
		if f.clients[i].ID == f.self {
			f.clients[i].Label = l
		}
	}
	return nil
}

func (f *fakeServeRex) DeclareActions(a ...rex.Action) error {
	f.declared = append(f.declared, a...)
	return nil
}

func (f *fakeServeRex) Reply(id string, r any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[id] = r
	return nil
}

func (f *fakeServeRex) ReplyError(id, code, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors[id] = code + ": " + msg
	return nil
}

func (f *fakeServeRex) DetachBlock(s, b string) error {
	if f.detachFails > 0 {
		f.detachFails--
		return errors.New("not_found: block not found")
	}
	f.detached = append(f.detached, [2]string{s, b})
	return nil
}

func serveClient(id, label string) rex.ClientInfo {
	return rex.ClientInfo{ID: id, Name: "rexmod", Label: label, Kind: "peer", UID: 1}
}

func TestServeClaimFirst(t *testing.T) {
	// Rex.app and a CLI are connected; no service yet.
	f := newFakeServeRex("client:05", rex.ClientInfo{ID: "client:01", Name: "Rex Beta", Label: "Mac mini"}, rex.ClientInfo{ID: "client:02", Name: "rex"})
	s := &server{rex: f}
	first, err := s.claim()
	if err != nil || !first {
		t.Fatalf("first service: %v %v", first, err)
	}
	if s.id != "client:05" || len(f.declared) != 1 || f.declared[0].Name != serveAction {
		t.Errorf("id %q, declared %v", s.id, f.declared)
	}
	if last := f.labels[len(f.labels)-1]; last != "rexmod-serve#0" {
		t.Errorf("label %q", last)
	}
}

func TestServeClaimTaken(t *testing.T) {
	// A running service: the new one leaves without declaring anything.
	f := newFakeServeRex("client:05", serveClient("client:03", "rexmod-serve#7"))
	s := &server{rex: f}
	if first, _ := s.claim(); first || len(f.declared) != 0 {
		t.Fatalf("claimed over a running service: %v %v", first, f.declared)
	}
	// Two starting at once: the older ID wins.
	older := newFakeServeRex("client:05", serveClient("client:04", "rexmod-serve#starting-1"))
	if first, _ := (&server{rex: older}).claim(); first {
		t.Error("lost to nobody: an older rival was starting")
	}
	// Both start at once: a younger rival labels itself between our check
	// and our label. We're older, so we keep the name (it gives up).
	f2 := newFakeServeRex("client:05")
	f2.onLabel = func(l string) {
		if strings.Contains(l, "#starting-") {
			f2.clients = append(f2.clients, serveClient("client:09", "rexmod-serve#starting-2"))
		}
	}
	if first, _ := (&server{rex: f2}).claim(); !first {
		t.Error("the older of two starting services gave up")
	}
	// And the same race seen from the younger one: it gives up.
	f3 := newFakeServeRex("client:09")
	f3.onLabel = func(l string) {
		if strings.Contains(l, "#starting-") {
			f3.clients = append(f3.clients, serveClient("client:05", "rexmod-serve#starting-1"))
		}
	}
	if first, _ := (&server{rex: f3}).claim(); first || len(f3.declared) != 0 {
		t.Error("the younger of two starting services kept going")
	}
}

func TestServeClaimOverLost(t *testing.T) {
	// A service that ended lingers as a lost client: it doesn't count.
	lost := serveClient("client:03", "rexmod-serve#7")
	lost.Lost = true
	f := newFakeServeRex("client:05", lost)
	if first, _ := (&server{rex: f}).claim(); !first {
		t.Error("gave way to a service that's gone")
	}
}

func TestServeDetachSelf(t *testing.T) {
	f := newFakeServeRex("client:05")
	s := &server{rex: f}
	s.detachSelf("", "")
	if len(f.detached) != 0 {
		t.Error("detached without a pane")
	}
	// The pane isn't placed yet for the first tries: it retries.
	f.detachFails = 2
	s.detachSelf("session:s", "block:b")
	if len(f.detached) != 1 || f.detached[0] != [2]string{"session:s", "block:b"} {
		t.Errorf("detached %v", f.detached)
	}
}

func perform(id string, argv []string) rex.PerformRequest {
	args := []any{}
	for _, a := range argv {
		args = append(args, a)
	}
	return rex.PerformRequest{CallID: id, Action: serveAction, Args: map[string]any{"proto": json.Number("1"), "argv": args}}
}

func TestServeHandle(t *testing.T) {
	f := newFakeServeRex("client:05")
	var ran [][]string
	s := &server{rex: f, maxMsg: 1 << 20, watch: newRepoWatch(nil), run: func(argv []string) (string, string, int, error) {
		ran = append(ran, argv)
		switch argv[0] {
		case "info":
			return "", "rexmod: no session\n", 1, nil
		case "follow":
			return "", "", 0, errors.New("rexmod follow took longer than 20s")
		}
		return `{"ok":true}`, "", 0, nil
	}}
	s.handle(perform("c1", []string{"state", "--session", "s"}))
	s.handle(perform("c2", []string{"info", "--session", "s"}))
	s.handle(perform("c3", []string{"hook", "prompt"}))
	s.handle(perform("c4", []string{"sh", "-c", "rm -rf ~"}))
	s.handle(perform("c5", []string{"comments", "add", "--file", "x"}))
	s.handle(perform("c6", []string{"comments", "send", "--session", "s"}))
	s.handle(perform("c7", []string{"follow", "--block", "b"}))
	s.handle(rex.PerformRequest{CallID: "c8", Action: "other.thing"})
	s.handle(perform("c10", []string{"open", "--session", "s", "/r/a.go"}))
	s.handle(perform("c11", []string{"pick", "--session", "s"}))
	s.handle(perform("c12", []string{"pick", "--inner", "--repo", "/r"}))
	s.handle(perform("c13", []string{"editor", "a.go"}))
	s.handle(rex.PerformRequest{CallID: "c9", Action: serveAction, Args: map[string]any{"proto": json.Number("2"), "argv": []any{"state"}}})

	r1, ok := f.replies["c1"].(serveReply)
	if !ok || r1.Code != 0 || r1.Stdout != `{"ok":true}` || r1.Proto != serveProto || r1.Version == "" {
		t.Errorf("state reply: %#v", f.replies["c1"])
	}
	if r2, _ := f.replies["c2"].(serveReply); r2.Code != 1 || r2.Stderr != "rexmod: no session\n" {
		t.Errorf("failing command: %#v", f.replies["c2"])
	}
	for _, id := range []string{"c10", "c11"} {
		if _, ok := f.replies[id]; !ok {
			t.Errorf("%s: not run: %q", id, f.errors[id])
		}
	}
	for _, id := range []string{"c3", "c4", "c5", "c12", "c13"} {
		if !strings.HasPrefix(f.errors[id], "refused: ") {
			t.Errorf("%s: want refused, got %q", id, f.errors[id])
		}
	}
	if _, ok := f.replies["c6"]; !ok {
		t.Error("comments send not run")
	}
	if !strings.HasPrefix(f.errors["c7"], "internal: ") {
		t.Errorf("timeout: %q", f.errors["c7"])
	}
	if !strings.HasPrefix(f.errors["c8"], "not_supported: ") || !strings.HasPrefix(f.errors["c9"], "not_supported: ") {
		t.Errorf("unknown action / proto: %q %q", f.errors["c8"], f.errors["c9"])
	}
	for _, argv := range ran {
		if argv[0] == "hook" || argv[0] == "sh" || (argv[0] == "comments" && argv[1] != "send") {
			t.Errorf("ran a refused command: %v", argv)
		}
	}
}

func TestServeTooLarge(t *testing.T) {
	f := newFakeServeRex("client:05")
	big := strings.Repeat("x", 10000)
	s := &server{rex: f, maxMsg: 8000, watch: newRepoWatch(nil), run: func([]string) (string, string, int, error) { return big, "", 0, nil }}
	s.handle(perform("c1", []string{"state"}))
	if _, ok := f.replies["c1"]; ok || !strings.HasPrefix(f.errors["c1"], "internal: too_large:") {
		t.Errorf("want too_large, got reply=%v err=%q", f.replies["c1"], f.errors["c1"])
	}
}

func TestRunSelf(t *testing.T) {
	t.Setenv("REX_SESSION", "session:serve")
	t.Setenv("REX_BLOCK", "block:serve")
	run := runSelf("/bin/sh")
	out, errOut, code, err := run([]string{"-c", `echo "s=$REX_SESSION b=$REX_BLOCK"; echo oops >&2; exit 3`})
	if err != nil || code != 3 || out != "s= b=\n" || errOut != "oops\n" {
		t.Errorf("got %q %q %d %v", out, errOut, code, err)
	}
	old := serveRunTimeout
	serveRunTimeout = 100 * time.Millisecond
	defer func() { serveRunTimeout = old }()
	if _, _, _, err := run([]string{"-c", "sleep 5"}); err == nil || !strings.Contains(err.Error(), "took longer") {
		t.Errorf("timeout: %v", err)
	}
}

func TestServeUpgraded(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "rexmod")
	os.WriteFile(exe, []byte("v1"), 0o755)
	st, _ := os.Stat(exe)
	s := &server{exe: exe, exeMtime: st.ModTime()}
	if s.upgraded() {
		t.Error("upgraded without a change")
	}
	later := st.ModTime().Add(2 * time.Second)
	os.Chtimes(exe, later, later)
	if !s.upgraded() {
		t.Error("a replaced binary went unnoticed")
	}
}

func TestServeBump(t *testing.T) {
	f := newFakeServeRex("client:05")
	s := &server{rex: f}
	s.bump()
	s.bump()
	if f.labels[len(f.labels)-1] != "rexmod-serve#2" {
		t.Errorf("labels %v", f.labels)
	}
}

// gitRepo makes a repository with one commit.
func gitRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", d}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	os.WriteFile(filepath.Join(d, "a.txt"), []byte("one\n"), 0o644)
	exec.Command("git", "-C", d, "add", ".").Run()
	exec.Command("git", "-C", d, "commit", "-qm", "init").Run()
	return d
}

func TestRepoWatch(t *testing.T) {
	repo := gitRepo(t)
	n := 0
	w := newRepoWatch(func() { n++ })
	now := time.Now()
	w.track(repo, now)
	step := func(what string, change func(), want int) {
		t.Helper()
		change()
		now = now.Add(repoSlowEvery) // past anything due
		w.scan(now)
		if n != want {
			t.Errorf("%s: %d changes, want %d", what, n, want)
		}
	}
	step("nothing", func() {}, 0)
	step("edit a file", func() { os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644) }, 1)
	step("edit it again", func() {
		f := filepath.Join(repo, "a.txt")
		os.WriteFile(f, []byte("three!\n"), 0o644)
		later := time.Now().Add(time.Second)
		os.Chtimes(f, later, later)
	}, 2)
	step("new file", func() { os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644) }, 3)
	step("commit", func() {
		exec.Command("git", "-C", repo, "add", ".").Run()
		exec.Command("git", "-C", repo, "commit", "-qm", "two").Run()
	}, 4)
	step("a comment", func() {
		os.MkdirAll(filepath.Join(repo, ".git", "rexmod"), 0o755)
		os.WriteFile(filepath.Join(repo, ".git", "rexmod", "comments.json"), []byte("[]"), 0o644)
	}, 5)
	step("nothing again", func() {}, 5)

	// Asked about it again: kept. Then idle: dropped, and no longer scanned.
	w.track(repo, now)
	w.scan(now.Add(repoIdle / 2))
	if !w.watching(repo) {
		t.Error("dropped while still asked about")
	}
	w.scan(now.Add(repoIdle + time.Minute))
	if w.watching(repo) {
		t.Error("idle repository still watched")
	}
}

func TestRepoWatchSlow(t *testing.T) {
	repo := gitRepo(t)
	w := newRepoWatch(nil)
	calls := 0
	w.fingerprint = func(string, string) string { calls++; time.Sleep(repoSlowCost + 10*time.Millisecond); return "x" }
	now := time.Now()
	w.track(repo, now)
	calls = 0
	w.scan(now.Add(repoScanEvery))
	w.scan(now.Add(2 * repoScanEvery)) // a slow repo isn't due again yet
	if calls != 1 {
		t.Errorf("slow repo scanned %d times in %s", calls, repoScanEvery)
	}
}

func TestStatusPaths(t *testing.T) {
	st := "# branch.oid abc\x00# branch.head main\x00" +
		"1 .M N... 100644 100644 100644 aaa bbb dir/a file.txt\x00" +
		"2 R. N... 100644 100644 100644 aaa bbb R100 new.txt\x00old.txt\x00" +
		"u UU N... 100644 100644 100644 100644 a b c conflict.txt\x00" +
		"? untracked dir/x.txt\x00"
	got := statusPaths(st)
	want := []string{"dir/a file.txt", "new.txt", "conflict.txt", "untracked dir/x.txt"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestServeTrack(t *testing.T) {
	repo := gitRepo(t)
	s := &server{watch: newRepoWatch(nil)}
	s.track([]string{"state", "--session", "x"}, `{"target":{"repo":"`+repo+`"}}`)
	if !s.watch.watching(repo) {
		t.Error("state's repository not watched")
	}
	other := gitRepo(t)
	s.track([]string{"files", "--repo", other}, "a.txt\n")
	if !s.watch.watching(other) {
		t.Error("files --repo not watched")
	}
}

func TestServeSweep(t *testing.T) {
	f := newFakeServeRex("client:05")
	f.sessions = []rex.SessionInfo{{ID: "session:mine", Label: "rexmod"}, {ID: "session:old", Label: "rexmod"}, {ID: "session:busy", Label: "rexmod"}, {ID: "session:user", Label: "work"}}
	f.blocks = map[string][]string{"session:mine": {"block:me"}, "session:old": nil, "session:busy": {"block:x"}, "session:user": nil}
	s := &server{rex: f, session: "session:mine", block: "block:me"}
	s.sweep()
	// Only the empty rexmod session goes: not ours, not one with a block,
	// not a user's session of another name.
	if strings.Join(f.destroyed, ",") != "session:old" {
		t.Errorf("destroyed %v", f.destroyed)
	}
}

func TestServeLeave(t *testing.T) {
	f := newFakeServeRex("client:05")
	f.blocks = map[string][]string{"session:mine": {"block:me"}}
	s := &server{rex: f, session: "session:mine", block: "block:me"}
	s.leave()
	if strings.Join(f.destroyed, ",") != "session:mine" {
		t.Errorf("own session not destroyed: %v", f.destroyed)
	}
	// Something else runs in it (someone placed a pane there): it stays.
	f2 := newFakeServeRex("client:05")
	f2.blocks = map[string][]string{"session:mine": {"block:me", "block:other"}}
	(&server{rex: f2, session: "session:mine", block: "block:me"}).leave()
	if len(f2.destroyed) != 0 {
		t.Errorf("destroyed a session with another pane: %v", f2.destroyed)
	}
	// Not started in a pane: nothing to do.
	f3 := newFakeServeRex("client:05")
	(&server{rex: f3}).leave()
	if len(f3.destroyed) != 0 {
		t.Error("destroyed without a session")
	}
}

// An open on the interactive client is answered while the main client is
// still busy with a slow state.
func TestServeUIAnswersBesideMain(t *testing.T) {
	main, ui := newFakeServeRex("client:05"), newFakeServeRex("client:06")
	release := make(chan struct{})
	s := &server{rex: main, maxMsg: 1 << 20, watch: newRepoWatch(nil), run: func(argv []string) (string, string, int, error) {
		if argv[0] == "state" {
			<-release
		}
		return "{}", "", 0, nil
	}}
	events := make(chan *rex.Event, 1)
	if err := s.serveUI(ui, events); err != nil {
		t.Fatal(err)
	}
	if len(ui.declared) != 1 || ui.declared[0].Name != serveAction || ui.labels[len(ui.labels)-1] != serveUILabel {
		t.Fatalf("interactive client: declared %v, labels %v", ui.declared, ui.labels)
	}
	go s.handle(perform("slow", []string{"state", "--session", "s"}))
	events <- performEvent(perform("open", []string{"open", "--session", "s", "/r/a.go"}))
	deadline := time.Now().Add(time.Second)
	for {
		ui.mu.Lock()
		_, ok := ui.replies["open"]
		ui.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("open waited for the slow state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	main.mu.Lock()
	_, stateDone := main.replies["slow"]
	main.mu.Unlock()
	if stateDone {
		t.Error("state finished before it was released")
	}
	close(release)
}

// The interactive client is no second service to the main one's claim.
func TestServeClaimIgnoresUIClient(t *testing.T) {
	f := newFakeServeRex("client:05", serveClient("client:03", serveUILabel))
	s := &server{rex: f}
	if first, err := s.claim(); err != nil || !first {
		t.Fatalf("claim with an interactive client around: %v %v", first, err)
	}
}

func performEvent(r rex.PerformRequest) *rex.Event {
	return &rex.Event{Name: "client_perform_requested", Payload: map[string]any{"call_id": r.CallID, "action": r.Action, "args": r.Args}}
}
