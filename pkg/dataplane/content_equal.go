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

package dataplane

import (
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

// ContentEqual compares every rendered byte and deployment-relevant file property.
func ContentEqual(leftConfig string, leftAux *AuxiliaryFiles, rightConfig string, rightAux *AuxiliaryFiles) bool {
	if leftConfig != rightConfig {
		return false
	}
	left := normalizedAuxiliaryFiles(leftAux)
	right := normalizedAuxiliaryFiles(rightAux)
	return slices.EqualFunc(left.GeneralFiles, right.GeneralFiles, generalFileEqual) &&
		slices.Equal(left.MapFiles, right.MapFiles) &&
		slices.Equal(left.SSLCertificates, right.SSLCertificates) &&
		slices.Equal(left.SSLCaFiles, right.SSLCaFiles) &&
		slices.Equal(left.CRTListFiles, right.CRTListFiles) &&
		slices.Equal(left.SecretFiles, right.SecretFiles)
}

// CloneAuxiliaryFiles returns an independently owned copy.
func CloneAuxiliaryFiles(files *AuxiliaryFiles) *AuxiliaryFiles {
	if files == nil {
		return nil
	}
	clone := *files
	clone.GeneralFiles = slices.Clone(files.GeneralFiles)
	for index := range clone.GeneralFiles {
		if files.GeneralFiles[index].ReloadOnPush != nil {
			reload := *files.GeneralFiles[index].ReloadOnPush
			clone.GeneralFiles[index].ReloadOnPush = &reload
		}
	}
	clone.MapFiles = slices.Clone(files.MapFiles)
	clone.SSLCertificates = slices.Clone(files.SSLCertificates)
	clone.SSLCaFiles = slices.Clone(files.SSLCaFiles)
	clone.CRTListFiles = slices.Clone(files.CRTListFiles)
	clone.SecretFiles = slices.Clone(files.SecretFiles)
	return &clone
}

func normalizedAuxiliaryFiles(files *AuxiliaryFiles) AuxiliaryFiles {
	if files == nil {
		return AuxiliaryFiles{}
	}
	return *files
}

func generalFileEqual(left, right auxiliaryfiles.GeneralFile) bool {
	return left.Filename == right.Filename && left.Path == right.Path && left.Content == right.Content &&
		left.IsCaFile == right.IsCaFile && left.ReloadsOnPush() == right.ReloadsOnPush()
}
