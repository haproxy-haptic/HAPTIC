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

package configpublisher

import "github.com/haproxy-haptic/HAPTIC/pkg/dataplane"

// Replacing a matching output in place preserves publication order.
func (c *Component) enqueueDeployed(work *publishWorkItem) {
	c.deployedPendingMu.Lock()
	index := c.deployedOutputIndexLocked(work)
	if index >= 0 {
		c.deployedPending[index] = work
	} else {
		c.deployedPending = append(c.deployedPending, work)
	}
	pruned := c.pruneSupersededDeployedLocked()
	depth := len(c.deployedPending)
	c.deployedPendingMu.Unlock()

	c.logger.Debug("Queued deployed config for publishing",
		"checksum", work.entry.contentChecksum,
		"correlation_id", work.correlationID,
		"replaced_same_output", index >= 0,
		"pruned_superseded", pruned,
		"queue_depth", depth)

	select {
	case c.deployedTrigger <- struct{}{}:
	default: // already signalled; the worker drains the whole queue
	}
}

func (c *Component) recordPodChecksum(namespace, name, checksum string) {
	if checksum == "" {
		return
	}
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	if c.deployedChecksumByPod == nil {
		c.deployedChecksumByPod = make(map[podAuthorityKey]string)
	}
	c.deployedChecksumByPod[podAuthorityKey{namespace: namespace, name: name}] = checksum
}

func (c *Component) forgetPodChecksum(namespace, name string) {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	delete(c.deployedChecksumByPod, podAuthorityKey{namespace: namespace, name: name})
}

// Keep every checksum still reported by a pod and every entry newer than the
// latest reported one because pod status can lag deployment. Caller holds deployedPendingMu.
func (c *Component) pruneSupersededDeployedLocked() int {
	if len(c.deployedPending) < 2 {
		return 0
	}

	live := make(map[string]struct{}, len(c.deployedChecksumByPod))
	for _, checksum := range c.deployedChecksumByPod {
		live[checksum] = struct{}{}
	}

	newestLive := -1
	for i := len(c.deployedPending) - 1; i >= 0; i-- {
		if _, ok := live[c.deployedPending[i].entry.contentChecksum]; ok {
			newestLive = i
			break
		}
	}
	if newestLive <= 0 {
		return 0 // nothing reported yet, or the oldest entry is the live one
	}

	pruned := 0
	kept := c.deployedPending[:0]
	for i, work := range c.deployedPending {
		if i < newestLive {
			if _, ok := live[work.entry.contentChecksum]; !ok {
				pruned++
				continue
			}
		}
		kept = append(kept, work)
	}
	clear(c.deployedPending[len(kept):]) // release the dropped entries' configs
	c.deployedPending = kept
	return pruned
}

// Retry at the front unless the output already has a queue position.
func (c *Component) requeueDeployedFront(work *publishWorkItem) {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	if index := c.deployedOutputIndexLocked(work); index >= 0 {
		c.deployedPending[index] = work
		return
	}
	c.deployedPending = append([]*publishWorkItem{work}, c.deployedPending...)
}

func (c *Component) deployedOutputIndexLocked(work *publishWorkItem) int {
	for index, pending := range c.deployedPending {
		if sameDeployedOutput(pending.entry, work.entry) {
			return index
		}
	}
	return -1
}

func sameDeployedOutput(left, right *renderedConfigEntry) bool {
	if left == nil || right == nil {
		return false
	}
	if left.outputSnapshot != nil || right.outputSnapshot != nil {
		if left.outputSnapshot == nil || right.outputSnapshot == nil {
			return false
		}
		same, err := left.outputSnapshot.SameRoot(right.outputSnapshot)
		return err == nil && same
	}
	if left.artifactSnapshot != nil || right.artifactSnapshot != nil {
		if left.artifactSnapshot == nil || right.artifactSnapshot == nil {
			return false
		}
		same, err := dataplane.SnapshotContentEqual(
			left.config, left.artifactSnapshot,
			right.config, right.artifactSnapshot,
		)
		return err == nil && same
	}
	return dataplane.ContentEqual(left.config, left.auxFiles, right.config, right.auxFiles)
}

func (c *Component) takeDeployed() *publishWorkItem {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	if len(c.deployedPending) == 0 {
		return nil
	}
	work := c.deployedPending[0]
	c.deployedPending = c.deployedPending[1:]
	return work
}

func (c *Component) newestAcknowledged() *publishWorkItem {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	for i := len(c.deployedPending) - 1; i >= 0; i-- {
		if work := c.deployedPending[i]; work.entry.confirmedPod != nil {
			return work
		}
	}
	return nil
}

// Publishing an older render after this one would move spec backwards.
func (c *Component) dropDeployedThrough(work *publishWorkItem) {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	if index := c.deployedOutputIndexLocked(work); index >= 0 {
		clear(c.deployedPending[:index+1])
		c.deployedPending = c.deployedPending[index+1:]
	}
}

func (c *Component) deployedQueueDepth() int {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	return len(c.deployedPending)
}
