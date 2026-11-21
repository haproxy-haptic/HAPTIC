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

package renderplan

import "slices"

// EqualRecord reports whether both backends declare the same record: every
// field that describes the backend itself rather than its text. The digests,
// the body and comment lines and ContentKnown describe the text and are
// compared by EqualContent. A plan with thousands of backends compares every
// one per deployment, so this is field by field rather than reflective.
func (b *Backend) EqualRecord(other *Backend) bool {
	if b == nil || other == nil {
		return b == other
	}
	return b.Name == other.Name &&
		b.Profile == other.Profile &&
		b.Mode == other.Mode &&
		b.GUID == other.GUID &&
		b.Balance == other.Balance &&
		b.HashType == other.HashType &&
		b.Shape == other.Shape &&
		b.ShapeReason == other.ShapeReason &&
		equalServers(b.Servers, other.Servers) &&
		EqualKeywordArgs(b.DefaultServer, other.DefaultServer)
}

func equalServers(left, right []Server) bool {
	if sameSlice(left, right) {
		return true
	}
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !left[index].Equal(&right[index]) {
			return false
		}
	}
	return true
}

// sameSlice reports whether both slices are the same run of the same backing
// array, in which case their elements are the same values. Plans built from
// one snapshot lineage share their records, so a diff between two of them
// answers most backends here.
func sameSlice[T any](left, right []T) bool {
	return len(left) == len(right) && (len(left) == 0 || &left[0] == &right[0])
}

// EqualContent reports whether both backends carry the same text description:
// the body, comment and record digests, the body and comment lines, and
// whether the content is known. TextDigest is left to the caller, since it
// changes with every render that re-emits the section.
func (b *Backend) EqualContent(other *Backend) bool {
	if b == nil || other == nil {
		return b == other
	}
	return b.BodyDigest == other.BodyDigest &&
		b.CommentsDigest == other.CommentsDigest &&
		b.RecordDigest == other.RecordDigest &&
		b.ContentKnown == other.ContentKnown &&
		(sameSlice(b.Body, other.Body) || slices.Equal(b.Body, other.Body)) &&
		(sameSlice(b.Comments, other.Comments) || slices.Equal(b.Comments, other.Comments))
}

// Equal reports whether both servers declare the same line.
func (s *Server) Equal(other *Server) bool {
	if s == nil || other == nil {
		return s == other
	}
	return s.Name == other.Name &&
		s.Address == other.Address &&
		s.Port == other.Port &&
		equalOptionalInt(s.Weight, other.Weight) &&
		s.Disabled == other.Disabled &&
		s.GUID == other.GUID &&
		s.Comment == other.Comment &&
		s.Template == other.Template &&
		EqualKeywordArgs(s.Extra, other.Extra)
}

// EqualKeywordArgs reports whether both keyword lists declare the same
// keywords with the same arguments in the same order. Nil and empty lists
// are the same list.
func EqualKeywordArgs(left, right []KeywordArg) bool {
	if sameSlice(left, right) {
		return true
	}
	return slices.EqualFunc(left, right, func(a, b KeywordArg) bool {
		return a.Name == b.Name && slices.Equal(a.Args, b.Args)
	})
}

func equalOptionalInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
