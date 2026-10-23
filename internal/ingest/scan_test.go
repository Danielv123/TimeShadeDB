package ingest

import (
	"strings"
	"testing"
)

func TestScanEnvelopeMessages(t *testing.T) {
	head := `"v":1,"tick":1,"src":"s","map":"m","recv_ms":5`
	cases := []struct {
		line string
		msgs []string // nil: no messages
		bad  bool
	}{
		{`{` + head + `,"msgs":[{"type":"a"} , 1,"x,]}",[1,[2]],{"k":"]"}]}`, []string{`{"type":"a"}`, `1`, `"x,]}"`, `[1,[2]]`, `{"k":"]"}`}, false},
		{`{` + head + `}`, nil, false},
		{`{` + head + `,"msgs":null}`, nil, false},
		{`{` + head + `,"msgs":[]}`, nil, false},
		{`{"msgs":[1],` + head + `,"msgs":[2,3]}`, []string{`2`, `3`}, false}, // last duplicate wins, like encoding/json
		{`{` + head + `,"MSGS":[4]}`, []string{`4`}, false},                   // case-insensitive, like encoding/json
		{`{` + head + `,"m\u0073gs":[5]}`, []string{`5`}, false},
		{`{` + head + `,"x":{"msgs":[6]}}`, nil, false}, // only the top level
		{`{` + head + `,"msgs":{"a":1}}`, nil, true},
		{`{` + head + `,"msgs":"[1]"}`, nil, true},
		{` { ` + head + ` , "msgs" : [ "a\"b" , 7 ] } `, []string{`"a\"b"`, `7`}, false},
	}
	for _, c := range cases {
		l := parsedLine{raw: []byte(c.line)}
		l.parseEnvelope()
		if (l.err != nil) != c.bad {
			t.Fatalf("%s: err %v", c.line, l.err)
		}
		var got []string
		for {
			m, ok := l.it.next()
			if !ok {
				break
			}
			got = append(got, string(m))
		}
		if strings.Join(got, "|") != strings.Join(c.msgs, "|") {
			t.Fatalf("%s: messages %q, want %q", c.line, got, c.msgs)
		}
	}
	if n := countArray([]byte(`[1,2,3,4,5]`), 3); n != 4 {
		t.Fatalf("countArray stops past the limit: %d", n)
	}
}
