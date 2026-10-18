// Package delta implements the rsync block-matching algorithm.
//
// The receiver splits its existing copy of a file (the "basis") into fixed
// size blocks and computes a weak rolling checksum plus a strong hash for each
// block. The sender slides a window over the new file, using the rolling
// checksum to find candidate blocks cheaply and the strong hash to confirm
// them, and emits a stream of operations: literal bytes, or "copy blocks
// i..i+n from the basis". The receiver replays those operations against the
// basis to reconstruct the new file.
package delta

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
)

// StrongLen is the number of bytes of the strong hash kept per block.
const StrongLen = 16

const (
	MinBlockSize = 700
	MaxBlockSize = 128 << 10

	// MaxLiteral is the largest literal op Generate emits.
	MaxLiteral = 64 << 10
)

// BlockSig is the signature of one basis block.
type BlockSig struct {
	Weak   uint32
	Strong [StrongLen]byte
}

// Signature describes a basis file as a sequence of blocks. Every block is
// BlockSize bytes long except possibly the last.
type Signature struct {
	BlockSize int
	BasisSize int64
	Blocks    []BlockSig
}

// Validate checks that a signature received from a peer is consistent, so a
// hostile one cannot drive oversized allocations or out-of-range slicing.
func (s *Signature) Validate() error {
	if len(s.Blocks) == 0 {
		return nil
	}
	if s.BlockSize <= 0 || s.BlockSize > MaxBlockSize {
		return fmt.Errorf("delta: invalid block size %d", s.BlockSize)
	}
	n, bs := int64(len(s.Blocks)), int64(s.BlockSize)
	if s.BasisSize <= (n-1)*bs || s.BasisSize > n*bs {
		return fmt.Errorf("delta: basis size %d does not match %d blocks of %d", s.BasisSize, n, bs)
	}
	return nil
}

func (s *Signature) blockLen(i int) int {
	if i == len(s.Blocks)-1 {
		return int(s.BasisSize - int64(i)*int64(s.BlockSize))
	}
	return s.BlockSize
}

// BlockSizeFor picks a block size for a basis of the given size, roughly the
// square root of the size as rsync does.
func BlockSizeFor(size int64) int {
	if size <= MinBlockSize*MinBlockSize {
		return MinBlockSize
	}
	b := int(math.Sqrt(float64(size))) &^ 7
	return min(b, MaxBlockSize)
}

// weakSum computes the rsync rolling checksum components of p. Arithmetic is
// mod 2^32 here; only the low 16 bits of each half are used.
func weakSum(p []byte) (a, b uint32) {
	n := uint32(len(p))
	for i, x := range p {
		a += uint32(x)
		b += (n - uint32(i)) * uint32(x)
	}
	return a, b
}

func pack(a, b uint32) uint32 { return a&0xffff | b<<16 }

func strongSum(p []byte) (s [StrongLen]byte) {
	h := sha256.Sum256(p)
	copy(s[:], h[:StrongLen])
	return s
}

// ComputeSignature reads a basis file and returns its block signature.
func ComputeSignature(r io.Reader, blockSize int) (*Signature, error) {
	if blockSize <= 0 {
		return nil, errors.New("delta: invalid block size")
	}
	sig := &Signature{BlockSize: blockSize}
	buf := make([]byte, blockSize)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			a, b := weakSum(buf[:n])
			sig.Blocks = append(sig.Blocks, BlockSig{Weak: pack(a, b), Strong: strongSum(buf[:n])})
			sig.BasisSize += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return sig, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Op kinds.
const (
	OpLiteral uint8 = iota
	OpCopy
)

// Op is one delta instruction. A literal op carries Data; a copy op refers to
// Count consecutive basis blocks starting at Block.
type Op struct {
	Kind  uint8
	Block uint32
	Count uint32
	Data  []byte
}

// Stats reports how much of the new file was sent literally versus matched
// against the basis.
type Stats struct {
	Literal int64
	Matched int64
}

// Generate reads the new file from r and calls emit with the delta against
// sig. If sig is nil or empty the whole file is emitted as literals. The Data
// slice passed to emit is only valid for the duration of the call.
func Generate(r io.Reader, sig *Signature, emit func(Op) error) (Stats, error) {
	if sig == nil || len(sig.Blocks) == 0 {
		return literalOnly(r, emit)
	}
	if err := sig.Validate(); err != nil {
		return Stats{}, err
	}
	g := &generator{
		r:     r,
		sig:   sig,
		emit:  emit,
		bs:    sig.BlockSize,
		index: make(map[uint32][]uint32, len(sig.Blocks)),
	}
	for i, b := range sig.Blocks {
		g.index[b.Weak] = append(g.index[b.Weak], uint32(i))
	}
	g.data = make([]byte, 0, 1<<20+2*g.bs)
	err := g.run()
	return g.st, err
}

func literalOnly(r io.Reader, emit func(Op) error) (Stats, error) {
	var st Stats
	buf := make([]byte, MaxLiteral)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			st.Literal += int64(n)
			if e := emit(Op{Kind: OpLiteral, Data: buf[:n]}); e != nil {
				return st, e
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return st, nil
		}
		if err != nil {
			return st, err
		}
	}
}

type generator struct {
	r    io.Reader
	sig  *Signature
	emit func(Op) error
	bs   int

	index map[uint32][]uint32

	data     []byte // buffered input; window starts at pos
	pos, lit int    // lit is the start of not-yet-emitted literal data
	eof      bool

	copyStart, copyCount uint32

	st Stats
}

func (g *generator) flushCopy() error {
	if g.copyCount == 0 {
		return nil
	}
	op := Op{Kind: OpCopy, Block: g.copyStart, Count: g.copyCount}
	g.copyCount = 0
	return g.emit(op)
}

func (g *generator) flushLit(end int) error {
	for g.lit < end {
		if err := g.flushCopy(); err != nil {
			return err
		}
		n := min(end-g.lit, MaxLiteral)
		g.st.Literal += int64(n)
		if err := g.emit(Op{Kind: OpLiteral, Data: g.data[g.lit : g.lit+n]}); err != nil {
			return err
		}
		g.lit += n
	}
	return nil
}

func (g *generator) addCopy(idx uint32) error {
	g.st.Matched += int64(g.sig.blockLen(int(idx)))
	if g.copyCount > 0 && idx == g.copyStart+g.copyCount && g.copyCount < math.MaxUint32 {
		g.copyCount++
		return nil
	}
	if err := g.flushCopy(); err != nil {
		return err
	}
	g.copyStart, g.copyCount = idx, 1
	return nil
}

// fill emits pending literals, compacts the buffer so the window is at the
// front, and reads until the buffer is full or the input is exhausted.
func (g *generator) fill() error {
	if err := g.flushLit(g.pos); err != nil {
		return err
	}
	n := copy(g.data[:cap(g.data)], g.data[g.pos:])
	g.data = g.data[:n]
	g.pos, g.lit = 0, 0
	for len(g.data) < cap(g.data) && !g.eof {
		m, err := g.r.Read(g.data[len(g.data):cap(g.data)])
		g.data = g.data[:len(g.data)+m]
		if err == io.EOF {
			g.eof = true
		} else if err != nil {
			return err
		}
	}
	return nil
}

// find looks for a basis block matching win, preferring the block right after
// the previous match so runs of copies coalesce.
func (g *generator) find(weak uint32, win []byte, prev int64) (uint32, bool) {
	cands, ok := g.index[weak]
	if !ok {
		return 0, false
	}
	var strong [StrongLen]byte
	computed := false
	try := func(i uint32) bool {
		if g.sig.blockLen(int(i)) != len(win) {
			return false
		}
		if !computed {
			strong = strongSum(win)
			computed = true
		}
		return g.sig.Blocks[i].Strong == strong
	}
	if prev >= 0 && prev+1 < int64(len(g.sig.Blocks)) {
		next := uint32(prev + 1)
		if g.sig.Blocks[next].Weak == weak && try(next) {
			return next, true
		}
	}
	for _, i := range cands {
		if try(i) {
			return i, true
		}
	}
	return 0, false
}

func (g *generator) run() error {
	bs := g.bs
	last := len(g.sig.Blocks) - 1
	lastLen := g.sig.blockLen(last)
	var a, b uint32
	have := false
	prev := int64(-1)
	for {
		if !g.eof && len(g.data)-g.pos < bs+1 {
			if err := g.fill(); err != nil {
				return err
			}
		}
		avail := len(g.data) - g.pos
		if avail < bs {
			// Only the short final basis block can still match, and only
			// aligned to the end of the input.
			if lastLen < bs && avail >= lastLen {
				start := len(g.data) - lastLen
				win := g.data[start:]
				wa, wb := weakSum(win)
				if g.sig.Blocks[last].Weak == pack(wa, wb) && g.sig.Blocks[last].Strong == strongSum(win) {
					if err := g.flushLit(start); err != nil {
						return err
					}
					if err := g.addCopy(uint32(last)); err != nil {
						return err
					}
					g.pos = len(g.data)
					g.lit = g.pos
				}
			}
			break
		}
		if !have {
			a, b = weakSum(g.data[g.pos : g.pos+bs])
			have = true
		}
		if idx, ok := g.find(pack(a, b), g.data[g.pos:g.pos+bs], prev); ok {
			if err := g.flushLit(g.pos); err != nil {
				return err
			}
			if err := g.addCopy(idx); err != nil {
				return err
			}
			g.pos += bs
			g.lit = g.pos
			have = false
			prev = int64(idx)
			continue
		}
		prev = -1
		if avail == bs {
			// At EOF with no byte to roll in; fall through to the tail check.
			g.pos++
			have = false
			continue
		}
		out, in := uint32(g.data[g.pos]), uint32(g.data[g.pos+bs])
		a = a - out + in
		b = b - uint32(bs)*out + a
		g.pos++
		if g.pos-g.lit >= MaxLiteral {
			if err := g.flushLit(g.pos); err != nil {
				return err
			}
		}
	}
	if err := g.flushLit(len(g.data)); err != nil {
		return err
	}
	return g.flushCopy()
}

// Applier reconstructs a file from delta ops and a basis.
type Applier struct {
	basis io.ReaderAt
	sig   *Signature
	w     io.Writer
	buf   []byte
}

// NewApplier returns an Applier writing to w. basis and sig may be nil when
// the delta contains only literals.
func NewApplier(basis io.ReaderAt, sig *Signature, w io.Writer) *Applier {
	return &Applier{basis: basis, sig: sig, w: w}
}

// Apply executes one op.
func (a *Applier) Apply(op Op) error {
	switch op.Kind {
	case OpLiteral:
		_, err := a.w.Write(op.Data)
		return err
	case OpCopy:
		if a.basis == nil || a.sig == nil {
			return errors.New("delta: copy op without a basis")
		}
		end := uint64(op.Block) + uint64(op.Count)
		if op.Count == 0 || end > uint64(len(a.sig.Blocks)) {
			return fmt.Errorf("delta: copy of blocks %d+%d out of range (%d blocks)", op.Block, op.Count, len(a.sig.Blocks))
		}
		if a.buf == nil {
			a.buf = make([]byte, 256<<10)
		}
		off := int64(op.Block) * int64(a.sig.BlockSize)
		stop := min(int64(end)*int64(a.sig.BlockSize), a.sig.BasisSize)
		for off < stop {
			n := int(min(int64(len(a.buf)), stop-off))
			m, err := a.basis.ReadAt(a.buf[:n], off)
			if m < n {
				if err == nil || err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return fmt.Errorf("delta: reading basis: %w", err)
			}
			if _, err := a.w.Write(a.buf[:n]); err != nil {
				return err
			}
			off += int64(n)
		}
		return nil
	default:
		return fmt.Errorf("delta: unknown op kind %d", op.Kind)
	}
}
