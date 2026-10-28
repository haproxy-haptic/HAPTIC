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

package configchange

import (
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

// Regression for repeated observations superseding a passing verdict (#522).
func TestConfigChangeHandler_DuplicateParsedKeepsSuccessfulValidation(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"gated"}, time.Second)
	validatorEvents := bus.Subscribe("duplicate-validator", 100)
	observed := bus.Subscribe("duplicate-observer", 100)
	bus.Start()
	go handler.Start(t.Context())

	bus.Publish(events.NewConfigValidatedEvent(&coreconfig.Config{}, nil, "initial", ""))
	testutil.WaitForEvent[*events.ConfigValidatedEvent](t, observed, testutil.LongTimeout)
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "candidate", ""))
	request := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorEvents, testutil.LongTimeout)

	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "candidate", ""))
	// Leadership replay is an event-loop barrier: the duplicate has been
	// processed before the first validation's response is delivered.
	bus.Publish(events.NewBecameLeaderEvent("duplicate-barrier"))
	testutil.WaitForEvent[*events.ConfigValidatedEvent](t, observed, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(request.RequestID(), "gated", true, nil))

	deadline := time.NewTimer(testutil.LongTimeout)
	defer deadline.Stop()
	for {
		select {
		case event := <-observed:
			if verdict, ok := event.(*events.ConfigValidatedEvent); ok && verdict.Version == "candidate" {
				return
			}
		case event := <-validatorEvents:
			if _, ok := event.(*events.ConfigValidationRequest); ok {
				t.Fatal("duplicate observation discarded the passing result and started a second validation")
			}
		case <-deadline.C:
			t.Fatal("passing candidate never received a validated verdict")
		}
	}
}

func TestConfigChangeHandler_DuplicateIdentityDoesNotHideChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*events.ConfigParsedEvent)
	}{
		{"credentials", func(e *events.ConfigParsedEvent) { e.SecretVersion = "rotated" }},
		{"payload", func(e *events.ConfigParsedEvent) {
			e.Config = &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "changed"}}
		}},
		{"source identity", func(e *events.ConfigParsedEvent) { e.Sources[0].UID = "recreated" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus, logger := testutil.NewTestBusAndLogger()
			handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"gated"}, time.Second)
			requests := bus.SubscribeTypes("changed-validator", 100, events.EventTypeConfigValidationRequest)
			observed := bus.Subscribe("changed-observer", 100)
			bus.Start()
			go handler.Start(t.Context())
			bus.Publish(events.NewConfigValidatedEvent(&coreconfig.Config{}, nil, "initial", ""))
			testutil.WaitForEvent[*events.ConfigValidatedEvent](t, observed, testutil.LongTimeout)
			first := events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "candidate", "original")
			first.Sources = []events.ConfigSourceRef{{Name: "config", UID: "original"}}
			bus.Publish(first)
			request := testutil.WaitForEvent[*events.ConfigValidationRequest](t, requests, testutil.LongTimeout)
			changed := events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "candidate", "original")
			changed.Sources = []events.ConfigSourceRef{{Name: "config", UID: "original"}}
			test.change(changed)
			bus.Publish(changed)
			bus.Publish(events.NewBecameLeaderEvent("changed-barrier"))
			testutil.WaitForEvent[*events.ConfigValidatedEvent](t, observed, testutil.LongTimeout)
			bus.Publish(events.NewConfigValidationResponse(request.RequestID(), "gated", true, nil))
			request = testutil.WaitForEvent[*events.ConfigValidationRequest](t, requests, testutil.LongTimeout)
			bus.Publish(events.NewConfigValidationResponse(request.RequestID(), "gated", true, nil))
			for {
				verdict := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, observed, testutil.LongTimeout)
				if verdict.Version == "candidate" {
					return
				}
			}
		})
	}
}
