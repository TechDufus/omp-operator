/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command fakeomp is a scripted stand-in for `omp --mode rpc`. It speaks omp's
// RPC wire protocol (JSON lines over stdin/stdout) using the omprpc wire types
// and is used by the driver tests and by the e2e image, where it is installed
// as /usr/local/bin/omp.
//
// # Command line
//
// The flags the driver passes to omp are accepted: --mode, --no-ui,
// --approval-mode, --tools, --max-time, --session-dir and
// --append-system-prompt (each also as --flag=value). Only --session-dir and
// --tools have an effect; every other argument is ignored.
//
// # Environment
//
//	FAKEOMP_SCRIPT    JSON: {"mode": "...", "calls": [{"tool": "...", "args": {}}],
//	                  "finish": {"verdict": "ok", "title": "...", "summary": "...",
//	                  "report": "...", "proposed_commands": [...]}}.
//	                  Unset means mode normal, no calls and finish verdict ok.
//	FAKEOMP_ECHO_ENV  Name of an environment variable whose value fakeomp leaks
//	                  on purpose, so redaction can be tested: the value is
//	                  appended to the notice message and to the finish summary,
//	                  and added as "fakeomp_echo" to the args of every
//	                  tool_execution_start event.
//
// # Modes
//
//	normal     the default: calls, then finish, then prompt_result completed.
//	no-finish  like normal but never calls finish (also on the nudge prompt).
//	garbage    like normal, but writes one non-JSON line before ready.
//	exit-early exits 1 when the first prompt arrives, before acknowledging it.
//	hang       acknowledges the prompt and never finishes it until `abort`
//	           arrives, then emits prompt_result with status aborted.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
)

const (
	modeNormal    = "normal"
	modeNoFinish  = "no-finish"
	modeGarbage   = "garbage"
	modeExitEarly = "exit-early"
	modeHang      = "hang"

	sessionID    = "fake-session"
	sessionFile  = "fake.jsonl"
	exportedHTML = "<html>fake</html>"

	// Protocol v2 limits the SDK requires the ready frame to advertise.
	maxFrameBytes            = 1 << 20
	maxReassembledFrameBytes = 64 << 20
)

type call struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type script struct {
	Mode   string                     `json:"mode"`
	Calls  []call                     `json:"calls"`
	Finish map[string]json.RawMessage `json:"finish"`
}

func loadScript() (script, error) {
	s := script{Mode: modeNormal}
	if raw := os.Getenv("FAKEOMP_SCRIPT"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return s, fmt.Errorf("FAKEOMP_SCRIPT: %w", err)
		}
	}
	switch s.Mode {
	case "":
		s.Mode = modeNormal
	case modeNormal, modeNoFinish, modeGarbage, modeExitEarly, modeHang:
	default:
		return s, fmt.Errorf("FAKEOMP_SCRIPT: unknown mode %q", s.Mode)
	}
	if s.Finish == nil {
		s.Finish = map[string]json.RawMessage{
			"verdict": json.RawMessage(`"ok"`),
			"title":   json.RawMessage(`"fake"`),
			"summary": json.RawMessage(`"fake"`),
			"report":  json.RawMessage(`"fake"`),
		}
	}
	return s, nil
}

// options holds the command-line flags that matter to fakeomp.
type options struct {
	sessionDir string
	tools      []string
}

// valueFlags are the flags that take a separate value argument.
var valueFlags = map[string]bool{
	"--mode": true, "--approval-mode": true, "--tools": true, "--max-time": true,
	"--session-dir": true, "--append-system-prompt": true,
}

func parseArgs(args []string) options {
	var o options
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if !valueFlags[name] {
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch name {
		case "--session-dir":
			o.sessionDir = value
		case "--tools":
			for tool := range strings.SplitSeq(value, ",") {
				if tool = strings.TrimSpace(tool); tool != "" {
					o.tools = append(o.tools, tool)
				}
			}
		}
	}
	return o
}

type server struct {
	script script
	opts   options
	echo   string // value of FAKEOMP_ECHO_ENV, when set

	outMu sync.Mutex
	out   io.Writer

	mu          sync.Mutex
	hostTools   []string
	sessionDir  string
	waiting     map[string]chan omprpc.HostToolResult
	activeAbort map[int64]context.CancelFunc

	seq atomic.Int64
}

func main() {
	s, err := loadScript()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeomp:", err)
		os.Exit(2)
	}
	srv := &server{
		script:      s,
		opts:        parseArgs(os.Args[1:]),
		out:         os.Stdout,
		waiting:     map[string]chan omprpc.HostToolResult{},
		activeAbort: map[int64]context.CancelFunc{},
	}
	srv.sessionDir = srv.opts.sessionDir
	if name := os.Getenv("FAKEOMP_ECHO_ENV"); name != "" {
		srv.echo = os.Getenv(name)
	}
	if err := srv.serve(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "fakeomp:", err)
		os.Exit(1)
	}
}

// emit writes one frame; frames are serialized so concurrent writers never interleave.
func (s *server) emit(frame any) {
	line, err := json.Marshal(frame)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeomp: marshal:", err)
		os.Exit(1)
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(append(line, '\n')); err != nil {
		// The client closed stdout; nothing left to serve.
		os.Exit(0)
	}
}

func (s *server) respond(id, command string, data any) {
	response := omprpc.RpcResponse{Command: command, Success: true, ID: &id}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakeomp: marshal:", err)
			os.Exit(1)
		}
		response.Data = raw
	}
	s.emit(response)
}

func (s *server) fail(id, command, code, message string) {
	response := omprpc.RpcResponse{Command: command, Success: false, Error: &message, Code: &code}
	if id != "" {
		response.ID = &id
	}
	s.emit(response)
}

func (s *server) serve(in io.Reader) error {
	if s.script.Mode == modeGarbage {
		s.outMu.Lock()
		_, err := io.WriteString(s.out, "fakeomp: this line is not json\n")
		s.outMu.Unlock()
		if err != nil {
			return err
		}
	}
	s.emit(omprpc.ReadyEvent{
		ProtocolVersion:           new(int64(1)),
		SupportedProtocolVersions: []int64{1, 2},
		MaxFrameBytes:             new(int64(maxFrameBytes)),
		MaxReassembledFrameBytes:  new(int64(maxReassembledFrameBytes)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.handle(ctx, line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *server) handle(ctx context.Context, line []byte) {
	var frame struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		s.fail("", "parse", "parse_error", err.Error())
		return
	}
	switch frame.Type {
	case "negotiate_protocol":
		s.respond(frame.ID, frame.Type, omprpc.NegotiateProtocolResult{ProtocolVersion: 2})
	case "set_host_tools":
		var params omprpc.SetHostToolsCommand
		if err := json.Unmarshal(line, &params); err != nil {
			s.fail(frame.ID, frame.Type, "invalid_params", err.Error())
			return
		}
		names := make([]string, 0, len(params.Tools))
		for _, tool := range params.Tools {
			names = append(names, tool.Name)
		}
		s.mu.Lock()
		s.hostTools = names
		s.mu.Unlock()
		s.respond(frame.ID, frame.Type, omprpc.SetHostToolsResult{ToolNames: names})
	case "get_state":
		s.respond(frame.ID, frame.Type, s.state())
	case "open_session":
		s.openSession(frame.ID, line)
	case "prompt":
		s.prompt(ctx, frame.ID)
	case "abort":
		s.mu.Lock()
		for _, cancel := range s.activeAbort {
			cancel()
		}
		s.mu.Unlock()
		s.respond(frame.ID, frame.Type, nil)
	case "get_session_stats":
		s.respond(frame.ID, frame.Type, map[string]any{
			"sessionId":         sessionID,
			"userMessages":      1,
			"assistantMessages": 1,
			"toolCalls":         len(s.script.Calls) + 1,
			"toolResults":       len(s.script.Calls) + 1,
			"totalMessages":     2,
			"tokens": map[string]any{
				"input": 1000, "output": 200, "cacheRead": 300, "cacheWrite": 40, "total": 1540,
			},
			"premiumRequests": 0,
			"cost":            0.0125,
		})
	case "export_html":
		s.exportHTML(frame.ID, line)
	case "host_tool_result":
		var result omprpc.HostToolResult
		if err := json.Unmarshal(line, &result); err != nil {
			fmt.Fprintln(os.Stderr, "fakeomp: bad host_tool_result:", err)
			return
		}
		s.mu.Lock()
		ch := s.waiting[result.ID]
		delete(s.waiting, result.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- result
		}
	case "host_tool_update":
		// Partial results are not recorded.
	default:
		s.fail(frame.ID, frame.Type, "unknown_command", "fakeomp does not implement "+frame.Type)
	}
}

func (s *server) state() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	tools := make([]omprpc.ToolDescriptor, 0, len(s.opts.tools)+len(s.hostTools))
	for _, name := range s.opts.tools {
		tools = append(tools, omprpc.ToolDescriptor{Name: name, Description: "fake built-in tool"})
	}
	for _, name := range s.hostTools {
		tools = append(tools, omprpc.ToolDescriptor{Name: name, Description: "fake host tool"})
	}
	return map[string]any{
		"sessionId":           sessionID,
		"isStreaming":         false,
		"isCompacting":        false,
		"tokensPerSecond":     nil,
		"messageCount":        0,
		"queuedMessageCount":  0,
		"hasPendingAsyncWork": false,
		"isSettled":           true,
		"todoPhases":          []any{},
		"systemPrompt":        []string{},
		"dumpTools":           tools,
		"goal":                nil,
	}
}

// sessionFiles lists the *.jsonl files of dir.
func sessionFiles(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	return files
}

func (s *server) openSession(id string, line []byte) {
	var params omprpc.OpenSessionCommand
	if err := json.Unmarshal(line, &params); err != nil || params.SessionDir == "" {
		s.fail(id, "open_session", "invalid_params", "sessionDir is required")
		return
	}
	resumed := len(sessionFiles(params.SessionDir)) > 0
	s.mu.Lock()
	s.sessionDir = params.SessionDir
	s.mu.Unlock()
	if err := s.appendSession(`{"type":"session","id":"` + sessionID + `"}`); err != nil {
		s.fail(id, "open_session", "io_error", err.Error())
		return
	}
	s.respond(id, "open_session", omprpc.OpenSessionResult{
		Resumed:     resumed,
		SessionID:   sessionID,
		SessionFile: new(filepath.Join(params.SessionDir, sessionFile)),
	})
}

// appendSession appends a line to fake.jsonl in the session directory; it does nothing without one.
func (s *server) appendSession(line string) error {
	s.mu.Lock()
	dir := s.sessionDir
	s.mu.Unlock()
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, sessionFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	return errors.Join(err, f.Close())
}

func (s *server) exportHTML(id string, line []byte) {
	var params omprpc.ExportHTMLCommand
	if err := json.Unmarshal(line, &params); err != nil {
		s.fail(id, "export_html", "invalid_params", err.Error())
		return
	}
	path := filepath.Join(os.TempDir(), "fakeomp-export.html")
	if params.OutputPath != nil && *params.OutputPath != "" {
		path = *params.OutputPath
	}
	if err := os.WriteFile(path, []byte(exportedHTML), 0o644); err != nil {
		s.fail(id, "export_html", "io_error", err.Error())
		return
	}
	s.respond(id, "export_html", omprpc.ExportHtmlResult{Path: path})
}

// prompt acknowledges a prompt and runs its turn in the background, so abort
// and host_tool_result frames keep being read while it waits.
func (s *server) prompt(base context.Context, id string) {
	if s.script.Mode == modeExitEarly {
		os.Exit(1)
	}
	s.respond(id, "prompt", struct{}{})
	ctx, cancel := context.WithCancel(base)
	n := s.seq.Add(1)
	s.mu.Lock()
	s.activeAbort[n] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.activeAbort, n)
			s.mu.Unlock()
			cancel()
		}()
		status := s.turn(ctx)
		s.emit(omprpc.PromptResultEvent{AgentInvoked: true, Status: status, SessionSettled: true, ID: &id})
	}()
}

// turn plays one agent run and reports how it ended.
func (s *server) turn(ctx context.Context) omprpc.PromptStatus {
	if s.script.Mode == modeHang {
		<-ctx.Done()
		return omprpc.PromptStatusAborted
	}
	s.emit(omprpc.AgentStartEvent{})
	message := "fakeomp working"
	if s.echo != "" {
		message += ": " + s.echo
	}
	s.emit(omprpc.NoticeEvent{Level: omprpc.NotifyTypeInfo, Message: message, Source: new("fakeomp")})

	for _, c := range s.script.Calls {
		if !s.callTool(ctx, c.Tool, c.Args) {
			return omprpc.PromptStatusAborted
		}
	}
	if s.script.Mode != modeNoFinish {
		finish := maps.Clone(s.script.Finish)
		if s.echo != "" {
			var summary string
			_ = json.Unmarshal(finish["summary"], &summary)
			finish["summary"], _ = json.Marshal(summary + " " + s.echo)
		}
		args, _ := json.Marshal(finish)
		if !s.callTool(ctx, "finish", args) {
			return omprpc.PromptStatusAborted
		}
	}
	if err := s.appendSession(`{"type":"message","text":"fake turn"}`); err != nil {
		fmt.Fprintln(os.Stderr, "fakeomp:", err)
	}
	s.emit(omprpc.AgentEndEvent{Messages: []omprpc.AgentMessage{}})
	return omprpc.PromptStatusCompleted
}

// callTool issues one host tool call and waits for its result; false means
// the run was aborted while waiting.
func (s *server) callTool(ctx context.Context, tool string, args json.RawMessage) bool {
	arguments := map[string]json.RawMessage{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil || arguments == nil {
			arguments = map[string]json.RawMessage{}
		}
	}
	n := s.seq.Add(1)
	toolCallID := fmt.Sprintf("call_%d", n)
	requestID := fmt.Sprintf("ht_%d", n)

	shown := maps.Clone(arguments)
	if s.echo != "" {
		shown["fakeomp_echo"], _ = json.Marshal(s.echo)
	}
	shownArgs, _ := json.Marshal(shown)

	ch := make(chan omprpc.HostToolResult, 1)
	s.mu.Lock()
	s.waiting[requestID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiting, requestID)
		s.mu.Unlock()
	}()

	s.emit(omprpc.ToolExecutionStartEvent{ToolCallID: toolCallID, ToolName: tool, Args: shownArgs})
	s.emit(omprpc.HostToolCallRequest{ID: requestID, ToolCallID: toolCallID, ToolName: tool, Arguments: arguments})
	select {
	case result := <-ch:
		raw, _ := json.Marshal(result.Result)
		isError := result.IsError
		if isError == nil {
			isError = result.Result.IsError
		}
		s.emit(omprpc.ToolExecutionEndEvent{ToolCallID: toolCallID, ToolName: tool, Result: raw, IsError: isError})
		return true
	case <-ctx.Done():
		return false
	}
}
