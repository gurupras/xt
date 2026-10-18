package delta

import (
	"bytes"
	"math/rand"
	"testing"
)

func roundTrip(t *testing.T, basis, target []byte, bs int) Stats {
	t.Helper()
	sig, err := ComputeSignature(bytes.NewReader(basis), bs)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ap := NewApplier(bytes.NewReader(basis), sig, &out)
	st, err := Generate(bytes.NewReader(target), sig, func(op Op) error {
		op.Data = bytes.Clone(op.Data)
		return ap.Apply(op)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), target) {
		t.Fatalf("reconstruction mismatch: got %d bytes, want %d", out.Len(), len(target))
	}
	if st.Literal+st.Matched != int64(len(target)) {
		t.Fatalf("stats literal %d + matched %d != %d", st.Literal, st.Matched, len(target))
	}
	return st
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestIdentical(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	data := randBytes(r, 3<<20+123)
	st := roundTrip(t, data, data, 2048)
	if st.Literal != 0 {
		t.Fatalf("identical file sent %d literal bytes", st.Literal)
	}
}

func TestEdits(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	basis := randBytes(r, 2<<20+77)
	target := bytes.Clone(basis)
	// Insert, delete and overwrite at a few places.
	target = append(target[:1000], append(randBytes(r, 333), target[1000:]...)...)
	target = append(target[:500000], target[510000:]...)
	copy(target[900000:], randBytes(r, 4096))
	target = append(target, randBytes(r, 999)...)
	st := roundTrip(t, basis, target, 1024)
	if st.Literal > 20000 {
		t.Fatalf("too much literal data: %d", st.Literal)
	}
}

func TestShortLastBlockMatches(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	basis := randBytes(r, 10*1000+17)
	target := append(randBytes(r, 50), basis...)
	st := roundTrip(t, basis, target, 1000)
	if st.Literal != 50 {
		t.Fatalf("literal = %d, want 50", st.Literal)
	}
}

func TestEdgeSizes(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	sizes := []int{0, 1, 699, 700, 701, 1399, 1400, 1401, 5000}
	for _, bn := range sizes {
		for _, tn := range sizes {
			basis := randBytes(r, bn)
			target := randBytes(r, tn)
			if tn > 0 && bn > 0 {
				copy(target, basis[:min(bn, tn)/2])
			}
			roundTrip(t, basis, target, 700)
			roundTrip(t, basis, basis, 700)
		}
	}
}

func TestRepetitive(t *testing.T) {
	basis := bytes.Repeat([]byte("abcdefgh"), 100000)
	target := bytes.Repeat([]byte("abcdefgh"), 100001)
	roundTrip(t, basis, target, 800)
	zeros := make([]byte, 1<<20)
	roundTrip(t, zeros, append([]byte{1}, zeros...), 4096)
}

func TestNoSignature(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	target := randBytes(r, 200000)
	var out bytes.Buffer
	ap := NewApplier(nil, nil, &out)
	st, err := Generate(bytes.NewReader(target), nil, ap.Apply)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), target) || st.Literal != int64(len(target)) {
		t.Fatal("literal-only transfer mismatch")
	}
}

func TestApplyRejectsBadCopy(t *testing.T) {
	sig := &Signature{BlockSize: 10, BasisSize: 25, Blocks: make([]BlockSig, 3)}
	ap := NewApplier(bytes.NewReader(make([]byte, 25)), sig, &bytes.Buffer{})
	if err := ap.Apply(Op{Kind: OpCopy, Block: 2, Count: 2}); err == nil {
		t.Fatal("expected out of range error")
	}
}
