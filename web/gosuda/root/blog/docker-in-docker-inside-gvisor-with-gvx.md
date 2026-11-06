---
author: Lemon Mint
title: Running Docker-in-Docker Inside gVisor and Nested Sandboxes with gvx
description: Why installing docker.io or nesting runsc inside a gVisor container fails across modprobe, iptables-nft, overlay2, cgroups, and seccomp EBUSY, and how gvx securely runs nested docker:dind inside an isolated user-space kernel without --privileged.
language: en
path: /blog/posts/docker-in-docker-inside-gvisor-with-gvx-z48916d03
go_package: z.gfire.dev/gvx
go_repourl: https://github.com/gfire-sigs/gvx.git
category: lowlevel
---

## Quick Solution: Securely Running `docker:dind` Inside an Unprivileged `gVisor` (`runsc`) Sandbox

Cloud AI agent platforms, Kubernetes `agent-sandbox` deployments (`RuntimeClass: gvisor`), and reinforcement learning evaluation harnesses isolate untrusted workloads inside **gVisor (`runsc`)** containers specifically to protect the host kernel. When a workload inside a default unprivileged `runsc` container tries to install `docker.io` or `docker-ce` via `apt-get` and start `dockerd`, it fails even if it has `root` (`UID 0`) inside the container.

With **[gvx](https://github.com/gfire-sigs/gvx)** (`rungv`), you can **securely** run `docker:dind`, `docker build`, and `docker compose` directly inside an unprivileged `gVisor` (`runsc`) container (or nest `gvx` inside `gvx`) **without weakening the outer `RuntimeClass`, exposing host sockets, or granting `--privileged`**.

> **Preserving and Strengthening Outer Sandbox Isolation**: Running `rungv` inside `runsc` does not bypass a single restriction enforced by the outer `gVisor` sandbox. Instead, `gvx` creates a second, nested user-space gVisor `Sentry` kernel with its own `hostlockdown` (`PR_SET_NO_NEW_PRIVS` + `seccomp`), its own in-memory `netstack`, and a single-file `ext4lite` volume. Inner `dockerd` and `runc` containers are double-sandboxed: their system calls, mounts, and raw packets never leave the inner `gvx` user-space kernel.

```sh
# Inside an existing gVisor (runsc) container or Kubernetes Pod:
mkdir -p ~/.gvx
rungv run \
  -p unix:~/.gvx/docker.sock:2375 \
  -v gvx-docker:/var/lib/docker \
  -e DOCKER_TLS_CERTDIR= \
  docker:dind --tls=false &

# Run standard Docker builds and containers against the inner gvx socket
export DOCKER_HOST="unix://$HOME/.gvx/docker.sock"
until docker info >/dev/null 2>&1; do sleep 0.5; done
docker info --format '{{.Driver}} {{.OperatingSystem}}'
# Output: overlayfs Alpine Linux ... (containerized)

docker build -t nested-app:test .
docker run --rm nested-app:test
```

---

## Five Reasons Why Direct Docker Installation Fails Inside `gVisor` (`runsc`)

When a user or AI coding agent runs `apt-get install -y docker.io` (or `docker-ce`) inside a standard `runsc` container and attempts to start `dockerd`, it hits five sequential technical barriers.

### 1. Kernel Module Loading (`modprobe`) Against Virtual Kernel `4.4.0`

During package installation and `dockerd` startup, initialization scripts run `modprobe` to load `overlay`, `br_netfilter`, `ip_tables`, `nf_nat`, and `xt_conntrack`. The outer gVisor Sentry blocks `init_module(2)` and `finit_module(2)` and reports a virtual kernel version (`4.4.0`) that has no `/lib/modules/4.4.0` directory:

```text
modprobe: FATAL: Module overlay not found in directory /lib/modules/4.4.0
modprobe: FATAL: Module br_netfilter not found in directory /lib/modules/4.4.0
modprobe: FATAL: Module ip_tables not found in directory /lib/modules/4.4.0
modprobe: FATAL: Module nf_nat not found in directory /lib/modules/4.4.0
```

### 2. `iptables-nft` (`nf_tables` Netlink) and `docker0` Bridge Failures

On Ubuntu 22.04, Ubuntu 24.04, and Debian 11/12, `apt-get install docker.io` selects `iptables-nft` (backed by the `nf_tables` netlink subsystem) as the default `/usr/sbin/iptables` alternative. In a standard gVisor container without network admin privileges or `nf_tables` netlink support, `dockerd` crashes while initializing the `DOCKER` NAT chain and the `docker0` bridge:

```text
failed to start daemon: Error initializing network controller: error obtaining controller instance: failed to create NAT chain DOCKER: iptables failed: iptables -t nat -N DOCKER
iptables v1.8.10 (nf_tables): Could not fetch rule set generation id: Invalid argument
iptables v1.8.10 (legacy): can't initialize iptables table 'nat': Table does not exist (do you need to insmod?)
iptables: No chain/target/match by that name.
Unable to enable SKIP DNAT rule: (iptables failed: iptables --wait -t nat -I DOCKER -i docker0 -j RETURN: iptables: No chain/target/match by that name.)
Error initializing network controller: error obtaining controller instance: failed to create bridge "docker0": operation not permitted
```

### 3. `overlay2` Failure on `gofer` / `9p` Rootfs and `vfs` Disk Exhaustion

Unless the outer `runsc` administrator explicitly pre-configured a dedicated `tmpfs` or filestore mount at `/var/lib/docker`, the container rootfs is backed by gVisor's `gofer` (`9p`/`lisafs`) or a nested overlay mount. Mounting `overlay2` on top of `/var/lib/docker` fails:

```text
level=error msg="[graphdriver] prior storage driver overlay2 failed: driver not supported"
failed to mount overlay: invalid argument
Error starting daemon: error initializing graphdriver: driver not supported: overlay2
```

When users or agents attempt to bypass this error by starting `dockerd --storage-driver=vfs`, Docker disables Copy-on-Write layer sharing completely. Every `RUN` instruction in a `Dockerfile` performs a full recursive copy (`dirCopy`) of the entire root filesystem, multiplying a 500 MB image across 10 build steps into 5 GB of writes and terminating the sandbox with `no space left on device` or an I/O timeout.

### 4. Nested `runc` Cgroup and `/proc` Mount Rejections

Even if `dockerd` is forced to start with `dockerd --iptables=false --bridge=none --storage-driver=vfs`, running `docker run` or `docker build` invokes `runc` to create the inner container. In a default unprivileged gVisor container, `runc` fails when mounting cgroups, remounting `/proc`, or loading inner seccomp filters:

```text
docker: Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process:
error during container init: error mounting "cgroup" to rootfs at "/sys/fs/cgroup": mount cgroup:/sys/fs/cgroup/cpuset (via /proc/self/fd/6), flags: 0xf, data: cpuset: operation not permitted
cgroups: cgroup mountpoint does not exist: unknown
runc create failed: systemd not running on this host: cannot use systemd cgroup manager
error during container init: error mounting "proc" to rootfs at "/proc": mount proc:/proc (via /proc/self/fd/6), flags: 0xe: operation not permitted
error during container init: error loading seccomp filter into kernel: operation not permitted
```

### 5. Nested `runsc` Fails with `SECCOMP_FILTER_FLAG_NEW_LISTENER` (`EBUSY`)

If a user tries to run another `runsc` instance inside `runsc` using the `systrap` platform, initialization aborts immediately:

```text
creating subprocess: sysmsg: failed to initialize thread: failed to install seccomp notify rules: device or resource busy
```

Both the Linux kernel and the gVisor Sentry allow **at most one** `SECCOMP_FILTER_FLAG_NEW_LISTENER` (`SECCOMP_RET_USER_NOTIF`) listener per task hierarchy. Because the outer sandbox already holds that listener slot, the inner `seccomp(2)` call returns `EBUSY` (`device or resource busy`).

---

## How `gvx` Solves Nested Execution Inside `gVisor`

`gvx` addresses all five barriers inside its own user-space kernel and platform layer, enabling `rungv` to run inside an outer `gVisor` (`runsc`) container or even recursively inside another `gvx` container (`gvx` in `gvx`).

![Nested gvx Execution Inside Outer gVisor](/assets/images/gvx/gvx-nested-seccomp-futex.svg)

### 1. Automatic `systrap` Fallback from `EBUSY` to the `_RUN_SYSCALL_LOOP` Futex Loop

In upstream gVisor, `systrap` has two ways for a container stub thread to hand control back to the Sentry when a system call occurs:

1. **User-Notify Mode (`_USE_SYSCALL_NOTIFY = 0`)**: When `sysmsg` traps a system call that has not yet been binary-patched into a fast-path jump, it triggers a `SECCOMP_RET_USER_NOTIF` event that the Sentry reads from a seccomp listener file descriptor.
2. **Syscall Loop Mode (`_RUN_SYSCALL_LOOP = 1`)**: The stub thread writes its register state into the per-thread shared memory structure (`struct sysmsg`), sets `sysmsg->state = THREAD_STATE_PREPARE`, wakes the Sentry via `FUTEX_WAKE`, and sleeps on `sys_futex(&sysmsg->state, FUTEX_WAIT, THREAD_STATE_PREPARE, NULL, NULL, 0)`.

Upstream gVisor unconditionally attempts to install a `SECCOMP_RET_USER_NOTIF` filter with `SECCOMP_FILTER_FLAG_NEW_LISTENER` during stub initialization and aborts if `seccomp(2)` returns `EBUSY`.

In `gvx` (`pkg/sentry/platform/systrap/subprocess_linux.go` and `sysmsg_thread_linux.go`, introduced in [PR #71](https://github.com/gfire-sigs/gvx/pull/71)), `systrap` inspects the return code of `seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_NEW_LISTENER, ...)`:

- If the call succeeds, `gvx` uses the normal `SECCOMP_RET_USER_NOTIF` listener.
- If the call fails with `EBUSY` because an outer `gVisor` (`runsc`) or outer `gvx` sandbox already holds the listener slot, `gvx` logs a single diagnostic warning, installs a plain `SECCOMP_RET_TRAP` BPF filter (without `SECCOMP_FILTER_FLAG_NEW_LISTENER`), and sets `stubCallMode = 1` (`_RUN_SYSCALL_LOOP`) in the stub's shared global page for all stub threads.

Inside `pkg/sentry/platform/systrap/sysmsg/sysmsg_lib.c`, both the `SIGSYS` handler (`sysmsg_sighandler`) and the binary-patched fast-path trampoline (`syshandler`) check `__export_stub_call_mode`:

```c
// When __export_stub_call_mode == _RUN_SYSCALL_LOOP (1), the stub thread
// communicates with the inner Sentry strictly through shared memory + futexes,
// without ever triggering a seccomp user-notification event.
if (__export_stub_call_mode == _RUN_SYSCALL_LOOP) {
  for (;;) {
    int ret = WaitForSentry(sysmsg);
    if (ret == 0)
      break;
    gvisor_errno = -ret;
    sys_exit_group(1);
  }
}
```

Because `SECCOMP_RET_TRAP` filters can stack arbitrarily deep without consuming the kernel's single user-notification listener slot, `gvx` runs cleanly inside `runsc` and supports recursive nesting (`gvx` inside `gvx`).

### 2. Native `overlayfs` via `ext4lite` and `fsuid`/`fsgid` Ownership (`inode_init_owner`)

To prevent `overlayfs` from touching the outer gVisor container's `gofer` (`9p`/`lisafs`) root filesystem, `-v gvx-docker:/var/lib/docker` backs `/var/lib/docker` with `gvx`'s internal **`ext4lite`** filesystem stored in a single regular file (`gvx-docker.ext4`).

To outer `runsc`, `gvx-docker.ext4` is just one regular file accessed via `pread64(2)` and `pwrite64(2)`. Inside `gvx`, `ext4lite` exposes a complete JBD2-journaled `ext4` filesystem with:

- `trusted.overlay.*` and `security.capability` extended attributes.
- Character device whiteout nodes (`0:0`) and atomic `RENAME_EXCHANGE`.
- Linux `inode_init_owner` credential inheritance ([PR #70](https://github.com/gfire-sigs/gvx/pull/70)): when nested processes or unprivileged build steps call `setfsuid(2)` and `setfsgid(2)` while `EffectiveUID` remains `0`, new files and directories in `ext4lite` and `hostfs` inherit `creds.EffectiveKUID` / `creds.EffectiveKGID` (and honor parent directory `S_ISGID` bits), matching the Linux kernel's `inode_init_owner` behavior required by `overlayfs` and nested rootless tools.

### 3. Self-Contained `netstack` with `docker0` and `iptables-legacy`

Inside `gvx`, `dockerd` does not interact with the outer `runsc` container's network interfaces or netfilter tables. Instead:

- `dockerd` creates its `docker0` bridge and `veth` pairs inside `gvx`'s own user-space `netstack`.
- All `iptables-legacy` rules (`-t nat -N DOCKER`, `MASQUERADE`, `conntrack`, `addrtype`) execute against `gvx`'s internal netfilter implementation.
- Nested `runc` containers mount their `/proc`, `/sys`, and `/sys/fs/cgroup` hierarchies inside `gvx`'s in-memory `VFS2` mount table, never calling the outer `runsc` `mount(2)` syscall.

---

## Verifying Nested `docker:dind` Inside `gVisor` (or `gvx` in `gvx`)

You can verify both scenarios (`rungv` inside `runsc`, or `rungv` inside `rungv`) with the following commands:

```sh
# Inside an outer gVisor (runsc) container or an outer rungv container:
mkdir -p ~/.gvx
rungv run \
  -p unix:~/.gvx/docker.sock:2375 \
  -v gvx-docker:/var/lib/docker \
  -e DOCKER_TLS_CERTDIR= \
  docker:dind --tls=false &
DIND_PID=$!

export DOCKER_HOST="unix://$HOME/.gvx/docker.sock"
until docker info >/dev/null 2>&1; do sleep 0.5; done

# Verify that the inner Docker daemon is using native overlayfs (not vfs)
docker info --format 'Storage Driver: {{.Driver}}'
# Output: Storage Driver: overlayfs

# Run a multi-layer build and execute a nested container
mkdir -p /tmp/nested-test && cd /tmp/nested-test
cat <<'EOF' > Dockerfile
FROM alpine:3.20
RUN adduser -D -u 1001 appuser && mkdir -p /app && chown appuser:appuser /app
USER appuser
WORKDIR /app
RUN echo "nested-build-ok" > status.txt
CMD ["cat", "/app/status.txt"]
EOF

docker build -t nested-test:latest .
docker run --rm nested-test:latest
# Output: nested-build-ok

kill -INT $DIND_PID
```

---

## Further Reading in This Series

1. **[gvx: Running Linux Containers, Docker, and Kubernetes in One Unprivileged Process](/blog/posts/gvx-cross-platform-linux-containers-z48916d01)**: Overview of `gvx` across Windows (`whpx`, `systrap`), macOS (`hvf`), and Linux (`systrap`).
2. **[Running Docker, docker build, and docker compose Without Root or User Namespaces](/blog/posts/run-docker-without-root-or-userns-gvx-z48916d02)**: How `gvx` runs `docker:dind` when `root`, `sudo`, `unshare(CLONE_NEWUSER)`, `mount(2)`, and `/dev/fuse` are blocked.
3. **[Running Docker-in-Docker Inside gVisor and Nested Sandboxes with gvx](/blog/posts/docker-in-docker-inside-gvisor-with-gvx-z48916d03)** (this post)
4. **[Fast Isolated Docker Environments for AI Agent RL Rollouts with gvx](/blog/posts/fast-docker-environments-for-agent-rl-rollouts-z48916d04)**: How to use `gvx` and single-file `ext4lite` copy-on-write snapshots to run hundreds of parallel `SWE-bench` and RL rollout environments with 100-millisecond resets.
