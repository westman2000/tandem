package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func TestTenChecksAndDefaultBudget(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	checks := 0
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		if in.Stage == "verify" {
			checks++
			return AgentResult{Text: fmt.Sprintf("Improved version %d; next issue %d\n[TANDEM_PROGRESS]\n[TANDEM_REVISE]", checks, checks)}, nil
		}
		return AgentResult{Text: "Document"}, nil
	}))
	defer e.Close()
	r, err := e.Create(p.ID, "live", "Plan", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	if checks != 10 || len(r.Tasks) != 26 || r.Quality != "limit" {
		t.Fatalf("budget truncated: %+v", r)
	}
	if _, err := e.Create(p.ID, "live", "Plan", 11, ""); err == nil {
		t.Fatal("accepted an unbounded budget")
	}
	checks = 0
	r, err = e.Create(p.ID, "live", "Plan", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	if r.MaxRevisions != 5 || checks != 5 {
		t.Fatal("default should be five checks")
	}
}

func TestStagnationAndReadyPriority(t *testing.T) {
	for _, mode := range []string{"stalled", "ready", "progress"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			p := testProject(t, s)
			checks := 0
			e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
				text := "Document"
				if in.Stage == "verify" {
					checks++
					text = fmt.Sprintf("Review %d: unresolved balance\n[TANDEM_NO_PROGRESS]\n[TANDEM_REVISE]", checks)
					if mode == "ready" && checks == 3 {
						text = "Ready\n[TANDEM_NO_PROGRESS]\n[TANDEM_READY]"
					}
					if mode == "progress" && checks%2 == 1 {
						text = "Improved\n[TANDEM_PROGRESS]\n[TANDEM_REVISE]"
					}
				}
				return AgentResult{Text: text}, nil
			}))
			defer e.Close()
			r, err := e.Create(p.ID, "live", "Plan", 5, "")
			if err != nil {
				t.Fatal(err)
			}
			r = awaitRun(t, s, r.ID, "completed")
			want := mode
			if mode == "progress" {
				want = "limit"
			}
			if r.Quality != want {
				t.Fatalf("wanted %s got %s", want, r.Quality)
			}
			if mode == "stalled" && (checks != 3 || r.StopReason == "" || r.Document == "") {
				t.Fatal("stagnation lost explanation or document")
			}
		})
	}
	r := Run{}
	for i := 0; i < 3; i++ {
		r.Tasks = append(r.Tasks, Task{Stage: "revision", Status: "done", Output: "Same document"}, Task{Stage: "verify", Status: "done", Output: "Same issue\n[TANDEM_REVISE]"})
	}
	if stagnationReason(r) == "" {
		t.Fatal("exact repetition not detected")
	}
	r.Tasks[4].Output = "Changed requirement"
	if stagnationReason(r) != "" {
		t.Fatal("document improvement ignored")
	}
}

func TestContinuationPreservesContextAndDoesNotReplayIdeas(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	m, err := s.SaveMaterial(p.ID, "original.md", "requirements", "text/plain", []byte("ORIGINAL_MATERIAL"))
	if err != nil {
		t.Fatal(err)
	}
	parent := Run{ID: ID(), ProjectID: p.ID, Mode: "live", Status: "completed", Quality: "limit", Brief: "Original task", Acceptance: "Acceptance", Title: p.Title, CreatedAt: now(), Document: "PARENT_DOCUMENT", Materials: []Material{m}, Models: map[string]string{"claude": "sonnet"}, LockedModels: map[string]string{"codex": "gpt-test", "claude": "claude-test"}, Tasks: []Task{{ID: ID(), Stage: "verify", Status: "done", Output: "PARENT_REVIEW"}}}
	if err = s.PutRun(parent); err != nil {
		t.Fatal(err)
	}
	_ = s.AddMessage(Message{ProjectID: p.ID, RunID: parent.ID, Actor: "user", Target: "codex", Stage: "steering", Text: "CODEX_ONLY_NOTE"})
	_ = s.AddMessage(Message{ProjectID: p.ID, RunID: parent.ID, Actor: "user", Target: "all", Stage: "steering", Text: "SHARED_NOTE"})
	_ = s.DeleteMaterial(m.ID)
	_, _ = s.SaveMaterial(p.ID, "new.md", "requirements", "text/plain", []byte("EXCLUDED_NEW_MATERIAL"))
	var mu sync.Mutex
	calls := []AgentInput{}
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		mu.Lock()
		calls = append(calls, in)
		mu.Unlock()
		for _, text := range []string{"PARENT_DOCUMENT", "PARENT_REVIEW", "ORIGINAL_MATERIAL", "NEW_DIRECTION", "SHARED_NOTE"} {
			if !strings.Contains(in.Prompt, text) {
				t.Errorf("missing %s", text)
			}
		}
		if strings.Contains(in.Prompt, "EXCLUDED_NEW_MATERIAL") {
			t.Error("material snapshot changed")
		}
		if in.Stage == "ideas" || in.Stage == "critique" || in.Stage == "draft" {
			t.Error("replayed initial discussion")
		}
		if in.Session != "" {
			t.Error("reused parent session")
		}
		if in.Agent == "claude" && strings.Contains(in.Prompt, "CODEX_ONLY_NOTE") {
			t.Error("targeted note leaked")
		}
		model := parent.LockedModels[in.Agent]
		if in.Model != model {
			t.Errorf("model changed: %s", in.Model)
		}
		return AgentResult{Text: "New document\n[TANDEM_READY]", Model: model}, nil
	}))
	defer e.Close()
	r, err := e.Continue(parent.ID, 5, "NEW_DIRECTION")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	if r.ParentRunID != parent.ID || r.Quality != "ready" || len(r.Tasks) != 2 || len(calls) != 2 || r.Document == parent.Document {
		t.Fatalf("bad continuation: %+v", r)
	}
	old, _ := s.Run(parent.ID)
	if old.Document != parent.Document || old.Quality != "limit" {
		t.Fatal("changed parent history")
	}
	copy, err := os.ReadFile(filepath.Join(s.Dir, "runs", r.ID, m.ID+".original"))
	if err != nil || string(copy) != "ORIGINAL_MATERIAL" {
		t.Fatal("lost original material")
	}
}

func TestModelsPinnedAndRecordedAndChangesRejected(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			s := testStore(t)
			p := testProject(t, s)
			e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
				model := "gpt-test"
				if in.Agent == "claude" {
					model = "claude-test"
				}
				if in.Stage == "ideas" {
					if in.Agent == "claude" && in.Model != "sonnet" {
						t.Error("lost selected alias")
					}
				} else if in.Model != model {
					t.Errorf("not pinned: %s", in.Model)
				}
				if changed && in.Agent == "codex" && in.Stage == "critique" {
					model = "gpt-other"
				}
				return AgentResult{Text: "Document\n[TANDEM_READY]", Model: model}, nil
			}))
			defer e.Close()
			r, err := e.Create(p.ID, "live", "Plan", 1, "", map[string]string{"claude": "sonnet"})
			if err != nil {
				t.Fatal(err)
			}
			status := "completed"
			if changed {
				status = "failed"
			}
			r = awaitRun(t, s, r.ID, status)
			if r.LockedModels["codex"] != "gpt-test" || r.LockedModels["claude"] != "claude-test" {
				t.Fatal("lost model lock")
			}
			msgs, _ := s.Messages(p.ID)
			for _, m := range msgs {
				if m.Actor != "user" && m.Model == "" {
					t.Fatal("missing actual model")
				}
				if m.Model == "gpt-other" {
					t.Fatal("accepted changed model")
				}
			}
			if changed && !strings.Contains(r.Error, "сменил модель") {
				t.Fatal("missing model change explanation")
			}
		})
	}
}

func TestModelArgumentsMetadataAndCatalog(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		for _, session := range []string{"", "exact-id"} {
			args := commandArgs(AgentInput{Agent: agent, Session: session, Model: "chosen-model"})
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--model chosen-model") || session != "" && !strings.Contains(joined, session) {
				t.Fatalf("bad args: %v", args)
			}
		}
	}
	for _, bad := range []string{"-bad", "$(command)", "bad\nmodel", "model with spaces"} {
		if validModel(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	state := streamState{}
	state.parse("claude", []byte(`{"type":"assistant","message":{"model":"claude-actual","content":[{"type":"thinking","thinking":"PRIVATE"},{"type":"text","text":"visible"}]}}`), func(_, v string) {
		if strings.Contains(v, "PRIVATE") {
			t.Fatal("leaked reasoning")
		}
	})
	if state.model != "claude-actual" {
		t.Fatal("did not read actual model")
	}
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	path := filepath.Join(dir, "sessions", "2026", "10", "11")
	_ = os.MkdirAll(path, 0700)
	_ = os.WriteFile(filepath.Join(path, "rollout-session-123.jsonl"), []byte("{\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-first\",\"instructions\":\"PRIVATE\"}}\n{\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-last\"}}\n"), 0600)
	if sessionModel("codex", "session-123") != "gpt-last" || sessionModel("codex", "../*") != "" {
		t.Fatal("bad session metadata lookup")
	}
	_ = os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-visible","display_name":"Visible","visibility":"list"},{"slug":"gpt-hidden","visibility":"hide"}]}`), 0600)
	s := testStore(t)
	catalog := modelCatalog(s)
	if len(catalog["codex"]) != 1 || catalog["codex"][0].ID != "gpt-visible" {
		t.Fatal("catalog did not respect visibility")
	}
}

func TestContinueHTTPAndInvalidState(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	e := NewEngine(s, CLIRunner{})
	defer e.Close()
	parent := Run{ID: ID(), ProjectID: p.ID, Mode: "demo", Status: "completed", Brief: "Plan", Document: "Saved", CreatedAt: now()}
	_ = s.PutRun(parent)
	server := NewServer(s, e, fstest.MapFS{})
	request := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/runs/"+parent.ID+"/continue", strings.NewReader(body))
		r.Host = "localhost:4317"
		r.Header.Set("X-Tandem-Token", server.Token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	if w := request(`{"maxRevisions":11}`); w.Code != 409 {
		t.Fatalf("invalid budget: %d", w.Code)
	}
	w := request(`{"maxRevisions":10,"instruction":"Keep scope"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var child Run
	_ = json.Unmarshal(w.Body.Bytes(), &child)
	if child.ParentRunID != parent.ID || len(child.Tasks) != 2 {
		t.Fatal("route did not create continuation")
	}
	if w := request(`{"maxRevisions":5}`); w.Code != 409 {
		t.Fatal("allowed duplicate active continuation")
	}
	if _, err := e.Continue(child.ID, 1, ""); err == nil {
		t.Fatal("continued an active cycle")
	}
}
