# gpulab

Add NVIDIA GPU workers, slice their GPUs with MIG (or hand them out whole), and give users
Incus system containers with one or more slices that they can SSH into.

```
 Browser ──► gpulab-server (UI + API + SQLite) ──mTLS──► Incus on each worker
                    ▲  long-poll heartbeats
                    └──────────── gpulab-agent (NVML: inventory + MIG) on each worker
 User ── ssh -p 22000 alice@worker ──► Incus proxy device ──► container:22
```

## Try it without GPUs

```bash
make demo        # or: GPULAB_ADMIN_TOKEN=demo go run ./cmd/gpulab-server --demo
```

Open http://localhost:8080 and sign in with `demo`. `--demo` runs a simulated worker in the same
process: one A100-40GB (MIG capable) and one RTX 4090 (not MIG capable), behind a fake Incus API.
Approve the worker, configure slices, add a user, then create a container. Demo state is thrown away on exit.

## How it works

- **Joining a worker.** The admin clicks *Add worker* and runs the one-line install command on the GPU host.
  The agent registers with a single-use token and reports its GPUs, MIG profiles and placements, plus its Incus
  server certificate fingerprint. When the admin approves it, the agent adds the control plane's client
  certificate to the local Incus trust store over the unix socket. From then on the control plane calls the
  worker's Incus API directly, pinning that fingerprint.
- **Slicing.** The UI only offers MIG layouts that fit. `internal/mig` checks them against the GPU's real
  placement rules (from NVML, or a built-in catalog for A100/H100/A30 before MIG is first enabled) and picks
  exact placements. The agent then converges the GPU incrementally: instances that already match are left
  alone, so containers on them keep running. Removing a slice that is assigned to a container is refused.
- **Containers.** Containers are created with `gputype=mig` + `mig.uuid` + `pci` (or `gputype=physical` for
  a whole GPU), `nvidia.runtime=true`, CPU and memory limits, a cloud-init user with the user's SSH keys, and a
  proxy device for SSH (ports 22000–22999 on the worker). `boot.autostart=false` is deliberate (see the next point).
- **Recovery after a reboot.** MIG instances don't survive a reboot, and when they are recreated they get
  **new UUIDs**. Slices are identified by (GPU, profile, placement), not by UUID. When a worker comes back with
  no MIG instances, the control plane re-applies the desired layout, rewrites `mig.uuid` on the affected
  containers, and only then starts them.
- **Consistency.** The control plane queues every change as an operation (*Activity* tab). Operations on one
  worker run in order. A reconciler compares containers with Incus every 15 s.

## Production setup

**Control plane** (any Linux box the workers can reach):

```bash
make build
sudo install bin/gpulab-server bin/gpulab-agent /usr/local/bin/
echo "GPULAB_ADMIN_TOKEN=$(openssl rand -hex 24)" | sudo tee /etc/default/gpulab-server
sudo chmod 600 /etc/default/gpulab-server
# edit deploy/gpulab-server.service (public URL, TLS cert), then:
sudo cp deploy/gpulab-server.service /etc/systemd/system/ && sudo systemctl enable --now gpulab-server
```

Serve it over TLS (`--tls-cert/--tls-key`, or behind a reverse proxy), because agents send their secret on every heartbeat.

**Each worker** needs:
- the NVIDIA driver
- `nvidia-container-toolkit`
- Incus initialised with `incus admin init`, with a default profile that has a root disk and a network
- port 8443 reachable from the control plane
- SSH ports 22000–22999 reachable from users

Then run the install command from *Add worker*. It sets `core.https_address=:8443` if unset, installs the agent
as a systemd service, and joins the worker. If the control plane reaches the worker on a different address than
the one the agent detects, pass `--incus-url https://ADDR:8443`.

`gpulab-agent` is built with cgo (NVML is loaded at runtime), so build it on a glibc no newer than the workers'.

## Development

```bash
make test   # vet + unit tests + end-to-end test (real server and agent, fake GPUs + fake Incus), with -race
```

| Path | What |
|---|---|
| `cmd/gpulab-server`, `cmd/gpulab-agent` | binaries |
| `internal/server` | API, agent endpoint, operation runner, reconciler |
| `internal/store` | SQLite schema and queries |
| `internal/mig` | layout validation / placement solver, built-in profile catalog |
| `internal/agent`, `internal/agent/gpu` | agent loop; NVML and fake GPU backends |
| `internal/incus` | minimal Incus REST client (mTLS, pinned cert); `fakeincus` test server |
| `internal/e2e` | end-to-end test |
| `web/` | embedded UI and the agent install script |

## Not done yet

- **Not yet run on real hardware.** The NVML backend and the Incus client compile and follow the
  documented APIs. The tests only exercise them against fakes, so the first run on a real A100/H100 + Incus host
  is the real test.
- Enabling MIG on some GPUs needs a GPU reset or reboot. The agent reports this as an error with instructions
  and doesn't reset the GPU itself.
- Single admin token. No per-user logins, quotas or reservations yet.
- No time-slicing/MPS sharing for GPUs without MIG, and no vGPU for VMs.
- SSH is one proxy port per container. A single SSH gateway (e.g. sshpiper) would be nicer.
- SQLite with a single control-plane instance.
