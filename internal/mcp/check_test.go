//go:build linux

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/mouizahmed/hutch/internal/agents"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureReply(r request) map[string]any {
	result := any(map[string]any{})
	if r.Method == "initialize" {
		result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
	} else if r.Method == "tools/list" {
		result = map[string]any{"tools": []any{map[string]any{"name": "echo"}}}
	}
	return map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": result}
}

func TestMCPFixture(t *testing.T) {
	mode := os.Getenv("HUTCH_MCP_FIXTURE")
	if mode == "" {
		return
	}
	if mode == "hang" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if mode == "bad" {
		fmt.Fprintln(os.Stderr, "synthetic-secret-must-not-appear")
		fmt.Println("not protocol output")
		os.Exit(1)
	}
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var r request
		json.Unmarshal(scan.Bytes(), &r)
		if r.ID != 0 {
			if mode == "ping" {
				fmt.Println(`{"jsonrpc":"2.0","id":"probe-ping","method":"ping"}`)
				if !scan.Scan() {
					os.Exit(3)
				}
				var answer response
				if json.Unmarshal(scan.Bytes(), &answer) != nil || string(answer.ID) != `"probe-ping"` || string(answer.Result) != "{}" {
					os.Exit(4)
				}
			}
			json.NewEncoder(os.Stdout).Encode(fixtureReply(r))
		}
	}
	os.Exit(0)
}

func TestStdioChecks(t *testing.T) {
	for _, mode := range []string{"echo", "ping", "bad", "hang", "missing"} {
		t.Run(mode, func(t *testing.T) {
			cmd, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				cmd = "/nonexistent-hutch-mcp"
			}
			timeout := 2 * time.Second
			if mode == "hang" {
				timeout = 200 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			start := time.Now()
			n, err := check(ctx, spec{Command: cmd, Args: []string{"-test.run=TestMCPFixture"}, Env: map[string]string{"HUTCH_MCP_FIXTURE": mode}})
			good := mode == "echo" || mode == "ping"
			if good && (err != nil || n != 1) {
				t.Fatalf("tools=%d err=%v", n, err)
			}
			if !good && err == nil {
				t.Fatal("accepted broken MCP")
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("stderr leaked")
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("command did not stop on its deadline")
			}
		})
	}
}

func TestHTTPChecks(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var closed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-secret" {
					w.WriteHeader(401)
					return
				}
				if r.Method == http.MethodDelete {
					closed.Store(true)
					w.WriteHeader(204)
					return
				}
				var req request
				json.NewDecoder(r.Body).Decode(&req)
				if req.Method != "initialize" && (r.Header.Get("MCP-Protocol-Version") != "2025-03-26" || r.Header.Get("Mcp-Session-Id") != "test-session") {
					t.Error("negotiated protocol/session missing")
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Mcp-Session-Id", "test-session")
				if req.ID == 0 {
					w.WriteHeader(202)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					data, _ := json.Marshal(fixtureReply(req))
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
					w.(http.Flusher).Flush()
					<-r.Context().Done() // The client must read its result before EOF.
				} else {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(fixtureReply(req))
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			n, err := check(ctx, spec{URL: server.URL, Headers: map[string]string{"Authorization": "Bearer synthetic-secret"}})
			if err != nil || n != 1 || !closed.Load() {
				t.Fatalf("tools=%d closed=%v err=%v", n, closed.Load(), err)
			}
		})
	}
}

func TestFileErrorsAndAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "synthetic-secret-must-not-appear")
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	data := fmt.Sprintf(`{"mcpServers":{"auth":{"url":%q},"disabled":{"command":"/missing","enabled":false}}}`, server.URL)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := CheckFile(t.Context(), path, "mcpServers", agents.MCPFields{Enabled: "enabled"}, time.Second)
	if err != nil || len(results) != 2 {
		t.Fatalf("%+v %v", results, err)
	}
	for _, r := range results {
		if r.Status != "warn" || strings.Contains(r.Detail, "synthetic-secret") {
			t.Fatalf("%+v", r)
		}
	}
	// ${VAR} is filled in from the environment, as agents do; unset, the check
	// says so instead of reporting an authentication failure.
	data = `{"mcpServers":{"api":{"url":"https://mcp.invalid/${HUTCH_TEST_UNSET_VAR}"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if results, err := CheckFile(t.Context(), path, "mcpServers", agents.MCPFields{}, time.Second); err != nil || len(results) != 1 || !strings.Contains(results[0].Detail, "HUTCH_TEST_UNSET_VAR") {
		t.Fatalf("unset variable: %+v %v", results, err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFile(t.Context(), path, "mcpServers", agents.MCPFields{Enabled: "enabled"}, time.Second); err == nil {
		t.Fatal("malformed config treated as empty")
	}
}

func TestProtocolErrorKeepsCodeWithoutProviderPayload(t *testing.T) {
	_, err := replyResult(response{Error: json.RawMessage(`{"code":-32001,"message":"synthetic-secret-must-not-appear","data":{"token":"synthetic-secret"}}`)})
	if err == nil || !strings.Contains(err.Error(), "-32001") || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("missing code or leaked provider payload: %v", err)
	}
}

func TestNoAuthRedirect(t *testing.T) {
	called := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, err := check(t.Context(), spec{URL: server.URL, Headers: map[string]string{"Authorization": "Bearer synthetic-secret"}}); err == nil || called {
		t.Fatalf("redirect followed=%v err=%v", called, err)
	}
}
