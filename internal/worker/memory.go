//go:build linux

package worker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mouizahmed/hutch/internal/firecracker"
)

// MemUse returns the host memory a running box's VM holds, in MiB: its
// proportional share, so pages it shares with its template or other forks count
// once across them.
func (w *Worker) MemUse(b *Box) (int, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/smaps_rollup", b.PID))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Pss:"); ok {
			kb, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
			return kb / 1024, err
		}
	}
	return 0, fmt.Errorf("no Pss for box %s", b.ID)
}

// HostMemory returns the worker's memory and how much of it is available, in MiB.
func HostMemory() (total, available int) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.Atoi(fields[1])
		switch fields[0] {
		case "MemTotal:":
			total = kb / 1024
		case "MemAvailable:":
			available = kb / 1024
		}
	}
	return total, available
}

// Reclaim inflates a running box's balloon over the memory its guest doesn't need,
// keeping headroom free, and returns the MiB taken. Release gives it all back.
func (w *Worker) Reclaim(ctx context.Context, b *Box, headroomMiB int) (int, error) {
	c := firecracker.NewClient(w.jailAPIPath(b.ID))
	st, err := c.BalloonStats(ctx)
	if err != nil {
		return 0, err
	}
	take := st.ActualMiB + int(st.Available>>20) - headroomMiB
	if take <= st.ActualMiB {
		return 0, nil
	}
	return take - st.ActualMiB, c.SetBalloon(ctx, take)
}

func (w *Worker) Release(ctx context.Context, b *Box) error {
	return firecracker.NewClient(w.jailAPIPath(b.ID)).SetBalloon(ctx, 0)
}
