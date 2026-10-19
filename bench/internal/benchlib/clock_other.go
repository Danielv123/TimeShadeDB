//go:build !windows

package benchlib

import "time"

var clockStart = time.Now()

// Tick is a high-resolution timestamp.
type Tick int64

// Now returns the current high-resolution timestamp.
func Now() Tick { return Tick(time.Since(clockStart)) }

// Since returns the time elapsed since t.
func Since(t Tick) time.Duration { return time.Duration(Now() - t) }
