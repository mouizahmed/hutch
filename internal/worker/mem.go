//go:build linux

package worker

import (
	"bytes"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

const pageSize = 4096

// applyDiff writes the pages of a diff snapshot's memory file into mem. The diff's
// data regions are the pages the VM wrote since its last snapshot; zero pages among
// them become holes in mem, so mem stays sparse without rescanning all of it. When
// mem is the diff itself (the first snapshot after a cold boot), only holes are punched.
func applyDiff(mem, diff string) error {
	d, err := os.Open(diff)
	if err != nil {
		return err
	}
	defer d.Close()
	m, err := os.OpenFile(mem, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer m.Close()
	inPlace := mem == diff
	dfd, mfd := int(d.Fd()), int(m.Fd())

	zero := make([]byte, pageSize)
	buf := make([]byte, 1<<20)
	// Runs of data or zero pages are flushed as one write or one hole punch.
	var runStart int64
	var runZero bool
	var run []byte
	flush := func(end int64) error {
		if end == runStart {
			return nil
		}
		var err error
		if runZero {
			err = unix.Fallocate(mfd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, runStart, end-runStart)
		} else if !inPlace {
			_, err = m.WriteAt(run, runStart)
		}
		run = run[:0]
		runStart = end
		return err
	}

	var off int64
	for {
		start, err := unix.Seek(dfd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil
		}
		if err != nil {
			return err
		}
		end, err := unix.Seek(dfd, start, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		runStart, run = start, run[:0]
		for pos := start; pos < end; {
			n, err := d.ReadAt(buf[:min(int64(len(buf)), end-pos)], pos)
			if err != nil {
				return err
			}
			for i := 0; i < n; i += pageSize {
				page := buf[i:min(i+pageSize, n)]
				isZero := bytes.Equal(page, zero[:len(page)])
				at := pos + int64(i)
				if at > runStart && isZero != runZero {
					if err := flush(at); err != nil {
						return err
					}
				}
				runZero = isZero
				if !isZero && !inPlace {
					run = append(run, page...)
				}
			}
			pos += int64(n)
			if !runZero && len(run) >= 8<<20 {
				if err := flush(pos); err != nil {
					return err
				}
			}
		}
		if err := flush(end); err != nil {
			return err
		}
		off = end
	}
}
