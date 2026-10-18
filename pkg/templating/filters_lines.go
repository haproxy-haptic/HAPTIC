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

package templating

import (
	"fmt"
	"strings"

	"github.com/haproxy-haptic/scriggo/ast"
	"github.com/haproxy-haptic/scriggo/ast/astutil"
)

// scriggoOnOwnLinesStart prefixes a line break when s's first line has content,
// so s never continues the line before it.
func scriggoOnOwnLinesStart(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	if blankLine(first) {
		return s
	}
	return "\n" + s
}

// scriggoOnOwnLinesJoin appends right to left, separated by a line break when
// left's last line and right's first line both have content.
func scriggoOnOwnLinesJoin(left, right string) string {
	first, _, _ := strings.Cut(right, "\n")
	if blankLine(left[strings.LastIndexByte(left, '\n')+1:]) || blankLine(first) {
		return left + right
	}
	return left + "\n" + right
}

func blankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

// Join before render_glob loses boundaries; balance calls to bound register use.
func distributeOnOwnLines(tree *ast.Tree) error {
	d := &onOwnLinesDistributor{visited: map[*ast.Tree]struct{}{}}
	d.walk(tree)
	return d.err
}

type onOwnLinesDistributor struct {
	visited map[*ast.Tree]struct{}
	err     error
}

func (d *onOwnLinesDistributor) walk(tree *ast.Tree) {
	if tree == nil || d.err != nil {
		return
	}
	if _, seen := d.visited[tree]; seen {
		return
	}
	d.visited[tree] = struct{}{}
	astutil.Inspect(tree, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.StructType, *ast.TypeDeclaration:
			// astutil.Walk panics on these; they hold only types.
			return false
		case *ast.BinaryOperator:
			d.rewritePipe(tree.Path, n)
		case *ast.Call:
			d.rewriteCall(tree.Path, n)
		case *ast.Render:
			d.walk(n.Tree)
		case *ast.RenderGlob:
			for _, render := range n.Renders {
				d.walk(render.Tree)
			}
		case *ast.Import:
			d.walk(n.Tree)
		case *ast.Extends:
			d.walk(n.Tree)
		}
		return d.err == nil
	})
}

// rewritePipe handles `render_glob "p" | on_own_lines()`, whose call carries no
// argument: the pipe operand is the glob.
func (d *onOwnLinesDistributor) rewritePipe(path string, pipe *ast.BinaryOperator) {
	call, ok := pipe.Expr2.(*ast.Call)
	if pipe.Op != ast.OperatorPipe || !ok || !d.claim(call) {
		return
	}
	if len(call.Args) == 0 {
		pipe.Expr1 = d.join(path, call, pipe.Expr1)
		return
	}
	d.reject(path, call)
}

func (d *onOwnLinesDistributor) rewriteCall(path string, call *ast.Call) {
	if !d.claim(call) {
		return
	}
	if len(call.Args) == 1 && !call.IsVariadic {
		call.Args[0] = d.join(path, call, call.Args[0])
		return
	}
	d.reject(path, call)
}

// claim reports whether call is an on_own_lines call, and points it at the
// declared function.
func (d *onOwnLinesDistributor) claim(call *ast.Call) bool {
	ident, ok := call.Func.(*ast.Identifier)
	if !ok || ident.Name != FuncOnOwnLines {
		return false
	}
	ident.Name = funcOnOwnLinesStart
	return true
}

func (d *onOwnLinesDistributor) join(path string, call *ast.Call, operand ast.Expression) ast.Expression {
	glob, ok := operand.(*ast.RenderGlob)
	if !ok || glob.Parallel {
		d.reject(path, call)
		return operand
	}
	if len(glob.Renders) == 0 {
		return glob
	}
	return joinOnOwnLines(glob, glob.Renders)
}

func (d *onOwnLinesDistributor) reject(path string, call *ast.Call) {
	d.err = fmt.Errorf("%s:%s: %s takes one render_glob expression; write %s(render_glob \"prefix-*\")",
		path, call.Pos(), FuncOnOwnLines, FuncOnOwnLines)
}

func joinOnOwnLines(glob *ast.RenderGlob, renders []*ast.Render) ast.Expression {
	if len(renders) == 1 {
		return renders[0]
	}
	half := len(renders) / 2
	position := *glob.Position
	return ast.NewCall(&position, ast.NewIdentifier(&position, funcOnOwnLinesJoin),
		[]ast.Expression{joinOnOwnLines(glob, renders[:half]), joinOnOwnLines(glob, renders[half:])}, false)
}
