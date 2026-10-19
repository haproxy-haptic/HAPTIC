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

package endpointrouting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
)

const maxHealthzBody = 64 << 10

// Healthz is one kube-proxy health answer, in the node's clock.
type Healthz struct {
	CurrentTime time.Time
	// Synced is when the last proxy-rule sync per IP family completed.
	Synced map[discoveryv1.AddressType]time.Time
}

// HealthzReader reads kube-proxy's health on one node address.
type HealthzReader func(ctx context.Context, address string) (Healthz, error)

// kube-proxy's top-level lastUpdated is the current time whenever it is
// healthy (kubernetes#83498), so only the per-family status (v1.33+) carries
// the sync time.
type healthzBody struct {
	CurrentTime time.Time `json:"currentTime"`
	Status      map[string]struct {
		LastUpdated time.Time `json:"lastUpdated"`
	} `json:"status"`
}

// ErrNoKubeProxy marks an answer that shows no kube-proxy healthz listens at
// the address: the connection failed, or something else answered. A
// kube-proxy that accepts the connection and then does not answer in time is
// still syncing, so that error does not carry it.
var ErrNoKubeProxy = errors.New("no kube-proxy healthz")

// HTTPHealthz reads GET /healthz on the given port. kube-proxy answers 503 with
// the same body while unhealthy or while its node is being removed; the sync
// times stay valid, so both codes are read.
func HTTPHealthz(port int) HealthzReader {
	client := &http.Client{Transport: &http.Transport{DialContext: dialMarkingAbsence}}
	return func(ctx context.Context, address string) (Healthz, error) {
		url := "http://" + net.JoinHostPort(address, strconv.Itoa(port)) + "/healthz"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return Healthz{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return Healthz{}, err
		}
		defer resp.Body.Close()
		health, err := decodeHealthz(resp)
		if err != nil && ctx.Err() == nil {
			err = fmt.Errorf("%w: %w", ErrNoKubeProxy, err)
		}
		if err != nil {
			return Healthz{}, fmt.Errorf("%s: %w", url, err)
		}
		return health, nil
	}
}

// dialMarkingAbsence marks a failed connection as ErrNoKubeProxy.
func dialMarkingAbsence(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoKubeProxy, err)
	}
	return conn, nil
}

func decodeHealthz(resp *http.Response) (Healthz, error) {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return Healthz{}, fmt.Errorf("answered %s", resp.Status)
	}
	var body healthzBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHealthzBody)).Decode(&body); err != nil {
		return Healthz{}, err
	}
	if body.CurrentTime.IsZero() {
		return Healthz{}, errors.New("no currentTime")
	}
	health := Healthz{CurrentTime: body.CurrentTime, Synced: map[discoveryv1.AddressType]time.Time{}}
	for family, status := range body.Status {
		if !status.LastUpdated.IsZero() {
			health.Synced[discoveryv1.AddressType(family)] = status.LastUpdated
		}
	}
	return health, nil
}
