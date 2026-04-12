// cortex-mcp is a minimal Model Context Protocol stdio server whose
// sole responsibility is reliable trail lifecycle for a host session.
//
// On process start it runs `cortex trail begin` and captures the trail
// id. On stdin EOF, SIGINT/SIGTERM, or an MCP `exit` notification it
// runs `cortex trail end` with CORTEX_TRAIL_ID set. It exposes zero
// tools, resources, or prompts — every other cortex command remains a
// CLI invocation. The MCP handshake exists only so that a host like
// Claude Code will keep the process alive for the duration of the
// session; tying the trail to a long-lived subprocess is more reliable
// than a SessionStart/SessionEnd hook pair because process lifetime is
// owned by the host and shutdown signals propagate cleanly.
//
// The implementation is a hand-rolled newline-delimited JSON-RPC loop
// so the project does not take on an MCP SDK dependency for a server
// this small.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "cortex-mcp"
	serverVersion   = "0.1.0"
	agentLabel      = "claude-code"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func logf(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "cortex-mcp: "+format+"\n", a...)
}

func main() {
	cortexBin := os.Getenv("CORTEX_BIN")
	if cortexBin == "" {
		cortexBin = "cortex"
	}

	cwd, _ := os.Getwd()
	trailName := fmt.Sprintf("%s @ %s", agentLabel, filepath.Base(cwd))

	out, err := runCortex(cortexBin, nil, "trail", "begin",
		"--agent="+agentLabel, "--name="+trailName)
	trailID := strings.TrimSpace(out)
	if err != nil {
		logf("trail begin failed, continuing without active trail: %v", err)
		trailID = ""
	} else {
		logf("trail begin ok: %s", trailID)
	}

	var endOnce sync.Once
	endTrail := func() {
		if trailID == "" {
			return
		}
		endOnce.Do(func() {
			env := append(os.Environ(), "CORTEX_TRAIL_ID="+trailID)
			if _, err := runCortex(cortexBin, env, "trail", "end"); err != nil {
				logf("trail end failed: %v", err)
				return
			}
			logf("trail end ok: %s", trailID)
		})
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		endTrail()
		os.Exit(0)
	}()
	defer endTrail()

	runRPCLoop(os.Stdin, os.Stdout)
}

func runRPCLoop(in io.Reader, out io.Writer) {
	stdout := bufio.NewWriter(out)
	var writeMu sync.Mutex
	writeMsg := func(v interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := json.NewEncoder(stdout).Encode(v); err != nil {
			logf("write error: %v", err)
			return
		}
		if err := stdout.Flush(); err != nil {
			logf("flush error: %v", err)
		}
	}

	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				if shouldExit := dispatch(line, writeMsg); shouldExit {
					return
				}
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			logf("stdin read error: %v", err)
			return
		}
	}
}

func dispatch(line []byte, writeMsg func(interface{})) (exit bool) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		logf("bad json: %v", err)
		return false
	}
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		writeMsg(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": protocolVersion,
				"capabilities": map[string]interface{}{
					"tools":     map[string]interface{}{},
					"resources": map[string]interface{}{},
					"prompts":   map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    serverName,
					"version": serverVersion,
				},
			},
		})
	case "tools/list":
		writeMsg(rpcResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]interface{}{"tools": []interface{}{}},
		})
	case "resources/list":
		writeMsg(rpcResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]interface{}{"resources": []interface{}{}},
		})
	case "prompts/list":
		writeMsg(rpcResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]interface{}{"prompts": []interface{}{}},
		})
	case "shutdown":
		writeMsg(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]interface{}{}})
	case "exit":
		return true
	case "notifications/initialized", "notifications/cancelled":
		// fire-and-forget
	default:
		if !isNotification {
			writeMsg(rpcResponse{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method},
			})
		}
	}
	return false
}

func runCortex(bin string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}
