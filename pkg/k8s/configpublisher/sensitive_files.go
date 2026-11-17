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

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SensitiveFileLabelSelector selects the sensitive-file Secrets published for
// a runtime config.
func SensitiveFileLabelSelector(runtimeConfigName string) *metav1.LabelSelector {
	selector := RuntimeConfigLabelSelector(runtimeConfigName)
	selector.MatchLabels["haproxy-haptic.org/type"] = sensitiveFileType
	return selector
}

// IsSensitiveFileSecret reports whether labels mark a sensitive-file Secret.
func IsSensitiveFileSecret(labels map[string]string) bool {
	return labels["haproxy-haptic.org/type"] == sensitiveFileType
}

// SensitiveFileContent returns the file a sensitive-file Secret carries,
// checked against the content checksum the Secret records.
func SensitiveFileContent(secret *corev1.Secret) (string, error) {
	compressed, declared := compressedAnnotation(secret)
	if !IsSensitiveFileSecret(secret.Labels) || !declared {
		return "", errors.New("not a sensitive-file Secret")
	}
	content, err := decodeRetainedContent(string(secret.Data[secretFileDataKey]), compressed)
	if err != nil {
		return "", err
	}
	if calculateChecksum(content) != string(secret.Data[secretFileChecksumDataKey]) {
		return "", errors.New("sensitive-file Secret content differs from its checksum")
	}
	return content, nil
}
