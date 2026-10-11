package app

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Store) previewPath(run, build string) string {
	return filepath.Join(s.Dir, "runs", run, "previews", build)
}
func (s *Store) capturePreview(r Run, buildID string) error {
	p := r.Prototype
	if p.Plan.Platform != "browser" {
		return nil
	}
	directory := p.Plan.PreviewDirectory
	if directory == "" {
		directory = "."
	}
	source := p.Workspace
	if directory != "." {
		var err error
		source, err = safeProjectPath(p.Workspace, directory)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(source, "index.html")); err != nil {
		return errors.New("Для локального просмотра не найден index.html в " + directory)
	}
	destination := s.previewPath(r.ID, buildID)
	if _, err := os.Stat(filepath.Join(destination, "index.html")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), "preview-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var size int64
	count := 0
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if privateSource(rel) {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("Предпросмотр не включает символические ссылки: " + rel)
		}
		target := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		size += info.Size()
		count++
		if size > 32<<20 || count > 4000 {
			return errors.New("Локальный просмотр ограничен 32 МБ и 4000 файлами")
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(dst, io.LimitReader(src, info.Size()+1))
		closeErr := dst.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	return os.Rename(tmp, destination)
}
func previewHandler(root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) || (r.Method != "GET" && r.Method != "HEAD") {
			http.Error(w, "Local preview only", 403)
			return
		}
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-modals allow-pointer-lock; default-src 'self' data: blob:; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		path, err := safeProjectPath(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		file, err := os.Open(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		http.ServeContent(w, r, filepath.Base(name), info.ModTime(), file)
	})
}
func (e *Engine) Preview(id, buildID string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return "", errors.New("Сервер завершается")
	}
	r, err := e.Store.Run(id)
	if err != nil || r.Prototype == nil {
		return "", errors.New("Прототип не найден")
	}
	found := false
	for _, b := range r.Prototype.Builds {
		if b.ID == buildID && b.PreviewReady {
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("У этой сборки нет сохранённого браузерного просмотра")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	if e.preview != nil {
		_ = e.preview.Close()
	}
	e.preview = &http.Server{Handler: previewHandler(e.Store.previewPath(id, buildID)), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	server := e.preview
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String() + "/", nil
}
