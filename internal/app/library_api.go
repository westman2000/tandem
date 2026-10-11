package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type folderChoice struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func directoryChoices(path string) (map[string]any, error) {
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	root, err := canonicalDirectory(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, errors.New("Нет доступа к этой папке")
	}
	choices := []folderChoice{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		kind := "folder"
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			kind = "repository"
		}
		if _, err := os.Stat(filepath.Join(p, ".tandem-incomplete")); err == nil {
			kind = "incomplete"
		}
		if _, err := os.Stat(filepath.Join(p, manifestName)); err == nil {
			kind = "project"
		}
		choices = append(choices, folderChoice{Name: e.Name(), Path: p, Kind: kind})
		if len(choices) >= 1000 {
			break
		}
	}
	host, _ := os.Hostname()
	return map[string]any{"path": root, "parent": filepath.Dir(root), "machine": host, "folders": choices, "truncated": len(choices) >= 1000}, nil
}
func (s *Server) libraryAPI(w http.ResponseWriter, r *http.Request, parts []string) bool {
	l := s.Library
	path := strings.Join(parts, "/")
	if path == "bootstrap" && r.Method == "GET" {
		l.mu.RLock()
		defer l.mu.RUnlock()
		ps, err := l.listLocked()
		if err != nil {
			problem(w, 500, err)
			return true
		}
		host, _ := os.Hostname()
		respond(w, 200, map[string]any{"token": s.Token, "projects": ps, "version": "0.6.0", "dataDir": s.Store.Dir, "defaultParent": l.DefaultParent, "machine": host})
		return true
	}
	if path == "folders" && r.Method == "GET" {
		if r.Header.Get("X-Tandem-Token") != s.Token {
			problem(w, 403, errors.New("Обновите страницу перед выбором папки"))
			return true
		}
		v, err := directoryChoices(r.URL.Query().Get("path"))
		if err != nil {
			problem(w, 400, err)
		} else {
			respond(w, 200, v)
		}
		return true
	}
	if path == "projects" && r.Method == "POST" {
		var in struct {
			Title  string `json:"title"`
			Parent string `json:"parent"`
		}
		if err := decode(w, r, &in); err != nil {
			problem(w, 400, err)
			return true
		}
		in.Title = strings.TrimSpace(in.Title)
		if in.Title == "" || len(in.Title) > 200 {
			problem(w, 400, errors.New("Название должно содержать от 1 до 200 байт"))
			return true
		}
		p, err := l.create(in.Title, in.Parent)
		if err != nil {
			problem(w, 409, err)
		} else {
			respond(w, 201, p)
		}
		return true
	}
	if path == "projects/open" && r.Method == "POST" {
		var in struct {
			Path string `json:"path"`
		}
		if err := decode(w, r, &in); err != nil {
			problem(w, 400, err)
			return true
		}
		p, err := l.open(in.Path, "")
		if err != nil {
			problem(w, 409, err)
		} else {
			respond(w, 200, p)
		}
		return true
	}
	if path == "projects/copy-folder" && r.Method == "POST" {
		var in struct {
			Path   string `json:"path"`
			Parent string `json:"parent"`
			Title  string `json:"title"`
		}
		if err := decode(w, r, &in); err != nil {
			problem(w, 400, err)
			return true
		}
		if len(in.Title) > 200 {
			problem(w, 400, errors.New("Слишком длинное название"))
			return true
		}
		p, err := l.copyFolder(in.Path, in.Parent, strings.TrimSpace(in.Title))
		if err != nil {
			problem(w, 409, err)
		} else {
			respond(w, 201, p)
		}
		return true
	}
	if len(parts) != 3 || parts[0] != "projects" || r.Method != "POST" {
		return false
	}
	action := parts[2]
	if action != "transfer" && action != "copy" && action != "relocate" && action != "rename" && action != "forget" && action != "reveal" {
		return false
	}
	var in struct {
		Path   string `json:"path"`
		Parent string `json:"parent"`
		Title  string `json:"title"`
	}
	if err := decode(w, r, &in); err != nil {
		problem(w, 400, err)
		return true
	}
	if len(in.Title) > 200 {
		problem(w, 400, errors.New("Слишком длинное название"))
		return true
	}
	if action == "relocate" {
		p, err := l.open(in.Path, parts[1])
		if err != nil {
			problem(w, 409, err)
		} else {
			respond(w, 200, p)
		}
		return true
	}
	if action == "transfer" || action == "copy" {
		p, err := l.transfer(parts[1], in.Parent, strings.TrimSpace(in.Title), action == "copy")
		if err != nil {
			problem(w, 409, err)
		} else {
			respond(w, 200, p)
		}
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[parts[1]]
	target := s
	if e != nil {
		if e.Removed {
			problem(w, 404, errors.New("Проект убран из списка"))
			return true
		}
		target = e.Server
	}
	if action == "forget" {
		if target != nil {
			if err := l.idle(target, parts[1]); err != nil {
				problem(w, 409, err)
				return true
			}
		}
		if e == nil {
			problem(w, 409, errors.New("Сначала перенесите проект в отдельную папку, чтобы его можно было открыть снова"))
			return true
		}
		e.Removed = true
		if err := l.save(e); err != nil {
			e.Removed = false
			problem(w, 500, err)
			return true
		}
		l.closeEntry(e)
		l.entries[parts[1]] = e
		respond(w, 200, map[string]bool{"ok": true})
		return true
	}
	if target == nil {
		problem(w, 409, errors.New("Сначала укажите расположение проекта"))
		return true
	}
	p, err := target.Store.Project(parts[1])
	if err != nil {
		problem(w, 404, err)
		return true
	}
	if action == "rename" {
		in.Title = strings.TrimSpace(in.Title)
		if in.Title == "" {
			problem(w, 400, errors.New("Введите название"))
			return true
		}
		p.Title = in.Title
		p.UpdatedAt = now()
		if err = target.Store.PutProject(p); err != nil {
			problem(w, 409, err)
			return true
		}
		if e != nil {
			e.Project = p
			if err = l.save(e); err != nil {
				problem(w, 500, err)
				return true
			}
		}
		respond(w, 200, p)
		return true
	}
	root := target.Store.Root
	if root == "" {
		root = target.Store.Dir
	}
	if err = target.Store.CheckFolder(); err != nil {
		problem(w, 409, err)
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", root)
	case "windows":
		cmd = exec.CommandContext(ctx, "explorer.exe", root)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", root)
	}
	if err = cmd.Run(); err != nil {
		problem(w, 409, errors.New("Не удалось открыть файловый менеджер. Скопируйте путь из карточки проекта"))
	} else {
		respond(w, 200, map[string]bool{"ok": true})
	}
	return true
}
