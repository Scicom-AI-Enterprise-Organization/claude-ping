package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Remote-side heartbeat: write a status.json every INTERVAL seconds so an agent can
// read health with a single `cat` (claude-ping status) instead of holding an SSH
// session. Best-effort — never interferes with the job it watches.
//
// Env (defaults in brackets):
//
//	LOG_DIR [.]            dir to write status.json into and scan for checkpoints
//	STATUS_JSON [$LOG_DIR/status.json]   output path
//	TRAIN_LOG [$LOG_DIR/train.log]       log file whose last line is reported
//	STATUS_INTERVAL [30]  seconds between writes
//	PROC_MATCH [train.py] pgrep -f pattern used to detect "job running"
//	CKPT_GLOB [$LOG_DIR/*.ckpt]  glob; newest match's mtime -> ckpt freshness
//
// Exits once the job has been seen running and is then gone for ~10 cycles.

type gpuStat struct {
	Util     *float64 `json:"util_pct"`
	MemUsed  *float64 `json:"mem_used_mb"`
	MemTotal *float64 `json:"mem_total_mb"`
	Temp     *float64 `json:"temp_c"`
	Power    *float64 `json:"power_w"`
}

type ckptStat struct {
	Path   *string `json:"path"`
	Exists bool    `json:"exists"`
	Mtime  int64   `json:"mtime"`
	AgeSec *int64  `json:"age_sec"`
}

type logStat struct {
	LastLine string `json:"last_line"`
	Lines    int    `json:"lines"`
}

type diskStat struct {
	Used  *float64 `json:"used"`
	Avail *float64 `json:"avail"`
}

type status struct {
	TS         int64    `json:"ts"`
	TSHuman    string   `json:"ts_human"`
	JobRunning bool     `json:"job_running"`
	GPU        *gpuStat `json:"gpu"`
	Checkpoint ckptStat `json:"checkpoint"`
	Log        logStat  `json:"log"`
	DiskKB     diskStat `json:"disk_kb"`
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runHeartbeat() error {
	logDir := envOr("LOG_DIR", ".")
	statusJSON := envOr("STATUS_JSON", logDir+"/status.json")
	trainLog := envOr("TRAIN_LOG", logDir+"/train.log")
	procMatch := envOr("PROC_MATCH", "train.py")
	ckptGlob := envOr("CKPT_GLOB", logDir+"/*.ckpt")

	interval := 30 * time.Second
	if v := os.Getenv("STATUS_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[heartbeat] writing %s every %v (proc=%q)\n", statusJSON, interval, procMatch)

	seen, missing := false, 0
	for {
		running := procRunning(procMatch)
		if running {
			seen, missing = true, 0
		} else {
			missing++
		}

		now := time.Now()
		st := status{
			TS:         now.Unix(),
			TSHuman:    now.Format("2006-01-02 15:04:05"),
			JobRunning: running,
			GPU:        readGPU(),
			Checkpoint: newestCheckpoint(ckptGlob, now),
			Log:        readLog(trainLog),
			DiskKB:     readDisk(logDir),
		}
		if err := writeStatus(statusJSON, st); err != nil {
			fmt.Fprintf(os.Stderr, "[heartbeat] write failed: %v\n", err)
		}

		if seen && missing >= 10 {
			fmt.Fprintf(os.Stderr, "[heartbeat] %q gone for %d cycles — exiting\n", procMatch, missing)
			return nil
		}
		time.Sleep(interval)
	}
}

// procRunning mirrors `pgrep -f <pattern>`: true if any process's full command line
// contains the pattern.
func procRunning(pattern string) bool {
	// pgrep is present on both Linux and macOS; -f matches the full command line.
	return exec.Command("pgrep", "-f", pattern).Run() == nil
}

// readGPU runs nvidia-smi; nil if unavailable (no GPU / no driver).
func readGPU() *gpuStat {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	line := firstLine(string(out))
	if line == "" {
		return nil
	}
	f := strings.Split(line, ",")
	get := func(i int) *float64 {
		if i < len(f) {
			return numPtr(strings.TrimSpace(f[i]))
		}
		return nil
	}
	return &gpuStat{Util: get(0), MemUsed: get(1), MemTotal: get(2), Temp: get(3), Power: get(4)}
}

// newestCheckpoint finds the newest file matching glob and reports its freshness.
func newestCheckpoint(glob string, now time.Time) ckptStat {
	matches, _ := filepath.Glob(glob)
	var newest string
	var mtime time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(mtime) {
			newest, mtime = m, info.ModTime()
		}
	}
	if newest == "" {
		return ckptStat{Exists: false}
	}
	mt := mtime.Unix()
	age := now.Unix() - mt
	return ckptStat{Path: &newest, Exists: true, Mtime: mt, AgeSec: &age}
}

// readLog reports the last line and total line count, mirroring `tail -n 1` +
// `wc -l`. A missing file yields an empty line and zero count.
func readLog(path string) logStat {
	data, err := os.ReadFile(path)
	if err != nil {
		return logStat{}
	}
	s := string(data)
	lines := strings.Count(s, "\n") // wc -l counts newline bytes
	// tail -n 1: drop one trailing newline, then take content after the last newline.
	s = strings.TrimSuffix(s, "\n")
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return logStat{LastLine: s, Lines: lines}
}

// readDisk reports used/available KiB for the filesystem holding dir (df -Pk).
func readDisk(dir string) diskStat {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return diskStat{}
	}
	bsize := uint64(st.Bsize)
	used := float64((uint64(st.Blocks)-uint64(st.Bfree))*bsize) / 1024
	avail := float64(uint64(st.Bavail)*bsize) / 1024
	return diskStat{Used: &used, Avail: &avail}
}

func writeStatus(path string, st status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// numPtr parses s as a float; nil on empty/unparseable (Python's num()).
func numPtr(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}
