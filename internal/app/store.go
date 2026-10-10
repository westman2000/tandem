package app

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	Dir  string
	lock *os.File
}

func ID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(dir, "tandem.lock"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "tandem.sqlite"))
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
	 CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY, body TEXT NOT NULL);
	 CREATE TABLE IF NOT EXISTS runs(id TEXT PRIMARY KEY, project_id TEXT NOT NULL, body TEXT NOT NULL);
	 CREATE INDEX IF NOT EXISTS runs_project ON runs(project_id);
	 CREATE TABLE IF NOT EXISTS materials(id TEXT PRIMARY KEY, project_id TEXT NOT NULL, body TEXT NOT NULL);
	 CREATE TABLE IF NOT EXISTS messages(seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE, project_id TEXT NOT NULL, run_id TEXT NOT NULL, body TEXT NOT NULL);
	 CREATE INDEX IF NOT EXISTS messages_project ON messages(project_id,seq);
	 CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, agent TEXT NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);
	 CREATE INDEX IF NOT EXISTS events_run ON events(run_id,id);`)
	if err != nil {
		db.Close()
		lock.Close()
		return nil, err
	}
	return &Store{db: db, Dir: dir, lock: lock}, nil
}

func (s *Store) Close() error { err := s.db.Close(); s.lock.Close(); return err }
func (s *Store) PutProject(p Project) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO projects(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", p.ID, string(body))
	return err
}
func (s *Store) Project(id string) (Project, error) {
	var p Project
	var body string
	err := s.db.QueryRow("SELECT body FROM projects WHERE id=?", id).Scan(&body)
	if err == nil {
		err = json.Unmarshal([]byte(body), &p)
	}
	return p, err
}
func (s *Store) Projects() ([]Project, error) {
	rows, err := s.db.Query("SELECT body FROM projects ORDER BY json_extract(body,'$.updatedAt') DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var b string
		var p Project
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) PutRun(r Run) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO runs(id,project_id,body) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", r.ID, r.ProjectID, string(b))
	return err
}
func (s *Store) Run(id string) (Run, error) {
	var r Run
	var b string
	err := s.db.QueryRow("SELECT body FROM runs WHERE id=?", id).Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &r)
	}
	return r, err
}
func (s *Store) UpdateRun(id string, update func(*Run)) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Run(id)
	if err != nil {
		return r, err
	}
	update(&r)
	r.UpdatedAt = now()
	return r, s.PutRun(r)
}
func (s *Store) Runs(project string) ([]Run, error) {
	query := "SELECT body FROM runs"
	args := []any{}
	if project != "" {
		query += " WHERE project_id=?"
		args = append(args, project)
	}
	query += " ORDER BY json_extract(body,'$.createdAt') DESC"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var b string
		var r Run
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) AddMessage(m Message) error {
	if m.ID == "" {
		m.ID = ID()
	}
	if m.CreatedAt == "" {
		m.CreatedAt = now()
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO messages(id,project_id,run_id,body) VALUES(?,?,?,?)", m.ID, m.ProjectID, m.RunID, string(b))
	return err
}
func (s *Store) Messages(project string) ([]Message, error) {
	rows, err := s.db.Query("SELECT body FROM messages WHERE project_id=? ORDER BY seq", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var b string
		var m Message
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) AddMaterial(m Material) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO materials(id,project_id,body) VALUES(?,?,?)", m.ID, m.ProjectID, string(b))
	return err
}
func (s *Store) Material(id string) (Material, error) {
	var m Material
	var b string
	err := s.db.QueryRow("SELECT body FROM materials WHERE id=?", id).Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &m)
	}
	return m, err
}
func (s *Store) Materials(project string) ([]Material, error) {
	rows, err := s.db.Query("SELECT body FROM materials WHERE project_id=? ORDER BY rowid", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Material{}
	for rows.Next() {
		var b string
		var m Material
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) DeleteMaterial(id string) error {
	_, err := s.db.Exec("DELETE FROM materials WHERE id=?", id)
	return err
}
func (s *Store) Event(run, agent, kind, text string) error {
	_, err := s.db.Exec("INSERT INTO events(run_id,agent,kind,text,created_at) VALUES(?,?,?,?,?)", run, agent, kind, text, now())
	return err
}
func (s *Store) Events(run string) ([]Event, error) {
	rows, err := s.db.Query("SELECT id,run_id,agent,kind,text,created_at FROM events WHERE run_id=? ORDER BY id DESC LIMIT 500", run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.RunID, &e.Agent, &e.Kind, &e.Text, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CommitTask saves the checkpoint and visible answer in one transaction.
func (s *Store) CommitTask(runID, taskID, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Run(runID)
	if err != nil {
		return err
	}
	var task *Task
	for i := range r.Tasks {
		if r.Tasks[i].ID == taskID {
			task = &r.Tasks[i]
			break
		}
	}
	if task == nil {
		return errors.New("task not found")
	}
	if task.Status == "done" {
		return nil
	}
	task.Status = "done"
	task.Output = answer
	task.FinishedAt = now()
	r.UpdatedAt = now()
	if task.Stage == "draft" || task.Stage == "revision" {
		r.Document = answer
	}
	m := Message{ID: ID(), ProjectID: r.ProjectID, RunID: r.ID, Actor: task.Agent, Target: "all", Text: answer, Stage: task.Stage, Demo: r.Mode == "demo", CreatedAt: now()}
	rb, _ := json.Marshal(r)
	mb, _ := json.Marshal(m)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE runs SET body=? WHERE id=?", string(rb), r.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO messages(id,project_id,run_id,body) VALUES(?,?,?,?)", m.ID, m.ProjectID, m.RunID, string(mb)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Recover() error {
	runs, err := s.Runs("")
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Status == "running" || r.Status == "pausing" {
			_, err = s.UpdateRun(r.ID, func(r *Run) {
				r.Status = "paused"
				r.Error = "Работа прервана перезапуском. Можно продолжить с последнего сохранённого этапа."
				for i := range r.Tasks {
					if r.Tasks[i].Status == "running" {
						r.Tasks[i].Status = "pending"
						delete(r.Sessions, r.Tasks[i].Agent)
					}
				}
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
