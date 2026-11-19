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
		br := bytes.NewReader(pack[off:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, errPack
		}
		data, err := io.ReadAll(io.LimitReader(zr, int64(size)+1))
		zr.Close()
		if err != nil {
			return nil, errPack
		}
		if len(data) != size {
			return nil, errPack
		}
		// bytes consumed = what the zlib reader pulled from br.
		off += len(pack[off:]) - br.Len()
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
