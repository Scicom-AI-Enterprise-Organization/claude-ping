package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// doneMarker is echoed by the remote probe only when every watch condition holds.
// It's deliberately unlikely to appear in normal log output.
const doneMarker = "__CPING_DONE__"

// runCapture runs a remote command over the master tunnel and returns its combined
// stdout+stderr. Unlike runStreamed it captures output instead of wiring it to the
// terminal, so the poll loop can test for a marker.
func (d *driver) runCapture(remoteCmd string) (string, error) {
	cmd := exec.Command("ssh", d.sshArgs(remoteCmd)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// watch polls the remote box until a completion condition is met, then returns
// (exit 0). It NEVER holds a streaming session — each poll is a single one-shot
// probe, and the tunnel is idle between polls — so it stays within the agent rules
// while still blocking until "done". Intended to be run in the BACKGROUND by an
// agent: when it exits, the harness re-invokes the agent with the result, i.e.
// claude-ping "pings you" when the job finishes.
//
// Conditions (may be combined; ALL must hold to be done):
//
//	--done-file PATH    a remote file exists (e.g. a job's .done sentinel)
//	--log-contains STR  a fixed substring appears in --log
//	--no-proc PATTERN   no remote process matches `pgrep -f PATTERN` (job exited)
func (d *driver) watch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: claude-ping watch [flags]  (run in the background; exits 0 when done)")
		fmt.Fprintln(os.Stderr, "  Poll the remote until a completion condition holds, then exit.")
		fs.PrintDefaults()
	}
	doneFile := fs.String("done-file", "", "remote path whose existence means done")
	logContains := fs.String("log-contains", "", "fixed substring in --log that means done")
	noProc := fs.String("no-proc", "", "pgrep -f pattern; done when no match remains (job exited)")
	logPath := fs.String("log", d.cfg.TrainLog, "log file for --log-contains and the per-poll progress line")
	interval := fs.Duration("interval", 60*time.Second, "poll interval (e.g. 30s, 2m)")
	timeout := fs.Duration("timeout", 0, "give up after this long (0 = wait forever)")
	tailN := fs.Int("tail", 20, "log lines to print when done")
	label := fs.String("label", "job", "name shown in progress/done output")
	quiet := fs.Bool("quiet", false, "suppress the per-poll progress line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *doneFile == "" && *logContains == "" && *noProc == "" {
		fs.Usage()
		return fmt.Errorf("watch needs at least one condition (--done-file / --log-contains / --no-proc)")
	}
	if *logContains != "" && *logPath == "" {
		return fmt.Errorf("--log-contains needs --log (or a configured train_log)")
	}

	// Make sure the master is up so probes are cheap and reconnect is handled.
	if err := d.control("check"); err != nil {
		if err := d.up(); err != nil {
			return err
		}
	}

	probe := buildProbe(*doneFile, *logContains, *noProc, *logPath)
	start := time.Now()
	fmt.Printf("[watch] %s — polling every %s%s\n", *label, *interval,
		timeoutSuffix(*timeout))

	for {
		out, err := d.runCapture(probe)
		elapsed := time.Since(start).Round(time.Second)
		if err != nil {
			// A dropped tunnel / transient ssh error is not fatal for a long
			// watch — warn and keep polling; the master auto-reconnects.
			fmt.Fprintf(os.Stderr, "[watch] %s +%s: probe error (%v) — retrying\n",
				*label, elapsed, err)
		} else if strings.Contains(out, doneMarker) {
			fmt.Printf("[watch] ✅ %s DONE after %s\n", *label, elapsed)
			if *logPath != "" {
				fmt.Printf("---- last %d lines of %s ----\n", *tailN, *logPath)
				if tail, _ := d.runCapture(fmt.Sprintf("tail -n %d %s 2>/dev/null",
					*tailN, shellQuote(*logPath))); tail != "" {
					fmt.Print(tail)
					if !strings.HasSuffix(tail, "\n") {
						fmt.Println()
					}
				}
			}
			return nil
		} else if !*quiet {
			last := lastNonMarkerLine(out)
			if last != "" {
				fmt.Printf("[watch] %s +%s | %s\n", *label, elapsed, last)
			} else {
				fmt.Printf("[watch] %s +%s | still running\n", *label, elapsed)
			}
		}

		sleep := *interval
		if *timeout > 0 {
			rem := *timeout - time.Since(start)
			if rem <= 0 {
				return fmt.Errorf("watch: %s not done after %s (timeout)", *label, *timeout)
			}
			if rem < sleep { // land the final poll right at the deadline
				sleep = rem
			}
		}
		time.Sleep(sleep)
	}
}

// buildProbe assembles a single remote sh command that echoes doneMarker iff every
// requested condition holds, followed by the log's last line (for a progress
// readout). Values are single-quoted so paths/patterns with spaces are safe.
func buildProbe(doneFile, logContains, noProc, logPath string) string {
	var b strings.Builder
	b.WriteString("ok=1; ")
	if doneFile != "" {
		fmt.Fprintf(&b, "[ -e %s ] || ok=0; ", shellQuote(doneFile))
	}
	if logContains != "" {
		fmt.Fprintf(&b, "grep -qaF -- %s %s 2>/dev/null || ok=0; ",
			shellQuote(logContains), shellQuote(logPath))
	}
	if noProc != "" {
		// `pgrep -f` matches against full command lines — including THIS probe's
		// own shell, whose argv contains the pattern (the classic self-match
		// footgun). Exclude our own pid ($$) so the probe doesn't see itself as
		// the still-running job; if any OTHER match remains, the job is alive.
		fmt.Fprintf(&b, `if pgrep -f -- %s 2>/dev/null | grep -vxF "$$" | grep -q .; then ok=0; fi; `,
			shellQuote(noProc))
	}
	fmt.Fprintf(&b, `[ "$ok" = 1 ] && echo %s; `, doneMarker)
	if logPath != "" {
		fmt.Fprintf(&b, "tail -n 1 %s 2>/dev/null; ", shellQuote(logPath))
	}
	b.WriteString("true")
	return b.String()
}

// lastNonMarkerLine returns the last non-empty line of out that isn't the marker.
func lastNonMarkerLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s != "" && !strings.Contains(s, doneMarker) {
			return s
		}
	}
	return ""
}

func timeoutSuffix(t time.Duration) string {
	if t <= 0 {
		return ""
	}
	return fmt.Sprintf(", timeout %s", t)
}
