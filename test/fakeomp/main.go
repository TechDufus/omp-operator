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
// # Per-turn scripts
//
// A prompt whose message starts with "FAKEOMP:" carries the script of that one turn as JSON
// after the prefix, and replaces the FAKEOMP_SCRIPT calls and finish for it:
//
//	{"mode": "normal|hang|exit-early", "files": {"<relative path>": "<content>"},
//	 "checkFiles": ["<path>"], "checkEnv": ["<NAME>"], "reply": "<text>",
//	 "ask": {"questions": [{"id": "<key>", "question": "<text>", "options": [{"label": "<text>"}], "multi": false}]}}
//
// The turn writes "files" into the working directory, then writes fakeomp-check.txt there:
// "cwd: <dir>", one "<path>: present|absent" line per checkFiles entry (relative paths are
// resolved against the working directory) and one "<NAME>: set|unset" line per checkEnv
// entry. It never calls finish. Every turn, scripted or not, makes get_last_assistant_text
// answer "reply" (default "fake reply"), followed by a space and the value of the
// FAKEOMP_ECHO_ENV variable when that is set; the same text is sent as an assistant
// message_end event.
//
// "ask" makes the turn call omp's ask tool once, after the files: fakeomp sends the one
// extension_ui_request of method "ask" that real omp sends in rpc-ui mode (all questions in
// it) and waits for the extension_ui_response (an abort ends the wait). It does so only after
// the host enabled it with set_ask_dialog; otherwise the turn fails, as nothing would ask. The
// answers go into the reply as " answers: <id>=<selected|custom>; ..." (selected labels joined
// by "," and free text appended after "+"; "answers: cancelled" for a cancelled dialog). Like
// real omp, a single-select question answered with both an option and free text is an error,
// which the reply reports as "answers: error: <id> is single-select but carries more than one answer".
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
)

const (
	modeNormal    = "normal"
	modeNoFinish  = "no-finish"
	modeGarbage   = "garbage"
	modeExitEarly = "exit-early"
	modeHang      = "hang"

	defaultReply = "fake reply"
	scriptPrefix = "FAKEOMP:"
	checkFile    = "fakeomp-check.txt"

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

// turnScript is the script of one turn, given in the prompt itself.
type turnScript struct {
	Mode       string            `json:"mode"`
	Files      map[string]string `json:"files"`
	CheckFiles []string          `json:"checkFiles"`
	CheckEnv   []string          `json:"checkEnv"`
	Reply      string            `json:"reply"`
	Ask        *askScript        `json:"ask"`
}

// askScript is the ask tool call of a turn, in the shape of omp's ask tool arguments.
type askScript struct {
	Questions []askQuestion `json:"questions"`
}

type askQuestion struct {
	ID       string      `json:"id"`
	Question string      `json:"question"`
	Options  []askOption `json:"options"`
	Multi    bool        `json:"multi"`
}

type askOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// uiResponse is the part of an extension_ui_response frame fakeomp reads.
type uiResponse struct {
	ID        string `json:"id"`
	Cancelled bool   `json:"cancelled"`
	Answers   []struct {
		ID              string   `json:"id"`
		SelectedOptions []string `json:"selectedOptions"`
		CustomInput     *string  `json:"customInput"`
	} `json:"answers"`
}

func validMode(mode string) bool {
	switch mode {
	case "", modeNormal, modeNoFinish, modeGarbage, modeExitEarly, modeHang:
		return true
	}
	return false
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

	mu        sync.Mutex
	hostTools []string
	// hostToolsSet records that the client registered host tools, even none.
	hostToolsSet bool
	lastReply    *string
	sessionDir   string
	// sessionPath is the file open_session chose to append to; empty means fake.jsonl in sessionDir.
	sessionPath string
	waiting     map[string]chan omprpc.HostToolResult
	activeAbort map[int64]context.CancelFunc
	// askDialog is set by set_ask_dialog; uiWaiting holds the extension UI requests awaiting a response.
	askDialog bool
	uiWaiting map[string]chan uiResponse

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
		uiWaiting:   map[string]chan uiResponse{},
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
		s.hostTools, s.hostToolsSet = names, true
		s.mu.Unlock()
		s.respond(frame.ID, frame.Type, omprpc.SetHostToolsResult{ToolNames: names})
	case "get_state":
		s.respond(frame.ID, frame.Type, s.state())
	case "open_session":
		s.openSession(frame.ID, line)
	case "prompt":
		s.prompt(ctx, frame.ID, line)
	case "get_last_assistant_text":
		s.mu.Lock()
		text := s.lastReply
		s.mu.Unlock()
		s.respond(frame.ID, frame.Type, omprpc.GetLastAssistantTextResult{Text: text})
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
	case "set_ask_dialog":
		var params omprpc.SetAskDialogCommand
		if err := json.Unmarshal(line, &params); err != nil {
			s.fail(frame.ID, frame.Type, "invalid_params", err.Error())
			return
		}
		s.mu.Lock()
		s.askDialog = params.Enabled
		s.mu.Unlock()
		s.respond(frame.ID, frame.Type, omprpc.SetAskDialogResult(params))
	case "extension_ui_response":
		var resp uiResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			fmt.Fprintln(os.Stderr, "fakeomp: bad extension_ui_response:", err)
			return
		}
		s.mu.Lock()
		ch := s.uiWaiting[resp.ID]
		delete(s.uiWaiting, resp.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
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

// realSession returns the newest *.jsonl of dir that is a real omp session, one whose first
// lines hold a {"type":"session","id":...} header, such as the conversation a handoff
// restores, and that header's id. Like omp, open_session continues it instead of starting a
// session of its own. Both are empty when there is none.
func realSession(dir string) (path, id string) {
	var newest time.Time
	for _, file := range sessionFiles(dir) {
		hdr := sessionHeaderID(file)
		if hdr == "" {
			continue
		}
		if st, err := os.Stat(file); err == nil && (path == "" || st.ModTime().After(newest)) {
			path, id, newest = file, hdr, st.ModTime()
		}
	}
	return path, id
}

// sessionHeaderID is the id of the session header among the first lines of file, or "".
func sessionHeaderID(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for range 20 {
		if !sc.Scan() {
			break
		}
		var hdr struct{ Type, ID string }
		if json.Unmarshal(sc.Bytes(), &hdr) == nil && hdr.Type == "session" {
			return hdr.ID
		}
	}
	return ""
}

func (s *server) openSession(id string, line []byte) {
	var params omprpc.OpenSessionCommand
	if err := json.Unmarshal(line, &params); err != nil || params.SessionDir == "" {
		s.fail(id, "open_session", "invalid_params", "sessionDir is required")
		return
	}
	resumed := len(sessionFiles(params.SessionDir)) > 0
	path, realID := realSession(params.SessionDir)
	s.mu.Lock()
	s.sessionDir, s.sessionPath = params.SessionDir, path
	s.mu.Unlock()
	sid, file := sessionID, filepath.Join(params.SessionDir, sessionFile)
	if path != "" {
		sid, file = realID, path
	} else if err := s.appendSession(`{"type":"session","id":"` + sessionID + `"}`); err != nil {
		s.fail(id, "open_session", "io_error", err.Error())
		return
	}
	s.respond(id, "open_session", omprpc.OpenSessionResult{
		Resumed:     resumed,
		SessionID:   sid,
		SessionFile: new(file),
	})
}

// appendSession appends a line to the session file open_session chose (fake.jsonl in the
// session directory by default); it does nothing without a session directory. In a resumed
// real session the line becomes a custom entry chained to the last one, which omp loads and
// ignores; a bare line would make omp fail to load the session.
func (s *server) appendSession(line string) error {
	s.mu.Lock()
	dir, path := s.sessionDir, s.sessionPath
	s.mu.Unlock()
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if path == "" {
		path = filepath.Join(dir, sessionFile)
	} else {
		entry, err := customEntry(path, line)
		if err != nil {
			return err
		}
		line = entry
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")
	return errors.Join(err, f.Close())
}

// customEntry wraps text in an omp custom session entry whose parent is the newest entry of
// the session file.
func customEntry(path, text string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var parent any // JSON null when the file has no entry yet
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range slices.Backward(lines) {
		var last struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &last) == nil && last.ID != "" && last.Type != "session" {
			parent = last.ID
			break
		}
	}
	now := time.Now()
	out, err := json.Marshal(map[string]any{
		"type": "custom", "customType": "fakeomp-turn", "data": text,
		"id": fmt.Sprintf("%08x", uint32(now.UnixNano())), "parentId": parent,
		"timestamp": now.UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	return string(out), err
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
func (s *server) prompt(base context.Context, id string, line []byte) {
	var cmd omprpc.PromptCommand
	if err := json.Unmarshal(line, &cmd); err != nil {
		s.fail(id, "prompt", "invalid_params", err.Error())
		return
	}
	var ts *turnScript
	if raw, ok := strings.CutPrefix(cmd.Message, scriptPrefix); ok {
		ts = &turnScript{}
		if err := json.Unmarshal([]byte(raw), ts); err != nil {
			s.fail(id, "prompt", "invalid_params", scriptPrefix+" "+err.Error())
			return
		}
		if !validMode(ts.Mode) {
			s.fail(id, "prompt", "invalid_params", fmt.Sprintf("%s unknown mode %q", scriptPrefix, ts.Mode))
			return
		}
	}
	mode := s.script.Mode
	if ts != nil && ts.Mode != "" {
		mode = ts.Mode
	}
	if mode == modeExitEarly {
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
		status, perr := s.turn(ctx, ts, mode)
		s.emit(omprpc.PromptResultEvent{AgentInvoked: true, Status: status, SessionSettled: true, ID: &id, Error: perr})
	}()
}

// turn plays one agent run and reports how it ended. ts is the turn's own script, when the
// prompt carried one: such a turn runs the script instead of the FAKEOMP_SCRIPT calls and finish.
func (s *server) turn(ctx context.Context, ts *turnScript, mode string) (omprpc.PromptStatus, *omprpc.PromptError) {
	if mode == modeHang {
		<-ctx.Done()
		return omprpc.PromptStatusAborted, nil
	}
	s.emit(omprpc.AgentStartEvent{})
	message := "fakeomp working"
	if s.echo != "" {
		message += ": " + s.echo
	}
	s.emit(omprpc.NoticeEvent{Level: omprpc.NotifyTypeInfo, Message: message, Source: new("fakeomp")})

	reply := defaultReply
	if ts != nil {
		if ts.Reply != "" {
			reply = ts.Reply
		}
		if err := runTurnScript(ts); err != nil {
			return omprpc.PromptStatusError, &omprpc.PromptError{Message: "fakeomp: " + err.Error()}
		}
		if ts.Ask != nil {
			text, aborted, err := s.ask(ctx, ts.Ask)
			switch {
			case aborted:
				return omprpc.PromptStatusAborted, nil
			case err != nil:
				return omprpc.PromptStatusError, &omprpc.PromptError{Message: "fakeomp: " + err.Error()}
			}
			reply += " " + text
		}
	} else if !s.runLegacy(ctx, mode) {
		return omprpc.PromptStatusAborted, nil
	}
	if s.echo != "" {
		reply += " " + s.echo
	}
	s.mu.Lock()
	s.lastReply = &reply
	s.mu.Unlock()

	if err := s.appendSession(`{"type":"message","text":"fake turn"}`); err != nil {
		fmt.Fprintln(os.Stderr, "fakeomp:", err)
	}
	assistant := omprpc.AgentMessage{Value: omprpc.AssistantMessage{
		Content:    []omprpc.AssistantContent{{Value: omprpc.TextContent{Text: reply}}},
		API:        "fake",
		Provider:   "fake",
		Model:      "fake-model",
		StopReason: omprpc.StopReasonStop,
	}}
	s.emit(omprpc.MessageEndEvent{Message: assistant})
	s.emit(omprpc.AgentEndEvent{Messages: []omprpc.AgentMessage{assistant}})
	return omprpc.PromptStatusCompleted, nil
}

// runLegacy plays the FAKEOMP_SCRIPT calls and finish; false means the run was aborted.
func (s *server) runLegacy(ctx context.Context, mode string) bool {
	for _, c := range s.script.Calls {
		if !s.callTool(ctx, c.Tool, c.Args) {
			return false
		}
	}
	if mode == modeNoFinish || !s.finishRegistered() {
		return true
	}
	finish := maps.Clone(s.script.Finish)
	if s.echo != "" {
		var summary string
		_ = json.Unmarshal(finish["summary"], &summary)
		finish["summary"], _ = json.Marshal(summary + " " + s.echo)
	}
	args, _ := json.Marshal(finish)
	return s.callTool(ctx, "finish", args)
}

// finishRegistered is false only when the client registered host tools and finish is not among
// them, as in cloud threads; a client that registered none gets finish calls as ever.
func (s *server) finishRegistered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.hostToolsSet || slices.Contains(s.hostTools, "finish")
}

// runTurnScript writes the script's files into the working directory, then its check file.
func runTurnScript(ts *turnScript) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	for name, content := range ts.Files {
		if !filepath.IsLocal(name) {
			return fmt.Errorf("file %q must be relative and stay inside the working directory", name)
		}
		path := filepath.Join(cwd, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if len(ts.CheckFiles) == 0 && len(ts.CheckEnv) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("cwd: " + cwd + "\n")
	for _, name := range ts.CheckFiles {
		state := "absent"
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, name)
		}
		if _, err := os.Stat(path); err == nil {
			state = "present"
		}
		b.WriteString(name + ": " + state + "\n")
	}
	for _, name := range ts.CheckEnv {
		state := "unset"
		if _, ok := os.LookupEnv(name); ok {
			state = "set"
		}
		b.WriteString(name + ": " + state + "\n")
	}
	return os.WriteFile(filepath.Join(cwd, checkFile), []byte(b.String()), 0o644)
}

// ask sends the script's ask tool call to the host the way omp does in rpc-ui mode after
// set_ask_dialog enabled it (one extension_ui_request of method "ask" with every question),
// waits for the answer and returns it as "answers: <id>=<selected|custom>; ...". The text
// also reports what real omp would reject: a single-select question answered with both an
// option and free text. aborted is true when the run was aborted while waiting.
func (s *server) ask(ctx context.Context, a *askScript) (text string, aborted bool, err error) {
	s.mu.Lock()
	enabled := s.askDialog
	s.mu.Unlock()
	if !enabled {
		return "", false, errors.New("ask: set_ask_dialog was not enabled, so omp would not send an ask request")
	}
	questions := make([]omprpc.AskQuestion, 0, len(a.Questions))
	for _, q := range a.Questions {
		options := make([]omprpc.AskOption, 0, len(q.Options))
		for _, o := range q.Options {
			opt := omprpc.AskOption{Label: o.Label}
			if o.Description != "" {
				opt.Description = new(o.Description)
			}
			options = append(options, opt)
		}
		questions = append(questions, omprpc.AskQuestion{ID: q.ID, Question: q.Question, Options: options, Multi: q.Multi})
	}
	n := s.seq.Add(1)
	id, toolCallID := fmt.Sprintf("ui_%d", n), fmt.Sprintf("call_%d", n)
	ch := make(chan uiResponse, 1)
	s.mu.Lock()
	s.uiWaiting[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.uiWaiting, id)
		s.mu.Unlock()
	}()

	args, _ := json.Marshal(a)
	s.emit(omprpc.ToolExecutionStartEvent{ToolCallID: toolCallID, ToolName: "ask", Args: args})
	s.emit(omprpc.AskUiRequest{ID: id, Questions: questions})
	var resp uiResponse
	select {
	case resp = <-ch:
	case <-ctx.Done():
		return "", true, nil
	}
	text = describeAnswers(a, resp)
	result, _ := json.Marshal(omprpc.TextResult(text))
	s.emit(omprpc.ToolExecutionEndEvent{ToolCallID: toolCallID, ToolName: "ask", Result: result})
	return text, false, nil
}

// describeAnswers renders an ask response as the reply text.
func describeAnswers(a *askScript, resp uiResponse) string {
	if resp.Cancelled {
		return "answers: cancelled"
	}
	multi := map[string]bool{}
	for _, q := range a.Questions {
		multi[q.ID] = q.Multi
	}
	parts := make([]string, 0, len(resp.Answers))
	for _, ans := range resp.Answers {
		custom := ""
		if ans.CustomInput != nil {
			custom = *ans.CustomInput
		}
		if count := len(ans.SelectedOptions); !multi[ans.ID] && (count > 1 || (count == 1 && custom != "")) {
			return fmt.Sprintf("answers: error: %s is single-select but carries more than one answer", ans.ID)
		}
		value := strings.Join(ans.SelectedOptions, ",")
		if custom != "" {
			if value != "" {
				value += "+"
			}
			value += custom
		}
		parts = append(parts, ans.ID+"="+value)
	}
	return "answers: " + strings.Join(parts, "; ")
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
