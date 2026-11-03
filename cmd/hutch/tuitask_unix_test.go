//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Cancel stops a task's command and what it started, and says the machine may
// already have done some of it.
func TestCancelStopsTheGroup(t *testing.T) {
	m := testTUI(t)
	c := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	ownGroup(c)
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	child, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatal(err)
	}
	tk := &task{ID: "1", Title: "slow", State: taskRunning, cmd: c, Started: time.Now()}
	m.tasks = []*task{tk}
	m.cancelTask(tk)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the command kept running after cancel")
	}
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(child, 0) == nil {
		syscall.Kill(child, syscall.SIGKILL)
		t.Fatal("what the command started kept running after cancel")
	}
	m.taskDone(taskDoneMsg{tk.ID, errors.New("signal: terminated")})
	if tk.State != taskCanceled || !strings.Contains(tk.Err, "may have done") || len(m.tasks) != 1 {
		t.Fatalf("state %s, %q, %d tasks", tk.State, tk.Err, len(m.tasks))
	}
}

// Quitting stops a task that ignores the gentle signal, and two of them don't
// wait twice.
func TestQuitStopsStubbornTasks(t *testing.T) {
	m := testTUI(t)
	m.cancelGrace = 300 * time.Millisecond
	for i := range 2 {
		c := exec.Command("sh", "-c", `trap "" TERM; echo ready; while :; do sleep 1; done`)
		ownGroup(c)
		out, err := c.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		out.Read(make([]byte, 8))
		tk := &task{ID: strconv.Itoa(i), Title: "stubborn", State: taskRunning, cmd: c, done: make(chan struct{}), Started: time.Now()}
		go func() { c.Wait(); close(tk.done) }()
		m.tasks = append(m.tasks, tk)
	}
	start := time.Now()
	m.stopTasks()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("quitting took %v", d)
	}
	for _, tk := range m.tasks {
		select {
		case <-tk.done:
		default:
			t.Fatal("a task outlived Hutch")
		}
		if tk.State != taskCanceled {
			t.Fatalf("state %s", tk.State)
		}
	}
}
