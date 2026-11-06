---
id: 48916d02a1024b009c11d2e3f4a50002
author: Lemon Mint
title: Running Docker, docker build, and docker compose Without Root or User Namespaces
description: How to run a complete Docker Engine (dockerd, BuildKit, overlay2, and docker compose) as an unprivileged user when root, sudo, unshare(CLONE_NEWUSER), mount(2), and /dev/fuse are blocked.
language: en
date: 2026-11-06T09:10:00Z
path: /blog/posts/run-docker-without-root-or-userns-gvx-z48916d02
go_package: z.gfire.dev/gvx
go_repourl: https://github.com/gfire-sigs/gvx.git
category: ops
---

## Quick Solution: Bootstrap Unprivileged Docker in Three Commands

If you are working inside a locked-down Linux container, CI runner, Kubernetes Pod, or AI agent sandbox where `root`, `sudo`, user namespaces (`unshare(CLONE_NEWUSER)`), `mount(2)`, and `/dev/fuse` are blocked, you can start a full `docker:dind` daemon in user space with **[gvx](https://github.com/gfire-sigs/gvx)** (`rungv`):

```bash
# 1. Install the static rungv binary into user space (no root required)
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -fsSL "https://github.com/gfire-sigs/gvx/releases/latest/download/rungv-linux-${ARCH}" -o ~/.local/bin/rungv
chmod +x ~/.local/bin/rungv

# 2. Launch a persistent Docker-in-Docker daemon backed by a single-file ext4lite volume
#    Creates ~/.cache/gvx/volumes/gvx-docker.ext4 and proxies ~/.gvx/docker.sock
~/.local/bin/rungv --docker-PersistVolume -d --name dind docker:dind

# 3. Point the standard Docker CLI and Docker Compose at the user-space socket
export DOCKER_HOST="unix://$HOME/.gvx/docker.sock"
docker info
docker build -t app:test .
docker run --rm app:test
docker compose up -d
```

If the `rungv` release binary is not yet cached locally and `go` (1.25+) is installed in the environment, you can build `rungv` directly from source:

```bash
git clone --depth 1 https://github.com/gfire-sigs/gvx.git /tmp/gvx
CGO_ENABLED=0 go -C /tmp/gvx build -o ~/.local/bin/rungv ./cmd/rungv
```

---

## Why Standard Rootless Tools Fail in Restricted Sandboxes

When an engineer or autonomous coding agent needs to run `docker build`, `docker run`, or `docker compose` inside a hardened container or multi-tenant runner, standard workarounds fail at five distinct kernel barriers.

### 1. No Host Docker Daemon Socket

Running `docker ps` or `docker build` fails immediately when the host `/var/run/docker.sock` is not mounted into the environment:

```text
Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?
```

### 2. `PR_SET_NO_NEW_PRIVS` Blocks `sudo` and Setuid Helpers

Hardened containers (`securityContext.allowPrivilegeEscalation: false` in Kubernetes, `--security-opt=no-new-privileges` in Docker, or `exebox` / `bwrap` sandboxes) set the Linux `PR_SET_NO_NEW_PRIVS` bit on the process tree. Attempting to escalate via `sudo` or setuid binaries (`newuidmap`, `newgidmap`, `fusermount3`) fails:

```text
sudo: The "no new privileges" flag is set, which prevents sudo from running as root.
sudo: effective uid is not 0, is /usr/bin/sudo on a file system with the 'nosuid' option set or an NFS file system without root privileges?
```

### 3. Seccomp and Sysctl Policies Block `unshare(CLONE_NEWUSER)`

Tools marketed as "rootless" (`Rootless Docker`, `Podman`, `Buildah`, `nerdctl`, `bubblewrap`) still rely on Linux **user namespaces** (`CLONE_NEWUSER`). They fail whenever:

- The outer container's default seccomp profile blocks `unshare(2)` or `clone(CLONE_NEWUSER)`.
- Ubuntu 24.04+ enables `kernel.apparmor_restrict_unprivileged_userns = 1`.
- Debian or RHEL nodes set `kernel.unprivileged_userns_clone = 0` or `user.max_user_namespaces = 0`.
- `newuidmap` cannot write `/proc/self/uid_map` because `PR_SET_NO_NEW_PRIVS` is active.

The resulting errors look like this:

```text
unshare: unshare failed: Operation not permitted
cannot clone: Operation not permitted
Error: cannot re-exec process to join the existing user namespace
newuidmap: write to uid_map failed: Operation not permitted
RootlessKit: parent: failed to start the child: fork/exec /proc/self/exe: operation not permitted
```

### 4. `mount(2)` Is Blocked Without Host `CAP_SYS_ADMIN`

Even if an environment permits `CLONE_NEWUSER`, mounting a fresh `/proc` inside a nested mount namespace fails if the outer container has masked `/proc` subtrees (`/proc/acpi`, `/proc/kcore`, `/proc/keys`) or dropped `mount(2)` in seccomp:

```text
mount("/proc", "/proc", "proc", MS_NOSUID|MS_NODEV|MS_NOEXEC, NULL): Operation not permitted
OCI runtime create failed: runc create failed: unable to start container process: error during container init: error mounting "proc" to rootfs at "/proc": mount proc:/proc (via /proc/self/fd/6), flags: 0xe: operation not permitted
bwrap: Creating new namespace failed: Operation not permitted
```

### 5. Missing `/dev/fuse` Breaks `fuse-overlayfs`

Rootless container engines that cannot mount kernel `overlayfs` fall back to `fuse-overlayfs`. In minimal containers or Kubernetes pods without `/dev/fuse` device passthrough, `fuse-overlayfs` fails immediately:

```text
fuse: device not found, try 'modprobe fuse' first
fusermount3: failed to open /dev/fuse: No such file or directory
error initializing graphdriver: driver not supported: fuse-overlayfs
```

---

## How `gvx` Runs `docker:dind` Completely in User Space

`gvx` bypasses all five kernel barriers because it does not ask the host Linux kernel to create namespaces, mount filesystems, or configure network interfaces. Instead, `rungv` runs an internal user-space Linux kernel (the gVisor `Sentry`) and a user-space network stack (`netstack`) inside one ordinary unprivileged process.

![Running Unprivileged Docker-in-Docker with gvx](/assets/images/gvx/gvx-unprivileged-dind.svg)

```d2
direction: down

host_sandbox: "Restricted Host Sandbox (uid=1000, no_new_privs=1, unshare=EPERM, no /dev/fuse)" {
  blocked: "Blocked Traditional Container Paths" {
    err1: "Host dockerd: /var/run/docker.sock missing"
    err2: "sudo / setuid: blocked by PR_SET_NO_NEW_PRIVS"
    err3: "Rootless Docker / Podman: unshare(CLONE_NEWUSER) -> EPERM"
    err4: "runc / bwrap: mount(/proc) -> EPERM"
    err5: "fuse-overlayfs: /dev/fuse missing"
  }

  cli: "Unmodified Host Docker CLI" {
    cmd: "docker build / docker run / docker compose up"
    sock: 'DOCKER_HOST=unix://$HOME/.gvx/docker.sock'
    cmd -> sock
  }

  rungv: "Single Unprivileged Process: rungv (--docker-PersistVolume)" {
    proxy: "Unix Socket Proxy -> Guest 127.0.0.1:2375"

    guest: "Guest Linux User Space (Virtual UID 0, Full Virtual Capabilities)" {
      dockerd: "dockerd (storage-driver=overlay2, iptables-legacy)"
      containerd: "containerd + runc + BuildKit"
      inner: "Nested Workload Containers"
      dockerd -> containerd -> inner
    }

    sentry: "gvx User-Space Kernel (Sentry)" {
      ext4: "ext4lite: /var/lib/docker on ~/.cache/gvx/volumes/gvx-docker.ext4"
      net: "netstack: docker0 Bridge + veth Pairs + NAT Chains"
      vfs: "In-Memory procfs, sysfs, cgroupfs Mounts"
    }

    proxy -> guest.dockerd: "Forwards Docker API"
    guest.dockerd -> sentry.ext4: "Native overlay2 upperdir/workdir"
    guest.dockerd -> sentry.net: "Bridge + iptables-legacy rules"
    guest.containerd -> sentry.vfs: "Unprivileged guest mount(2)"
  }

  host_reqs: "Minimal Host Kernel Primitives Required by rungv" {
    req1: "1. ptrace(PTRACE_ATTACH) once at startup on self-child"
    req2: "2. seccomp(SECCOMP_SET_MODE_FILTER)"
    req3: "3. mmap(PROT_EXEC)"
  }

  cli.sock -> rungv.proxy: "Connects without root"
  rungv -> host_reqs: "Uses only 3 unprivileged primitives"
}
```

### 1. Virtual `UID 0` and Virtual Mount Table Inside the Sentry

When `dockerd`, `containerd`, and `runc` execute inside `rungv`, their system calls are intercepted by `systrap` and handled by the in-process `Sentry` kernel:

- `getuid()` and `geteuid()` return `0` inside the guest while the host `rungv` process runs under your unprivileged host UID (for example, `uid=1000`).
- `unshare(2)`, `clone(2)`, `pivot_root(2)`, `chroot(2)`, and `mount(2)` manipulate the Sentry's in-memory `VFS2` mount table (`procfs`, `sysfs`, `tmpfs`, `devpts`, `cgroupfs` v2) without invoking any host kernel namespace or mount syscall.

### 2. Native `overlay2` on Single-File `ext4lite` Volumes

When you pass `--docker-PersistVolume` (or explicitly mount `-v my-docker.ext4:/var/lib/docker`), `gvx` mounts its built-in **`ext4lite`** filesystem at `/var/lib/docker`.

Because `ext4lite` implements a genuine `ext4` filesystem inside a single host file (`~/.cache/gvx/volumes/gvx-docker.ext4`) with support for `trusted.overlay.*` extended attributes, character whiteout devices (`0:0`), `RENAME_EXCHANGE`, and `fsuid`/`fsgid` inode ownership (`inode_init_owner`), `dockerd` uses its native **`overlay2`** storage driver inside the guest. Multi-stage `docker build` operations share Copy-on-Write layers without `/dev/fuse` and without the disk-space explosion of Docker's `vfs` fallback driver.

### 3. User-Space `docker0` Bridge and `iptables-legacy`

When `docker:dind` starts inside `gvx`:

- `rungv` automatically sets `GVX_DOCKER_IPTABLES=legacy` so the entrypoint configures `iptables-legacy` inside the container.
- Guest `dockerd` creates the `docker0` bridge (`172.17.0.1/16`), attaches virtual `veth` pairs for nested containers, and programs `NAT` (`DOCKER`, `POSTROUTING`) and `FILTER` chains inside `gvx`'s user-space `netstack`.
- When `--docker-PersistVolume` is enabled, `rungv` binds a host Unix domain socket at `~/.gvx/docker.sock` and proxies connections directly to `127.0.0.1:2375` inside the guest network namespace.

---

## Comparison with Existing Container Tools

| Capability / Constraint                                      | Standard Docker (`dockerd`)  | Rootless Docker / Podman  | Kaniko / Buildah (Unprivileged) | `gvx` (`rungv` + `docker:dind`)    |
| :----------------------------------------------------------- | :--------------------------- | :------------------------ | :------------------------------ | :--------------------------------- |
| **Host `root` or `sudo` required**                           | Yes                          | No                        | No                              | **No**                             |
| **Works with `PR_SET_NO_NEW_PRIVS=1`**                       | No                           | No (`newuidmap` fails)    | Partial                         | **Yes**                            |
| **Works when `unshare(CLONE_NEWUSER)` is blocked (`EPERM`)** | N/A                          | No                        | Buildah: No / Kaniko: Yes       | **Yes**                            |
| **Works without `/dev/fuse`**                                | Yes (uses kernel `overlay2`) | Only via slow `vfs` copy  | Yes                             | **Yes (`overlay2` on `ext4lite`)** |
| **Supports `docker build` (BuildKit)**                       | Yes                          | Yes (if `userns` allowed) | Image build only (no runtime)   | **Yes**                            |
| **Supports `docker run` and `docker compose up`**            | Yes                          | Yes (if `userns` allowed) | **No**                          | **Yes**                            |
| **Runs on Windows and macOS without a Linux VM**             | No (requires WSL2 / VM)      | No (requires Linux VM)    | No                              | **Yes (`whpx`, `hvf`, `systrap`)** |

---

## Exact Host Kernel Requirements on Linux

`gvx` requires zero elevated capabilities (`CapEff: 0000000000000000`) and zero user namespaces. On a Linux host, `rungv` relies on only three standard unprivileged kernel primitives:

1. **`ptrace(PTRACE_ATTACH)` on its own child thread once during initialization**: `systrap` spawns a local stub helper thread at startup, attaches to it to initialize the shared-memory context, and detaches. If the host sets `kernel.yama.ptrace_scope = 3` (which disables all `ptrace` system-wide) or blocks `ptrace` in an outer seccomp filter, `systrap` initialization cannot complete. (Under `yama.ptrace_scope = 1`, the default on Ubuntu and Debian, `gvx` succeeds because parent-to-child tracing is permitted.)
2. **`seccomp(SECCOMP_SET_MODE_FILTER)`**: Used to trap guest system calls from container stub threads. As covered in [Part 3 of this series](/blog/posts/docker-in-docker-inside-gvisor-with-gvx-z48916d03), `gvx` works both when `SECCOMP_FILTER_FLAG_NEW_LISTENER` is available and when it returns `EBUSY` inside a nested sandbox.
3. **`mmap(..., PROT_EXEC, ...)`**: Used to map guest ELF segments and the `systrap` shared-memory trampoline.

---

## End-to-End Example: Building and Testing a Multi-Container App Without Root

The following script runs a complete `docker build` and two-container integration test (`redis` + application container) inside an unprivileged user session:

```bash
# Start persistent unprivileged docker:dind if not already running
rungv --docker-PersistVolume -d --name dind docker:dind
export DOCKER_HOST="unix://$HOME/.gvx/docker.sock"

# Wait until dockerd is ready
until docker info >/dev/null 2>&1; do sleep 0.5; done

# Create a minimal Dockerfile and test script
mkdir -p /tmp/demo-app && cd /tmp/demo-app
cat <<'EOF' > Dockerfile
FROM alpine:3.20
RUN apk add --no-cache redis
CMD ["redis-cli", "-h", "cache", "ping"]
EOF

# Build the image using overlay2 on ext4lite
docker build -t demo-ping:latest .

# Create a bridge network, start Redis, and verify connectivity between containers
docker network create demo-net
docker run -d --name cache --network demo-net redis:7-alpine
docker run --rm --network demo-net demo-ping:latest
# Output: PONG

# Clean up workload containers (or stop the entire dind instance with: rungv kill dind)
docker rm -f cache
docker network rm demo-net
```

---

## Further Reading in This Series

1. **[gvx: Running Linux Containers, Docker, and Kubernetes in One Unprivileged Process](/blog/posts/gvx-cross-platform-linux-containers-z48916d01)**: Architectural overview of `gvx`, its cross-platform execution engines (`whpx`, `hvf`, `systrap`), `VFS2` (`erofs`, `ext4lite`, `hostfs` v2), and `netstack`.
2. **[Running Docker, docker build, and docker compose Without Root or User Namespaces](/blog/posts/run-docker-without-root-or-userns-gvx-z48916d02)** (this post)
3. **[Running Docker-in-Docker Inside gVisor and Nested Sandboxes with gvx](/blog/posts/docker-in-docker-inside-gvisor-with-gvx-z48916d03)**: How `gvx` solves `modprobe`, `iptables-nft`, `overlay2` on `gofer`, and nested `SECCOMP_FILTER_FLAG_NEW_LISTENER` (`EBUSY`) inside `gVisor` (`runsc`).
4. **[Fast Isolated Docker Environments for AI Agent RL Rollouts with gvx](/blog/posts/fast-docker-environments-for-agent-rl-rollouts-z48916d04)**: Scaling parallel `SWE-bench` and RL rollout environments with 100-millisecond copy-on-write `.ext4` volume resets.
