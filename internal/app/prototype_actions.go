package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

func checkEnv() []string {
	out := []string{}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		upper := strings.ToUpper(k)
		if strings.HasPrefix(upper, "GIT_") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.HasSuffix(upper, "API_KEY") {
			continue
		}
		out = append(out, v)
	}
	return append(out, "CI=1", "GIT_TERMINAL_PROMPT=0")
}
func (e *Engine) PrototypeAction(id, action, buildID, note string) (Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.Store.Run(id)
	if err != nil {
		return r, err
	}
	p := r.Prototype
	if p == nil {
		return r, errors.New("Это не цикл прототипа")
	}
	if len(note) > 10000 {
		return r, errors.New("Комментарий слишком длинный")
	}
	if _, active := e.active[id]; active {
		return r, errors.New("Сначала дождитесь окончания работы или поставьте её на паузу")
	}
	if action == "extend-time" {
		if r.Status != "completed" || r.Quality != "limit" || p.UsedSeconds < float64(p.Config.MaxMinutes*60) {
			return r, errors.New("Продление времени доступно после остановки по времени")
		}
		_, err = e.Store.UpdateRun(id, func(r *Run) {
			r.Prototype.UsedSeconds = 0
			r.Status = "paused"
			r.Quality = "pending"
			r.StopReason = ""
			r.Prototype.Decisions = append(r.Prototype.Decisions, Decision{CreatedAt: now(), Actor: "user", Text: "Предоставлен дополнительный лимит активного времени"})
		})
		if err != nil {
			return r, err
		}
		return e.startLocked(id)
	}
	if action == "tools" {
		return e.Store.UpdateRun(id, func(r *Run) { r.Prototype.Tools = toolChecks(r.Prototype.Plan) })
	}
	if action == "revise-plan" {
		if r.Status != "awaiting_plan" || strings.TrimSpace(note) == "" {
			return r, errors.New("Укажите замечания к ещё не утверждённому плану")
		}
		_, err = e.Store.UpdateRun(id, func(r *Run) {
			n := len(r.Tasks)
			r.Prototype.ActiveFrom = n
			r.Tasks = append(r.Tasks, prototypeTask(otherAgent(p.Config.Author), "prototype_plan_review", n), prototypeTask(p.Config.Author, "prototype_plan_final", n+1))
			r.Prototype.Plan = nil
			r.Prototype.PlanHash = ""
			r.Prototype.UsedSeconds = 0
			r.Status = "paused"
			r.Prototype.Decisions = append(r.Prototype.Decisions, Decision{CreatedAt: now(), Actor: "user", Text: "Изменение плана: " + note})
		})
		if err != nil {
			return r, err
		}
		if err = e.Store.AddMessage(Message{ProjectID: r.ProjectID, RunID: r.ID, Actor: "user", Target: "all", Text: note, Stage: "steering"}); err != nil {
			return r, err
		}
		return e.startLocked(id)
	}
	idx := -1
	for i := range p.Builds {
		if p.Builds[i].ID == buildID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return r, errors.New("Выберите сохранённую сборку")
	}
	if action == "accept" {
		if p.Builds[idx].Review == nil {
			return r, errors.New("Сначала дождитесь рецензии этой сборки")
		}
		return e.Store.UpdateRun(id, func(r *Run) {
			b := &r.Prototype.Builds[idx]
			b.AcceptedAt = now()
			b.AcceptanceNote = note
			r.Prototype.Decisions = append(r.Prototype.Decisions, Decision{CreatedAt: now(), Actor: "user", BuildID: buildID, Text: "Принято пользователем. " + note})
		})
	}
	if action != "feedback" && action != "restore" {
		return r, errors.New("Неизвестное действие")
	}
	if action == "feedback" && strings.TrimSpace(note) == "" {
		return r, errors.New("Напишите, что изменить в прототипе")
	}
	if r.Status != "completed" && r.Status != "stopped" {
		return r, errors.New("Сначала завершите текущий цикл; затем можно выбрать сборку и начать новую итерацию")
	}
	status, err := git(context.Background(), p.Workspace, "status", "--porcelain")
	if err != nil {
		return r, err
	}
	if status != "" {
		return r, errors.New("В рабочей папке есть несохранённые изменения. Tandem не будет их удалять при возврате к сборке")
	}
	commit := p.Builds[idx].Commit
	if _, err = git(context.Background(), p.Workspace, "checkout", "-b", "tandem/restore-"+ID(), commit); err != nil {
		return r, err
	}
	r, err = e.Store.UpdateRun(id, func(r *Run) {
		p := r.Prototype
		p.CurrentCommit = commit
		p.Decisions = append(p.Decisions, Decision{CreatedAt: now(), Actor: "user", BuildID: buildID, Text: fmt.Sprintf("Выбрана сборка %d: %s. %s", p.Builds[idx].Number, action, note)})
		if action == "feedback" {
			p.Feedback += fmt.Sprintf("\nОт сборки %d: %s", p.Builds[idx].Number, note)
			p.RoundLimit = len(p.Builds) + p.Config.MaxRounds
			p.UsedSeconds = 0
			p.FailureCount = 0
			n := len(r.Tasks)
			p.ActiveFrom = n
			r.Tasks = append(r.Tasks, prototypeTask(p.Config.Author, "prototype_code", n), prototypeTask(otherAgent(p.Config.Author), "prototype_review", n+1))
			r.Status = "paused"
			r.Quality = "pending"
			r.StopReason = ""
			r.Error = ""
		}
	})
	if err != nil {
		return r, err
	}
	if err = e.Store.Event(id, "user", "decision", fmt.Sprintf("%s / сборка %d / %s\n%s", action, p.Builds[idx].Number, commit, note)); err != nil {
		return r, err
	}
	if action == "feedback" {
		return e.startLocked(id)
	}
	return r, nil
}
