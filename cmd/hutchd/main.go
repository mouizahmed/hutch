//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mouizahmed/hutch/internal/agent"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/image"
	"github.com/mouizahmed/hutch/internal/server"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/worker"
)

const usage = `usage: hutchd [--root DIR] [--firecracker VERSION] [--socket PATH] <command>

commands:
  box boot <id> [--image NAME] [--cpus N] [--mem MIB]   cold boot a new box from an image
  box fork <src> <id>                                    new box from a sleeping box's snapshot
  box sleep | wake | archive | rm <id>
  box ls
  box exec <id> [--user NAME] -- <command> [args...]
  box console <id>                                       print the serial console log
  box status <id>                                        print what hutch-guest reports (threads, sessions, ports)
  image build <name> [<context-dir>]                     build an image from the Dockerfile in a directory (none: the default image)
  run [--name NAME]                                      run the machine: boxes, their state, and the API
  api                                                    bridge stdio to the API (what ssh <machine> hutchd api runs)
  restore                                                recover the state from a bucket: {"bucket": ..., "passphrase": ...} on stdin
  version
`

func main() {
	root := flag.String("root", "/var/lib/hutch", "data directory (XFS with reflink)")
	fcVersion := flag.String("firecracker", "v1.17.0", "Firecracker version under <root>/firecracker/")
	socket := flag.String("socket", client.Socket, "the API's Unix socket")
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
	if err := dispatch(ctx, *root, *fcVersion, *socket, args); err != nil {
		fmt.Fprintln(os.Stderr, "hutchd:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, root, fcVersion, socket string, args []string) error {
	switch args[0] {
	case "api":
		return client.Bridge(socket)
	case "run":
		return run(ctx, root, fcVersion, socket, args)
	case "restore":
		var p api.ServerInitParams
		if err := json.NewDecoder(os.Stdin).Decode(&p); err != nil {
			return fmt.Errorf("restore: read the bucket and passphrase: %w", err)
		}
		at, err := server.Restore(ctx, filepath.Join(root, "state"), p.Bucket, p.Passphrase)
		if err != nil {
			return err
		}
		fmt.Printf("restored the backup from %s\n", at.Local().Format(time.DateTime))
		return nil
	}
	if len(args) >= 2 && args[0] == "image" && args[1] == "build" {
		return buildImage(ctx, root, args[2:])
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

func buildImage(ctx context.Context, root string, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: hutchd image build <name> [<context-dir>]")
	}
	b := &image.Builder{Root: root, CacheBytes: 20 << 30}
	var info image.Info
	var err error
	if len(args) == 1 {
		info, err = b.BuildDefault(ctx, os.Stdout)
	} else {
		files := map[string][]byte{}
		err = filepath.WalkDir(args[1], func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, _ := filepath.Rel(args[1], p)
			files[rel], err = os.ReadFile(p)
			return err
		})
		if err == nil {
			info, err = b.Build(ctx, args[0], files, os.Stdout)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("built %s from %v\n", info.Name, info.Sources)
	return nil
}

// run is the machine's one daemon: the server's state and scheduling, and the
// worker that runs the boxes, joined in memory.
func run(ctx context.Context, root, fcVersion, socket string, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	host, _ := os.Hostname()
	name := fs.String("name", host, "this machine's name")
	fs.Parse(args[1:])
	id, err := workerID(root)
	if err != nil {
		return err
	}
	w, err := worker.New(root, fcVersion)
	if err != nil {
		return err
	}
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if g, err := user.LookupGroup("hutch"); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(dir, 0, gid); err != nil {
			return err
		}
	}
	s, err := server.New(server.Config{Dir: filepath.Join(root, "state"), Socket: socket})
	if err != nil {
		return err
	}
	a, b := net.Pipe()
	go func() {
		if err := agent.Run(ctx, w, *name, a); err != nil && ctx.Err() == nil {
			log.Printf("worker: %v", err)
		}
	}()
	go s.AttachWorker(ctx, id, *name, b)
	log.Printf("hutchd %s on %s, data in %s", version.String(), *name, root)
	return s.Run(ctx)
}

// workerID is this machine's worker ID, made on first start.
func workerID(root string) (string, error) {
	path := filepath.Join(root, "worker-id")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 6)
	rand.Read(b)
	id := "w-" + hex.EncodeToString(b)
	return id, os.WriteFile(path, []byte(id+"\n"), 0o600)
}
