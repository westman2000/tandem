package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func accessEngine(t *testing.T, runner AgentRunner) *Engine {
	t.Helper()
	e := NewEngine(testStore(t), runner)
	e.Health = NewHealthMonitor(e.Store)
	e.Health.probe = func(_ context.Context, agent string) (AgentStatus, *QuotaReading, error) {
		return AgentStatus{Agent: agent, Installed: true, Authenticated: true, Subscription: true, AccountKey: "account"}, nil, nil
	}
	t.Cleanup(e.Close)
	return e
}

func TestExplicitAccessCheckIsIsolatedAndDoesNotInventQuota(t *testing.T) {
	calls := 0
	e := accessEngine(t, runnerFunc(func(_ context.Context, in AgentInput, emit func(string, string)) (AgentResult, error) {
		calls++
		files, err := os.ReadDir(in.Directory)
		if err != nil || len(files) != 0 || in.Stage != "access_check" || in.Session != "" || in.Agent != "claude" || in.Prompt != "Reply with OK only." || in.Model != "sonnet" {
			t.Fatalf("unexpected access request: %+v, files %v, %v", in, files, err)
		}
		args := strings.Join(commandArgs(in), "|")
		for _, flag := range []string{"--max-turns|1", "--no-session-persistence", "--tools||", "--system-prompt|Reply with OK only.", "--restricted", "--safe-mode"} {
			if !strings.Contains(args, flag) {
				t.Fatal("missing bound", flag, args)
			}
		}
		emit("message", "OK")
		return AgentResult{Text: "OK", Model: "claude-sonnet-test"}, nil
	}))
	if err := e.CheckClaude(context.Background(), "sonnet"); err != nil {
		t.Fatal(err)
	}
	h := e.Health.Statuses()[1].Health
	if h.AccessCheck.State != "available" || h.LastSuccessAt == "" || h.Quota != nil {
		t.Fatalf("invented quota or missed success: %+v", h)
	}
	projects, _ := e.Store.Projects()
	if len(projects) != 0 {
		t.Fatal("check created a project")
	}
	if err := e.CheckClaude(context.Background(), "sonnet"); err == nil || calls != 1 {
		t.Fatal("duplicate check spent quota", err, calls)
	}
	fresh := NewHealthMonitor(e.Store).Statuses()[1].Health
	if fresh.AccessCheck.State != "available" || fresh.AccessCheck.ActualModel != "claude-sonnet-test" {
		t.Fatal("result not persisted", fresh)
	}
}

func TestAccessCheckFailuresAreEvidenceNotQuotaEstimates(t *testing.T) {
	for _, kind := range []string{"limit", "auth", "network", "throttle", "timeout", "interrupted", "error", "different_model"} {
		t.Run(kind, func(t *testing.T) {
			e := accessEngine(t, runnerFunc(func(_ context.Context, _ AgentInput, emit func(string, string)) (AgentResult, error) {
				switch kind {
				case "limit":
					q := parseClaudeQuota([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day","resetsAt":1900000000}}`))
					b, _ := json.Marshal(q)
					emit("quota", string(b))
				case "timeout":
					return AgentResult{}, context.DeadlineExceeded
				case "interrupted":
					return AgentResult{}, context.Canceled
				case "error":
					return AgentResult{}, errors.New("private diagnostic that must not reach the UI")
				case "different_model":
					return AgentResult{Text: "OK", Model: "claude-haiku-test"}, nil
				}
				return AgentResult{}, &AgentUnavailableError{Agent: "claude", Kind: kind, Detail: "fixture failure"}
			}))
			if err := e.CheckClaude(context.Background(), "sonnet"); err != nil {
				t.Fatal(err)
			}
			h := e.Health.Statuses()[1].Health
			if h.AccessCheck.State == "available" || h.LastSuccessAt != "" {
				t.Fatal("false available", h)
			}
			if kind == "limit" {
				if e.Health.Blocking("claude", "sonnet") == nil || h.Quota.Windows[0].UsedPercent != nil || h.Issue.ResetAt != 1900000000 {
					t.Fatal("lost quota refusal", h)
				}
			} else if e.Health.Blocking("claude", "sonnet") != nil && kind != "auth" {
				t.Fatal("non-quota error blocks subscription", h)
			}
			b, _ := json.Marshal(h)
			if strings.Contains(string(b), "private diagnostic") {
				t.Fatal("leaked stderr")
			}
		})
	}
}

func TestAccessCheckRequiresSubscriptionAndSerializesWithWork(t *testing.T) {
	e := accessEngine(t, runnerFunc(func(context.Context, AgentInput, func(string, string)) (AgentResult, error) {
		t.Fatal("must not spend quota")
		return AgentResult{}, nil
	}))
	e.agentSlots["claude"] <- struct{}{}
	if err := e.CheckClaude(context.Background(), ""); err == nil {
		t.Fatal("concurrent request allowed")
	}
	<-e.agentSlots["claude"]
	e.Health.probe = func(context.Context, string) (AgentStatus, *QuotaReading, error) {
		return AgentStatus{Agent: "claude", Installed: true, Authenticated: true}, nil, nil
	}
	if err := e.CheckClaude(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if e.Health.Statuses()[1].Health.AccessCheck.State != "auth" {
		t.Fatal("API-key login accepted")
	}
}

func TestFailedAccessCheckRetainsKnownLimit(t *testing.T) {
	e := accessEngine(t, runnerFunc(func(context.Context, AgentInput, func(string, string)) (AgentResult, error) {
		return AgentResult{}, &AgentUnavailableError{Agent: "claude", Kind: "network", Detail: "Нет связи"}
	}))
	e.Health.Refresh(context.Background(), false)
	e.Health.Failure("claude", AgentIssue{Kind: "limit", Detail: "Лимит исчерпан", Since: now()})
	if err := e.CheckClaude(context.Background(), "sonnet"); err != nil {
		t.Fatal(err)
	}
	h := e.Health.Statuses()[1].Health
	if h.AccessCheck.State != "network" || e.Health.Blocking("claude", "sonnet") == nil {
		t.Fatal("network check erased known quota refusal", h)
	}
}

func TestAccessCheckHTTPReceiptAndCSRF(t *testing.T) {
	calls := 0
	e := accessEngine(t, runnerFunc(func(context.Context, AgentInput, func(string, string)) (AgentResult, error) {
		calls++
		return AgentResult{Text: "OK", Model: "claude-sonnet-test"}, nil
	}))
	srv := NewServer(e.Store, e, nil)
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/agents/claude/check", strings.NewReader(`{"model":"sonnet"}`))
		r.Host = "localhost:4317"
		r.Header.Set("X-Tandem-Token", token)
		r.Header.Set("X-Tandem-Request", "access-check-receipt-1234")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	if w := request(""); w.Code != 403 || calls != 0 {
		t.Fatal("missing CSRF", w.Code, calls)
	}
	first := request(srv.Token)
	second := request(srv.Token)
	if first.Code != 200 || second.Code != 200 || calls != 1 || second.Header().Get("X-Tandem-Replayed") != "true" {
		t.Fatal("request replay spent quota", first.Code, second.Code, calls)
	}
}

func TestAccessSuccessPreservesNewRefusalAndOtherModelLimit(t *testing.T) {
	for _, late := range []bool{false, true} {
		e := accessEngine(t, runnerFunc(func(_ context.Context, _ AgentInput, emit func(string, string)) (AgentResult, error) {
			if late {
				q := parseClaudeQuota([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}`))
				b, _ := json.Marshal(q)
				emit("quota", string(b))
			}
			return AgentResult{Text: "OK", Model: "claude-sonnet-test"}, nil
		}))
		e.Health.Refresh(context.Background(), false)
		e.Health.Observe("claude", QuotaReading{Allowed: boolRef(false), Windows: []QuotaWindow{{ID: "five_hour", State: "blocked"}, {ID: "seven_day_opus", State: "blocked", Scope: "opus"}}})
		e.Health.Failure("claude", AgentIssue{Kind: "limit", Since: now()})
		if err := e.CheckClaude(context.Background(), "sonnet"); err != nil {
			t.Fatal(err)
		}
		if (e.Health.Blocking("claude", "sonnet") != nil) != late {
			t.Fatal("wrong treatment of newer vs older limit", late)
		}
		if e.Health.Blocking("claude", "opus") == nil {
			t.Fatal("erased another model's limit")
		}
	}
}

func TestAccessCheckShutdownAndAccountChange(t *testing.T) {
	started := make(chan struct{})
	e := accessEngine(t, runnerFunc(func(ctx context.Context, _ AgentInput, _ func(string, string)) (AgentResult, error) {
		close(started)
		<-ctx.Done()
		return AgentResult{}, ctx.Err()
	}))
	done := make(chan error, 1)
	go func() { done <- e.CheckClaude(context.Background(), "sonnet") }()
	<-started
	fresh := NewHealthMonitor(e.Store).Statuses()[1].Health
	if fresh.AccessCheck.State != "interrupted" {
		t.Fatal("restart left a perpetual spinner")
	}
	e.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel inference")
	}
	if e.Health.Statuses()[1].Health.AccessCheck.State != "interrupted" {
		t.Fatal("shutdown lost interruption")
	}
	h := e.Health
	h.mu.Lock()
	v := h.records["claude"]
	v.AccountKey = "new account"
	h.saveLocked("claude", v)
	h.mu.Unlock()
	h.finishAccessCheck("account", v.Health.AccessCheck.StartedAt, AgentResult{Text: "OK", Model: "claude-sonnet-test"}, nil)
	if h.Statuses()[1].Health.AccessCheck.State == "available" {
		t.Fatal("attributed old-account result to new login")
	}
}
