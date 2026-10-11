package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Quota readings are evidence, not predictions of how many turns are left.
type QuotaWindow struct {
	ID          string   `json:"id"`
	CheckedAt   string   `json:"checkedAt,omitempty"`
	Label       string   `json:"label"`
	Scope       string   `json:"scope,omitempty"`
	UsedPercent *float64 `json:"usedPercent,omitempty"`
	ResetsAt    int64    `json:"resetsAt,omitempty"`
	State       string   `json:"state"`
}
type QuotaReading struct {
	Source    string        `json:"source"`
	CheckedAt string        `json:"checkedAt"`
	Windows   []QuotaWindow `json:"windows"`
	Allowed   *bool         `json:"allowed,omitempty"`
	Scope     string        `json:"scope,omitempty"`
}
type AgentHealth struct {
	AccountKey    string        `json:"-"`
	QuotaMode     string        `json:"quotaMode"`
	CheckedAt     string        `json:"checkedAt,omitempty"`
	CheckError    string        `json:"checkError,omitempty"`
	Quota         *QuotaReading `json:"quota,omitempty"`
	Issue         *AgentIssue   `json:"issue,omitempty"`
	LastSuccessAt string        `json:"lastSuccessAt,omitempty"`
	Notice        string        `json:"notice,omitempty"`
}
type savedHealth struct {
	Status     AgentStatus `json:"status"`
	Health     AgentHealth `json:"health"`
	AccountKey string      `json:"accountKey"`
}
type HealthMonitor struct {
	store    *Store
	mu       sync.Mutex
	refresh  chan struct{}
	records  map[string]savedHealth
	probe    func(context.Context, string) (AgentStatus, *QuotaReading, error)
	next     time.Time
	failures int
}

func NewHealthMonitor(s *Store) *HealthMonitor {
	h := &HealthMonitor{store: s, refresh: make(chan struct{}, 1), records: map[string]savedHealth{}, probe: probeAgent}
	rows, err := s.db.Query("SELECT body FROM agent_health")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var b string
			var v savedHealth
			if rows.Scan(&b) == nil && json.Unmarshal([]byte(b), &v) == nil {
				v.Status.Health = nil
				h.records[v.Status.Agent] = v
			}
		}
	}
	return h
}
func (h *HealthMonitor) Start(ctx context.Context) {
	h.Refresh(ctx, false)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.Refresh(ctx, false)
		}
	}
}
func (h *HealthMonitor) Statuses() []AgentStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []AgentStatus{}
	for _, agent := range []string{"codex", "claude"} {
		v, ok := h.records[agent]
		if !ok {
			v.Status = AgentStatus{Agent: agent, Detail: "Проверяем подключение", LoginCommand: agent + " login"}
			v.Health.QuotaMode = quotaMode(agent)
		}
		// Deep copy: callers must never mutate shared persisted observations.
		b, _ := json.Marshal(v.Health)
		var copy AgentHealth
		_ = json.Unmarshal(b, &copy)
		v.Status.Health = &copy
		out = append(out, v.Status)
	}
	return out
}
func quotaMode(agent string) string {
	if agent == "codex" {
		return "poll"
	}
	return "events"
}
func (h *HealthMonitor) Refresh(ctx context.Context, force bool) {
	select {
	case h.refresh <- struct{}{}:
		defer func() { <-h.refresh }()
	case <-ctx.Done():
		return
	}
	h.mu.Lock()
	due := time.Now().After(h.next)
	h.mu.Unlock()
	if !due && !force {
		return
	}
	// Even the refresh button shares one poller and a minimum cooldown.
	h.mu.Lock()
	last := h.next
	h.mu.Unlock()
	if force && !due && time.Until(last) > 50*time.Second {
		return
	}
	type result struct {
		agent   string
		status  AgentStatus
		quota   *QuotaReading
		err     error
		started time.Time
	}
	ch := make(chan result, 2)
	for _, agent := range []string{"codex", "claude"} {
		go func(agent string) {
			at := time.Now()
			s, q, err := h.probe(ctx, agent)
			ch <- result{agent, s, q, err, at}
		}(agent)
	}
	failed := false
	for i := 0; i < 2; i++ {
		r := <-ch
		if ctx.Err() != nil {
			continue
		}
		failed = failed || r.err != nil
		h.mu.Lock()
		old := h.records[r.agent]
		v := old
		if r.status.AccountKey != "" && r.status.AccountKey != old.AccountKey {
			v = savedHealth{}
		}
		v.Status = r.status
		v.Status.Health = nil
		if r.status.AccountKey != "" {
			v.AccountKey = r.status.AccountKey
		}
		v.Health.QuotaMode = quotaMode(r.agent)
		v.Health.CheckedAt = now()
		v.Health.CheckError = ""
		if r.err != nil {
			v.Health.CheckError = "Не удалось обновить данные лимита. Последнее измерение может быть устаревшим."
		}
		if !r.status.Subscription {
			v.Health.Quota = nil
			v.Health.Issue = nil
		}
		if r.quota != nil && (v.Health.Quota == nil || !after(v.Health.Quota.CheckedAt, r.started)) {
			v.Health.Quota = r.quota
			// A timer or a percentage cannot prove that ordinary usage is permitted.
			if r.quota.Allowed != nil && *r.quota.Allowed {
				if v.Health.Issue != nil && v.Health.Issue.Kind != "manual" {
					v.Health.Issue = nil
				}
			}
		}
		if r.status.Subscription && v.Health.Issue != nil && (v.Health.Issue.Kind == "auth" || v.Health.Issue.Kind == "missing_cli") {
			v.Health.Issue = nil
		}
		h.saveLocked(r.agent, v)
		h.mu.Unlock()
	}
	h.mu.Lock()
	if failed {
		h.failures++
	} else {
		h.failures = 0
	}
	delay := time.Minute * time.Duration(1<<min(h.failures, 3))
	h.next = time.Now().Add(delay)
	h.mu.Unlock()
}
func after(s string, t time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && at.After(t)
}
func (h *HealthMonitor) saveLocked(agent string, v savedHealth) {
	old := h.records[agent]
	v.Health.Notice = healthNotice(v.Health)
	b, err := json.Marshal(v)
	if err == nil {
		_, err = h.store.db.Exec("INSERT INTO agent_health(agent,body) VALUES(?,?) ON CONFLICT(agent) DO UPDATE SET body=excluded.body", agent, string(b))
	}
	if err != nil {
		v.Health.CheckError = "Не удалось сохранить состояние мониторинга"
	}
	h.records[agent] = v
	if v.Health.Notice != "" && v.Health.Notice != old.Health.Notice {
		runs, _ := h.store.Runs("")
		for _, r := range runs {
			if r.Mode == "live" && (r.Status == "running" || r.Status == "paused" || r.Status == "failed") {
				_ = h.store.Event(r.ID, agent, "health", v.Health.Notice)
				_ = h.store.AddMessage(Message{ProjectID: r.ProjectID, RunID: r.ID, Actor: "system", Target: "all", Stage: "health", Text: agent + ": " + v.Health.Notice})
			}
		}
	}
}
func healthNotice(h AgentHealth) string {
	if h.Issue != nil {
		return h.Issue.Detail
	}
	if h.Quota == nil {
		return ""
	}
	if h.Quota.Allowed != nil && !*h.Quota.Allowed {
		return "Сервис сообщил об ограничении использования подписки"
	}
	level := 0
	for _, w := range h.Quota.Windows {
		if w.State == "blocked" {
			return "Достигнут лимит: " + w.Label
		}
		if w.State == "warning" {
			level = max(level, 1)
		}
		if w.UsedPercent != nil {
			if *w.UsedPercent >= 95 {
				level = max(level, 2)
			} else if *w.UsedPercent >= 80 {
				level = max(level, 1)
			}
		}
	}
	if level == 2 {
		return "Осталось не более 5% одного из лимитов. Работа может прерваться."
	}
	if level == 1 {
		return "Лимит близок к исчерпанию. При остановке результат будет сохранён."
	}
	return ""
}
func (h *HealthMonitor) Identity(agent string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records[agent].AccountKey
}
func (h *HealthMonitor) ObserveAccount(agent, key string, q QuotaReading) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if key != "" && h.records[agent].AccountKey != key {
		return
	}
	h.observeLocked(agent, q)
}
func (h *HealthMonitor) Observe(agent string, q QuotaReading) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observeLocked(agent, q)
}
func (h *HealthMonitor) observeLocked(agent string, q QuotaReading) {
	v := h.records[agent]
	v.Status.Agent = agent
	v.Health.QuotaMode = quotaMode(agent)
	for i := range q.Windows {
		if q.Windows[i].CheckedAt == "" {
			q.Windows[i].CheckedAt = now()
		}
	}
	// Event reports may cover just one window. Preserve other known windows.
	if v.Health.Quota != nil {
		seen := map[string]bool{}
		for _, w := range q.Windows {
			seen[w.ID] = true
		}
		for _, w := range v.Health.Quota.Windows {
			if !seen[w.ID] {
				q.Windows = append(q.Windows, w)
			}
		}
	}
	q.CheckedAt = now()
	v.Health.Quota = &q
	if q.Allowed != nil && *q.Allowed && v.Health.Issue != nil && (q.Scope == "" || v.Health.Issue.Scope == q.Scope) {
		v.Health.Issue = nil
	}
	h.saveLocked(agent, v)
}
func (h *HealthMonitor) Failure(agent string, issue AgentIssue) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.records[agent]
	v.Status.Agent = agent
	v.Health.QuotaMode = quotaMode(agent)
	if issue.Kind == "limit" && v.Health.Quota != nil {
		issue.ResetAt = quotaReset(v.Health.Quota)
		issue.Scope = v.Health.Quota.Scope
	}
	v.Health.Issue = &issue
	h.saveLocked(agent, v)
}
func (h *HealthMonitor) Success(agent string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.records[agent]
	v.Status.Agent = agent
	v.Health.LastSuccessAt = now()
	if v.Health.Issue != nil && (v.Health.Issue.Kind == "network" || v.Health.Issue.Kind == "throttle") {
		v.Health.Issue = nil
	}
	h.saveLocked(agent, v)
}
func (h *HealthMonitor) Clear(agent string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.records[agent]
	v.Health.Issue = nil
	if v.Health.Quota != nil {
		v.Health.Quota.Allowed = nil
		for i := range v.Health.Quota.Windows {
			if v.Health.Quota.Windows[i].State == "blocked" {
				v.Health.Quota.Windows[i].State = "unknown"
			}
		}
	}
	h.saveLocked(agent, v)
}
func (h *HealthMonitor) Blocking(agent, model string) *AgentIssue {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.records[agent]
	if i := v.Health.Issue; i != nil && (i.Kind == "limit" || i.Kind == "auth" || i.Kind == "missing_cli") && scopeMatches(i.Scope, model) {
		copy := *i
		return &copy
	}
	q := v.Health.Quota
	if q == nil {
		return nil
	}
	if q.Allowed != nil && !*q.Allowed && scopeMatches(q.Scope, model) {
		return &AgentIssue{Kind: "limit", Detail: "Сервис сообщил об ограничении использования подписки", Since: q.CheckedAt, Scope: q.Scope, ResetAt: quotaReset(q)}
	}
	for _, w := range q.Windows {
		if w.State == "blocked" && scopeMatches(w.Scope, model) {
			return &AgentIssue{Kind: "limit", Detail: "Достигнут лимит: " + w.Label, Since: q.CheckedAt, Scope: w.Scope, ResetAt: w.ResetsAt}
		}
	}
	return nil
}
func scopeMatches(scope, model string) bool {
	return scope == "" || strings.Contains(strings.ToLower(model), scope)
}
func quotaReset(q *QuotaReading) int64 {
	var reset int64
	for _, w := range q.Windows {
		if w.State == "blocked" {
			reset = max(reset, w.ResetsAt)
		}
	}
	return reset
}
func quotaLabel(id string, minutes int) string {
	switch id {
	case "five_hour":
		return "5 часов"
	case "seven_day":
		return "7 дней"
	case "seven_day_opus":
		return "7 дней · Opus"
	case "seven_day_sonnet":
		return "7 дней · Sonnet"
	case "overage":
		return "Дополнительное использование"
	}
	if minutes > 0 {
		if minutes%1440 == 0 {
			return fmt.Sprintf("%d дней", minutes/1440)
		}
		if minutes%60 == 0 {
			return fmt.Sprintf("%d часов", minutes/60)
		}
		return fmt.Sprintf("%d минут", minutes)
	}
	return id
}
func parseClaudeQuota(raw []byte) *QuotaReading {
	var envelope struct {
		Type string `json:"type"`
		Info struct {
			Status      string   `json:"status"`
			Type        string   `json:"rateLimitType"`
			ResetsAt    int64    `json:"resetsAt"`
			Utilization *float64 `json:"utilization"`
			Unified     map[string]struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    int64    `json:"resetsAt"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Type != "rate_limit_event" {
		return nil
	}
	i := envelope.Info
	state := map[string]string{"allowed": "allowed", "allowed_warning": "warning", "rejected": "blocked"}[i.Status]
	if state == "" {
		return nil
	}
	scope := ""
	if strings.Contains(i.Type, "opus") {
		scope = "opus"
	}
	if strings.Contains(i.Type, "sonnet") {
		scope = "sonnet"
	}
	if i.Type == "overage" || strings.Contains(i.Type, "overage") {
		scope = "overage"
	}
	var percent *float64
	if i.Utilization != nil && *i.Utilization >= 0 {
		v := *i.Utilization * 100
		percent = &v
	}
	allowed := i.Status != "rejected"
	id := i.Type
	if id == "" {
		id = "subscription"
	}
	q := &QuotaReading{Source: "Claude Code · событие CLI", CheckedAt: now(), Allowed: &allowed, Scope: scope, Windows: []QuotaWindow{{ID: id, Label: quotaLabel(id, 0), Scope: scope, UsedPercent: percent, ResetsAt: i.ResetsAt, State: state}}}
	for _, key := range []string{"five_hour", "seven_day"} {
		if w, ok := i.Unified[key]; ok && key != id {
			var percent *float64
			if w.Utilization != nil && *w.Utilization >= 0 {
				p := *w.Utilization * 100
				percent = &p
			}
			s := "allowed"
			if percent != nil && *percent >= 80 {
				s = "warning"
			}
			q.Windows = append(q.Windows, QuotaWindow{ID: key, Label: quotaLabel(key, 0), UsedPercent: percent, ResetsAt: w.ResetsAt, State: s})
		}
	}
	return q
}
func parseCodexQuota(raw []byte) (*QuotaReading, error) {
	type window struct {
		UsedPercent *float64 `json:"usedPercent"`
		Minutes     int      `json:"windowDurationMins"`
		ResetsAt    int64    `json:"resetsAt"`
	}
	type bucket struct {
		ID        string  `json:"limitId"`
		Name      string  `json:"limitName"`
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
		Reached   *string `json:"rateLimitReachedType"`
	}
	var v struct {
		Allowed *bool             `json:"ordinaryUsageAllowed"`
		Single  bucket            `json:"rateLimits"`
		Buckets map[string]bucket `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if len(v.Buckets) == 0 {
		v.Buckets = map[string]bucket{"codex": v.Single}
	}
	q := &QuotaReading{Source: "Codex · данные подписки", CheckedAt: now(), Allowed: v.Allowed, Windows: []QuotaWindow{}}
	keys := []string{}
	for id := range v.Buckets {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		b := v.Buckets[id]
		scope := ""
		if id != "codex" {
			scope = id
		}
		for j, w := range []*window{b.Primary, b.Secondary} {
			if w == nil {
				continue
			}
			state := "allowed"
			if w.UsedPercent != nil && *w.UsedPercent >= 80 {
				state = "warning"
			}
			if b.Reached != nil && *b.Reached != "" && w.UsedPercent != nil && *w.UsedPercent >= 100 {
				state = "blocked"
			}
			label := quotaLabel("", w.Minutes)
			if b.Name != "" {
				label = b.Name + " · " + label
			}
			q.Windows = append(q.Windows, QuotaWindow{ID: fmt.Sprintf("%s/%d", id, j), Label: label, Scope: scope, UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt, State: state})
		}
	}
	if len(q.Windows) == 0 && q.Allowed == nil {
		return nil, fmt.Errorf("quota data unavailable")
	}
	for i := range q.Windows {
		q.Windows[i].CheckedAt = q.CheckedAt
	}
	return q, nil
}
