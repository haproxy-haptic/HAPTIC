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

package templating

import (
	"fmt"
	"net/url"
)

func scriggoParseURL(value string) (map[string]any, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	return map[string]any{
		"scheme":      parsed.Scheme,
		"host":        parsed.Host,
		"hostname":    parsed.Hostname(),
		"port":        parsed.Port(),
		"path":        parsed.Path,
		"escapedPath": parsed.EscapedPath(),
		"rawQuery":    parsed.RawQuery,
		"forceQuery":  parsed.ForceQuery,
		"fragment":    parsed.Fragment,
		"opaque":      parsed.Opaque,
		"hasUserInfo": parsed.User != nil,
	}, nil
}
