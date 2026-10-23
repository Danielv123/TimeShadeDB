package core

import (
	"context"
	"errors"
)

// RecordKind is the kind of a series record (docs/v3/FORMAT.md "Series files").
type RecordKind uint8

// Series record kinds.
const (
	// RecordDelta changes part of the state; it applies on top of the records before it.
	RecordDelta RecordKind = 1
	// RecordKeyframe is a full state equal to every record up to and including the
	// previous one with the same effective time; reading can start at it.
	RecordKeyframe RecordKind = 2
	// RecordSnapshot is a full state that stands alone (coarse pyramid levels). Reading
	// can start at it.
	RecordSnapshot RecordKind = 3
)

// IsBase reports whether reading can start at a record of this kind.
func (k RecordKind) IsBase() bool { return k == RecordKeyframe || k == RecordSnapshot }

// RecordFlags are kind-specific flags. Bit 0 is reserved for "wide" (u16 palette
// indices) by the terrain format; the series layer does not interpret them.
type RecordFlags uint8

// FlagWide marks terrain payloads that use u16 palette indices.
const FlagWide RecordFlags = 1 << 0

// RecordMeta describes one record without its payload.
type RecordMeta struct {
	// Seq is the record number, from 0, in append order.
	Seq uint64
	// TStart and TLen give the record's time span; TEnd = TStart + TLen is its effective
	// time (terrain: the window end; snapshots: TLen 0).
	TStart TimeMs
	TLen   uint32
	Kind   RecordKind
	Flags  RecordFlags
	// Len is the stored payload length in bytes.
	Len int
}

// TEnd returns the effective time.
func (m RecordMeta) TEnd() TimeMs { return m.TStart + TimeMs(m.TLen) }

// Record is a record with its payload. Series store payloads as given; compression is
// the caller's choice (terrain compresses with zstd before Append).
type Record struct {
	RecordMeta
	Payload []byte
}

// ErrOutOfOrder is returned by Append for a record whose TEnd is before the last
// record's TEnd.
var ErrOutOfOrder = errors.New("record out of order")

// Series is one append-only record log: a .blk payload file and a .idx index of fixed
// 24-byte records (WP2, internal/v3/series; reference memstore.Series). Terrain keeps
// one series per (datastore, z0 tile); the pyramid one per (datastore, coarse tile).
//
// Records are appended with non-decreasing TEnd. A Series is safe for one writer and
// concurrent readers.
type Series interface {
	// Append stores a record and returns its Seq. Kind, TStart, TLen, Flags and the
	// payload are taken from rec; Seq and Len are assigned.
	Append(ctx context.Context, rec Record) (uint64, error)
	// Len returns the number of records.
	Len() uint64
	// Meta returns the metadata of record seq, or ErrNoVersion.
	Meta(seq uint64) (RecordMeta, error)
	// Read returns record seq with its payload, or ErrNoVersion.
	Read(ctx context.Context, seq uint64) (Record, error)
	// Chain returns the records needed to rebuild the state at time t: the newest base
	// record (keyframe or snapshot) with TEnd <= t, or record 0 if there is none, then
	// every later record with TEnd <= t, in Seq order. Empty when no record has
	// TEnd <= t.
	Chain(ctx context.Context, t TimeMs) ([]Record, error)
	// Sync makes every appended record durable (no-op in memory).
	Sync() error
	Close() error
}

// SeriesStore opens series by name. Names are slash-separated relative paths made of
// id-safe parts, for example "<datastore id>/tiles/0/3_-2".
type SeriesStore interface {
	// Open opens the named series. With create false a missing series returns ErrNoTile.
	Open(ctx context.Context, name string, create bool) (Series, error)
	// List returns the names that start with prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	Close() error
}
