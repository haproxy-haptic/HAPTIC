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
	"errors"
	"fmt"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// ErrUnknownOp is what an op kind this agent does not execute wraps. The
// server refuses the whole batch and falls back to a reload.
var ErrUnknownOp = errors.New("unknown op kind")

// expectDone is HAProxy's answer to the commands that report completion
// rather than what they did.
const expectDone = "Done"

// VersionPlaceholder stands in for the map version `prepare map` allocates.
// The executor substitutes it in the rest of the program once the allocating
// command answered.
const VersionPlaceholder = "\x01ver\x01"

// Command is one HAProxy runtime command with the framing and the verdict rule
// that belong to it.
type Command struct {
	// Text is the command line. It never contains a newline, so a batch can
	// join commands with ';'.
	Text string
	// Payload is the block a "<<" command carries. A command with a payload is
	// always the last one on its line.
	Payload string
	// Expect is the substring HAProxy answers on success, matched
	// case-insensitively. Empty means success is any response that is not an
	// error.
	Expect string
	// Capture marks the command whose response carries the map version the rest
	// of the program substitutes into VersionPlaceholder.
	Capture bool
	// Repeat marks a command that has to run until HAProxy refuses it: `del map`
	// removes one duplicate per call on 3.4 and all of them on 3.0.
	Repeat bool
	// Optional marks the session prefix, whose response is version-dependent
	// and must never be mistaken for an op's verdict.
	Optional bool
}

// Program is the command sequence of one typed op, plus the cleanup that has
// to run when one of its commands fails. An open `set ssl cert` transaction
// wedges that certificate until a reload, so the abort is not optional.
type Program struct {
	Kind     string
	Commands []Command
	Abort    []Command
}

// Compile turns one typed op into its program. Every string that reaches
// HAProxy passes the negative-space check first.
func Compile(op *api.Op, content Content) (Program, error) {
	build, ok := compilers[op.Kind]
	if !ok {
		return Program{}, fmt.Errorf("%w: %q", ErrUnknownOp, op.Kind)
	}
	cmds, abort, err := build(op, content)
	if err != nil {
		return Program{}, fmt.Errorf("op %s: %w", op.Kind, err)
	}
	for _, c := range cmds {
		if len(c.Text) > api.MaxCommandLineBytes {
			return Program{}, fmt.Errorf("op %s: command is %d bytes, over the %d-byte line limit",
				op.Kind, len(c.Text), api.MaxCommandLineBytes)
		}
		if len(c.Payload) > api.MaxPayloadBytes {
			return Program{}, fmt.Errorf("op %s: payload is %d bytes, over the %d-byte limit",
				op.Kind, len(c.Payload), api.MaxPayloadBytes)
		}
		if err := validatePayloadBlock(c.Payload); err != nil {
			return Program{}, fmt.Errorf("op %s: %w", op.Kind, err)
		}
	}
	return Program{Kind: op.Kind, Commands: cmds, Abort: abort}, nil
}

// Content supplies the on-disk bytes of a manifest path for the ops that push
// a whole file through the socket.
type Content func(path string) ([]byte, error)

type compiler func(*api.Op, Content) (cmds, abort []Command, err error)

// compilers is the op → command table. It is the only place in the agent that
// knows HAProxy command strings.
var compilers = map[string]compiler{
	api.OpBackendAdd:           compileBackendAdd,
	api.OpBackendPublish:       compileBackendVerb("publish backend", "Backend published"),
	api.OpBackendUnpublish:     compileBackendVerb("unpublish backend", ""),
	api.OpBackendDel:           compileBackendVerb("del backend", "Backend deleted"),
	api.OpBackendWaitRemovable: compileWaitBackend,
	api.OpServerAdd:            compileServerAdd,
	api.OpServerEnable:         compileServerEnable,
	api.OpServerDisable:        compileServerVerb("disable server", ""),
	api.OpServerSetAddr:        compileServerSetAddr,
	api.OpServerSetWeight:      compileServerSetWeight,
	api.OpServerSetMaxconn:     compileServerSetMaxconn,
	api.OpServerSetState:       compileServerSetState,
	api.OpServerWaitRemovable:  compileWaitServer,
	api.OpShutdownSessions:     compileServerVerb("shutdown sessions server", ""),
	api.OpServerDel:            compileServerVerb("del server", "Server deleted"),
	api.OpMapAdd:               compileMapAdd,
	api.OpMapSet:               compileMapSet,
	api.OpMapDel:               compileMapDel,
	api.OpMapReplace:           compileMapReplace,
	api.OpCertSet:              compileCert(false),
	api.OpCertNew:              compileCert(true),
	api.OpCASet:                compileCA(false),
	api.OpCANew:                compileCA(true),
	api.OpCRTListAdd:           compileCRTListAdd,
	api.OpCRTListDel:           compileCRTListDel,
}

// Kinds lists the op kinds this agent executes, for /v1/state.agent_ops.
func Kinds() []string {
	kinds := make([]string, 0, len(compilers))
	for k := range compilers {
		kinds = append(kinds, k)
	}
	return kinds
}

func compileBackendAdd(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	if err := errors.Join(
		validateToken("backend", op.Backend),
		validateToken("profile", op.Profile),
		validateEnum("mode", op.Mode, "http", "tcp"),
	); err != nil {
		return nil, nil, err
	}
	text := fmt.Sprintf("add backend %s from %s mode %s", op.Backend, op.Profile, op.Mode)
	if op.GUID != "" {
		if err := validateToken("guid", op.GUID); err != nil {
			return nil, nil, err
		}
		text += " guid " + op.GUID
	}
	return []Command{{Text: text, Expect: "New backend registered"}}, nil, nil
}

func compileBackendVerb(verb, expect string) compiler {
	return func(op *api.Op, _ Content) (cmds, abort []Command, err error) {
		if err := validateToken("backend", op.Backend); err != nil {
			return nil, nil, err
		}
		return []Command{{Text: verb + " " + op.Backend, Expect: expect}}, nil, nil
	}
}

func compileServerVerb(verb, expect string) compiler {
	return func(op *api.Op, _ Content) (cmds, abort []Command, err error) {
		ref, err := serverRef(op)
		if err != nil {
			return nil, nil, err
		}
		return []Command{{Text: verb + " " + ref, Expect: expect}}, nil, nil
	}
}

func compileWaitBackend(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	if err := validateToken("backend", op.Backend); err != nil {
		return nil, nil, err
	}
	ms, err := waitMs(op.TimeoutMs)
	if err != nil {
		return nil, nil, err
	}
	return []Command{{Text: fmt.Sprintf("wait %d be-removable %s", ms, op.Backend), Expect: expectDone}}, nil, nil
}

func compileWaitServer(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	ref, err := serverRef(op)
	if err != nil {
		return nil, nil, err
	}
	ms, err := waitMs(op.TimeoutMs)
	if err != nil {
		return nil, nil, err
	}
	return []Command{{Text: fmt.Sprintf("wait %d srv-removable %s", ms, ref), Expect: expectDone}}, nil, nil
}

func compileServerAdd(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	ref, err := serverRef(op)
	if err != nil {
		return nil, nil, err
	}
	if err := validateToken("address", op.Address); err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	b.WriteString("add server ")
	b.WriteString(ref)
	b.WriteByte(' ')
	b.WriteString(op.Address)
	if op.Port > 0 {
		fmt.Fprintf(&b, ":%d", op.Port)
	}
	for _, kw := range op.Keywords {
		if err := writeKeyword(&b, kw); err != nil {
			return nil, nil, err
		}
	}
	return []Command{{Text: b.String(), Expect: "New server registered"}}, nil, nil
}

func compileServerEnable(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	ref, err := serverRef(op)
	if err != nil {
		return nil, nil, err
	}
	if op.Health {
		cmds = append(cmds, Command{Text: "enable health " + ref})
	}
	return append(cmds, Command{Text: "enable server " + ref}), nil, nil
}

func compileServerSetAddr(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	ref, err := serverRef(op)
	if err != nil {
		return nil, nil, err
	}
	if err := validateToken("address", op.Address); err != nil {
		return nil, nil, err
	}
	text := fmt.Sprintf("set server %s addr %s", ref, op.Address)
	if op.Port > 0 {
		text += fmt.Sprintf(" port %d", op.Port)
	}
	// HAProxy answers with what it did — "IP changed from", "port changed
	// from", "nothing changed" — at WARNING severity, the same level as its
	// refusals, so the answer is pinned on the words.
	return []Command{{Text: text, Expect: "chang"}}, nil, nil
}

func compileServerSetWeight(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	ref, err := serverRef(op)
	if err != nil {
		return nil, nil, err
	}
	if op.Weight == nil {
		return nil, nil, fmt.Errorf("%w: weight is unset", ErrUnsafeToken)
	}
	return []Command{{Text: fmt.Sprintf("set server %s weight %d", ref, *op.Weight)}}, nil, nil
}

func compileServerSetMaxconn(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	ref, err := serverRef(op)
	if err != nil {
		return nil, nil, err
	}
	if op.Maxconn == nil || *op.Maxconn < 0 {
		return nil, nil, fmt.Errorf("%w: maxconn is unset or negative", ErrUnsafeToken)
	}
	return []Command{{Text: fmt.Sprintf("set maxconn server %s %d", ref, *op.Maxconn)}}, nil, nil
}

func compileServerSetState(op *api.Op, _ Content) (cmds, abort []Command, err error) {
	ref, err := serverRef(op)
	if err != nil {
		return nil, nil, err
	}
	if err := validateEnum("state", op.State, "ready", "maint", "drain"); err != nil {
		return nil, nil, err
	}
	return []Command{{Text: fmt.Sprintf("set server %s state %s", ref, op.State)}}, nil, nil
}

func serverRef(op *api.Op) (string, error) {
	if err := errors.Join(
		validateToken("backend", op.Backend),
		validateToken("server", op.Server),
	); err != nil {
		return "", err
	}
	return op.Backend + "/" + op.Server, nil
}

func waitMs(ms int) (int, error) {
	if ms <= 0 || ms > api.MaxWaitBudgetMs {
		return 0, fmt.Errorf("%w: wait of %d ms is outside 1..%d", ErrUnsafeToken, ms, api.MaxWaitBudgetMs)
	}
	return ms, nil
}

func writeKeyword(b *strings.Builder, kw api.KeywordArg) error {
	if err := validateToken("keyword", kw.Name); err != nil {
		return err
	}
	b.WriteByte(' ')
	b.WriteString(kw.Name)
	for _, arg := range kw.Args {
		if err := validateToken("keyword argument", arg); err != nil {
			return err
		}
		b.WriteByte(' ')
		b.WriteString(arg)
	}
	return nil
}
