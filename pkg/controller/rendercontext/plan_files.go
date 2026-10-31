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

package rendercontext

import (
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// planFiles lists every file of the render with its digest, in the kinds the
// deploy side reasons about, plus the rendered content of every map keyed by
// its plan path. ReloadOnChange is true only where a content change cannot be
// applied over the runtime API.
func (r *PlanRegistry) planFiles(config string, aux *dataplane.AuxiliaryFiles) (files []renderplan.File, mapContents map[string]string, err error) {
	files = []renderplan.File{{
		Path:           names.MainTemplateName,
		Kind:           renderplan.FileKindConfig,
		ReloadOnChange: true,
		Digest:         renderplan.DigestString(config),
		Size:           int64(len(config)),
		Content:        config,
		ContentKnown:   true,
	}}
	if aux == nil {
		return files, nil, nil
	}

	mapContents = make(map[string]string, len(aux.MapFiles))
	for _, file := range aux.MapFiles {
		mapPath, err := r.MapPath(file.Path)
		if err != nil {
			return nil, nil, err
		}
		mapContents[mapPath] = file.Content
		files = append(files, planFile(mapPath, renderplan.FileKindMap, false, file.Content))
	}
	for _, file := range aux.SSLCertificates {
		certPath, err := r.filePath(file.Path, "cert")
		if err != nil {
			return nil, nil, err
		}
		files = append(files, planFile(certPath, renderplan.FileKindCert, false, file.Content))
	}
	for _, file := range aux.SSLCaFiles {
		files = append(files, planFile(file.Path, renderplan.FileKindCA, false, file.Content))
	}
	for _, file := range aux.CRTListFiles {
		listPath, err := r.filePath(file.Path, "crt-list")
		if err != nil {
			return nil, nil, err
		}
		files = append(files, planFile(listPath, renderplan.FileKindCRTList, false, file.Content))
	}
	for _, file := range aux.GeneralFiles {
		// A ca-file is delivered as a general file but rotates over the runtime
		// API, so it is a CA file to the deploy side (file_registry.GetFiles).
		if file.IsCaFile {
			files = append(files, planFile(file.Path, renderplan.FileKindCA, false, file.Content))
			continue
		}
		reload := file.ReloadOnPush == nil || *file.ReloadOnPush
		files = append(files, planFile(file.Path, renderplan.FileKindGeneral, reload, file.Content))
	}
	for _, file := range aux.SecretFiles {
		sealed := planFile(file.Path, renderplan.FileKindGeneral, false, file.Content)
		sealed.Seal = file.Seal
		files = append(files, sealed)
	}
	return files, mapContents, nil
}

func planFile(path, kind string, reloadOnChange bool, content string) renderplan.File {
	return renderplan.File{
		Path:           path,
		Kind:           kind,
		ReloadOnChange: reloadOnChange,
		Digest:         renderplan.DigestString(content),
		Size:           int64(len(content)),
		Content:        content,
		ContentKnown:   true,
	}
}
