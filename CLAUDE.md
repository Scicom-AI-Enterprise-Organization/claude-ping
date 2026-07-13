# claude-ping

Keep a Claude Code agent (or any script) **reliably connected to a remote instance**
over **one persistent SSH connection**, instead of re-connecting on every command.

A single self-contained **Go** binary (`claude-ping`), no runtime dependencies beyond
the system `ssh`/`rsync` (and `nvidia-smi`/`pgrep` on the remote box, best-effort).

Extracted and generalized from the `neucodec-44k` RunPod workflow — the same idea
works for any remote box (RunPod, EC2, bare metal) reached over SSH.

## The problem it solves

Agents driving a remote box over SSH are flaky for two reasons:

1. **Streaming sessions never return.** `tail -f` (or an interactive REPL) holds the
   connection open forever — it blocks the agent's turn and dies on any network blip.
2. **A fresh connection per command** — full TCP+auth handshake each time, no
   keepalive, no retry. One transient drop fails the whole command.

The remote *job* is usually already crash-proof (nohup + logfile + auto-resume), so
the disconnects only ever break the **agent's observability**. claude-ping makes that
channel stateless and self-healing.

## How it works

**One persistent SSH master, reused by everything.** The `claude-ping` binary shells
out to the system `ssh` with connection multiplexing (`ControlMaster` +
`ControlPersist=yes`): the first command opens a master socket that stays alive in the
background; every later command rides that same tunnel with **no re-handshake**, and
if it drops the next command silently re-opens it (plus 3× retry with backoff and
TCP/Server keepalives).

Shelling out to `ssh` is deliberate — the master must persist *across separate CLI
invocations*, which a native in-process Go SSH client cannot do (it would die with the
process). Everything else (config, retry, dispatch, the heartbeat, and the SSH-free
monitor) is pure Go, stdlib only — so there is no `python3` / `wandb` / `huggingface_hub`
dependency.

## Rules for agents

- **Never hold a streaming session** (`follow`, `shell`) from an agent. Poll with
  one-shot verbs — each returns immediately, so a disconnect just means "run the last
  command again".
- **Prefer the no-SSH path for monitoring.** If the job logs to WandB and pushes
  artifacts to HF, `claude-ping monitor` reads both from your laptop — zero SSH.
- **For remote-side health without WandB,** `claude-ping status` `cat`s the
  `status.json` written by `claude-ping heartbeat` (GPU util, checkpoint age, last log
  line).

## Build

```bash
make                 # -> ./claude-ping for this machine (drives SSH from your laptop)
make linux           # -> dist/claude-ping-linux-amd64 (deploy this for `heartbeat`)
make install         # -> $GOBIN/claude-ping
```

Requires Go 1.26+. No third-party modules.

## Setup

```bash
cp claude-ping.example.json claude-ping.json    # then edit host/port/key/paths
# (or configure entirely via env: PING_HOST, PING_PORT, PING_KEY, PING_REMOTE_DIR, …)
```

Config precedence: env `PING_*` > `./claude-ping.json` > defaults. Point at another
file with `CLAUDE_PING_CONFIG=/path/to.json`.

## Usage

```bash
./claude-ping up                 # open the persistent master connection (once)
./claude-ping exec "nvidia-smi"  # run a command on the remote (retries, reuses tunnel)
./claude-ping logs 200           # last 200 lines of train_log (one-shot — returns)
./claude-ping status             # cat the heartbeat status.json
./claude-ping gpu                # one-shot nvidia-smi summary
./claude-ping sync               # rsync local_dir -> remote_dir (same tunnel)
./claude-ping env-sync           # push secrets/env (secret_keys or .env) to remote_env_file (600)
./claude-ping launch             # run the configured launch_cmd in the background
./claude-ping bootstrap          # run the configured bootstrap_cmd
./claude-ping down               # close the master when done

# SSH-FREE monitoring (WandB GraphQL + HuggingFace HTTP, no packages needed)
./claude-ping monitor --history 8
```

`follow` (blocking `tail -f`) and `shell` (interactive) exist for **humans**, not agents.

## Remote-side heartbeat

Cross-compile for the remote box (`make linux`), copy the binary over (e.g. via
`claude-ping sync`), and start it in the background on the remote box so `status.json`
stays fresh:

```bash
LOG_DIR=/work/out TRAIN_LOG=/work/train.log PROC_MATCH=train.py \
  nohup ./claude-ping-linux-amd64 heartbeat >/work/heartbeat.log 2>&1 &
```

It writes `status.json` every 30 s (GPU, newest-checkpoint age, last log line, disk)
and self-exits once the watched process has been seen and is then gone.

## Files

| file | role |
|---|---|
| `main.go` | CLI dispatch + usage |
| `config.go` | resolve config (env `PING_*` > `claude-ping.json` > defaults) |
| `ping.go` | persistent-SSH driver (`up/check/exec/logs/status/gpu/sync/env-sync/launch/bootstrap/shell/follow/down`) |
| `heartbeat.go` | remote-side: write `status.json` (GPU/ckpt/log/disk) every N seconds |
| `hf.go` / `wandb.go` / `monitor.go` | local, SSH-free status from WandB metrics + HF artifact freshness |
| `claude-ping.example.json` | config template (copy to `claude-ping.json`, gitignored) |
