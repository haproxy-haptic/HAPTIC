// Copyright 2025 Philipp Hossner
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

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// ErrWorkerGone means a deferred attempt no longer addresses its original worker.
var ErrWorkerGone = errors.New("the deferred delete's HAProxy worker has changed")

const (
	workerSessionTimeout = 30 * time.Second
	maxSessionReplyBytes = 64 * 1024
	workerPrompt         = "\n> "
)

// A session never reconnects: show info, wait, shutdown and del must share a worker.
type workerSession struct {
	ctx              context.Context
	conn             net.Conn
	reader           *bufio.Reader
	stopCancellation func() bool
}

func (c *Client) openWorker(ctx context.Context, worker api.HAProxyInfo) (*workerSession, error) {
	if !worker.HasWorkerIdentity() {
		return nil, errors.New("invalid expected worker identity")
	}
	dialer := net.Dialer{Timeout: workerSessionTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.cfg.WorkerSocket)
	if err != nil {
		return nil, err
	}
	s := &workerSession{ctx: ctx, conn: conn, reader: bufio.NewReader(conn)}
	s.stopCancellation = context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := s.identify(worker); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *workerSession) identify(worker api.HAProxyInfo) error {
	for _, command := range []string{"prompt", "set severity-output number", experimentalPrefix} {
		raw, err := s.execute(command)
		if err != nil {
			return err
		}
		if result := matchBatch(raw, []Command{{Text: command}})[0]; result.Err != nil {
			return fmt.Errorf("worker session setup: %w: %s", result.Err, result.Output)
		}
	}
	raw, err := s.execute("show info float")
	if err != nil {
		return err
	}
	info, err := parseInfo(raw)
	if err != nil {
		return err
	}
	if !info.SameWorker(worker) {
		return ErrWorkerGone
	}
	return nil
}

func (s *workerSession) close() {
	s.stopCancellation()
	_ = s.conn.Close()
}

func (s *workerSession) execute(command string) (string, error) {
	return s.executeWithin(command, maxSessionReplyBytes)
}

func (s *workerSession) executeWithin(command string, maxReplyBytes int) (string, error) {
	if err := s.ctx.Err(); err != nil {
		return "", err
	}
	if err := s.conn.SetDeadline(time.Now().Add(workerSessionTimeout)); err != nil {
		return "", err
	}
	if _, err := io.WriteString(s.conn, command+"\n"); err != nil {
		return "", err
	}
	var out bytes.Buffer
	for out.Len() < maxReplyBytes {
		b, err := s.reader.ReadByte()
		if err != nil {
			return "", fmt.Errorf("incomplete worker reply: %w", err)
		}
		out.WriteByte(b)
		if bytes.HasSuffix(out.Bytes(), []byte(workerPrompt)) {
			return strings.TrimSpace(strings.TrimSuffix(out.String(), workerPrompt)), nil
		}
	}
	return "", fmt.Errorf("worker reply exceeds %d bytes", maxReplyBytes)
}
