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

package controller

import (
	"log/slog"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/inputisolation"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/introspection"
)

func newInputSelector(setup *componentSetup, cfg *config.Config, validatingPipeline *pipeline.Pipeline, logger *slog.Logger) *inputisolation.Service {
	selector := inputisolation.New(validatingPipeline, cfg.WatchedResources, logger, inputisolation.Callbacks{
		Updated: func(rejections []inputisolation.Rejection) {
			changes := make([]events.InputRejection, len(rejections))
			inputs := make([]metrics.RejectedInput, len(rejections))
			for i := range rejections {
				r := &rejections[i]
				changes[i] = events.InputRejection{Object: r.Object, Deleted: r.Deleted, Reason: r.Reason}
				inputs[i] = metrics.RejectedInput{ResourceType: r.Store, Namespace: r.Namespace, Name: r.Name}
			}
			setup.MetricsComponent.Metrics().SetRejectedWatchedInputs(inputs)
			setup.Bus.Publish(events.NewWatchedInputsRejectedEvent(changes))
		},
	})
	setup.IntrospectionRegistry.Publish("inputRejections", introspection.Func(func() (any, error) {
		_, rejected, ready := selector.Snapshot()
		return map[string]any{"ready": ready, "resources": rejected}, nil
	}))
	return selector
}
