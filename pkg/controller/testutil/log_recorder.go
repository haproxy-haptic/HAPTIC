// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package testutil

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
)

// LogRecorder keeps every record a logger emits, at every level, for tests
// that assert the level a message was logged at.
type LogRecorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// NewRecordingLogger returns a logger whose records the returned recorder keeps.
func NewRecordingLogger() (*slog.Logger, *LogRecorder) {
	recorder := &LogRecorder{}
	return slog.New(slog.NewJSONHandler(recorder, &slog.HandlerOptions{Level: slog.LevelDebug})), recorder
}

func (r *LogRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

// Levels returns the level of each record whose message starts with prefix,
// nil when there is none.
func (r *LogRecorder) Levels(prefix string) []slog.Level {
	records := r.Records(prefix)
	if len(records) == 0 {
		return nil
	}
	levels := make([]slog.Level, 0, len(records))
	for _, raw := range records {
		var record struct {
			Level slog.Level `json:"level"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			panic(err)
		}
		levels = append(levels, record.Level)
	}
	return levels
}

// Records returns each record whose message starts with prefix, as JSON.
func (r *LogRecorder) Records(prefix string) []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var records []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(r.buf.Bytes()))
	for decoder.More() {
		var raw json.RawMessage
		var record struct {
			Msg string `json:"msg"`
		}
		if err := decoder.Decode(&raw); err != nil {
			panic(err)
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			panic(err)
		}
		if strings.HasPrefix(record.Msg, prefix) {
			records = append(records, raw)
		}
	}
	return records
}
