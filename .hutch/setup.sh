# Go toolchain for building and testing Hutch.
go_version=1.27.3
go_sha256=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${go_version}.linux-amd64.tar.gz"
echo "${go_sha256}  /tmp/go.tgz" | sha256sum -c --quiet
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tgz
rm /tmp/go.tgz
ln -sf /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/

sudo -u dev go mod download
sudo -u dev go build ./...
