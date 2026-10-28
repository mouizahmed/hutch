package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/hashicorp/yamux"
	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"rsc.io/qr"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The Firecracker release hutchd runs, and its published checksums.
const fcVersion = "v1.17.0"

var fcSHA256 = map[string]string{
	"x86_64":  "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558",
	"aarch64": "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256",
}

const hutchdUnit = `[Unit]
Description=Hutch
After=network-online.target
Wants=network-online.target
RequiresMountsFor=/var/lib/hutch

[Service]
ExecStart=/usr/local/bin/hutchd run
Restart=always
RestartSec=2
KillMode=process

[Install]
WantedBy=multi-user.target
`

// remote runs commands on a machine over SSH, as its SSH user, with sudo where needed.
type remote struct{ host string }

func (r remote) cmd(script string) *exec.Cmd {
	return exec.Command("ssh", "-o", "BatchMode=yes", r.host, "bash -c "+shellQuote("set -euo pipefail; "+script))
}

func (r remote) run(script string) error {
	c := r.cmd(script)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// upload streams a local file to path on the machine, compressed on the way.
func (r remote) upload(local, path string, mode os.FileMode) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	go func() {
		enc, _ := zstd.NewWriter(pw, zstd.WithEncoderLevel(zstd.SpeedFastest))
		_, err := io.Copy(enc, f)
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	tmp := path + ".new"
	c := r.cmd(fmt.Sprintf("sudo mkdir -p %s && zstd -dcq | sudo dd of=%s bs=1M conv=sparse status=none && sudo chmod %o %s && sudo mv %s %s",
		shellQuote(filepath.Dir(path)), shellQuote(tmp), mode, shellQuote(tmp), shellQuote(tmp), shellQuote(path)))
	var out bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = pr, &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("upload %s: %v: %s", filepath.Base(local), err, strings.TrimSpace(out.String()))
	}
	return nil
}

const probeScript = `
echo "user=$(id -un)"
if [ "$(id -u)" = 0 ]; then echo sudo=root; elif sudo -n true 2>/dev/null; then echo sudo=yes; else echo sudo=no; fi
echo "arch=$(uname -m)"
echo "kernel=$(uname -r)"
if [ -c /dev/kvm ]; then echo kvm=yes; fi
if command -v apt-get >/dev/null; then echo apt=yes; fi
if command -v mkfs.xfs >/dev/null && command -v zstd >/dev/null && command -v curl >/dev/null; then echo tools=yes; fi
echo "mem_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo)"
echo "cpus=$(nproc)"
echo "root_free_kb=$(df -Pk / | awk 'NR==2{print $4}')"
echo "hutch_fs=$(findmnt -n -o FSTYPE /var/lib/hutch 2>/dev/null || true)"
lsblk -nrpbo NAME,TYPE,FSTYPE,SIZE,PKNAME 2>/dev/null | sed 's/^/blk=/' || true
echo "hutchd=$(hutchd version 2>/dev/null | awk '{print $2}' || true)"
if [ -d /var/lib/hutch/images/base ]; then echo base=yes; fi
if [ -n "$(sudo -n ls /var/lib/hutch/state/hutch.db 2>/dev/null)" ]; then echo state=yes; fi
`

type probe struct {
	values map[string]string
	blocks []string
}

func (p probe) get(k string) string { return p.values[k] }

func (p probe) kb(k string) int64 {
	n, _ := strconv.ParseInt(p.values[k], 10, 64)
	return n
}

func (r remote) probe() (probe, error) {
	c := r.cmd(probeScript)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return probe{}, fmt.Errorf("ssh %s: %v: %s", r.host, err, strings.TrimSpace(stderr.String()))
	}
	p := probe{values: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		if k == "blk" {
			p.blocks = append(p.blocks, v)
		} else {
			p.values[k] = v
		}
	}
	return p, nil
}

// blankDisks are whole disks with no filesystem and no partitions.
func (p probe) blankDisks() [][2]string {
	parents := map[string]bool{}
	for _, b := range p.blocks {
		if f := strings.Split(b, " "); len(f) == 5 && f[4] != "" {
			parents[f[4]] = true
		}
	}
	var out [][2]string
	for _, b := range p.blocks {
		f := strings.Split(b, " ")
		if len(f) == 5 && f[1] == "disk" && f[2] == "" && !parents[f[0]] {
			size, _ := strconv.ParseInt(f[3], 10, 64)
			out = append(out, [2]string{f[0], fmt.Sprintf("%d GB", size>>30)})
		}
	}
	return out
}

// check prints a setup check; a failure ends setup with its fix.
func check(ok bool, name, detail, fix string, fatal bool) error {
	mark := "✓"
	if !ok {
		mark = "!"
		if fatal {
			mark = "✗"
		}
	}
	line := "  " + mark + " " + name
	if detail != "" {
		line += ": " + detail
	}
	fmt.Println(line)
	if !ok && fix != "" {
		fmt.Println("      fix: " + fix)
	}
	if !ok && fatal {
		return fmt.Errorf("%s: %s", name, fix)
	}
	return nil
}

func kernelAtLeast(release string, major, minor int) bool {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, _ := strconv.Atoi(parts[0])
	mi, _ := strconv.Atoi(strings.TrimLeftFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	return ma > major || (ma == major && mi >= minor)
}

// checkMachine prints what the machine has and fails on what Hutch can't run without.
func checkMachine(p probe) error {
	fmt.Println("\nMachine")
	if err := check(p.get("sudo") != "no", "sudo", p.get("user"), "connect as root, or give "+p.get("user")+" passwordless sudo", true); err != nil {
		return err
	}
	arch := p.get("arch")
	if err := check(fcSHA256[arch] != "", "architecture", arch, "Hutch runs on x86_64 and aarch64", true); err != nil {
		return err
	}
	if err := check(p.get("kvm") == "yes", "/dev/kvm", "", "boxes need KVM until gVisor support lands: a dedicated server, a VPS plan with nested virtualization, or your own hardware with VT-x or AMD-V on", true); err != nil {
		return err
	}
	if err := check(kernelAtLeast(p.get("kernel"), 6, 1), "kernel", p.get("kernel"), "Linux 6.1 or later", true); err != nil {
		return err
	}
	if p.get("apt") != "yes" && p.get("tools") != "yes" {
		return check(false, "packages", "no apt-get", "install xfsprogs, zstd and curl, then run setup again", true)
	}
	mem := p.kb("mem_kb") >> 20
	check(mem >= 8, "memory", fmt.Sprintf("%d GB", mem), "8 GB or more leaves room for a few boxes", false)
	check(p.kb("cpus") >= 2, "CPUs", p.get("cpus"), "2 or more", false)
	return nil
}

type storagePlan struct {
	disk      string // format this blank disk
	imageSize int64  // or make an image file of this many GB
}

// planStorage decides where /var/lib/hutch lives: an XFS mount that's there, a
// blank disk, or an XFS image file on the root filesystem.
func planStorage(p probe, disk string, imageGB int64, yes bool) (storagePlan, error) {
	fmt.Println("\nStorage")
	if fs := p.get("hutch_fs"); fs != "" {
		return storagePlan{}, check(fs == "xfs", "/var/lib/hutch", fs, "/var/lib/hutch must be XFS with reflink; unmount it, then run setup again", true)
	}
	blank := p.blankDisks()
	if disk == "" && len(blank) == 1 {
		disk = blank[0][0]
	}
	if disk != "" {
		size := ""
		for _, b := range blank {
			if b[0] == disk {
				size = b[1]
			}
		}
		if size == "" {
			return storagePlan{}, fmt.Errorf("%s isn't a blank disk on the machine", disk)
		}
		fmt.Printf("  %s (%s) is blank: it becomes /var/lib/hutch, formatted as XFS with reflink.\n", disk, size)
		if !yes && !confirm("Format "+disk+"?") {
			return storagePlan{}, errors.New("cancelled; choose another disk with --disk, or none with --disk none")
		}
		return storagePlan{disk: disk}, nil
	}
	free := p.kb("root_free_kb") >> 20
	if imageGB == 0 {
		imageGB = free * 8 / 10
	}
	if imageGB < 20 {
		return storagePlan{}, check(false, "disk", fmt.Sprintf("%d GB free", free), "Hutch needs at least 20 GB for /var/lib/hutch: free some space, or attach a blank disk", true)
	}
	fmt.Printf("  No blank disk: /var/lib/hutch becomes an XFS image file of %d GB on / (%d GB free).\n", imageGB, free)
	if !yes && !confirm("Create it?") {
		return storagePlan{}, errors.New("cancelled; set its size with --image-size, or attach a blank disk")
	}
	return storagePlan{imageSize: imageGB}, nil
}

// artifacts are what setup puts on the machine from this computer: hutchd and
// the base image, built for the machine's architecture.
type artifacts struct{ hutchd, kernel, rootfs string }

func findArtifacts(arch, hutchd, images string) (artifacts, error) {
	goarch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[arch]
	exe, err := selfPath()
	if err != nil {
		return artifacts{}, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	dir := filepath.Dir(exe)
	if hutchd == "" {
		hutchd = filepath.Join(dir, "linux-"+goarch, "hutchd")
	}
	if images == "" {
		images = filepath.Join(dir, "..", "dist", "images")
	}
	a := artifacts{hutchd, filepath.Join(images, "vmlinux-"+arch), filepath.Join(images, "rootfs-"+arch+".ext4")}
	for _, f := range []string{a.hutchd, a.kernel, a.rootfs} {
		if _, err := os.Stat(f); err != nil {
			return a, fmt.Errorf("%s: build it with make linux images, or pass --hutchd and --images", f)
		}
	}
	return a, nil
}

// install puts hutchd on the machine as a service: packages, storage, Firecracker,
// hutchd, the base image, and the hutch group. It starts hutchd unless told not to.
func install(r remote, p probe, a artifacts, st storagePlan, start bool) error {
	arch := p.get("arch")
	step := func(what string, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		fmt.Println("  ✓ " + what)
		return nil
	}
	fmt.Println("\nInstalling")
	if p.get("apt") == "yes" {
		if err := step("packages: xfsprogs, zstd, curl", r.run("sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq xfsprogs zstd curl ca-certificates >/dev/null")); err != nil {
			return err
		}
	}
	fstab := func(src, opts string) string {
		return fmt.Sprintf("sudo mkdir -p /var/lib/hutch && { grep -q ' /var/lib/hutch ' /etc/fstab || echo %s | sudo tee -a /etc/fstab >/dev/null; } && sudo mount /var/lib/hutch",
			shellQuote(src+" /var/lib/hutch xfs "+opts+" 0 2"))
	}
	switch {
	case st.disk != "":
		if err := step("formatted "+st.disk+" as XFS, mounted at /var/lib/hutch", r.run(
			fmt.Sprintf("sudo mkfs.xfs -q -L hutch -m reflink=1 %s && ", shellQuote(st.disk))+fstab("LABEL=hutch", "defaults,nofail"))); err != nil {
			return err
		}
	case st.imageSize > 0:
		if err := step(fmt.Sprintf("created /var/lib/hutch.img (%d GB, XFS), mounted at /var/lib/hutch", st.imageSize), r.run(
			fmt.Sprintf("sudo truncate -s %dG /var/lib/hutch.img && sudo mkfs.xfs -q -m reflink=1 /var/lib/hutch.img && ", st.imageSize)+fstab("/var/lib/hutch.img", "loop,nofail"))); err != nil {
			return err
		}
	}
	if err := r.run("xfs_info /var/lib/hutch | grep -q reflink=1"); err != nil {
		return check(false, "/var/lib/hutch", "XFS without reflink", "reformat it with mkfs.xfs -m reflink=1", true)
	}
	fc := "/var/lib/hutch/firecracker/" + fcVersion
	tgz := fmt.Sprintf("firecracker-%s-%s.tgz", fcVersion, arch)
	if err := step("Firecracker "+fcVersion, r.run(fmt.Sprintf(`
if [ ! -x %[1]s/firecracker ]; then
  t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
  curl -fsSL -o "$t/fc.tgz" https://github.com/firecracker-microvm/firecracker/releases/download/%[2]s/%[3]s
  echo "%[4]s  $t/fc.tgz" | sha256sum -c --quiet
  tar -C "$t" -xzf "$t/fc.tgz"
  sudo mkdir -p %[1]s
  sudo install -m 755 "$t/release-%[2]s-%[5]s/firecracker-%[2]s-%[5]s" %[1]s/firecracker
  sudo install -m 755 "$t/release-%[2]s-%[5]s/jailer-%[2]s-%[5]s" %[1]s/jailer
fi`, fc, fcVersion, tgz, fcSHA256[arch], arch))); err != nil {
		return err
	}
	if err := step("hutchd", r.upload(a.hutchd, "/usr/local/bin/hutchd", 0o755)); err != nil {
		return err
	}
	if p.get("base") != "yes" {
		fmt.Println("  … base image (a few hundred MB compressed)")
		if err := r.upload(a.kernel, "/var/lib/hutch/tmp/vmlinux", 0o644); err != nil {
			return err
		}
		if err := r.upload(a.rootfs, "/var/lib/hutch/tmp/rootfs.ext4", 0o644); err != nil {
			return err
		}
		if err := step("base image", r.run("sudo hutchd image import base /var/lib/hutch/tmp/vmlinux /var/lib/hutch/tmp/rootfs.ext4 >/dev/null && sudo rm -rf /var/lib/hutch/tmp")); err != nil {
			return err
		}
	}
	if err := step("group hutch, with "+p.get("user")+" in it", r.run(`sudo groupadd -f hutch && sudo usermod -aG hutch "$(id -un)"`)); err != nil {
		return err
	}
	unit := "sudo tee /etc/systemd/system/hutchd.service >/dev/null <<'UNIT'\n" + hutchdUnit + "UNIT\nsudo systemctl daemon-reload && sudo systemctl enable -q hutchd"
	if start {
		unit += " && sudo systemctl restart hutchd && for i in $(seq 50); do [ -S /run/hutch/api.sock ] && exit 0; sleep 0.2; done; echo 'hutchd did not start:'; sudo journalctl -u hutchd -n 20 --no-pager; exit 1"
	}
	return step("hutchd service", r.run(unit))
}

type installFlags struct {
	yes          bool
	disk, hutchd string
	images       string
	imageSize    int64
	noBucket     bool
	noNotify     bool
	ntfy         string
}

func (f *installFlags) register(c *cobra.Command, setup bool) {
	c.Flags().BoolVar(&f.yes, "yes", false, "take every recommended answer without asking")
	c.Flags().StringVar(&f.disk, "disk", "", "blank disk to format for /var/lib/hutch (default: the only blank disk; none: an image file)")
	c.Flags().Int64Var(&f.imageSize, "image-size", 0, "size in GB of /var/lib/hutch.img when there's no blank disk (default: 80% of free space)")
	c.Flags().StringVar(&f.hutchd, "hutchd", "", "hutchd binary for the machine (default: linux-<arch>/hutchd next to this program)")
	c.Flags().StringVar(&f.images, "images", "", "directory with vmlinux-<arch> and rootfs-<arch>.ext4 (default: dist/images in this checkout)")
	if setup {
		c.Flags().BoolVar(&f.noBucket, "no-bucket", false, "no bucket for now (add one later with hutch machine bucket)")
		c.Flags().BoolVar(&f.noNotify, "no-notify", false, "no ntfy notifications")
		c.Flags().StringVar(&f.ntfy, "ntfy", "https://ntfy.sh", "ntfy server for notifications")
	}
}

// prepare probes and checks a machine and plans its storage and files.
func prepare(host string, f installFlags) (remote, probe, artifacts, storagePlan, error) {
	r := remote{host}
	fmt.Printf("Connecting to %s...\n", host)
	p, err := r.probe()
	if err != nil {
		return r, p, artifacts{}, storagePlan{}, fmt.Errorf("%w\n  fix: make ssh %s work without a password prompt (a key in ~/.ssh, or a Host entry in ~/.ssh/config)", err, host)
	}
	if err := checkMachine(p); err != nil {
		return r, p, artifacts{}, storagePlan{}, err
	}
	a, err := findArtifacts(p.get("arch"), f.hutchd, f.images)
	if err != nil {
		return r, p, a, storagePlan{}, err
	}
	disk := f.disk
	if disk == "none" {
		p.blocks, disk = nil, ""
	}
	st, err := planStorage(p, disk, f.imageSize, f.yes)
	return r, p, a, st, err
}

func setupCommand() *cobra.Command {
	var f installFlags
	c := &cobra.Command{
		Use:   "setup <ssh-host>",
		Short: "Set up your machine: hutchd, checks, notifications, and an optional bucket",
		Long: `Set up a machine you reach over SSH to run your boxes. It checks the machine,
installs what's missing (packages, Firecracker, hutchd as a service, the base image),
makes it your machine, offers ntfy notifications and a bucket, and ends with
hutch doctor. Run it again to update hutchd; boxes and state are kept.`,
		GroupID: "setup",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			r, p, a, st, err := prepare(host, f)
			if err != nil {
				return err
			}
			if err := install(r, p, a, st, true); err != nil {
				return err
			}
			if err := setUserMachine(host); err != nil {
				return err
			}
			fmt.Printf("  ✓ machine = %q in %s\n", host, config.UserPath())
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			interactive := term.IsTerminal(int(os.Stdin.Fd())) && !f.yes
			if p.get("state") != "yes" && !f.noBucket && interactive {
				if err := offerBucket(cmd, sess); err != nil {
					return err
				}
			}
			if !f.noNotify {
				if err := offerNtfy(cmd, sess, f.ntfy, f.yes || !interactive); err != nil {
					return err
				}
			}
			d := doctorCommand()
			d.SetContext(cmd.Context())
			return d.RunE(d, nil)
		},
	}
	f.register(c, true)
	return c
}

func offerBucket(cmd *cobra.Command, sess *yamux.Session) error {
	add := true
	err := huh.NewForm(huh.NewGroup(huh.NewSelect[bool]().
		Title("Keep backups and box snapshots in a bucket?").
		Description("Without one, losing this machine loses its boxes, and you can't move them to a new machine.").
		Options(huh.NewOption("Add a bucket (recommended)", true), huh.NewOption("No bucket for now (add one later with hutch machine bucket)", false)).
		Value(&add))).Run()
	if err != nil || !add {
		return err
	}
	cfg, pass, err := bucketForm(true)
	if err != nil {
		return err
	}
	fmt.Println("Checking the bucket and writing the first backup...")
	if err := rpc.Call(cmd.Context(), sess, api.ServerInit, api.ServerInitParams{Bucket: cfg, Passphrase: pass}, nil); err != nil {
		return err
	}
	fmt.Printf("  ✓ bucket %s: everything durable goes there, encrypted\n", cfg.Name)
	return nil
}

// offerNtfy sets notify.command to publish to a new private ntfy topic, shows how
// to subscribe, and sends a test notification through the machine.
func offerNtfy(cmd *cobra.Command, sess *yamux.Session, server string, yes bool) error {
	cfg, err := config.ReadFile(config.User, config.UserPath())
	if err != nil {
		return err
	}
	if config.New(cfg).String("notify.command") != "" {
		fmt.Println("\nNotifications: notify.command is already set.")
		if err := rpc.Call(cmd.Context(), sess, api.NotifyTest, struct{}{}, nil); err != nil {
			return fmt.Errorf("test notification: %w", err)
		}
		fmt.Println("  ✓ sent a test notification from the machine")
		return nil
	}
	if !yes && !confirm("\nSend notifications to your phone and desktop with ntfy (a private topic on "+server+")?") {
		fmt.Println("  Notifications show in the TUI while it's open; set notify.command for any other notifier.")
		return nil
	}
	b := make([]byte, 15)
	rand.Read(b)
	topic := "hutch-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	url := strings.TrimRight(server, "/") + "/" + topic
	command := `curl -fsS -o /dev/null -H "Title: $HUTCH_TITLE" -H "Tags: $HUTCH_EVENT" -d "$HUTCH_MESSAGE" ` + url
	if err := editLayer(cmd.Context(), sess, config.User, func(text []byte) ([]byte, error) {
		path, _ := config.SplitPath("notify.command")
		return config.Set(withVersion(text), path, command)
	}); err != nil {
		return err
	}
	fmt.Printf("\nNotifications go to %s\n", url)
	fmt.Println("Subscribe in the ntfy app (phone: scan this; desktop: open the link) to that topic. Anyone with the name can read it, so keep it private.")
	printQR(url)
	if err := rpc.Call(cmd.Context(), sess, api.NotifyTest, struct{}{}, nil); err != nil {
		return fmt.Errorf("test notification: %w", err)
	}
	fmt.Println("  ✓ sent a test notification from the machine")
	return nil
}

func printQR(text string) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return
	}
	black := func(x, y int) bool { return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y) }
	var sb strings.Builder
	for y := -2; y < code.Size+2; y += 2 {
		sb.WriteString("  ")
		for x := -2; x < code.Size+2; x++ {
			top, bottom := black(x, y), black(x, y+1)
			switch {
			case top && bottom:
				sb.WriteString(" ")
			case top:
				sb.WriteString("▄")
			case bottom:
				sb.WriteString("▀")
			default:
				sb.WriteString("█")
			}
		}
		sb.WriteString("\n")
	}
	fmt.Print(sb.String())
}

func upgradeCommand() *cobra.Command {
	var f installFlags
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Install this version of hutchd on the machine and restart it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host := machineTarget()
			if host == "" {
				return errors.New("no machine set: hutch setup <ssh-host>")
			}
			r := remote{host}
			p, err := r.probe()
			if err != nil {
				return err
			}
			a, err := findArtifacts(p.get("arch"), f.hutchd, f.images)
			if err != nil {
				return err
			}
			fmt.Printf("hutchd %s → this version\n", p.get("hutchd"))
			if err := install(r, p, a, storagePlan{}, true); err != nil {
				return err
			}
			sess, err := connect()
			if err != nil {
				return err
			}
			sess.Close()
			fmt.Println("Upgraded.")
			return nil
		},
	}
	f.register(c, false)
	return c
}

func restoreCommand() *cobra.Command {
	var f installFlags
	c := &cobra.Command{
		Use:   "restore <ssh-host>",
		Short: "Set up a new machine from your bucket: its state, then its boxes as you use them",
		Long: `Set up a new machine and restore your old machine's state onto it from the bucket,
with your recovery passphrase. Boxes come back from their newest snapshots the
first time you use them. Then the new machine is your machine.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			r, p, a, st, err := prepare(host, f)
			if err != nil {
				return err
			}
			if p.get("state") == "yes" {
				return fmt.Errorf("%s already has Hutch state in /var/lib/hutch/state; restore onto a fresh machine", host)
			}
			cfg, pass, err := bucketForm(false)
			if err != nil {
				return err
			}
			if err := install(r, p, a, st, false); err != nil {
				return err
			}
			body, _ := json.Marshal(api.ServerInitParams{Bucket: cfg, Passphrase: pass})
			c := r.cmd("sudo hutchd restore")
			var out bytes.Buffer
			c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(body), &out, &out
			if err := c.Run(); err != nil {
				return fmt.Errorf("restore: %v: %s", err, strings.TrimSpace(out.String()))
			}
			fmt.Print("  ✓ " + out.String())
			if err := r.run("sudo systemctl restart hutchd && for i in $(seq 50); do [ -S /run/hutch/api.sock ] && exit 0; sleep 0.2; done; exit 1"); err != nil {
				return fmt.Errorf("start hutchd: %w", err)
			}
			if err := setUserMachine(host); err != nil {
				return err
			}
			fmt.Printf("  ✓ machine = %q; boxes restore from the bucket when you use them\n", host)
			time.Sleep(time.Second)
			d := doctorCommand()
			d.SetContext(cmd.Context())
			return d.RunE(d, nil)
		},
	}
	f.register(c, false)
	return c
}
