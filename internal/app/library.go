package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type projectEntry struct {
	Project Project
	Path    string
	Removed bool
	Server  *Server
	Error   string
}
type Library struct {
	mu            sync.RWMutex // leases cover requests; moving/closing cannot race a handler
	main          *Server
	entries       map[string]*projectEntry
	DefaultParent string
}

func NewLibrary(main *Server) (*Library, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	l := &Library{main: main, entries: map[string]*projectEntry{}, DefaultParent: filepath.Join(home, "Tandem", "Projects")}
	_, err = main.Store.db.Exec(`CREATE TABLE IF NOT EXISTS project_registry(id TEXT PRIMARY KEY,body TEXT NOT NULL); CREATE TABLE IF NOT EXISTS app_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);`)
	if err != nil {
		return nil, err
	}
	var parent string
	if main.Store.db.QueryRow("SELECT value FROM app_settings WHERE key='project_parent'").Scan(&parent) == nil && parent != "" {
		l.DefaultParent = parent
	}
	rows, err := main.Store.db.Query("SELECT body FROM project_registry")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b string
		var e projectEntry
		if rows.Scan(&b) == nil && json.Unmarshal([]byte(b), &e) == nil {
			l.entries[e.Project.ID] = &e
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, e := range l.entries {
		if !e.Removed {
			e.Server, err = l.openFolder(e.Path, e.Project.ID)
			if err != nil {
				e.Error = err.Error()
			}
		}
	}
	main.Library = l
	return l, nil
}
func (l *Library) save(e *projectEntry) error {
	copy := *e
	copy.Server = nil
	copy.Error = ""
	b, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	_, err = l.main.Store.db.Exec("INSERT INTO project_registry(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", e.Project.ID, string(b))
	return err
}
func (l *Library) openFolder(root, id string) (*Server, error) {
	root, err := canonicalDirectory(root)
	if err != nil {
		return nil, err
	}
	if err := localProjectVolume(root); err != nil {
		return nil, err
	}
	m, err := readManifest(root)
	if err != nil {
		return nil, err
	}
	if m.ID != id {
		return nil, errors.New("В этой папке другой проект. Используйте «Открыть проект»")
	}
	for _, rel := range []string{".tandem", ".tandem/tandem.sqlite", "materials", "docs", "prototypes"} {
		i, e := os.Lstat(filepath.Join(root, rel))
		if e != nil {
			return nil, fmt.Errorf("Отсутствует часть проекта: %s", rel)
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("Служебные папки проекта не могут быть символическими ссылками")
		}
	}
	s, err := OpenStore(filepath.Join(root, ".tandem"))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Server, error) { s.Close(); return nil, err }
	if err = s.bindFolder(root); err != nil {
		return fail(err)
	}
	ps, err := s.Projects()
	if err != nil || len(ps) != 1 || ps[0].ID != id {
		return fail(errors.New("История не соответствует файлу проекта. Восстановите резервную копию"))
	}
	runs, err := s.Runs("")
	if err != nil {
		return fail(err)
	}
	for _, r := range runs {
		if !validID(r.ID) || r.ProjectID != id {
			return fail(errors.New("Некорректный идентификатор цикла в проекте"))
		}
		for _, m := range r.Materials {
			if !validID(m.ID) || m.ProjectID != id {
				return fail(errors.New("Некорректная ссылка на материал"))
			}
		}
		if p := r.Prototype; p != nil {
			if !within(root, p.Workspace) || !within(root, p.Config.ParentDirectory) || p.Config.SourceRepo != "" && !within(root, p.Config.SourceRepo) {
				return fail(errors.New("Проект содержит внешние рабочие пути. Перенесите его через Tandem"))
			}
			for _, path := range []string{p.Workspace, p.Config.ParentDirectory, p.Config.SourceRepo} {
				if path != "" {
					if err := noProjectLinks(root, path); err != nil {
						return fail(err)
					}
				}
			}
		}
	}
	for _, table := range []string{"runs", "materials", "messages", "approvals"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE project_id IS NOT ?", id).Scan(&count); err != nil || count != 0 {
			return fail(errors.New("В папке обнаружены данные другого проекта"))
		}
	}
	ms, err := s.Materials(id)
	if err != nil {
		return fail(err)
	}
	for _, m := range ms {
		if !validID(m.ID) {
			return fail(errors.New("Некорректный идентификатор материала"))
		}
	}
	// Validation above is complete before saved execution state is changed.
	for _, r := range runs {
		if r.Prototype != nil {
			r.Prototype.Tools = toolChecks(r.Prototype.Plan)
		}
		if len(r.Sessions) > 0 || r.Prototype != nil {
			r.Sessions = map[string]string{}
			if err = s.PutRun(r); err != nil {
				return fail(err)
			}
		}
	}

	if err = s.Recover(); err != nil {
		return fail(err)
	}
	e := NewEngine(s, l.main.Engine.Runner)
	e.Health = l.main.Engine.Health
	e.agentSlots = l.main.Engine.agentSlots
	child := NewServer(s, e, l.main.UI)
	child.Token = l.main.Token
	if e.Health != nil {
		e.Health.AddStore(s)
	}
	return child, nil
}
func (l *Library) closeEntry(e *projectEntry) {
	if e.Server == nil {
		return
	}
	if e.Server.Engine.Health != nil {
		e.Server.Engine.Health.RemoveStore(e.Server.Store)
	}
	e.Server.Engine.Close()
	e.Server.Store.Close()
	e.Server = nil
}
func (l *Library) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		l.closeEntry(e)
	}
}
func (l *Library) listLocked() ([]Project, error) {
	legacy, err := l.main.Store.Projects()
	if err != nil {
		return nil, err
	}
	all := []Project{}
	for _, p := range legacy {
		if _, ok := l.entries[p.ID]; !ok {
			p.Directory = l.main.Store.Dir
			p.FolderState = "legacy"
			if err := l.main.Store.CheckFolder(); err != nil {
				p.FolderError = err.Error()
			}
			all = append(all, p)
		}
	}
	for _, e := range l.entries {
		if e.Removed {
			continue
		}
		p := e.Project
		p.Directory = e.Path
		p.FolderState = "unavailable"
		p.FolderError = e.Error
		if e.Server != nil {
			if err := e.Server.Store.CheckFolder(); err != nil {
				p.FolderError = err.Error()
			} else {
				if current, err := e.Server.Store.Project(p.ID); err == nil {
					p = current
					p.Directory = e.Path
					p.FolderState = "ready"
					p.FolderError = ""
				}
			}
		}
		all = append(all, p)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt > all[j].UpdatedAt })
	return all, nil
}
func (l *Library) List() ([]Project, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.listLocked()
}
func (l *Library) idle(s *Server, project string) error {
	s.Engine.mu.Lock()
	defer s.Engine.mu.Unlock()
	for id := range s.Engine.active {
		r, err := s.Store.Run(id)
		if err != nil {
			return err
		}
		if r.ProjectID == project {
			return errors.New("Сначала поставьте работу проекта на паузу и дождитесь остановки")
		}
	}
	return nil
}

// Scoped routes keep copies independent even when they share historical run IDs.
func (l *Library) dispatch(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/"), "/")
	if len(parts) < 2 {
		return false
	}
	management := map[string]bool{"relocate": true, "transfer": true, "copy": true, "forget": true, "reveal": true, "rename": true}
	if parts[0] == "projects" && (parts[1] == "open" || parts[1] == "copy-folder" || len(parts) == 3 && management[parts[2]]) {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	project := ""
	if parts[0] == "projects" {
		project = parts[1]
	} else if parts[0] == "runs" {
		if old, err := l.main.Store.Run(parts[1]); err == nil {
			project = old.ProjectID
		}
	} else if parts[0] == "materials" {
		if old, err := l.main.Store.Material(parts[1]); err == nil {
			project = old.ProjectID
		}
	}
	if project == "" {
		return false
	}
	target := l.main
	if e, ok := l.entries[project]; ok {
		if e.Removed {
			problem(w, 404, errors.New("Проект убран из списка. Откройте его папку снова"))
			return true
		}
		if e.Server == nil {
			problem(w, 409, errors.New(e.Error))
			return true
		}
		target = e.Server
	} else if _, err := l.main.Store.Project(project); err != nil {
		return false
	}
	if err := target.Store.CheckFolder(); err != nil {
		problem(w, 409, err)
		return true
	}
	if len(parts) > 3 && parts[0] == "projects" && parts[2] == "api" {
		rest := parts[3:]
		valid := false
		if len(rest) > 1 && rest[0] == "runs" {
			v, err := target.Store.Run(rest[1])
			valid = err == nil && v.ProjectID == project
		}
		if len(rest) > 1 && rest[0] == "materials" {
			v, err := target.Store.Material(rest[1])
			valid = err == nil && v.ProjectID == project
		}
		if !valid {
			problem(w, 404, errors.New("Объект не принадлежит выбранному проекту"))
			return true
		}
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Path = "/api/" + strings.Join(rest, "/")
		clone.URL = &u
		r = clone
	}
	if r.Method == "GET" {
		target.api(w, r)
	} else {
		target.mutate(w, r)
	}
	return true
}

// Only the built-in default may be created automatically. A missing remembered
// disk/folder must be restored or explicitly replaced, not silently recreated.
func (l *Library) projectParent(parent string) (string, error) {
	if parent == "" {
		parent = l.DefaultParent
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if parent == filepath.Join(home, "Tandem", "Projects") {
		if err = os.MkdirAll(parent, 0700); err != nil {
			return "", err
		}
	}
	return prepareProjectParent(parent)
}

func (l *Library) create(title, parent string) (Project, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	parent, err := l.projectParent(parent)
	if err != nil {
		return Project{}, err
	}
	if within(l.main.Store.Dir, parent) {
		return Project{}, errors.New("Выберите расположение вне служебного хранилища Tandem")
	}
	name := folderName(title)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return Project{}, err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), name) {
			return Project{}, errors.New("Папка с таким именем уже существует. Измените название или расположение")
		}
	}
	root := filepath.Join(parent, name)
	if err = os.Mkdir(root, 0700); err != nil {
		return Project{}, err
	}
	if err = exclusiveWrite(filepath.Join(root, ".tandem-incomplete"), []byte("Создание не завершено. Готовый проект содержит tandem.project.json.")); err != nil {
		return Project{}, err
	}
	for _, d := range []string{"docs", "materials", "prototypes"} {
		if err = os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			return Project{}, err
		}
	}
	s, err := OpenStore(filepath.Join(root, ".tandem"))
	if err != nil {
		return Project{}, err
	}
	p := Project{ID: ID(), Title: title, CreatedAt: now(), UpdatedAt: now()}
	err = s.PutProject(p)
	ce := s.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return Project{}, err
	}
	if err = writeManifest(root, p.ID); err != nil {
		return Project{}, err
	}
	_ = os.Remove(filepath.Join(root, ".tandem-incomplete"))
	entry := &projectEntry{Project: p, Path: root}
	entry.Server, err = l.openFolder(root, p.ID)
	if err != nil {
		return p, err
	}
	if err = l.save(entry); err != nil {
		l.closeEntry(entry)
		return p, err
	}
	l.entries[p.ID] = entry
	l.DefaultParent = parent
	_, _ = l.main.Store.db.Exec("INSERT INTO app_settings(key,value) VALUES('project_parent',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", parent)
	p.Directory, p.FolderState = root, "ready"
	return p, nil
}
func (l *Library) open(path, expected string) (Project, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	root, err := canonicalDirectory(path)
	if err != nil {
		return Project{}, err
	}
	m, err := readManifest(root)
	if err != nil {
		return Project{}, err
	}
	if expected != "" && m.ID != expected {
		return Project{}, errors.New("Выбрана папка другого проекта")
	}
	old := l.entries[m.ID]
	if old != nil && !old.Removed && old.Path != root && expected == "" {
		return Project{}, errors.New("Этот проект уже известен по другому пути. Используйте «Указать расположение» или «Создать независимую копию»")
	}
	if old != nil && old.Server != nil {
		if old.Path == root && old.Server.Store.CheckFolder() == nil {
			return old.Server.Store.Project(m.ID)
		}
		if err = l.idle(old.Server, m.ID); err != nil {
			return Project{}, err
		}
		l.closeEntry(old)
	}
	server, err := l.openFolder(root, m.ID)
	if err != nil {
		if old != nil && old.Server == nil {
			old.Error = err.Error()
			restored, restoreErr := l.openFolder(old.Path, m.ID)
			if restoreErr == nil {
				old.Server = restored
				old.Error = ""
			}
		}
		return Project{}, err
	}
	p, err := server.Store.Project(m.ID)
	if err != nil {
		l.closeEntry(&projectEntry{Server: server})
		return Project{}, err
	}
	e := &projectEntry{Project: p, Path: root, Server: server}
	if err = l.save(e); err != nil {
		l.closeEntry(e)
		return Project{}, err
	}
	l.entries[p.ID] = e
	return p, nil
}
