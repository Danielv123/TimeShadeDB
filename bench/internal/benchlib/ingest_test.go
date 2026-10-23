package benchlib

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"timeshadedb/internal/core"
)

// randomRows returns n chunk rows with incompressible charts (the worst case for the
// envelope size).
func randomRows(n int) []ChunkRow {
	rng := rand.New(rand.NewSource(3))
	rows := make([]ChunkRow, n)
	for i := range rows {
		px := new(core.ChunkPixels)
		for k := range px {
			px[k] = uint16(rng.Intn(1 << 16))
		}
		rows[i] = ChunkRow{T: 1791630000000 + int64(i)*10, Map: "m", Surface: "nauvis", Force: "player", CX: int32(i % 64), CY: int32(i / 64), Pixels: px}
	}
	return rows
}

func lines(b []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func TestV3EnvelopesStayUnderTheLineCap(t *testing.T) {
	v := &V3{Src: "standalone:bench"}
	rows := randomRows(4096)
	bodies, err := v.EncodeChunks(rows)
	if err != nil {
		t.Fatal(err)
	}
	total, envs := 0, 0
	for _, b := range bodies {
		if len(b.Data) > MaxBodyBytes {
			t.Fatalf("body of %d bytes", len(b.Data))
		}
		if b.Kind != "chunks" {
			t.Fatalf("kind %q", b.Kind)
		}
		n := 0
		for _, l := range lines(b.Data) {
			if len(l) > MaxEnvelopeBytes+envelopeOverhead {
				t.Fatalf("envelope line of %d bytes", len(l))
			}
			var env core.Envelope
			if err := json.Unmarshal(l, &env); err != nil {
				t.Fatal(err)
			}
			n += len(env.Msgs)
			envs++
		}
		if n != b.Rows {
			t.Fatalf("body says %d rows, holds %d messages", b.Rows, n)
		}
		total += b.Rows
	}
	if total != len(rows) || envs < 5 {
		t.Fatalf("%d rows in %d envelopes, want %d rows in several envelopes", total, envs, len(rows))
	}

	// A large mover batch with carriages: several move messages, each under its cap.
	synth := NewSynth(DefaultSynth())
	mrows := synth.NextMovers(16384)
	for i := range mrows {
		for k := 0; k < 6; k++ {
			mrows[i].Entry.Car = append(mrows[i].Entry.Car, [3]float64{float64(k) * 7.123456, 3.987654, 0.123456})
		}
	}
	mb, err := v.EncodeMovers(mrows)
	if err != nil {
		t.Fatal(err)
	}
	msgs, samples := 0, 0
	for _, b := range mb {
		for _, l := range lines(b.Data) {
			if len(l) > MaxEnvelopeBytes+envelopeOverhead {
				t.Fatalf("mover envelope line of %d bytes", len(l))
			}
			var env core.Envelope
			if err := json.Unmarshal(l, &env); err != nil {
				t.Fatal(err)
			}
			for _, m := range env.Msgs {
				if len(m) > MaxMoveMsgBytes {
					t.Fatalf("move message of %d bytes", len(m))
				}
				var mv core.MoveMsg
				if err := json.Unmarshal(m, &mv); err != nil {
					t.Fatal(err)
				}
				samples += len(mv.M)
				msgs++
			}
		}
	}
	if samples != len(mrows) || msgs < 2 {
		t.Fatalf("%d samples in %d move messages", samples, msgs)
	}
}

func TestV3IngestChecksTheAnswer(t *testing.T) {
	s := startFake(t, "v3")
	ctx := context.Background()
	one := func(src string) func() ([]ChunkRow, bool) {
		sent := false
		return func() ([]ChunkRow, bool) {
			if sent {
				return nil, false
			}
			sent = true
			return randomRows(300), true
		}
	}

	// Large batches are split and every row is stored.
	c := NewClient(s.BaseURL, 2)
	v := &V3{C: c, Src: "standalone:bench"}
	st, err := SendPipeline(ctx, c, 1, 2, PipelineOptions{}, one(""), v.EncodeChunks)
	if err != nil || st.Outcome.Accepted != 300 {
		t.Fatalf("plain ingest: %+v %v", st.Outcome, err)
	}

	// 200 with rows reported late: an error unless AllowRejected.
	rv := &V3{C: c, Src: "reject"}
	_, err = SendPipeline(ctx, c, 1, 2, PipelineOptions{}, one(""), rv.EncodeChunks)
	if err == nil || !strings.Contains(err.Error(), "stored 0 of 300") {
		t.Fatalf("rejected rows not reported: %v", err)
	}
	st, err = SendPipeline(ctx, c, 1, 2, PipelineOptions{AllowRejected: true}, one(""), rv.EncodeChunks)
	if err != nil || st.Outcome.Late != 300 || st.StoredRate() != 0 {
		t.Fatalf("allow-rejected: %+v %v", st.Outcome, err)
	}

	// A bad line next to a good one: 200 with bad_lines 1.
	good, _ := v.EncodeChunks(randomRows(2))
	bad := Body{Path: good[0].Path, ContentType: good[0].ContentType, Kind: "chunks", Rows: 2,
		Data: append([]byte("{not json\n"), good[0].Data...)}
	_, o, _, _, err := PostIngest(ctx, c, bad, time.Second)
	if err != nil || o.BadLines != 1 || o.Accepted != 2 || o.Shortfall(2) == nil {
		t.Fatalf("bad line: %+v %v", o, err)
	}

	// Persistent 503: bounded by the retry wait, and by the context.
	bv := &V3{C: c, Src: "busy"}
	busy, _ := bv.EncodeChunks(randomRows(1))
	start := time.Now()
	_, _, retries, _, err := PostIngest(ctx, c, busy[0], 350*time.Millisecond)
	if !errors.Is(err, ErrOverloaded) || retries != 3 || time.Since(start) > 5*time.Second {
		t.Fatalf("503 cap: retries %d, %v", retries, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_, _, _, _, err = PostIngest(cctx, c, busy[0], time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("503 with a cancelled context: %v", err)
	}
}

// runParent runs the test binary as a fake bench tool and returns the pid of the server
// it started.
func runParent(t *testing.T, mode string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "BENCHLIB_FAKE="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := 0
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if n, ok := strings.CutPrefix(sc.Text(), "pid "); ok {
			for _, ch := range n {
				pid = pid*10 + int(ch-'0')
			}
		}
	}
	cmd.Wait()
	if pid == 0 {
		t.Fatalf("%s parent did not report a server pid", mode)
	}
	t.Cleanup(func() {
		if ProcessAlive(pid) {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
	})
	return pid
}

func waitGone(pid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestFatalStopsServers(t *testing.T) {
	pid := runParent(t, "fatal")
	if !waitGone(pid) {
		t.Fatalf("server %d still running after the tool exited through Fatal", pid)
	}
}

func TestServerDiesWithItsParent(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("children are tied to the parent only on Windows (Job Object) and Linux (Pdeathsig)")
	}
	pid := runParent(t, "orphan")
	if !waitGone(pid) {
		t.Fatalf("server %d outlived a tool that exited without stopping it", pid)
	}
}

func TestEncodeChartMatchesCore(t *testing.T) {
	s := NewSynth(DefaultSynth())
	for _, r := range s.NextChunks(50) {
		got, err := core.DecodeChart(encodeChart(r.Pixels))
		if err != nil {
			t.Fatal(err)
		}
		if *got != *r.Pixels {
			t.Fatal("chart does not round-trip")
		}
	}
}
