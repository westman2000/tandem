package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func fixtureApproval(t *testing.T, s *Store, p Project) Revision {
	t.Helper()
	r := Run{ID: ID(), ProjectID: p.ID, Kind: "design", Mode: "live", Status: "completed", CreatedAt: now(), Title: p.Title, Document: "IMMUTABLE DESIGN: turn increases energy by 2", Models: map[string]string{}, LockedModels: map[string]string{}, Tasks: []Task{{ID: ID(), Stage: "draft", Status: "done", Output: "Draft"}, {ID: ID(), Stage: "revision", Status: "done", Output: "IMMUTABLE DESIGN: turn increases energy by 2"}}}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	vs, err := s.Revisions(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.ApproveRevision(p.ID, vs[len(vs)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func encoded(v any) string { b, _ := json.Marshal(v); return string(b) }
func fixturePlan(t *testing.T) PrototypePlan {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is needed for the runnable prototype fixture")
	}
	return PrototypePlan{Title: "Energy loop", Platform: "browser", Stack: "HTML + JavaScript", Hypothesis: "A full turn", Scope: []string{"one turn"}, Simplifications: []string{"temporary graphics"}, Acceptance: []string{"turn adds 2"}, Tools: []string{"node"}, Checks: []CheckCommand{{Name: "Mechanic", Kind: "test", Args: []string{node, "-e", `const assert=require('node:assert/strict');const game=require('./game.cjs');assert.equal(game.turn(3),5);console.log('MECHANIC PASSED')`}, TimeoutSeconds: 10}}, RunInstructions: "Open index.html", ManualChecks: []string{"Play one turn in the browser"}}
}
func fixtureCode(increment int) CodeProposal {
	return CodeProposal{Summary: fmt.Sprintf("Energy turn +%d", increment), Decisions: []string{"One deterministic turn first"}, Limitations: []string{"Graphics are placeholders"}, Files: []FileEdit{{Path: "game.cjs", Content: fmt.Sprintf("exports.turn = energy => energy + %d;\n", increment)}, {Path: "index.html", Content: "<!doctype html><title>Energy loop</title><button>Turn</button>"}, {Path: ".gitignore", Content: "node_modules/\n.tmp\n"}}}
}
func waitInactive(t *testing.T, e *Engine, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		_, active := e.active[id]
		e.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("engine still active")
}

func TestRevisionApprovalAndLegacyHistory(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a := fixtureApproval(t, s, p)
	vs, err := s.Revisions(p.ID)
	if err != nil || len(vs) != 2 || vs[1].ApprovedAt == "" {
		t.Fatalf("versions: %+v %v", vs, err)
	}
	old, err := s.ApproveRevision(p.ID, a.ID)
	if err != nil || old.ApprovedAt != a.ApprovedAt {
		t.Fatal("approval is not idempotent")
	}
	_, err = s.UpdateRun(a.RunID, func(r *Run) { r.Document = "NEW TEXT" })
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Approval(p.ID, a.ID)
	if err != nil || saved.Text != a.Text || saved.SHA256 != a.SHA256 {
		t.Fatal("approval changed")
	}
	if _, err = s.Approval("different-project", a.ID); err == nil {
		t.Fatal("cross-project approval")
	}
	demo := Run{ID: ID(), ProjectID: p.ID, Mode: "demo", CreatedAt: now(), Document: "Demo"}
	if err = s.PutRun(demo); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveRevision(p.ID, demo.ID+"-document"); err == nil {
		t.Fatal("approved demo")
	}
}
func TestPrototypePlanCodeChecksFeedbackAndRollback(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a := fixtureApproval(t, s, p)
	plan := fixturePlan(t)
	calls := 0
	var mu sync.Mutex
	runner := runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		if !strings.Contains(in.Prompt, a.Text) {
			t.Error("missing immutable source")
		}
		if in.Session != "" {
			t.Error("coding reused opaque CLI state")
		}
		answer := "Review the plan"
		switch in.Stage {
		case "prototype_plan_final":
			answer = encoded(plan)
		case "prototype_code":
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			increment := 1
			if n > 1 {
				increment = 2
			}
			answer = encoded(fixtureCode(increment))
		case "prototype_review":
			if !strings.Contains(in.Prompt, "exitCode") || !strings.Contains(in.Prompt, "game.cjs") {
				t.Error("reviewer cannot see evidence")
			}
			mu.Lock()
			ready := calls > 1
			mu.Unlock()
			issues := []string{}
			if !ready {
				issues = []string{"Use +2, not +1"}
			}
			answer = encoded(CodeReview{Ready: ready, Summary: "Reviewed actual mechanic check", Issues: issues, ManualChecks: []string{"Play in browser"}})
		}
		return AgentResult{Text: answer}, nil
	})
	e := NewEngine(s, runner)
	defer e.Close()
	r, err := e.CreatePrototype(p.ID, PrototypeConfig{RevisionID: a.ID, Author: "claude", MaxRounds: 3})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	waitInactive(t, e, r.ID)
	if r.Prototype.Plan == nil || r.Tasks[0].Agent != "claude" || r.Tasks[1].Agent != "codex" {
		t.Fatal("roles or plan missing")
	}
	if _, err = os.Stat(r.Prototype.Workspace); !os.IsNotExist(err) {
		t.Fatal("code folder exists before plan approval")
	}
	if _, err = e.ApprovePrototype(r.ID, "stale-plan"); err == nil {
		t.Fatal("approved wrong plan")
	}
	if _, err = e.Resume(r.ID); err == nil {
		t.Fatal("resume bypassed plan approval")
	}
	// Exercise a plan revision without replaying the first final-plan checkpoint.
	r, err = e.PrototypeAction(r.ID, "revise-plan", "", "Keep one simple turn")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	waitInactive(t, e, r.ID)
	if len(r.Tasks) != 5 || r.Tasks[4].Status != "done" {
		t.Fatalf("revised plan not executed: %+v", r.Tasks)
	}
	r, err = e.ApprovePrototype(r.ID, r.Prototype.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	if r.Quality != "ready" || len(r.Prototype.Builds) != 2 {
		t.Fatalf("result %s %s builds %d", r.Quality, r.Error, len(r.Prototype.Builds))
	}
	b1, b2 := r.Prototype.Builds[0], r.Prototype.Builds[1]
	if b1.Checks[0].Status != "failed" || b2.Checks[0].Status != "passed" || !strings.Contains(b2.Checks[0].Output, "MECHANIC PASSED") {
		t.Fatalf("fake check success: %+v", r.Prototype.Builds)
	}
	if b2.AcceptedAt != "" {
		t.Fatal("agent accepted on behalf of user")
	}
	r, err = e.PrototypeAction(r.ID, "accept", b2.ID, "Played it")
	if err != nil || r.Prototype.Builds[1].AcceptedAt == "" {
		t.Fatal("accept", err)
	}
	if err = os.WriteFile(filepath.Join(r.Prototype.Workspace, "user.txt"), []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.PrototypeAction(r.ID, "restore", b1.ID, ""); err == nil {
		t.Fatal("rollback discarded user changes")
	}
	if err = os.Remove(filepath.Join(r.Prototype.Workspace, "user.txt")); err != nil {
		t.Fatal(err)
	}
	r, err = e.PrototypeAction(r.ID, "restore", b1.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(filepath.Join(r.Prototype.Workspace, "game.cjs"))
	if !strings.Contains(string(content), "+ 1") {
		t.Fatal("did not restore first build")
	}
	r, err = e.PrototypeAction(r.ID, "feedback", b1.ID, "Improve the same turn")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	if len(r.Prototype.Builds) != 3 || r.Prototype.Builds[1].Commit != b2.Commit || r.Prototype.Source.Text != a.Text {
		t.Fatal("feedback lost earlier builds or changed source")
	}
	server := NewServer(s, e, fstest.MapFS{})
	for _, url := range []string{"http://localhost/api/runs/" + r.ID + "/journal", "http://localhost/api/runs/" + r.ID + "/builds/" + b1.ID + "/source"} {
		req := httptest.NewRequest("GET", url, nil)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("export %s: %d %s", url, w.Code, w.Body.String())
		}
		if strings.HasSuffix(url, "/source") && !strings.HasPrefix(w.Body.String(), "PK") {
			t.Fatal("not zip")
		}
	}
}
func TestPrototypeFailureLimitAndMissingTools(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a := fixtureApproval(t, s, p)
	plan := fixturePlan(t)
	plan.Tools = append(plan.Tools, "tandem_nonexistent_compiler_fixture")
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		answer := "Plan review"
		switch in.Stage {
		case "prototype_plan_final":
			answer = encoded(plan)
		case "prototype_code":
			answer = encoded(fixtureCode(1))
		case "prototype_review":
			answer = encoded(CodeReview{Ready: true, Summary: "Reviewer mistakenly claims success"})
		}
		return AgentResult{Text: answer}, nil
	}))
	defer e.Close()
	r, err := e.CreatePrototype(p.ID, PrototypeConfig{RevisionID: a.ID, MaxFailures: 2})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	waitInactive(t, e, r.ID)
	if _, err = e.ApprovePrototype(r.ID, r.Prototype.PlanHash); err == nil {
		t.Fatal("missing tool permitted coding")
	}
	_, err = s.UpdateRun(r.ID, func(r *Run) { r.Prototype.Plan.Tools = []string{"node"} })
	if err != nil {
		t.Fatal(err)
	}
	r, err = e.ApprovePrototype(r.ID, r.Prototype.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	if r.Quality != "stalled" || len(r.Prototype.Builds) != 2 {
		t.Fatalf("unchecked reviewer success accepted: %s", r.Quality)
	}
}
func TestPrototypePauseRecoveryAndSafePaths(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a := fixtureApproval(t, s, p)
	plan := fixturePlan(t)
	started := make(chan struct{})
	var once sync.Once
	e := NewEngine(s, runnerFunc(func(ctx context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		answer := "Plan review"
		if in.Stage == "prototype_plan_final" {
			answer = encoded(plan)
		}
		if in.Stage == "prototype_code" {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return AgentResult{}, ctx.Err()
		}
		return AgentResult{Text: answer}, nil
	}))
	defer e.Close()
	r, err := e.CreatePrototype(p.ID, PrototypeConfig{RevisionID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	waitInactive(t, e, r.ID)
	r, err = e.ApprovePrototype(r.ID, r.Prototype.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no code call")
	}
	if _, err = e.Pause(r.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "paused")
	waitInactive(t, e, r.ID)
	if r.Tasks[3].Status != "pending" || r.Prototype.BaseCommit == "" {
		t.Fatal("lost paused checkpoint")
	}
	for _, name := range []string{"../outside", "/tmp/outside", ".git/config", "x/../../outside", "x\\outside", "a/.GIT/config"} {
		if _, err = safeProjectPath(r.Prototype.Workspace, name); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	outside := t.TempDir()
	link := filepath.Join(r.Prototype.Workspace, "linked")
	if err = os.Symlink(outside, link); err == nil {
		if _, err = safeProjectPath(r.Prototype.Workspace, "linked/outside.txt"); err == nil {
			t.Fatal("followed symlink")
		}
	}
	_, err = s.UpdateRun(r.ID, func(r *Run) {
		r.Status = "running"
		r.Prototype.ActiveSince = time.Now().Add(-3 * time.Second).Format(time.RFC3339Nano)
		r.Prototype.Builds = []Build{{Checks: []CheckResult{{Status: "running"}}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(r.ID)
	if r.Status != "paused" || r.Prototype.Builds[0].Checks[0].Status != "interrupted" || r.Prototype.UsedSeconds < 3 {
		t.Fatal("recovery lost command state or time budget")
	}
}
func TestPrototypeJournalIncludesAllEventsAndCSRF(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a := fixtureApproval(t, s, p)
	for i := 0; i < 510; i++ {
		if err := s.Event(a.RunID, "system", "check", fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	recent, _ := s.Events(a.RunID)
	all, _ := s.events(a.RunID, true)
	if len(recent) != 500 || len(all) != 510 {
		t.Fatal("journal export was truncated")
	}
	e := NewEngine(s, nil)
	defer e.Close()
	server := NewServer(s, e, fstest.MapFS{})
	req := httptest.NewRequest("POST", "http://localhost/api/projects/"+p.ID+"/revisions/"+a.ID+"/approve", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("no CSRF on development API")
	}
}

func TestPrototypeExistingRepositoryAndCommitRecovery(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a := fixtureApproval(t, s, p)
	plan := fixturePlan(t)
	source := t.TempDir()
	ctx := context.Background()
	if _, err := git(ctx, source, "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "original.txt"), []byte("KEEP ORIGINAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, source, "add", "original.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, source, "commit", "-m", "original"); err != nil {
		t.Fatal(err)
	}
	initialHead, _ := git(ctx, source, "rev-parse", "HEAD")
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		answer := "Plan"
		switch in.Stage {
		case "prototype_plan_final":
			answer = encoded(plan)
		case "prototype_code":
			if !strings.Contains(in.Prompt, "KEEP ORIGINAL") {
				t.Error("missing repository context")
			}
			answer = encoded(fixtureCode(2))
		case "prototype_review":
			answer = encoded(CodeReview{Ready: true, Summary: "Good", Issues: []string{}})
		}
		return AgentResult{Text: answer}, nil
	}))
	defer e.Close()
	r, err := e.CreatePrototype(p.ID, PrototypeConfig{RevisionID: a.ID, SourceRepo: source})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	waitInactive(t, e, r.ID)
	// The source moves while the plan is reviewed. Implementation must still
	// start from the commit whose files were included in that plan.
	if err = os.WriteFile(filepath.Join(source, "original.txt"), []byte("CHANGED LATER"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = git(ctx, source, "add", "original.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err = git(ctx, source, "commit", "-m", "later user change"); err != nil {
		t.Fatal(err)
	}
	initialHead, _ = git(ctx, source, "rev-parse", "HEAD")
	r, err = e.ApprovePrototype(r.ID, r.Prototype.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	sourceHead, _ := git(ctx, source, "rev-parse", "HEAD")
	status, _ := git(ctx, source, "status", "--porcelain")
	if sourceHead != initialHead || status != "" {
		t.Fatal("modified original repository")
	}
	if _, err = os.Stat(filepath.Join(source, "game.cjs")); !os.IsNotExist(err) {
		t.Fatal("wrote code in source repo")
	}
	b := r.Prototype.Builds[0]
	var task Task
	for _, v := range r.Tasks {
		if v.ID == b.TaskID {
			task = v
		}
	}
	// Simulate a crash after git commit but before the build was added to SQLite.
	r.Prototype.Builds = nil
	r.Prototype.CurrentCommit = r.Prototype.BaseCommit
	recovered, err := applyProposal(ctx, r, task, fixtureCode(2))
	if err != nil || recovered.Commit != b.Commit {
		t.Fatalf("commit recovery: %v", err)
	}
	count, _ := git(ctx, r.Prototype.Workspace, "rev-list", "--count", "HEAD")
	if count != "2" {
		t.Fatal("replayed commit", count)
	}
}
func TestPrototypeCommandCancellationAndRetry(t *testing.T) {
	s := testStore(t)
	project := testProject(t, s)
	a := fixtureApproval(t, s, project)
	plan := fixturePlan(t)
	node, _ := exec.LookPath("node")
	plan.Checks[0].Args = []string{node, "-e", `const fs=require('node:fs');if(!fs.existsSync('.tmp')){fs.writeFileSync('.tmp','started');setTimeout(()=>{},30000)}else{console.log('RESUMED CHECK')}`}
	plan.Checks[0].TimeoutSeconds = 60
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		answer := "Plan"
		switch in.Stage {
		case "prototype_plan_final":
			answer = encoded(plan)
		case "prototype_code":
			answer = encoded(fixtureCode(2))
		case "prototype_review":
			answer = encoded(CodeReview{Ready: true, Summary: "Checked"})
		}
		return AgentResult{Text: answer}, nil
	}))
	defer e.Close()
	r, err := e.CreatePrototype(project.ID, PrototypeConfig{RevisionID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	waitInactive(t, e, r.ID)
	r, err = e.ApprovePrototype(r.ID, r.Prototype.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		if _, err = os.Stat(filepath.Join(r.Prototype.Workspace, ".tmp")); err == nil {
			started = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !started {
		t.Fatal("check never started")
	}
	if _, err = e.Pause(r.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "paused")
	waitInactive(t, e, r.ID)
	if r.Prototype.Builds[0].Checks[0].Status != "interrupted" {
		t.Fatal("cancelled check looked successful")
	}
	commit := r.Prototype.Builds[0].Commit
	r, err = e.Resume(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	b := r.Prototype.Builds[0]
	if len(r.Prototype.Builds) != 1 || b.Commit != commit || len(b.Checks) != 2 || b.Checks[1].Status != "passed" || !strings.Contains(b.Checks[1].Output, "RESUMED CHECK") {
		t.Fatalf("bad resumed build: %+v", b)
	}
}

func TestPrototypeInsideAnotherRepositoryAndPreviewIsolation(t *testing.T) {
	s := testStore(t)
	outer := t.TempDir()
	ctx := context.Background()
	if _, err := git(ctx, outer, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, outer, "commit", "--allow-empty", "-m", "outer"); err != nil {
		t.Fatal(err)
	}
	outerHead, _ := git(ctx, outer, "rev-parse", "HEAD")
	outerBranch, _ := git(ctx, outer, "branch", "--show-current")
	r := Run{ID: ID(), Prototype: &Prototype{Workspace: filepath.Join(outer, "tandem-owned", "workspace"), Plan: &PrototypePlan{Platform: "browser"}}}
	if err := prepareWorkspace(ctx, s, r); err != nil {
		t.Fatal(err)
	}
	top, _ := git(ctx, r.Prototype.Workspace, "rev-parse", "--show-toplevel")
	top, _ = filepath.EvalSymlinks(top)
	want, _ := filepath.EvalSymlinks(r.Prototype.Workspace)
	if top != want {
		t.Fatal("inherited parent repository")
	}
	head, _ := git(ctx, outer, "rev-parse", "HEAD")
	branch, _ := git(ctx, outer, "branch", "--show-current")
	if head != outerHead || branch != outerBranch {
		t.Fatal("changed outer repository")
	}
	if err := prepareWorkspace(ctx, s, r); err != nil {
		t.Fatal("setup is not resumable", err)
	}
	if err := os.WriteFile(filepath.Join(r.Prototype.Workspace, "index.html"), []byte("<h1>Frozen build</h1>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Prototype.Workspace, ".env"), []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	buildID := ID()
	if err := s.capturePreview(r, buildID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Prototype.Workspace, "index.html"), []byte("Later build"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := previewHandler(s.previewPath(r.ID, buildID))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:4319/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Frozen build") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || strings.Contains(w.Header().Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatal("preview isn't an isolated snapshot")
	}
	for _, url := range []string{"http://127.0.0.1:4319/.env", "http://127.0.0.1:4319/../tandem.sqlite", "http://127.0.0.1:4319/api/bootstrap", "http://evil.test/"} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code == 200 {
			t.Fatal("preview exposed private path", url)
		}
	}
}

func TestPrototypeActiveTimeLimitCanBeExtended(t *testing.T) {
	s := testStore(t)
	project := testProject(t, s)
	a := fixtureApproval(t, s, project)
	plan := fixturePlan(t)
	r := Run{ID: ID(), ProjectID: project.ID, Kind: "prototype", Mode: "live", Status: "paused", Quality: "pending", CreatedAt: now(), Sessions: map[string]string{}, Tasks: []Task{prototypeTask("codex", "prototype_plan", 0), prototypeTask("claude", "prototype_plan_review", 1), prototypeTask("codex", "prototype_plan_final", 2)}, Prototype: &Prototype{Source: a, Config: PrototypeConfig{Author: "codex", Platform: "browser", MaxMinutes: 5}, UsedSeconds: 299.97}}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, runnerFunc(func(ctx context.Context, _ AgentInput, _ func(string, string)) (AgentResult, error) {
		<-ctx.Done()
		return AgentResult{}, ctx.Err()
	}))
	defer e.Close()
	var err error
	r, err = e.Resume(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	if r.Quality != "limit" || r.Prototype.UsedSeconds < 300 || r.Tasks[0].Status != "pending" {
		t.Fatalf("invalid deadline state: %+v", r.Prototype)
	}
	e.Runner = runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		text := "Plan review"
		if in.Stage == "prototype_plan_final" {
			text = encoded(plan)
		}
		return AgentResult{Text: text}, nil
	})
	r, err = e.PrototypeAction(r.ID, "extend-time", "", "")
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "awaiting_plan")
	if r.Prototype.UsedSeconds >= 300 || r.Prototype.Plan == nil {
		t.Fatal("extension did not resume saved plan work")
	}
}
