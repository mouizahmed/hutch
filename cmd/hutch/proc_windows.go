//go:build windows

package main

import (
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGroup is a started command and what it starts: a job object. The job's
// handle stays open while Hutch runs, so a late signal never reaches another.
type procGroup struct {
	job windows.Handle // 0 when the command couldn't join one
	p   *os.Process
}

func startGroup(c *exec.Cmd) (*procGroup, error) {
	if err := c.Start(); err != nil {
		return nil, err
	}
	g := &procGroup{p: c.Process}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return g, nil
	}
	// It joins as it starts, before a hutch command starts anything.
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(c.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	if err != nil {
		windows.CloseHandle(job)
		return g, nil
	}
	g.job = job
	return g, nil
}

// signal stops the group: Windows has no gentler signal for another process.
func (g *procGroup) signal(bool) {
	if g.job != 0 {
		windows.TerminateJobObject(g.job, 1)
	}
	g.p.Kill()
}

// alive reports whether anything in the group still runs.
func (g *procGroup) alive() bool { return g.count() > 0 }

// count is how many processes run in the job.
func (g *procGroup) count() int {
	if g.job == 0 {
		return 0
	}
	var info struct {
		times                                      [4]int64
		pageFaults, total, active, totalTerminated uint32
	}
	if windows.QueryInformationJobObject(g.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil) != nil {
		return 0
	}
	return int(info.active)
}
