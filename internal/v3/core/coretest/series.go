package coretest

import (
	"context"
	"errors"
	"testing"

	"timeshadedb/internal/v3/core"
)

// SeriesFactory opens a new, empty series store. The test closes it.
type SeriesFactory func(t *testing.T) core.SeriesStore

// SeriesStore runs the series contract tests.
func SeriesStore(t *testing.T, open SeriesFactory) {
	ctx := context.Background()

	t.Run("AppendChainRead", func(t *testing.T) {
		st := open(t)
		defer st.Close()
		if _, err := st.Open(ctx, "ds/tiles/0/0_0", false); !errors.Is(err, core.ErrNoTile) {
			t.Fatalf("open missing err = %v", err)
		}
		s := get(st.Open(ctx, "ds/tiles/0/0_0", true))(t)
		rec := func(start core.TimeMs, kind core.RecordKind, payload string) core.Record {
			return core.Record{RecordMeta: core.RecordMeta{TStart: start, TLen: 60_000, Kind: kind}, Payload: []byte(payload)}
		}
		for i, r := range []core.Record{
			rec(0, core.RecordDelta, "d0"),
			rec(60_000, core.RecordDelta, "d1"),
			rec(60_000, core.RecordKeyframe, "k1"), // same TEnd as d1: the state after it
			rec(120_000, core.RecordDelta, "d2"),
			rec(240_000, core.RecordDelta, "d3"),
		} {
			seq := get(s.Append(ctx, r))(t)
			if seq != uint64(i) {
				t.Fatalf("seq = %d, want %d", seq, i)
			}
		}
		if _, err := s.Append(ctx, rec(0, core.RecordDelta, "late")); !errors.Is(err, core.ErrOutOfOrder) {
			t.Fatalf("out-of-order append err = %v", err)
		}
		if s.Len() != 5 {
			t.Fatalf("len = %d", s.Len())
		}
		names := func(recs []core.Record) string {
			out := ""
			for _, r := range recs {
				out += string(r.Payload) + " "
			}
			return out
		}
		for _, c := range []struct {
			t    core.TimeMs
			want string
		}{
			{59_999, ""},
			{60_000, "d0 "},
			{119_999, "d0 "},
			{120_000, "k1 "},
			{180_000, "k1 d2 "},
			{1 << 50, "k1 d2 d3 "},
		} {
			if got := names(get(s.Chain(ctx, c.t))(t)); got != c.want {
				t.Fatalf("Chain(%d) = %q, want %q", c.t, got, c.want)
			}
		}
		m := get(s.Meta(2))(t)
		if m.Seq != 2 || m.Kind != core.RecordKeyframe || m.TEnd() != 120_000 || m.Len != 2 {
			t.Fatalf("meta = %+v", m)
		}
		r := get(s.Read(ctx, 3))(t)
		r.Payload[0] = 'X'
		if again := get(s.Read(ctx, 3))(t); string(again.Payload) != "d2" {
			t.Fatal("Read returned shared payload memory")
		}
		if _, err := s.Read(ctx, 5); !errors.Is(err, core.ErrNoVersion) {
			t.Fatalf("read past end err = %v", err)
		}
		ok(t, s.Sync())
		ok(t, s.Close())
		s2 := get(st.Open(ctx, "ds/tiles/0/0_0", false))(t)
		if s2.Len() != 5 {
			t.Fatalf("reopened len = %d", s2.Len())
		}
		get(st.Open(ctx, "ds/tiles/0/1_0", true))(t)
		get(st.Open(ctx, "other/x", true))(t)
		list := get(st.List(ctx, "ds/"))(t)
		if len(list) != 2 || list[0] != "ds/tiles/0/0_0" || list[1] != "ds/tiles/0/1_0" {
			t.Fatalf("list = %v", list)
		}
		for _, bad := range []string{"", "../x", "a//b", "a/./b", "a b"} {
			if _, err := st.Open(ctx, bad, true); err == nil {
				t.Fatalf("bad name %q accepted", bad)
			}
		}
	})
}
