package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		b.Buffer.Write(p)
	}
	return n, nil
}

// Subscription mode never silently switches to API-key billing or a custom proxy.
func subscriptionEnv(env []string) []string {
	out := []string{}
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		switch k {
		case "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_SIMPLE", "CLAUDECODE":
			continue
		}
		out = append(out, v)
	}
	return out
}

func AgentStatuses(ctx context.Context) []AgentStatus {
	out := make([]AgentStatus, 2)
	var wg sync.WaitGroup
	for i, agent := range []string{"codex", "claude"} {
		wg.Add(1)
		go func(i int, agent string) { defer wg.Done(); out[i] = agentStatus(ctx, agent) }(i, agent)
	}
	wg.Wait()
	return out
}
func agentStatus(ctx context.Context, agent string) AgentStatus {
	s := AgentStatus{Agent: agent, LoginCommand: "codex login"}
	if agent == "claude" {
		s.LoginCommand = "claude auth login --claudeai"
	}
	bin, err := exec.LookPath(agent)
	if err != nil {
		s.Detail = "CLI не найден в PATH"
		return s
	}
	s.Installed = true
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = subscriptionEnv(os.Environ())
		return cmd.CombinedOutput()
	}
	v, _ := run("--version")
	s.Version = strings.TrimSpace(string(v))
	if agent == "codex" {
		b, e := run("login", "status")
		s.Authenticated = e == nil
		s.Subscription = e == nil && strings.Contains(strings.ToLower(string(b)), "chatgpt")
	} else {
		b, e := run("auth", "status")
		var a struct {
			LoggedIn         bool   `json:"loggedIn"`
			AuthMethod       string `json:"authMethod"`
			SubscriptionType string `json:"subscriptionType"`
		}
		if e == nil && json.Unmarshal(b, &a) == nil {
			s.Authenticated = a.LoggedIn
			s.Subscription = a.LoggedIn && (strings.EqualFold(a.AuthMethod, "claude.ai") || strings.EqualFold(a.AuthMethod, "oauth") || a.SubscriptionType != "")
		}
	}
	switch {
	case s.Subscription:
		s.Detail = "Вход по подписке подтверждён; остаток лимита не проверяется"
	case s.Authenticated:
		s.Detail = "Вход найден, но подписка не подтверждена. Войдите через официальный CLI"
	default:
		s.Detail = "Нужен вход в CLI под этим пользователем ОС"
	}
	return s
}

type AgentInput struct{ Agent, Directory, Session, Prompt, Stage, Model string }
type AgentResult struct{ Text, Session, Model string }
type AgentRunner interface {
	Run(context.Context, AgentInput, func(string, string)) (AgentResult, error)
}
type CLIRunner struct{}

func commandArgs(in AgentInput) []string {
	if in.Agent == "claude" {
		args := []string{"-p", "--output-format", "stream-json", "--verbose", "--restricted", "--safe-mode", "--tools", "", "--permission-mode", "dontAsk", "--permission-prompts", "none", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
		if in.Model != "" {
			args = append(args, "--model", in.Model)
		}
		if in.Session != "" {
			args = append(args, "--resume", in.Session)
		}
		return args
	}
	args := []string{"exec"}
	if in.Session != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check", "-c", `approval_policy="never"`, "-c", `sandbox_mode="read-only"`, "-c", `web_search="disabled"`)
	args = append(args, "--ignore-user-config")
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.Session != "" {
		args = append(args, in.Session)
	}
	return append(args, "-")
}

type streamState struct {
	unavailable *AgentUnavailableError
	session     string
	model       string
	answer      strings.Builder
	final       string
	failure     string
	completed   bool
}

func (s *streamState) parse(agent string, line []byte, emit func(string, string)) {
	var e map[string]json.RawMessage
	if json.Unmarshal(line, &e) != nil {
		return
	}
	str := func(m map[string]json.RawMessage, k string) string {
		var v string
		_ = json.Unmarshal(m[k], &v)
		return v
	}
	typ := str(e, "type")
	var bad bool
	_ = json.Unmarshal(e["is_error"], &bad)
	errorPayload := typ == "turn.failed" || typ == "error" || typ == "result" && bad || typ == "assistant" && str(e, "error") != ""
	if errorPayload {
		if problem := unavailableDiagnostic(agent, string(line)); problem != nil {
			s.unavailable = problem
		}
	}
	if typ == "turn.completed" || typ == "result" && !bad {
		s.unavailable = nil
	}

	if agent == "codex" {
		if typ == "turn.completed" {
			s.completed = true
		}
		if typ == "thread.started" {
			s.session = str(e, "thread_id")
			s.model = str(e, "model")
		}
		if typ == "item.completed" {
			var item map[string]json.RawMessage
			_ = json.Unmarshal(e["item"], &item)
			switch str(item, "type") {
			case "agent_message":
				t := str(item, "text")
				s.answer.WriteString(t + "\n\n")
				s.final = t
				emit("message", t)
			case "command_execution":
				emit("activity", "Команда завершена (вывод не включён в журнал)")
			case "file_change":
				emit("activity", "Изменения файлов")
			}
		}
		if typ == "turn.failed" {
			s.failure = "Codex сообщил об ошибке запроса. Проверьте вход, лимиты и подключение."
		}
	} else {
		if typ == "system" && str(e, "subtype") == "init" {
			s.session = str(e, "session_id")
		}
		if typ == "assistant" {
			var msg struct {
				Model   string
				Content []struct{ Type, Text, Name string }
			}
			if json.Unmarshal(e["message"], &msg) == nil {
				if validModel(msg.Model) && msg.Model != "" {
					s.model = msg.Model
				}
				for _, part := range msg.Content {
					switch part.Type {
					case "text":
						s.answer.WriteString(part.Text + "\n\n")
						emit("message", part.Text)
					case "tool_use":
						emit("activity", "Инструмент: "+part.Name)
					}
				}
			}
		}
		if typ == "result" {
			s.completed = true
			var bad bool
			_ = json.Unmarshal(e["is_error"], &bad)
			if bad {
				s.failure = "Claude сообщил об ошибке запроса. Проверьте вход, лимиты и подключение."
			} else {
				s.final = str(e, "result")
			}
			if id := str(e, "session_id"); id != "" {
				s.session = id
			}
		}
	}
	// Thinking / reasoning blocks and raw provider payloads are intentionally not persisted.
}

func (CLIRunner) Run(ctx context.Context, in AgentInput, emit func(string, string)) (AgentResult, error) {
	if in.Agent != "codex" && in.Agent != "claude" {
		return AgentResult{}, errors.New("Неизвестный агент")
	}
	cmd := exec.CommandContext(ctx, in.Agent, commandArgs(in)...)
	cmd.Dir = in.Directory
	cmd.Env = subscriptionEnv(os.Environ())
	cmd.Stdin = strings.NewReader(in.Prompt)
	cmd.WaitDelay = 3 * time.Second
	isolateProcess(cmd)
	var stderr cappedBuffer
	stderr.limit = 32 << 10
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return AgentResult{}, err
	}
	if err = cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return AgentResult{}, &AgentUnavailableError{Agent: in.Agent, Kind: "missing_cli", Detail: "Официальный CLI не найден в PATH"}
		}
		return AgentResult{}, fmt.Errorf("Не удалось запустить %s: %w", in.Agent, err)
	}
	state := streamState{}
	scan := bufio.NewScanner(io.LimitReader(stdout, 8<<20))
	scan.Buffer(make([]byte, 64<<10), 2<<20)
	lastSession := ""
	for scan.Scan() {
		state.parse(in.Agent, scan.Bytes(), emit)
		if state.session != "" && state.session != lastSession {
			lastSession = state.session
			emit("session", state.session)
		}
	}
	scanErr := scan.Err()
	if scanErr != nil {
		_ = cmd.Cancel()
	}
	err = cmd.Wait()
	result := AgentResult{Session: state.session, Model: state.model, Text: strings.TrimSpace(state.answer.String())}
	if state.final != "" {
		result.Text = state.final
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if scanErr != nil {
		return result, errors.New("Слишком большой или повреждённый поток ответа")
	}
	if state.unavailable != nil {
		return result, state.unavailable
	}
	if err != nil {
		if problem := unavailableDiagnostic(in.Agent, stderr.String()); problem != nil {
			return result, problem
		}
	}
	if state.failure != "" {
		return result, errors.New(state.failure)
	}
	if err != nil {
		return result, fmt.Errorf("%s завершился с ошибкой (%v). Проверьте вход в CLI, сеть и лимит подписки", in.Agent, err)
	}
	if !state.completed {
		return result, errors.New("Поток CLI оборвался до подтверждения завершения ответа")
	}
	if result.Text == "" {
		return result, errors.New("CLI завершился без текстового ответа")
	}
	if in.Agent == "codex" && result.Model == "" {
		result.Model = sessionModel("codex", result.Session)
	}
	if result.Model == "" || !validModel(result.Model) {
		return result, errors.New("CLI не сообщил фактическую модель. Не удалось закрепить модель цикла; проверьте версию CLI и доступность локального журнала сессии")
	}
	return result, nil
}
