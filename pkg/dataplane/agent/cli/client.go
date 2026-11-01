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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haproxytech/client-native/v6/runtime"
	"github.com/haproxytech/client-native/v6/runtime/options"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// experimentalPrefix opens every worker session: `add backend`, `publish` and
// `del backend` are documented experimental and the setting is per connection.
const experimentalPrefix = "experimental-mode on"

// PayloadTerminator ends a payload block. HAProxy's default terminator is an
// empty line, which a blank line inside a certificate or a map would trip; a
// custom pattern (max 7 characters) is what makes the content irrelevant to
// the framing.
const PayloadTerminator = "HAPTIC"

// lineReserve leaves room for the experimental prefix and for the
// `set severity-output number;` prologue client-native puts on every line.
const lineReserve = 64

// Config names the worker and master sockets the agent talks to.
type Config struct {
	WorkerSocket string
	MasterSocket string
	Logger       *slog.Logger
}

// Client is the agent's runtime plumbing.
type Client struct {
	cfg      Config
	worker   *runtime.SingleRuntime
	masterRT *runtime.SingleRuntime
	master   func() (runtime.Runtime, error)
	logger   *slog.Logger
}

// New wires both sockets without probing them: readiness is the server's job,
// and the agent must come up before HAProxy answers.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.WorkerSocket == "" {
		return nil, errors.New("worker socket is required")
	}
	if cfg.MasterSocket == "" {
		return nil, errors.New("master socket is required")
	}
	deferProbe := options.RuntimeOptions{DoNotCheckRuntimeOnInit: true}
	worker := &runtime.SingleRuntime{}
	if err := worker.Init(cfg.WorkerSocket, false, deferProbe); err != nil {
		return nil, fmt.Errorf("worker socket %s: %w", cfg.WorkerSocket, err)
	}
	masterRT := &runtime.SingleRuntime{}
	if err := masterRT.Init(cfg.MasterSocket, true, deferProbe); err != nil {
		return nil, fmt.Errorf("master socket %s: %w", cfg.MasterSocket, err)
	}
	// client-native's initWithMasterSocket probes even with DoNotCheckRuntimeOnInit.
	master := sync.OnceValues(func() (runtime.Runtime, error) {
		return runtime.New(ctx, options.MasterSocket(cfg.MasterSocket), options.DoNotCheckRuntimeOnInit)
	})
	return &Client{cfg: cfg, worker: worker, masterRT: masterRT, master: master, logger: cfg.Logger}, nil
}

// Info reads the worker's process ID and microsecond-resolution start time.
func (c *Client) Info() (api.HAProxyInfo, error) {
	raw, err := c.worker.ExecuteRaw("show info float")
	if err != nil {
		return api.HAProxyInfo{}, fmt.Errorf("show info: %w", err)
	}
	return parseInfo(raw)
}

func parseInfo(raw string) (api.HAProxyInfo, error) {
	info := api.HAProxyInfo{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Version":
			info.Version, info.FullVersion = value, value
		case "Pid":
			info.WorkerPID, _ = strconv.Atoi(value)
		case "Start_time_sec":
			start, err := time.ParseDuration(value + "s")
			if err != nil || start <= 0 {
				return api.HAProxyInfo{}, fmt.Errorf("show info: invalid worker start time %q", value)
			}
			info.WorkerStartTimeUnixMicros = start.Microseconds()
		}
	}
	if !info.HasWorkerIdentity() {
		return api.HAProxyInfo{}, errors.New("show info: missing or invalid worker identity")
	}
	return info, nil
}

// ShowProc asks the master process for its worker table. It is the master
// socket's readiness gate and nothing else.
func (c *Client) ShowProc() (string, error) {
	out, err := c.masterRT.ExecuteMaster("show proc")
	if err != nil {
		return "", fmt.Errorf("show proc: %w", err)
	}
	return out, nil
}

// Reload asks the master to re-exec. The returned string is HAProxy's startup
// log, which is what a NACK carries back to the operator.
func (c *Client) Reload() (string, error) {
	master, err := c.master()
	if err != nil {
		return "", fmt.Errorf("master socket %s: %w", c.cfg.MasterSocket, err)
	}
	return master.Reload()
}

// Raw runs one command on the worker socket with the experimental prefix.
func (c *Client) Raw(command string) (string, error) {
	return c.worker.ExecuteRaw(experimentalPrefix + ";" + command)
}

// Execute runs the programs of one apply in the order the controller composed
// them and returns one result per program. It stops at the first failure: the
// caller's answer to a rejected op is to reload the desired set, which makes
// the commands after the failure irrelevant.
func (c *Client) Execute(programs []Program) ([]api.OpResult, error) {
	if len(programs) > api.MaxOpsPerApply {
		return nil, fmt.Errorf("%d ops exceed the %d-op limit", len(programs), api.MaxOpsPerApply)
	}
	run := &execution{client: c, results: make([]api.OpResult, len(programs)), failed: -1}
	for i, p := range programs {
		run.results[i] = api.OpResult{Kind: p.Kind, OK: true}
	}
	err := run.all(programs)
	if err != nil && run.failed >= 0 {
		c.abort(programs[run.failed])
	}
	return run.results, err
}

// logCommand records one runtime command and HAProxy's verdict at debug level.
// Only the verb and its target are logged: map keys and values can be API keys,
// and a certificate payload carries its private key.
func (c *Client) logCommand(cmd Command, result CommandResult) {
	command := commandTarget(cmd.Text)
	if result.Err != nil {
		c.logger.Debug("runtime command refused", "command", command, "payload_bytes", len(cmd.Payload), "error", result.Err)
		return
	}
	c.logger.Debug("runtime command applied", "command", command, "payload_bytes", len(cmd.Payload))
}

// commandTarget keeps a command's verb, object and target: `set map <path>`,
// `add map @<version> <path>`, `set server <backend/server>`.
func commandTarget(text string) string {
	fields := strings.Fields(text)
	keep := min(3, len(fields))
	if keep == 3 && strings.HasPrefix(fields[2], "@") && len(fields) > 3 {
		keep = 4
	}
	return strings.Join(fields[:keep], " ")
}

func (c *Client) abort(p Program) {
	for _, cmd := range p.Abort {
		if _, err := c.Raw(cmd.Text); err != nil {
			c.logger.Warn("cleanup command failed", "command", cmd.Text, "error", err)
		}
	}
}

// execution carries the state of one Execute call: the pending line, the map
// version a `prepare map` allocated, and the per-op verdicts.
type execution struct {
	client  *Client
	results []api.OpResult
	pending []pendingCommand
	length  int
	version string
	failed  int
}

type pendingCommand struct {
	program int
	Command
}

func (e *execution) all(programs []Program) error {
	for i, p := range programs {
		if err := e.program(i, p); err != nil {
			return err
		}
	}
	return e.flush()
}

func (e *execution) program(index int, p Program) error {
	for _, cmd := range p.Commands {
		cmd.Text = strings.ReplaceAll(cmd.Text, VersionPlaceholder, e.version)
		var err error
		if cmd.Repeat {
			err = e.repeat(index, cmd)
		} else {
			err = e.add(index, cmd)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// repeat runs a command until HAProxy refuses it, which is how a key with
// duplicates is removed on every supported version. The first refusal is the
// op's verdict only when nothing was removed at all.
func (e *execution) repeat(index int, cmd Command) error {
	if err := e.flush(); err != nil {
		return err
	}
	removed := 0
	var last CommandResult
	for removed < api.MaxMapDelRepeat {
		result, err := e.once(index, cmd)
		if err != nil {
			return err
		}
		if result.Err != nil {
			last = result
			break
		}
		removed++
	}
	if removed == 0 {
		return e.record(index, last)
	}
	if removed == api.MaxMapDelRepeat {
		return e.record(index, CommandResult{
			Output: cmd.Text,
			Err:    fmt.Errorf("%w: still not exhausted after %d calls", ErrUnreadableResponse, removed),
		})
	}
	return nil
}

func (e *execution) once(index int, cmd Command) (CommandResult, error) {
	raw, err := e.client.worker.ExecuteRaw(joinLine([]pendingCommand{{program: index, Command: cmd}}))
	if err != nil {
		return CommandResult{}, fmt.Errorf("runtime socket: %w", err)
	}
	result := matchBatch(raw, []Command{{Text: experimentalPrefix, Optional: true}, cmd})[1]
	e.client.logCommand(cmd, result)
	return result, nil
}

// add appends a command to the pending line, flushing around the commands that
// cannot share one: a payload command must be last on its line, and a capturing
// command's answer decides the text of the commands after it.
func (e *execution) add(index int, cmd Command) error {
	solo := cmd.Payload != "" || cmd.Capture
	if solo || e.length+len(cmd.Text) >= api.MaxCommandLineBytes-lineReserve {
		if err := e.flush(); err != nil {
			return err
		}
	}
	e.pending = append(e.pending, pendingCommand{program: index, Command: cmd})
	e.length += len(cmd.Text) + 1
	if solo {
		return e.flush()
	}
	return nil
}

// flush sends the pending line and attributes its response.
func (e *execution) flush() error {
	if len(e.pending) == 0 {
		return nil
	}
	pending := e.pending
	e.pending, e.length = nil, 0

	raw, err := e.client.worker.ExecuteRaw(joinLine(pending))
	if err != nil {
		return fmt.Errorf("runtime socket: %w", err)
	}
	commands := make([]Command, 0, len(pending)+1)
	commands = append(commands, Command{Text: experimentalPrefix, Optional: true})
	for _, p := range pending {
		commands = append(commands, p.Command)
	}
	results := matchBatch(raw, commands)[1:]
	var first error
	for i, r := range results {
		e.client.logCommand(pending[i].Command, r)
		if pending[i].Capture && r.Err == nil {
			e.version = mapVersion(r.Output)
		}
		if err := e.record(pending[i].program, r); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// record marks an op failed. Every command after the first failure is recorded
// too, because its outcome is unknown, not successful.
func (e *execution) record(program int, r CommandResult) error {
	if r.Err == nil {
		return nil
	}
	if e.failed < 0 {
		e.failed = program
	}
	e.results[program] = api.OpResult{Kind: e.results[program].Kind, OK: false, Output: r.Output}
	return fmt.Errorf("op %s: %w: %s", e.results[program].Kind, r.Err, r.Output)
}

// joinLine renders one worker line: the experimental prefix, the commands
// joined by ';' and, when the last one carries a payload, its heredoc. The
// caller's transport appends the newline that ends the terminator line.
func joinLine(pending []pendingCommand) string {
	var b strings.Builder
	b.WriteString(experimentalPrefix)
	for _, p := range pending {
		b.WriteByte(';')
		b.WriteString(p.Text)
	}
	last := pending[len(pending)-1]
	if last.Payload != "" {
		b.WriteString(" <<" + PayloadTerminator + "\n")
		b.WriteString(last.Payload)
		if !strings.HasSuffix(last.Payload, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(PayloadTerminator)
	}
	return b.String()
}

// mapVersion reads the version out of "New version created: <n>".
func mapVersion(out string) string {
	_, after, found := strings.Cut(out, ":")
	if !found {
		return ""
	}
	if _, again, more := strings.Cut(after, ":"); more {
		after = again
	}
	version := strings.TrimSpace(after)
	if _, err := strconv.ParseUint(version, 10, 64); err != nil {
		return ""
	}
	return version
}
