package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLibrary(t *testing.T) (*Library, *Server) {
	t.Helper()
	store := testStore(t)
	engine := NewEngine(store, runnerFunc(func(context.Context, AgentInput, func(string, string)) (AgentResult, error) {
		t.Error("unexpected live inference")
		return AgentResult{}, nil
	}))
	engine.Health = NewHealthMonitor(store)
	server := NewServer(store, engine, nil)
	l, err := NewLibrary(server)
	if err != nil {
		t.Fatal(err)
	}
	l.DefaultParent = t.TempDir()
	t.Cleanup(func() { l.Close(); engine.Close() })
	return l, server
}

func TestInterruptedTransferRetainsSourceAndIsRecognized(t *testing.T) {
	l, _ := testLibrary(t)
	p, err := l.create("Source", "")
	if err != nil {
		t.Fatal(err)
	}
	s := l.entries[p.ID].Server.Store
	m, err := s.SaveMaterial(p.ID, "input.md", "requirements", "text/markdown", []byte("source bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.MaterialsDir(), m.ID), []byte("external damage"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = l.transfer(p.ID, l.DefaultParent, "Incomplete", false); err == nil {
		t.Fatal("corrupted material accepted")
	}
	if l.entries[p.ID].Path != p.Directory {
		t.Fatal("registry changed before transfer verified")
	}
	_, err = readManifest(filepath.Join(l.DefaultParent, "Incomplete"))
	if err == nil || !strings.Contains(err.Error(), "не завершён") {
		t.Fatal(err)
	}
	if _, err = s.Project(p.ID); err != nil {
		t.Fatal("source lost", err)
	}
}

func TestFolderActionsRespectCSRFAndPortableAliases(t *testing.T) {
	l, server := testLibrary(t)
	noToken := httptest.NewRequest("GET", "/api/folders", nil)
	noToken.Host = "localhost:4317"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, noToken)
	if w.Code != 403 {
		t.Fatal("folder listing missing token guard", w.Code)
	}
	body, _ := json.Marshal(map[string]string{"title": "Created once", "parent": l.DefaultParent})
	first := libraryRequest(server, "POST", "/api/projects", string(body), "create-folder-receipt-123")
	second := libraryRequest(server, "POST", "/api/projects", string(body), "create-folder-receipt-123")
	if first.Code != 201 || second.Code != 201 || first.Body.String() != second.Body.String() {
		t.Fatal("create was duplicated", first.Code, second.Code)
	}
	var p Project
	_ = json.Unmarshal(first.Body.Bytes(), &p)
	if w := libraryRequest(server, "POST", "/api/projects/"+p.ID+"/forget", "{}", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if ps, _ := l.List(); len(ps) != 0 {
		t.Fatal("project not forgotten")
	}
	if _, err := os.Stat(filepath.Join(p.Directory, manifestName)); err != nil {
		t.Fatal("forget deleted files")
	}
	if _, err := l.open(p.Directory, ""); err != nil {
		t.Fatal("cannot reopen forgotten project", err)
	}
}

func TestMovedFolderCancelsActiveGeneration(t *testing.T) {
	l, server := testLibrary(t)
	started := make(chan struct{})
	server.Engine.Runner = runnerFunc(func(ctx context.Context, _ AgentInput, _ func(string, string)) (AgentResult, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return AgentResult{}, ctx.Err()
	})
	p, err := l.create("Active", "")
	if err != nil {
		t.Fatal(err)
	}
	e := l.entries[p.ID].Server.Engine
	r, err := e.Create(p.ID, "live", "A design", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = l.transfer(p.ID, l.DefaultParent, "Busy copy", true); err == nil {
		t.Fatal("active project copied")
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err = os.Rename(p.Directory, moved); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		e.mu.Lock()
		n := len(e.active)
		e.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider was not cancelled after losing folder")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err = l.open(moved, p.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := l.entries[p.ID].Server.Store.Run(r.ID)
	if err != nil || recovered.Status != "paused" {
		t.Fatal("did not recover to explicit pause", recovered.Status, err)
	}
}
func savedDocument(t *testing.T, s *Store, project, text string) Run {
	t.Helper()
	r := Run{ID: ID(), ProjectID: project, Mode: "live", Status: "completed", CreatedAt: now(), UpdatedAt: now(), Document: text, Models: map[string]string{}, Sessions: map[string]string{}, Materials: []Material{}, MaxRevisions: 5, AvailabilityPolicy: "solo", Tasks: []Task{{ID: ID(), Agent: "codex", Stage: "revision", Status: "done", Output: text, FinishedAt: now()}}}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	return r
}
func libraryRequest(server *Server, method, path, body, receipt string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "localhost:4317"
	r.Header.Set("X-Tandem-Token", server.Token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if receipt != "" {
		r.Header.Set("X-Tandem-Request", receipt)
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}
func TestFolderProjectCreationAndScopedHistory(t *testing.T) {
	l, server := testLibrary(t)
	p, err := l.create("Лесная стая", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Directory != filepath.Join(l.DefaultParent, "Лесная стая") {
		t.Fatal(p)
	}
	child := l.entries[p.ID].Server
	if child.Engine.Health != server.Engine.Health || child.Engine.agentSlots["claude"] != server.Engine.agentSlots["claude"] {
		t.Fatal("subscriptions became per-project")
	}
	if _, err = l.create("лесная стая", ""); err == nil {
		t.Fatal("case collision allowed")
	}
	if _, err = l.create("nested", p.Directory); err == nil {
		t.Fatal("nested project allowed")
	}
	r := savedDocument(t, child.Store, p.ID, "A portable design")
	if err = child.Store.AddMessage(Message{ProjectID: p.ID, RunID: r.ID, Actor: "codex", Text: "saved discussion"}); err != nil {
		t.Fatal(err)
	}
	w := libraryRequest(server, "GET", "/api/projects/"+p.ID, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "saved discussion") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = libraryRequest(server, "GET", "/api/projects/"+p.ID+"/api/runs/"+r.ID+"/document", "", "")
	if w.Code != 200 || w.Body.String() != r.Document {
		t.Fatal(w.Code, w.Body.String())
	}
	vs, _ := child.Store.Revisions(p.ID)
	if _, err = os.Stat(filepath.Join(p.Directory, "docs", vs[0].ID+".md")); err != nil {
		t.Fatal("document not materialized", err)
	}
	if _, err = child.Store.db.Exec("SELECT * FROM project_registry"); err == nil {
		t.Fatal("global registry leaked into project")
	}
}
func TestLegacyTransferSelectsOnlyOwnDataAndKeepsSource(t *testing.T) {
	l, server := testLibrary(t)
	a := testProject(t, server.Store)
	b := testProject(t, server.Store)
	ma, err := server.Store.SaveMaterial(a.ID, "a.md", "requirements", "text/markdown", []byte("material A"))
	if err != nil {
		t.Fatal(err)
	}
	mb, err := server.Store.SaveMaterial(b.ID, "b.md", "requirements", "text/markdown", []byte("material B"))
	if err != nil {
		t.Fatal(err)
	}
	r := savedDocument(t, server.Store, a.ID, "document A")
	r.Materials = []Material{ma}
	if err = server.Store.PutRun(r); err != nil {
		t.Fatal(err)
	}
	if err = server.Store.Snapshot(r); err != nil {
		t.Fatal(err)
	}
	other := savedDocument(t, server.Store, b.ID, "private other document")
	_ = server.Store.Event(r.ID, "codex", "fixture", "old journal")
	vs, _ := server.Store.Revisions(a.ID)
	if _, err = server.Store.ApproveRevision(a.ID, vs[0].ID); err != nil {
		t.Fatal(err)
	}
	p, err := l.transfer(a.ID, l.DefaultParent, "Transferred", false)
	if err != nil {
		t.Fatal(err)
	}
	s := l.entries[p.ID].Server.Store
	if _, err = s.Run(other.ID); err == nil {
		t.Fatal("copied another project's run")
	}
	if _, err = os.Stat(filepath.Join(s.MaterialsDir(), mb.ID)); !os.IsNotExist(err) {
		t.Fatal("copied another project's file")
	}
	if _, err = s.Approval(a.ID, vs[0].ID); err != nil {
		t.Fatal(err)
	}
	if ev, _ := s.events(r.ID, true); len(ev) != 1 || ev[0].Text != "old journal" {
		t.Fatal(ev)
	}
	if _, err = server.Store.Run(r.ID); err != nil {
		t.Fatal("source removed")
	}
	if _, err = os.Stat(filepath.Join(server.Store.MaterialsDir(), ma.ID)); err != nil {
		t.Fatal("source attachment removed")
	}
	ps, _ := l.List()
	if len(ps) != 2 {
		t.Fatal("legacy project duplicated", ps)
	}
	if w := libraryRequest(server, "GET", "/api/runs/"+r.ID+"/document", "", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestCopyAndRelocateKeepHistoryIndependent(t *testing.T) {
	l, server := testLibrary(t)
	p, err := l.create("Original", "")
	if err != nil {
		t.Fatal(err)
	}
	s := l.entries[p.ID].Server.Store
	r := savedDocument(t, s, p.ID, "original")
	copy, err := l.transfer(p.ID, l.DefaultParent, "Copy", true)
	if err != nil {
		t.Fatal(err)
	}
	if copy.ID == p.ID {
		t.Fatal("copy reused identity")
	}
	cs := l.entries[copy.ID].Server.Store
	cr, err := cs.Run(r.ID)
	if err != nil || cr.ProjectID != copy.ID {
		t.Fatal(cr, err)
	}
	cr.Document = "copy only"
	if err = cs.PutRun(cr); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{p.ID: "original", copy.ID: "copy only"} {
		w := libraryRequest(server, "GET", "/api/projects/"+id+"/api/runs/"+r.ID+"/document", "", "")
		if w.Code != 200 || w.Body.String() != want {
			t.Fatal("crossed histories", w.Code, w.Body.String())
		}
	}
	newPath := filepath.Join(t.TempDir(), "moved")
	if err = os.Rename(p.Directory, newPath); err != nil {
		t.Fatal(err)
	}
	if err = s.PutRun(r); err == nil {
		t.Fatal("kept writing through a moved directory")
	}
	ps, _ := l.List()
	for _, v := range ps {
		if v.ID == p.ID && v.FolderState != "unavailable" {
			t.Fatal("missing folder hidden", v)
		}
	}
	p, err = l.open(newPath, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	canonicalNew, _ := canonicalDirectory(newPath)
	if p.Directory != canonicalNew {
		t.Fatal(p)
	}
	l.closeEntry(l.entries[p.ID])
	l.entries[p.ID].Error = "closed fixture"
	p, err = l.open(newPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := l.entries[p.ID].Server.Store.Run(r.ID); v.Document != "original" {
		t.Fatal("lost history", v)
	}
}
func TestFolderValidationLocksAndFutureFormat(t *testing.T) {
	l, _ := testLibrary(t)
	p, err := l.create("Project", "")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := testLibrary(t)
	if _, err = other.open(p.Directory, ""); err == nil {
		t.Fatal("second process allowed to write")
	}
	copyPath := filepath.Join(t.TempDir(), "finder-copy")
	l.closeEntry(l.entries[p.ID])
	if err = copyProjectTree(p.Directory, copyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = l.open(copyPath, ""); err == nil {
		t.Fatal("duplicate identity silently registered")
	}
	p2, err := l.copyFolder(copyPath, l.DefaultParent, "Independent")
	if err != nil {
		t.Fatal(err)
	}
	if p2.ID == p.ID {
		t.Fatal("copy identity")
	}
	before, err := os.ReadFile(filepath.Join(copyPath, ".tandem", "tandem.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(projectManifest{Format: 999, ID: p.ID})
	if err = os.WriteFile(filepath.Join(copyPath, manifestName), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = other.open(copyPath, ""); err == nil {
		t.Fatal("future schema opened")
	}
	after, _ := os.ReadFile(filepath.Join(copyPath, ".tandem", "tandem.sqlite"))
	if textHash(string(before)) != textHash(string(after)) {
		t.Fatal("future DB mutated")
	}
}
func TestExternalDocumentImportPreservesApproval(t *testing.T) {
	l, server := testLibrary(t)
	p, err := l.create("Docs", "")
	if err != nil {
		t.Fatal(err)
	}
	s := l.entries[p.ID].Server.Store
	savedDocument(t, s, p.ID, "approved text")
	vs, _ := s.Revisions(p.ID)
	original, err := s.ApproveRevision(p.ID, vs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SyncDocuments(p.ID); err != nil {
		t.Fatal(err)
	}
	file := vs[0].ID + ".md"
	if err = os.WriteFile(filepath.Join(p.Directory, "docs", file), []byte("manual correction"), 0600); err != nil {
		t.Fatal(err)
	}
	changes, err := s.SyncDocuments(p.ID)
	if err != nil || len(changes) != 1 {
		t.Fatal(changes, err)
	}
	body, _ := json.Marshal(changes[0])
	path := "/api/projects/" + p.ID + "/import-document"
	w := libraryRequest(server, "POST", path, string(body), "external-edit-request-123")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = libraryRequest(server, "POST", path, string(body), "external-edit-request-123")
	if w.Code != 201 {
		t.Fatal("receipt replay", w.Code, w.Body.String())
	}
	var imported Run
	_ = json.Unmarshal(w.Body.Bytes(), &imported)
	journal, err := s.Events(imported.ID)
	if err != nil || len(journal) != 1 || journal[0].Kind != "document_import" {
		t.Fatal("external edit not journaled", journal, err)
	}
	vs, _ = s.Revisions(p.ID)
	if len(vs) != 2 || vs[1].Text != "manual correction" || vs[1].ApprovedAt != "" {
		t.Fatal(vs)
	}
	if a, _ := s.Approval(p.ID, original.ID); a.Text != "approved text" {
		t.Fatal("approval overwritten")
	}
	changes, err = s.SyncDocuments(p.ID)
	if err != nil || len(changes) != 0 {
		t.Fatal("import repeated", changes, err)
	}
}
func TestPortableWorkspacePathsAndWorkingChanges(t *testing.T) {
	l, _ := testLibrary(t)
	p, err := l.create("Code", "")
	if err != nil {
		t.Fatal(err)
	}
	s := l.entries[p.ID].Server.Store
	r := savedDocument(t, s, p.ID, "design")
	r.Kind = "prototype"
	r.Prototype = &Prototype{Workspace: filepath.Join(p.Directory, "prototypes", "tandem-"+r.ID, "workspace"), Config: PrototypeConfig{ParentDirectory: filepath.Join(p.Directory, "prototypes")}}
	if err = os.MkdirAll(r.Prototype.Workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(r.Prototype.Workspace, "main.go"), []byte("uncommitted work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	var raw string
	_ = s.db.QueryRow("SELECT body FROM runs WHERE id=?", r.ID).Scan(&raw)
	if strings.Contains(raw, p.Directory) {
		t.Fatal("absolute execution path stored", raw)
	}
	p2, err := l.transfer(p.ID, l.DefaultParent, "Moved code", false)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.entries[p2.ID].Server.Store.Run(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !within(p2.Directory, r2.Prototype.Workspace) {
		t.Fatal(r2.Prototype.Workspace)
	}
	b, err := os.ReadFile(filepath.Join(r2.Prototype.Workspace, "main.go"))
	if err != nil || string(b) != "uncommitted work" {
		t.Fatal("working changes lost", err)
	}
}

func TestLegacyReceiptsSurviveMigration(t *testing.T) {
	l, server := testLibrary(t)
	p := testProject(t, server.Store)
	if w := libraryRequest(server, "POST", "/api/projects/"+p.ID+"/forget", "{}", ""); w.Code != 409 {
		t.Fatal("legacy project hidden without reopening path", w.Code)
	}
	r := savedDocument(t, server.Store, p.ID, "Saved text")
	r.Status = "paused"
	if err := server.Store.PutRun(r); err != nil {
		t.Fatal(err)
	}
	path := "/api/projects/" + p.ID + "/messages"
	b, _ := json.Marshal(map[string]string{"text": "A clarification", "target": "all", "runId": r.ID})
	body := string(b)
	first := libraryRequest(server, "POST", path, body, "legacy-message-receipt-123")
	if first.Code != 201 {
		t.Fatal(first.Code, first.Body.String())
	}
	if _, err := l.transfer(p.ID, l.DefaultParent, "Migrated", false); err != nil {
		t.Fatal(err)
	}
	second := libraryRequest(server, "POST", path, body, "legacy-message-receipt-123")
	if second.Code != first.Code || second.Body.String() != first.Body.String() || second.Header().Get("X-Tandem-Replayed") != "true" {
		t.Fatal("lost legacy response replayed after migration", second.Code, second.Body.String())
	}
	messages, err := l.entries[p.ID].Server.Store.Messages(p.ID)
	if err != nil || len(messages) != 1 {
		t.Fatal("duplicate message", messages, err)
	}
}

func TestPortablePrototypeRetainsPinnedSourceAfterOriginalDisappears(t *testing.T) {
	l, _ := testLibrary(t)
	p, err := l.create("Pinned source", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.entries[p.ID].Server
	approval := fixtureApproval(t, child.Store, p)
	plan := fixturePlan(t)
	child.Engine.Health = nil
	child.Engine.Runner = runnerFunc(func(_ context.Context, in AgentInput, _ func(string, string)) (AgentResult, error) {
		answer := "Plan"
		if in.Stage == "prototype_plan_final" {
			answer = encoded(plan)
		}
		return AgentResult{Text: answer}, nil
	})
	source := t.TempDir()
	ctx := context.Background()
	if _, err = git(ctx, source, "init"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "original.txt"), []byte("PINNED SOURCE"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = git(ctx, source, "add", "original.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err = git(ctx, source, "commit", "-m", "original"); err != nil {
		t.Fatal(err)
	}
	r, err := child.Engine.CreatePrototype(p.ID, PrototypeConfig{RevisionID: approval.ID, SourceRepo: source})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitRun(t, child.Store, r.ID, "awaiting_plan")
	waitInactive(t, child.Engine, r.ID)
	if !within(p.Directory, r.Prototype.Config.SourceRepo) {
		t.Fatal("source not pinned in project")
	}
	if err = os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	moved, err := l.transfer(p.ID, l.DefaultParent, "Moved pinned source", false)
	if err != nil {
		t.Fatal(err)
	}
	store := l.entries[moved.ID].Server.Store
	r, err = store.Run(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepareWorkspace(ctx, store, r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.Prototype.Workspace, "original.txt"))
	if err != nil || string(b) != "PINNED SOURCE" {
		t.Fatal("pinned source unavailable after transfer", err)
	}
}

func TestDocumentExportFailurePreservesCompletedTask(t *testing.T) {
	l, _ := testLibrary(t)
	p, err := l.create("Export failure", "")
	if err != nil {
		t.Fatal(err)
	}
	child := l.entries[p.ID].Server
	original := savedDocument(t, child.Store, p.ID, "old revision")
	if _, err = child.Store.SyncDocuments(p.ID); err != nil {
		t.Fatal(err)
	}
	vs, _ := child.Store.Revisions(p.ID)
	file := filepath.Join(p.Directory, "docs", vs[0].ID+".md")
	if err = os.WriteFile(file, []byte(strings.Repeat("x", 600001)), 0600); err != nil {
		t.Fatal(err)
	}
	r := original
	r.ID = ID()
	r.Status = "running"
	r.Tasks = []Task{{ID: ID(), Agent: "codex", Stage: "revision", Status: "pending"}}
	if err = child.Store.PutRun(r); err != nil {
		t.Fatal(err)
	}
	child.Engine.Runner = runnerFunc(func(context.Context, AgentInput, func(string, string)) (AgentResult, error) {
		return AgentResult{Text: "saved answer"}, nil
	})
	if err = child.Engine.performOnce(context.Background(), r, r.Tasks[0]); err != nil {
		t.Fatal("committed task reported as failed", err)
	}
	got, err := child.Store.Run(r.ID)
	if err != nil || got.Document != "saved answer" || got.Tasks[0].Status != "done" {
		t.Fatal("completed checkpoint lost", got, err)
	}
	events, _ := child.Store.Events(r.ID)
	if len(events) == 0 || events[0].Kind != "document_export" {
		t.Fatal("export error not journaled", events)
	}
}

func TestDefaultParentCreationDoesNotRecreateMissingCustomDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	l, _ := testLibrary(t)
	l.DefaultParent = filepath.Join(home, "Tandem", "Projects")
	p, err := l.create("First project", l.DefaultParent)
	if err != nil {
		t.Fatal("visible default path not created", err)
	}
	if _, err = os.Stat(p.Directory); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "missing-disk", "projects")
	l.DefaultParent = missing
	if _, err = l.create("Do not recreate", ""); err == nil {
		t.Fatal("missing custom location recreated")
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created replacement of missing custom disk", err)
	}
}
