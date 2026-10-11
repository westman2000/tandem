package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

func probeAgent(ctx context.Context, agent string) (AgentStatus, *QuotaReading, error) {
	s := agentStatus(ctx, agent)
	if !s.Subscription || agent != "codex" {
		return s, nil, nil
	}
	key, q, err := readCodexQuota(ctx)
	if key != "" {
		s.AccountKey = key
	}
	return s, q, err
}

// The app-server owns subscription authentication. No credentials are extracted,
// no thread/turn is created, and no generation is used as a quota probe.
func readCodexQuota(parent context.Context) (string, *QuotaReading, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://", "-c", "analytics.enabled=false")
	cmd.Env = subscriptionEnv(os.Environ())
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = 2 * time.Second
	isolateProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return "", nil, err
	}
	defer func() { _ = stdin.Close(); cancel(); _ = cmd.Wait() }()
	return codexQuotaRPC(stdin, stdout)
}
func codexQuotaRPC(w io.Writer, r io.Reader) (string, *QuotaReading, error) {
	encoder := json.NewEncoder(w)
	scanner := bufio.NewScanner(io.LimitReader(r, 4<<20))
	scanner.Buffer(make([]byte, 8192), 1<<20)
	call := func(id int, method string, params any) (json.RawMessage, error) {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for scanner.Scan() {
			var v struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &v) != nil || v.ID == nil || *v.ID != id {
				continue
			}
			if len(v.Error) > 0 && string(v.Error) != "null" {
				return nil, fmt.Errorf("quota query unavailable")
			}
			return v.Result, nil
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	}
	if _, err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "tandem_monitor", "title": "Tandem", "version": "0.5.1"}}); err != nil {
		return "", nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return "", nil, err
	}
	raw, err := call(2, "account/read", map[string]bool{"refreshToken": false})
	if err != nil {
		return "", nil, err
	}
	var auth struct {
		Account *struct {
			Type  string `json:"type"`
			Email string `json:"email"`
			ID    string `json:"id"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.Account == nil || auth.Account.Type != "chatgpt" {
		return "", nil, errors.New("ChatGPT subscription not confirmed")
	}
	key := textHash(auth.Account.Type + ":" + auth.Account.ID + ":" + auth.Account.Email)
	raw, err = call(3, "account/rateLimits/read", nil)
	if err != nil {
		return key, nil, err
	}
	q, err := parseCodexQuota(raw)
	return key, q, err
}
