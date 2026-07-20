// claude-ping — keep a Claude agent (or any script) reliably connected to a remote
// instance over ONE persistent SSH connection, reused by every command.
//
// Why: agents driving a remote box over SSH are flaky when they (a) hold streaming
// sessions like `tail -f` that never return, and (b) open a fresh connection per
// call with no keepalive/retry. claude-ping fixes both: it keeps a single SSH master
// socket alive in the background (ControlMaster + ControlPersist=yes) so every later
// command rides the same tunnel with no re-handshake, auto-reconnects if it drops,
// and every verb returns immediately (no streaming).
//
// GOLDEN RULE for agents: never hold a streaming session (no `follow`). Poll with
// one-shot verbs; a disconnect just means "run the last command again". For metrics,
// prefer the SSH-free reader (`claude-ping monitor`) over shelling into the box.
package main

import (
	"fmt"
	"os"
	"strconv"
)

const usage = `usage: claude-ping <command> [args]

Remote SSH driver (config: env PING_* > ./claude-ping.json > defaults):
  up                 open the persistent master connection (once)
  check              is the master connection alive?
  down               close the master connection
  exec <cmd...>      run a command on the remote (retries, reuses tunnel)
  logs [N]           last N lines of the log file (one-shot; default 120)
  status             cat the heartbeat status.json (GPU/ckpt/log)
  gpu                one-shot nvidia-smi summary
  sync               rsync local_dir -> remote_dir (same tunnel)
  env-sync           push secret env vars (secret_keys / local .env) -> remote_env_file (600)
  proxy [ports...]   forward local->remote ports over the master (PORT or LOCAL:REMOTE)
  proxy-stop [ports] cancel forwards added by 'proxy' (defaults to configured ports)
  launch             run the configured launch_cmd in the background
  bootstrap          run the configured bootstrap_cmd
  watch [flags]      poll until a job is done, then exit 0 (run in background;
                     --done-file / --log-contains / --no-proc, --interval, --timeout)
  shell              interactive shell (humans)
  follow [N]         tail -f (humans only — blocks; NOT for agents)

Remote-side heartbeat (env-driven; see 'heartbeat' section):
  heartbeat          write status.json every N seconds until the job is gone

SSH-free monitoring:
  monitor [flags]    WandB metrics + HF artifact freshness (no SSH)

  version            print version and exit
  help               show this message`

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	cmd := os.Args[1]

	// Subcommands that don't need the SSH config resolved.
	switch cmd {
	case "help", "-h", "--help":
		fmt.Println(usage)
		return
	case "version", "--version", "-v":
		fmt.Printf("claude-ping %s\n", version)
		return
	case "heartbeat":
		if err := runHeartbeat(); err != nil {
			fatal(err)
		}
		return
	case "monitor":
		if err := runMonitor(LoadConfig().Monitor, os.Args[2:]); err != nil {
			os.Exit(1) // flag package already printed the error
		}
		return
	}

	cfg := LoadConfig()
	d, err := newDriver(cfg)
	if err != nil {
		fatal(err)
	}

	switch cmd {
	case "up":
		err = d.up()
	case "check":
		err = d.check()
	case "down":
		err = d.down()
	case "exec":
		err = d.exec(joinArgs(os.Args[2:]))
	case "logs":
		err = d.logs(intArg(2, 120))
	case "status":
		err = d.status()
	case "gpu":
		err = d.gpu()
	case "sync":
		err = d.sync()
	case "env-sync":
		err = d.envSync()
	case "proxy":
		err = d.proxy(os.Args[2:])
	case "proxy-stop":
		err = d.proxyStop(os.Args[2:])
	case "launch":
		err = d.launch()
	case "bootstrap":
		err = d.bootstrap()
	case "watch":
		err = d.watch(os.Args[2:])
	case "shell":
		err = d.shell()
	case "follow":
		err = d.follow(intArg(2, 80))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, usage)
		os.Exit(1)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[claude-ping] %v\n", err)
	os.Exit(1)
}

// joinArgs collapses the exec command args into a single string, matching the bash
// `"$*"` behavior (ssh runs it through the remote shell).
func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// intArg parses os.Args[idx] as an int, returning def if absent or invalid.
func intArg(idx, def int) int {
	if len(os.Args) > idx {
		if n, err := strconv.Atoi(os.Args[idx]); err == nil {
			return n
		}
	}
	return def
}
