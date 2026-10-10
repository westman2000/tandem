package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

type runnerFunc func(context.Context, AgentInput, func(string, string)) (AgentResult, error)

func (f runnerFunc) Run(c context.Context, i AgentInput, e func(string, string)) (AgentResult, error) {
	return f(c, i, e)
}
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testProject(t *testing.T, s *Store) Project {
	t.Helper()
	p := Project{ID: ID(), Title: "Game", CreatedAt: now(), UpdatedAt: now()}
	if err := s.PutProject(p); err != nil {
		t.Fatal(err)
	}
	return p
}
func awaitRun(t *testing.T, s *Store, id, status string) Run {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.Run(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == status {
			return r
		}
		if r.Status == "failed" && status != "failed" {
			t.Fatalf("unexpected failure: %s", r.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, _ := s.Run(id)
	t.Fatalf("wanted %s got %s", status, r.Status)
	return r
}

func TestQualityLoopAndSharedContext(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	m, err := s.SaveMaterial(p.ID, "requirements.md", "requirements", "text/plain", []byte("OFFLINE_REQUIRED"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := []AgentInput{}
	checks := 0
	runner := runnerFunc(func(ctx context.Context, in AgentInput, emit func(string, string)) (AgentResult, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, in)
		if !strings.Contains(in.Prompt, "OFFLINE_REQUIRED") || !strings.Contains(in.Prompt, m.SHA256) {
			t.Error("missing shared material snapshot")
		}
		if in.Stage == "ideas" && strings.Contains(in.Prompt, "PROPOSAL_") {
			t.Error("independent proposal leaked")
		}
		if in.Stage == "critique" && (!strings.Contains(in.Prompt, "PROPOSAL_codex") || !strings.Contains(in.Prompt, "PROPOSAL_claude")) {
			t.Error("critique missing partner proposal")
		}
		if in.Stage != "ideas" && in.Session != "session-"+in.Agent {
			t.Errorf("wrong session %q", in.Session)
		}
		emit("session", "session-"+in.Agent)
		answer := "PROPOSAL_" + in.Agent
		if in.Stage == "verify" {
			checks++
			answer = "Fix acceptance criterion\n[TANDEM_REVISE]"
			if checks == 2 {
				answer = "All criteria passed\n[TANDEM_READY]"
			}
		}
		return AgentResult{Text: answer, Session: "session-" + in.Agent}, nil
	})
	e := NewEngine(s, runner)
	defer e.Close()
	r, err := e.Create(p.ID, "live", "Create a game", 3, "Work offline")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	if r.Quality != "ready" || len(r.Tasks) != 10 {
		t.Fatalf("wrong loop result: quality=%s tasks=%d", r.Quality, len(r.Tasks))
	}
	messages, _ := s.Messages(p.ID)
	if len(messages) != 11 {
		t.Fatalf("expected brief + 10 answers got %d", len(messages))
	}
	if err := s.CommitTask(r.ID, r.Tasks[0].ID, "duplicate"); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.Messages(p.ID)
	if len(messages) != 11 {
		t.Fatal("duplicate checkpoint created duplicate message")
	}
	if r.Document == "" {
		t.Fatal("no document")
	}
}

func TestQualityLimitDoesNotClaimReady(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		return AgentResult{Text: "Unresolved requirements\n[TANDEM_REVISE]"}, nil
	}))
	defer e.Close()
	r, err := e.Create(p.ID, "live", "Plan", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	if r.Quality != "limit" || len(r.Tasks) != 10 {
		t.Fatalf("unexpected quality: %+v", r)
	}
	if reviewReady("Mention [TANDEM_READY] in prose") || reviewReady("[TANDEM_READY]\nBut not really") {
		t.Fatal("accepted invalid review marker")
	}
}

func TestPauseResumeRetainsCompletedAnswers(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	started := make(chan struct{}, 2)
	e := NewEngine(s, runnerFunc(func(ctx context.Context, in AgentInput, emit func(string, string)) (AgentResult, error) {
		emit("session", "old-"+in.Agent)
		if in.Stage == "critique" {
			started <- struct{}{}
			<-ctx.Done()
			return AgentResult{}, ctx.Err()
		}
		return AgentResult{Text: "initial-" + in.Agent}, nil
	}))
	r, err := e.Create(p.ID, "live", "Plan", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner never started")
	}
	if _, err = e.Pause(r.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "paused")
	e.Close()
	if r.Tasks[0].Status != "done" || r.Tasks[1].Status != "done" {
		t.Fatal("lost completed proposals")
	}
	if len(r.Sessions) != 0 {
		t.Fatal("interrupted CLI sessions should be discarded")
	}
	var mu sync.Mutex
	called := map[string]int{}
	e2 := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		mu.Lock()
		called[in.Stage]++
		mu.Unlock()
		if in.Stage == "ideas" {
			t.Error("replayed a completed answer")
		}
		return AgentResult{Text: "result\n[TANDEM_READY]"}, nil
	}))
	defer e2.Close()
	if _, err = e2.Resume(r.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	messages, _ := s.Messages(p.ID)
	if len(messages) != 9 {
		t.Fatalf("expected 9 durable messages, got %d", len(messages))
	}
}

func TestRecoveryAndExclusiveDataDirectory(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenStore(dir); err == nil {
		other.Close()
		t.Fatal("allowed simultaneous owners")
	}
	r := Run{ID: ID(), ProjectID: "p", Status: "running", Sessions: map[string]string{"codex": "partial", "claude": "complete"}, Tasks: []Task{{ID: "1", Agent: "codex", Status: "running"}, {ID: "2", Agent: "claude", Status: "done", Output: "saved"}}}
	if err = s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Recover(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(r.ID)
	if r.Status != "paused" || r.Tasks[0].Status != "pending" || r.Tasks[1].Output != "saved" || r.Sessions["codex"] != "" || r.Sessions["claude"] != "complete" {
		t.Fatalf("bad recovery: %+v", r)
	}
}

func TestMaterialExtractionAndImmutableSnapshot(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("word/document.xml")
	_, _ = f.Write([]byte(`<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Hello</w:t></w:r></w:p><w:p><w:r><w:t>World</w:t></w:r></w:p></w:body></w:document>`))
	z.Close()
	value, warning, err := extractText("brief.docx", b.Bytes())
	if err != nil || value != "Hello\nWorld" || warning == "" {
		t.Fatalf("extraction failed: %q %q %v", value, warning, err)
	}
	if _, _, err = extractText("photo.png", []byte("fake")); err == nil {
		t.Fatal("unsupported binary accepted")
	}
	if _, _, err = extractText("data.txt", []byte{0xff, 0}); err == nil {
		t.Fatal("binary accepted as text")
	}
	s := testStore(t)
	p := testProject(t, s)
	m, err := s.SaveMaterial(p.ID, "../../brief.md", "draft", "text/plain", []byte("Original content"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "brief.md" {
		t.Fatal("filename traversal not sanitized")
	}
	r := Run{ID: ID(), ProjectID: p.ID, Materials: []Material{m}}
	if err = s.Snapshot(r); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteMaterial(m.ID); err != nil {
		t.Fatal(err)
	}
	copy, err := os.ReadFile(filepath.Join(s.Dir, "runs", r.ID, m.ID+".original"))
	if err != nil || string(copy) != "Original content" {
		t.Fatal("snapshot lost after removal")
	}
	if err = os.WriteFile(filepath.Join(s.Dir, "materials", m.ID), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	r.ID = ID()
	if err = s.Snapshot(r); err == nil {
		t.Fatal("accepted checksum mismatch")
	}
}

func TestStreamParsersDoNotExposeHiddenReasoning(t *testing.T) {
	cases := []struct {
		agent  string
		lines  []string
		answer string
	}{
		{"codex", []string{`{"type":"thread.started","thread_id":"thread-1"}`, `{"type":"item.completed","item":{"type":"reasoning","text":"SECRET_THOUGHT"}}`, `{"type":"item.completed","item":{"type":"agent_message","text":"Visible answer"}}`}, "Visible answer"},
		{"claude", []string{`{"type":"system","subtype":"init","session_id":"thread-1"}`, `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"SECRET_THOUGHT"},{"type":"text","text":"Visible answer"}]}}`, `{"type":"result","is_error":false,"result":"Visible answer","session_id":"thread-1"}`}, "Visible answer"},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			state := streamState{}
			var events []string
			for _, line := range tc.lines {
				state.parse(tc.agent, []byte(line), func(k, v string) { events = append(events, k+v) })
			}
			if state.session != "thread-1" || strings.TrimSpace(state.answer.String()) != tc.answer {
				t.Fatalf("bad result %+v", state)
			}
			if strings.Contains(strings.Join(events, ""), "SECRET_THOUGHT") {
				t.Fatal("hidden reasoning leaked")
			}
		})
	}
}

func TestSubscriptionEnvAndExactResume(t *testing.T) {
	env := subscriptionEnv([]string{"HOME=/home/test", "ANTHROPIC_API_KEY=secret", "OPENAI_API_KEY=secret", "ANTHROPIC_BASE_URL=https://proxy", "CLAUDE_CODE_USE_VERTEX=1", "PATH=/bin"})
	if len(env) != 2 || env[0] != "HOME=/home/test" {
		t.Fatalf("unexpected env %v", env)
	}
	for _, agent := range []string{"codex", "claude"} {
		args := strings.Join(commandArgs(AgentInput{Agent: agent, Session: "exact-run-session"}), " ")
		if !strings.Contains(args, "exact-run-session") || strings.Contains(args, "--last") || strings.Contains(args, "bypass") {
			t.Fatalf("unsafe resume: %s", args)
		}
	}
}

func TestServerLocalOriginAndCSRF(t *testing.T) {
	s := testStore(t)
	e := NewEngine(s, CLIRunner{})
	defer e.Close()
	server := NewServer(s, e, fstest.MapFS{"index.html": {Data: []byte("Tandem UI")}})
	cases := []struct {
		method, path, host, origin, token, body string
		want                                    int
	}{
		{"GET", "/api/bootstrap", "evil.test", "", "", "", 403},
		{"GET", "/api/bootstrap", "127.0.0.1:4317", "https://evil.test", "", "", 403},
		{"POST", "/api/projects", "127.0.0.1:4317", "", "", `{"title":"Injected"}`, 403},
		{"POST", "/api/projects", "127.0.0.1:4317", "http://127.0.0.1:4317", server.Token, `{"title":"Project"}`, 201},
		{"GET", "/api/bootstrap", "127.0.0.1:4317", "", "", "", 200},
		{"GET", "/", "localhost:4317", "", "", "", 200},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			req.Host = c.host
			req.Header.Set("Origin", c.origin)
			req.Header.Set("X-Tandem-Token", c.token)
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("expected %d got %d: %s", c.want, rec.Code, rec.Body)
			}
		})
	}
	projects, _ := s.Projects()
	if len(projects) != 1 {
		t.Fatal("cross-origin mutation succeeded")
	}
}

func TestProviderFailureNeverBecomesDemo(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	e := NewEngine(s, runnerFunc(func(_ context.Context, _ AgentInput, _ func(string, string)) (AgentResult, error) {
		return AgentResult{}, errors.New("provider unavailable")
	}))
	defer e.Close()
	r, err := e.Create(p.ID, "live", "Plan", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "failed")
	if r.Mode != "live" || r.Document != "" {
		t.Fatal("error silently produced demo content")
	}
	messages, _ := s.Messages(p.ID)
	if len(messages) != 1 {
		b, _ := json.Marshal(messages)
		t.Fatalf("unexpected answer: %s", b)
	}
}

func TestDocumentDownload(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	r := Run{ID: ID(), ProjectID: p.ID, Document: "# Durable document"}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, CLIRunner{})
	defer e.Close()
	server := NewServer(s, e, fstest.MapFS{})
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+r.ID+"/document", nil)
	req.Host = "127.0.0.1:4317"
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != r.Document || !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("download failed")
	}
}
