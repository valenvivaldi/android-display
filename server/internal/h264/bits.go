package h264

import "errors"

var errEOF = errors.New("h264: read past the end of the data")

// bitReader reads big-endian bits and Exp-Golomb codes. The first error sticks in err and
// later reads return 0, so callers can read a run of fields and check err once.
type bitReader struct {
	data []byte
	pos  int // in bits
	err  error
}

func (r *bitReader) bit() uint32 {
	if r.err != nil {
		return 0
	}
	if r.pos >= len(r.data)*8 {
		r.err = errEOF
		return 0
	}
	b := (r.data[r.pos/8] >> (7 - r.pos%8)) & 1
	r.pos++
	return uint32(b)
}

func (r *bitReader) u(n int) (uint32, error) {
	var v uint32
	for range n {
		v = v<<1 | r.bit()
	}
	return v, r.err
}

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() (uint32, error) {
	zeros := 0
	for r.bit() == 0 {
		if r.err != nil {
			return 0, r.err
		}
		zeros++
		if zeros > 31 {
			r.err = errEOF
			return 0, r.err
		}
	}
	suffix, _ := r.u(zeros)
	return 1<<zeros - 1 + suffix, r.err
}

// se reads a signed Exp-Golomb code.
func (r *bitReader) se() (int32, error) {
	k, err := r.ue()
	if k%2 == 1 {
		return int32((k + 1) / 2), err
	}
	return -int32(k / 2), err
}

// bitWriter builds a big-endian bit string.
type bitWriter struct {
	buf   []byte
	nbits int
}

func (w *bitWriter) flag(v bool) {
	if w.nbits%8 == 0 {
		w.buf = append(w.buf, 0)
	}
	if v {
		w.buf[w.nbits/8] |= 1 << (7 - w.nbits%8)
	}
	w.nbits++
}

// ue writes an unsigned Exp-Golomb code.
func (w *bitWriter) ue(v uint32) {
	code := v + 1
	length := 0
	for t := code; t > 0; t >>= 1 {
		length++
	}
	for range length - 1 {
		w.flag(false)
	}
	for i := length - 1; i >= 0; i-- {
		w.flag(code>>uint(i)&1 == 1)
	}
}

// copyBits appends the first n bits of src.
func (w *bitWriter) copyBits(src []byte, n int) {
	for i := range n {
		w.flag(src[i/8]>>(7-i%8)&1 == 1)
	}
}

// alignWithZeros pads with zero bits to a byte boundary.
func (w *bitWriter) alignWithZeros() {
	for w.nbits%8 != 0 {
		w.flag(false)
	}
}

func (w *bitWriter) bytes() []byte { return w.buf }
