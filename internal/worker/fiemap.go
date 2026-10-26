//go:build linux

package worker

import (
	"os"
	"slices"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/durable"
)

const (
	fsIocFiemap      = 0xC020660B
	fiemapFlagSync   = 0x1
	extentLast       = 0x1
	extentUnknown    = 0x2
	extentDelalloc   = 0x4
	extentUnwritten  = 0x800
	fiemapBatch      = 1024
	fiemapHeaderSize = 32
	fiemapExtentSize = 56
)

type extent struct {
	logical, physical, length uint64
	flags                     uint32
}

// extents returns a file's mapping from logical offsets to disk blocks.
func extents(f *os.File) ([]extent, error) {
	buf := make([]byte, fiemapHeaderSize+fiemapBatch*fiemapExtentSize)
	var out []extent
	var start uint64
	for {
		clear(buf)
		*(*uint64)(unsafe.Pointer(&buf[0])) = start
		*(*uint64)(unsafe.Pointer(&buf[8])) = ^uint64(0) - start
		*(*uint32)(unsafe.Pointer(&buf[16])) = fiemapFlagSync
		*(*uint32)(unsafe.Pointer(&buf[24])) = fiemapBatch
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
			return nil, errno
		}
		n := *(*uint32)(unsafe.Pointer(&buf[20]))
		if n == 0 {
			return out, nil
		}
		for i := range int(n) {
			e := buf[fiemapHeaderSize+i*fiemapExtentSize:]
			x := extent{
				logical:  *(*uint64)(unsafe.Pointer(&e[0])),
				physical: *(*uint64)(unsafe.Pointer(&e[8])),
				length:   *(*uint64)(unsafe.Pointer(&e[16])),
				flags:    *(*uint32)(unsafe.Pointer(&e[40])),
			}
			out = append(out, x)
			start = x.logical + x.length
			if x.flags&extentLast != 0 {
				return out, nil
			}
		}
	}
}

// piece is the part of an extent inside one chunk.
type piece struct {
	logical, physical, length uint64
	unwritten                 bool
}

// changes compares a file with a reference copy made by reflink: a chunk whose
// blocks are still the very same blocks on disk can't have changed, since writing
// either file would have copied them. Holes and preallocated ranges read as zeros.
// Without a reference, every chunk with data is Changed.
func changes(cur, ref *os.File) (func(int) durable.Change, error) {
	ce, err := extents(cur)
	if err != nil {
		return nil, err
	}
	var re []extent
	if ref != nil {
		if re, err = extents(ref); err != nil {
			return nil, err
		}
	}
	fi, err := cur.Stat()
	if err != nil {
		return nil, err
	}
	n := int((fi.Size() + durable.ChunkSize - 1) / durable.ChunkSize)
	out := make([]durable.Change, n)
	for i := range n {
		from, to := uint64(i)*durable.ChunkSize, uint64(i+1)*durable.ChunkSize
		cp, cu := chunkPieces(&ce, from, to)
		if !cu && allUnwritten(cp) {
			out[i] = durable.Hole
			continue
		}
		if ref == nil {
			continue
		}
		rp, ru := chunkPieces(&re, from, to)
		if !cu && !ru && slices.Equal(cp, rp) {
			out[i] = durable.Same
		}
	}
	return func(i int) durable.Change { return out[i] }, nil
}

// chunkPieces clips extents to one chunk, merging pieces contiguous on disk so the
// same blocks give the same pieces however each file's extents are split. Extents
// that end inside the chunk are consumed; one running into the next is kept.
func chunkPieces(exts *[]extent, from, to uint64) ([]piece, bool) {
	var out []piece
	unknown := false
	skip := 0
	for i, e := range *exts {
		if e.logical >= to {
			break
		}
		end := e.logical + e.length
		if end <= from {
			skip = i + 1
			continue
		}
		if e.flags&(extentUnknown|extentDelalloc) != 0 {
			unknown = true
		}
		lo, hi := max(e.logical, from), min(end, to)
		p := piece{lo, e.physical + (lo - e.logical), hi - lo, e.flags&extentUnwritten != 0}
		if n := len(out); n > 0 && out[n-1].logical+out[n-1].length == p.logical &&
			out[n-1].physical+out[n-1].length == p.physical && out[n-1].unwritten == p.unwritten {
			out[n-1].length += p.length
		} else {
			out = append(out, p)
		}
		if end <= to {
			skip = i + 1
		}
	}
	*exts = (*exts)[skip:]
	return out, unknown
}

func allUnwritten(ps []piece) bool {
	for _, p := range ps {
		if !p.unwritten {
			return false
		}
	}
	return true
}
