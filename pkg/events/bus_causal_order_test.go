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

package events

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// causeTestEvent runs onClone each time the bus copies it for a subscriber.
type causeTestEvent struct {
	onClone func()
}

func (e *causeTestEvent) EventType() string    { return "test.cause" }
func (e *causeTestEvent) Timestamp() time.Time { return time.Time{} }

func (e *causeTestEvent) CloneForSubscriber() Event {
	e.onClone()
	return &causeTestEvent{onClone: e.onClone}
}

// A subscriber that reacts to an event must not have its reaction overtake the
// event at a later subscriber: the deployment scheduler dropped a render gate
// verdict that arrived before the render it named, and never deployed.
func TestPublishDeliversAReactionAfterItsCauseToEverySubscriber(t *testing.T) {
	bus := NewEventBus(1)
	reactor := bus.Subscribe("reactor", 2)
	observer := bus.SubscribeTypes("observer", 2, "test.cause", "test.event")
	bus.Start()

	reacted := false
	react := func() {
		require.IsType(t, &causeTestEvent{}, <-reactor)
		reacted = true
		bus.Publish(testEvent{message: "effect"})
	}
	clones := 0
	cause := &causeTestEvent{onClone: func() {
		clones++
		if clones != 2 {
			return
		}
		// The observer's copy: react now if the reactor already holds the cause.
		if len(reactor) > 0 {
			react()
		}
	}}

	bus.Publish(cause)
	if !reacted {
		react()
	}

	assert.IsType(t, &causeTestEvent{}, <-observer, "the cause must reach the observer first")
	assert.Equal(t, testEvent{message: "effect"}, <-observer)
}
