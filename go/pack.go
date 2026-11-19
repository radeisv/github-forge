// Pack v2 read/write (no deltas: the suite's client never sends deltas,
// and a plain pack satisfies every check).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

var errPack = errors.New("bad packfile")

// zlibWriterPool reuses compressors across pack writes; a writer is
// Reset to a fresh buffer for each object, so pooled state never leaks
// from one pack into another.
var zlibWriterPool = sync.Pool{
	New: func() any { return zlib.NewWriter(io.Discard) },
}

// zlibReaderPool reuses decompressors across pack reads. A pooled
// reader is Reset to a fresh sliceReader for each object; the pool
// only ever holds readers whose last stream ended cleanly.
var zlibReaderPool = sync.Pool{
	New: func() any {
		zr, err := zlib.NewReader(bytes.NewReader(emptyZlibStream))
		if err != nil {
			return nil
		}
		return zr
	},
}

// emptyZlibStream is a valid zlib stream for zero bytes, used only to
// seed a pooled decompressor (zlib.NewReader refuses an empty input).
var emptyZlibStream = func() []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Close()
	return buf.Bytes()
}()

// sliceReader is a zero-allocation io.Reader over a byte slice that
// reports how many bytes were consumed, so a pooled zlib reader can
// be Reset onto it and the pack parser knows where the next object
// starts without allocating a bytes.Reader per object.
// It implements io.ByteReader: without ReadByte, the flate
// decompressor buffers ahead and silently swallows bytes that belong
// to the next object.
type sliceReader struct {
	b   []byte
	off int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

func (r *sliceReader) ReadByte() (byte, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	c := r.b[r.off]
	r.off++
	return c, nil
}

// readPack parses a packfile, verifies its trailer checksum and every
// object's hash, and returns the objects in pack order.
func readPack(pack []byte) ([]*obj, error) {
	if len(pack) < 32 {
		return nil, errPack
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errPack
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return nil, errPack
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	trailer := pack[len(pack)-20:]
	h := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(h[:], trailer) {
		return nil, errPack
	}
	off := 12
	objects := make([]*obj, 0, count)
	for i := uint32(0); i < count; i++ {
		if off >= len(pack)-20 {
			return nil, errPack
		}
		typ, size, n, ok := readPackVarint(pack, off)
		if !ok {
			return nil, errPack
		}
		off += n
		var typName string
		switch typ {
		case 1:
			typName = "commit"
		case 2:
			typName = "tree"
		case 3:
			typName = "blob"
		case 4:
			typName = "tag"
		default:
			// OFS_DELTA (6) / REF_DELTA (7) are never sent by the client.
			return nil, errPack
		}
		// One pre-sized buffer for the object: the old code grew one
		// via io.ReadAll over a LimitReader and allocated a fresh
		// zlib reader per object.
		data := make([]byte, size)
		sr := &sliceReader{b: pack[off:]}
		zr, _ := zlibReaderPool.Get().(io.ReadCloser)
		if zr == nil {
			var err error
			zr, err = zlib.NewReader(sr)
			if err != nil {
				return nil, errPack
			}
		} else if err := zr.(interface{ Reset(io.Reader, []byte) error }).Reset(sr, nil); err != nil {
			// a reader whose Reset fails is dropped, never re-pooled
			return nil, errPack
		}
		if _, err := io.ReadFull(zr, data); err != nil {
			zr.Close()
			zlibReaderPool.Put(zr)
			return nil, errPack
		}
		// the stream must end exactly at size: one extra byte means
		// the declared size lied
		var one [1]byte
		if n, _ := zr.Read(one[:]); n != 0 {
			zr.Close()
			zlibReaderPool.Put(zr)
			return nil, errPack
		}
		zr.Close()
		zlibReaderPool.Put(zr)
		// bytes consumed = what the zlib reader pulled from sr.
		off += sr.off
		objects = append(objects, &obj{typ: typName, data: data})
	}
	if off != len(pack)-20 {
		return nil, errPack
	}
	return objects, nil
}

// readPackVarint reads the type/size varint at off. Returns type code,
// size, bytes consumed.
func readPackVarint(pack []byte, off int) (int, int, int, bool) {
	if off >= len(pack) {
		return 0, 0, 0, false
	}
	b := pack[off]
	typ := int((b >> 4) & 0x7)
	size := int(b & 0x0F)
	shift := 4
	n := 1
	for b&0x80 != 0 {
		if off+n >= len(pack) {
			return 0, 0, 0, false
		}
		b = pack[off+n]
		size |= int(b&0x7F) << shift
		shift += 7
		n++
	}
	return typ, size, n, true
}

// writePack serializes objects as a plain (non-delta) pack v2 with a SHA-1
// trailer. Objects are written in the given order.
func writePack(objects []*obj) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objects)))
	buf.Write(hdr[:])
	for _, o := range objects {
		var code int
		switch o.typ {
		case "commit":
			code = 1
		case "tree":
			code = 2
		case "blob":
			code = 3
		case "tag":
			code = 4
		}
		writePackVarint(&buf, code, len(o.data))
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&buf)
		zw.Write(o.data)
		zw.Close()
		zlibWriterPool.Put(zw)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writePackVarint(buf *bytes.Buffer, typ, size int) {
	// first byte: low 4 bits of size, type in bits 4-6, continuation bit
	b0 := byte((typ << 4) | (size & 0x0F))
	size >>= 4
	if size == 0 {
		buf.WriteByte(b0)
		return
	}
	buf.WriteByte(b0 | 0x80)
	for {
		b := byte(size & 0x7F)
		size >>= 7
		if size == 0 {
			buf.WriteByte(b)
			return
		}
		buf.WriteByte(b | 0x80)
	}
}
