package app

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

func singleRecoveryRun(t *testing.T, s *Store, project, policy string) Run {
	t.Helper()
	r := Run{ID: ID(), ProjectID: project, Mode: "live", Status: "paused", Quality: "pending", CreatedAt: now(), AvailabilityPolicy: policy, Sessions: map[string]string{}, Tasks: []Task{{ID: ID(), Agent: "codex", Stage: "ideas", Phase: 0, Status: "pending"}}}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestConnectionRetryPreservesCheckpoints(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	r := singleRecoveryRun(t, s, p.ID, "wait")
	r.Tasks = append([]Task{{ID: ID(), Agent: "claude", Stage: "ideas", Phase: 0, Status: "done", Output: "already saved"}}, r.Tasks...)
	_ = s.PutRun(r)
	var calls atomic.Int32
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		if in.Agent != "codex" {
			t.Error("replayed completed stage")
		}
		if calls.Add(1) == 1 {
			return AgentResult{Text: "partial"}, &AgentUnavailableError{Agent: in.Agent, Kind: "network", Detail: "Disconnected"}
		}
		if in.Session != "" {
			t.Error("resumed interrupted conversation")
		}
		return AgentResult{Text: "complete"}, nil
	}))
	e.RetryDelays = []time.Duration{time.Millisecond}
	defer e.Close()
	if _, err := e.Resume(r.ID); err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, s, r.ID, "completed")
	waitInactive(t, e, r.ID)
	m, _ := s.Messages(p.ID)
	if calls.Load() != 2 || len(m) != 1 || m[0].Text != "complete" || len(r.Waiting) != 0 {
		t.Fatalf("calls %d messages %+v", calls.Load(), m)
	}
	events, _ := s.Events(r.ID)
	var waiting, reconnected bool
	for _, ev := range events {
		waiting = waiting || ev.Kind == "waiting"
		reconnected = reconnected || ev.Kind == "reconnected"
	}
	if !waiting || !reconnected {
		t.Fatal("missing recovery journal")
	}
}
func TestConnectionStopsAndManualPauseDoesNotResume(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded retries", true: "manual pause"}[manual], func(t *testing.T) {
			s := testStore(t)
			p := testProject(t, s)
			r := singleRecoveryRun(t, s, p.ID, "wait")
			var calls atomic.Int32
			e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
				calls.Add(1)
				return AgentResult{}, &AgentUnavailableError{Agent: in.Agent, Kind: "network", Detail: "Offline"}
			}))
			e.RetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
			if manual {
				e.RetryDelays = []time.Duration{time.Hour}
			}
			defer e.Close()
			_, _ = e.Resume(r.ID)
			if manual {
				deadline := time.Now().Add(2 * time.Second)
				for {
					fresh, _ := s.Run(r.ID)
					if len(fresh.Waiting) > 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("not waiting")
					}
					time.Sleep(time.Millisecond)
				}
				if _, err := e.Pause(r.ID); err != nil {
					t.Fatal(err)
				}
			}
			r = awaitRun(t, s, r.ID, "paused")
			waitInactive(t, e, r.ID)
			if r.Recovery == nil || len(r.Waiting) != 0 || r.Tasks[0].Status != "pending" {
				t.Fatal("missing interrupted state")
			}
			if manual && r.Recovery.Reason != "manual" {
				t.Fatal("manual intent overwritten")
			}
			if !manual && (r.Recovery.Reason != "connection" || calls.Load() != 3) {
				t.Fatal("unbounded retry", calls.Load())
			}
		})
	}
}
func TestConnectionWaitDoesNotSpendPrototypeBudget(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	r := singleRecoveryRun(t, s, p.ID, "wait")
	r.Prototype = &Prototype{}
	_ = s.PutRun(r)
	e := NewEngine(s, nil)
	defer e.Close()
	ctx, finish := e.prototypeBudget(context.Background(), r.ID, 0.1, 0)
	err := e.waitForRetry(ctx, r.ID, r.Tasks[0].ID, &AgentUnavailableError{Agent: "codex", Kind: "network", Detail: "Offline"}, 1, 200*time.Millisecond)
	if err != nil || ctx.Err() != nil {
		t.Fatal("waiting exhausted active budget", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("active budget not enforced after waiting")
	}
	if used := finish(); used < 0.1 || used > 0.22 {
		t.Fatal("incorrect active time", used)
	}
}
func TestRestartDuringWaitingKeepsManualRecovery(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	r := singleRecoveryRun(t, s, p.ID, "wait")
	r.Status = "running"
	r.Tasks[0].Status = "running"
	r.Waiting = map[string]WaitState{"task": {Reason: "network"}}
	r.Prototype = &Prototype{UsedSeconds: 10, ActiveSince: time.Now().Add(-time.Hour).Format(time.RFC3339Nano)}
	_ = s.PutRun(r)
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(r.ID)
	if r.Status != "paused" || r.Recovery == nil || r.Recovery.Reason != "restart" || r.Prototype.UsedSeconds != 10 || r.Prototype.ActiveSince != "" || len(r.Waiting) != 0 {
		t.Fatalf("bad recovery %+v", r)
	}
}
func TestDurableMutationReceipt(t *testing.T) {
	s := testStore(t)
	e := NewEngine(s, nil)
	defer e.Close()
	server := NewServer(s, e, fstest.MapFS{})
	id := ID()
	send := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "http://localhost/api/projects", strings.NewReader(body))
		request.Header.Set("X-Tandem-Token", server.Token)
		request.Header.Set("X-Tandem-Request", id)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, request)
		return w
	}
	first := send(`{"title":"Receipt test"}`)
	if first.Code != 201 {
		t.Fatal(first.Body.String())
	}
	// A restarted HTTP server still replays the persisted receipt.
	server = NewServer(s, e, fstest.MapFS{})
	second := send(`{"title":"Receipt test"}`)
	if second.Code != 201 || second.Body.String() != first.Body.String() || second.Header().Get("X-Tandem-Replayed") != "true" {
		t.Fatal("duplicate or missing response", second)
	}
	projects, _ := s.Projects()
	if len(projects) != 1 {
		t.Fatal("duplicate projects")
	}
	if send(`{"title":"Different"}`).Code != 409 {
		t.Fatal("reused ID with other payload")
	}
	id = ID()
	hash := textHash("POST\n/api/projects\n" + `{"title":"Uncertain"}`)
	_, _ = s.db.Exec("INSERT INTO requests(id,fingerprint,created_at) VALUES(?,?,?)", id, hash, now())
	uncertain := send(`{"title":"Uncertain"}`)
	if uncertain.Code != 409 || uncertain.Header().Get("X-Tandem-Pending") != "true" {
		t.Fatal("blindly replayed uncertain operation")
	}
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if w := send(`{"title":"Uncertain"}`); w.Code != 409 || w.Header().Get("X-Tandem-Uncertain") != "true" {
		t.Fatal("restart did not expose reconciliation path")
	}
}
func TestUploadReceiptIgnoresMultipartBoundary(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	e := NewEngine(s, nil)
	defer e.Close()
	server := NewServer(s, e, fstest.MapFS{})
	id := ID()
	send := func() *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		_ = form.WriteField("role", "reference")
		file, _ := form.CreateFormFile("file", "idea.md")
		_, _ = file.Write([]byte("# Saved material"))
		_ = form.Close()
		request := httptest.NewRequest("POST", "http://localhost/api/projects/"+p.ID+"/materials", &body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		request.Header.Set("X-Tandem-Token", server.Token)
		request.Header.Set("X-Tandem-Request", id)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, request)
		return w
	}
	first, second := send(), send()
	if first.Code != 201 || second.Code != 201 || first.Body.String() != second.Body.String() {
		t.Fatal(first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	materials, _ := s.Materials(p.ID)
	if len(materials) != 1 {
		b, _ := json.Marshal(materials)
		t.Fatal("duplicate upload", string(b))
	}
}
