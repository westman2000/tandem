package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const projectFormat = 1
const manifestName = "tandem.project.json"

type projectManifest struct {
	Format int    `json:"format"`
	ID     string `json:"id"`
}

func readManifest(root string) (projectManifest, error) {
	var m projectManifest
	info, err := os.Lstat(filepath.Join(root, manifestName))
	if err != nil {
		if _, incomplete := os.Stat(filepath.Join(root, ".tandem-incomplete")); incomplete == nil {
			return m, errors.New("Создание или перенос этой папки не завершён. В ней могут быть частично записанные файлы; исходный проект сохранён. Для новой попытки выберите другое имя папки")
		}
		return m, errors.New("В папке нет проекта Tandem. Выберите папку с tandem.project.json")
	}
	if !info.Mode().IsRegular() || info.Size() > 8192 {
		return m, errors.New("Некорректный файл проекта")
	}
	b, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		return m, err
	}
	if json.Unmarshal(b, &m) != nil || !validID(m.ID) {
		return m, errors.New("Повреждён файл проекта; восстановите резервную копию")
	}
	if m.Format != projectFormat {
		return m, fmt.Errorf("Формат проекта %d не поддерживается. Обновите Tandem", m.Format)
	}
	return m, nil
}
func validID(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func canonicalDirectory(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = h
		} else {
			path = filepath.Join(h, strings.TrimPrefix(path, "~/"))
		}
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("Выберите папку")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("Папка не найдена или недоступна")
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("Укажите существующую папку")
	}
	return abs, nil
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func folderName(title string) string {
	var b strings.Builder
	for _, c := range title {
		if unicode.IsControl(c) || strings.ContainsRune(`<>:"/\|?*`, c) {
			b.WriteRune('_')
		} else {
			b.WriteRune(c)
		}
	}
	s := strings.Trim(b.String(), " .")
	r := []rune(s)
	if len(r) > 60 {
		s = string(r[:60])
	}
	stem := strings.ToUpper(strings.Split(s, ".")[0])
	if s == "" || stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9' {
		s = "Проект-" + s
	}
	return strings.TrimRight(s, " .")
}
func prepareProjectParent(parent string) (string, error) {
	root, err := canonicalDirectory(parent)
	if err != nil {
		return "", err
	}
	if err := localProjectVolume(root); err != nil {
		return "", err
	}
	for p := root; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, manifestName)); err == nil {
			return "", errors.New("Новый проект нельзя вложить в другой проект Tandem")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return root, nil
}
func exclusiveWrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func writeManifest(root, id string) error {
	b, _ := json.MarshalIndent(projectManifest{projectFormat, id}, "", "  ")
	return exclusiveWrite(filepath.Join(root, manifestName), b)
}
func (s *Store) MaterialsDir() string {
	if s.Root != "" {
		return filepath.Join(s.Root, "materials")
	}
	return filepath.Join(s.Dir, "materials")
}
func (s *Store) CheckFolder() error {
	s.faultMu.RLock()
	fault := s.fault
	s.faultMu.RUnlock()
	if fault != "" {
		return errors.New(fault)
	}
	if s.Root == "" {
		return nil
	}
	root, err := os.Lstat(s.Root)
	if err != nil || !root.IsDir() || !os.SameFile(root, s.rootInfo) {
		return errors.New("Папка проекта перемещена, заменена или недоступна. Укажите её расположение")
	}
	db, err := os.Stat(filepath.Join(s.Dir, "tandem.sqlite"))
	if err != nil || !os.SameFile(db, s.dbInfo) {
		return errors.New("Хранилище проекта недоступно или заменено. Новые действия остановлены")
	}
	for _, p := range []string{s.Dir, s.MaterialsDir(), filepath.Join(s.Root, "docs"), filepath.Join(s.Root, "prototypes"), filepath.Join(s.Dir, "runs"), filepath.Join(s.Dir, "sources")} {
		if err := noProjectLinks(s.Root, p); err != nil {
			return err
		}
	}
	return nil
}
func noProjectLinks(root, path string) error {
	if !within(root, path) {
		return errors.New("Путь выходит за папку проекта")
	}
	rel, _ := filepath.Rel(root, path)
	p := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		p = filepath.Join(p, part)
		i, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Символическая ссылка в рабочем пути: %s", rel)
		}
	}
	return nil
}
func (s *Store) CheckWritable() error {
	if err := s.CheckFolder(); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".write-check-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write([]byte("Tandem checkpoint"))
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}
func (s *Store) bindFolder(root string) error {
	var err error
	s.Root = root
	s.rootInfo, err = os.Stat(root)
	if err != nil {
		return err
	}
	s.dbInfo, err = os.Stat(filepath.Join(s.Dir, "tandem.sqlite"))
	return err
}
func (s *Store) diskRun(r Run) Run {
	if s.Root == "" || r.Prototype == nil {
		return r
	}
	p := *r.Prototype
	r.Prototype = &p
	convert := func(path string) string {
		if filepath.IsAbs(path) && within(s.Root, path) {
			rel, _ := filepath.Rel(s.Root, path)
			return filepath.ToSlash(rel)
		}
		return path
	}
	p.Workspace = convert(p.Workspace)
	p.Config.ParentDirectory = convert(p.Config.ParentDirectory)
	p.Config.SourceRepo = convert(p.Config.SourceRepo)
	return r
}
func (s *Store) liveRun(r Run) Run {
	if s.Root == "" || r.Prototype == nil {
		return r
	}
	p := *r.Prototype
	r.Prototype = &p
	convert := func(path string) string {
		if path != "" && !filepath.IsAbs(path) {
			return filepath.Join(s.Root, filepath.FromSlash(path))
		}
		return path
	}
	p.Workspace = convert(p.Workspace)
	p.Config.ParentDirectory = convert(p.Config.ParentDirectory)
	p.Config.SourceRepo = convert(p.Config.SourceRepo)
	return r
}

// Files are copied only from an idle project. Reject links/special files instead
// of following them out of the project. Dependencies are rebuilt after transfer.
func copyProjectTree(src, dst string) error {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("Ожидалась папка")
	}
	var total int64
	count := 0
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".venv" || d.Name() == "__pycache__") {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Перенос не поддерживает символическую ссылку: %s", rel)
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Специальный файл: %s", rel)
		}
		count++
		total += info.Size()
		if count > 100000 || total > 4<<30 {
			return errors.New("Проект превышает предел переноса: 4 ГБ или 100 000 файлов без зависимостей")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()|0600)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(in, info.Size()+1))
		if err == nil {
			current, statErr := os.Lstat(path)
			if statErr != nil || n != info.Size() || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
				err = fmt.Errorf("Файл изменился во время переноса: %s. Остановите внешние редакторы и повторите перенос", rel)
			}
		}
		if err == nil {
			err = out.Sync()
		}
		ce := out.Close()
		if err != nil {
			return err
		}
		return ce
	})
}

// Do not let externally replaced files exhaust memory or block on special files.
func readRegularLimited(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("Файл должен быть обычным файлом не больше %d байт: %s", limit, filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("Файл увеличился во время чтения; повторите действие")
	}
	return b, nil
}
