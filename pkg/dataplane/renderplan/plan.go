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

// Package renderplan holds the structure a render declares about its own
// output: the ordered sections of haproxy.cfg, the backend records the
// generator emitted them from, the map entries, and the file set.
//
// The structure is produced by the generator (chart macros through
// pkg/controller/rendercontext.PlanRegistry), never by parsing HAProxy
// configuration. Pure data: no controller, templating or client-native imports.
package renderplan

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/cespare/xxhash/v2"
)

// SchemaVersion identifies the plan format. A consumer reading a foreign
// version must treat the plan as unknown rather than decode it partially.
const SchemaVersion = 1

// Section kinds.
const (
	SectionKindCore    = "core"
	SectionKindProfile = "profile"
	SectionKindBackend = "backend"
)

// Backend shapes: a dynamic backend can be created and populated over the
// runtime API, a structural one needs a reload.
const (
	ShapeDynamic    = "dynamic"
	ShapeStructural = "structural"
)

// File kinds.
const (
	FileKindConfig  = "config"
	FileKindMap     = "map"
	FileKindCert    = "cert"
	FileKindCA      = "ca"
	FileKindCRTList = "crtlist"
	FileKindGeneral = "general"
)

// ConfigFilePath is the single main configuration path in every render plan.
const ConfigFilePath = "haproxy.cfg"

// Plan is the immutable structure of one render.
type Plan struct {
	SchemaVersion int                `json:"schemaVersion"`
	ID            string             `json:"id"`
	Sections      []Section          `json:"sections"`
	Backends      map[string]Backend `json:"backends"`
	Profiles      map[string]Profile `json:"profiles"`
	Maps          map[string]Map     `json:"maps"`
	CRTLists      map[string]CRTList `json:"crtLists,omitempty"`
	Files         []File             `json:"files"`
}

// Section is one contiguous run of haproxy.cfg. The sections of a plan
// partition the file: concatenating their text reproduces it byte for byte.
type Section struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	TextDigest string `json:"textDigest"`
	Length     int    `json:"length"`

	Text      string `json:"-"`
	TextKnown bool   `json:"-"`
}

// Backend is the record a generator macro declared alongside the backend
// section text it emitted.
type Backend struct {
	Name          string       `json:"name"`
	Profile       string       `json:"profile,omitempty"`
	Mode          string       `json:"mode,omitempty"`
	GUID          string       `json:"guid,omitempty"`
	Balance       string       `json:"balance,omitempty"`
	HashType      string       `json:"hashType,omitempty"`
	Shape         string       `json:"shape"`
	ShapeReason   string       `json:"shapeReason,omitempty"`
	Servers       []Server     `json:"servers,omitempty"`
	DefaultServer []KeywordArg `json:"defaultServer,omitempty"`

	BodyDigest     string `json:"bodyDigest"`
	CommentsDigest string `json:"commentsDigest"`
	RecordDigest   string `json:"recordDigest"`
	TextDigest     string `json:"textDigest"`

	Body         []string `json:"-"`
	Comments     []string `json:"-"`
	ContentKnown bool     `json:"-"`
}

// Server is one server line, declared as data.
type Server struct {
	Name     string       `json:"name"`
	Address  string       `json:"address"`
	Port     int          `json:"port"`
	Weight   *int         `json:"weight,omitempty"` // nil: no weight keyword; 0 is a real weight
	Disabled bool         `json:"disabled,omitempty"`
	GUID     string       `json:"guid,omitempty"`
	Comment  string       `json:"comment,omitempty"`
	Extra    []KeywordArg `json:"extra,omitempty"`
}

// KeywordArg is a server or default-server keyword with its arguments,
// structured so no consumer has to scan the emitted line.
type KeywordArg struct {
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
}

// Profile is a named defaults section shared by backends.
type Profile struct {
	Name       string `json:"name"`
	BodyDigest string `json:"bodyDigest"`
	HasRules   bool   `json:"hasRules,omitempty"`
}

// Map is the entry list of one map file, derived from its rendered content.
type Map struct {
	Path    string  `json:"path"`
	Ordered bool    `json:"ordered"`
	Entries []Entry `json:"entries,omitempty"`
}

// Entry is one map-file line: the key and the rest of the line.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// CRTList is the entry list of one crt-list file, declared by the generator
// that emitted the file.
type CRTList struct {
	Path    string         `json:"path"`
	Entries []CRTListEntry `json:"entries,omitempty"`
}

// CRTListEntry is one crt-list line: the certificate, its ssl options and the
// SNI filters that select it.
type CRTListEntry struct {
	Cert       string       `json:"cert"`
	Options    []KeywordArg `json:"options,omitempty"`
	SNIFilters []string     `json:"sniFilters,omitempty"`
}

// File is one file the render produced.
type File struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	ReloadOnChange bool   `json:"reloadOnChange"`
	Digest         string `json:"digest"`
	Size           int64  `json:"size"`
	// Seal replaces Digest in the identity and in checkpoints, which readers
	// without Secret access can see: see auxiliaryfiles.SealContent.
	Seal string `json:"seal,omitempty"`

	Content      string `json:"-"`
	ContentKnown bool   `json:"-"`
}

// Clone returns an independently owned copy of the plan.
func (p *Plan) Clone() *Plan {
	if p == nil {
		return nil
	}
	cloned := *p
	cloned.Sections = slices.Clone(p.Sections)
	if p.Backends != nil {
		cloned.Backends = make(map[string]Backend, len(p.Backends))
		for name := range p.Backends {
			backend := p.Backends[name]
			backend.Servers = cloneServers(backend.Servers)
			backend.DefaultServer = cloneKeywordArgs(backend.DefaultServer)
			backend.Body = slices.Clone(backend.Body)
			backend.Comments = slices.Clone(backend.Comments)
			cloned.Backends[name] = backend
		}
	}
	cloned.Profiles = maps.Clone(p.Profiles)
	if p.Maps != nil {
		cloned.Maps = make(map[string]Map, len(p.Maps))
		for path, sourceMap := range p.Maps {
			sourceMap.Entries = slices.Clone(sourceMap.Entries)
			cloned.Maps[path] = sourceMap
		}
	}
	if p.CRTLists != nil {
		cloned.CRTLists = make(map[string]CRTList, len(p.CRTLists))
		for path, crtList := range p.CRTLists {
			crtList.Entries = cloneCRTListEntries(crtList.Entries)
			cloned.CRTLists[path] = crtList
		}
	}
	cloned.Files = slices.Clone(p.Files)
	return &cloned
}

// ExactlyEqual compares the complete render identity without trusting a
// digest. It returns false when either plan came from a blob and therefore no
// longer carries the controller-local exact bytes.
func ExactlyEqual(left, right *Plan) bool {
	if left == right {
		// The same object; only its exactness is in question. A deep compare
		// of a plan with itself cost 3 ms per pod per deployment at scale.
		return hasExactIdentity(left)
	}
	if !hasExactIdentity(left) || !hasExactIdentity(right) {
		return false
	}
	leftCopy := *left
	rightCopy := *right
	leftCopy.ID = ""
	rightCopy.ID = ""
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func hasExactIdentity(plan *Plan) bool {
	if plan == nil {
		return false
	}
	for i := range plan.Sections {
		if !plan.Sections[i].TextKnown {
			return false
		}
	}
	for name := range plan.Backends {
		if !plan.Backends[name].ContentKnown {
			return false
		}
	}
	for i := range plan.Files {
		if !plan.Files[i].ContentKnown {
			return false
		}
	}
	return true
}

func cloneServers(source []Server) []Server {
	cloned := slices.Clone(source)
	for index := range cloned {
		if source[index].Weight != nil {
			weight := *source[index].Weight
			cloned[index].Weight = &weight
		}
		cloned[index].Extra = cloneKeywordArgs(source[index].Extra)
	}
	return cloned
}

func cloneKeywordArgs(source []KeywordArg) []KeywordArg {
	cloned := slices.Clone(source)
	for index := range cloned {
		cloned[index].Args = slices.Clone(source[index].Args)
	}
	return cloned
}

func cloneCRTListEntries(source []CRTListEntry) []CRTListEntry {
	cloned := slices.Clone(source)
	for index := range cloned {
		cloned[index].Options = cloneKeywordArgs(source[index].Options)
		cloned[index].SNIFilters = slices.Clone(source[index].SNIFilters)
	}
	return cloned
}

// CurrentConfig is the client-native-free view of the running configuration
// that templates read as `currentConfig`.
type CurrentConfig struct {
	ServerIndex map[string]map[string]ServerAddr `json:"serverIndex"`
}

// ServerAddr is one server's address as templates see it. Port is a pointer
// because a server line may omit it and templates dereference it.
type ServerAddr struct {
	Address string
	Port    *int64
}

// Canonical returns the deterministic JSON encoding of the plan with the ID
// and every sealed file's digest cleared, so the ID can be a digest over it.
// encoding/json sorts map keys and keeps struct fields in declaration order.
func (p *Plan) Canonical() []byte {
	clone := *p
	clone.ID = ""
	clone.Files = ClearSealedDigests(p.Files)
	encoded, err := json.Marshal(&clone)
	if err != nil {
		// Only a new field that is not JSON-safe can get here, and digesting a
		// partial encoding would make two different plans compare equal.
		panic(fmt.Sprintf("renderplan: encoding the plan failed: %v", err))
	}
	return encoded
}

// ClearSealedDigests returns files with every sealed file's digest cleared,
// sharing the slice when no file is sealed.
func ClearSealedDigests(files []File) []File {
	cleared := files
	copied := false
	for i := range files {
		if files[i].Seal == "" {
			continue
		}
		if !copied {
			cleared, copied = slices.Clone(files), true
		}
		cleared[i].Digest = ""
	}
	return cleared
}

// ComputeID sets the plan ID to the digest of its canonical encoding.
func (p *Plan) ComputeID() {
	p.ID = Digest(p.Canonical())
}

// CurrentConfig projects the plan's backend servers into the shape templates
// read as `currentConfig`.
func (p *Plan) CurrentConfig() CurrentConfig {
	index := make(map[string]map[string]ServerAddr, len(p.Backends))
	for name := range p.Backends {
		declared := p.Backends[name].Servers
		if len(declared) == 0 {
			continue
		}
		servers := make(map[string]ServerAddr, len(declared))
		for _, server := range declared {
			port := int64(server.Port)
			servers[server.Name] = ServerAddr{Address: server.Address, Port: &port}
		}
		index[name] = servers
	}
	return CurrentConfig{ServerIndex: index}
}

// Digest returns the xxhash64 of b as fixed-width hex. It is the only hash the
// plan uses, so digests from different producers are comparable.
func Digest(b []byte) string {
	return FormatDigest(xxhash.Sum64(b))
}

// DigestString is Digest over a string, without copying it to a byte slice.
func DigestString(s string) string {
	return FormatDigest(xxhash.Sum64String(s))
}

// FormatDigest renders an xxhash64 sum as the plan's 16-character hex digest.
func FormatDigest(sum uint64) string {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], sum)
	return hex.EncodeToString(raw[:])
}
