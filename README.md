# claude-ping

Keep a Claude Code agent (or any script) **reliably connected to a remote instance**
over **one persistent SSH connection**, instead of re-connecting on every command.

A single self-contained **Go** binary — no runtime dependencies beyond the system
`ssh`/`rsync`. The remote heartbeat and the SSH-free WandB/HuggingFace monitor are all
pure Go (stdlib only): no `python3`, `wandb`, or `huggingface_hub` required.

## Why

Agents driving a remote box over SSH are flaky because they (1) hold streaming
sessions like `tail -f` that never return, and (2) open a fresh connection per command
with no keepalive or retry. claude-ping fixes both: it keeps a single SSH master socket
alive in the background (`ControlMaster` + `ControlPersist=yes`) so every later command
rides the same tunnel with no re-handshake, auto-reconnects if it drops, and every verb
returns immediately.

## Install

### Download a prebuilt binary

Grab the latest release for your platform (Linux amd64/arm64, macOS arm64) from the
[Releases page](https://github.com/Scicom-AI-Enterprise-Organization/claude-ping/releases),
or via `curl`:

```bash
REPO=Scicom-AI-Enterprise-Organization/claude-ping
VER=v0.1.0                          # latest tag
BIN=claude-ping-darwin-arm64        # pick: linux-amd64 | linux-arm64 | darwin-arm64
base="https://github.com/$REPO/releases/download/$VER"
curl -fsSLO "$base/$BIN"
curl -fsSLO "$base/$BIN.sha256"
sha256sum -c "$BIN.sha256"          # macOS: shasum -a 256 -c "$BIN.sha256"
chmod +x "$BIN" && mv "$BIN" claude-ping
```

### Build from source

```bash
make            # build ./claude-ping for this machine
make install    # or install to $GOBIN
make linux      # cross-compile dist/claude-ping-linux-amd64 for the remote box
```

Requires Go 1.26+. No third-party modules. `claude-ping version` prints the build.

## Configure

```bash
cp claude-ping.example.json claude-ping.json    # edit host/port/key/paths
```

Precedence: env `PING_*` > `./claude-ping.json` > defaults. Override the file path with
`CLAUDE_PING_CONFIG=/path/to.json`.

## Usage

```bash
claude-ping up                 # open the persistent master connection (once)
claude-ping exec "nvidia-smi"  # run a command on the remote (retries, reuses tunnel)
claude-ping logs 200           # last 200 lines of the log file (one-shot — returns)
claude-ping status             # cat the heartbeat status.json
claude-ping gpu                # one-shot nvidia-smi summary
claude-ping sync               # rsync local_dir -> remote_dir (same tunnel)
claude-ping env-sync           # push secrets/env (secret_keys or local .env) to the remote
claude-ping launch             # run the configured launch_cmd in the background
claude-ping bootstrap          # run the configured bootstrap_cmd
claude-ping down               # close the master when done

claude-ping monitor --history 8    # SSH-free: WandB metrics + HF artifact freshness
claude-ping heartbeat              # remote-side: write status.json every N seconds
```

`follow` (blocking `tail -f`) and `shell` (interactive) are for **humans**, not agents —
agents should poll the one-shot verbs above.

## Sync secrets / env to the remote

Push API keys and other environment variables to the remote box over the same tunnel,
so a `launch_cmd` / `bootstrap_cmd` can `source` them. Values are fed over **stdin, never
the command line**, and land in a file with mode `600`:

```bash
claude-ping env-sync
```

- **Which vars** — the `secret_keys` list in `claude-ping.json` (or `PING_SECRET_KEYS`,
  comma-separated). Leave it empty to sync *every* key in your local `.env`.
- **Where each value comes from** — your shell environment first, falling back to a local
  `.env` (cwd or the binary's dir — copy `.env.example` to `.env`).
- **Destination** — `remote_env_file`, defaulting to `<remote_dir>/.env`.

```bash
# no config file needed — pick the vars inline:
PING_SECRET_KEYS=WANDB_API_KEY,HF_TOKEN claude-ping env-sync
```

## Remote heartbeat

Cross-compile (`make linux`), copy the binary to the remote box, and run it in the
background so `status.json` stays fresh for `claude-ping status`:

```bash
LOG_DIR=/work/out TRAIN_LOG=/work/train.log PROC_MATCH=train.py \
  nohup ./claude-ping-linux-amd64 heartbeat >/work/heartbeat.log 2>&1 &
```

## Monitor (no SSH)

`claude-ping monitor` reads WandB (GraphQL) and HuggingFace (HTTP) directly. It loads
`WANDB_API_KEY` / `HF_TOKEN` / `WANDB_ENTITY` from `.env` (cwd or the binary's dir), and
takes defaults from the `monitor` block of `claude-ping.json`:

```bash
claude-ping monitor --project P --run R --repo org/name --metric val/loss --history 8
claude-ping monitor --files config.json,model.safetensors
```

## License

MIT — see [LICENSE](LICENSE).
