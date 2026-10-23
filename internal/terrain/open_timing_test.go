package terrain

import (
	"os"
	"testing"
)

// TestOpenTiming opens an existing database (TSDB_OPEN_DB) a few times and reports the
// startup time. Use it with -cpuprofile to see where startup goes.
func TestOpenTiming(t *testing.T) {
	dir := os.Getenv("TSDB_OPEN_DB")
	if dir == "" {
		t.Skip("TSDB_OPEN_DB not set")
	}
	for i := 0; i < 5; i++ {
		t0 := hrNow()
		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		d := hrSince(t0)
		st := s.Stats()
		s.Close()
		t.Logf("open %d: %v (%d datastores, %d tiles)", i, d, st.Datastores, st.Tiles)
	}
}
