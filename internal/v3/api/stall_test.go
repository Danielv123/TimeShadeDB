package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
)

// openPartialPost sends the headers of an ingest POST and part of its body, and leaves
// the connection open.
func openPartialPost(t *testing.T, url string, length int, sent string) net.Conn {
	t.Helper()
	host := strings.TrimPrefix(url, "http://")
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "POST /api/v3/ingest HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-ndjson\r\nContent-Length: %d\r\n\r\n%s", host, length, sent)
	return c
}

// TestIngestStalledClient: a client that sends part of a body and stops must not hold
// up other ingest requests; it gets 408 once the read timeout passes.
func TestIngestStalledClient(t *testing.T) {
	f := newFixture(t, fixtureOpt{cfg: func(c *Config) { c.IngestReadTimeout = 1500 * time.Millisecond }})
	line := envLine(t, 60, f.clock(), chunkMsg(1, 1, 0x4444))
	stalled := openPartialPost(t, f.ts.URL, 10000, line[:20])
	time.Sleep(100 * time.Millisecond) // let the server start reading it

	done := make(chan resp, 1)
	go func() { done <- f.post("/api/v3/ingest", line) }()
	select {
	case r := <-done:
		r.wantStatus(t, 200, "ingest beside a stalled client")
	case <-time.After(1 * time.Second):
		t.Fatal("an ingest waited behind a stalled body")
	}

	// The stalled request times out.
	_ = stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(stalled), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled body: status %d (%s)", res.StatusCode, b)
	}
}

// TestIngestSpoolBudget: bodies being received beyond the spool budget get 503.
func TestIngestSpoolBudget(t *testing.T) {
	f := newFixture(t, fixtureOpt{cfg: func(c *Config) { c.IngestSpool = 100 << 10 }})
	line := envLine(t, 60, f.clock(), chunkMsg(1, 1, 0x4444))
	// One body announced at 90 KiB holds most of the budget while it trickles in.
	openPartialPost(t, f.ts.URL, 90<<10, line[:20])
	time.Sleep(100 * time.Millisecond)
	r := f.post("/api/v3/ingest", line+strings.Repeat(" ", 20<<10)+"\n")
	r.wantError(t, 503, "overloaded", "spool full")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
	// A small body still fits.
	f.post("/api/v3/ingest", line).wantStatus(t, 200, "small body beside a held budget")
}

// TestLiveMethodsAndClose: HEAD on live is refused, and CloseLive ends open streams.
func TestLiveMethodsAndClose(t *testing.T) {
	f := newFixture(t, fixtureOpt{})
	req, _ := http.NewRequest(http.MethodHead, f.ts.URL+"/api/v3/live?ds="+dsID, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD live: status %d", res.StatusCode)
	}
	s := openSSE(t, f.ts.URL+"/api/v3/live?ds="+dsID)
	s.next(t, "hello")
	f.srv.CloseLive()
	deadline := time.After(2 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-s.events:
		case <-deadline:
			t.Fatal("live stream still open after CloseLive")
		}
	}
	f.get("/api/v3/live?ds="+dsID).wantError(t, 503, "overloaded", "live after CloseLive")
}

// TestIngestKicksCommitUnderPressure: once ingest has filled a quarter of the mover
// open rows, the commit loop steps at once instead of waiting for its period, so a
// fast replay is committed before the store refuses batches.
func TestIngestKicksCommitUnderPressure(t *testing.T) {
	f := newFixture(t, fixtureOpt{maxRows: 4000, cfg: func(c *Config) { c.CommitEvery = time.Hour }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.srv.Run(ctx)
	sfc := core.SurfaceKey{Map: testDS.Map, Surface: "nauvis"}
	meta := core.MoverMeta{ID: "9.9.9", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 1, Kinds: "L"}
	start := f.clock() - 30*core.Minute
	var samples []core.MoverSample
	for i := range 1500 {
		samples = append(samples, core.MoverSample{Surface: sfc, Meta: meta, T: start + core.TimeMs(i)*core.Second, X: float64(i), Orientation: 0.25})
	}
	if _, err := f.mov.Append(context.Background(), samples); err != nil {
		t.Fatal(err)
	}
	f.srv.Notifier().SamplesApplied(samples)
	deadline := time.Now().Add(5 * time.Second)
	for f.mov.Pressure() >= 0.25 {
		if time.Now().After(deadline) {
			t.Fatalf("no commit: pressure %.2f", f.mov.Pressure())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
