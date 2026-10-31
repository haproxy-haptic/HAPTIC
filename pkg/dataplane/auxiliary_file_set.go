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

package dataplane

import (
	"fmt"
	"path"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

// CanonicalizeAuxiliaryFiles returns an independent, deterministically sorted
// file set and rejects definitions that collapse to one written file. The agent
// writes a file under the path the plan names, so the base name is the whole
// identity: two definitions that share one are the same file.
func CanonicalizeAuxiliaryFiles(files *AuxiliaryFiles) (*AuxiliaryFiles, error) {
	if files == nil {
		return &AuxiliaryFiles{}, nil
	}

	result := &AuxiliaryFiles{}
	var err error
	result.MapFiles, err = canonicalizeFileItems(
		"Map file", files.MapFiles,
		func(file auxiliaryfiles.MapFile) string { return file.Path },
		func(a, b auxiliaryfiles.MapFile) bool { return a == b },
		func(file auxiliaryfiles.MapFile) auxiliaryfiles.MapFile { return file },
	)
	if err != nil {
		return nil, err
	}
	result.GeneralFiles, err = canonicalizeFileItems(
		"General file", files.GeneralFiles,
		func(file auxiliaryfiles.GeneralFile) string { return file.Filename },
		generalFilesEqual,
		cloneGeneralFile,
	)
	if err != nil {
		return nil, err
	}
	result.SSLCertificates, err = canonicalizeFileItems(
		"SSL certificate", files.SSLCertificates,
		func(file auxiliaryfiles.SSLCertificate) string { return path.Base(file.Path) },
		func(a, b auxiliaryfiles.SSLCertificate) bool { return a == b },
		func(file auxiliaryfiles.SSLCertificate) auxiliaryfiles.SSLCertificate { return file },
	)
	if err != nil {
		return nil, err
	}
	result.SSLCaFiles, err = canonicalizeFileItems(
		"SSL CA file", files.SSLCaFiles,
		func(file auxiliaryfiles.SSLCaFile) string { return path.Base(file.Path) },
		func(a, b auxiliaryfiles.SSLCaFile) bool { return a == b },
		func(file auxiliaryfiles.SSLCaFile) auxiliaryfiles.SSLCaFile { return file },
	)
	if err != nil {
		return nil, err
	}
	result.CRTListFiles, err = canonicalizeFileItems(
		"CRT-list file", files.CRTListFiles,
		func(file auxiliaryfiles.CRTListFile) string { return path.Base(file.Path) },
		func(a, b auxiliaryfiles.CRTListFile) bool { return a == b },
		func(file auxiliaryfiles.CRTListFile) auxiliaryfiles.CRTListFile { return file },
	)
	if err != nil {
		return nil, err
	}

	result.SecretFiles, err = canonicalizeFileItems(
		"Secret file", files.SecretFiles,
		func(file auxiliaryfiles.SecretFile) string { return path.Base(file.Path) },
		func(a, b auxiliaryfiles.SecretFile) bool { return a == b },
		func(file auxiliaryfiles.SecretFile) auxiliaryfiles.SecretFile { return file },
	)
	if err != nil {
		return nil, err
	}

	if err := rejectSharedGeneralStorageName(result.GeneralFiles, result.CRTListFiles); err != nil {
		return nil, err
	}
	result.Sort()
	return result, nil
}

func canonicalizeFileItems[T any](
	kind string,
	items []T,
	identity func(T) string,
	equal func(T, T) bool,
	clone func(T) T,
) ([]T, error) {
	canonical := make([]T, 0, len(items))
	indexes := make(map[string]int, len(items))
	for _, item := range items {
		id := identity(item)
		if index, exists := indexes[id]; exists {
			if !equal(canonical[index], item) {
				return nil, fmt.Errorf("%s %q has conflicting definitions; keep one definition", kind, id)
			}
			continue
		}
		indexes[id] = len(canonical)
		canonical = append(canonical, clone(item))
	}
	return canonical, nil
}

func generalFilesEqual(a, b auxiliaryfiles.GeneralFile) bool {
	return a.Filename == b.Filename &&
		a.Path == b.Path &&
		a.Content == b.Content &&
		a.IsCaFile == b.IsCaFile &&
		a.ReloadsOnPush() == b.ReloadsOnPush()
}

func cloneGeneralFile(file auxiliaryfiles.GeneralFile) auxiliaryfiles.GeneralFile {
	if file.ReloadOnPush != nil {
		reloadOnPush := *file.ReloadOnPush
		file.ReloadOnPush = &reloadOnPush
	}
	return file
}

func rejectSharedGeneralStorageName(
	generalFiles []auxiliaryfiles.GeneralFile,
	crtListFiles []auxiliaryfiles.CRTListFile,
) error {
	generalNames := make(map[string]struct{}, len(generalFiles))
	for _, file := range generalFiles {
		generalNames[file.Filename] = struct{}{}
	}
	for _, file := range crtListFiles {
		name := path.Base(file.Path)
		if _, exists := generalNames[name]; exists {
			return fmt.Errorf("general file and CRT-list %q use the same storage name; rename one file", name)
		}
	}
	return nil
}
