// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package renderartifact holds authenticated immutable auxiliary render output.
package renderartifact

import (
	"errors"
	"path"
	"strings"
)

var (
	errInvalidAuthority  = errors.New("render artifact authority is invalid")
	errInvalidFamily     = errors.New("render artifact family is invalid")
	errInvalidContent    = errors.New("render artifact content is invalid")
	errInvalidArtifact   = errors.New("render artifact is invalid")
	errInvalidSnapshot   = errors.New("render artifact snapshot is invalid")
	errNilVisitor        = errors.New("render artifact visitor is nil")
	errNilContent        = errors.New("render artifact content is nil")
	errBuilderSealed     = errors.New("render artifact builder is sealed")
	errForeignSnapshot   = errors.New("render artifact snapshot has a foreign authority")
	errArtifactNotFound  = errors.New("render artifact is not in the snapshot")
	errContentMismatch   = errors.New("render artifact content differs")
	errInvalidWriteCount = errors.New("render artifact writer returned an invalid byte count")
)

// Family identifies one legacy AuxiliaryFiles family without collapsing files
// that happen to share deployment behaviour.
type Family uint8

const (
	Map Family = iota + 1
	General
	Certificate
	CA
	CRTList
	GeneralCA
	Secret
)

// Descriptor is the deployment-relevant identity and metadata of one artifact.
type Descriptor struct {
	Family Family
	// ReloadOnChange is effective General metadata; every other family normalizes false.
	ReloadOnChange bool
	// Name is Filename for general families and the canonical path identity otherwise.
	Name string
	// Path is the compatibility output path and remains part of equality.
	Path string
	// RuntimePath is the path declared in the render plan. Empty defaults to Path.
	RuntimePath string
	// Seal is the Secret family's keyed content digest; every other family normalizes empty.
	Seal string `json:",omitempty"`
}

type artifactKey struct {
	family Family
	name   string
}

type sharedStorageKey struct {
	name string
}

type descriptorData struct {
	value Descriptor
	key   artifactKey
	seal  *descriptorData
	auth  descriptorAuthentication
}

type descriptorAuthentication struct {
	value Descriptor
	key   artifactKey
}

func normalizeDescriptor(descriptor Descriptor) (*descriptorData, error) {
	descriptor, key, err := canonicalizeDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	descriptor.Path = strings.Clone(descriptor.Path)
	descriptor.RuntimePath = strings.Clone(descriptor.RuntimePath)
	descriptor.Seal = strings.Clone(descriptor.Seal)
	switch descriptor.Family {
	case Map:
		descriptor.Name = descriptor.Path
	case Certificate, CA, CRTList, Secret:
		descriptor.Name = path.Base(descriptor.Path)
	default:
		descriptor.Name = strings.Clone(descriptor.Name)
	}
	key.name = descriptor.Name
	data := &descriptorData{value: descriptor, key: key}
	data.seal = data
	data.auth = descriptorAuthentication{value: data.value, key: data.key}
	return data, nil
}

func canonicalizeDescriptor(descriptor Descriptor) (Descriptor, artifactKey, error) {
	if !descriptor.Family.valid() {
		return Descriptor{}, artifactKey{}, errInvalidFamily
	}
	if descriptor.Family != General {
		descriptor.ReloadOnChange = false
	}
	if descriptor.Family != Secret {
		descriptor.Seal = ""
	} else if descriptor.Seal == "" {
		return Descriptor{}, artifactKey{}, errInvalidArtifact
	}
	if descriptor.RuntimePath == "" {
		descriptor.RuntimePath = descriptor.Path
	}
	switch descriptor.Family {
	case Map:
		descriptor.Name = descriptor.Path
	case Certificate, CA, CRTList, Secret:
		descriptor.Name = path.Base(descriptor.Path)
	}
	return descriptor, artifactKey{
		family: descriptor.Family,
		name:   descriptor.Name,
	}, nil
}

func (f Family) valid() bool {
	return f >= Map && f <= Secret
}

func (d *descriptorData) validate() error {
	if d == nil || d.seal != d || !d.value.Family.valid() ||
		d.auth.value != d.value || d.auth.key != d.key ||
		d.key.family != d.value.Family || d.key.name != d.value.Name {
		return errInvalidArtifact
	}
	canonical, key, err := canonicalizeDescriptor(d.value)
	if err != nil || canonical != d.value || key != d.key {
		return errInvalidArtifact
	}
	return nil
}

func (d *descriptorData) detached() Descriptor {
	return d.value
}

func descriptorSharedStorage(descriptor Descriptor) (sharedStorageKey, bool) {
	switch descriptor.Family {
	case General, GeneralCA, CRTList:
		return sharedStorageKey{name: descriptor.Name}, true
	default:
		return sharedStorageKey{}, false
	}
}

func descriptorsEqual(left, right *descriptorData) bool {
	return left != nil && right != nil && left.value == right.value
}
