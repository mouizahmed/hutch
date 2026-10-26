//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mouizahmed/hutch/internal/agent"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/worker"
)

const usage = `usage: hutchd [--root DIR] [--firecracker VERSION] <command>

commands:
  box boot <id> [--image NAME] [--cpus N] [--mem MIB]   cold boot a new box from an image
  box fork <src> <id>                                    new box from a sleeping box's snapshot
  box sleep | wake | archive | rm <id>
  box ls
  box exec <id> [--user NAME] -- <command> [args...]
  box console <id>                                       print the serial console log
  box status <id>                                        print what hutch-guest reports (threads, sessions, ports)
  image import <name> <vmlinux> <rootfs.ext4>
  enroll --server HOST:PORT --token TOKEN [--name NAME]   join a server
  run [--name NAME]                                      serve the server's calls
  boot                                                   start an elastic worker (data disk, enrollment from user data, run)
  version
`

func main() {
	root := flag.String("root", "/var/lib/hutch", "worker data directory (XFS with reflink)")
	fcVersion := flag.String("firecracker", "v1.17.0", "Firecracker version under <root>/firecracker/")
	flag.Usage = func() { fmt.Fprint(flag.CommandLine.Output(), usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if args[0] == "version" {
		fmt.Println("hutchd", version.String())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dispatch(ctx, *root, *fcVersion, args); err != nil {
		fmt.Fprintln(os.Stderr, "hutchd:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, root, fcVersion string, args []string) error {
	if len(args) >= 2 && args[0] == "image" && args[1] == "import" {
		if len(args) != 5 {
			return fmt.Errorf("usage: hutchd image import <name> <vmlinux> <rootfs.ext4>")
		}
		return importImage(root, args[2], args[3], args[4])
	}
	if args[0] == "boot" {
		return boot(ctx, root, fcVersion)
	}
	if args[0] == "enroll" || args[0] == "run" {
		return daemon(ctx, root, fcVersion, args)
	}
	if len(args) < 2 || args[0] != "box" {
		flag.Usage()
		os.Exit(2)
	}
	w, err := worker.New(root, fcVersion)
	if err != nil {
		return err
	}
	cmd, rest := args[1], args[2:]
	start := time.Now()
	done := func(b *worker.Box, err error) error {
		if err != nil {
			return err
		}
		fmt.Printf("%s %s in %s", b.ID, b.State, time.Since(start).Round(time.Millisecond))
		if ip := b.ReachIP(); ip != "" {
			fmt.Printf(" (reach %s)", ip)
		}
		fmt.Println()
		return nil
	}

	switch cmd {
	case "boot":
		fs := flag.NewFlagSet("boot", flag.ExitOnError)
		image := fs.String("image", "base", "image name")
		cpus := fs.Int("cpus", 2, "vCPUs")
		mem := fs.Int("mem", 2048, "memory in MiB")
		id, err := oneArg(fs, rest, "box boot <id>")
		if err != nil {
			return err
		}
		return done(w.Boot(ctx, id, *image, *cpus, *mem, 0, worker.Idle{}, nil, nil))
	case "fork":
		if len(rest) != 2 {
			return fmt.Errorf("usage: hutchd box fork <src> <id>")
		}
		return done(w.Fork(ctx, rest[0], rest[1], worker.Idle{}, nil, nil))
	case "sleep":
		id, err := oneArg(flag.NewFlagSet("sleep", flag.ExitOnError), rest, "box sleep <id>")
		if err != nil {
			return err
		}
		return done(w.Sleep(ctx, id))
	case "wake":
		id, err := oneArg(flag.NewFlagSet("wake", flag.ExitOnError), rest, "box wake <id>")
		if err != nil {
			return err
		}
		return done(w.Wake(ctx, id))
	case "archive":
		id, err := oneArg(flag.NewFlagSet("archive", flag.ExitOnError), rest, "box archive <id>")
		if err != nil {
			return err
		}
		return done(w.Archive(id))
	case "rm":
		id, err := oneArg(flag.NewFlagSet("rm", flag.ExitOnError), rest, "box rm <id>")
		if err != nil {
			return err
		}
		return w.Remove(id)
	case "ls":
		boxes, err := w.List()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "BOX\tSTATE\tSIZE\tREACH\tGEN\tCLASS")
		for _, b := range boxes {
			state := string(b.State)
			if b.State == worker.Running && !w.Alive(b) {
				state = "crashed"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d vCPU, %d MiB\t%s\t%d\t%s\n", b.ID, state, b.VCPUs, b.MemMiB, b.ReachIP(), b.Generation, b.Class)
		}
		return tw.Flush()
	case "exec":
		fs := flag.NewFlagSet("exec", flag.ExitOnError)
		user := fs.String("user", "dev", "user to run as")
		if len(rest) < 2 {
			return fmt.Errorf("usage: hutchd box exec <id> [--user NAME] -- <command> [args...]")
		}
		fs.Parse(rest[1:])
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: hutchd box exec <id> [--user NAME] -- <command> [args...]")
		}
		code, err := w.Exec(ctx, rest[0], guestproto.Exec{Argv: fs.Args(), User: *user}, os.Stdout, os.Stderr)
		if err != nil {
			return err
		}
		os.Exit(code)
	case "status":
		id, err := oneArg(flag.NewFlagSet("status", flag.ExitOnError), rest, "box status <id>")
		if err != nil {
			return err
		}
		st, err := w.GuestStatus(ctx, id)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	case "console":
		id, err := oneArg(flag.NewFlagSet("console", flag.ExitOnError), rest, "box console <id>")
		if err != nil {
			return err
		}
		f, err := os.Open(w.ConsolePath(id))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	default:
		flag.Usage()
		os.Exit(2)
	}
	return nil
}

func oneArg(fs *flag.FlagSet, args []string, use string) (string, error) {
	// Accept flags after the positional argument too: `box boot web --mem 4096`.
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		args = append(args[1:], args[0])
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		return "", fmt.Errorf("usage: hutchd %s", use)
	}
	return fs.Arg(0), nil
}

func importImage(root, name, kernel, rootfs string) error {
	dir := filepath.Join(root, "images", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for src, dst := range map[string]string{kernel: "vmlinux", rootfs: "rootfs.ext4"} {
		if err := copyFile(src, filepath.Join(dir, dst)); err != nil {
			return err
		}
	}
	fmt.Println("imported", dir)
	return nil
}

func copyFile(src, dst string) error {
	out, err := exec.Command("cp", "--sparse=always", src, dst+".tmp").CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy %s: %v: %s", src, err, out)
	}
	if err := os.Chmod(dst+".tmp", 0o644); err != nil {
		return err
	}
	return os.Rename(dst+".tmp", dst)
}

func daemon(ctx context.Context, root, fcVersion string, args []string) error {
	id := agent.Identity{Dir: filepath.Join(root, "identity")}
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	host, _ := os.Hostname()
	name := fs.String("name", host, "worker name shown by the server")
	serverAddr := fs.String("server", "", "server address, host:port")
	token := fs.String("token", "", "join token from `hutch server token`")
	fs.Parse(args[1:])
	if args[0] == "enroll" {
		if *serverAddr == "" || *token == "" {
			return fmt.Errorf("usage: hutchd enroll --server HOST:PORT --token TOKEN [--name NAME]")
		}
		wid, err := agent.Enroll(ctx, id, *serverAddr, *token, *name)
		if err != nil {
			return err
		}
		fmt.Printf("enrolled as %s\n", wid)
		return nil
	}
	w, err := worker.New(root, fcVersion)
	if err != nil {
		return err
	}
	return agent.Run(ctx, id, w, *name)
}
