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

package api

// Limits, asserted at both ends. Every loop the agent runs is bounded by one
// of these or by a counted collection.
const (
	MaxApplyBodyBytes        = 64 << 20 // one apply request
	MaxFiles                 = 4096
	MaxPlanBlobBytes         = 8 << 20
	MaxPathBytes             = 255
	MaxOpsPerApply           = 1000 // operations per execution batch, including in-place ops in the first
	MaxOpBatches             = 8
	MaxCommandLineBytes      = 12 << 10 // ';'-joined CLI line; tune.bufsize is 16 KiB all-or-nothing
	MaxPayloadBytes          = 12 << 10 // one payload command; HAProxy 3.0 caps payloads at tune.bufsize
	MaxWaitBudgetMs          = 30000    // total wait …-removable per apply
	MaxReloadMs              = 60000
	MaxReloadIntervalMs      = 60000 // --reload-interval-min, refused above it
	MaxPendingServerDeletes  = 1000  // per pod, beyond → forced reload
	MaxPendingBackendDeletes = 100
	ConnectRetries           = 3
	ConnectRetryBackoffMs    = 100
	MaxSections              = 65536
	MaxMapDelRepeat          = 64 // 3.4 deletes one duplicate per `del map` call
	MaxDeferredAttempts      = 5  // per queued delete before the agent gives up on it
	MaxInventoryEntries      = 4096
	MaxMapEntries            = 1 << 18 // one map's entries; the design target is one per route
	MaxRequiredBlobs         = 256
)

// A per-command round trip is bounded by client-native's own 30 s task
// timeout (runtime.taskTimeout), which is not configurable; the agent adds no
// second deadline, because a deadline it cannot enforce on the socket read
// would report a timeout for a command that still lands.
