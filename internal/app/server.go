package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Server struct {
	Store  *Store
	Engine *Engine
	Token  string
	UI     fs.FS
}

func NewServer(s *Store, e *Engine, ui fs.FS) *Server {
	return &Server{Store: s, Engine: e, Token: ID() + ID(), UI: ui}
}
func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, code int, err error) {
	respond(w, code, map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("Некорректное тело запроса")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("Лишние данные в запросе")
	}
	return nil
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if !loopbackHost(r.Host) {
		problem(w, 403, errors.New("Разрешён только локальный адрес"))
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "http" || u.Host != r.Host {
			problem(w, 403, errors.New("Запрос с другого сайта запрещён"))
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Header.Get("X-Tandem-Token") != s.Token {
			problem(w, 403, errors.New("Обновите страницу и повторите действие"))
			return
		}
		s.api(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if r.URL.Path == "/" {
		r.URL.Path = "/index.html"
	}
	// FileServer redirects /index.html; serve the SPA entry directly instead.
	if r.URL.Path == "/index.html" {
		b, err := fs.ReadFile(s.UI, "index.html")
		if err != nil {
			http.Error(w, "Сначала выполните npm run build", 503)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
		return
	}
	http.FileServer(http.FS(s.UI)).ServeHTTP(w, r)
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	parts := strings.Split(path, "/")
	if s.developmentAPI(w, r, parts) {
		return
	}
	if path == "bootstrap" && r.Method == "GET" {
		p, err := s.Store.Projects()
		if err != nil {
			problem(w, 500, err)
			return
		}
		respond(w, 200, map[string]any{"token": s.Token, "projects": p, "version": "0.4.0", "dataDir": s.Store.Dir})
		return
	}
	if path == "models" && r.Method == "GET" {
		respond(w, 200, modelCatalog(s.Store))
		return
	}
	if path == "agents" && r.Method == "GET" {
		respond(w, 200, AgentStatuses(r.Context()))
		return
	}
	if path == "projects" && r.Method == "POST" {
		var in struct {
			Title string `json:"title"`
		}
		if err := decode(w, r, &in); err != nil {
			problem(w, 400, err)
			return
		}
		in.Title = strings.TrimSpace(in.Title)
		if in.Title == "" || len(in.Title) > 200 {
			problem(w, 400, errors.New("Название должно содержать от 1 до 200 байт"))
			return
		}
		p := Project{ID: ID(), Title: in.Title, CreatedAt: now(), UpdatedAt: now()}
		if err := s.Store.PutProject(p); err != nil {
			problem(w, 500, err)
			return
		}
		respond(w, 201, p)
		return
	}
	if len(parts) >= 2 && parts[0] == "projects" {
		p, err := s.Store.Project(parts[1])
		if err != nil {
			problem(w, 404, errors.New("Проект не найден"))
			return
		}
		if len(parts) == 2 && r.Method == "GET" {
			messages, err := s.Store.Messages(p.ID)
			if err != nil {
				problem(w, 500, err)
				return
			}
			materials, err := s.Store.Materials(p.ID)
			if err != nil {
				problem(w, 500, err)
				return
			}
			runs, err := s.Store.Runs(p.ID)
			if err != nil {
				problem(w, 500, err)
				return
			}
			revisions, err := s.Store.Revisions(p.ID)
			if err != nil {
				problem(w, 500, err)
				return
			}
			respond(w, 200, map[string]any{"project": p, "messages": messages, "materials": materials, "runs": runs, "revisions": revisions})
			return
		}
		if len(parts) == 3 && parts[2] == "runs" && r.Method == "POST" {
			var in struct {
				Availability AvailabilityOptions `json:"availability"`
				Mode         string              `json:"mode"`
				Brief        string              `json:"brief"`
				MaxRevisions int                 `json:"maxRevisions"`
				Acceptance   string              `json:"acceptance"`
				Models       map[string]string   `json:"models"`
			}
			if err := decode(w, r, &in); err != nil {
				problem(w, 400, err)
				return
			}
			if in.Mode == "live" && !s.checkAvailability(w, r.Context(), &in.Availability) {
				return
			}
			if in.MaxRevisions == 0 {
				in.MaxRevisions = DefaultRevisions
			}
			run, err := s.Engine.CreateConfigured(p.ID, in.Mode, in.Brief, in.MaxRevisions, in.Acceptance, in.Models, in.Availability)
			if err != nil {
				problem(w, 409, err)
				return
			}
			respond(w, 201, run)
			return
		}
		if len(parts) == 3 && parts[2] == "messages" && r.Method == "POST" {
			var in struct {
				Text   string `json:"text"`
				Target string `json:"target"`
				RunID  string `json:"runId"`
			}
			if err := decode(w, r, &in); err != nil {
				problem(w, 400, err)
				return
			}
			if strings.TrimSpace(in.Text) == "" || len(in.Text) > 20_000 {
				problem(w, 400, errors.New("Сообщение должно содержать от 1 до 20 000 байт"))
				return
			}
			if in.Target != "all" && in.Target != "codex" && in.Target != "claude" {
				problem(w, 400, errors.New("Неизвестный адресат"))
				return
			}
			run, err := s.Store.Run(in.RunID)
			if err != nil || run.ProjectID != p.ID {
				problem(w, 404, errors.New("Цикл не найден"))
				return
			}
			if run.Status != "running" && run.Status != "paused" && run.Status != "pausing" && run.Status != "failed" {
				problem(w, 409, errors.New("Цикл завершён. Начните новый цикл с уточнённой задачей"))
				return
			}
			m := Message{ID: ID(), ProjectID: p.ID, RunID: run.ID, Actor: "user", Target: in.Target, Text: in.Text, Stage: "steering", Demo: run.Mode == "demo", CreatedAt: now()}
			if err := s.Store.AddMessage(m); err != nil {
				problem(w, 500, err)
				return
			}
			respond(w, 201, m)
			return
		}
		if len(parts) == 3 && parts[2] == "materials" && r.Method == "POST" {
			r.Body = http.MaxBytesReader(w, r.Body, MaxFileSize+(1<<20))
			if err := r.ParseMultipartForm(MaxFileSize); err != nil {
				problem(w, 400, errors.New("Не удалось загрузить файл. Максимальный размер — 12 МБ"))
				return
			}
			defer r.MultipartForm.RemoveAll()
			f, h, err := r.FormFile("file")
			if err != nil {
				problem(w, 400, err)
				return
			}
			defer f.Close()
			data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
			if err != nil {
				problem(w, 400, err)
				return
			}
			m, err := s.Store.SaveMaterial(p.ID, h.Filename, r.FormValue("role"), h.Header.Get("Content-Type"), data)
			if err != nil {
				problem(w, 400, err)
				return
			}
			respond(w, 201, m)
			return
		}
	}
	if len(parts) == 2 && parts[0] == "materials" && r.Method == "DELETE" {
		if _, err := s.Store.Material(parts[1]); err != nil {
			problem(w, 404, errors.New("Материал не найден"))
			return
		}
		if err := s.Store.DeleteMaterial(parts[1]); err != nil {
			problem(w, 500, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(parts) >= 2 && parts[0] == "runs" {
		run, err := s.Store.Run(parts[1])
		if err != nil {
			problem(w, 404, errors.New("Цикл не найден"))
			return
		}
		if len(parts) == 3 && parts[2] == "events" && r.Method == "GET" {
			events, err := s.Store.Events(run.ID)
			if err != nil {
				problem(w, 500, err)
				return
			}
			respond(w, 200, events)
			return
		}
		if len(parts) == 3 && parts[2] == "document" && r.Method == "GET" {
			if run.Document == "" {
				problem(w, 404, errors.New("Документ ещё не создан"))
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="tandem-design-`+run.ID+`.md"`)
			_, _ = io.WriteString(w, run.Document)
			return
		}
		if len(parts) == 3 && r.Method == "POST" {
			switch parts[2] {
			case "availability":
				var in struct {
					AvailabilityOptions
					ReturnAgent string `json:"returnAgent"`
				}
				if err := decode(w, r, &in); err != nil {
					problem(w, 400, err)
					return
				}
				run, err = s.Engine.SetAvailability(run.ID, in.AvailabilityOptions, in.ReturnAgent)
			case "independent-review":
				var in struct {
					TaskID  string `json:"taskId"`
					BuildID string `json:"buildId"`
				}
				if err := decode(w, r, &in); err != nil {
					problem(w, 400, err)
					return
				}
				artifact, artifactErr := auditArtifact(run, in.TaskID, in.BuildID)
				if artifactErr != nil {
					problem(w, 409, artifactErr)
					return
				}
				a := agentStatus(r.Context(), otherAgent(artifact.Agent))
				if !a.Subscription {
					problem(w, 409, fmt.Errorf("%s: %s", a.Agent, a.Detail))
					return
				}
				run, err = s.Engine.RequestAudit(run.ID, in.TaskID, in.BuildID)
			case "continue":
				var in struct {
					MaxRevisions int    `json:"maxRevisions"`
					Instruction  string `json:"instruction"`
				}
				if err := decode(w, r, &in); err != nil {
					problem(w, 400, err)
					return
				}
				if run.Mode == "live" && !s.checkRunAgents(w, r.Context(), run) {
					return
				}
				run, err = s.Engine.Continue(run.ID, in.MaxRevisions, in.Instruction)
			case "pause":
				run, err = s.Engine.Pause(run.ID)
			case "resume":
				if run.Mode == "live" && !s.checkRunAgents(w, r.Context(), run) {
					return
				}
				run, err = s.Engine.Resume(run.ID)
			case "finish":
				run, err = s.Engine.Finish(run.ID)
			default:
				http.NotFound(w, r)
				return
			}
			if err != nil {
				problem(w, 409, err)
				return
			}
			respond(w, 200, run)
			return
		}
	}
	http.NotFound(w, r)
}

// Authentication confirms subscription billing, not remaining quota.
func availableSubscriptions(a AvailabilityOptions, statuses []AgentStatus) (map[string]AgentIssue, error) {
	a, err := normalizeAvailability(a)
	if err != nil {
		return nil, err
	}
	issues := map[string]AgentIssue{}
	for k, v := range a.Issues {
		issues[k] = v
	}
	if a.UnavailableAgent != "" {
		issues[a.UnavailableAgent] = AgentIssue{Kind: "manual", Detail: "Участник отключён пользователем", Since: now()}
	}
	available := 0
	for _, s := range statuses {
		if _, blocked := issues[s.Agent]; blocked {
			continue
		}
		if s.Subscription {
			available++
			continue
		}
		kind := "auth"
		if !s.Installed {
			kind = "missing_cli"
		}
		issues[s.Agent] = AgentIssue{Kind: kind, Detail: s.Detail + ". В терминале: " + s.LoginCommand, Since: now()}
	}
	if available == 0 {
		return issues, errors.New("Нет доступных участников с входом по подписке. Восстановите доступ и разрешите повторный запрос")
	}
	if a.Policy != "solo" && len(issues) > 0 {
		return issues, errors.New("Участник недоступен. Восстановите доступ или выберите продолжение одним агентом")
	}
	return issues, nil
}
func (s *Server) checkAvailability(w http.ResponseWriter, ctx context.Context, a *AvailabilityOptions) bool {
	issues, err := availableSubscriptions(*a, AgentStatuses(ctx))
	a.Issues = issues
	if err != nil {
		problem(w, 409, err)
		return false
	}
	return true
}
func (s *Server) checkRunAgents(w http.ResponseWriter, ctx context.Context, r Run) bool {
	// An independent audit needs only its reviewer, even under the wait policy.
	if r.ActiveAuditID != "" {
		for _, audit := range r.Audits {
			if audit.ID == r.ActiveAuditID {
				if issue, blocked := r.Unavailable[audit.Agent]; blocked {
					problem(w, 409, issueError(audit.Agent, issue))
					return false
				}
				a := agentStatus(ctx, audit.Agent)
				if !a.Subscription {
					problem(w, 409, fmt.Errorf("%s: %s", a.Agent, a.Detail))
					return false
				}
				return true
			}
		}
		problem(w, 409, errors.New("Не найдена задача независимого ревью"))
		return false
	}
	a := AvailabilityOptions{Policy: r.AvailabilityPolicy, Issues: r.Unavailable}
	okay := s.checkAvailability(w, ctx, &a)
	for agent, issue := range a.Issues {
		if _, known := r.Unavailable[agent]; !known {
			if err := s.Engine.markUnavailable(r.ID, &AgentUnavailableError{Agent: agent, Kind: issue.Kind, Detail: issue.Detail}); err != nil {
				if okay {
					problem(w, 500, err)
				}
				return false
			}
		}
	}
	return okay
}

func Serve(ctx context.Context, address, data string, ui fs.FS) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || !loopbackHost(host) {
		return errors.New("Tandem MVP слушает только loopback: 127.0.0.1 или localhost")
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return err
	}
	store, err := OpenStore(data)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.Recover(); err != nil {
		return err
	}
	engine := NewEngine(store, CLIRunner{})
	defer engine.Close()
	server := &http.Server{Addr: address, Handler: NewServer(store, engine, ui), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Tandem → http://%s\nДанные → %s\n", address, data)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(stop)
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
