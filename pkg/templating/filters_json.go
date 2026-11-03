// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package templating

import (
	"encoding/json"
	"fmt"
)

func scriggoParseJSON(data string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("parse_json: %w", err)
	}
	return value, nil
}
