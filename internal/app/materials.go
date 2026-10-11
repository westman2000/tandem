package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxFileSize = 12 << 20
const MaxTextSize = 300_000
const MaxMaterials = 20

func extractText(name string, data []byte) (string, string, error) {
	ext := strings.ToLower(filepath.Ext(name))
	var value string
	switch ext {
	case ".docx":
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return "", "", errors.New("Повреждённый DOCX")
		}
		found := false
		for _, f := range z.File {
			if f.Name == "word/document.xml" {
				found = true
				if f.UncompressedSize64 > 24<<20 {
					return "", "", errors.New("Содержимое DOCX слишком велико")
				}
				r, err := f.Open()
				if err != nil {
					return "", "", err
				}
				defer r.Close()
				d := xml.NewDecoder(io.LimitReader(r, 24<<20))
				var b strings.Builder
				inText := false
				for {
					token, err := d.Token()
					if err == io.EOF {
						break
					}
					if err != nil {
						return "", "", errors.New("Не удалось прочитать XML документа")
					}
					switch t := token.(type) {
					case xml.StartElement:
						if t.Name.Local == "t" {
							inText = true
						}
						if t.Name.Local == "tab" {
							b.WriteString("\t")
						}
						if t.Name.Local == "br" {
							b.WriteString("\n")
						}
					case xml.CharData:
						if inText {
							b.Write(t)
						}
					case xml.EndElement:
						if t.Name.Local == "t" {
							inText = false
						}
						if t.Name.Local == "p" {
							b.WriteString("\n")
						}
					}
				}
				value = b.String()
				break
			}
		}
		if !found {
			return "", "", errors.New("В DOCX нет основного текста")
		}
	case ".pdf":
		bin, err := exec.LookPath("pdftotext")
		if err != nil {
			return "", "", errors.New("Для PDF нужен pdftotext (brew install poppler). Пока можно загрузить DOCX, Markdown или TXT")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-enc", "UTF-8", "-", "-")
		cmd.Stdin = bytes.NewReader(data)
		var out cappedBuffer
		out.limit = 4 << 20
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", "", errors.New("Не удалось извлечь текст PDF; сканированные страницы требуют OCR")
		}
		value = out.String()
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".yaml", ".yml", ".xml", ".html", ".css", ".js", ".jsx", ".ts", ".tsx", ".go", ".py", ".rs", ".java", ".kt", ".swift", ".c", ".cpp", ".h", ".sql", ".toml", ".ini", ".log":
		if !utf8.Valid(data) || bytes.Contains(data, []byte{0}) {
			return "", "", errors.New("Нужен текстовый файл в UTF-8")
		}
		value = string(data)
	default:
		return "", "", errors.New("Поддерживаются DOCX, PDF с текстовым слоем и текстовые файлы (MD, TXT, код, JSON, CSV)")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", errors.New("В файле не найден текст. Для изображений и сканов нужен OCR")
	}
	warning := ""
	if ext == ".docx" {
		warning = "Извлечён основной текст. Изображения, комментарии, колонтитулы и форматирование не передаются."
	}
	if ext == ".pdf" {
		warning = "Извлечён текстовый слой. Изображения и макет страниц не передаются."
	}
	if len(value) > MaxTextSize {
		return "", "", fmt.Errorf("В файле больше %d байт текста. Разделите его на части", MaxTextSize)
	}
	return value, warning, nil
}

func (s *Store) SaveMaterial(project, name, role, mime string, data []byte) (Material, error) {
	if _, err := s.Project(project); err != nil {
		return Material{}, err
	}
	existing, err := s.Materials(project)
	if err != nil {
		return Material{}, err
	}
	if len(existing) >= MaxMaterials {
		return Material{}, errors.New("Максимум 20 материалов на проект")
	}
	if len(data) == 0 || len(data) > MaxFileSize {
		return Material{}, errors.New("Размер файла должен быть от 1 байта до 12 МБ")
	}
	switch role {
	case "requirements", "draft", "reference", "inspiration":
	default:
		return Material{}, errors.New("Неизвестная роль материала")
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if len(name) > 240 {
		return Material{}, errors.New("Слишком длинное имя файла")
	}
	value, warning, err := extractText(name, data)
	if err != nil {
		return Material{}, err
	}
	total := len(value)
	for _, m := range existing {
		total += len(m.Text)
	}
	if total > 600_000 {
		return Material{}, errors.New("Материалы проекта превышают 600 КБ текста. Уменьшите объём контекста")
	}
	hash := sha256.Sum256(data)
	m := Material{ID: ID(), ProjectID: project, Name: name, Role: role, MIME: mime, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:]), Text: value, Warning: warning, CreatedAt: now()}
	if err := s.CheckFolder(); err != nil {
		return m, err
	}
	dir := s.MaterialsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return m, err
	}
	path := filepath.Join(dir, m.ID)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return m, err
	}
	if err := s.AddMaterial(m); err != nil {
		os.Remove(path)
		return m, err
	}
	return m, nil
}

func (s *Store) Snapshot(r Run) error {
	if err := s.CheckFolder(); err != nil {
		return err
	}
	dir := filepath.Join(s.Dir, "runs", r.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, agent := range []string{"codex", "claude"} {
		if err := os.MkdirAll(filepath.Join(dir, agent), 0700); err != nil {
			return err
		}
	}
	for _, m := range r.Materials {
		data, err := os.ReadFile(filepath.Join(s.MaterialsDir(), m.ID))
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != m.SHA256 {
			return errors.New("Контрольная сумма материала изменилась")
		}
		if err := os.WriteFile(filepath.Join(dir, m.ID+".original"), data, 0400); err != nil {
			return err
		}
	}
	return nil
}
