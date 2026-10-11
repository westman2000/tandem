package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"unicode/utf8"
)

type DocumentChange struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

func (s *Store) SyncDocuments(project string) ([]DocumentChange, error) {
	changes := []DocumentChange{}
	if s.Root == "" {
		return changes, nil
	}
	s.docMu.Lock()
	defer s.docMu.Unlock()
	if err := s.CheckFolder(); err != nil {
		return nil, err
	}
	vs, err := s.Revisions(project)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		name := v.ID + ".md"
		path, err := safeProjectPath(s.Root, "docs/"+name)
		if err != nil {
			return nil, err
		}
		b, err := readRegularLimited(path, 600000)
		if os.IsNotExist(err) {
			if err = exclusiveWrite(path, []byte(v.Text)); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		hash := textHash(string(b))
		if hash == v.SHA256 {
			continue
		}
		var accepted string
		_ = s.db.QueryRow("SELECT hash FROM document_imports WHERE file=?", name).Scan(&accepted)
		if hash != accepted {
			changes = append(changes, DocumentChange{File: name, SHA256: hash})
		}
	}
	return changes, nil
}
func (s *Store) ImportDocument(project, file, hash string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Root == "" {
		return Run{}, errors.New("Сначала перенесите проект в отдельную папку")
	}
	if err := s.CheckFolder(); err != nil {
		return Run{}, err
	}
	vs, err := s.Revisions(project)
	if err != nil {
		return Run{}, err
	}
	var source *Revision
	for i := range vs {
		if vs[i].ID+".md" == file {
			source = &vs[i]
			break
		}
	}
	if source == nil {
		return Run{}, errors.New("Редакция не найдена")
	}
	path, err := safeProjectPath(s.Root, filepath.ToSlash(filepath.Join("docs", file)))
	if err != nil {
		return Run{}, err
	}

	b, err := readRegularLimited(path, 600000)
	if err != nil {
		return Run{}, err
	}
	if textHash(string(b)) != hash {
		return Run{}, errors.New("Файл снова изменился. Обновите список перед импортом")
	}
	if !utf8.Valid(b) || len(b) == 0 {
		return Run{}, errors.New("Нужен непустой Markdown-файл в UTF-8")
	}
	var accepted string
	_ = s.db.QueryRow("SELECT hash FROM document_imports WHERE file=?", file).Scan(&accepted)
	if accepted == hash || hash == source.SHA256 {
		return Run{}, errors.New("Эта правка уже учтена")
	}
	parent, err := s.Run(source.RunID)
	if err != nil {
		return Run{}, err
	}
	r := Run{ID: ID(), ProjectID: project, Mode: parent.Mode, Status: "completed", Title: parent.Title, Brief: "Правка документа во внешнем редакторе", ParentRunID: parent.ID, CreatedAt: now(), UpdatedAt: now(), Document: string(b), Models: parent.Models, LockedModels: parent.LockedModels, Sessions: map[string]string{}, Materials: parent.Materials, MaxRevisions: parent.MaxRevisions, Acceptance: parent.Acceptance, AvailabilityPolicy: parent.AvailabilityPolicy, Quality: "pending", NeedsReview: true}
	r.Tasks = []Task{{ID: ID(), Agent: "user", Stage: "revision", Status: "done", Output: string(b), FinishedAt: now()}}
	// Run + accepted file hash form one transaction, so a retry cannot import it twice.
	if err = s.Snapshot(r); err != nil {
		return Run{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	rb, _ := json.Marshal(r)
	if _, err = tx.Exec("INSERT INTO runs(id,project_id,body) VALUES(?,?,?)", r.ID, project, string(rb)); err != nil {
		return Run{}, err
	}
	if _, err = tx.Exec("INSERT INTO document_imports(file,hash) VALUES(?,?) ON CONFLICT(file) DO UPDATE SET hash=excluded.hash", file, hash); err != nil {
		return Run{}, err
	}
	m := Message{ID: ID(), ProjectID: project, RunID: r.ID, Actor: "user", Target: "all", Stage: "revision", Text: "Импортирована внешняя правка документа: " + file, CreatedAt: r.CreatedAt}
	mb, _ := json.Marshal(m)
	if _, err = tx.Exec("INSERT INTO messages(id,project_id,run_id,body) VALUES(?,?,?,?)", m.ID, project, r.ID, string(mb)); err != nil {
		return Run{}, err
	}
	if _, err = tx.Exec("INSERT INTO events(run_id,agent,kind,text,created_at) VALUES(?,?,?,?,?)", r.ID, "user", "document_import", m.Text, r.CreatedAt); err != nil {
		return Run{}, err
	}
	if err = tx.Commit(); err != nil {
		return Run{}, err
	}
	return r, nil
}
