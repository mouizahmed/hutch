//go:build linux

// Package mcp performs bounded, model-free readiness checks inside a box.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mouizahmed/hutch/internal/agents"
)

const maxMessage = 1 << 20

type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail"`
}

type spec struct {
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	HTTP     map[string]string `json:"http_headers"`
	Bearer   string            `json:"bearer_token_env_var"`
	Type     string            `json:"type"`
	Enabled  *bool             `json:"enabled"`
	Disabled bool              `json:"disabled"`
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type exchange func(request) (json.RawMessage, error)

var errAuth = errors.New("authentication required; verify OAuth with the native MCP client")
var errVersion = errors.New("negotiated MCP version needs a native client check")
var errNative = errors.New("server-initiated HTTP requests need a native client check")

// CheckFile reads only the adapter-declared user scope. It never prints config,
// token values, server payloads, or subprocess stderr. OAuth managed by a native
// client and legacy SSE/WebSocket transports are reported as unverified.
func CheckFile(ctx context.Context, path, key string, timeout time.Duration) ([]Result, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read MCP config")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMessage+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read MCP config")
	}
	if len(data) > maxMessage {
		return nil, fmt.Errorf("MCP config exceeds 1 MiB")
	}
	servers, err := agents.ParseMCPServers(path, data, key)
	if err != nil {
		return nil, errors.New("invalid MCP configuration")
	}
	var names []string
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Result
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			out = append(out, Result{Name: name, Status: "warn", Detail: "not checked: diagnostic deadline reached"})
			continue
		}
		r := Result{Name: name, Status: "ok"}
		data, _ := json.Marshal(servers[name])
		var s spec
		if err := json.Unmarshal(data, &s); err != nil {
			r.Status, r.Detail = "fail", "invalid command, args, environment, or headers"
		} else if s.Disabled || s.Enabled != nil && !*s.Enabled {
			r.Status, r.Detail = "warn", "disabled in config"
		} else if s.Type == "sse" || s.Type == "websocket" {
			r.Status, r.Detail = "warn", "legacy SSE/WebSocket transport needs a native client check"
		} else {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			n, err := check(cctx, s)
			cancel()
			if errors.Is(err, errAuth) {
				r.Status, r.Detail = "warn", errAuth.Error()
			} else if errors.Is(err, errVersion) {
				r.Status, r.Detail = "warn", errVersion.Error()
			} else if errors.Is(err, errNative) {
				r.Status, r.Detail = "warn", errNative.Error()
			} else if err != nil {
				r.Status, r.Detail = "fail", err.Error()
			} else {
				r.Detail = fmt.Sprintf("initialize passed; %d tools discovered", n)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func check(ctx context.Context, s spec) (int, error) {
	var talk exchange
	var httpClient *httpExchange
	if s.URL != "" {
		h := &httpExchange{ctx: ctx, spec: s, client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
		talk = h.call
		httpClient = h
		defer h.close()
	} else {
		if s.Command == "" {
			return 0, errors.New("no command or URL configured")
		}
		cmd := exec.CommandContext(ctx, s.Command, s.Args...)
		cmd.Env = os.Environ()
		for k, v := range s.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = time.Second
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return 0, errors.New("cannot open MCP command input")
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, errors.New("cannot open MCP command output")
		}
		if err := cmd.Start(); err != nil {
			return 0, errors.New("MCP command could not start; check the executable and arguments")
		}
		defer func() { stdin.Close(); syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 4096), maxMessage)
		talk = func(r request) (json.RawMessage, error) {
			if err := json.NewEncoder(stdin).Encode(r); err != nil {
				return nil, errors.New("MCP command closed its input")
			}
			if r.ID == 0 {
				return nil, nil
			}
			for i := 0; i < 64 && scan.Scan(); i++ {
				var reply response
				if err := json.Unmarshal(scan.Bytes(), &reply); err != nil || reply.JSONRPC != "2.0" {
					return nil, errors.New("MCP command wrote invalid protocol output")
				}
				if reply.Method != "" {
					if len(reply.ID) != 0 && string(reply.ID) != "null" {
						answer := map[string]any{"jsonrpc": "2.0", "id": reply.ID}
						if reply.Method == "ping" {
							answer["result"] = map[string]any{}
						} else {
							answer["error"] = map[string]any{"code": -32601, "message": "doctor does not support this method"}
						}
						if err := json.NewEncoder(stdin).Encode(answer); err != nil {
							return nil, errors.New("MCP command closed its input")
						}
					}
					continue
				}
				if string(reply.ID) == strconv.Itoa(r.ID) {
					return replyResult(reply)
				}
			}
			return nil, errors.New("MCP command closed, timed out, or exceeded the response limit")
		}
	}
	init, err := talk(request{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "hutch-doctor", "version": "1"},
	}})
	if err != nil {
		return 0, err
	}
	var initialized struct {
		Protocol string                         `json:"protocolVersion"`
		Caps     map[string]json.RawMessage     `json:"capabilities"`
		Server   struct{ Name, Version string } `json:"serverInfo"`
	}
	if json.Unmarshal(init, &initialized) != nil || initialized.Protocol == "" || initialized.Caps == nil || initialized.Server.Name == "" || initialized.Server.Version == "" {
		return 0, errors.New("invalid MCP initialize result")
	}
	if initialized.Protocol != "2025-06-18" && initialized.Protocol != "2025-03-26" && initialized.Protocol != "2024-11-05" {
		return 0, errVersion
	}
	if httpClient != nil {
		httpClient.protocol = initialized.Protocol
	}
	if _, err := talk(request{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return 0, err
	}
	if initialized.Caps["tools"] == nil {
		return 0, nil
	}
	tools, err := talk(request{JSONRPC: "2.0", ID: 2, Method: "tools/list"})
	if err != nil {
		return 0, err
	}
	var result struct {
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(tools, &result) != nil || len(result.Tools) == 0 || string(result.Tools) == "null" {
		return 0, errors.New("invalid tools/list result")
	}
	var inventory []json.RawMessage
	if json.Unmarshal(result.Tools, &inventory) != nil {
		return 0, errors.New("invalid tools/list inventory")
	}
	return len(inventory), nil
}

func replyResult(r response) (json.RawMessage, error) {
	if len(r.Error) > 0 && string(r.Error) != "null" {
		return nil, errors.New("MCP server returned a protocol error; inspect it with the native client")
	}
	if len(r.Result) == 0 {
		return nil, errors.New("MCP server returned no result")
	}
	return r.Result, nil
}

type httpExchange struct {
	ctx      context.Context
	spec     spec
	client   *http.Client
	session  string
	protocol string
}

func (h *httpExchange) headers(req *http.Request) error {
	for k, v := range h.spec.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range h.spec.HTTP {
		req.Header.Set(k, v)
	}
	if h.spec.Bearer != "" {
		value := os.Getenv(h.spec.Bearer)
		if value == "" {
			return errAuth
		}
		req.Header.Set("Authorization", "Bearer "+value)
	}
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	if h.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", h.protocol)
	}
	return nil
}

func (h *httpExchange) call(r request) (json.RawMessage, error) {
	data, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.spec.URL, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid MCP URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if err := h.headers(req); err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, errors.New("MCP HTTP connection failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errAuth
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("MCP HTTP status %d", resp.StatusCode)
	}
	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		h.session = session
	}
	if r.ID == 0 {
		return nil, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return eventResult(resp.Body, r.ID)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
	if err != nil || len(data) > maxMessage {
		return nil, errors.New("MCP HTTP response exceeded the limit or could not be read")
	}
	var reply response
	if json.Unmarshal(data, &reply) != nil || reply.JSONRPC != "2.0" || string(reply.ID) != strconv.Itoa(r.ID) || reply.Method != "" {
		return nil, errors.New("MCP HTTP server returned an invalid response")
	}
	return replyResult(reply)
}

func eventResult(body io.Reader, id int) (json.RawMessage, error) {
	scan := bufio.NewScanner(io.LimitReader(body, maxMessage+1))
	scan.Buffer(make([]byte, 4096), maxMessage)
	var data []string
	for scan.Scan() {
		line := scan.Text()
		if line == "" && len(data) > 0 {
			var reply response
			if json.Unmarshal([]byte(strings.Join(data, "\n")), &reply) == nil && reply.JSONRPC == "2.0" {
				if reply.Method != "" && len(reply.ID) != 0 && string(reply.ID) != "null" {
					return nil, errNative
				}
				if reply.Method == "" && string(reply.ID) == strconv.Itoa(id) {
					return replyResult(reply)
				}
			}
			data = nil
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(value))
		}
	}
	return nil, errors.New("MCP event stream closed, timed out, or returned no matching result")
}

func (h *httpExchange) close() {
	if h.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.spec.URL, nil)
	if err != nil {
		return
	}
	if h.headers(r) != nil {
		return
	}
	if resp, err := h.client.Do(r); err == nil {
		resp.Body.Close()
	}
}
