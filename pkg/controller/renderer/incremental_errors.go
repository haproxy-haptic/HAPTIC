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

package renderer

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// attributeToWatchedObject prefixes err with the object the query evaluated.
func attributeToWatchedObject(key incremental.QueryKey, err error) error {
	source, namespace, name, ok := queryWatchedObject(key)
	if !ok {
		return fmt.Errorf("incremental query %s: %w", key, err)
	}
	return fmt.Errorf("%s: %w", watchedObjectRef(source, namespace, name), err)
}

// watchedObjectRef names a watched object as "<source> <namespace>/<name>".
func watchedObjectRef(source, namespace, name string) string {
	if namespace == "" {
		return source + " " + name
	}
	return source + " " + namespace + "/" + name
}

// describeIncrementalGroupLocation renders an incrementalGroupLocationKey for
// messages: the call's index, its component, and the object it rendered.
func describeIncrementalGroupLocation(location string) string {
	const indexLength = 8
	if len(location) > indexLength {
		parts, ok := decodeIncrementalOrderedTuple([]byte(location[:len(location)-indexLength]))
		if ok && len(parts) == 4 {
			index := binary.BigEndian.Uint64([]byte(location[len(location)-indexLength:]))
			return fmt.Sprintf("%d of %s for %s", index, parts[0], watchedObjectRef(parts[1], parts[2], parts[3]))
		}
	}
	return strconv.Quote(location)
}

func queryWatchedObject(key incremental.QueryKey) (source, namespace, name string, ok bool) {
	if _, source, namespace, name, ok = parseComponentQueryKey(key); ok {
		return source, namespace, name, true
	}
	if source, namespace, name, ok = parseActivationQueryKey(key); ok {
		return source, namespace, name, true
	}
	return parseDerivedProjectionQueryKey(key)
}

type incrementalTemplateError struct {
	component string
	err       error
}

func (e *incrementalTemplateError) Error() string {
	switch err := e.err.(type) {
	case *templating.RenderError:
		return templating.NewRenderError(e.component, err.Cause).Error()
	case *templating.RenderTimeoutError:
		return (&templating.RenderTimeoutError{TemplateName: e.component, Cause: err.Cause}).Error()
	case *templating.TemplateNotFoundError:
		return templating.NewTemplateNotFoundError(e.component, err.AvailableTemplates).Error()
	default:
		return e.err.Error()
	}
}

func (e *incrementalTemplateError) Unwrap() error {
	return e.err
}

func remapIncrementalTemplateError(component, privateEntryPoint string, err error) error {
	if err == nil {
		return nil
	}
	switch typed := err.(type) {
	case *templating.RenderError:
		if typed.TemplateName == privateEntryPoint {
			return &incrementalTemplateError{component: component, err: err}
		}
	case *templating.RenderTimeoutError:
		if typed.TemplateName == privateEntryPoint {
			return &incrementalTemplateError{component: component, err: err}
		}
	case *templating.TemplateNotFoundError:
		if typed.TemplateName == privateEntryPoint {
			return &incrementalTemplateError{component: component, err: err}
		}
	}
	return err
}
