//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
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
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/server"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/worker"
)

const usage = `usage: hutchd [--root DIR] [--firecracker VERSION] [--socket PATH] <command>

commands:
  box boot <id> [--image NAME] [--cpus N] [--mem MIB]   cold boot a new box from an image
  box fork <src> <id>                                    new box from a sleeping box's snapshot
  box sleep | wake | archive | rm <id>
  box checkpoint <id> | --running                       checkpoint without stopping boxes
  box ls
  box exec <id> [--user NAME] -- <command> [args...]
  box console <id>                                       print the serial console log
  box status <id>                                        print what hutch-guest reports (threads, sessions, ports)
  image build [--pin REF@DIGEST]... <name> [<dir>]       build an image from the Dockerfile in a directory (none: the default image)
  run [--name NAME]                                      run the machine: boxes, their state, and the API
  fence [<why>]                                          stop this machine acting on its own: restart hutchd with its boxes asleep
  join --lead HOST:PORT --token TOKEN --name NAME        join another machine's lead as a worker (hutch machine add runs it)
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
	case "join":
		return join(ctx, root, args)
	case "fence":
		return fence(root, args)
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
	case "checkpoint":
		if len(rest) != 1 {
			return fmt.Errorf("usage: hutchd box checkpoint <id> | --running")
		}
		if rest[0] != "--running" {
			return done(w.Checkpoint(ctx, rest[0]))
		}
		boxes, err := w.List()
		if err != nil {
			return err
		}
		for _, b := range boxes {
			if b.State == worker.Running && w.Alive(b) {
				if err := done(w.Checkpoint(ctx, b.ID)); err != nil {
					return fmt.Errorf("checkpoint %s: %w", b.ID, err)
				}
			}
		}
		return nil
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
	var pins []string
	for len(args) > 1 && args[0] == "--pin" {
		pins, args = append(pins, args[1]), args[2:]
	}
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: hutchd image build [--pin ref@sha256:…]... <name> [<context-dir>]")
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
			info, err = b.Build(ctx, args[0], files, pins, os.Stdout, false)
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
	if ld := link.Dir(filepath.Join(root, "link")); ld.Enrolled() {
		log.Printf("hutchd %s on %s, data in %s: a worker of another machine's lead", version.String(), ld.Name(), root)
		a := agent.Start(ctx, w, ld.Name(), ld)
		if why, err := os.ReadFile(filepath.Join(root, "fenced")); err == nil {
			log.Printf("fenced: %s", strings.TrimSpace(string(why)))
			a.Fence(ctx, strings.TrimSpace(string(why)))
		}
		go func() {
			if err := a.ServeCLI(ctx, socket, ld); err != nil {
				log.Printf("cli: %v", err)
			}
		}()
		a.Link(ctx, ld)
		return nil
	}
	s, err := server.New(server.Config{Dir: filepath.Join(root, "state"), Socket: socket})
	if err != nil {
		return err
	}
	a, b := net.Pipe()
	go func() {
		if err := agent.Start(ctx, w, *name, "").Serve(ctx, a); err != nil && ctx.Err() == nil {
			log.Printf("worker: %v", err)
		}
	}()
	go s.AttachWorker(ctx, id, *name, b)
	log.Printf("hutchd %s on %s, data in %s", version.String(), *name, root)
	return s.Run(ctx)
}

// join enrolls this machine as a worker of another machine's lead. A machine
// that keeps its own state can't join: it leads.
func join(ctx context.Context, root string, args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	lead := fs.String("lead", "", "the lead's link address, host:port")
	token := fs.String("token", "", "a token from hutch machine add")
	name := fs.String("name", "", "this machine's name")
	fs.Parse(args[1:])
	if *lead == "" || *token == "" || *name == "" {
		return errors.New("usage: hutchd join --lead HOST:PORT --token TOKEN --name NAME")
	}
	if _, err := os.Stat(filepath.Join(root, "state", "hutch.db")); err == nil {
		return fmt.Errorf("this machine has its own Hutch state in %s; it can't join another", filepath.Join(root, "state"))
	}
	// A machine added again replaces its old enrollment once the new one succeeds.
	dir := filepath.Join(root, "link")
	os.RemoveAll(dir + ".new")
	id, err := link.Enroll(ctx, link.Dir(dir+".new"), *lead, *token, *name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(dir+".new", dir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "worker-id"), []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	// The lead it joins decides about its boxes from now on.
	if err := os.Remove(filepath.Join(root, "fenced")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("joined %s as %s\n", *lead, id)
	return nil
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

// fence stops this machine acting on its own, as another takes its boxes: it
// records why, kills the running hutchd (SIGKILL works on a paused one too; the
// boxes' VMs live on), and starts it again fenced, which puts every box to sleep.
func fence(root string, args []string) error {
	why := "another machine took over its boxes"
	if len(args) > 1 {
		why = strings.Join(args[1:], " ")
	}
	if err := os.WriteFile(filepath.Join(root, "fenced"), []byte(why+"\n"), 0o600); err != nil {
		return err
	}
	exec.Command("systemctl", "kill", "--kill-whom=main", "--signal=SIGKILL", "hutchd").Run()
	if out, err := exec.Command("systemctl", "restart", "hutchd").CombinedOutput(); err != nil {
		return fmt.Errorf("restart hutchd: %v: %s", err, out)
	}
	fmt.Println("fenced:", why)
	return nil
}
