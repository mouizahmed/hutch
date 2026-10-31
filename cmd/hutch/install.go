package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/hashicorp/yamux"
	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/release"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
)

// The Firecracker release hutchd runs, and its published checksums.
const fcVersion = "v1.17.0"

var fcSHA256 = map[string]string{
	"x86_64":  "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558",
	"aarch64": "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256",
}

// gVisor runs every image build step, and gVisor boxes on machines without KVM.
const gvisorVersion = "20261012.0"

var gvisorSHA256 = map[string]string{
	"x86_64":  "21c1f60222826204e57eda551efc68fcdfd94a3ba2ef3e01d2a43d8bc4429e51",
	"aarch64": "bc76435fe7ca56f918d9a61aa27013989fee26d5731ee0ed82b99bb1241faa20",
}

// BuildKit builds images from Dockerfiles on the machine.
const buildkitVersion = "v0.34.0"

var buildkitSHA256 = map[string]string{
	"x86_64":  "14813b1fb2a40082114e6d386c6fc5e7dd5298176ce7ee1a18bc33e73e109ac3",
	"aarch64": "0619ca3e3205a32ea2beb115e0242136e4a0716cc1917ba0ceaa4ef4d52382c9",
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
	return exec.Command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", r.host, "bash -c "+shellQuote("set -euo pipefail; "+script))
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
func (r remote) upload(local, dst string, mode os.FileMode) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	defer pr.Close() // Unblock the compressor if SSH exits before reading stdin.
	go func() {
		enc, _ := zstd.NewWriter(pw, zstd.WithEncoderLevel(zstd.SpeedFastest))
		_, err := io.Copy(enc, f)
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	// A unique file on the destination filesystem keeps interrupted and concurrent
	// transfers separate. Verify the complete payload before atomic replacement.
	// These are Linux machine paths, including when this CLI runs on Windows.
	c := r.cmd(fmt.Sprintf(`sudo mkdir -p %s
tmp=$(sudo mktemp %s)
trap 'sudo rm -- "$tmp" 2>/dev/null || true' EXIT
zstd -dcq | sudo dd of="$tmp" bs=1M conv=sparse status=none
if ! printf '%%s  %%s\n' %s "$tmp" | sudo sha256sum -c --status; then
  echo 'received file failed checksum verification' >&2; exit 1
fi
sudo chmod %o "$tmp"
sudo mv -T -- "$tmp" %s`,
		shellQuote(path.Dir(dst)), shellQuote(path.Join(path.Dir(dst), ".hutch-upload.XXXXXXXX")),
		shellQuote(hex.EncodeToString(h.Sum(nil))), mode, shellQuote(dst)))
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
if [ -f /var/lib/hutch/images/base/image.json ]; then echo base=yes; fi
for f in kernel/vmlinux layer/hutch-guest layer/tmux-$(uname -m) layer/terminfo.tar; do echo "sum_$(basename $f)=$(sudo -n sha256sum /var/lib/hutch/$f 2>/dev/null | cut -c1-64)"; done
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
	if err := check(arch == "x86_64", "architecture", arch, "Hutch machines currently need x86_64; arm64 CLI clients can connect to an x86_64 machine", true); err != nil {
		return err
	}
	if p.get("kvm") == "yes" {
		check(true, "/dev/kvm", "boxes are Firecracker VMs", "", false)
	} else {
		fmt.Println("  · /dev/kvm: none, so boxes are gVisor sandboxes: builds run about 1.5× slower than in a VM, and isolation is gVisor's rather than a VM's")
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

// artifacts are what setup puts on the machine from this computer, built for its
// architecture: hutchd, the Firecracker guest kernel, and Hutch's image layer.
type artifacts struct {
	hutchd, guest, kernel, tmux, terminfo string
	downloadDir                           string
}

func (a artifacts) close() {
	if a.downloadDir != "" {
		os.RemoveAll(a.downloadDir)
	}
}

func findArtifacts(ctx context.Context, arch, hutchd, images string) (artifacts, error) {
	goarch := map[string]string{"x86_64": "amd64"}[arch]
	if goarch == "" {
		return artifacts{}, fmt.Errorf("unsupported machine architecture %q", arch)
	}
	explicit := hutchd != "" || images != ""
	if !explicit && release.IsVersion(version.String()) {
		fmt.Printf("Downloading machine files for Hutch %s (%s)...\n", version.String(), goarch)
		dir, err := release.NewClient().Machine(ctx, version.String(), goarch)
		if err != nil {
			return artifacts{}, fmt.Errorf("machine release: %w", err)
		}
		return artifacts{hutchd: filepath.Join(dir, "hutchd"), guest: filepath.Join(dir, "hutch-guest"),
			kernel: filepath.Join(dir, "vmlinux-"+arch), tmux: filepath.Join(dir, "tmux-"+arch),
			terminfo: filepath.Join(dir, "terminfo.tar"), downloadDir: dir}, nil
	}
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
	a := artifacts{hutchd: hutchd, guest: filepath.Join(filepath.Dir(hutchd), "hutch-guest"), kernel: filepath.Join(images, "vmlinux-"+arch),
		tmux: filepath.Join(images, "tmux-"+arch), terminfo: filepath.Join(images, "terminfo.tar")}
	for _, f := range []string{a.hutchd, a.guest, a.kernel, a.tmux, a.terminfo} {
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
	if p.get("kvm") == "yes" {
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
	}
	gv := "/var/lib/hutch/gvisor/" + gvisorVersion
	if err := step("gVisor "+gvisorVersion, r.run(fmt.Sprintf(`
if [ ! -x %[1]s/runsc ]; then
  t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
  curl -fsSL -o "$t/gvisor.tar.zstd" https://storage.googleapis.com/gvisor/releases/release/%[2]s/%[3]s/gvisor.tar.zstd
  echo "%[4]s  $t/gvisor.tar.zstd" | sha256sum -c --quiet
  sudo mkdir -p %[1]s.new && sudo tar -C %[1]s.new --zstd -xf "$t/gvisor.tar.zstd" && sudo mv %[1]s.new %[1]s
fi`, gv, gvisorVersion, arch, gvisorSHA256[arch]))); err != nil {
		return err
	}
	bk := "/var/lib/hutch/buildkit/" + buildkitVersion
	goarch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[arch]
	if err := step("BuildKit "+buildkitVersion, r.run(fmt.Sprintf(`
if [ ! -x %[1]s/bin/buildkitd ]; then
  t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
  curl -fsSL -o "$t/bk.tgz" https://github.com/moby/buildkit/releases/download/%[2]s/buildkit-%[2]s.linux-%[3]s.tar.gz
  echo "%[4]s  $t/bk.tgz" | sha256sum -c --quiet
  sudo mkdir -p %[1]s.new && sudo tar -C %[1]s.new -xzf "$t/bk.tgz" && sudo mv %[1]s.new %[1]s
fi`, bk, buildkitVersion, goarch, buildkitSHA256[arch]))); err != nil {
		return err
	}
	if err := step("hutchd", r.upload(a.hutchd, "/usr/local/bin/hutchd", 0o755)); err != nil {
		return err
	}
	files := []struct{ local, path string }{
		{a.guest, "/var/lib/hutch/layer/hutch-guest"},
		{a.tmux, "/var/lib/hutch/layer/tmux-" + arch},
		{a.terminfo, "/var/lib/hutch/layer/terminfo.tar"},
	}
	if p.get("kvm") == "yes" {
		files = append(files, struct{ local, path string }{a.kernel, "/var/lib/hutch/kernel/vmlinux"})
	}
	guestChanged := false
	for _, f := range files {
		sum, err := fileSHA256(f.local)
		if err != nil {
			return err
		}
		if p.get("sum_"+filepath.Base(f.path)) == sum {
			continue
		}
		if err := r.upload(f.local, f.path, 0o755); err != nil {
			return err
		}
		guestChanged = guestChanged || filepath.Base(f.path) == "hutch-guest"
	}
	fmt.Println("  ✓ Hutch's image layer" + map[bool]string{true: " and the guest kernel"}[p.get("kvm") == "yes"])
	if p.get("base") != "yes" || guestChanged {
		fmt.Println("  … the default image, built on the machine (a minute or two the first time)")
		var out bytes.Buffer
		c := r.cmd("sudo hutchd image build base")
		c.Stdout, c.Stderr = &out, &out
		if err := c.Run(); err != nil {
			return fmt.Errorf("default image: %v\n%s", err, lastLines(out.String(), 30))
		}
		fmt.Println("  ✓ default image")
	}
	if err := step("group hutch, with "+p.get("user")+" in it", r.run(`sudo groupadd -f hutch && sudo usermod -aG hutch "$(id -un)"`)); err != nil {
		return err
	}
	unit := "sudo tee /etc/systemd/system/hutchd.service >/dev/null <<'UNIT'\n" + hutchdUnit + "UNIT\nsudo systemctl daemon-reload && sudo systemctl enable -q hutchd"
	if start {
		if p.get("state") == "yes" {
			// The uploaded binary understands the current checkpoint operation,
			// even when the old daemon predates it. Per-box file locks coordinate
			// this with that daemon's own checkpoints. Failure leaves it running.
			if err := step("checkpointed running boxes", r.run("sudo /usr/local/bin/hutchd box checkpoint --running")); err != nil {
				return err
			}
		}
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
	force        bool
}

func (f *installFlags) register(c *cobra.Command, setup bool) {
	c.Flags().BoolVar(&f.yes, "yes", false, "take every recommended answer without asking")
	c.Flags().StringVar(&f.disk, "disk", "", "blank disk to format for /var/lib/hutch (default: the only blank disk; none: an image file)")
	c.Flags().Int64Var(&f.imageSize, "image-size", 0, "size in GB of /var/lib/hutch.img when there's no blank disk (default: 80% of free space)")
	c.Flags().StringVar(&f.hutchd, "hutchd", "", "local hutchd binary for the machine (default: download the matching release, or use build output)")
	c.Flags().StringVar(&f.images, "images", "", "local directory with vmlinux-<arch>, tmux-<arch> and terminfo.tar")
	c.Flags().BoolVar(&f.force, "force", false, "install even when the machine runs this or a newer hutchd")
	if setup {
		c.Flags().BoolVar(&f.noBucket, "no-bucket", false, "no bucket for now (add one later with hutch machine bucket)")
	}
}

// prepare probes and checks a machine and plans its storage and files.
func prepare(ctx context.Context, host string, f installFlags) (remote, probe, artifacts, storagePlan, error) {
	r := remote{host}
	fmt.Printf("Connecting to %s...\n", host)
	p, err := r.probe()
	if err != nil {
		return r, p, artifacts{}, storagePlan{}, fmt.Errorf("%w\n  fix: make ssh %s work without a password prompt (a key in ~/.ssh, or a Host entry in ~/.ssh/config)", err, host)
	}
	if err := checkDowngrade(p.get("hutchd"), f.force); err != nil {
		return r, p, artifacts{}, storagePlan{}, err
	}
	if err := checkMachine(p); err != nil {
		return r, p, artifacts{}, storagePlan{}, err
	}
	a, err := findArtifacts(ctx, p.get("arch"), f.hutchd, f.images)
	if err != nil {
		return r, p, a, storagePlan{}, err
	}
	disk := f.disk
	if disk == "none" {
		p.blocks, disk = nil, ""
	}
	st, err := planStorage(p, disk, f.imageSize, f.yes)
	if err != nil {
		a.close()
	}
	return r, p, a, st, err
}

func setupCommand() *cobra.Command {
	var f installFlags
	c := &cobra.Command{
		Use:   "setup <ssh-host>",
		Short: "Set up your machine: hutchd, checks, and an optional bucket",
		Long: `Set up a machine you reach over SSH to run your boxes. It checks the machine,
installs what's missing (packages, Firecracker, gVisor, BuildKit, hutchd as a
service, the default image),
makes it your machine, offers a bucket, and ends with
hutch doctor. Run it again to update hutchd; boxes and state are kept.`,
		GroupID: "setup",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			r, p, a, st, err := prepare(cmd.Context(), host, f)
			if err != nil {
				return err
			}
			defer a.close()
			if err := install(r, p, a, st, true); err != nil {
				return err
			}
			if err := setUserMachine(host); err != nil {
				return err
			}
			fmt.Printf("  ✓ machine = %q in %s\n", host, config.UserPath())
			sess, err := connectTarget(host)
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
			d := doctorCommandFor(host)
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
	if err := saveRecovery(cfg, pass); err != nil {
		return err
	}
	fmt.Printf("  ✓ its details, sealed with your passphrase, in %s (restoring asks only for the passphrase)\n", recoveryPath())
	return nil
}

// checkDowngrade refuses to install this hutchd over a newer one: its state may
// be newer than this version reads.
func checkDowngrade(machine string, force bool) error {
	if !force && version.Older(version.String(), machine) {
		return fmt.Errorf("the machine runs hutchd %s, newer than this hutch (%s): upgrade hutch on this computer, or pass --force", machine, version.String())
	}
	return nil
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
			have, want := p.get("hutchd"), version.String()
			// A build with uncommitted changes isn't identified by its version.
			if err := checkDowngrade(have, f.force); err != nil {
				return err
			}
			if have == want && want != "dev" && !strings.HasSuffix(want, "-dirty") && !f.force {
				// The file may have been uploaded by an interrupted upgrade while
				// the old process is still running. Ask the actual daemon as well.
				if running, err := daemonVersion(cmd.Context(), host); err == nil && running == want {
					fmt.Printf("The machine already runs hutchd %s.\n", have)
					return nil
				}
			}
			a, err := findArtifacts(cmd.Context(), p.get("arch"), f.hutchd, f.images)
			if err != nil {
				return err
			}
			defer a.close()
			fmt.Printf("hutchd %s → %s\n", have, want)
			if err := install(r, p, a, storagePlan{}, true); err != nil {
				return err
			}
			sess, err := connectTarget(host)
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

func daemonVersion(ctx context.Context, host string) (string, error) {
	sess, err := client.Dial(host, client.Socket)
	if err != nil {
		return "", err
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var h api.Handshake
	err = rpc.Call(ctx, sess, api.Hello, api.Handshake{Protocol: api.Protocol, Version: version.String()}, &h)
	return h.Version, err
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
			r, p, a, st, err := prepare(cmd.Context(), host, f)
			if err != nil {
				return err
			}
			defer a.close()
			if p.get("state") == "yes" {
				return fmt.Errorf("%s already has Hutch state in /var/lib/hutch/state; restore onto a fresh machine", host)
			}
			cfg, pass, err := recoverBucket()
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
			// The restored copy of your config is the newest one, but it names the
			// old machine.
			if err := adoptMachine(cmd, host); err != nil {
				return err
			}
			fmt.Printf("  ✓ machine = %q; boxes restore from the bucket when you use them\n", host)
			d := doctorCommandFor(host)
			d.SetContext(cmd.Context())
			return d.RunE(d, nil)
		},
	}
	f.register(c, false)
	return c
}

const recoveryAAD = "laptop/bucket"

// recoveryPath is this computer's copy of the bucket's location and credentials,
// sealed with the recovery passphrase, so restoring onto a new machine asks only
// for the passphrase.
func recoveryPath() string { return filepath.Join(filepath.Dir(config.UserPath()), "recovery.json") }

func saveRecovery(cfg bucket.Config, pass string) error {
	plain, _ := json.Marshal(cfg)
	return writeFileAtomic(recoveryPath(), durable.WrapBytes(recoveryAAD, plain, pass).Marshal())
}

// recoverBucket asks for the passphrase and opens the saved bucket details, or
// asks for everything when this computer has none.
func recoverBucket() (bucket.Config, string, error) {
	data, err := os.ReadFile(recoveryPath())
	if errors.Is(err, os.ErrNotExist) {
		cfg, pass, err := bucketForm(false)
		if err == nil {
			err = saveRecovery(cfg, pass)
		}
		return cfg, pass, err
	}
	if err != nil {
		return bucket.Config{}, "", err
	}
	var r durable.Recovery
	if err := json.Unmarshal(data, &r); err != nil {
		return bucket.Config{}, "", fmt.Errorf("%s: %w", recoveryPath(), err)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return bucket.Config{}, "", errors.New("this needs a terminal: the recovery passphrase is typed, never passed as an argument")
	}
	var pass string
	err = huh.NewForm(huh.NewGroup(huh.NewInput().Title("Recovery passphrase").
		Description("Unlocks your bucket details saved on this computer, then the bucket.").
		EchoMode(huh.EchoModePassword).Value(&pass))).Run()
	if err != nil {
		return bucket.Config{}, "", err
	}
	plain, err := r.UnwrapBytes(recoveryAAD, pass)
	if err != nil {
		return bucket.Config{}, "", err
	}
	var cfg bucket.Config
	if err := json.Unmarshal(plain, &cfg); err != nil {
		return bucket.Config{}, "", err
	}
	fmt.Printf("  ✓ bucket %s, from %s\n", cfg.Name, recoveryPath())
	return cfg, pass, nil
}

// saveExistingBucket saves the details of the bucket the machine already uses on
// this computer, once they and the passphrase open the machine's keys in it.
func saveExistingBucket(cmd *cobra.Command, name string) error {
	cfg, pass, err := bucketForm(false)
	if err != nil {
		return err
	}
	if cfg.Name != name {
		return fmt.Errorf("the machine's bucket is %s, not %s", name, cfg.Name)
	}
	if err := checkPassphrase(cmd, cfg, pass); err != nil {
		return err
	}
	if err := saveRecovery(cfg, pass); err != nil {
		return err
	}
	fmt.Printf("Saved in %s, sealed with your passphrase: restoring onto a new machine asks only for the passphrase.\n", recoveryPath())
	return nil
}

// adoptMachine makes host this computer's machine: its copy of your config, with
// machine set to host, is the one to keep.
func adoptMachine(cmd *cobra.Command, host string) error {
	sess, err := client.Dial(host, client.Socket)
	if err != nil {
		return err
	}
	defer sess.Close()
	if err := checkVersion(cmd.Context(), sess); err != nil {
		return err
	}
	if err := syncUserConfig(cmd.Context(), sess, host, "theirs"); err != nil {
		return err
	}
	if err := setUserMachine(host); err != nil {
		return err
	}
	return syncUserConfig(cmd.Context(), sess, host, "mine")
}

// machineAddress is the machine's SSH target as another computer can use it: the
// user, host, and port an SSH alias stands for.
func machineAddress(target string) string {
	out, err := exec.Command("ssh", "-G", target).Output()
	if err != nil {
		return target
	}
	v := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, val, ok := strings.Cut(line, " "); ok {
			v[k] = val
		}
	}
	addr := v["hostname"]
	if v["user"] != "" {
		addr = v["user"] + "@" + addr
	}
	if p := v["port"]; p != "" && p != "22" {
		return "ssh://" + addr + ":" + p
	}
	return addr
}

func addComputerCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add-computer [<public key>]",
		Short: "Use the machine from another computer too",
		Long: `Prints the command to run on another computer to use this machine from it. Given
that computer's SSH public key (hutch machine join prints it when it can't connect),
it first lets the key in on the machine.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := machineTarget()
			if target == "" {
				return errors.New("no machine set: hutch setup <ssh-host>")
			}
			addr := machineAddress(target)
			if len(args) == 1 {
				key := strings.TrimSpace(args[0])
				if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key)); err != nil {
					return fmt.Errorf("not an SSH public key: %w", err)
				}
				q := shellQuote(key)
				if err := (remote{target}).run(`mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && { grep -qxF ` + q + ` ~/.ssh/authorized_keys || echo ` + q + ` >> ~/.ssh/authorized_keys; }`); err != nil {
					return err
				}
				fmt.Printf("Let the key in on %s.\n", target)
			}
			fmt.Printf("On the other computer, install hutch, then run:\n\n  hutch machine join %s\n\n", addr)
			fmt.Println("It checks it can reach the machine (and says what to do if not), takes your config, and saves the bucket details sealed with your passphrase.")
			return nil
		},
	}
}

func joinCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "join <ssh-host>",
		Short: "Use a machine that another of your computers set up",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			if err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", host, "true").Run(); err != nil {
				fmt.Printf("This computer can't SSH to %s yet.\n", host)
				if key := publicKey(); key != "" {
					fmt.Printf("On a computer that can, run:\n\n  hutch machine add-computer '%s'\n\nthen run this again.\n", key)
				} else {
					fmt.Println("Make it an SSH key first (ssh-keygen -t ed25519), then run this again for the next step.")
				}
				return errors.New("no SSH access yet")
			}
			if err := adoptMachine(cmd, host); err != nil {
				return err
			}
			fmt.Printf("  ✓ machine = %q, with your config from it\n", host)
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var info api.BucketInfo
			if err := rpc.Call(cmd.Context(), sess, api.BucketGet, struct{}{}, &info); err != nil {
				return err
			}
			if info.Name != "" {
				if err := saveBucketFrom(cmd, remote{host}, info.Name); err != nil {
					return err
				}
			}
			fmt.Println("Done. This computer uses the machine; clone your repositories here to start boxes from them.")
			return nil
		},
	}
}

// publicKey is this computer's SSH public key, if it has one.
func publicKey() string {
	home, _ := os.UserHomeDir()
	for _, name := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		if b, err := os.ReadFile(filepath.Join(home, ".ssh", name)); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// saveBucketFrom saves the machine's bucket details on this computer, read from the
// machine over SSH, once the passphrase opens the machine's keys in the bucket; or
// typed, when this SSH user can't read them.
func saveBucketFrom(cmd *cobra.Command, r remote, name string) error {
	c := r.cmd("sudo -n cat /var/lib/hutch/state/bucket.json")
	out, err := c.Output()
	var cfg bucket.Config
	if err != nil || json.Unmarshal(out, &cfg) != nil || cfg.Name != name {
		fmt.Printf("The machine backs up to %s. Enter its details to save them here.\n", name)
		return saveExistingBucket(cmd, name)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("this needs a terminal: the recovery passphrase is typed, never passed as an argument")
	}
	var pass string
	err = huh.NewForm(huh.NewGroup(huh.NewInput().Title("Recovery passphrase").
		Description("The machine backs up to " + name + ". The passphrase seals its details on this computer, for restoring onto a new machine.").
		EchoMode(huh.EchoModePassword).Value(&pass))).Run()
	if err != nil {
		return err
	}
	if err := checkPassphrase(cmd, cfg, pass); err != nil {
		return err
	}
	if err := saveRecovery(cfg, pass); err != nil {
		return err
	}
	fmt.Printf("  ✓ bucket %s's details in %s, sealed with your passphrase\n", name, recoveryPath())
	return nil
}

// checkPassphrase opens the machine's keys in the bucket with pass.
func checkPassphrase(cmd *cobra.Command, cfg bucket.Config, pass string) error {
	b, err := bucket.New(cfg)
	if err != nil {
		return err
	}
	data, err := b.Get(cmd.Context(), "server/master")
	if err != nil {
		return fmt.Errorf("read %s: %w", cfg.Name, err)
	}
	var r durable.Recovery
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	_, err = r.Unwrap(pass)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
