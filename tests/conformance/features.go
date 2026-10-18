//go:build gateway_conformance

// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package conformance

import (
	"errors"

	"sigs.k8s.io/gateway-api/pkg/features"
)

func retryCodesFeature(available []features.Feature) (features.FeatureName, error) {
	// Upstream renamed the status-code retry feature after v1.6; see issue #291.
	for _, name := range []features.FeatureName{"HTTPRouteRetryCodes", "HTTPRouteRetry"} {
		for _, feature := range available {
			if feature.Name == name {
				return name, nil
			}
		}
	}
	return "", errors.New("no recognized retry-codes feature in Gateway API; update the conformance feature mapping")
}
