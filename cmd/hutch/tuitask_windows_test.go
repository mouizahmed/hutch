//go:build windows

package main

import (
	"os/exec"
	"testing"
	"time"
)

// Cancel stops a task's command and what it started.
func TestCancelStopsTheJob(t *testing.T) {
	m := testTUI(t)
	c := exec.Command("cmd", "/c", "start /b ping -n 60 127.0.0.1 >nul & ping -n 60 127.0.0.1 >nul")
	g, err := startGroup(c)
	if err != nil {
		t.Fatal(err)
	}
	if g.job == 0 {
		t.Fatal("the command joined no job")
	}
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	for end := time.Now().Add(5 * time.Second); g.count() < 3; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%d processes in the job, want cmd and two pings", g.count())
		}
	}
	tk := &task{ID: "1", Title: "slow", State: taskRunning, group: g, done: done, Started: time.Now()}
	m.tasks = []*task{tk}
	m.cancelTask(tk)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the command kept running after cancel")
	}
	for end := time.Now().Add(5 * time.Second); g.alive(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("what the command started kept running after cancel")
		}
	}
}
