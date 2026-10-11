package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func (e *Engine) checkBuild(ctx context.Context, id, buildID string) error {
	r, err := e.Store.Run(id)
	if err != nil {
		return err
	}
	p := r.Prototype
	idx := -1
	for i := range p.Builds {
		if p.Builds[i].ID == buildID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("Сборка не найдена")
	}
	if p.Builds[idx].Review != nil {
		return nil
	}
	for _, check := range p.Plan.Checks {
		r, err = e.Store.Run(id)
		if err != nil {
			return err
		}
		p = r.Prototype
		b := p.Builds[idx]
		done := false
		for i := len(b.Checks) - 1; i >= 0; i-- {
			if b.Checks[i].Name == check.Name {
				done = b.Checks[i].Status == "passed" || b.Checks[i].Status == "failed"
				break
			}
		}
		if done {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		result := CheckResult{Name: check.Name, Kind: check.Kind, Args: check.Args, Status: "running", ExitCode: -1, StartedAt: now()}
		command, _ := json.Marshal(check.Args)
		if err = e.Store.Event(id, "system", "command", fmt.Sprintf("Сборка %d / %s\n%s", b.Number, check.Name, command)); err != nil {
			return err
		}
		attempt := len(b.Checks)
		_, err = e.Store.UpdateRun(id, func(r *Run) { r.Prototype.Builds[idx].Checks = append(r.Prototype.Builds[idx].Checks, result) })
		if err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
		cmd := exec.CommandContext(callCtx, check.Args[0], check.Args[1:]...)
		cmd.Dir = p.Workspace
		cmd.Env = checkEnv()
		cmd.WaitDelay = 3 * time.Second
		isolateProcess(cmd)
		var output cappedBuffer
		output.limit = 128 << 10
		cmd.Stdout = &output
		cmd.Stderr = &output
		runErr := cmd.Run()
		result.Output = output.String()
		result.Status = "passed"
		result.ExitCode = 0
		if runErr != nil {
			result.Status = "failed"
			result.ExitCode = -1
			if cmd.ProcessState != nil {
				result.ExitCode = cmd.ProcessState.ExitCode()
			}
			result.Output += "\n" + runErr.Error()
		}
		if callCtx.Err() != nil {
			result.Status = "interrupted"
			result.Output += "\nКоманда прервана: " + callCtx.Err().Error()
			if ctx.Err() == nil {
				result.Status = "failed"
			}
		}
		cancel()
		result.FinishedAt = now()
		if output.Len() >= output.limit {
			result.Output += "\n[Журнал этой команды ограничен 128 КБ]"
		}
		_, err = e.Store.UpdateRun(id, func(r *Run) { r.Prototype.Builds[idx].Checks[attempt] = result })
		if err != nil {
			return err
		}
		if err = e.Store.Event(id, "system", "check", fmt.Sprintf("Сборка %d / %s / %s / exit %d\n%s", b.Number, check.Name, result.Status, result.ExitCode, result.Output)); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	// A build result must refer to the source checkpoint that was reviewed.
	changed, err := git(ctx, p.Workspace, "diff", "HEAD", "--name-only")
	if err != nil {
		return err
	}
	if strings.TrimSpace(changed) != "" {
		return errors.New("Команды проверки изменили исходники: " + changed + ". Изменения сохранены в рабочей папке; проверьте их перед продолжением")
	}
	if p.Plan.Platform == "browser" {
		fresh, loadErr := e.Store.Run(id)
		if loadErr != nil {
			return loadErr
		}
		if checksPassed(fresh.Prototype.Builds[idx], *p.Plan) && !fresh.Prototype.Builds[idx].PreviewReady {
			previewErr := e.Store.capturePreview(fresh, buildID)
			_, err = e.Store.UpdateRun(id, func(r *Run) {
				b := &r.Prototype.Builds[idx]
				if previewErr == nil {
					b.PreviewReady = true
					b.PreviewError = ""
				} else {
					b.PreviewError = previewErr.Error()
				}
			})
			if err != nil {
				return err
			}
			if previewErr != nil {
				if err = e.Store.Event(id, "system", "preview", previewErr.Error()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
