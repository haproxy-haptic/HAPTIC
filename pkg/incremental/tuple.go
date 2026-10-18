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

package incremental

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EncodeTuple encodes an ordered tuple of strings as one key. Each element is
// prefixed with its canonical uvarint length, so distinct tuples never share an
// encoding and every element may hold any byte.
func EncodeTuple(head string, tail ...string) string {
	encodedLen := tupleElementEncodedLen(head)
	for _, element := range tail {
		encodedLen += tupleElementEncodedLen(element)
	}
	var encoded strings.Builder
	encoded.Grow(encodedLen)
	writeTupleElement(&encoded, head)
	for _, element := range tail {
		writeTupleElement(&encoded, element)
	}
	return encoded.String()
}

func tupleElementEncodedLen(value string) int {
	return tupleUvarintLen(uint64(len(value))) + len(value)
}

func tupleUvarintLen(value uint64) int {
	length := 1
	for value >= 0x80 {
		value >>= 7
		length++
	}
	return length
}

func writeTupleElement(encoded *strings.Builder, value string) {
	var prefix [binary.MaxVarintLen64]byte
	length := binary.PutUvarint(prefix[:], uint64(len(value)))
	encoded.Write(prefix[:length])
	encoded.WriteString(value)
}

// TupleDecoder reads the elements of an [EncodeTuple] value in order.
type TupleDecoder struct {
	value    string
	position int
}

// NewTupleDecoder starts reading value at its first element.
func NewTupleDecoder(value string) TupleDecoder {
	return TupleDecoder{value: value}
}

// Next returns the next element. It reports false when no element is left or
// the remaining bytes are not a canonical element.
func (d *TupleDecoder) Next() (string, bool) {
	if d == nil || d.position < 0 || d.position >= len(d.value) {
		return "", false
	}
	length, prefixLength, ok := readTupleUvarint(d.value[d.position:])
	if !ok || length > math.MaxInt {
		return "", false
	}
	size := int(length)
	if size > len(d.value)-d.position-prefixLength {
		return "", false
	}
	start := d.position + prefixLength
	end := start + size
	d.position = end
	return d.value[start:end], true
}

// Done reports whether every byte of the value has been read.
func (d *TupleDecoder) Done() bool {
	return d != nil && d.position == len(d.value)
}

func readTupleUvarint(value string) (decoded uint64, prefixLength int, ok bool) {
	for index := 0; index < binary.MaxVarintLen64 && index < len(value); index++ {
		current := value[index]
		if index == binary.MaxVarintLen64-1 && current > 1 {
			return 0, 0, false
		}
		decoded |= uint64(current&0x7f) << (index * 7)
		if current < 0x80 {
			length := index + 1
			return decoded, length, tupleUvarintLen(decoded) == length
		}
	}
	return 0, 0, false
}

// DescribeTuple renders an [EncodeTuple] value for people, as
// head(element, ...). Any other value comes back Go-quoted, so a description
// never carries a raw control character.
func DescribeTuple(value string) string {
	decoder := NewTupleDecoder(value)
	head, ok := decoder.Next()
	if !ok || !plainTupleElement(head) {
		return strconv.Quote(value)
	}
	var described strings.Builder
	described.WriteString(head)
	described.WriteByte('(')
	for first := true; !decoder.Done(); first = false {
		element, ok := decoder.Next()
		if !ok {
			return strconv.Quote(value)
		}
		if !first {
			described.WriteString(", ")
		}
		if plainTupleElement(element) {
			described.WriteString(element)
		} else {
			described.WriteString(strconv.Quote(element))
		}
	}
	described.WriteByte(')')
	return described.String()
}

// plainTupleElement reports whether element reads unambiguously without quotes.
func plainTupleElement(element string) bool {
	if element == "" || !utf8.ValidString(element) {
		return false
	}
	for _, r := range element {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) || strings.ContainsRune(`"(),`, r) {
			return false
		}
	}
	return true
}

// String describes the key for messages; see [DescribeTuple].
func (k QueryKey) String() string {
	return DescribeTuple(k.id.String())
}

// String describes the key for messages; see [DescribeTuple].
func (k InputKey) String() string {
	return DescribeTuple(k.id.String())
}
