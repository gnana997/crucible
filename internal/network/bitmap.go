package network

// bitmap is a fixed-size bit set backed by []uint64. Bit N lives in
// word N/64 at bit position N%64 (LSB-indexed). Intentionally
// minimal: no dynamic resizing, no thread safety (callers provide
// their own synchronization if needed), and only the handful of
// operations the subnet allocator needs.
type bitmap struct {
	words []uint64
	size  int
}

func newBitmap(size int) *bitmap {
	if size <= 0 {
		return &bitmap{}
	}
	return &bitmap{
		words: make([]uint64, (size+63)/64),
		size:  size,
	}
}

func (b *bitmap) set(i int)      { b.words[i>>6] |= 1 << uint(i&63) }
func (b *bitmap) clear(i int)    { b.words[i>>6] &^= 1 << uint(i&63) }
func (b *bitmap) get(i int) bool { return b.words[i>>6]&(1<<uint(i&63)) != 0 }

// findFirstZeroFrom returns the smallest index >= start whose bit
// is zero and < b.size, along with ok=true. Returns (0, false) if
// none exists.
//
// Naive linear scan is fine at /16-scale (16K bits, 256 words).
// The natural optimization when we outgrow it is a word-level
// loop using bits.TrailingZeros64 on (^word | start-mask).
func (b *bitmap) findFirstZeroFrom(start int) (int, bool) {
	if start < 0 {
		start = 0
	}
	if start >= b.size {
		return 0, false
	}
	for i := start; i < b.size; i++ {
		if !b.get(i) {
			return i, true
		}
	}
	return 0, false
}
