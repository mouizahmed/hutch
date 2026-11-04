//go:build linux

package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mouizahmed/hutch/internal/worker"
)

func reset(root, socket string, args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "permanently delete local boxes and setup state")
	check := fs.Bool("check", false, "check whether the machine can be reset, without changing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || (!*yes && !*check) {
		return fmt.Errorf("reset permanently deletes local boxes and setup state: use reset --yes after stopping hutchd")
	}
	if err := checkReset(root); err != nil {
		return err
	}
	if *check {
		return nil
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("hutchd is running: stop it with sudo systemctl stop hutchd before resetting")
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("check whether hutchd is stopped: %w", err)
	}
	// Use the installed runtime named in each box's metadata, without requiring
	// the currently preferred runtime or reconfiguring the host network.
	w := &worker.Worker{Root: root}
	entries, err := os.ReadDir(filepath.Join(root, "boxes"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := w.Remove(e.Name()); err != nil {
				return fmt.Errorf("remove box %s: %w", e.Name(), err)
			}
		}
	}
	for _, name := range []string{"boxes", "state", "link", "worker-id", "fenced", "no-boxes", "cache", "jail"} {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// A lead's enrollment database must outlive the workers that still trust it.
func checkReset(root string) error {
	name := filepath.Join(root, "state", "hutch.db")
	if _, err := os.Stat(name); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: name, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM workers WHERE linked = 1 AND gone = 0 ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) > 0 {
		return fmt.Errorf("this lead still has workers (%s): hand over the lead or remove them with hutch machine rm before resetting", strings.Join(names, ", "))
	}
	return nil
}
