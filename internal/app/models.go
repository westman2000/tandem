package app

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type ModelOption struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

var modelName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,119}(\[1m\])?$`)
var sessionID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,80}$`)

func validModel(model string) bool { return model == "" || modelName.MatchString(model) }

func cliHome(agent string) string {
	home, _ := os.UserHomeDir()
	if agent == "codex" {
		if dir := os.Getenv("CODEX_HOME"); dir != "" {
			return dir
		}
		return filepath.Join(home, ".codex")
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}

// Read only the model metadata of the exact CLI session; never persist its raw log.
func sessionModel(agent, id string) string {
	if !sessionID.MatchString(id) {
		return ""
	}
	root := cliHome(agent)
	pattern := filepath.Join(root, "sessions", "*", "*", "*", "*-"+id+".jsonl")
	if agent == "claude" {
		pattern = filepath.Join(root, "projects", "*", id+".jsonl")
	}
	paths, _ := filepath.Glob(pattern)
	model := ""
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scan := bufio.NewScanner(io.LimitReader(f, 128<<20))
		scan.Buffer(make([]byte, 64<<10), 8<<20)
		for scan.Scan() {
			var record struct {
				Type    string `json:"type"`
				Payload struct {
					Model string `json:"model"`
				} `json:"payload"`
				Message struct {
					Model string `json:"model"`
				} `json:"message"`
			}
			if json.Unmarshal(scan.Bytes(), &record) != nil {
				continue
			}
			value := ""
			if agent == "codex" && record.Type == "turn_context" {
				value = record.Payload.Model
			}
			if agent == "claude" && record.Type == "assistant" {
				value = record.Message.Model
			}
			if value != "" && validModel(value) {
				model = value
			}
		}
		f.Close()
	}
	return model
}

func readModelJSON(path string, out any) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(out)
}

// Catalog entries are suggestions, not an entitlement check. CLI authentication
// and the actual inference request remain responsible for model availability.
func modelCatalog(store *Store) map[string][]ModelOption {
	out := map[string][]ModelOption{"codex": {}, "claude": {}}
	add := func(agent, id, name, source string) {
		if id == "" || !validModel(id) {
			return
		}
		for _, old := range out[agent] {
			if old.ID == id {
				return
			}
		}
		if name == "" {
			name = id
		}
		out[agent] = append(out[agent], ModelOption{ID: id, Name: name, Source: source})
	}
	var cache struct {
		Models []struct {
			Slug        string
			DisplayName string `json:"display_name"`
			Visibility  string
		}
	}
	readModelJSON(filepath.Join(cliHome("codex"), "models_cache.json"), &cache)
	for _, m := range cache.Models {
		if m.Visibility == "list" {
			add("codex", m.Slug, m.DisplayName, "catalog")
		}
	}
	// Documented Claude CLI aliases; explicit IDs can also be entered in the UI.
	for _, alias := range []string{"fable", "opus", "sonnet", "haiku"} {
		add("claude", alias, alias, "alias")
	}
	home, _ := os.UserHomeDir()
	claudeConfig := filepath.Join(home, ".claude.json")
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		claudeConfig = filepath.Join(cliHome("claude"), ".claude.json")
	}
	var extra struct {
		Options []struct{ Value, Label string } `json:"additionalModelOptionsCache"`
	}
	readModelJSON(claudeConfig, &extra)
	for _, m := range extra.Options {
		add("claude", m.Value, m.Label, "catalog")
	}
	runs, _ := store.Runs("")
	for _, r := range runs {
		if r.Mode != "live" {
			continue
		}
		for _, task := range r.Tasks {
			add(task.Agent, task.Model, task.Model, "used")
		}
		for agent, model := range r.LockedModels {
			add(agent, strings.TrimSuffix(model, "[1m]"), model, "used")
		}
	}
	return out
}
