package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cloneSource(source, target, commit string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	if _, err := git(context.Background(), filepath.Dir(target), "clone", "--no-hardlinks", "--no-checkout", "--", source, target); err != nil {
		return err
	}
	if _, err := git(context.Background(), target, "checkout", "--detach", commit); err != nil {
		return err
	}
	_, err := git(context.Background(), target, "remote", "remove", "origin")
	return err
}
func transferRun(src, dst *Store, r Run, project string) (Run, error) {
	r.ProjectID = project
	r.Sessions = map[string]string{}
	r.Waiting = nil
	for i := range r.Materials {
		r.Materials[i].ProjectID = project
	}
	if err := copyProjectTree(filepath.Join(src.Dir, "runs", r.ID), filepath.Join(dst.Dir, "runs", r.ID)); err != nil {
		return r, err
	}
	if p := r.Prototype; p != nil {
		p.Source.ProjectID = project
		p.ActiveSince = ""
		workspace := filepath.Join(dst.Root, "prototypes", "tandem-"+r.ID, "workspace")
		_, workspaceErr := os.Stat(p.Workspace)
		if workspaceErr != nil && (!os.IsNotExist(workspaceErr) || p.CurrentCommit != "") {
			return r, errors.New("Рабочая папка прототипа отсутствует или недоступна; перенос остановлен")
		}
		if workspaceErr == nil {
			if src.Root != "" {
				if err := noProjectLinks(src.Root, p.Workspace); err != nil {
					return r, err
				}
			}
			if err := copyProjectTree(p.Workspace, workspace); err != nil {
				return r, err
			}
			if err := exclusiveWrite(filepath.Join(filepath.Dir(workspace), "owner"), []byte(r.ID)); err != nil {
				return r, err
			}
		}
		p.Workspace = workspace
		p.Config.ParentDirectory = filepath.Join(dst.Root, "prototypes")
		if workspaceErr == nil {
			p.Config.SourceRepo = ""
		} else if p.Config.SourceRepo != "" {
			source := filepath.Join(dst.Dir, "sources", r.ID)
			if err := cloneSource(p.Config.SourceRepo, source, p.SourceCommit); err != nil {
				return r, err
			}
			p.Config.SourceRepo = source
		}
		p.Tools = toolChecks(nil)
	}
	return r, nil
}

// The caller excludes HTTP writes and requires the project to be idle. Copy
// selected records into a fresh database: never copy another project's data or
// machine-wide auth/health into a portable folder.
func copyProjectData(src, dst *Store, oldID, newID string) error {
	runs, err := src.Runs(oldID)
	if err != nil {
		return err
	}
	materials, err := src.Materials(oldID)
	if err != nil {
		return err
	}
	files := map[string]string{}
	for _, m := range materials {
		files[m.ID] = m.SHA256
		m.ProjectID = newID
		if err = dst.AddMaterial(m); err != nil {
			return err
		}
	}
	for _, r := range runs {
		for _, m := range r.Materials {
			files[m.ID] = m.SHA256
		}
		r, err = transferRun(src, dst, r, newID)
		if err != nil {
			return err
		}
		if err = dst.PutRun(r); err != nil {
			return err
		}
		events, err := src.events(r.ID, true)
		if err != nil {
			return err
		}
		for i := len(events) - 1; i >= 0; i-- {
			v := events[i]
			if _, err = dst.db.Exec("INSERT INTO events(run_id,agent,kind,text,created_at) VALUES(?,?,?,?,?)", v.RunID, v.Agent, v.Kind, v.Text, v.CreatedAt); err != nil {
				return err
			}
		}
	}
	for id, hash := range files {
		if !validID(id) {
			return errors.New("Некорректная ссылка на материал")
		}
		if src.Root != "" {
			if _, err := safeProjectPath(src.Root, "materials/"+id); err != nil {
				return err
			}
		}
		b, err := readRegularLimited(filepath.Join(src.MaterialsDir(), id), MaxFileSize)
		if err != nil {
			return fmt.Errorf("Не удалось перенести материал %s: %w", id, err)
		}
		if hash != "" && textHash(string(b)) != hash {
			return fmt.Errorf("Материал %s изменён вне Tandem; перенос остановлен", id)
		}
		if err = exclusiveWrite(filepath.Join(dst.MaterialsDir(), id), b); err != nil {
			return err
		}
	}
	for _, r := range runs {
		r.ProjectID = newID
		if err = dst.Snapshot(r); err != nil {
			return err
		}
	}
	messages, err := src.Messages(oldID)
	if err != nil {
		return err
	}
	for _, m := range messages {
		m.ProjectID = newID
		if err = dst.AddMessage(m); err != nil {
			return err
		}
	}
	rows, err := src.db.Query("SELECT body FROM approvals WHERE project_id=?", oldID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b string
		var v Revision
		if err = rows.Scan(&b); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(b), &v); err != nil {
			break
		}
		v.ProjectID = newID
		data, _ := json.Marshal(v)
		if _, err = dst.db.Exec("INSERT INTO approvals(id,project_id,body) VALUES(?,?,?)", v.ID, newID, string(data)); err != nil {
			break
		}
	}
	rowsErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowsErr != nil {
		return rowsErr
	}
	// Preserve receipts only when moving this same portable project. A new copy
	// has its own namespace; its old HTTP responses must not refer to the original.
	if src.Root != "" && oldID == newID {
		rows, err = src.db.Query("SELECT id,fingerprint,status,body,created_at FROM requests")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, hash, body, at string
			var status int
			if err = rows.Scan(&id, &hash, &status, &body, &at); err != nil {
				break
			}
			if _, err = dst.db.Exec("INSERT INTO requests(id,fingerprint,status,body,created_at) VALUES(?,?,?,?,?)", id, hash, status, body, at); err != nil {
				break
			}
		}
		rowsErr = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if rowsErr != nil {
			return rowsErr
		}
	}
	if src.Root != "" {
		if err = copyProjectTree(filepath.Join(src.Root, "docs"), filepath.Join(dst.Root, "docs")); err != nil {
			return err
		}
		rows, err = src.db.Query("SELECT file,hash FROM document_imports")
		if err != nil {
			return err
		}
		for rows.Next() {
			var file, hash string
			if err = rows.Scan(&file, &hash); err != nil {
				break
			}
			if _, err = dst.db.Exec("INSERT INTO document_imports(file,hash) VALUES(?,?)", file, hash); err != nil {
				break
			}
		}
		rowsErr = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if rowsErr != nil {
			return rowsErr
		}
	}
	var integrity string
	if err = dst.db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("Не прошла проверка перенесённой истории")
	}
	return nil
}

func (l *Library) transfer(id, parent, title string, independent bool) (Project, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.transferLocked(id, parent, title, independent, nil)
}
func (l *Library) transferLocked(id, parent, title string, independent bool, source *Server) (Project, error) {
	src := l.main
	old := l.entries[id]
	if source != nil {
		src = source
		old = nil
	} else if old != nil {
		if old.Server == nil || old.Removed {
			return Project{}, errors.New("Сначала откройте исходный проект")
		}
		src = old.Server
	}
	if err := l.idle(src, id); err != nil {
		return Project{}, err
	}
	if err := src.Store.CheckFolder(); err != nil {
		return Project{}, err
	}
	p, err := src.Store.Project(id)
	if err != nil {
		return Project{}, err
	}
	if title != "" {
		p.Title = title
	}
	if independent {
		p.ID = ID()
		p.CreatedAt = now()
	}
	p.UpdatedAt = now()
	parent, err = l.projectParent(parent)
	if err != nil {
		return Project{}, err
	}
	if within(l.main.Store.Dir, parent) {
		return Project{}, errors.New("Выберите расположение вне служебного хранилища Tandem")
	}
	name := folderName(p.Title)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return Project{}, err
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), name) {
			return Project{}, errors.New("Папка назначения уже существует. Выберите другое имя или расположение")
		}
	}
	root := filepath.Join(parent, name)
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		return Project{}, errors.New("Папка назначения уже существует. Выберите другое имя или расположение")
	}
	if within(src.Store.Dir, root) || src.Store.Root != "" && within(src.Store.Root, root) {
		return Project{}, errors.New("Нельзя перенести проект внутрь исходной папки")
	}
	if err = os.Mkdir(root, 0700); err != nil {
		return Project{}, err
	}
	if err = exclusiveWrite(filepath.Join(root, ".tandem-incomplete"), []byte("Перенос не завершён. Исходный проект сохранён.")); err != nil {
		return Project{}, err
	}
	for _, d := range []string{"docs", "materials", "prototypes"} {
		if err = os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			return Project{}, err
		}
	}
	dst, err := OpenStore(filepath.Join(root, ".tandem"))
	if err != nil {
		return Project{}, err
	}
	if err = dst.bindFolder(root); err == nil {
		err = dst.PutProject(p)
	}
	if err == nil {
		err = copyProjectData(src.Store, dst, id, p.ID)
	}
	if err == nil {
		_, err = dst.SyncDocuments(p.ID)
	}
	ce := dst.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return Project{}, fmt.Errorf("Перенос не завершён: %w. Исходные данные сохранены. Незавершённая папка: %s", err, root)
	}
	if err = writeManifest(root, p.ID); err != nil {
		return Project{}, err
	}
	_ = os.Remove(filepath.Join(root, ".tandem-incomplete"))
	server, err := l.openFolder(root, p.ID)
	if err != nil {
		return Project{}, err
	}
	entry := &projectEntry{Project: p, Path: root, Server: server}
	if err = l.save(entry); err != nil {
		l.closeEntry(entry)
		return Project{}, err
	}
	if !independent && old != nil {
		l.closeEntry(old)
	}
	l.entries[p.ID] = entry
	p.Directory, p.FolderState = root, "ready"
	return p, nil
}

func (l *Library) copyFolder(path, parent, title string) (Project, error) {
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
	if e := l.entries[m.ID]; e != nil && e.Server != nil && e.Path == root {
		return l.transferLocked(m.ID, parent, title, true, e.Server)
	}
	server, err := l.openFolder(root, m.ID)
	if err != nil {
		return Project{}, err
	}
	defer l.closeEntry(&projectEntry{Server: server})
	return l.transferLocked(m.ID, parent, title, true, server)
}
