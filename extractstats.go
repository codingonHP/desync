package desync

import (
	"sync/atomic"
)

// ExtractStats contains detailed statistics about a file extract operation, such
// as if data chunks were copied from seeds or cloned.
type ExtractStats struct {
	ChunksTotal     int    `json:"chunks-total"`
	ChunksFromSeeds uint64 `json:"chunks-from-seeds"`
	ChunksFromStore uint64 `json:"chunks-from-store"`
	ChunksInPlace   uint64 `json:"chunks-in-place"`
	BytesTotal      int64  `json:"bytes-total"`
	BytesCopied     uint64 `json:"bytes-copied-from-seeds"`
	BytesCloned     uint64 `json:"bytes-cloned-from-seeds"`
	Seeds           int    `json:"seeds"`
	Blocksize       uint64 `json:"blocksize"`
}

func (s *ExtractStats) incChunksFromStore() {
	atomic.AddUint64(&s.ChunksFromStore, 1)
}

func (s *ExtractStats) incChunksInPlace() {
	atomic.AddUint64(&s.ChunksInPlace, 1)
}

func (s *ExtractStats) addChunksFromSeed(n uint64) {
	atomic.AddUint64(&s.ChunksFromSeeds, n)
}

func (s *ExtractStats) addBytesCopied(n uint64) {
	atomic.AddUint64(&s.BytesCopied, n)
}

func (s *ExtractStats) addBytesCloned(n uint64) {
	atomic.AddUint64(&s.BytesCloned, n)
}
