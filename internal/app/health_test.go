package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func boolRef(v bool) *bool         { return &v }
func numberRef(v float64) *float64 { return &v }
func TestQuotaSourcesAndProtocol(t *testing.T) {
	t.Run("codex read only handshake", func(t *testing.T) {
		input := strings.NewReader("{\"id\":1,\"result\":{}}\n{\"method\":\"notice\"}\n{\"id\":2,\"result\":{\"account\":{\"type\":\"chatgpt\",\"email\":\"private@example.test\"}}}\n" + `{"id":3,"result":{"ordinaryUsageAllowed":false,"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1800000000},"secondary":{"usedPercent":33,"windowDurationMins":10080,"resetsAt":1800500000},"rateLimitReachedType":"primary"},"other":{"primary":{"usedPercent":4,"windowDurationMins":60,"resetsAt":1800000000}}}}}` + "\n")
		var sent bytes.Buffer
		key, q, err := codexQuotaRPC(&sent, input)
		if err != nil || key == "" || strings.Contains(key, "private") || q.Allowed == nil || *q.Allowed || len(q.Windows) != 3 {
			t.Fatalf("%s %+v %v", key, q, err)
		}
		if q.Windows[0].State != "blocked" || q.Windows[2].Scope != "other" {
			t.Fatal(q.Windows)
		}
		for _, forbidden := range []string{"turn/start", "thread/start", "login/start", "consume"} {
			if strings.Contains(sent.String(), forbidden) {
				t.Fatal("monitor spent quota or changed auth")
			}
		}
		for _, method := range []string{"initialize", "initialized", "account/read", "account/rateLimits/read"} {
			if !strings.Contains(sent.String(), method) {
				t.Fatal("missing handshake step", method)
			}
		}
	})
	t.Run("missing values are unknown", func(t *testing.T) {
		q, err := parseCodexQuota([]byte(`{"rateLimits":{"primary":{"windowDurationMins":300}},"ordinaryUsageAllowed":null}`))
		if err != nil || q.Allowed != nil || q.Windows[0].UsedPercent != nil {
			t.Fatal(q, err)
		}
		if _, err = parseCodexQuota([]byte(`{}`)); err == nil {
			t.Fatal("invented empty quota")
		}
	})
	t.Run("claude warning and scoped rejection", func(t *testing.T) {
		q := parseClaudeQuota([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"five_hour","resetsAt":1800000000,"utilization":0.96,"unifiedWindows":{"seven_day":{"utilization":0.7,"resetsAt":1800500000}}}}`))
		if q == nil || len(q.Windows) != 2 || *q.Windows[0].UsedPercent != 96 || q.Windows[0].State != "warning" {
			t.Fatal(q)
		}
		q = parseClaudeQuota([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day_opus","resetsAt":1800000000}}`))
		if q == nil || q.Scope != "opus" || q.Windows[0].UsedPercent != nil || *q.Allowed {
			t.Fatal(q)
		}
		if parseClaudeQuota([]byte(`{"type":"assistant","rate_limit_info":{"status":"rejected"}}`)) != nil {
			t.Fatal("classified assistant text as telemetry")
		}
		var state streamState
		state.parse("claude", []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`), func(string, string) {})
		state.parse("claude", []byte(`{"type":"assistant","error":"rate_limit"}`), func(string, string) {})
		if state.unavailable == nil || state.unavailable.Kind != "limit" {
			t.Fatal("quota rejection treated as transient throttle")
		}
		state.parse("claude", []byte(`{"type":"result","is_error":false,"result":"saved"}`), func(string, string) {})
		state.parse("claude", []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`), func(string, string) {})
		if state.unavailable != nil {
			t.Fatal("lost successful result due to post-response quota update")
		}
		emitted := false
		state.parse("claude", []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning"}}`), func(kind, stringValue string) { emitted = kind == "quota" })
		if !emitted {
			t.Fatal("dropped native event")
		}
	})
}
func TestHealthSharedPersistenceAndFreshness(t *testing.T) {
	s := testStore(t)
	h := NewHealthMonitor(s)
	h.Observe("claude", QuotaReading{Allowed: boolRef(false), Scope: "opus", Windows: []QuotaWindow{{ID: "seven_day_opus", Scope: "opus", State: "blocked", ResetsAt: time.Now().Add(-time.Hour).Unix()}}})
	if h.Blocking("claude", "sonnet") != nil || h.Blocking("claude", "opus") == nil {
		t.Fatal("model-scoped limit or timer mistaken for recovery")
	}
	h.Observe("claude", QuotaReading{Allowed: boolRef(true), Windows: []QuotaWindow{{ID: "five_hour", State: "allowed"}}})
	if h.Blocking("claude", "opus") == nil {
		t.Fatal("short window erased weekly limit")
	}
	h = NewHealthMonitor(s)
	if h.Blocking("claude", "opus") == nil {
		t.Fatal("lost quota after restart")
	}
	h.Clear("claude")
	if h.Blocking("claude", "opus") != nil {
		t.Fatal("explicit retry not allowed")
	}
	h.Observe("codex", QuotaReading{Allowed: boolRef(false), Source: "fixture", Windows: []QuotaWindow{{ID: "codex/0", Label: "5 часов", State: "blocked", UsedPercent: numberRef(100)}}})
	h.probe = func(_ context.Context, agent string) (AgentStatus, *QuotaReading, error) {
		return AgentStatus{Agent: agent, Installed: true, Subscription: true}, nil, errors.New("usage endpoint HTTP 429")
	}
	h.Refresh(context.Background(), false)
	status := h.Statuses()[0]
	if status.Health.CheckError == "" || status.Health.Quota == nil || status.Health.Quota.Windows[0].UsedPercent == nil || h.Blocking("codex", "") == nil {
		t.Fatal("failed monitor erased last-known quota")
	}
	// The monitoring endpoint failing alone never creates an exhausted-model state.
	fresh := NewHealthMonitor(testStore(t))
	fresh.probe = h.probe
	fresh.Refresh(context.Background(), false)
	if fresh.Blocking("codex", "") != nil {
		t.Fatal("monitor failure disabled inference")
	}
}
func TestHealthAccountChangeAndRefreshCoalescing(t *testing.T) {
	h := NewHealthMonitor(testStore(t))
	var count atomic.Int32
	h.probe = func(_ context.Context, agent string) (AgentStatus, *QuotaReading, error) {
		count.Add(1)
		return AgentStatus{Agent: agent, Installed: true, Subscription: true, AccountKey: "first"}, nil, nil
	}
	h.Refresh(context.Background(), false)
	h.Failure("claude", AgentIssue{Kind: "limit", Since: now()})
	for i := 0; i < 5; i++ {
		h.Refresh(context.Background(), true)
	}
	if count.Load() != 2 {
		t.Fatal("refresh storm", count.Load())
	}
	h.next = time.Time{}
	h.probe = func(_ context.Context, agent string) (AgentStatus, *QuotaReading, error) {
		return AgentStatus{Agent: agent, Installed: true, Subscription: true, AccountKey: "second"}, nil, nil
	}
	h.Refresh(context.Background(), false)
	if h.Blocking("claude", "") != nil {
		t.Fatal("quota leaked between accounts")
	}
	h.ObserveAccount("claude", "first", QuotaReading{Allowed: boolRef(false)})
	if h.Blocking("claude", "") != nil {
		t.Fatal("old account stream contaminated new account")
	}
	encoded, _ := json.Marshal(h.Statuses())
	if strings.Contains(string(encoded), "second") || strings.Contains(string(encoded), "first") {
		t.Fatal("exposed private account identity")
	}
}
func TestHealthIgnoresSlowPollAfterNewerEvent(t *testing.T) {
	h := NewHealthMonitor(testStore(t))
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	h.probe = func(_ context.Context, agent string) (AgentStatus, *QuotaReading, error) {
		if agent == "codex" {
			close(started)
			<-release
			return AgentStatus{Agent: agent, Subscription: true}, &QuotaReading{CheckedAt: time.Now().Add(-time.Minute).Format(time.RFC3339Nano), Allowed: boolRef(true)}, nil
		}
		return AgentStatus{Agent: agent, Subscription: true}, nil, nil
	}
	go func() { h.Refresh(context.Background(), false); close(done) }()
	<-started
	h.Observe("codex", QuotaReading{Allowed: boolRef(false)})
	close(release)
	<-done
	if h.Blocking("codex", "") == nil {
		t.Fatal("stale poll erased a new limit")
	}
}
func TestHealthPreventsKnownQuotaAcrossRuns(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	h := NewHealthMonitor(s)
	h.Failure("claude", AgentIssue{Kind: "limit", Detail: "Quota", Since: now()})
	e := NewEngine(s, runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		if in.Agent == "claude" {
			t.Error("known blocked account invoked")
		}
		return AgentResult{Text: "saved"}, nil
	}))
	e.Health = h
	defer e.Close()
	for i := 0; i < 2; i++ {
		r := singleRecoveryRun(t, s, p.ID, "solo")
		r.Tasks[0].Agent = "claude"
		_ = s.PutRun(r)
		if _, err := e.Resume(r.ID); err != nil {
			t.Fatal(err)
		}
		r = awaitRun(t, s, r.ID, "completed")
		waitInactive(t, e, r.ID)
		if r.Tasks[0].Agent != "codex" || r.Tasks[0].RequestedAgent != "claude" {
			t.Fatal("lost handoff provenance")
		}
	}
}

// Opt-in read-only smoke test: never creates a model turn or reads auth files.
func TestInstalledCodexQuota(t *testing.T) {
	if os.Getenv("TANDEM_LIVE_QUOTA_CHECK") != "1" {
		t.Skip("opt-in installed CLI check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_, q, err := readCodexQuota(ctx)
	if err != nil {
		t.Fatalf("installed CLI quota check: %v", err)
	}
	t.Logf("Quota protocol succeeded: %d windows; ordinary usage permission supplied: %t", len(q.Windows), q.Allowed != nil)
}
