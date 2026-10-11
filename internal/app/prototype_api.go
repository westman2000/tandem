package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"time"
)

func (s *Server) developmentAPI(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if len(parts) >= 3 && parts[0] == "projects" {
		project := parts[1]
		if parts[2] == "revisions" {
			if len(parts) == 3 && r.Method == "GET" {
				v, err := s.Store.Revisions(project)
				if err != nil {
					problem(w, 500, err)
				} else {
					respond(w, 200, v)
				}
				return true
			}
			if len(parts) == 5 && parts[4] == "approve" && r.Method == "POST" {
				v, err := s.Store.ApproveRevision(project, parts[3])
				if err != nil {
					problem(w, 409, err)
				} else {
					respond(w, 200, v)
				}
				return true
			}
			if len(parts) == 4 && r.Method == "GET" {
				vs, err := s.Store.Revisions(project)
				if err != nil {
					problem(w, 500, err)
					return true
				}
				for _, v := range vs {
					if v.ID == parts[3] {
						w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
						w.Header().Set("Content-Disposition", `attachment; filename="tandem-revision-`+v.ID+`.md"`)
						_, _ = io.WriteString(w, v.Text)
						return true
					}
				}
				http.NotFound(w, r)
				return true
			}
		}
		if len(parts) == 3 && parts[2] == "prototypes" && r.Method == "POST" {
			var c PrototypeConfig
			if err := decode(w, r, &c); err != nil {
				problem(w, 400, err)
				return true
			}
			source, err := s.Store.Approval(project, c.RevisionID)
			if err != nil {
				problem(w, 409, err)
				return true
			}
			parent, err := s.Store.Run(source.RunID)
			if err != nil {
				problem(w, 409, err)
				return true
			}
			if c.Availability.Policy == "" {
				c.Availability.Policy = parent.AvailabilityPolicy
			}
			c.Availability.Issues = parent.Unavailable
			if !s.checkAvailability(w, r.Context(), &c.Availability) {
				return true
			}
			run, err := s.Engine.CreatePrototype(project, c)
			if err != nil {
				problem(w, 409, err)
			} else {
				respond(w, 201, run)
			}
			return true
		}
	}
	if len(parts) >= 3 && parts[0] == "runs" {
		run, err := s.Store.Run(parts[1])
		if err != nil {
			return false
		}
		if len(parts) == 3 && parts[2] == "journal" && r.Method == "GET" {
			events, err := s.Store.events(run.ID, true)
			if err != nil {
				problem(w, 500, err)
				return true
			}
			messages, err := s.Store.Messages(run.ProjectID)
			if err != nil {
				problem(w, 500, err)
				return true
			}
			own := []Message{}
			for _, m := range messages {
				if m.RunID == run.ID {
					own = append(own, m)
				}
			}
			w.Header().Set("Content-Disposition", `attachment; filename="tandem-journal-`+run.ID+`.json"`)
			respond(w, 200, map[string]any{"run": run, "messages": own, "events": events})
			return true
		}
		if run.Prototype == nil {
			return false
		}
		if len(parts) == 5 && parts[2] == "builds" && parts[4] == "source" && r.Method == "GET" {
			for _, b := range run.Prototype.Builds {
				if b.ID == parts[3] {
					ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=", "archive", "--format=zip", b.Commit)
					cmd.Dir = run.Prototype.Workspace
					cmd.Env = gitEnv()
					isolateProcess(cmd)
					var out cappedBuffer
					out.limit = 32 << 20
					cmd.Stdout = &out
					if err = cmd.Run(); err != nil {
						problem(w, 500, err)
						return true
					}
					if out.Len() >= out.limit {
						problem(w, 413, errors.New("Архив больше 32 МБ. Используйте рабочую папку"))
						return true
					}
					w.Header().Set("Content-Type", "application/zip")
					w.Header().Set("Content-Disposition", `attachment; filename="tandem-build-`+b.ID+`.zip"`)
					_, _ = w.Write(out.Bytes())
					return true
				}
			}
			http.NotFound(w, r)
			return true
		}
		if len(parts) == 4 && parts[2] == "prototype" && r.Method == "POST" {
			var in struct {
				PlanHash string `json:"planHash"`
				BuildID  string `json:"buildId"`
				Note     string `json:"note"`
			}
			if err = decode(w, r, &in); err != nil {
				problem(w, 400, err)
				return true
			}
			if parts[3] == "preview" {
				url, err := s.Engine.Preview(run.ID, in.BuildID)
				if err != nil {
					problem(w, 409, err)
				} else {
					respond(w, 200, map[string]string{"url": url})
				}
				return true
			}
			if parts[3] == "approve" || parts[3] == "feedback" || parts[3] == "revise-plan" {
				if !s.checkRunAgents(w, r.Context(), run) {
					return true
				}
			}
			if parts[3] == "approve" {
				run, err = s.Engine.ApprovePrototype(run.ID, in.PlanHash)
			} else {
				run, err = s.Engine.PrototypeAction(run.ID, parts[3], in.BuildID, in.Note)
			}
			if err != nil {
				problem(w, 409, err)
			} else {
				respond(w, 200, run)
			}
			return true
		}
	}
	return false
}
