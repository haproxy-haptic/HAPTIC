// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const maxManifestBytes = 32 << 20

var manifestFetchWait = testutil.WaitConfig{Timeout: 5 * time.Minute, InitialInterval: 2 * time.Second, MaxInterval: 30 * time.Second, Multiplier: 2}

// fetchManifest retries network errors, 429 and 5xx: GitHub release downloads
// return transient 504s that otherwise fail whole CI jobs.
func fetchManifest(ctx context.Context, url string) ([]byte, error) {
	var content []byte
	err := testutil.Poll(ctx, manifestFetchWait, "download "+url, func(ctx context.Context) (testutil.PollResult, error) {
		attempt, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		request, err := http.NewRequestWithContext(attempt, http.MethodGet, url, http.NoBody)
		if err != nil {
			return testutil.PollFailed, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return testutil.PollPending, err
		}
		defer response.Body.Close()
		switch {
		case response.StatusCode == http.StatusOK:
		case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError:
			return testutil.PollPending, fmt.Errorf("%s returned HTTP %d", url, response.StatusCode)
		default:
			return testutil.PollFailed, fmt.Errorf("%s returned HTTP %d", url, response.StatusCode)
		}
		content, err = io.ReadAll(io.LimitReader(response.Body, maxManifestBytes))
		if err != nil {
			return testutil.PollPending, err
		}
		return testutil.PollSucceeded, nil
	})
	return content, err
}
