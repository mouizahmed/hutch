FROM ghcr.io/mouizahmed/hutch-default
ARG TARGETARCH
# Go toolchain for building and testing Hutch.
RUN set -e; case "$TARGETARCH" in \
      amd64) sum=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40 ;; \
      arm64) sum=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49 ;; \
    esac; \
    curl -fsSL --retry 5 -o /tmp/go.tgz "https://go.dev/dl/go1.27.3.linux-$TARGETARCH.tar.gz"; \
    echo "$sum  /tmp/go.tgz" | sha256sum -c --quiet; \
    tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz; \
    ln -sf /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/
