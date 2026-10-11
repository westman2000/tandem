package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type AvailabilityOptions struct {
	Issues           map[string]AgentIssue `json:"-"`
	Policy           string                `json:"policy"`
	UnavailableAgent string                `json:"unavailableAgent,omitempty"`
}
type AgentIssue struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Since  string `json:"since"`
}
type AgentUnavailableError struct{ Agent, Kind, Detail string }

func (e *AgentUnavailableError) Error() string { return e.Agent + ": " + e.Detail }
func issueError(agent string, i AgentIssue) error {
	return &AgentUnavailableError{Agent: agent, Kind: i.Kind, Detail: i.Detail}
}
func validAgent(a string) bool { return a == "codex" || a == "claude" }
func normalizeAvailability(a AvailabilityOptions) (AvailabilityOptions, error) {
	if a.Policy == "" {
		a.Policy = "wait"
	}
	if a.Policy != "wait" && a.Policy != "solo" {
		return a, errors.New("Выберите ожидание участника или продолжение одним агентом")
	}
	if a.UnavailableAgent != "" && !validAgent(a.UnavailableAgent) {
		return a, errors.New("Неизвестный недоступный участник")
	}
	return a, nil
}
func setInitialAvailability(r *Run, a AvailabilityOptions) {
	r.AvailabilityPolicy = a.Policy
	r.Unavailable = map[string]AgentIssue{}
	for k, v := range a.Issues {
		r.Unavailable[k] = v
	}
	if a.UnavailableAgent != "" {
		r.Unavailable[a.UnavailableAgent] = AgentIssue{Kind: "manual", Detail: "Пользователь указал, что агент сейчас недоступен", Since: now()}
	}
}

// Only diagnostics from provider error events / failing processes are classified.
// Normal assistant text, code failures and malformed answers never trigger routing.
func unavailableDiagnostic(agent, text string) *AgentUnavailableError {
	lower := strings.ToLower(text)
	groups := []struct {
		kind, detail string
		patterns     []string
	}{
		{"limit", "Исчерпан лимит или сервис ограничил частоту запросов", []string{"rate_limit", "rate limit", "usage limit", "usage_limit", "quota exceeded", "insufficient_quota", "you've hit your limit", "you have hit your limit", "limit reached", "лимит исчерпан", "лимит использования", "too many requests", "http 429", "status 429"}},
		{"auth", "Требуется восстановить вход в официальный CLI", []string{"authentication_error", "not logged in", "please log in", "please login", "login required", "token expired", "token has expired", "invalid authentication", "unauthorized", "http 401", "status 401"}},
		{"network", "Сервис временно недоступен или нет соединения", []string{"overloaded_error", "service unavailable", "connection refused", "connection reset", "network error", "network_error", "econnreset", "econnrefused", "etimedout", "enotfound", "unable to connect", "failed to connect", "http 503", "status 503"}},
	}
	for _, g := range groups {
		for _, pattern := range g.patterns {
			if strings.Contains(lower, pattern) {
				return &AgentUnavailableError{Agent: agent, Kind: g.kind, Detail: g.detail}
			}
		}
	}
	return nil
}
func requestedAgent(t Task) string {
	if t.RequestedAgent != "" {
		return t.RequestedAgent
	}
	return t.Agent
}
func reviewStage(stage string) bool {
	switch stage {
	case "review", "verify", "prototype_review", "prototype_plan_review":
		return true
	}
	return false
}
func authorForTask(r Run, t Task) string {
	for i := len(r.Tasks) - 1; i >= 0; i-- {
		old := r.Tasks[i]
		if old.Status != "done" || old.Phase >= t.Phase {
			continue
		}
		if (t.Stage == "review" || t.Stage == "verify") && (old.Stage == "draft" || old.Stage == "revision") {
			return old.Agent
		}
		if t.Stage == "prototype_review" && old.Stage == "prototype_code" {
			return old.Agent
		}
		if t.Stage == "prototype_plan_review" && (old.Stage == "prototype_plan" || old.Stage == "prototype_plan_final") {
			return old.Agent
		}
	}
	return ""
}
func (e *Engine) markUnavailable(id string, problem *AgentUnavailableError) error {
	_, err := e.Store.UpdateRun(id, func(r *Run) {
		if r.Unavailable == nil {
			r.Unavailable = map[string]AgentIssue{}
		}
		r.Unavailable[problem.Agent] = AgentIssue{Kind: problem.Kind, Detail: problem.Detail, Since: now()}
		delete(r.Sessions, problem.Agent)
	})
	if err != nil {
		return err
	}
	return e.Store.Event(id, problem.Agent, "unavailable", problem.Detail)
}
func (e *Engine) perform(ctx context.Context, r Run, t Task) error {
	intended := requestedAgent(t)
	for attempt := 0; attempt < 3; attempt++ {
		fresh, err := e.Store.Run(r.ID)
		if err != nil {
			return err
		}
		actual := intended
		if issue, blocked := fresh.Unavailable[actual]; blocked {
			if fresh.AvailabilityPolicy != "solo" || t.Stage == "independent_review" {
				return issueError(actual, issue)
			}
			actual = otherAgent(actual)
			if other, blocked := fresh.Unavailable[actual]; blocked {
				return fmt.Errorf("Оба участника недоступны. Работа сохранена. %w", issueError(actual, other))
			}
		}
		slot := e.agentSlots[actual]
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		// Another task may have exhausted this account while we were waiting.
		fresh, err = e.Store.Run(r.ID)
		if err != nil {
			<-slot
			return err
		}
		if issue, blocked := fresh.Unavailable[actual]; blocked {
			<-slot
			if attempt == 2 {
				return issueError(actual, issue)
			}
			continue
		}
		t.RequestedAgent = intended
		t.Agent = actual
		t.SelfReview = reviewStage(t.Stage) && authorForTask(fresh, t) == actual
		_, err = e.Store.UpdateRun(r.ID, func(r *Run) {
			for i := range r.Tasks {
				if r.Tasks[i].ID == t.ID {
					r.Tasks[i].RequestedAgent = intended
					r.Tasks[i].Agent = actual
					r.Tasks[i].SelfReview = t.SelfReview
					break
				}
			}
		})
		if err == nil && actual != intended {
			err = e.Store.Event(r.ID, actual, "handoff", fmt.Sprintf("Задача %s (%s): вместо %s работает %s. Независимое ревью не подменяется самопроверкой.", t.ID, stageLabel(t.Stage), intended, actual))
		}
		if err == nil {
			err = e.performOnce(ctx, fresh, t)
		}
		var unavailable *AgentUnavailableError
		retryable := err != nil && errors.As(err, &unavailable) && ctx.Err() == nil
		if retryable {
			if saveErr := e.markUnavailable(r.ID, unavailable); saveErr != nil {
				<-slot
				return saveErr
			}
		}
		<-slot
		if err == nil {
			return nil
		}
		if !retryable {
			return err
		}

		if fresh.AvailabilityPolicy != "solo" || t.Stage == "independent_review" {
			return unavailable
		}
	}
	return errors.New("Оба участника недоступны. Сохранённые этапы и файлы остаются на месте")
}
func availabilityPrompt(r Run, t Task) string {
	if len(r.Unavailable) == 0 && t.Agent == requestedAgent(t) && !t.SelfReview {
		return ""
	}
	text := fmt.Sprintf("\nФактический исполнитель: %s. Изначально задача назначена %s. Не изображай другого агента и не приписывай ему свои выводы. При подмене участника его адресованные уточнения относятся к переданной задаче.\n", t.Agent, requestedAgent(t))
	if t.SelfReview {
		text += "Это САМОПРОВЕРКА: ты проверяешь собственный результат. Не называй её независимой проверкой или согласием второго агента. Всё равно честно оцени критерии и исправимые недостатки.\n"
	}
	return text
}
func (e *Engine) SetAvailability(id string, options AvailabilityOptions, returnAgent string) (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := normalizeAvailability(options)
	if err != nil {
		return Run{}, err
	}
	if returnAgent != "" && !validAgent(returnAgent) {
		return Run{}, errors.New("Неизвестный участник")
	}
	if _, active := e.active[id]; active {
		return Run{}, errors.New("Сначала дождитесь завершения или поставьте работу на паузу")
	}
	r, err := e.Store.UpdateRun(id, func(r *Run) {
		r.AvailabilityPolicy = a.Policy
		if r.Unavailable == nil {
			r.Unavailable = map[string]AgentIssue{}
		}
		if a.UnavailableAgent != "" {
			r.Unavailable[a.UnavailableAgent] = AgentIssue{Kind: "manual", Detail: "Участник отключён пользователем до восстановления доступа", Since: now()}
			delete(r.Sessions, a.UnavailableAgent)
		}
		if returnAgent != "" {
			delete(r.Unavailable, returnAgent)
			delete(r.Sessions, returnAgent)
		}
	})
	if err != nil {
		return r, err
	}
	err = e.Store.Event(id, "user", "availability", fmt.Sprintf("Политика: %s. Недоступен: %s. Разрешён повторный запрос: %s", a.Policy, a.UnavailableAgent, returnAgent))
	return r, err
}
