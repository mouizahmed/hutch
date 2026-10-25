//go:build linux

package worker

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

// Every box sees the same guest network; only the netns and veth addresses differ per slot.
const (
	guestIP  = "169.254.0.22"
	tapIP    = "169.254.0.21"
	guestMAC = "02:FC:00:00:00:05"
	// The same in every netns: a restored guest still has the gateway's MAC from its
	// snapshot in its ARP cache, and a new one would cut it off until that entry expires.
	tapMAC    = "02:FC:00:00:00:01"
	bootIPArg = "ip=" + guestIP + "::" + tapIP + ":255.255.255.252::eth0:off"
	maxSlot   = 32766
)

// Ranges a box must never reach: the worker's LAN, other private networks, link-local
// (including cloud metadata at 169.254.169.254), and the slot networks themselves.
var blocked = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}

type slot int

func (s slot) netns() string    { return fmt.Sprintf("hutch%d", s) }
func (s slot) vethHost() string { return fmt.Sprintf("hv%d", s) }
func (s slot) vethPeer() string { return fmt.Sprintf("hp%d", s) }
func (s slot) hostIP() string   { return fmt.Sprintf("10.12.%d.%d", int(s)*2/256, int(s)*2%256) }
func (s slot) peerIP() string   { return fmt.Sprintf("10.12.%d.%d", (int(s)*2+1)/256, (int(s)*2+1)%256) }

// ReachIP is the worker-local address that is DNATed to the box's guest.
func (s slot) ReachIP() string { return fmt.Sprintf("10.11.%d.%d", int(s)/256, int(s)%256) }

// inputDrop stops a box from opening connections to the worker itself.
func (s slot) inputDrop() []string {
	return []string{"INPUT", "-i", s.vethHost(), "-m", "conntrack", "--ctstate", "NEW", "-j", "DROP"}
}

func (s slot) netnsPath() string { return "/var/run/netns/" + s.netns() }

func run(name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func uplink() (string, error) {
	out, err := exec.Command("ip", "-o", "route", "show", "default").Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1], nil
		}
	}
	return "", fmt.Errorf("no default route")
}

// hostNAT enables forwarding and masquerades all box traffic leaving the worker.
func hostNAT() error {
	up, err := uplink()
	if err != nil {
		return err
	}
	if err := run("sysctl", "-qw", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	rule := []string{"POSTROUTING", "-s", "10.12.0.0/16", "-o", up, "-j", "MASQUERADE"}
	if run("iptables", append([]string{"-t", "nat", "-C"}, rule...)...) == nil {
		return nil
	}
	return run("iptables", append([]string{"-t", "nat", "-A"}, rule...)...)
}

func runInput(input, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// Allowed targets can never be the slot networks (other boxes) or link-local.
var neverAllowed = []string{"10.11.0.0/16", "10.12.0.0/16", "169.254.0.0/16"}

// allowRules resolves host:port targets to ACCEPT rules; targets that don't resolve
// or fall in neverAllowed are skipped with a log line, and the box starts anyway.
func allowRules(allow []string) string {
	var rules strings.Builder
	for _, target := range allow {
		host, port, err := net.SplitHostPort(target)
		if err != nil {
			log.Printf("network.allow %q: %v", target, err)
			continue
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			log.Printf("network.allow %q: %v", target, err)
			continue
		}
		for _, ip := range ips {
			if ip.To4() == nil || inRanges(ip, neverAllowed) {
				continue
			}
			fmt.Fprintf(&rules, "-A FORWARD -i tap0 -d %s/32 -p tcp --dport %s -j ACCEPT\n", ip, port)
		}
	}
	return rules.String()
}

func inRanges(ip net.IP, cidrs []string) bool {
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func (s slot) setup(tapUID int, allow []string) error {
	if _, err := os.Stat(s.netnsPath()); err == nil {
		s.teardown()
	}
	ns := s.netns()

	hostLinks := fmt.Sprintf(`netns add %[1]s
link add %[2]s type veth peer name %[3]s
link set %[3]s netns %[1]s
addr add %[4]s/31 dev %[2]s
link set %[2]s up
`, ns, s.vethHost(), s.vethPeer(), s.hostIP())

	nsLinks := fmt.Sprintf(`link set lo up
tuntap add dev tap0 mode tap user %d
link set tap0 address %s
addr add %s/30 dev tap0
link set tap0 up
addr add %s/31 dev %s
link set %s up
route add default via %s
`, tapUID, tapMAC, tapIP, s.peerIP(), s.vethPeer(), s.vethPeer(), s.hostIP())

	nsRules := fmt.Sprintf(`*nat
-A POSTROUTING -o %s -s %s -j SNAT --to %s
-A PREROUTING -i %s -d %s -j DNAT --to %s
COMMIT
*filter
-A FORWARD -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
`, s.vethPeer(), guestIP, s.peerIP(), s.vethPeer(), s.ReachIP(), guestIP)
	nsRules += allowRules(allow)
	for _, cidr := range blocked {
		nsRules += fmt.Sprintf("-A FORWARD -i tap0 -d %s -j DROP\n", cidr)
	}
	nsRules += "COMMIT\n"

	hostRules := fmt.Sprintf("*filter\n-I FORWARD -i %[1]s -j ACCEPT\n-I FORWARD -o %[1]s -j ACCEPT\n-I %s\nCOMMIT\n",
		s.vethHost(), strings.Join(s.inputDrop(), " "))

	steps := []func() error{
		func() error { return runInput(hostLinks, "ip", "-batch", "-") },
		func() error { return runInput(nsLinks, "ip", "-n", ns, "-batch", "-") },
		func() error {
			return runInput(nsRules, "ip", "netns", "exec", ns, "sh", "-c", "sysctl -qw net.ipv4.ip_forward=1 && iptables-restore --noflush")
		},
		func() error { return run("ip", "route", "add", s.ReachIP()+"/32", "via", s.peerIP()) },
		func() error { return runInput(hostRules, "iptables-restore", "--noflush") },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			s.teardown()
			return err
		}
	}
	return nil
}

func (s slot) teardown() {
	rules := fmt.Sprintf("*filter\n-D FORWARD -i %[1]s -j ACCEPT\n-D FORWARD -o %[1]s -j ACCEPT\n-D %s\nCOMMIT\n",
		s.vethHost(), strings.Join(s.inputDrop(), " "))
	if runInput(rules, "iptables-restore", "--noflush") != nil {
		run("iptables", "-D", "FORWARD", "-i", s.vethHost(), "-j", "ACCEPT")
		run("iptables", "-D", "FORWARD", "-o", s.vethHost(), "-j", "ACCEPT")
		run("iptables", append([]string{"-D"}, s.inputDrop()...)...)
	}
	runInput(fmt.Sprintf("route del %s/32\nlink del %s\nnetns del %s\n", s.ReachIP(), s.vethHost(), s.netns()), "ip", "-force", "-batch", "-")
}
