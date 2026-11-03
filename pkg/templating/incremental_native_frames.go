// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package templating

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/builtin"
	"github.com/haproxy-haptic/scriggo/native"
)

var incrementalNativeFunctionFrameTrampolines = newIncrementalNativeFunctionFrameTrampolines()
var incrementalRootFunctionFrameTrampolines = newIncrementalRootFunctionFrameTrampolines()

func newIncrementalNativeFunctionFrameTrampolines() []*native.FunctionTrampoline {
	trampolines := incrementalNativeMethodFrameTrampolines()
	trampolines = append(trampolines, incrementalRootFunctionFrameTrampolines...)
	declarations := buildScriggoIncrementalGlobals(nil, nil)
	names := make([]string, 0, len(declarations))
	for name := range declarations {
		names = append(names, name)
	}
	slices.Sort(names)
	seen := make(map[reflect.Value]struct{}, len(names))
	for _, name := range names {
		declaration := declarations[name]
		if synchronous, ok := declaration.(native.SynchronousDeclaration); ok {
			declaration = synchronous.Declaration
		}
		if adaptive, ok := declaration.(native.AdaptiveFunc); ok {
			declaration = adaptive.Impl
		}
		value := reflect.ValueOf(declaration)
		identity := incrementalResourceNativeFunctionIdentity(value)
		if !identity.IsValid() {
			continue
		}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		trampoline := makeIncrementalNativeFunctionSignatureFrameTrampoline(declaration)
		if trampoline == nil {
			continue
		}
		seen[identity] = struct{}{}
		trampolines = append(trampolines, trampoline)
	}
	return trampolines
}

func newIncrementalRootFunctionFrameTrampolines() []*native.FunctionTrampoline {
	functions := [...]any{
		scriggoIncrementalRender,
		scriggoIncrementalValues,
		scriggoIncrementalRankedFragments,
		scriggoIncrementalRankedFragmentsJoin,
		scriggoIncrementalRankedTextFragment,
		scriggoIncrementalRankedTextFragmentJoin,
	}
	trampolines := make([]*native.FunctionTrampoline, 0, len(functions))
	for _, function := range functions {
		trampoline := makeIncrementalNativeFunctionSignatureFrameTrampoline(function)
		if trampoline == nil {
			panic(fmt.Sprintf("templating: incremental root primitive %T lacks a direct frame", function))
		}
		trampolines = append(trampolines, trampoline)
	}
	return trampolines
}

func incrementalNativeMethodFrameTrampolines() []*native.FunctionTrampoline {
	return []*native.FunctionTrampoline{
		makeIncrementalSharedMethodFrameTrampoline("Unique", func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, incrementalFrameReceiver[SharedContributionContext](frame).Unique(
				frame.ArgString(0), frame.ArgString(1), frame.ArgString(2),
			))
		}),
		makeIncrementalSharedMethodFrameTrampoline("Publish", func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, incrementalFrameReceiver[SharedContributionContext](frame).Publish(
				frame.ArgString(0), frame.ArgString(1), native.FunctionCallFrameArg[any](frame, 2),
			))
		}),
		makeIncrementalSharedMethodFrameTrampoline("PublishRanked", func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, incrementalFrameReceiver[SharedContributionContext](frame).PublishRanked(
				frame.ArgString(0), frame.ArgString(1), frame.ArgString(2),
				native.FunctionCallFrameArg[any](frame, 3),
			))
		}),
		makeIncrementalSharedMethodFrameTrampoline("Select", func(frame native.FunctionCallFrame) {
			value, found := incrementalFrameReceiver[SharedContributionContext](frame).Select(
				frame.ArgString(0), frame.ArgString(1), frame.ArgString(2),
			)
			frame.SetResultValue(0, incrementalFrameValue(value))
			frame.SetResultBool(1, found)
		}),
		makeIncrementalSharedMethodFrameTrampoline("SelectValues", func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(
				incrementalFrameReceiver[SharedContributionContext](frame).SelectValues(
					frame.ArgString(0), frame.ArgString(1),
				),
			))
		}),
		makeIncrementalSharedMethodFrameTrampoline("Count", func(frame native.FunctionCallFrame) {
			frame.SetResultInt(0, int64(incrementalFrameReceiver[SharedContributionContext](frame).Count(
				frame.ArgString(0), frame.ArgString(1),
			)))
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[HTTPFetcher](memberFetch, func(frame native.FunctionCallFrame) {
			value, err := incrementalFrameReceiver[HTTPFetcher](frame).Fetch(
				incrementalFrameVariadicInterfaces(frame, 0)...,
			)
			frame.SetResultValue(0, incrementalFrameValue(value))
			incrementalFrameSetError(frame, err)
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[HTTPFetcher](memberPending, func(frame native.FunctionCallFrame) {
			frame.SetResultBool(0, incrementalFrameReceiver[HTTPFetcher](frame).Pending(frame.ArgString(0)))
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[IncrementalBackendPlanRegistrar]("Profile", func(frame native.FunctionCallFrame) {
			value, err := incrementalFrameReceiver[IncrementalBackendPlanRegistrar](frame).Profile(
				native.FunctionCallFrameArg[map[string]any](frame, 0),
			)
			frame.SetResultString(0, value)
			incrementalFrameSetError(frame, err)
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[IncrementalBackendPlanRegistrar](memberBackend, func(frame native.FunctionCallFrame) {
			value, err := incrementalFrameReceiver[IncrementalBackendPlanRegistrar](frame).Backend(
				native.FunctionCallFrameArg[map[string]any](frame, 0), frame.ArgString(1),
			)
			frame.SetResultString(0, value)
			incrementalFrameSetError(frame, err)
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[IncrementalBackendPlanRegistrar]("BackendWhenAny", func(frame native.FunctionCallFrame) {
			value, err := incrementalFrameReceiver[IncrementalBackendPlanRegistrar](frame).BackendWhenAny(
				native.FunctionCallFrameArg[map[string]any](frame, 0),
				frame.ArgString(1),
				frame.ArgString(2),
				native.FunctionCallFrameArg[[]string](frame, 3),
			)
			frame.SetResultString(0, value)
			incrementalFrameSetError(frame, err)
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[ResourceStore](memberList, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(incrementalFrameReceiver[ResourceStore](frame).List()))
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[ResourceStore](memberFetch, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(incrementalFrameReceiver[ResourceStore](frame).Fetch(
				incrementalFrameVariadicInterfaces(frame, 0)...,
			)))
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[ResourceStore](memberGetSingle, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(incrementalFrameReceiver[ResourceStore](frame).GetSingle(
				incrementalFrameVariadicInterfaces(frame, 0)...,
			)))
		}),
		makeIncrementalConcreteMethodFrameTrampoline[time.Duration]("Milliseconds", func(frame native.FunctionCallFrame) {
			frame.SetResultInt(0, incrementalFrameReceiver[time.Duration](frame).Milliseconds())
		}),
		makeIncrementalConcreteMethodFrameTrampoline[builtin.Time]("UnixNano", func(frame native.FunctionCallFrame) {
			frame.SetResultInt(0, incrementalFrameReceiver[builtin.Time](frame).UnixNano())
		}),
		makeIncrementalInterfaceMethodFrameTrampoline[error]("Error", func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, incrementalFrameReceiver[error](frame).Error())
		}),
	}
}

func makeIncrementalFunctionFrameTrampoline(
	function any,
	callFrame func(native.FunctionCallFrame),
) *native.FunctionTrampoline {
	value := reflect.ValueOf(function)
	return native.MakeFunctionTrampolineForWithFrame(value, func(args []reflect.Value) []reflect.Value {
		if value.Type().IsVariadic() {
			return value.CallSlice(args)
		}
		return value.Call(args)
	}, callFrame)
}

func makeIncrementalNativeFunctionSignatureFrameTrampoline(function any) *native.FunctionTrampoline {
	if trampoline := makeIncrementalAnyInputSignatureFrameTrampoline(function); trampoline != nil {
		return trampoline
	}
	if trampoline := makeIncrementalMixedSignatureFrameTrampoline(function); trampoline != nil {
		return trampoline
	}
	return makeIncrementalTextSignatureFrameTrampoline(function)
}

func makeIncrementalAnyInputSignatureFrameTrampoline(function any) *native.FunctionTrampoline {
	switch function := function.(type) {
	case func(native.Env, any, ...string) any:
		if reflect.ValueOf(function).Pointer() == reflect.ValueOf(incrementalDig).Pointer() {
			return makeIncrementalDigFrameTrampoline(function)
		}
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				incrementalFrameVariadicStrings(frame, 2)...,
			)))
		})
	case func(native.Env, any, string, ...string) string:
		if reflect.ValueOf(function).Pointer() == reflect.ValueOf(incrementalDigString).Pointer() {
			return makeIncrementalDigStringFrameTrampoline(function)
		}
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				frame.ArgString(2),
				incrementalFrameVariadicStrings(frame, 3)...,
			))
		})
	case func(native.Env, any, map[string]any) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				native.FunctionCallFrameArg[map[string]any](frame, 2),
			))
		})
	case func(native.Env, any, map[string]any, ...map[string]any) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				native.FunctionCallFrameArg[map[string]any](frame, 2),
				incrementalFrameVariadicMaps(frame, 3)...,
			))
		})
	case func(native.Env, any) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0), native.FunctionCallFrameArg[any](frame, 1),
			))
		})
	case func(native.Env, float64) float64:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultFloat(0, function(frame.ArgEnv(0), frame.ArgFloat(1)))
		})
	case func(native.Env, string, string, string, string, any, string) map[string]any:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0),
				frame.ArgString(1),
				frame.ArgString(2),
				frame.ArgString(3),
				frame.ArgString(4),
				native.FunctionCallFrameArg[any](frame, 5),
				frame.ArgString(6),
			)))
		})
	case func(native.Env, string, any, string, any) any:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(function(
				frame.ArgEnv(0),
				frame.ArgString(1),
				native.FunctionCallFrameArg[any](frame, 2),
				frame.ArgString(3),
				native.FunctionCallFrameArg[any](frame, 4),
			)))
		})
	case func(int, int) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(int(frame.ArgInt(0)), int(frame.ArgInt(1))))
		})
	case func(native.Env, any, any) any:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				native.FunctionCallFrameArg[any](frame, 2),
			)))
		})
	case func(native.Env, any, string) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0), native.FunctionCallFrameArg[any](frame, 1), frame.ArgString(2),
			))
		})
	case func(native.Env, any, string) any:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(function(
				frame.ArgEnv(0), native.FunctionCallFrameArg[any](frame, 1), frame.ArgString(2),
			)))
		})
	case func(native.Env, any) []string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0), native.FunctionCallFrameArg[any](frame, 1),
			)))
		})
	default:
		return nil
	}
}

func makeIncrementalMixedSignatureFrameTrampoline(function any) *native.FunctionTrampoline {
	switch function := function.(type) {
	case func(native.Env, ...any) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0), incrementalFrameVariadicInterfaces(frame, 1)...,
			))
		})
	case func(native.Env, string, ...any) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0), frame.ArgString(1), incrementalFrameVariadicInterfaces(frame, 2)...,
			))
		})
	case func(map[string]any, map[string]any) map[string]any:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				native.FunctionCallFrameArg[map[string]any](frame, 0),
				native.FunctionCallFrameArg[map[string]any](frame, 1),
			)))
		})
	case func(string) (any, error):
		return makeIncrementalStringParserFrameTrampoline(function)
	case func(string) (map[string]any, error):
		return makeIncrementalStringParserFrameTrampoline(function)
	case func(string, string) (map[string]string, error):
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			value, err := function(frame.ArgString(0), frame.ArgString(1))
			frame.SetResultValue(0, reflect.ValueOf(value))
			incrementalFrameSetError(frame, err)
		})
	case func(string) (time.Duration, error):
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			value, err := function(frame.ArgString(0))
			frame.SetResultInt(0, int64(value))
			incrementalFrameSetError(frame, err)
		})
	case func(string, int) (int, error):
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			value, err := function(frame.ArgString(0), int(frame.ArgInt(1)))
			frame.SetResultInt(0, int64(value))
			incrementalFrameSetError(frame, err)
		})
	case func(native.Env, string, string) builtin.Time:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0), frame.ArgString(1), frame.ArgString(2),
			)))
		})
	case func(native.Env, any, string, string) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				frame.ArgString(2),
				frame.ArgString(3),
			))
		})
	case func(native.Env, any, any, any) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				native.FunctionCallFrameArg[any](frame, 2),
				native.FunctionCallFrameArg[any](frame, 3),
			))
		})
	case func(native.Env, any, string, ...any) []any:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				frame.ArgString(2),
				incrementalFrameVariadicInterfaces(frame, 3)...,
			)))
		})
	default:
		return nil
	}
}

func makeIncrementalTextSignatureFrameTrampoline(function any) *native.FunctionTrampoline {
	switch function := function.(type) {
	case func(native.Env, []any) []string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0), native.FunctionCallFrameArg[[]any](frame, 1),
			)))
		})
	case func(string, string) []string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(frame.ArgString(0), frame.ArgString(1))))
		})
	case func(string, string, int) []string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgString(0), frame.ArgString(1), int(frame.ArgInt(2)),
			)))
		})
	case func(native.Env, any, any, int) []string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0),
				native.FunctionCallFrameArg[any](frame, 1),
				native.FunctionCallFrameArg[any](frame, 2),
				int(frame.ArgInt(3)),
			)))
		})
	case func(native.Env, any) map[string]string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0), native.FunctionCallFrameArg[any](frame, 1),
			)))
		})
	case func(native.Env, any) int:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultInt(0, int64(function(
				frame.ArgEnv(0), native.FunctionCallFrameArg[any](frame, 1),
			)))
		})
	case func(native.Env, string) TextFragment:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(function(
				frame.ArgEnv(0), frame.ArgString(1),
			)))
		})
	case func(native.Env, string, string) []any:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, reflect.ValueOf(function(
				frame.ArgEnv(0), frame.ArgString(1), frame.ArgString(2),
			)))
		})
	case func(native.Env, string, string) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0), frame.ArgString(1), frame.ArgString(2),
			))
		})
	case func(native.Env, string, string, string) string:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, function(
				frame.ArgEnv(0), frame.ArgString(1), frame.ArgString(2), frame.ArgString(3),
			))
		})
	case func(native.Env, string, string) TextFragment:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(function(
				frame.ArgEnv(0), frame.ArgString(1), frame.ArgString(2),
			)))
		})
	case func(native.Env, string, string, string) TextFragment:
		return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
			frame.SetResultValue(0, incrementalFrameValue(function(
				frame.ArgEnv(0), frame.ArgString(1), frame.ArgString(2), frame.ArgString(3),
			)))
		})
	default:
		return nil
	}
}

func makeIncrementalDigFrameTrampoline(
	function func(native.Env, any, ...string) any,
) *native.FunctionTrampoline {
	return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
		value, found, err := incrementalDigFrameValue(frame, 1, 2)
		if err != nil {
			incrementalStop(frame.ArgEnv(0), FuncDig, err)
			frame.SetResultZero(0)
			return
		}
		if !found || !value.IsValid() {
			frame.SetResultZero(0)
			return
		}
		frame.SetResultValue(0, value)
	})
}

func makeIncrementalDigStringFrameTrampoline(
	function func(native.Env, any, string, ...string) string,
) *native.FunctionTrampoline {
	return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
		value, found, err := incrementalDigFrameValue(frame, 1, 3)
		if err != nil {
			incrementalStop(frame.ArgEnv(0), FuncDig, err)
			frame.SetResultString(0, "")
			return
		}
		if !found || incrementalFrameValueIsNil(value) {
			frame.SetResultString(0, frame.ArgString(2))
			return
		}
		scalar, err := deterministicScalarOfValue(value)
		if err != nil {
			incrementalStop(frame.ArgEnv(0), FuncDigString, err)
			frame.SetResultString(0, "")
			return
		}
		frame.SetResultString(0, scalar.text)
	})
}

func incrementalDigFrameValue(
	frame native.FunctionCallFrame,
	objectIndex int,
	keysIndex int,
) (reflect.Value, bool, error) {
	current := frame.ArgValue(objectIndex)
	count := frame.VariadicLen()
	if count < 0 {
		keys := frame.ArgValue(keysIndex)
		for index := range keys.Len() {
			var found bool
			var err error
			current, found, err = incrementalFieldValue(current, keys.Index(index).String())
			if err != nil || !found {
				return reflect.Value{}, false, err
			}
		}
		return current, true, nil
	}
	for index := range count {
		var found bool
		var err error
		current, found, err = incrementalFieldValue(current, frame.VariadicString(index))
		if err != nil || !found {
			return reflect.Value{}, false, err
		}
	}
	return current, true, nil
}

func incrementalFrameValueIsNil(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	return value.Kind() == reflect.Interface && value.IsNil()
}

func makeIncrementalSharedMethodFrameTrampoline(
	name string,
	callFrame func(native.FunctionCallFrame),
) *native.FunctionTrampoline {
	return makeIncrementalInterfaceMethodFrameTrampoline[SharedContributionContext](name, callFrame)
}

func makeIncrementalInterfaceMethodFrameTrampoline[T any](
	name string,
	callFrame func(native.FunctionCallFrame),
) *native.FunctionTrampoline {
	return native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[T](),
		name,
		func([]reflect.Value) []reflect.Value {
			panic("incremental native interface trampoline requires a call frame")
		},
		callFrame,
	)
}

func makeIncrementalConcreteMethodFrameTrampoline[T any](
	name string,
	callFrame func(native.FunctionCallFrame),
) *native.FunctionTrampoline {
	return native.MakeMethodTrampolineWithFrame(
		reflect.TypeFor[T](),
		name,
		func([]reflect.Value) []reflect.Value {
			panic("incremental native method trampoline requires a call frame")
		},
		callFrame,
	)
}

func incrementalFrameReceiver[T any](frame native.FunctionCallFrame) T {
	return native.FunctionCallFrameReceiver[T](frame)
}

func incrementalFrameVariadicStrings(frame native.FunctionCallFrame, index int) []string {
	count := frame.VariadicLen()
	if count < 0 {
		return native.FunctionCallFrameArg[[]string](frame, index)
	}
	values := make([]string, count)
	for valueIndex := range count {
		values[valueIndex] = frame.VariadicString(valueIndex)
	}
	return values
}

func incrementalFrameVariadicInterfaces(frame native.FunctionCallFrame, index int) []any {
	count := frame.VariadicLen()
	if count < 0 {
		return native.FunctionCallFrameArg[[]any](frame, index)
	}
	values := make([]any, count)
	for valueIndex := range count {
		values[valueIndex] = native.FunctionCallFrameVariadicArg[any](frame, valueIndex)
	}
	return values
}

func incrementalFrameInterface(value reflect.Value) any {
	if !value.IsValid() || value.Kind() == reflect.Interface && value.IsNil() {
		return nil
	}
	return value.Interface()
}

func incrementalFrameValue(value any) reflect.Value {
	if value == nil {
		return reflect.Zero(reflect.TypeFor[any]())
	}
	return reflect.ValueOf(value)
}

func incrementalFrameSetError(frame native.FunctionCallFrame, err error) {
	const errorResult = 1
	if err == nil {
		frame.SetResultZero(errorResult)
		return
	}
	frame.SetResultValue(errorResult, reflect.ValueOf(err))
}

func certifyIncrementalNativeFunctionFrames(
	templates []*scriggo.Template,
	generated []*native.FunctionTrampoline,
) error {
	static := make(
		[]*native.FunctionTrampoline,
		0,
		len(incrementalNativeFunctionFrameTrampolines)+len(generated),
	)
	static = append(static, incrementalNativeFunctionFrameTrampolines...)
	static = append(static, generated...)
	if err := validateIncrementalNativeFunctionFrameTrampolines(static); err != nil {
		return err
	}
	missing := make(map[string]struct{})
	for _, template := range templates {
		if template == nil {
			missing["nil template"] = struct{}{}
			continue
		}
		collectMissingIncrementalDeclarationFrames(template, static, missing)
		collectMissingIncrementalCallableFrames(template, static, missing)
	}
	if len(missing) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(missing))
	for reason := range missing {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	return fmt.Errorf("native calls lack direct frames: %s", strings.Join(reasons, "; "))
}

func collectMissingIncrementalDeclarationFrames(
	template *scriggo.Template,
	static []*native.FunctionTrampoline,
	missing map[string]struct{},
) {
	for _, declaration := range template.UsedNativeDeclarations() {
		if declaration.Kind != scriggo.NativeDeclarationFunction {
			continue
		}
		function := declaration.Declaration
		if adaptive, ok := function.(native.AdaptiveFunc); ok {
			function = adaptive.Impl
		}
		value := reflect.ValueOf(function)
		if !value.IsValid() || value.Kind() != reflect.Func {
			missing[fmt.Sprintf("%s.%s has no function value", declaration.Package, declaration.Name)] = struct{}{}
			continue
		}
		if scriggo.NativeFunctionUsesReflection(value.Type()) &&
			!incrementalNativeFunctionHasFrame(static, value) {
			missing[fmt.Sprintf("%s.%s %v", declaration.Package, declaration.Name, value.Type())] = struct{}{}
		}
	}
}

func collectMissingIncrementalCallableFrames(
	template *scriggo.Template,
	static []*native.FunctionTrampoline,
	missing map[string]struct{},
) {
	callables := template.UsedNativeCallables()
	for index := range callables {
		callable := &callables[index]
		if incrementalNativeCallableHasFrame(static, callable) {
			continue
		}
		missing[fmt.Sprintf(
			"%s.%s %s %v.%s",
			callable.Package,
			callable.DeclarationName,
			incrementalNativeCallableKind(callable.Kind),
			callable.Receiver,
			callable.Name,
		)] = struct{}{}
	}
}

func validateIncrementalNativeFunctionFrameTrampolines(
	trampolines []*native.FunctionTrampoline,
) error {
	for index, trampoline := range trampolines {
		if trampoline == nil || !trampoline.SupportsFunctionCallFrame() {
			return fmt.Errorf("native function trampoline %d lacks a direct frame", index)
		}
	}
	return nil
}

func incrementalNativeFunctionHasFrame(
	trampolines []*native.FunctionTrampoline,
	function reflect.Value,
) bool {
	identity := incrementalResourceNativeFunctionIdentity(function)
	if !identity.IsValid() {
		return false
	}
	framed := make(map[reflect.Value]struct{}, len(trampolines))
	for _, trampoline := range trampolines {
		if trampoline.SupportsFunctionCallFrame() {
			framed[incrementalResourceNativeFunctionIdentity(trampoline.Value())] = struct{}{}
		}
	}
	_, found := framed[identity]
	return found
}

func incrementalNativeCallableHasFrame(
	trampolines []*native.FunctionTrampoline,
	callable *scriggo.UsedNativeCallable,
) bool {
	if callable.Constructed {
		return false
	}
	if callable.DeclarationName == declResources &&
		callable.Kind == scriggo.NativeCallableFunctionField {
		switch callable.Name {
		case memberAPIVersion, memberList, memberFetch, memberGetSingle:
			return true
		}
	}
	if callable.Kind != scriggo.NativeCallableMethod || callable.Receiver == nil || callable.Name == "" {
		return false
	}
	var matched *native.FunctionTrampoline
	for _, trampoline := range trampolines {
		receiver, name, method := trampoline.MethodTarget()
		if !method || name != callable.Name ||
			receiver != callable.Receiver &&
				(receiver.Kind() != reflect.Interface || !callable.Receiver.Implements(receiver)) {
			continue
		}
		if matched != nil && matched != trampoline {
			return false
		}
		matched = trampoline
	}
	return matched != nil && matched.SupportsFunctionCallFrame()
}

func incrementalNativeCallableKind(kind scriggo.NativeCallableKind) string {
	switch kind {
	case scriggo.NativeCallableMethod:
		return "method"
	case scriggo.NativeCallableFunctionField:
		return "function-field"
	case scriggo.NativeCallableIndexedFunction:
		return "indexed-function"
	case scriggo.NativeCallableFunctionResult:
		return "function-result"
	default:
		return fmt.Sprintf("callable-%d", kind)
	}
}

func makeIncrementalStringParserFrameTrampoline[T any](function func(string) (T, error)) *native.FunctionTrampoline {
	return makeIncrementalFunctionFrameTrampoline(function, func(frame native.FunctionCallFrame) {
		value, err := function(frame.ArgString(0))
		frame.SetResultValue(0, incrementalFrameValue(value))
		incrementalFrameSetError(frame, err)
	})
}

func incrementalFrameVariadicMaps(frame native.FunctionCallFrame, index int) []map[string]any {
	count := frame.VariadicLen()
	if count < 0 {
		return native.FunctionCallFrameArg[[]map[string]any](frame, index)
	}
	values := make([]map[string]any, count)
	for valueIndex := range count {
		values[valueIndex] = native.FunctionCallFrameVariadicArg[map[string]any](frame, valueIndex)
	}
	return values
}
