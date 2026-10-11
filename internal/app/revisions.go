package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

type Revision struct {
	ID         string `json:"id"`
	RunID      string `json:"runId"`
	ProjectID  string `json:"projectId"`
	Number     int    `json:"number"`
	Stage      string `json:"stage"`
	Text       string `json:"text"`
	SHA256     string `json:"sha256"`
	CreatedAt  string `json:"createdAt"`
	ApprovedAt string `json:"approvedAt,omitempty"`
	Demo       bool   `json:"demo"`
}

func textHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (s *Store) Revisions(project string) ([]Revision, error) {
	runs, err := s.Runs(project)
	if err != nil {
		return nil, err
	}
	out := []Revision{}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.Prototype != nil {
			continue
		}
		found := false
		for _, t := range r.Tasks {
			if t.Status != "done" || (t.Stage != "draft" && t.Stage != "revision") || t.Output == "" {
				continue
			}
			found = true
			at := t.FinishedAt
			if at == "" {
				at = r.CreatedAt
			}
			out = append(out, Revision{ID: r.ID + "-" + t.ID, RunID: r.ID, ProjectID: project, Stage: t.Stage, Text: t.Output, SHA256: textHash(t.Output), CreatedAt: at, Demo: r.Mode == "demo"})
		}
		if !found && r.Document != "" && r.ParentRunID == "" {
			out = append(out, Revision{ID: r.ID + "-document", RunID: r.ID, ProjectID: project, Stage: "revision", Text: r.Document, SHA256: textHash(r.Document), CreatedAt: r.CreatedAt, Demo: r.Mode == "demo"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	for i := range out {
		out[i].Number = i + 1
		var b string
		err = s.db.QueryRow("SELECT body FROM approvals WHERE id=? AND project_id=?", out[i].ID, project).Scan(&b)
		if err == nil {
			var a Revision
			if json.Unmarshal([]byte(b), &a) == nil && a.SHA256 == out[i].SHA256 {
				out[i].ApprovedAt = a.ApprovedAt
			}
		}
	}
	return out, nil
}
func (s *Store) ApproveRevision(project, id string) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revisions, err := s.Revisions(project)
	if err != nil {
		return Revision{}, err
	}
	for _, v := range revisions {
		if v.ID == id {
			if v.Demo {
				return v, errors.New("Демонстрационный документ нельзя утвердить для реальной разработки")
			}
			if v.ApprovedAt != "" {
				return v, nil
			}
			v.ApprovedAt = now()
			b, _ := json.Marshal(v)
			_, err = s.db.Exec("INSERT INTO approvals(id,project_id,body) VALUES(?,?,?)", v.ID, project, string(b))
			return v, err
		}
	}
	return Revision{}, errors.New("Редакция не найдена")
}
func (s *Store) Approval(project, id string) (Revision, error) {
	var b string
	var v Revision
	err := s.db.QueryRow("SELECT body FROM approvals WHERE id=? AND project_id=?", id, project).Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &v)
	}
	if err != nil {
		return v, errors.New("Сначала утвердите выбранную редакцию документа")
	}
	return v, nil
}
