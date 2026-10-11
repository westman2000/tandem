package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// An explicit, small inference checks availability, not the amount of quota left.
// It is never scheduled by the passive monitor or repeated automatically.
type AccessCheck struct {
	State       string `json:"state"`
	Model       string `json:"model,omitempty"`
	ActualModel string `json:"actualModel,omitempty"`
	StartedAt   string `json:"startedAt"`
	CheckedAt   string `json:"checkedAt,omitempty"`
	Detail      string `json:"detail"`
}

func (e *Engine) CheckClaude(parent context.Context, model string) error {
	if !validModel(model) || e.Health == nil {
		return errors.New("Проверка доступности недоступна")
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return errors.New("Tandem завершает работу")
	}
	slot := e.agentSlots["claude"]
	select {
	case slot <- struct{}{}:
	default:
		e.mu.Unlock()
		return errors.New("Claude уже выполняет запрос. Дождитесь результата: он тоже обновит состояние агента")
	}
	e.checkCancel = cancel
	e.wg.Add(1)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.checkCancel = nil
		<-slot
		e.mu.Unlock()
		e.wg.Done()
	}()
	h := e.Health
	h.mu.Lock()
	old := h.records["claude"].Health.AccessCheck
	tooSoon := old != nil && after(old.StartedAt, time.Now().Add(-time.Minute))
	h.mu.Unlock()
	if tooSoon {
		return errors.New("Повторная проверка доступна через минуту после предыдущей: каждый запрос расходует лимит подписки")
	}
	// Check the official CLI login again before inference; cached login alone
	// must not permit a request after the user has changed authentication mode.
	status, _, probeErr := h.probe(ctx, "claude")
	if ctx.Err() != nil {
		return errors.New("Не удалось проверить вход Claude за отведённое время")
	}
	if probeErr != nil {
		return errors.New("Не удалось проверить вход Claude")
	}
	started := now()
	h.mu.Lock()
	v := h.records["claude"]
	if status.AccountKey != v.AccountKey {
		v = savedHealth{}
	}
	v.AccountKey, v.Status = status.AccountKey, status
	v.Status.Health = nil
	v.Health.QuotaMode, v.Health.CheckedAt = "events", started
	if !status.Subscription {
		v.Health.Quota, v.Health.Issue = nil, nil
		v.Health.LastSuccessAt = ""
	}
	v.Health.AccessCheck = &AccessCheck{State: "checking", Model: model, StartedAt: started, Detail: "Отправляем короткий запрос Claude…"}
	h.saveLocked("claude", v)
	h.mu.Unlock()
	key := status.AccountKey
	var result AgentResult
	var err error
	if !status.Subscription {
		err = &AgentUnavailableError{Agent: "claude", Kind: "auth", Detail: "Вход по подписке Claude не подтверждён. Подключите аккаунт в настройках"}
	} else {
		var dir string
		dir, err = os.MkdirTemp("", "tandem-access-")
		if err == nil {
			defer os.RemoveAll(dir)
			result, err = e.Runner.Run(ctx, AgentInput{Agent: "claude", Directory: dir, Stage: "access_check", Model: model, Prompt: "Reply with OK only."}, func(kind, data string) {
				if kind == "quota" {
					var q QuotaReading
					if json.Unmarshal([]byte(data), &q) == nil {
						h.ObserveAccount("claude", key, q)
					}
				}
			})
		}
	}
	h.finishAccessCheck(key, started, result, err)
	return nil
}

func (h *HealthMonitor) finishAccessCheck(key, started string, result AgentResult, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.records["claude"]
	c := v.Health.AccessCheck
	if v.AccountKey != key || c == nil || c.StartedAt != started {
		return // Do not attribute a response from the old account to a new login.
	}
	c.CheckedAt, c.ActualModel = now(), result.Model
	c.State, c.Detail = "available", "Ответ получен. Модель была доступна на момент проверки."
	var unavailable *AgentUnavailableError
	switch {
	case errors.As(err, &unavailable):
		c.State, c.Detail = unavailable.Kind, unavailable.Detail
		issue := AgentIssue{Kind: unavailable.Kind, Detail: unavailable.Detail, Since: c.CheckedAt}
		if issue.Kind == "limit" {
			issue.Scope = strings.ToLower(strings.TrimSuffix(c.Model, "[1m]"))
			if q := v.Health.Quota; q != nil && q.Allowed != nil && !*q.Allowed {
				issue.Scope, issue.ResetAt = q.Scope, quotaReset(q)
			}
		}
		// A failed connectivity check does not invalidate a known quota/auth
		// refusal. Its own outcome is still visible in AccessCheck.Detail.
		if prior := v.Health.Issue; prior == nil || (prior.Kind != "limit" && prior.Kind != "auth") || (issue.Kind != "network" && issue.Kind != "throttle") {
			v.Health.Issue = &issue
		}
	case errors.Is(err, context.Canceled):
		c.State, c.Detail = "interrupted", "Проверка прервана. Доступность не подтверждена; автоматически повторять запрос не будем."
	case errors.Is(err, context.DeadlineExceeded):
		c.State, c.Detail = "error", "Claude не завершил проверку за 45 секунд. Доступность не подтверждена; это не означает, что лимит исчерпан."
	case err != nil:
		c.State, c.Detail = "error", "CLI не завершил проверку успешно. Доступность не подтверждена; проверьте подключение и версию Claude Code."
	case result.Model == "" || (c.Model != "" && !scopeMatches(strings.ToLower(strings.TrimSuffix(c.Model, "[1m]")), result.Model)):
		c.State, c.Detail = "error", "CLI ответил другой моделью или не сообщил её. Доступность выбранной модели не подтверждена."
	default:
		v.Health.LastSuccessAt = c.CheckedAt
		at, _ := time.Parse(time.RFC3339Nano, started)
		// A real successful call supersedes old refusals for this model, but
		// does not invalidate another model's limit or a newer quota event.
		if i := v.Health.Issue; i != nil && scopeMatches(i.Scope, result.Model) && !after(i.Since, at) {
			v.Health.Issue = nil
		}
		if q := v.Health.Quota; q != nil {
			if scopeMatches(q.Scope, result.Model) && !after(q.CheckedAt, at) {
				q.Allowed = nil
			}
			for j := range q.Windows {
				w := &q.Windows[j]
				if w.State == "blocked" && scopeMatches(w.Scope, result.Model) && !after(w.CheckedAt, at) {
					w.State = "unknown"
				}
			}
		}
	}
	h.saveLocked("claude", v)
}
