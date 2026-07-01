package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// driver wraps the system `ssh`/`rsync` binaries with SSH connection multiplexing.
//
// We deliberately shell out rather than use a native Go SSH client: the whole point
// of claude-ping is a master connection that PERSISTS across separate CLI
// invocations (ControlMaster + ControlPersist=yes). A native connection would die
// with the process; only the OS ssh master socket survives between commands.
type driver struct {
	cfg      Config
	hostspec string   // user@host
	sshOpts  []string // shared ssh options (multiplexing + keepalive)
}

func newDriver(cfg Config) (*driver, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("no host — set PING_HOST or create claude-ping.json (see claude-ping.example.json)")
	}
	home, _ := os.UserHomeDir()
	// Best-effort: ControlPath lives under ~/.ssh.
	if home != "" {
		_ = os.MkdirAll(home+"/.ssh", 0o700)
	}
	opts := []string{
		"-p", cfg.Port, "-i", cfg.Key,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + home + "/.ssh/cm-cping-%h-%p", "-o", "ControlPersist=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4", "-o", "TCPKeepAlive=yes",
		"-o", "ConnectTimeout=15",
	}
	return &driver{cfg: cfg, hostspec: cfg.User + "@" + cfg.Host, sshOpts: opts}, nil
}

// sshArgs builds the argv for an `ssh` invocation with the shared options.
func (d *driver) sshArgs(extra ...string) []string {
	args := append([]string{}, d.sshOpts...)
	args = append(args, d.hostspec)
	return append(args, extra...)
}

// runStreamed runs ssh <opts> host <remoteCmd> with stdio wired to the terminal.
func (d *driver) runStreamed(remoteCmd string) error {
	var extra []string
	if remoteCmd != "" {
		extra = []string{remoteCmd}
	}
	cmd := exec.Command("ssh", d.sshArgs(extra...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// retry runs fn up to 3 times with exponential backoff (2s, 4s), matching the
// original _retry. The master tunnel makes retries cheap and usually unnecessary.
func retry(label string, fn func() error) error {
	const max = 3
	delay := 2 * time.Second
	var err error
	for n := 1; n <= max; n++ {
		if err = fn(); err == nil {
			return nil
		}
		if n >= max {
			fmt.Fprintf(os.Stderr, "[claude-ping] failed after %d tries: %s\n", max, label)
			return err
		}
		fmt.Fprintf(os.Stderr, "[claude-ping] transient failure — retry %d/%d in %v…\n", n, max, delay)
		time.Sleep(delay)
		delay *= 2
	}
	return err
}

// need returns an error if a required config value is empty.
func need(val, name string) error {
	if val == "" {
		return fmt.Errorf("%s not configured", name)
	}
	return nil
}

// --- verbs ------------------------------------------------------------------

func (d *driver) up() error {
	if err := retry("ssh true", func() error { return d.runStreamed("true") }); err != nil {
		return fmt.Errorf("could not open master connection")
	}
	if err := d.control("check"); err != nil {
		return fmt.Errorf("could not open master connection")
	}
	fmt.Printf("[claude-ping] master connection OPEN -> %s (persists until 'down')\n", d.hostspec)
	return nil
}

func (d *driver) check() error {
	if err := d.control("check"); err != nil {
		fmt.Println("[claude-ping] no master connection (a command will auto-open one)")
		return err
	}
	fmt.Printf("[claude-ping] master connection ALIVE -> %s\n", d.hostspec)
	return nil
}

func (d *driver) down() error {
	if err := d.control("exit"); err != nil {
		fmt.Println("[claude-ping] no master connection to close")
		return nil
	}
	fmt.Println("[claude-ping] master connection closed")
	return nil
}

// control runs `ssh -O <op>` (check/exit) against the master, suppressing output.
func (d *driver) control(op string) error {
	args := append([]string{}, d.sshOpts...)
	args = append(args, "-O", op, d.hostspec)
	cmd := exec.Command("ssh", args...)
	return cmd.Run()
}

func (d *driver) exec(remoteCmd string) error {
	return retry(remoteCmd, func() error { return d.runStreamed(remoteCmd) })
}

func (d *driver) logs(n int) error {
	if err := need(d.cfg.TrainLog, "train_log"); err != nil {
		return err
	}
	cmd := fmt.Sprintf("tail -n %d %s", n, d.cfg.TrainLog)
	return retry(cmd, func() error { return d.runStreamed(cmd) })
}

func (d *driver) status() error {
	if err := need(d.cfg.StatusJSON, "status_json"); err != nil {
		return err
	}
	cmd := fmt.Sprintf("cat %s 2>/dev/null || echo '{\"error\":\"no status.json yet\"}'", d.cfg.StatusJSON)
	return retry(cmd, func() error { return d.runStreamed(cmd) })
}

func (d *driver) gpu() error {
	const cmd = "nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw --format=csv,noheader 2>/dev/null || echo 'no nvidia-smi'"
	return retry(cmd, func() error { return d.runStreamed(cmd) })
}

func (d *driver) launch() error {
	if err := need(d.cfg.LaunchCmd, "launch_cmd"); err != nil {
		return err
	}
	fmt.Println("[claude-ping] launch")
	return retry(d.cfg.LaunchCmd, func() error { return d.runStreamed(d.cfg.LaunchCmd) })
}

func (d *driver) bootstrap() error {
	if err := need(d.cfg.BootstrapCmd, "bootstrap_cmd"); err != nil {
		return err
	}
	return retry(d.cfg.BootstrapCmd, func() error { return d.runStreamed(d.cfg.BootstrapCmd) })
}

func (d *driver) shell() error {
	return d.runStreamed("")
}

func (d *driver) follow(n int) error {
	if err := need(d.cfg.TrainLog, "train_log"); err != nil {
		return err
	}
	// Streaming (tail -f): humans only. No retry — it blocks by design.
	return d.runStreamed(fmt.Sprintf("tail -n %d -f %s", n, d.cfg.TrainLog))
}

func (d *driver) sync() error {
	if err := need(d.cfg.RemoteDir, "remote_dir"); err != nil {
		return err
	}
	fmt.Printf("[claude-ping] rsync %s -> %s:%s\n", d.cfg.LocalDir, d.hostspec, d.cfg.RemoteDir)
	rsh := "ssh " + strings.Join(d.sshOpts, " ")
	args := []string{"-az", "--delete", "-e", rsh}
	for _, e := range d.cfg.SyncExcludes {
		args = append(args, "--exclude", e)
	}
	args = append(args, d.cfg.LocalDir+"/", d.hostspec+":"+d.cfg.RemoteDir+"/")
	return retry("rsync", func() error {
		cmd := exec.Command("rsync", args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})
}
