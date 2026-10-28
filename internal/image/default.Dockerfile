# Hutch's default image: projects without a Dockerfile use it, and most project
# Dockerfiles start FROM it. Hutch's layer (hutch-guest, its init, the dev user) is
# added on top of whatever image a template uses.
FROM ubuntu:24.04@sha256:24737cef17f4e22b1977df7d9a05e77c4e28b7da5b8911f8d8ccda136bf3573c
ARG TARGETARCH

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl git tmux ncurses-term sudo openssh-server less xz-utils unzip \
      build-essential pkg-config iproute2 iputils-ping procps \
 && rm -rf /var/lib/apt/lists/*

# Downloads that stall are retried rather than waited on (nodejs.org does, at times).
RUN set -e; fetch() { curl -fsSL --retry 5 --retry-all-errors --speed-limit 50000 --speed-time 20 -o "$3" "$1"; echo "$2  $3" | sha256sum -c --quiet; }; \
    case "$TARGETARCH" in \
      amd64) node=x64 nodesum=6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6 \
             gh=amd64 ghsum=bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386 \
             uv=x86_64 uvsum=b0828fc4cdc47d65951e94b5f0a15a5364420e8dcd88723f3b21e7a2eb30bbc3 ;; \
      arm64) node=arm64 nodesum=dbabf7a1cbf947e6059ffad2c27b80041db97dea1e553b62afdee7f13fca336e \
             gh=arm64 ghsum=7862c86c72f43df3a2d93ddde6f473285b4e2af61b494849846827e513ef6484 \
             uv=aarch64 uvsum=281ca6cf43f4e26e35474839ad4861be86de42af2682f74134460f5b2071b96d ;; \
    esac; \
    fetch https://nodejs.org/dist/v24.22.1/node-v24.22.1-linux-$node.tar.xz $nodesum /tmp/node.tar.xz; \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE'; \
    fetch https://github.com/cli/cli/releases/download/v2.102.0/gh_2.102.0_linux_$gh.tar.gz $ghsum /tmp/gh.tar.gz; \
    tar -xzf /tmp/gh.tar.gz -C /tmp && install -m755 /tmp/gh_2.102.0_linux_$gh/bin/gh /usr/local/bin/gh; \
    fetch https://github.com/astral-sh/uv/releases/download/0.13.0/uv-$uv-unknown-linux-musl.tar.gz $uvsum /tmp/uv.tar.gz; \
    tar -xzf /tmp/uv.tar.gz -C /tmp && install -m755 /tmp/uv-$uv-unknown-linux-musl/uv /tmp/uv-$uv-unknown-linux-musl/uvx /usr/local/bin/; \
    rm -rf /tmp/*
