# The Machine page — hardware & live usage

Every node card has a small chart icon. It opens the **Machine** page for that server: what the hardware actually is, and what it's doing right now. You can also reach it from a server's page via the **Machine** button.

The page is one card per subsystem — processor, memory, graphics, each disk, each network interface, platform, temperature — and each card pairs the **specification** with its **live number**. The CPU model and the CPU load sit together, rather than at opposite ends of the page.

## What it shows

| Card | Specification | Live |
|---|---|---|
| **Processor** | model, cores/threads, cache, max clock | utilisation %, current clock, load average |
| **Memory** | total | used / available / cached, swap |
| **Graphics** (one per card) | model, vendor, driver | temperature, fan, power draw, utilisation and VRAM — AMD from sysfs, NVIDIA via the Container Toolkit (see below) |
| **Disk** (one per device) | model, capacity, SSD or HDD | **free space**, used/total per mount, read and write throughput |
| **Network** (one per physical NIC) | link speed, state | up and down throughput |
| **Platform** | OS, kernel, architecture, board, Docker version | uptime, container/image/volume counts, battery |
| **Temperature** | — | every sensor the host exposes (CPU package and cores, drives, GPU, board) |

## Where the data comes from

The Docker API alone knows very little about the machine: the OS, kernel, architecture, CPU **count** and total RAM. No make, no model, no CPU model name, no GPU, no disks, no temperatures.

So DockBack reads the rest from the host itself, by starting a **short-lived container** on that node with the host's `/proc` and `/sys` bind-mounted **read-only**. It is the same throwaway-sidecar pattern backups already use:

- **unprivileged** — no `--privileged`, no added capabilities, no access to `/dev/mem`
- **read-only** — both mounts are `:ro`; the probe writes nothing anywhere
- **short-lived** — it runs for about a second and is removed immediately
- **labelled** — so the orphan sweep reclaims it if DockBack is stopped mid-probe

### Serial numbers are never read

`product_serial`, `board_serial` and `chassis_serial` are deliberately skipped. They are asset-tracking identifiers with no backup value, and reading them would put hardware serials into the DockBack database and into anything exported from it. They cannot leak from here because they are never collected.

### Memory modules are not listed

Per-DIMM detail (size, speed, slot) needs `dmidecode`, which requires a privileged container and `/dev/mem`. That is a much larger privilege than the rest of this page needs, so it is out of scope. Total memory, usage, cache and swap all come from `/proc/meminfo` and need no privilege at all.

### GPU model names

DockBack resolves the model as precisely as your host allows, best source first:

1. **The NVIDIA proprietary driver's own record** (`/proc/driver/nvidia/gpus/*/information`), which gives the exact marketing name — *NVIDIA GeForce RTX 3060 Laptop GPU*. Better than any database, and free.
2. **Your host's own PCI id database**, if it has one — mounted read-only from `/usr/share/hwdata/pci.ids` (or `/usr/share/misc/pci.ids`). This turns `0x22b0` into *Atom/Celeron/Pentium Processor x5 Integrated Graphics Controller*.
3. **Vendor + device id**, which is all that can honestly be claimed otherwise.

DockBack does not ship a PCI database of its own — that would be a megabyte of lookup tables inside a backup tool, and it would go stale. If a card shows only an id, installing **`hwdata`** (Debian/Ubuntu, Fedora) or **`pciutils`** on that host gives you full names on the next probe. The page tells you so.

### GPU temperature, fan and power

Where the live figures come from depends entirely on the driver, because the drivers differ in what they publish.

**AMD (`amdgpu`) and Nouveau** register a standard **hwmon** node in `/sys`, so DockBack reads temperature, fan, power — and on AMD, real **utilisation and VRAM used/total** — directly, with nothing installed.

**NVIDIA (proprietary) publishes none of this to the kernel.** It registers no hwmon node, which is also why `lm-sensors` cannot read an NVIDIA GPU's temperature. The numbers live only behind **NVML**, and `nvidia-smi` is the tool that reads it. From sysfs alone, DockBack can identify the card and nothing more.

To get NVIDIA telemetry, DockBack runs the **host's own `nvidia-smi`** inside a throwaway container. That needs two things on the host:

1. **The NVIDIA Container Toolkit.** Having `nvidia-smi` installed is *not* sufficient — that is the driver. The toolkit is what lets a container see the GPU, and it is what injects the host's `nvidia-smi` and libraries into the container. Install `nvidia-container-toolkit` and restart Docker.
2. Nothing else. DockBack ships no NVIDIA components and installs nothing; it uses whatever the host already has.

When both are in place you get **temperature, utilisation, VRAM used/total, power draw and fan speed**, and the card's exact marketing name from `nvidia-smi` itself. When the toolkit is absent, the card still shows its identity and the page explains what is missing.

The NVIDIA probe only runs on a machine where an NVIDIA card was actually detected, and only when its figures are still missing — so it costs nothing on every other host. It uses a small glibc base image (the injected `nvidia-smi` is glibc-linked and cannot run on the musl-based Alpine sidecar used elsewhere); if the host already has any common Debian or Ubuntu base, that one is reused and nothing is pulled.

It is built to work across the whole NVIDIA range rather than one card:

- **Older drivers** don't recognise every field, and `nvidia-smi` rejects the *entire* query if one field is unknown. DockBack asks for progressively smaller field sets until one succeeds, so an old Quadro still reports its temperature even when it can't report power.
- **Passive datacentre cards** (Tesla, A-series) have no fan; **some vGPU profiles** report no power. Those fields simply stay absent instead of showing a misleading `0`.
- **Multi-GPU servers** get per-card figures, matched by **PCI bus address** rather than enumeration order — on a hybrid laptop the DRM card index and the `nvidia-smi` index need not agree, and guessing would put the discrete GPU's temperature on the integrated one.
- **Multiple PCI domains** (large servers) are distinguished, not collapsed.
- Both the modern `--gpus`-style device request and the older `nvidia` runtime are attempted, so old and new Docker installs both work.

**Intel (`i915`) and most integrated graphics** publish no sensors at all; the tile says so rather than showing zeros.

### Temperatures come from two places

`/sys/class/thermal` is the ACPI view — often just a couple of vague zones. The real sensors live in **`/sys/class/hwmon`**: `coretemp`/`k10temp` for the CPU package and per-core, `nvme` for drives, `amdgpu`/`nvidia` for graphics, plus board sensors. DockBack reads both and merges them, preferring the more precise hwmon reading when the two overlap.

### Free space, and why it comes from somewhere else

A disk's **capacity** comes from `/sys/block` — that is the raw device size. **Free space does not exist there at all**: it is a property of a *filesystem*, obtained by a `statfs()` call on a mounted path. A disk may carry several filesystems, or none mounted at all.

So DockBack mounts the host tree **read-only** at `/hostfs` and runs `df` against it. `df` performs `statfs()` only — no file is ever opened or read — and only real block-backed filesystems are reported (the device must be under `/dev`, which drops tmpfs, overlay, squashfs and the container's own layers).

Each disk tile then shows **how much room is left**, the used/total split, and the free space per mount point. A disk with nothing mounted says so, which is a real answer for a spare or unformatted drive rather than a gap.

Filesystems that do not belong to a listed disk — LVM volumes, software RAID, network mounts — are still reported in the machine's full filesystem list; they simply are not attributed to a physical drive.

**On the extra mount:** this does not widen what an attacker who compromised DockBack could do. DockBack already holds Docker API permission to create containers, so any host path could already be mounted by anything controlling it. What changes is what DockBack's own probe touches, so the mount is read-only, short-lived, unprivileged, and used for `statfs` alone. A host that refuses the mount still gets every other section of the page.

### Which disks and interfaces are listed

**Disks** are whole devices only. eMMC storage exposes `mmcblk0boot0`, `mmcblk0boot1` and `mmcblk0rpmb` alongside the real `mmcblk0` — hardware boot and replay-protect areas of the *same chip*, a few MB each. Listing them as separate drives would be wrong, so anything whose name is another device's name plus a suffix is treated as part of that device (this also covers partitions). Loop, RAM, device-mapper, zram and optical devices are skipped too.

**Network interfaces** are physical links only. The test is structural rather than a list of names: a real NIC has a `device` link to its PCI/USB/MMIO parent in `/sys`, and every virtual interface has none. That keeps **Tailscale, WireGuard, OpenVPN, `tun`/`tap`, Docker bridges, `veth` pairs, bonds and VLANs** off a page about the machine, with no product names to keep chasing as new VPNs appear.

If a host's only connectivity is a VPN tunnel, it will show no network card — which is accurate: the tunnel is not the machine's hardware.

## These numbers differ from the dashboard — on purpose

The CPU and memory figures on the **dashboard node cards** are the sum of that node's **container** statistics. The Machine page shows the **whole host**.

They will not match, and neither is wrong. A machine running something heavy outside Docker — a compile, a game, another VM — can sit at 70% on the Machine page while the dashboard correctly reports 3%, because Docker really is only using 3%. If you are asking *"is this box busy?"*, the Machine page is the one to trust.

## Caching — the page never waits on a probe

Because each reading starts a container on the host, the page is served **from cache** and refreshed **in the background**:

- Opening the page shows the **last recorded reading immediately**. If it is more than 15 seconds old, a refresh starts behind the scenes and the next poll picks it up.
- The reading is **saved to disk**, so restarting DockBack doesn't send every node back to a slow first load.
- The only time a request waits is the **very first** probe of a node that has never been read — and that wait is capped, after which it falls back to a background attempt.
- The header says what you're looking at: *Live*, *As of 2 min ago*, or *Refreshing…*.
- It refreshes only **while the page is visible** — switching tabs stops it.
- Concurrent requests for one node are **collapsed into a single probe**, so three tabs cost one.
- Nothing else in DockBack triggers it. The dashboard, the fleet refresh and background jobs never probe hardware.
- **Re-probe** forces a fresh reading immediately.

If a probe fails after a good one, DockBack **keeps showing the good reading** and says how old it is, rather than blanking the page. Hardware specifications don't stop being true because a host was briefly slow — only the live figures go stale, and the page says so.

## The first scan

The first time you open the page for a node there is nothing cached, so DockBack takes a reading. It says **"Scanning this machine…"** and polls quickly until the result arrives — it does not report a problem, because nothing has gone wrong yet.

Only when a scan has actually **run and failed** does the page warn you, and it then waits for you to press **Re-probe** rather than retrying on every poll and hammering a host that is already struggling.

## When a host reports very little

Not every machine can answer every question, and the page says so rather than showing zeros.

- **A virtual machine** reports the hardware its *hypervisor* presents, not the physical host. The page flags this explicitly.
- **A NAS** (Synology, QNAP) often exposes no DMI identity and no temperature sensors.
- **Docker Desktop / WSL2** reports the Linux VM, not your PC.
- **Firmware placeholders** like *"To Be Filled By O.E.M."* are treated as missing rather than displayed as a model name.

Anything unavailable is simply left out — a host with no GPU shows no graphics card at all, rather than an empty one — and a **"What this host doesn't report"** note at the bottom explains what was missing and why.

## If the probe fails entirely

The page still renders, showing everything the Docker API knows, plus the reason the probe didn't run. The usual causes:

- **The socket-proxy forbids container creation.** The probe needs `POST /containers/create`, the same permission volume backups need — so if backups work, this will too.
- **The host has no `/sys`** (very unusual, some minimal or non-Linux hosts).
- **The sidecar image can't be pulled** on a node with no registry access. It is the same image volume backups use, so it is normally already present. The pull has its own generous time budget separate from the probe, so a first-ever read on a slow link is not reported as a timeout.
- **A host attribute is blocking.** Some `/sys` files stall — a thermal zone mid-transition, a GPU asleep in a low-power state, a device that has been unplugged. Every attribute read is individually bounded, so one sulking file costs a single field rather than the whole page.

### NAS disks in standby

On a NAS whose drives spin down, reading a disk's *model* wakes it, and that read blocks in **uninterruptible I/O** — which no timeout can cut short, because neither `SIGTERM` nor `SIGKILL` reaches a process in that state. With several sleeping bays this can outlast the probe.

DockBack is built to survive it rather than pretend otherwise:

- The probe writes **cheapest-first** and reads block devices **last**, so identity, CPU, memory, network and sensors are already recorded before anything can hang.
- Whatever arrived by the deadline is **used**, not discarded. Previously a slow host produced a blank page saying nothing could be read, even though most of the answer had already been received.
- A cut-short reading is labelled **Partial reading** — the data below it is real; only the sections that didn't respond are missing.
- A disk's model is read on its own, *after* its size and type, so a hang costs the model string rather than the drive.

If a NAS consistently reports partially, that is the disks sleeping rather than a fault. Pressing **Re-probe** while they are awake (just after a backup, say) usually returns the full picture.
