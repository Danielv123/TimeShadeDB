package ingest

import (
	"bytes"
	"encoding/json"
	"strings"
)

// A minimal scanner over JSON text that encoding/json has already validated. It finds
// spans without allocating, so the messages of an envelope (and the entries of a move
// message) can be counted and decoded a window at a time instead of all at once.

func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString returns the index after the string starting at b[i] == '"'.
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(b)
}

// skipValue returns the index after the value starting at b[i].
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return i
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for ; i < len(b); i++ {
			switch b[i] {
			case '"':
				i = skipString(b, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return len(b)
	default: // number, true, false, null
		for i < len(b) {
			switch b[i] {
			case ',', ']', '}', ' ', '\t', '\r', '\n':
				return i
			}
			i++
		}
		return i
	}
}

// findMember returns the value of key in the object obj (nil if absent). Like
// encoding/json it matches keys case-insensitively and the last duplicate wins.
func findMember(obj []byte, key string) []byte {
	i := skipWS(obj, 0)
	if i >= len(obj) || obj[i] != '{' {
		return nil
	}
	var found []byte
	i++
	for {
		i = skipWS(obj, i)
		if i >= len(obj) || obj[i] != '"' {
			return found
		}
		k0 := i
		i = skipString(obj, i)
		kraw := obj[k0:i]
		i = skipWS(obj, i)
		if i >= len(obj) || obj[i] != ':' {
			return found
		}
		i = skipWS(obj, i+1)
		v0 := i
		i = skipValue(obj, i)
		if keyMatches(kraw, key) {
			found = obj[v0:i]
		}
		i = skipWS(obj, i)
		if i < len(obj) && obj[i] == ',' {
			i++
		}
	}
}

func keyMatches(quoted []byte, key string) bool {
	if len(quoted) < 2 {
		return false
	}
	inner := quoted[1 : len(quoted)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return strings.EqualFold(string(inner), key)
	}
	var s string
	return json.Unmarshal(quoted, &s) == nil && strings.EqualFold(s, key)
}

// arrayIter iterates the elements of a JSON array. The zero value has no elements.
type arrayIter struct {
	b []byte
	i int
}

// newArrayIter iterates arr, which must start with '['.
func newArrayIter(arr []byte) arrayIter { return arrayIter{b: arr, i: 1} }

// next returns the next element's text.
func (it *arrayIter) next() ([]byte, bool) {
	i := skipWS(it.b, it.i)
	if i < len(it.b) && it.b[i] == ',' {
		i = skipWS(it.b, i+1)
	}
	if i >= len(it.b) || it.b[i] == ']' {
		it.i = len(it.b)
		return nil, false
	}
	j := skipValue(it.b, i)
	it.i = j
	return it.b[i:j], true
}

// countArray returns the number of elements of the array arr, stopping once it exceeds
// limit.
func countArray(arr []byte, limit int) int {
	it := newArrayIter(arr)
	n := 0
	for n <= limit {
		if _, ok := it.next(); !ok {
			break
		}
		n++
	}
	return n
}
