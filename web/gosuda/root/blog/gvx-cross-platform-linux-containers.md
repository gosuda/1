---
id: 48916d01a1014b009c11d2e3f4a50001
author: Goldberg
title: 'gvx: Running Linux Containers, Docker, and Kubernetes in One Unprivileged Process'
description: Technical overview of gvx, a cross-platform container runtime and Go SDK built on the gVisor user-space kernel that runs Linux containers, Docker-in-Docker, and multi-node HA Kubernetes in a single unprivileged process across Windows, macOS, and Linux.
language: en
date: 2026-11-06T09:00:00Z
path: /blog/posts/gvx-cross-platform-linux-containers-z48916d01
go_package: z.gfire.dev/gvx
go_repourl: https://github.com/gfire-sigs/gvx.git
category: lowlevel
---

## Introduction

Running Linux containers normally depends on host-specific privileged infrastructure:

- On **Linux**, standard OCI runtimes (`runc`, `crun`, `runsc`) require either `root` privileges or user namespaces (`unshare(CLONE_NEWUSER)`), `newuidmap` setuid helpers, `mount(2)` calls for `/proc` and `/sys/fs/cgroup`, and `/dev/fuse` or host `CAP_SYS_ADMIN` for overlay filesystems.
- On **Windows** and **macOS**, container engines (`Docker Desktop`, `Podman Machine`, `Lima`, `OrbStack`) boot a full Linux virtual machine kernel inside WSL2, Hyper-V, or `Virtualization.framework` and run a background daemon inside that VM.

**[gvx](https://github.com/gfire-sigs/gvx)** (`rungv` CLI and the `z.gfire.dev/gvx/pkg/gvx` Go SDK) takes a different architectural path. Built on a heavily extended fork of Google's **gVisor** user-space Linux kernel (`Sentry`) and user-space network stack (`netstack`), `gvx` executes unmodified `linux/amd64` and `linux/arm64` ELF containers directly inside **a single unprivileged host process** on Windows, macOS, and Linux.

No Linux VM kernel boots on Windows or macOS. No background daemon runs on the host. No `root`, `sudo`, user namespaces (`CLONE_NEWUSER`), `mount(2)`, `TUN`/`TAP` interfaces, or `CAP_NET_ADMIN` privileges are required on the host OS. Despite running entirely as an unprivileged user process, `gvx` provides enough Linux kernel fidelity to run **Docker-in-Docker (`docker:dind`)**, `docker build`, `docker compose`, and **multi-node High-Availability Kubernetes (`k3s` with embedded `etcd` and `Calico` CNI)**.

---

## Single-Process Architecture

Every `gvx` container instance consists of one host OS process. Inside that process, the gVisor `Sentry` implements approximately 370 Linux system calls, a virtual memory manager, an internal virtual filesystem (`VFS2`), and a user-space TCP/IP stack (`netstack`).

![gvx Single-Process Architecture](/assets/images/gvx/gvx-architecture.svg)

```d2
direction: down

host_process: "Single Unprivileged Host Process (rungv CLI / pkg/gvx Go SDK)" {
  lockdown: "pkg/hostlockdown (Applied Before Container Execution)" {
    win_lock: "Windows: Restricted Token + Job Object + Mitigation Policies"
    mac_lock: "macOS: Seatbelt sandbox_init (deny default)"
    lin_lock: "Linux: PR_SET_NO_NEW_PRIVS + BPF seccomp Filter"
  }

  workloads: "Unmodified Linux Guest Workloads (ELF Binaries)" {
    app: "OCI Containers (alpine, ubuntu, python, go)"
    dind: "docker:dind (dockerd + containerd + runc + BuildKit)"
    k3s: "Multi-Node HA k3s + Calico CNI + etcd"
  }

  platforms: "Platform Syscall Interception (pkg/sentry/platform)" {
    whpx: "Windows whpx (Ring 0 Hyper-V Partition)"
    win_systrap: "Windows systrap (Ring 3 VEH + Nirvana Callback)"
    hvf: "macOS hvf (Hypervisor.framework vCPU)"
    lin_systrap: "Linux systrap (SECCOMP_RET_USER_NOTIF / Trampoline / Futex Loop)"
  }

  sentry: "User-Space Linux Kernel (gVisor Sentry: ~370 Syscalls)" {
    vfs2: "VFS2 Virtual Filesystem Layer" {
      erofs: "erofs: Immutable OCI Layers (Shared Page Cache)"
      ext4lite: "ext4lite: Single-File .ext4 Volume (fsuid/fsgid, xattr, CoW)"
      hostfs: "hostfs v2: Capability-Scoped Bind Mounts (os.Root)"
      tmpfs: "tmpfs / procfs / sysfs / cgroupfs (In-Memory)"
    }

    netstack: "User-Space TCP/IP Network Stack (netstack)" {
      vnics: "Virtual NICs: eth0, docker0 Bridge, veth, tun/tap, vxlan"
      netfilter: "iptables-legacy: nat, filter, raw Tables"
      nat_mode: "-net=nat: 10.0.2.15/24 -> Host TCP/UDP Dialer"
      wg_mode: "-net=wg: 10.200.0.x/24 In-Memory WireGuard Mesh"
    }
  }

  handles: "Pre-Opened Host Handles (Only Host Kernel Boundary)" {
    cache_fd: "CacheDirRoot (~/.cache/gvx/blobs, volumes/*.ext4)"
    bind_fd: "os.Root Bind Mounts (-v host:guest)"
    udp_fd: "Single UDP Socket (-net=wg) or Host Socket Dialer (-net=nat)"
  }

  lockdown -> workloads: "Restricts host OS access before guest start"
  workloads -> platforms: "Linux syscalls / page faults"
  platforms -> sentry: "Dispatches SyscallRegs"
  sentry.vfs2 -> handles.cache_fd: "pread / pwrite on pre-opened FDs"
  sentry.vfs2 -> handles.bind_fd: "Openat2-style scoped file ops"
  sentry.netstack -> handles.udp_fd: "User-space packet / stream I/O"
}
```

When a guest Linux process invokes a system call such as `openat(2)`, `clone(2)`, `mount(2)`, or `socket(2)`, the platform execution engine intercepts the trap before it reaches the host OS kernel and dispatches the register state to the in-process `Sentry`. The host OS kernel never sees guest Linux system calls, guest UIDs, or guest mount tables.

---

## Cross-Platform Execution Engines (`pkg/sentry/platform`)

Upstream gVisor only targets Linux hosts (`systrap`, `kvm`, `ptrace`). `gvx` extends `pkg/sentry/platform` with four execution engines across Windows, macOS, and Linux:

| Host OS & Architecture       | Platform Flag       | Execution Mechanism                                                                                                                                                                                                                                                       | Host Privilege Requirement                                                                                                       |
| :--------------------------- | :------------------ | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | :------------------------------------------------------------------------------------------------------------------------------- |
| **Windows (`amd64`)**        | `-platform=whpx`    | Runs the Sentry kernel in guest Ring 0 and container code in guest Ring 3 inside a hardware-backed Windows Hypervisor Platform (`WHPX`) partition without booting a Linux VM kernel.                                                                                      | Windows Hypervisor Platform feature enabled (no administrator elevation required at runtime).                                    |
| **Windows (`amd64`)**        | `-platform=systrap` | Runs container threads in host Ring 3 and intercepts Linux `syscall` instructions using Windows Vectored Exception Handling (`AddVectoredExceptionHandler`) and the Nirvana instrumentation callback (`NtSetInformationProcess` with `ProcessInstrumentationCallback`).   | Zero hypervisor features; runs inside standard unprivileged Windows user sessions and Windows VMs without nested virtualization. |
| **macOS (`arm64`, `amd64`)** | `-platform=hvf`     | Executes container threads inside lightweight Apple `Hypervisor.framework` vCPUs with two-stage MMU translation (`Stage-1` guest page tables + `Stage-2` EPT / ARM64 Stage-2) and traps `SVC #0` / `SYSCALL` directly into the Sentry.                                    | Standard `com.apple.security.hypervisor` entitlement embedded in the `rungv` binary (no `root` required).                        |
| **Linux (`amd64`, `arm64`)** | `-platform=systrap` | Intercepts guest syscalls via `SECCOMP_RET_USER_NOTIF`, patches `syscall` call sites with jumps into a shared-memory trampoline, and automatically falls back to a shared-memory futex loop (`_RUN_SYSCALL_LOOP`) when nested inside an outer seccomp listener (`EBUSY`). | Unprivileged user process (`ptrace` on self-child once at init, `seccomp`, and `mmap(PROT_EXEC)`).                               |

On Windows and macOS, `-platform=auto` selects the hardware-accelerated engine (`whpx` or `hvf`) when available and falls back to `systrap` on Windows when hypervisor partitions are unavailable.

---

## Storage Architecture: `erofs`, `ext4lite`, and `hostfs` v2

Container workloads require three distinct storage behaviors: fast read-only rootfs layers, full POSIX read-write volumes for inner container engines (`overlay2`, `etcd`, SQLite), and safe host directory sharing. `gvx` implements all three inside `VFS2` without invoking host `mount(2)` or `/dev/fuse`.

### 1. Immutable OCI Layers via `erofs`

When `gvx` pulls an OCI image from a registry, it converts each compressed tar layer into an **EROFS** (Enhanced Read-Only File System) image stored under `~/.cache/gvx/blobs/<sha256>.erofs`. At runtime, `pkg/sentry/fsimpl/erofs` memory-maps the pre-opened `.erofs` file descriptors. Multiple concurrent `gvx` processes share the same host page cache for identical base layers without unpacking tarballs into host directories.

### 2. Mutable Single-File Volumes via `ext4lite`

Nested container engines (`dockerd`, `containerd`, `k3s`) cannot run their `overlay2` storage driver on top of virtual 9P (`gofer`), Windows NTFS, or macOS APFS mounts because `overlay2` requires POSIX whiteout character devices (`0:0`), atomic `RENAME_EXCHANGE`, `d_type` directory entries, and `trusted.overlay.*` / `user.overlay.*` extended attributes.

To solve this portably across every host OS, `gvx` includes **`ext4lite`** (`pkg/sentry/fsimpl/ext4lite`), a pure-Go read-write `ext4` filesystem implementation backed by a single sparse file on the host (`-v mydata.ext4:/var/lib/docker`). `ext4lite` provides:

- **Full POSIX ownership and `fsuid`/`fsgid` semantics**: New inodes inherit credentials from the calling task's `fsuid` and `fsgid` (`inode_init_owner`, including `S_ISGID` directory group inheritance), which unprivileged BuildKit and rootless inner containers rely on.
- **Extended attributes (`xattr`)**: Native storage for `trusted.*`, `security.capability`, `system.posix_acl_*`, and `user.*` namespaces.
- **Lock-free read fast path**: Clean cached 4 KiB blocks are served via an atomic pointer table (`atomic.Pointer[cachedBlock]`) without acquiring the filesystem mutex on cache hits.
- **Metadata integrity**: Extent trees, HTree directory indexing (`dx_root`), and `crc32c` metadata checksums compatible with Linux `e2fsck`.

### 3. Capability-Scoped Host Mounts via `hostfs` v2

When you bind-mount a host directory (`-v /host/src:/workspace`), `gvx` opens the host directory using Go's traversal-resistant `os.OpenRoot` (`*os.Root`) before locking down the process. Inside the Sentry, `pkg/sentry/fsimpl/hostfs` routes all guest file operations through that scoped root handle, preventing symlink or `..` traversal escapes outside the mounted subtree. Because Windows and macOS filesystems do not natively store Linux UIDs, device nodes, or `security.capability` xattrs, `hostfs` maintains an in-memory synthetic metadata overlay for guest `chown`, `chmod`, `mknod`, and `setxattr` operations.

---

## User-Space Networking: `-net=nat` and `-net=wg`

Standard container runtimes configure networking by creating host `veth` pairs, attaching them to a host bridge, and programming host `iptables` or `nftables` rules. That requires `CAP_NET_ADMIN` on Linux and privileged VM networking daemons on Windows and macOS.

`gvx` runs its entire network stack in user space via `netstack`:

1. **NAT Mode (`-net=nat`, default)**:
   - Assigns `10.0.2.15/24` to guest `eth0`, `10.0.2.2` to the virtual gateway, and `10.0.2.3` to a built-in DNS proxy.
   - Intercepts outbound TCP connections and UDP datagrams inside the Sentry and translates them into ordinary unprivileged host socket calls (`net.DialTCP`, `net.ListenUDP`).
   - Supports inbound port publishing via `-p <hostPort>:<guestPort>` without elevated privileges.
2. **WireGuard Mesh Mode (`-net=wg`)**:
   - Embeds an in-memory `wireguard-go` endpoint directly inside `netstack`, bound to a single unprivileged host UDP socket.
   - Multiple `gvx` containers on the same machine or across different hosts (Windows, macOS, and Linux) form an encrypted Layer-3 mesh (`10.200.0.x/24`) with direct container-to-container routing.
3. **Full Inner L2/L3 Emulation for DinD and Kubernetes**:
   - Inside a `gvx` container, guest workloads have virtual `CAP_NET_ADMIN` over their own `netstack` instance. Guest `dockerd`, `kube-proxy`, and `Calico` can create virtual bridges (`docker0`), `veth` pairs, `tun`/`tap` devices, `vxlan` tunnels, IP-in-IP tunnels, and `iptables-legacy` (`nat`, `filter`, `raw`) rules completely inside user space.

---

## Defense-in-Depth Host Lockdown (`pkg/hostlockdown`)

Before `gvx` transfers control to the first instruction of a container binary, it pre-opens every host resource it will need for the lifetime of the container (`CacheDirRoot`, `StateDirRoot`, `.ext4` volume files, `os.Root` bind mounts, and the UDP socket) and then invokes `hostlockdown.Apply()`. This step is irreversible for the lifetime of the process:

- **Windows**: Replaces the process token with a Restricted Token (`CreateRestrictedToken` with `DISABLE_MAX_PRIVILEGE`, stripping `SeDebugPrivilege` and `SeImpersonatePrivilege`), assigns a Job Object with `JOB_OBJECT_LIMIT_ACTIVE_PROCESS` to block child process creation, and enables `ProcessDynamicCodePolicy`, `ProcessSystemCallDisablePolicy` (Win32k/GDI syscall block), and `ProcessImageLoadPolicy`.
- **macOS**: Applies an Apple Seatbelt profile via `sandbox_init` starting from `(version 1)(deny default)` and blocks `process-fork` and `process-exec`.
- **Linux**: Sets `PR_SET_NO_NEW_PRIVS`, clears ambient capabilities, and installs a BPF `seccomp` filter blocking dangerous host syscalls (`kexec_load`, `mount`, `umount2`, `pivot_root`, `init_module`, `finit_module`, `bpf`, `perf_event_open`, `userfaultfd`, `keyctl`, `acct`, `swapon`).

---

## Practical Workloads: From Alpine to Docker-in-Docker and HA Kubernetes

### 1. Running Containers and Docker-in-Docker via `rungv`

A single `rungv` binary runs standard OCI images or boots a persistent `docker:dind` engine backed by `ext4lite`:

```bash
# Run an interactive Alpine container on Windows, macOS, or Linux
rungv -it alpine:latest sh

# Start a persistent unprivileged Docker-in-Docker engine
# Creates ~/.cache/gvx/volumes/gvx-docker.ext4 and proxies ~/.gvx/docker.sock
rungv --docker-PersistVolume -d --name dind docker:dind

# Use the standard host docker CLI against the unprivileged gvx daemon
export DOCKER_HOST="unix://$HOME/.gvx/docker.sock"
docker info
docker build -t myapp:test .
docker compose up -d
```

### 2. Multi-Node HA Kubernetes (`k3s` + `Calico`) over WireGuard

Because `gvx` implements `/dev/kmsg`, `cgroupfs` v2, `timerfd`, `inotify`, `fanotify`, `overlay2` on `ext4lite`, and `iptables-legacy` + `ipset` in `netstack`, you can boot a 3-node High-Availability `k3s` cluster (2 control-plane nodes with embedded `etcd` + 1 worker node) connected over `-net=wg` where every node is a single unprivileged `rungv` process:

```bash
# Node 1 (Control Plane 1, 10.200.0.1)
rungv -d --name k3s-cp1 \
  -net=wg -wg-ip=10.200.0.1/24 -wg-listen=:51821 \
  -wg-Key=$CP1_KEY -wg-peers="$CP2_PUB@127.0.0.1:51822,$WK1_PUB@127.0.0.1:51823" \
  -v cp1.ext4:/var/lib/rancher/k3s \
  -p 16443:6443 \
  rancher/k3s:v1.35.3-k3s1 server --cluster-init --flannel-backend=none --disable-network-policy
```

Inside this cluster, `Calico` v3.29 (`calico-node` + `calico-kube-controllers`) establishes cross-node pod routing across the user-space WireGuard mesh without touching the host kernel routing table.

### 3. Embedding Linux Containers in Go via `pkg/gvx`

Because `gvx` is structured as a Go library first, applications and test harnesses can spawn isolated Linux containers in-process or via the `z.gfire.dev/gvx/pkg/gvx` SDK without shelling out to a host Docker daemon:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"z.gfire.dev/gvx/pkg/gvx"
)

func main() {
	ctx := context.Background()

	runner, err := gvx.NewRunner(gvx.RunnerOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer runner.Close()

	res, err := runner.Run(ctx, gvx.RunConfig{
		Image: "alpine:latest",
		Args:  []string{"uname", "-a"},
		Env:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("exit=%d output=%s\n", res.ExitCode, string(res.Stdout))
}
```

---

## Further Reading in This Series

This article is the first in a four-part technical series on `gvx`:

1. **[gvx: Running Linux Containers, Docker, and Kubernetes in One Unprivileged Process](/blog/posts/gvx-cross-platform-linux-containers-z48916d01)** (this post)
2. **[Running Docker, docker build, and docker compose Without Root or User Namespaces](/blog/posts/run-docker-without-root-or-userns-gvx-z48916d02)**: How to bootstrap `docker:dind` in locked-down sandboxes where `sudo`, `unshare(CLONE_NEWUSER)`, `mount(2)`, and `/dev/fuse` are blocked.
3. **[Running Docker-in-Docker Inside gVisor and Nested Sandboxes with gvx](/blog/posts/docker-in-docker-inside-gvisor-with-gvx-z48916d03)**: Why `apt-get install docker.io` and nested `runsc` fail inside `gVisor` across five kernel barriers, and how `gvx` runs nested `docker:dind` using a shared-memory futex loop.
4. **[Fast Isolated Docker Environments for AI Agent RL Rollouts with gvx](/blog/posts/fast-docker-environments-for-agent-rl-rollouts-z48916d04)**: Scaling `SWE-bench` and agent reinforcement learning rollouts with hundreds of parallel unprivileged `docker:dind` instances and 100-millisecond copy-on-write `.ext4` volume resets.
