package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/provider"
	"github.com/mouizahmed/hutch/internal/provider/aws"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/server"
	"github.com/mouizahmed/hutch/internal/sigv4"
)

const serverDiskGB = 20

// awsInit creates a Hutch server in an AWS account from nothing: a key pair, a
// security group (SSH only from here), a bucket, the IAM role the server and
// workers reach it with, and the server instance; then it installs the server,
// sets the recovery passphrase, and connects AWS for workers.
func awsInit(ctx context.Context, serverType, binary string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("this needs a terminal: keys and the recovery passphrase are typed")
	}
	arch := "amd64"
	if family, _, _ := strings.Cut(serverType, "."); strings.HasSuffix(family, "g") {
		arch = "arm64"
	}
	if binary == "" {
		exe, _ := os.Executable()
		binary = filepath.Join(filepath.Dir(exe), "linux-"+arch, "hutch")
	}
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("the server program for linux/%s isn't at %s (build it with make linux, or pass --server-binary)", arch, binary)
	}

	cfg := aws.Config{Region: "us-east-1", Fleet: "f-" + hexID(8)}
	err := huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Hutch on AWS").Description("Creates a server instance, a bucket, a security group, and an IAM role in your account.\nThe keys need EC2, S3, and IAM access; they stay on the server, to start workers."),
		huh.NewInput().Title("Region").Value(&cfg.Region),
		huh.NewInput().Title("AWS access key").Value(&cfg.Credentials.AccessKey),
		huh.NewInput().Title("AWS secret key").EchoMode(huh.EchoModePassword).Value(&cfg.Credentials.SecretKey),
	)).Run()
	if err != nil {
		return err
	}
	cfg.Region, cfg.Credentials.AccessKey = strings.TrimSpace(cfg.Region), strings.TrimSpace(cfg.Credentials.AccessKey)
	a, err := aws.New(cfg)
	if err != nil {
		return err
	}
	fmt.Println("Checking the account...")
	account, err := a.Account(ctx)
	if err != nil {
		return err
	}
	if _, _, err := a.WorkerImage(ctx); err != nil {
		return err
	}
	types, err := a.InstanceTypes(ctx)
	if err != nil {
		return err
	}
	serverPrice, err := a.Price(ctx, serverType)
	if err != nil {
		return err
	}
	rates, err := a.Rates(ctx)
	if err != nil {
		return err
	}

	workerType := "m8i.2xlarge"
	var typeOpts []huh.Option[string]
	for _, t := range types {
		label := fmt.Sprintf("%-12s %2d vCPU  %4d GB   $%.3f/h", t.Name, t.VCPUs, t.MemMiB/1024, t.PerHour)
		typeOpts = append(typeOpts, huh.NewOption(label, t.Name))
	}
	bucketName := fmt.Sprintf("hutch-%s-%s", account, hexID(2))
	var pass, again string
	ok := false
	serverMonth := serverPrice*730 + float64(serverDiskGB)*rates.DiskGBMonth + rates.IPv4Hour*730
	estimate := func() string {
		var w provider.InstanceType
		for _, t := range types {
			if t.Name == workerType {
				w = t
			}
		}
		stopped := float64(100+12) * rates.DiskGBMonth
		return fmt.Sprintf("Always on: the server (%s) about $%.2f a month.\nWorkers (%s): $%.3f an hour each while running; a stopped one keeps its disk for about $%.2f a month.\nThe bucket: $%.3f per GB a month.",
			serverType, serverMonth, workerType, w.PerHour+rates.IPv4Hour, stopped, rates.BucketGBMonth)
	}
	err = huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().Title("Worker instance type").
			Description("Every elastic worker is this type; boxes pack onto it. Change it later with providers.aws.worker_type.").
			Options(typeOpts...).Value(&workerType)),
		huh.NewGroup(huh.NewInput().Title("Bucket").Description("A new bucket for snapshots and backups, all encrypted.").Value(&bucketName)),
		huh.NewGroup(
			huh.NewInput().Title("Recovery passphrase").Description(fmt.Sprintf("At least %d characters. Unlocks the bucket on a new server if this one is lost. Hutch never stores it.", server.MinPassphrase)).
				EchoMode(huh.EchoModePassword).Value(&pass).Validate(func(s string) error {
				if len(s) < server.MinPassphrase {
					return fmt.Errorf("at least %d characters", server.MinPassphrase)
				}
				return nil
			}),
			huh.NewInput().Title("Passphrase again").EchoMode(huh.EchoModePassword).Value(&again).Validate(func(s string) error {
				if s != pass {
					return errors.New("doesn't match")
				}
				return nil
			})),
		huh.NewGroup(huh.NewConfirm().Title("Create it?").DescriptionFunc(estimate, &workerType).
			Affirmative("Yes, I've stored the passphrase").Negative("Cancel").Value(&ok)),
	).Run()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("cancelled")
	}

	name := "hutch-" + cfg.Fleet
	key, err := sshKey()
	if err != nil {
		return err
	}
	myIP, err := publicIP(ctx)
	if err != nil {
		return err
	}
	step := func(s string) { fmt.Println("==> " + s) }
	step("network")
	subnet, vpc, err := a.DefaultSubnet(ctx)
	if err != nil {
		return err
	}
	if err := a.ImportKey(ctx, name, key); err != nil {
		return err
	}
	group, err := a.CreateGroup(ctx, name, vpc, myIP, "7443")
	if err != nil {
		return fmt.Errorf("security group: %w", err)
	}
	step("bucket " + bucketName)
	bcfg := bucket.Config{Region: cfg.Region, Name: bucketName, AccessKey: cfg.Credentials.AccessKey, SecretKey: cfg.Credentials.SecretKey}
	b, err := bucket.New(bcfg)
	if err != nil {
		return err
	}
	if err := b.Create(ctx); err != nil {
		return err
	}
	if err := b.Tag(ctx, map[string]string{provider.TagFleet: cfg.Fleet}); err != nil {
		return err
	}
	step("IAM role " + name)
	if err := a.CreateRole(ctx, account, name, bucketName); err != nil {
		return err
	}
	cfg.SubnetID, cfg.SecurityGroupID, cfg.InstanceProfile = subnet, group, name
	if a, err = aws.New(cfg); err != nil {
		return err
	}
	step("server instance (" + serverType + ")")
	image, err := a.UbuntuImage(ctx, arch)
	if err != nil {
		return err
	}
	node, err := a.LaunchServer(ctx, aws.ServerSpec{Name: name + "-server", Type: serverType, Image: image, KeyName: name, DiskGB: serverDiskGB})
	if err != nil {
		return err
	}
	for node.PublicIP == "" || node.State != provider.Running {
		time.Sleep(3 * time.Second)
		if node, err = a.Node(ctx, node.ID); err != nil {
			return err
		}
	}
	target := "ubuntu@" + node.PublicIP
	ssh := func(stdin io.Reader, stderr io.Writer, script string) error {
		args := append(client.SSHArgs(), "-o", "StrictHostKeyChecking=accept-new", "-o", "LogLevel=ERROR", "-o", "ConnectTimeout=5", target, script)
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Stdin, cmd.Stderr = stdin, stderr
		return cmd.Run()
	}
	sshRun := func(stdin io.Reader, script string) error { return ssh(stdin, os.Stderr, script) }
	step("install the server on " + node.PublicIP)
	for i := 0; ; i++ {
		if err := ssh(nil, io.Discard, "true"); err == nil {
			break
		} else if i > 60 {
			return fmt.Errorf("can't reach %s over SSH: %w", target, err)
		}
		time.Sleep(5 * time.Second)
	}
	f, err := os.Open(binary)
	if err != nil {
		return err
	}
	err = sshRun(f, "sudo tee /usr/local/bin/hutch >/dev/null && sudo chmod 755 /usr/local/bin/hutch")
	f.Close()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Hutch server
After=network-online.target

[Service]
User=ubuntu
ExecStart=/usr/local/bin/hutch server run --listen 0.0.0.0:7443 --name %s --name %s
Restart=always

[Install]
WantedBy=multi-user.target
`, node.PrivateIP, node.PublicIP)
	if err := sshRun(strings.NewReader(unit), "sudo tee /etc/systemd/system/hutch-server.service >/dev/null && sudo systemctl daemon-reload && sudo systemctl enable --now -q hutch-server && for i in $(seq 50); do [ -S ~/.local/state/hutch/server/api.sock ] && exit 0; sleep 0.2; done; exit 1"); err != nil {
		return fmt.Errorf("start the server: %w", err)
	}

	step("bucket and recovery passphrase")
	sess, err := client.Dial(target, "")
	if err != nil {
		return err
	}
	defer sess.Close()
	role := bucket.Config{Region: cfg.Region, Name: bucketName, InstanceRole: true}
	if err := rpc.Call(ctx, sess, api.ServerInit, api.ServerInitParams{Bucket: role, Passphrase: pass}, nil); err != nil {
		return err
	}
	step("AWS for workers")
	var info api.ProviderInfo
	if err := rpc.Call(ctx, sess, api.ProviderAdd, api.ProviderParams{Credentials: sigv4.Credentials{AccessKey: cfg.Credentials.AccessKey,
		SecretKey: cfg.Credentials.SecretKey}, InstanceProfile: name, Fleet: cfg.Fleet, ServerDiskGB: serverDiskGB}, &info); err != nil {
		return err
	}
	if err := sshRun(nil, "hutch config set --server providers.aws.worker_type "+workerType); err != nil {
		return err
	}
	if err := setUserServer(target); err != nil {
		return err
	}
	fmt.Printf("\nHutch is running on %s (%s). This machine now uses it: server = %q in %s.\n", node.PublicIP, node.ID, target, config.UserPath())
	fmt.Printf("SSH reaches it only from %s; from elsewhere, add your address to security group %s.\n", myIP, group)
	fmt.Println("Next: cd into a repository and run hutch init.")
	return nil
}

func hexID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sshKey returns Hutch's SSH public key, creating the key pair the first time.
func sshKey() ([]byte, error) {
	path := client.SSHKeyPath()
	if pub, err := os.ReadFile(path + ".pub"); err == nil {
		return pub, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hutch")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	out := ssh.MarshalAuthorizedKey(sp)
	return out, os.WriteFile(path+".pub", out, 0o644)
}

// publicIP is this machine's address as AWS sees it, for the SSH rule.
func publicIP(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("couldn't tell this machine's public address (%q)", ip)
	}
	return ip, nil
}

// setUserServer points this machine's user config at the new server.
func setUserServer(target string) error {
	path := config.UserPath()
	text, err := readOptional(path)
	if err != nil {
		return err
	}
	p, _ := config.SplitPath("server")
	if text, err = config.Set(withVersion(text), p, target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, text)
}
