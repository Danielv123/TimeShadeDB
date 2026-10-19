package ingest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"timeshadedb/internal/v3/core"
)

// checkFastMsg: whenever the fast decoder accepts a message, encoding/json must accept
// it too and decode the same values.
func checkFastMsg(t *testing.T, raw []byte) bool {
	t.Helper()
	var fm wireMsg
	ok := decodeMsgFast(raw, &fm, &interner{})
	var jm wireMsg
	jerr := json.Unmarshal(raw, &jm)
	if !ok {
		return false
	}
	if jerr != nil {
		t.Fatalf("fast path accepted %s; encoding/json: %v", raw, jerr)
	}
	if fm.hasD {
		s := string(fm.dRaw)
		fm.D = &s
	}
	fm.dRaw, fm.hasD = nil, false
	if !reflect.DeepEqual(fm, jm) {
		t.Fatalf("%s:\nfast %+v\njson %+v", raw, fm, jm)
	}
	return true
}

func checkFastEnv(t *testing.T, line []byte) bool {
	t.Helper()
	var fe envHeader
	msgs, ok := parseEnvelopeFast(line, &fe, &interner{})
	var je envHeader
	jerr := json.Unmarshal(line, &je)
	if !ok {
		return false
	}
	if jerr != nil {
		t.Fatalf("fast path accepted %s; encoding/json: %v", line, jerr)
	}
	if !reflect.DeepEqual(fe, je) {
		t.Fatalf("%s:\nfast %+v\njson %+v", line, fe, je)
	}
	if jm := findMember(line, "msgs"); !bytes.Equal(jm, msgs) {
		t.Fatalf("%s: msgs %q, encoding/json path %q", line, msgs, jm)
	}
	return true
}

var fastMsgCases = []struct {
	raw  string
	fast bool // the fast path should take it
}{
	{`{"type":"chunk","s":"nauvis","f":"player","x":1,"y":-2,"d":"eJzt","tick":600}`, true},
	{` { "type" : "chunk" , "s":"nauvis","f":"player","x":-0,"y":2e3} `, true},
	{`{"type":"chunk","s":"nauvis","f":"player","x":1,"y":2,"d":null}`, true},
	{`{"type":"chunk","s":"nauvis","f":"player","x":null,"y":2}`, true},
	{`{"type":"move","s":"nauvis","m":[{"j":"5.1.7","k":"t","f":"player","name":"locomotive","x":1.5,"y":2.5,"h":0.25,"n":2,"o":"LC","stop":true,"car":[[1.5,2.5,0.25],[1.5,9.5,0.25]]},{"j":"p:bob","k":"p","x":4,"y":4,"tick":590},{"j":"9","gone":true}]}`, true},
	{`{"type":"move","s":"nauvis","m":[]}`, true},
	{`{"type":"move","s":"nauvis","m":null}`, true},
	{`{"type":"move","s":"nauvis","m":[{"j":"1","car":[]}]}`, true},
	{`{"type":"move","s":"nauvis","m":[{"j":"1","es":false,"stop":null,"h":null}]}`, true},
	{`{"type":"tag","s":"nauvis","f":"player","n":3,"x":1,"y":2,"icon":"item/iron-plate","text":"hi"}`, true},
	{`{"type":"recipe","s":"nauvis","f":"player","u":99,"x":1,"y":2,"r":"gear","q":"normal"}`, true},
	{`{"type":"rail","s":"nauvis","f":"player","u":5,"x":1,"y":2,"name":"straight-rail","dir":4}`, true},
	{`{"type":"adopt","j":"5.1.7","trace":"t"}`, true},
	{`{"type":"chunk","s":"nauvis","s":"gleba"}`, true}, // last duplicate wins
	{`{}`, true},
	// Left to encoding/json.
	{`{"type":"chunk","S":"nauvis"}`, false},
	{`{"type":"chunk","s":"n\u0061uvis"}`, false},
	{`{"type":"chunk","s":"nä"}`, true}, // valid UTF-8 is kept as is
	{`{"type":"chunk","s":"n\xffa"}`, false},
	{`{"type":"chunk","x":1.5e400}`, false},
	{`{"type":"chunk","tick":1.5}`, false},
	{`{"type":"chunk","x":"1"}`, false},
	{`{"type":"chunk","x":01}`, false},
	{`{"type":"chunk","x":1.}`, false},
	{`{"type":"chunk","x":NaN}`, false},
	{`{"type":"chunk","x":1,}`, false},
	{`{"type":"rail","dir":256}`, false},
	{`{"type":"rail","dir":-1}`, false},
	{`{"type":"surfaces","list":[{"name":"nauvis","tile":true}],"created":true}`, false},
	{`{"type":"move","m":[{"j":"1","car":[[1,2]]}]}`, false},
	{`{"type":"move","m":[null]}`, false},
	{`{"type":"move","m":[{"j":"1"}],"m":[{"j":"2"}]}`, false},
	{`{"type":"move","m":[{"j":"1","extra":1}]}`, false},
	{`{"type":"chunk"} x`, false},
	{`[1]`, false},
	{`{"type":"chunk","s":"a` + "\x01" + `"}`, false},
}

func TestFastJSONMatchesEncodingJSON(t *testing.T) {
	for _, c := range fastMsgCases {
		if got := checkFastMsg(t, []byte(c.raw)); got != c.fast {
			t.Errorf("%s: fast path %v, want %v", c.raw, got, c.fast)
		}
	}
	envs := []struct {
		line string
		fast bool
	}{
		{`{"v":1,"tick":600,"speed":1,"src":"clusterio:5","map":"gw-a.1","instance":5,"recv_ms":1792000000000,"seq":7,"msgs":[{"type":"a"},1,"x",[1,[2]],{"k":"]"}]}`, true},
		{`{"v":1,"tick":600,"src":"s","map":"m","recv_ms":5}`, true},
		{`{"v":1,"src":"s","map":"m","recv_ms":5,"msgs":null,"seq":null,"future":{"a":[1,2,{"b":null}]}}`, true},
		{`{"v":null,"src":"s"}`, true},
		{`{"msgs":[1],"v":1,"msgs":[2,3]}`, true},
		{`{"v":1,"MSGS":[4]}`, false},
		{`{"v":1,"m\u0073gs":[5]}`, false},
		{`{"v":1,"msgs":[1,]}`, false},
		{`{"v":1.5}`, false},
		{`{"v":1}{}`, false},
		{`{"v":1,"Src":"s"}`, false},
		{`{"v":1,"é":1}`, false},
	}
	for _, c := range envs {
		if got := checkFastEnv(t, []byte(c.line)); got != c.fast {
			t.Errorf("%s: fast path %v, want %v", c.line, got, c.fast)
		}
	}
	// Deep nesting goes to encoding/json.
	deep := `{"v":1,"x":` + string(bytes.Repeat([]byte("["), 100)) + string(bytes.Repeat([]byte("]"), 100)) + `}`
	if checkFastEnv(t, []byte(deep)) {
		t.Error("deep nesting took the fast path")
	}
}

// TestFastJSONChartRoundTrip: a real chart decodes the same either way.
func TestFastJSONChartRoundTrip(t *testing.T) {
	d := core.EncodeChart(chart(0x1234))
	raw := []byte(`{"type":"chunk","s":"nauvis","f":"player","x":3,"y":4,"d":"` + d + `"}`)
	if !checkFastMsg(t, raw) {
		t.Fatal("not taken by the fast path")
	}
	m := decodeMsg(raw, 1<<20)
	got, ok := m.chart()
	if !ok || string(got) != d {
		t.Fatalf("chart %q", got)
	}
}

func FuzzFastJSON(f *testing.F) {
	for _, c := range fastMsgCases {
		f.Add([]byte(c.raw))
	}
	f.Add([]byte(`{"v":1,"tick":600,"src":"s","map":"m","recv_ms":5,"seq":3,"msgs":[{"type":"chunk","x":1}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		checkFastMsg(t, b)
		checkFastEnv(t, b)
	})
}
