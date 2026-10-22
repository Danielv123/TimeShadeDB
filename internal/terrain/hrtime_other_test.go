//go:build !windows

package terrain

import "time"

type hrStamp = time.Time

func hrNow() hrStamp                  { return time.Now() }
func hrSince(s hrStamp) time.Duration { return time.Since(s) }
