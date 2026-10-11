package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type WaitState struct {
	Agent   string `json:"agent"`
	Reason  string `json:"reason"`
	Attempt int    `json:"attempt"`
	Since   string `json:"since"`
	RetryAt string `json:"retryAt"`
	Detail  string `json:"detail"`
}
type RecoveryState struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
	Detail string `json:"detail"`
}
type connectionWaitError struct{ detail string }

func (e *connectionWaitError) Error() string { return e.detail }
func (e *Engine) reconcileHealth(r Run) Run {
	if e.Health == nil || r.Mode != "live" {
		return r
	}
	if r.Unavailable == nil {
		r.Unavailable = map[string]AgentIssue{}
	}
	for _, agent := range []string{"codex", "claude"} {
		if old, ok := r.Unavailable[agent]; ok && old.Kind == "manual" {
			continue
		}
		if issue := e.Health.Blocking(agent, runModel(r, agent)); issue != nil {
			r.Unavailable[agent] = *issue
		} else if old, ok := r.Unavailable[agent]; ok && (old.Kind == "limit" || old.Kind == "auth" || old.Kind == "missing_cli") {
			// Only a newer affirmative reading may clear a previously observed limit.
			for _, s := range e.Health.Statuses() {
				if s.Agent == agent && (old.Kind == "auth" || old.Kind == "missing_cli") && s.Subscription {
					at, _ := time.Parse(time.RFC3339Nano, old.Since)
					if after(s.Health.CheckedAt, at) {
						delete(r.Unavailable, agent)
					}
				}
				if s.Agent == agent && s.Health.Quota != nil {
					q := s.Health.Quota
					at, _ := time.Parse(time.RFC3339Nano, old.Since)
					if q.Allowed != nil && *q.Allowed && after(q.CheckedAt, at) && scopeMatches(q.Scope, runModel(r, agent)) {
						delete(r.Unavailable, agent)
					}
				}
			}
		}
	}
	return r
}
func transient(kind string) bool { return kind == "network" || kind == "throttle" }
func (e *Engine) clearTransient(id string) error {
	_, err := e.Store.UpdateRun(id, func(r *Run) {
		for agent, i := range r.Unavailable {
			if transient(i.Kind) {
				delete(r.Unavailable, agent)
				delete(r.Sessions, agent)
			}
		}
	})
	return err
}
func (e *Engine) perform(ctx context.Context, r Run, t Task) error {
	if r.Mode == "demo" {
		return e.performAvailable(ctx, r, t)
	}
	delays := e.RetryDelays
	if delays == nil {
		delays = []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second}
	}
	counts := map[string]int{}
	defer func() { _, _ = e.Store.UpdateRun(r.ID, func(r *Run) { delete(r.Waiting, t.ID) }) }()
	for attempt := 0; ; attempt++ {
		err := e.performAvailable(ctx, r, t)
		if err == nil {
			if attempt > 0 {
				_ = e.Store.Event(r.ID, "system", "reconnected", "Соединение восстановлено. Незавершённый этап выполнен; сохранённые этапы не повторялись.")
			}
			return nil
		}
		var issue *AgentUnavailableError
		if ctx.Err() != nil || !errors.As(err, &issue) || !transient(issue.Kind) {
			return err
		}
		counts[issue.Agent]++
		if e.Health != nil {
			e.Health.Failure(issue.Agent, AgentIssue{Kind: issue.Kind, Detail: issue.Detail, Since: now()})
		}
		_, saveErr := e.Store.UpdateRun(r.ID, func(r *Run) { delete(r.Sessions, issue.Agent) })
		if saveErr != nil {
			return saveErr
		}
		if attempt >= len(delays) {
			return &connectionWaitError{"Соединение не восстановилось после нескольких попыток. Работа сохранена. Нажмите «Продолжить», когда связь появится."}
		}
		// After two transient failures, try the other provider under the chosen policy.
		fresh, loadErr := e.Store.Run(r.ID)
		if loadErr != nil {
			return loadErr
		}
		if counts[issue.Agent] == 2 && fresh.AvailabilityPolicy == "solo" && t.Stage != "independent_review" {
			other := otherAgent(issue.Agent)
			if _, blocked := fresh.Unavailable[other]; !blocked {
				if err = e.markUnavailable(r.ID, issue); err != nil {
					return err
				}
				_ = e.Store.Event(r.ID, "system", "connection", "Повторные ошибки связи с "+issue.Agent+". Проверяем возможность продолжить другим участником.")
				continue
			}
		}
		if err = e.waitForRetry(ctx, r.ID, t.ID, issue, attempt+1, delays[attempt]); err != nil {
			return err
		}
		if err = e.clearTransient(r.ID); err != nil {
			return err
		}
	}
}
func (e *Engine) waitForRetry(ctx context.Context, id, task string, issue *AgentUnavailableError, attempt int, delay time.Duration) error {
	if clock, ok := ctx.Value(budgetKey{}).(*activeClock); ok {
		clock.pause()
		defer clock.resume()
	}
	detail := fmt.Sprintf("%s: %s. Сохранённые этапы остаются на месте. Повтор %d.", issue.Agent, issue.Detail, attempt)
	_, err := e.Store.UpdateRun(id, func(r *Run) {
		if r.Waiting == nil {
			r.Waiting = map[string]WaitState{}
		}
		r.Waiting[task] = WaitState{Agent: issue.Agent, Reason: issue.Kind, Attempt: attempt, Since: now(), RetryAt: time.Now().Add(delay).UTC().Format(time.RFC3339Nano), Detail: detail}
	})
	if err != nil {
		return err
	}
	defer func() { _, _ = e.Store.UpdateRun(id, func(r *Run) { delete(r.Waiting, task) }) }()
	if err = e.Store.Event(id, issue.Agent, "waiting", detail); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func finishRecovery(r *Run, ctx context.Context, failure error) {
	r.Waiting = nil
	var waiting *connectionWaitError
	if errors.As(failure, &waiting) && ctx.Err() == nil {
		r.Status = "paused"
		r.Error = waiting.Error()
		r.Recovery = &RecoveryState{Reason: "connection", At: now(), Detail: waiting.Error()}
	} else if ctx.Err() != nil {
		if r.Recovery == nil {
			r.Recovery = &RecoveryState{Reason: "paused", At: now(), Detail: "Работа остановлена. Незавершённый этап будет повторён после продолжения."}
		}
	} else {
		var issue *AgentUnavailableError
		if errors.As(failure, &issue) {
			r.Recovery = &RecoveryState{Reason: issue.Kind, At: now(), Detail: issue.Detail + ". Завершённые этапы сохранены. Проверьте статус участников перед продолжением."}
		}
	}
}

// A context-local clock excludes retry waits without polling the whole run JSON.
type budgetKey struct{}
type activeClock struct {
	mu     sync.Mutex
	used   float64
	last   time.Time
	paused int
	woke   bool
}

func (c *activeClock) sampleLocked() {
	at := time.Now()
	delta := at.Sub(c.last).Seconds()
	c.last = at
	if delta > 5 {
		c.woke = true
	}
	if c.paused == 0 && delta <= 5 {
		c.used += delta
	}
}
func (c *activeClock) sample() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sampleLocked()
	return c.used
}
func (c *activeClock) pause()  { c.mu.Lock(); defer c.mu.Unlock(); c.sampleLocked(); c.paused++ }
func (c *activeClock) resume() { c.mu.Lock(); defer c.mu.Unlock(); c.sampleLocked(); c.paused-- }
func (e *Engine) prototypeBudget(parent context.Context, id string, maxSeconds, initial float64) (context.Context, func() float64) {
	clock := &activeClock{used: initial, last: time.Now()}
	ctx, cancel := context.WithCancel(context.WithValue(parent, budgetKey{}, clock))
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		saved := time.Now()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				used := clock.sample()
				clock.mu.Lock()
				woke := clock.woke
				clock.woke = false
				clock.mu.Unlock()
				if woke {
					_ = e.Store.Event(id, "system", "wake", "Обнаружен длительный перерыв. Время сна не добавлено к активным минутам; сведения о соединениях могут быть устаревшими.")
				}
				if time.Since(saved) >= 5*time.Second {
					_, err := e.Store.UpdateRun(id, func(r *Run) { r.Prototype.UsedSeconds = used; r.Prototype.ActiveSince = now() })
					saved = time.Now()
					if err != nil {
						cancel()
						return
					}
				}
				if used >= maxSeconds {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() float64 { close(stop); <-done; used := clock.sample(); cancel(); return used }
}
