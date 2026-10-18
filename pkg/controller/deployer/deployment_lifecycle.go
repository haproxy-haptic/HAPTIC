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

package deployer

import (
	"context"
	"errors"
	"fmt"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
)

// errDeploymentSuperseded cancels a deploy whose pod set was replaced; the
// replacement deploy follows, so its pods' cancelled applies are not failures.
var errDeploymentSuperseded = fmt.Errorf("deployment superseded: %w", context.Canceled)

func cancelCause(reason string) error {
	if reason == events.DeploymentCancelReasonEndpointAuthorityChanged {
		return errDeploymentSuperseded
	}
	return nil
}

// superseded reports whether err is the deliberate cancellation of a superseded deploy.
func superseded(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && errors.Is(context.Cause(ctx), errDeploymentSuperseded)
}

func (c *Component) flushPendingCancellationRequests() {
	for {
		select {
		case <-c.cancelEventChan:
		default:
			return
		}
	}
}

func (c *Component) runCancellationLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-c.cancelEventChan:
			request, ok := event.(*events.DeploymentCancelRequestEvent)
			if ok {
				c.handleDeploymentCancelRequest(request)
			}
		}
	}
}

// beginDeployment registers the deployment as active, or reports false once
// Drain has ended this term's deployments.
func (c *Component) beginDeployment(ctx context.Context, deploymentID, correlationID string) (context.Context, context.CancelFunc, bool) {
	c.cancelMu.Lock()
	if c.draining {
		c.cancelMu.Unlock()
		return nil, nil, false
	}
	deployCtx, cancel := context.WithCancelCause(ctx)
	c.activeDeploymentID = deploymentID
	c.activeCorrelationID = correlationID
	c.activeCancelFunc = cancel
	c.deploymentDone = make(chan struct{})
	if c.pendingCancellation == deploymentID {
		cancel(c.pendingCancelCause)
	}
	c.pendingCancellation = ""
	c.pendingCancelCause = nil
	c.cancelMu.Unlock()
	return deployCtx, func() { cancel(nil) }, true
}

// Drain ends this term's deployments and returns once the one in flight has
// published its receipt, provided the term still holds the Lease.
func (c *Component) Drain(ctx context.Context) error {
	c.cancelMu.Lock()
	// Checked under the lock Start resets draining with, so an ended term's
	// drain cannot stop the next term's deployments.
	if err := ctx.Err(); err != nil {
		c.cancelMu.Unlock()
		return err
	}
	c.draining = true
	done := c.deploymentDone
	c.cancelMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.checkRetainedAuthority(ctx)
}

// handleDeploymentCancelRequest cancels the exact in-progress deployment.
func (c *Component) handleDeploymentCancelRequest(event *events.DeploymentCancelRequestEvent) {
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()

	if c.activeDeploymentID == "" || c.activeCancelFunc == nil {
		c.pendingCancellation = event.DeploymentID
		c.pendingCancelCause = cancelCause(event.Reason)
		c.Logger().Debug("Queued cancel request for a deployment that has not started",
			"requested_deployment_id", event.DeploymentID,
			"reason", event.Reason)
		return
	}

	if c.activeDeploymentID != event.DeploymentID {
		c.pendingCancellation = event.DeploymentID
		c.pendingCancelCause = cancelCause(event.Reason)
		c.Logger().Debug("Queued cancel request for the next deployment",
			"requested_deployment_id", event.DeploymentID,
			"active_deployment_id", c.activeDeploymentID,
			"reason", event.Reason)
		return
	}

	c.Logger().Info("Cancelling in-progress deployment",
		"deployment_id", event.DeploymentID,
		"correlation_id", c.activeCorrelationID,
		"reason", event.Reason)

	c.activeCancelFunc(cancelCause(event.Reason))
}

// cancelActiveDeployment cancels any active deployment regardless of its ID.
// Used for graceful shutdown.
func (c *Component) cancelActiveDeployment(reason string) {
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()

	if c.activeCancelFunc == nil {
		return
	}

	c.Logger().Info("Cancelling active deployment",
		"deployment_id", c.activeDeploymentID,
		"correlation_id", c.activeCorrelationID,
		"reason", reason)

	c.activeCancelFunc(nil)

	// Wait for deployment to complete if deploymentDone channel exists
	done := c.deploymentDone
	if done != nil {
		c.cancelMu.Unlock()
		<-done
		c.cancelMu.Lock()
	}
}
