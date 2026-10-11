package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func TestAvailabilityDiagnostics(t *testing.T) {
	for _, tc := range []struct{ agent, line, kind string }{
		{"codex", `{"type":"turn.failed","error":{"message":"You've hit your usage limit"}}`, "limit"},
		{"claude", `{"type":"result","is_error":true,"result":"Rate limit reached"}`, "limit"},
		{"claude", `{"type":"assistant","error":"rate_limit","message":{"content":[{"type":"text","text":"Try later"}]}}`, "limit"},
		{"codex", `{"type":"turn.failed","error":{"message":"Please log in"}}`, "auth"},
		{"codex", `{"type":"turn.failed","error":{"message":"HTTP 503 service unavailable"}}`, "network"},
		{"claude", `{"type":"assistant","message":{"content":[{"type":"text","text":"Implement a rate limit / unauthorized response"}]}}`, ""},
		{"codex", `{"type":"item.completed","item":{"type":"agent_message","text":"usage limit test"}}`, ""},
		{"codex", `{"type":"turn.failed","error":{"message":"invalid model selection"}}`, ""},
	} {
		state := streamState{}
		state.parse(tc.agent, []byte(tc.line), func(string, string) {})
		got := ""
		if state.unavailable != nil {
			got = state.unavailable.Kind
		}
		if got != tc.kind {
			t.Errorf("%s: want %q, got %q", tc.line, tc.kind, got)
		}
	}
	state := streamState{}
	state.parse("codex", []byte(`{"type":"error","message":"HTTP 503"}`), func(string, string) {})
	state.parse("codex", []byte(`{"type":"turn.completed"}`), func(string, string) {})
	if state.unavailable != nil {
		t.Fatal("a recovered CLI retry is still unavailable")
	}
}

func TestQuotaMidCycleChangesAuthorAndKeepsModels(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, emit func(string, string)) (AgentResult, error) {
		want := "gpt-codex"
		if in.Agent == "claude" {
			want = "claude-fixture"
		}
		if in.Model != want {
			t.Errorf("handoff used another provider's model: %s", in.Model)
		}
		if in.Stage == "revision" && in.Agent == "codex" {
			return AgentResult{}, &AgentUnavailableError{Agent: "codex", Kind: "limit", Detail: "quota mid-cycle"}
		}
		if in.Stage == "revision" && in.Session != "" {
			t.Error("handoff reused opaque reviewer session")
		}
		emit("session", "session-"+in.Agent)
		text := "Document"
		if in.Stage == "verify" {
			text = "Ready\n[TANDEM_READY]"
		}
		return AgentResult{Text: text, Model: want}, nil
	}))
	defer e.Close()
	r, err := e.CreateConfigured(p.ID, "live", "Game", 1, "", map[string]string{"codex": "gpt-codex", "claude": "claude-fixture"}, AvailabilityOptions{Policy: "solo"})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	draft, revision, verify := r.Tasks[4], r.Tasks[6], r.Tasks[7]
	if draft.Agent != "codex" || revision.Agent != "claude" || revision.RequestedAgent != "codex" || !verify.SelfReview || r.Quality != "self_checked" {
		t.Fatalf("mid-cycle provenance lost: %+v", r.Tasks)
	}
	if r.LockedModels["codex"] != "gpt-codex" || r.LockedModels["claude"] != "claude-fixture" {
		t.Fatal("handoff changed pinned models")
	}
}

func TestSoloHandoffBothDirectionsAndDeferredDocumentReview(t *testing.T) {
	for _, offline := range []string{"claude", "codex"} {
		t.Run(offline, func(t *testing.T) {
			s := testStore(t)
			p := testProject(t, s)
			var mu sync.Mutex
			recovered := false
			offlineCalls := 0
			calls := map[string]int{}
			active := map[string]int{}
			runner := runnerFunc(func(ctx context.Context, in AgentInput, emit func(string, string)) (AgentResult, error) {
				mu.Lock()
				calls[in.Agent]++
				active[in.Agent]++
				if active[in.Agent] > 1 {
					t.Error("concurrent use of the same provider")
				}
				isRecovered := recovered
				if in.Agent == offline && !isRecovered {
					offlineCalls++
				}
				mu.Unlock()
				defer func() { mu.Lock(); active[in.Agent]--; mu.Unlock() }()
				if in.Agent == offline && !isRecovered {
					return AgentResult{}, &AgentUnavailableError{Agent: offline, Kind: "limit", Detail: "Test quota exhausted"}
				}
				select {
				case <-ctx.Done():
					return AgentResult{}, ctx.Err()
				case <-time.After(time.Millisecond):
				}
				text := "Document: game mechanics"
				if in.Stage == "review" || in.Stage == "verify" {
					if !isRecovered && !strings.Contains(in.Prompt, "САМОПРОВЕРКА") {
						t.Error("self-review not disclosed to model")
					}
					text = "Criteria satisfied\n[TANDEM_READY]"
				}
				if in.Stage == "independent_review" {
					if !isRecovered || in.Agent != offline || in.Session != "" {
						t.Error("audit was not fresh/independent")
					}
					if !strings.Contains(in.Prompt, "Document: game mechanics") {
						t.Error("missing saved document")
					}
					text = encoded(CodeReview{Ready: true, Summary: "Independent check", ManualChecks: []string{"Try the mechanic"}})
				}
				if isRecovered && in.Stage == "revision" && !strings.Contains(in.Prompt, "Independent check") {
					t.Error("continuation lost independent findings")
				}
				emit("session", "session-"+in.Agent)
				return AgentResult{Text: text, Model: "model-" + in.Agent}, nil
			})
			e := NewEngine(s, runner)
			defer e.Close()
			r, err := e.CreateConfigured(p.ID, "live", "Game", 5, "Runnable mechanic", nil, AvailabilityOptions{Policy: "solo"})
			if err != nil {
				t.Fatal(err)
			}
			r = awaitRun(t, s, r.ID, "completed")
			waitInactive(t, e, r.ID)
			if r.Quality != "self_checked" || !r.NeedsReview || len(r.Tasks) != 8 {
				t.Fatalf("bad solo result: %+v", r)
			}
			mu.Lock()
			n := offlineCalls
			mu.Unlock()
			if n != 1 {
				t.Fatalf("quota retry storm: %d", n)
			}
			for _, task := range r.Tasks {
				if task.Agent != otherAgent(offline) || task.Model != "model-"+otherAgent(offline) || task.RequestedAgent == "" {
					t.Fatalf("false provenance: %+v", task)
				}
				if reviewStage(task.Stage) && !task.SelfReview {
					t.Fatal("self review called independent")
				}
			}
			messages, _ := s.Messages(p.ID)
			found := false
			for _, m := range messages {
				if m.Stage == "verify" {
					found = m.SelfReview && m.Actor == otherAgent(offline)
				}
			}
			if !found {
				t.Fatal("persisted messages lost actual reviewer")
			}
			original := r.Document
			mu.Lock()
			recovered = true
			mu.Unlock()
			if _, err = e.RequestAudit(r.ID, "", ""); err != nil {
				t.Fatal(err)
			}
			r = awaitRun(t, s, r.ID, "completed")
			waitInactive(t, e, r.ID)
			if r.NeedsReview || r.Quality != "ready" || r.Document != original || len(r.Audits) != 1 || r.Audits[0].Review == nil {
				t.Fatalf("audit lost history: %+v", r)
			}
			if len(r.Unavailable) > 0 {
				t.Fatal("reviewer remains blocked")
			}
			// Later continuation receives the independently discovered remarks as context.
			child, err := e.Continue(r.ID, 1, "Improve")
			if err != nil {
				t.Fatal(err)
			}
			if child.AvailabilityPolicy != "solo" {
				t.Fatal("continuation lost fallback policy")
			}
			awaitRun(t, s, child.ID, "completed")
			waitInactive(t, e, child.ID)
		})
	}
}

func TestCLIFailureClassificationAtProcessBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixtures")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for _, tc := range []struct{ agent, script, kind string }{
		{"codex", "echo '{\"type\":\"turn.failed\",\"error\":{\"message\":\"usage limit PRIVATE_TOKEN\"}}'\nexit 1\n", "limit"},
		{"claude", "echo 'HTTP 401 PRIVATE_TOKEN' >&2\nexit 1\n", "auth"},
		{"claude", "echo '{\"type\":\"result\",\"is_error\":true,\"result\":\"service unavailable\"}'\n", "network"},
		{"codex", "echo 'invalid model PRIVATE_TOKEN' >&2\nexit 1\n", ""},
	} {
		if err := os.WriteFile(filepath.Join(dir, tc.agent), []byte("#!/bin/sh\n"+tc.script), 0700); err != nil {
			t.Fatal(err)
		}
		_, err := (CLIRunner{}).Run(context.Background(), AgentInput{Agent: tc.agent, Directory: dir}, func(string, string) {})
		if err == nil {
			t.Fatal("failed process accepted")
		}
		var issue *AgentUnavailableError
		got := ""
		if errors.As(err, &issue) {
			got = issue.Kind
		}
		if got != tc.kind || strings.Contains(err.Error(), "PRIVATE_TOKEN") {
			t.Fatalf("wrong classification / leaked diagnostics: %v", err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	_, err := (CLIRunner{}).Run(context.Background(), AgentInput{Agent: "codex", Directory: dir}, func(string, string) {})
	var issue *AgentUnavailableError
	if !errors.As(err, &issue) || issue.Kind != "missing_cli" {
		t.Fatal("missing CLI not classified")
	}
}

func TestSoloAPIAuthenticatesOnlyAvailableSubscription(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix auth fixtures")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for name, body := range map[string]string{"codex": "echo 'Logged in using ChatGPT'\n", "claude": "echo '{\"loggedIn\":false}'\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	s := testStore(t)
	p := testProject(t, s)
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		if in.Agent != "codex" {
			t.Error("provider without subscription was invoked")
		}
		return AgentResult{Text: "Document\n[TANDEM_READY]"}, nil
	}))
	defer e.Close()
	server := NewServer(s, e, fstest.MapFS{})
	request := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/"+path, strings.NewReader(body))
		req.Host = "localhost:4317"
		req.Header.Set("X-Tandem-Token", server.Token)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	if w := request("projects/"+p.ID+"/runs", `{"mode":"live","brief":"Game","availability":{"policy":"wait"}}`); w.Code != 409 {
		t.Fatal("wait policy ignored auth")
	}
	w := request("projects/"+p.ID+"/runs", `{"mode":"live","brief":"Game","maxRevisions":1,"availability":{"policy":"solo"}}`)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var r Run
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	if r.Unavailable["claude"].Kind != "auth" || !r.NeedsReview {
		t.Fatal("preflight did not carry outage into run")
	}
	w = request("runs/"+r.ID+"/availability", `{"policy":"solo","returnAgent":"claude"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	saved, _ := s.Run(r.ID)
	if len(saved.Unavailable) > 0 {
		t.Fatal("settings endpoint did not restore provider")
	}
	w = request("runs/"+r.ID+"/independent-review", `{}`)
	if w.Code != 409 {
		t.Fatal("audit bypassed subscription preflight")
	}
}

func TestWaitGenericFailureAndBothUnavailable(t *testing.T) {
	for _, scenario := range []string{"wait", "both", "malformed", "known"} {
		t.Run(scenario, func(t *testing.T) {
			s := testStore(t)
			p := testProject(t, s)
			calls := map[string]int{}
			var mu sync.Mutex
			e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
				mu.Lock()
				calls[in.Agent]++
				mu.Unlock()
				if in.Agent == "claude" || scenario == "both" {
					if scenario == "malformed" {
						return AgentResult{}, errors.New("malformed model answer")
					}
					return AgentResult{}, &AgentUnavailableError{Agent: in.Agent, Kind: "limit", Detail: "quota"}
				}
				return AgentResult{Text: "Ready\n[TANDEM_READY]"}, nil
			}))
			defer e.Close()
			options := AvailabilityOptions{Policy: "solo"}
			want := "failed"
			if scenario == "wait" {
				options.Policy = "wait"
			}
			if scenario == "known" {
				options.UnavailableAgent = "claude"
				want = "completed"
			}
			r, err := e.CreateConfigured(p.ID, "live", "Game", 1, "", nil, options)
			if err != nil {
				t.Fatal(err)
			}
			r = awaitRun(t, s, r.ID, want)
			waitInactive(t, e, r.ID)
			mu.Lock()
			cc := calls["claude"]
			mu.Unlock()
			if scenario == "known" && cc != 0 {
				t.Fatal("called a manually disabled provider")
			}
			if scenario == "both" && len(r.Unavailable) != 2 {
				t.Fatal("missing double outage")
			}
			if scenario == "malformed" && len(r.Unavailable) != 0 {
				t.Fatal("a content error triggered provider failover")
			}
			if scenario == "wait" {
				if r.Quality == "self_checked" {
					t.Fatal("wait policy silently continued")
				}
				if _, err = e.SetAvailability(r.ID, AvailabilityOptions{Policy: "solo"}, ""); err != nil {
					t.Fatal(err)
				}
				if _, err = e.Resume(r.ID); err != nil {
					t.Fatal(err)
				}
				r = awaitRun(t, s, r.ID, "completed")
				waitInactive(t, e, r.ID)
				mu.Lock()
				after := calls["claude"]
				mu.Unlock()
				if after != cc {
					t.Fatal("resume retried known outage")
				}
				if !r.NeedsReview {
					t.Fatal("resume cleared review debt")
				}
			}
		})
	}
}

func TestIndependentReviewNeverFallsBackAndResumes(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	document := Task{ID: ID(), Agent: "codex", Stage: "revision", Status: "done", Output: "Saved document"}
	r := Run{ID: ID(), ProjectID: p.ID, Mode: "live", Status: "completed", Quality: "self_checked", NeedsReview: true, AvailabilityPolicy: "solo", Tasks: []Task{document}, Sessions: map[string]string{}, Document: document.Output}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	fail := true
	calls := 0
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		calls++
		if in.Agent != "claude" {
			t.Error("audit fell back to its author")
		}
		if fail {
			return AgentResult{}, &AgentUnavailableError{Agent: "claude", Kind: "limit", Detail: "quota"}
		}
		return AgentResult{Text: encoded(CodeReview{Summary: "Fix the balance", Issues: []string{"Unbounded growth"}})}, nil
	}))
	defer e.Close()
	if _, err := e.RequestAudit(r.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "failed")
	waitInactive(t, e, r.ID)
	if calls != 1 || !r.NeedsReview || r.Document != document.Output {
		t.Fatal("failed audit changed saved artifact")
	}
	fail = false
	if _, err := e.SetAvailability(r.ID, AvailabilityOptions{Policy: "solo"}, "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(r.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	if calls != 2 || r.Quality != "review_changes" || r.NeedsReview {
		t.Fatal("audit did not resume with independent findings")
	}
}

func TestSubscriptionPreflightForSolo(t *testing.T) {
	statuses := []AgentStatus{{Agent: "codex", Installed: true, Subscription: true}, {Agent: "claude", Installed: true, Subscription: false, Detail: "No subscription"}}
	issues, err := availableSubscriptions(AvailabilityOptions{Policy: "solo"}, statuses)
	if err != nil || issues["claude"].Kind != "auth" {
		t.Fatal("one subscription must suffice; the other must not be invoked")
	}
	if _, err := availableSubscriptions(AvailabilityOptions{Policy: "wait"}, statuses); err == nil {
		t.Fatal("wait policy accepted missing provider")
	}
	if _, err := availableSubscriptions(AvailabilityOptions{Policy: "solo", UnavailableAgent: "codex"}, statuses); err == nil {
		t.Fatal("accepted no available providers")
	}
}

func TestSoloPrototypeAndAuditPinnedCheckpoint(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	approval := fixtureApproval(t, s, p)
	plan := fixturePlan(t)
	var audited string
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		if in.Stage == "independent_review" {
			audited = in.Prompt
			return AgentResult{Text: encoded(CodeReview{Ready: true, Summary: "Independent approval"})}, nil
		}
		if in.Agent == "claude" {
			t.Error("disabled Claude was called")
		}
		switch in.Stage {
		case "prototype_plan_final":
			return AgentResult{Text: encoded(plan)}, nil
		case "prototype_code":
			return AgentResult{Text: encoded(fixtureCode(2))}, nil
		case "prototype_review":
			return AgentResult{Text: encoded(CodeReview{Ready: true, Summary: "Self check passed"})}, nil
		}
		return AgentResult{Text: "Plan"}, nil
	}))
	defer e.Close()
	r, err := e.CreatePrototype(p.ID, PrototypeConfig{RevisionID: approval.ID, Platform: "browser", MaxRounds: 1, Availability: AvailabilityOptions{Policy: "solo", UnavailableAgent: "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	waitInactive(t, e, r.ID)
	if !r.NeedsReview {
		t.Fatal("solo plan claims independent review")
	}
	if _, err = e.ApprovePrototype(r.ID, r.Prototype.PlanHash); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	if r.Quality != "self_checked" || len(r.Prototype.Builds) != 1 {
		t.Fatalf("prototype failed: %+v", r)
	}
	build := r.Prototype.Builds[0]
	if build.Author != "codex" || build.ReviewAgent != "codex" || !build.SelfReview || !checksPassed(build, plan) {
		t.Fatalf("incorrect build provenance: %+v", build)
	}
	// A review must never accidentally read unsaved edits from the working tree.
	if err = os.WriteFile(filepath.Join(r.Prototype.Workspace, "game.cjs"), []byte("UNSAVED_PRIVATE_CHANGE"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.RequestAudit(r.ID, "", build.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	if strings.Contains(audited, "UNSAVED_PRIVATE_CHANGE") || !strings.Contains(audited, "exports.turn = energy => energy + 2") || !strings.Contains(audited, build.Commit) {
		t.Fatal("audit did not read pinned Git snapshot")
	}
	if r.NeedsReview || r.Quality != "ready" || r.Audits[0].Agent != "claude" || !r.Prototype.Builds[0].SelfReview {
		t.Fatal("audit lost distinction from original self-review")
	}
	bytes, err := os.ReadFile(filepath.Join(r.Prototype.Workspace, "game.cjs"))
	if err != nil || string(bytes) != "UNSAVED_PRIVATE_CHANGE" {
		t.Fatal("audit modified working files")
	}
}
