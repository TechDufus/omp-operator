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

package driver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

const (
	// maxEventText is the longest event text the API accepts.
	maxEventText = 500
	// maxToolArgs is how much of a tool call's arguments an event shows.
	maxToolArgs = 200
	// maxBuffered caps the events held between flushes; the oldest are dropped.
	maxBuffered = 500
)

// Event kinds the driver emits.
const (
	eventTool       = "tool"
	eventNotice     = "notice"
	eventRetry      = "retry"
	eventNudge      = "nudge"
	eventTranscript = "transcript"
	eventContinuity = "continuity"
)

// eventSink batches progress events and posts them every interval. Everything it
// accepts is redacted first.
type eventSink struct {
	api   *Client
	red   *Redactor
	every time.Duration
	log   *slog.Logger

	mu   sync.Mutex
	buf  []agentapi.Event
	stop chan struct{}
	done chan struct{}
}

func newEventSink(api *Client, red *Redactor, every time.Duration, log *slog.Logger) *eventSink {
	return &eventSink{api: api, red: red, every: every, log: log, stop: make(chan struct{}), done: make(chan struct{})}
}

// run flushes on a timer until stop is closed, then flushes once more.
func (s *eventSink) run() {
	defer close(s.done)
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.flush()
		case <-s.stop:
			s.flush()
			return
		}
	}
}

// close flushes what is buffered and stops the timer.
func (s *eventSink) close() {
	close(s.stop)
	<-s.done
}

func (s *eventSink) add(kind, text string) {
	text = truncate(s.red.String(text), maxEventText)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) >= maxBuffered {
		s.buf = s.buf[1:]
	}
	s.buf = append(s.buf, agentapi.Event{At: time.Now().UTC(), Kind: kind, Text: text})
}

func (s *eventSink) flush() {
	s.mu.Lock()
	events := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(events) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	if err := s.api.Events(ctx, events); err != nil {
		s.log.Warn("post events", "error", err)
	}
}

// drain turns omp's frames into events until the connection ends.
func (s *eventSink) drain(frames <-chan omprpc.RpcServerFrame) {
	for frame := range frames {
		switch ev := frame.Value.(type) {
		case omprpc.ToolExecutionStartEvent:
			args := truncate(s.red.String(string(ev.Args)), maxToolArgs)
			s.add(eventTool, strings.TrimSpace(ev.ToolName+" "+args))
		case omprpc.NoticeEvent:
			s.add(eventNotice, ev.Message)
		case omprpc.AutoRetryStartEvent:
			s.add(eventRetry, fmt.Sprintf("retry %d/%d in %dms: %s", ev.Attempt, ev.MaxAttempts, ev.DelayMs, ev.ErrorMessage))
		}
	}
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	if len(s) <= n { // fewer bytes than n implies fewer runes
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
