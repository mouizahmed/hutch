//go:build linux

package guest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// TokenEnv carries the token TCP clients must present; init hands it to serve only.
const TokenEnv = "HUTCH_GUEST_TOKEN"

type service struct {
	name  string
	argv  []string
	env   []string
	log   string // file for its output; "" means init's own (the console)
	tty   string // run as a login shell with this terminal as its controlling tty
	uid   int
	pid   int
	since time.Time
	fails int
	next  time.Time
}

// Init is PID 1 in every box, whatever the image: images need no systemd. It mounts
// what the kernel or sandbox didn't, supervises hutch-guest serve, sshd and dockerd
// when the image has them, and the project's services, and reaps orphans.
func Init(listen, backend string) error {
	log.SetPrefix("hutch-init: ")
	log.SetFlags(0)
	mountEssentials(backend)
	if b, err := os.ReadFile("/etc/hostname"); err == nil {
		unix.Sethostname([]byte(strings.TrimSpace(string(b))))
	}
	loopbackUp()
	for _, d := range []string{"/run/sshd", "/run/hutch", "/var/lib/hutch-guest", "/var/log/hutch"} {
		os.MkdirAll(d, 0o755)
	}
	os.Chmod("/tmp", os.ModeSticky|0o777) // Go drops a numeric 0o1000

	token := os.Getenv(TokenEnv)
	os.Unsetenv(TokenEnv)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	serve := &service{name: "hutch-guest", argv: []string{self, "serve", "--listen", listen}}
	if token != "" {
		serve.env = []string{TokenEnv + "=" + token}
	}
	fixed := []*service{serve}
	if _, err := os.Stat("/usr/sbin/sshd"); err == nil {
		fixed = append(fixed, &service{name: "sshd", argv: []string{"/bin/sh", "-c", sshdScript}, log: "/var/log/hutch/sshd.log"})
	}
	if p := dockerd(); p != "" {
		argv := []string{p}
		if backend == "gvisor" {
			if err := dockerForGVisor(); err != nil {
				log.Printf("docker: %v", err)
			}
			argv = []string{"/bin/sh", "-c", gvisorDockerNAT + "\nexec " + p}
		}
		fixed = append(fixed, &service{name: "dockerd", argv: argv, log: "/var/log/hutch/dockerd.log"})
	}
	if _, err := os.Stat("/dev/ttyS0"); err == nil && backend == "firecracker" {
		fixed = append(fixed, &service{name: "console", argv: []string{loginShell(), "-l"}, tty: "/dev/ttyS0"})
	}

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGPWR)
	running := map[int]*service{}
	all := append([]*service{}, fixed...)
	all = append(all, readServices(nil)...)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		now := time.Now()
		changed := false
		for _, s := range all {
			if s.pid == 0 && !now.Before(s.next) {
				changed = true
				if pid, err := s.start(); err != nil {
					log.Printf("start %s: %v", s.name, err)
					s.backoff(now)
				} else {
					s.pid, s.since = pid, now
					running[pid] = s
				}
			}
		}
		if changed {
			writeStatus(all)
		}
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGCHLD:
				reap(running)
				writeStatus(all)
			case syscall.SIGHUP:
				all = append(append([]*service{}, fixed...), readServices(all)...)
				stopRemoved(running, all)
				writeStatus(all)
			default:
				shutdown()
				return nil
			}
		case <-tick.C:
		}
	}
}

// gvisorDockerNAT runs before Docker in a gVisor box: gVisor's netstack has
// legacy iptables only, and the one NAT rule Docker's bridge networks need is
// added here, as Docker runs without its own rules (see dockerForGVisor).
const gvisorDockerNAT = `for t in iptables ip6tables; do [ -x /usr/sbin/$t-legacy ] && update-alternatives --set $t /usr/sbin/$t-legacy >/dev/null 2>&1; done
iptables -t nat -C POSTROUTING -s 172.16.0.0/12 ! -o docker0 -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s 172.16.0.0/12 ! -o docker0 -j MASQUERADE`

// dockerForGVisor sets what Docker needs in a gVisor box in its daemon.json,
// keeping the image's own settings: no iptables rules of its own (gVisor lacks the
// addrtype match), and no address announcements on new networks (gVisor can't send
// them). Compose files then work unchanged; Docker's default bridge doesn't.
func dockerForGVisor() error {
	const path = "/etc/docker/daemon.json"
	conf := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &conf); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	conf["iptables"], conf["ip6tables"] = false, false
	opts, _ := conf["default-network-opts"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	bridge, _ := opts["bridge"].(map[string]any)
	if bridge == nil {
		bridge = map[string]any{}
	}
	bridge["com.docker.network.advertise_addr_nmsgs"] = "0"
	opts["bridge"], conf["default-network-opts"] = bridge, opts
	data, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll("/etc/docker", 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// sshd waits for its host keys: hutch-guest makes them for every new box.
const sshdScript = `while [ ! -f /etc/ssh/ssh_host_ed25519_key ]; do sleep 0.5; done
exec /usr/sbin/sshd -D -e`

func (s *service) start() (int, error) {
	attr := &syscall.ProcAttr{
		Dir:   "/",
		Env:   append(baseEnv(s.uid), s.env...),
		Sys:   &syscall.SysProcAttr{Setsid: true},
		Files: []uintptr{0, 1, 2},
	}
	if s.uid != 0 {
		attr.Sys.Credential = &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.uid)}
		if u, err := user.LookupId(fmt.Sprint(s.uid)); err == nil {
			attr.Sys.Credential = credential(u)
		}
		attr.Dir = homeOf(s.uid)
	}
	switch {
	case s.tty != "":
		f, err := os.OpenFile(s.tty, os.O_RDWR, 0)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		attr.Files = []uintptr{f.Fd(), f.Fd(), f.Fd()}
		attr.Sys.Setctty = true
		attr.Dir = "/root"
	case s.log != "":
		f, err := os.OpenFile(s.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		null, _ := os.Open(os.DevNull)
		defer null.Close()
		attr.Files = []uintptr{null.Fd(), f.Fd(), f.Fd()}
	}
	return syscall.ForkExec(s.argv[0], s.argv, attr)
}

func writeStatus(all []*service) {
	var st []guestproto.ServiceStatus
	for _, s := range all {
		st = append(st, guestproto.ServiceStatus{Name: s.name, PID: s.pid, UID: s.uid, Since: s.since, Fails: s.fails})
	}
	data, _ := json.Marshal(st)
	if os.WriteFile(guestproto.InitStatusPath+".tmp", data, 0o644) == nil {
		os.Rename(guestproto.InitStatusPath+".tmp", guestproto.InitStatusPath)
	}
}

// backoff delays a failing service's next start: 1 s, doubling to a minute.
func (s *service) backoff(now time.Time) {
	s.fails++
	d := time.Second << min(s.fails-1, 6)
	s.next = now.Add(min(d, time.Minute))
}

// reap collects every exited child, supervised or not; supervised ones restart.
func reap(running map[int]*service) {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		s, ok := running[pid]
		if !ok {
			continue
		}
		delete(running, pid)
		s.pid = 0
		if s.argv == nil {
			continue // removed from the services file
		}
		log.Printf("%s exited (%s); restarting", s.name, describe(ws))
		if time.Since(s.since) > time.Minute {
			s.fails = 0
		}
		s.backoff(time.Now())
	}
}

func describe(ws syscall.WaitStatus) string {
	if ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return fmt.Sprintf("status %d", ws.ExitStatus())
}

// readServices parses the services file; lines already running keep their process.
func readServices(prev []*service) []*service {
	byLine := map[string]*service{}
	for _, s := range prev {
		if strings.HasPrefix(s.name, "service: ") {
			byLine[s.name] = s
		}
	}
	f, err := os.Open(guestproto.ServicesPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []*service
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := "service: " + line
		if s, ok := byLine[name]; ok {
			out = append(out, s)
			delete(byLine, name)
			continue
		}
		n := len(out) + 1
		out = append(out, &service{name: name, argv: []string{"/bin/sh", "-c", line}, uid: 1000,
			log: fmt.Sprintf("/var/log/hutch/service-%d.log", n)})
	}
	return out
}

// stopRemoved stops running services that are no longer listed.
func stopRemoved(running map[int]*service, all []*service) {
	keep := map[*service]bool{}
	for _, s := range all {
		keep[s] = true
	}
	for pid, s := range running {
		if !keep[s] {
			s.argv = nil
			syscall.Kill(-pid, syscall.SIGTERM)
		}
	}
}

func shutdown() {
	syscall.Kill(-1, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, _ := syscall.Wait4(-1, nil, syscall.WNOHANG, nil); pid < 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(-1, syscall.SIGKILL)
	syscall.Sync()
}

func baseEnv(uid int) []string {
	home := homeOf(uid)
	return []string{userPath(home), "HOME=" + home, "TERM=linux", "LANG=C.UTF-8"}
}

func homeOf(uid int) string {
	if uid == 0 {
		return "/root"
	}
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return "/"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ":")
		if len(p) >= 6 && p[2] == fmt.Sprint(uid) {
			return p[5]
		}
	}
	return "/"
}

func dockerd() string {
	for _, p := range []string{"/usr/bin/dockerd", "/usr/local/bin/dockerd"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// loginShell is bash when the image has it (Alpine and slim images may not).
func loginShell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// mountEssentials mounts what a kernel boot leaves out; a sandbox has most of it
// already, and anything mounted is left alone.
func mountEssentials(backend string) {
	unix.Mount("", "/", "", unix.MS_REMOUNT, "")
	mounts := []struct {
		target, fstype, data string
		flags                uintptr
	}{
		{"/proc", "proc", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"/sys", "sysfs", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"/dev", "devtmpfs", "mode=0755", unix.MS_NOSUID},
		{"/dev/pts", "devpts", "mode=0620,gid=5,ptmxmode=0666", unix.MS_NOSUID | unix.MS_NOEXEC},
		{"/dev/shm", "tmpfs", "mode=1777", unix.MS_NOSUID | unix.MS_NODEV},
		{"/run", "tmpfs", "mode=0755", unix.MS_NOSUID | unix.MS_NODEV},
		{"/sys/fs/cgroup", "cgroup2", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	}
	if backend == "gvisor" {
		mounts = mounts[:len(mounts)-1] // the sandbox's spec mounts gVisor's cgroups
	}
	mounted := mountpoints()
	for _, m := range mounts {
		if mounted[m.target] {
			continue
		}
		os.MkdirAll(m.target, 0o755)
		if err := unix.Mount(m.fstype, m.target, m.fstype, m.flags, m.data); err != nil {
			log.Printf("mount %s: %v", m.target, err)
		}
		if m.target == "/proc" {
			mounted = mountpoints()
		}
	}
	for link, target := range map[string]string{"/dev/fd": "/proc/self/fd", "/dev/stdin": "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1", "/dev/stderr": "/proc/self/fd/2", "/dev/ptmx": "pts/ptmx"} {
		if _, err := os.Lstat(link); err != nil {
			os.Symlink(target, link)
		}
	}
}

func mountpoints() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) > 4 {
			out[filepath.Clean(fields[4])] = true
		}
	}
	return out
}

func loopbackUp() {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
