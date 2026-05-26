/*
Copyright 2023.

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

package runner

import (
	"container/ring"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/go-logr/logr"
)

const defaultBufferSize = 5

// DebugLog is a printf-style debug log function, matching the v3 action.DebugLog signature.
type DebugLog func(format string, v ...interface{})

// NewDebugLog creates a DebugLog that forwards to the given logr.Logger.
func NewDebugLog(log logr.Logger) DebugLog {
	return func(format string, v ...interface{}) {
		log.Info(fmt.Sprintf(format, v...))
	}
}

type LogBuffer struct {
	mu     sync.Mutex
	log    DebugLog
	buffer *ring.Ring
}

func NewLogBuffer(log DebugLog, size int) *LogBuffer {
	if size <= 0 {
		size = defaultBufferSize
	}
	return &LogBuffer{
		log:    log,
		buffer: ring.New(size),
	}
}

func (l *LogBuffer) Log(format string, v ...interface{}) {
	l.mu.Lock()

	// Filter out duplicate log lines, this happens for example when
	// Helm is waiting on workloads to become ready.
	msg := fmt.Sprintf(format, v...)
	if prev := l.buffer.Prev(); prev.Value != msg {
		l.buffer.Value = msg
		l.buffer = l.buffer.Next()
	}

	l.mu.Unlock()
	l.log(format, v...)
}

func (l *LogBuffer) Reset() {
	l.mu.Lock()
	l.buffer = ring.New(l.buffer.Len())
	l.mu.Unlock()
}

func (l *LogBuffer) String() string {
	var str string
	l.mu.Lock()
	l.buffer.Do(func(s interface{}) {
		if s == nil {
			return
		}
		str += s.(string) + "\n"
	})
	l.mu.Unlock()
	return strings.TrimSpace(str)
}

// SlogHandler returns a slog.Handler that feeds log records into this LogBuffer.
func (l *LogBuffer) SlogHandler() slog.Handler {
	return &logBufferSlogHandler{buf: l}
}

type logBufferSlogHandler struct {
	buf   *LogBuffer
	attrs []slog.Attr
}

func (h *logBufferSlogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *logBufferSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.buf.Log("%s", r.Message)
	return nil
}

func (h *logBufferSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logBufferSlogHandler{buf: h.buf, attrs: append(h.attrs, attrs...)}
}

func (h *logBufferSlogHandler) WithGroup(_ string) slog.Handler { return h }
