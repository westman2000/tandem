package app

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
)

var requestID = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,100}$`)

// A durable receipt prevents duplicate mutations after a lost HTTP response.
// An interrupted pending receipt is never replayed blindly: side effects may
// already exist. The client must reconcile the visible project state first.
func (s *Server) mutate(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Tandem-Request")
	if id == "" {
		s.api(w, r)
		return
	}
	if !requestID.MatchString(id) {
		problem(w, 400, errors.New("Некорректный ID действия"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
	if err != nil {
		problem(w, 413, errors.New("Слишком большой запрос"))
		return
	}
	sum := sha256.Sum256(append([]byte(r.Method+"\n"+r.URL.RequestURI()+"\n"), body...))
	fingerprint := hex.EncodeToString(sum[:])
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		clone := r.Clone(r.Context())
		clone.Body = io.NopCloser(bytes.NewReader(body))
		if err = clone.ParseMultipartForm(20 << 20); err != nil {
			problem(w, 400, errors.New("Некорректные вложения"))
			return
		}
		defer clone.MultipartForm.RemoveAll()
		entries := []any{r.Method, r.URL.RequestURI()}
		keys := []string{}
		for key := range clone.MultipartForm.Value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			entries = append(entries, []any{key, clone.MultipartForm.Value[key]})
		}
		keys = nil
		for key := range clone.MultipartForm.File {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			for _, header := range clone.MultipartForm.File[key] {
				file, openErr := header.Open()
				if openErr != nil {
					problem(w, 400, openErr)
					return
				}
				hash := sha256.New()
				_, copyErr := io.Copy(hash, file)
				file.Close()
				if copyErr != nil {
					problem(w, 400, copyErr)
					return
				}
				entries = append(entries, []any{key, header.Filename, header.Header.Get("Content-Type"), hex.EncodeToString(hash.Sum(nil))})
			}
		}
		encoded, _ := json.Marshal(entries)
		hash := sha256.Sum256(encoded)
		fingerprint = hex.EncodeToString(hash[:])
	}
	result, err := s.Store.db.Exec("INSERT INTO requests(id,fingerprint,created_at) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING", id, fingerprint, now())
	if err != nil {
		problem(w, 500, err)
		return
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		var hash, saved string
		var status int
		err = s.Store.db.QueryRow("SELECT fingerprint,status,body FROM requests WHERE id=?", id).Scan(&hash, &status, &saved)
		if err != nil && err != sql.ErrNoRows {
			problem(w, 500, err)
			return
		}
		if hash != fingerprint {
			problem(w, 409, errors.New("Это действие уже отправлялось с другими данными. Обновите состояние проекта"))
			return
		}
		if status <= 0 {
			if status == -1 {
				w.Header().Set("X-Tandem-Uncertain", "true")
			}
			w.Header().Set("X-Tandem-Pending", "true")
			problem(w, 409, errors.New("Результат предыдущей отправки ещё не подтверждён. Обновите проект и проверьте историю: действие могло выполниться. Повтор автоматически не запускается"))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Tandem-Replayed", "true")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, saved)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	record := httptest.NewRecorder()
	s.api(record, r)
	if record.Code >= 500 {
		// Preserve the uncertain receipt, since a handler may have committed part of
		// its result before failing to persist the final response.
		_, _ = s.Store.db.Exec("UPDATE requests SET status=-1 WHERE id=?", id)
		w.Header().Set("X-Tandem-Pending", "true")
		w.Header().Set("X-Tandem-Uncertain", "true")
	} else {
		_, err = s.Store.db.Exec("UPDATE requests SET status=?,body=? WHERE id=?", record.Code, record.Body.String(), id)
		if err != nil {
			w.Header().Set("X-Tandem-Pending", "true")
			problem(w, 500, errors.New("Действие выполнено, но подтверждение не сохранено. Проверьте состояние проекта"))
			return
		}
	}
	for k, values := range record.Header() {
		w.Header()[k] = values
	}
	w.WriteHeader(record.Code)
	_, _ = w.Write(record.Body.Bytes())
}
