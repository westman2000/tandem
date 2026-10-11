package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

func gitEnv() []string {
	env := []string{}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if !strings.HasPrefix(k, "GIT_") {
			env = append(env, v)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}
func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	base := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgSign=false", "-c", "user.name=Tandem", "-c", "user.email=tandem@localhost"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	isolateProcess(cmd)
	cmd.WaitDelay = 3 * time.Second
	var b cappedBuffer
	b.limit = 2 << 20
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	if err != nil {
		return b.String(), fmt.Errorf("Git %s: %w: %s", args[0], err, b.String())
	}
	return strings.TrimRight(b.String(), "\r\n"), nil
}
func localRepository(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	root, err := git(context.Background(), abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil || root != abs {
		return "", errors.New("Укажите корень локального Git-репозитория")
	}
	status, err := git(context.Background(), abs, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", errors.New("В исходном репозитории есть несохранённые изменения. Сначала сохраните их в Git; Tandem возьмёт отдельную копию")
	}
	if _, err = git(context.Background(), abs, "rev-parse", "HEAD"); err != nil {
		return "", errors.New("В исходном репозитории нужен хотя бы один коммит")
	}
	return abs, nil
}
func prepareWorkspace(ctx context.Context, s *Store, r Run) error {
	p := r.Prototype
	root := filepath.Dir(p.Workspace)
	owner := filepath.Join(root, "owner")
	if b, err := os.ReadFile(owner); err == nil {
		if string(b) != r.ID {
			return errors.New("Рабочая папка принадлежит другому циклу")
		}
	} else {
		if err = os.Mkdir(root, 0700); err != nil {
			return fmt.Errorf("Создание отдельной папки: %w", err)
		}
		if err = os.WriteFile(owner, []byte(r.ID), 0600); err != nil {
			return err
		}
	}
	if p.Config.SourceRepo != "" {
		if info, err := os.Stat(filepath.Join(p.Workspace, ".git")); err != nil || !info.IsDir() {
			if _, statErr := os.Stat(p.Workspace); statErr == nil {
				return errors.New("Копирование репозитория было прервано. Сохраните нужные файлы и удалите незавершённую рабочую копию перед повтором: " + p.Workspace)
			}
			source, err := localRepository(p.Config.SourceRepo)
			if err != nil {
				return err
			}
			if _, err = git(ctx, root, "clone", "--no-hardlinks", "--", source, p.Workspace); err != nil {
				return err
			}
		}
		remotes, err := git(ctx, p.Workspace, "remote")
		if err != nil {
			return err
		}
		for _, name := range strings.Fields(remotes) {
			if name == "origin" {
				if _, err = git(ctx, p.Workspace, "remote", "remove", "origin"); err != nil {
					return err
				}
			}
		}
	} else {
		if err := os.MkdirAll(p.Workspace, 0700); err != nil {
			return err
		}
		if info, err := os.Stat(filepath.Join(p.Workspace, ".git")); err != nil || !info.IsDir() {
			if _, err = git(ctx, p.Workspace, "init"); err != nil {
				return err
			}
		}
		if _, err := git(ctx, p.Workspace, "rev-parse", "HEAD"); err != nil {
			if _, err = git(ctx, p.Workspace, "commit", "--allow-empty", "-m", "Tandem prototype baseline"); err != nil {
				return err
			}
		}
	}
	top, err := git(ctx, p.Workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(p.Workspace)
	if err != nil || top != expected {
		return errors.New("Рабочая папка не является отдельным Git-репозиторием")
	}
	branch, err := git(ctx, p.Workspace, "branch", "--show-current")
	if err != nil {
		return err
	}
	if branch == "tandem/"+r.ID {
		return nil
	}
	args := []string{"checkout", "-b", "tandem/" + r.ID}
	if p.SourceCommit != "" {
		args = append(args, p.SourceCommit)
	}
	_, err = git(ctx, p.Workspace, args...)
	return err
}

func safeProjectPath(root, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\:\x00\r\n") || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", errors.New("Недопустимый путь файла")
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "", fmt.Errorf("Недопустимый путь: %s", name)
		}
	}
	path := root
	for _, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular())) {
			return "", fmt.Errorf("Ссылки и специальные файлы не поддерживаются: %s", name)
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return path, nil
}
func privateSource(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return base == ".env" || strings.HasPrefix(base, ".env.") && base != ".env.example" || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || base == "credentials.json" || base == "auth.json"
}
func workspaceText(root string) (string, error) {
	raw, err := git(context.Background(), root, "ls-files", "-z")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	count := 0
	for _, name := range strings.Split(raw, "\x00") {
		if name == "" || privateSource(name) {
			continue
		}
		path, err := safeProjectPath(root, name)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > 512<<10 {
			fmt.Fprintf(&b, "\nФайл %s: пропущен, больше 512 КБ\n", name)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
			fmt.Fprintf(&b, "\nДвоичный файл %s\n", name)
			continue
		}
		count++
		if count > 300 || b.Len()+len(data) > 2<<20 {
			return "", errors.New("Исходники превышают контекст первого прототипа: до 300 текстовых файлов и 2 МБ. Выберите небольшой отдельный проект")
		}
		fmt.Fprintf(&b, "\n--- Файл %s ---\n%s\n--- Конец файла ---\n", name, data)
	}
	return b.String(), nil
}
func validateProposal(p CodeProposal) error {
	if strings.TrimSpace(p.Summary) == "" || len(p.Files) > 100 {
		return errors.New("Автор должен вернуть описание и не более 100 изменений файлов")
	}
	seen := map[string]bool{}
	size := 0
	for _, f := range p.Files {
		name := strings.ToLower(f.Path)
		if seen[name] {
			return errors.New("Повторяющийся путь файла")
		}
		seen[name] = true
		if _, err := safeProjectPath("", f.Path); err != nil {
			return err
		}
		if privateSource(f.Path) {
			return errors.New("Секреты не должны входить в предлагаемые исходники")
		}
		size += len(f.Content)
		if len(f.Content) > 1<<20 || size > 3<<20 || strings.ContainsRune(f.Content, 0) {
			return errors.New("Слишком большой или двоичный набор изменений")
		}
	}
	return nil
}
func applyProposal(ctx context.Context, r Run, t Task, proposal CodeProposal) (Build, error) {
	p := r.Prototype
	w := p.Workspace
	b := Build{ID: t.ID, TaskID: t.ID, Number: len(p.Builds) + 1, CreatedAt: now(), Summary: proposal.Summary, Limitations: proposal.Limitations, Checks: []CheckResult{}}
	if err := validateProposal(proposal); err != nil {
		return b, err
	}
	head, err := git(ctx, w, "rev-parse", "HEAD")
	if err != nil {
		return b, err
	}
	subject, _ := git(ctx, w, "log", "-1", "--format=%s")
	committed := subject == "Tandem build "+t.ID
	if !committed && head != p.CurrentCommit {
		return b, errors.New("Git checkpoint изменился вне Tandem. Проверьте рабочую папку")
	}
	paths := []string{}
	for _, f := range proposal.Files {
		path, err := safeProjectPath(w, f.Path)
		if err != nil {
			return b, err
		}
		paths = append(paths, path)
	}
	// A restarted write may leave some of the exact proposed contents on disk.
	// Any other modification belongs to the user and must not be overwritten.
	status, err := git(ctx, w, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return b, err
	}
	for _, line := range strings.Split(status, "\x00") {
		if len(line) < 4 {
			continue
		}
		name := line[3:]
		matched := false
		for i, f := range proposal.Files {
			if f.Path == name {
				data, readErr := os.ReadFile(paths[i])
				matched = f.Delete && os.IsNotExist(readErr) || !f.Delete && readErr == nil && string(data) == f.Content
				break
			}
		}
		if !matched {
			return b, fmt.Errorf("В рабочей папке есть посторонние изменения (%s). Сохраните их отдельно перед продолжением", name)
		}
	}
	if !committed {
		for i, f := range proposal.Files {
			if f.Delete {
				if err = os.Remove(paths[i]); err != nil && !os.IsNotExist(err) {
					return b, err
				}
			} else {
				if err = os.MkdirAll(filepath.Dir(paths[i]), 0700); err != nil {
					return b, err
				}
				if err = os.WriteFile(paths[i], []byte(f.Content), 0644); err != nil {
					return b, err
				}
			}
		}
		for _, f := range proposal.Files {
			// git add -A handles deletion; absent never-tracked deletions need no stage.
			if _, err = os.Stat(filepath.Join(w, filepath.FromSlash(f.Path))); os.IsNotExist(err) {
				tracked, _ := git(ctx, w, "ls-files", "--", f.Path)
				if tracked == "" {
					continue
				}
			}
			if _, err = git(ctx, w, "add", "-A", "--", f.Path); err != nil {
				return b, err
			}
		}
		if _, err = git(ctx, w, "commit", "--allow-empty", "-m", "Tandem build "+t.ID); err != nil {
			return b, err
		}
		head, err = git(ctx, w, "rev-parse", "HEAD")
		if err != nil {
			return b, err
		}
	}
	b.Commit = head
	b.Diff, err = git(ctx, w, "diff", "HEAD^", "HEAD", "--stat", "--patch")
	return b, err
}
func toolChecks(plan *PrototypePlan) []ToolCheck {
	names := []string{"git"}
	if plan != nil {
		names = append(names, plan.Tools...)
		for _, c := range plan.Checks {
			if len(c.Args) > 0 && !strings.HasPrefix(c.Args[0], ".") {
				names = append(names, c.Args[0])
			}
		}
	}
	out := []ToolCheck{{Name: "ОС", Available: true, Detail: runtime.GOOS + "/" + runtime.GOARCH}}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		path, err := exec.LookPath(name)
		detail := path
		if err != nil {
			detail = "Не найден в PATH сервера"
		}
		out = append(out, ToolCheck{Name: name, Available: err == nil, Detail: detail})
	}
	if plan != nil && plan.Platform == "ios" && runtime.GOOS != "darwin" {
		out = append(out, ToolCheck{Name: "Сборка iOS", Detail: "Требуется macOS с Xcode"})
	}
	return out
}
func missingTools(tools []ToolCheck) bool {
	for _, t := range tools {
		if !t.Available {
			return true
		}
	}
	return false
}
func parseObject(text string, target any) error {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		_, body, ok := strings.Cut(text, "\n")
		if !ok {
			return errors.New("Ожидался JSON")
		}
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("Некорректный структурированный ответ агента: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("Лишние данные после JSON")
	}
	return nil
}
