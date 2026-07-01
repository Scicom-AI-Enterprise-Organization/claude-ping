package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runMonitor is the SSH-free status reader: WandB metrics + HuggingFace artifact
// freshness, read directly from your laptop. Port of train_status.py.
func runMonitor(cfg MonitorConfig, args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: claude-ping monitor [flags]")
		fmt.Fprintln(os.Stderr, "  SSH-free status from WandB metrics + HF artifact freshness.")
		fs.PrintDefaults()
	}
	project := fs.String("project", cfg.WandbProject, "wandb project")
	run := fs.String("run", cfg.WandbRun, "wandb run display name")
	entity := fs.String("entity", cfg.WandbEntity, "wandb entity (default: your default entity)")
	metric := fs.String("metric", cfg.Metric, "metric to highlight / chart")
	repo := fs.String("repo", cfg.HFRepo, "HF repo")
	files := fs.String("files", strings.Join(cfg.HFFiles, ","), "comma-separated HF files to show (default: all)")
	history := fs.Int("history", 0, "also print last N points of --metric")
	if err := fs.Parse(args); err != nil {
		return err
	}

	loadDotenv()
	wandbStatus(*project, *run, *entity, *metric, *history)
	fmt.Println()

	var fileList []string
	for _, f := range strings.Split(*files, ",") {
		if f = strings.TrimSpace(f); f != "" {
			fileList = append(fileList, f)
		}
	}
	hfStatus(*repo, fileList)
	return nil
}

// loadDotenv loads .env from the cwd or the executable's dir (first found) without
// overriding variables already present in the environment (setdefault semantics).
func loadDotenv() {
	dirs := []string{}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		p := filepath.Join(d, ".env")
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			k, v, _ := strings.Cut(line, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if _, ok := os.LookupEnv(k); !ok {
				_ = os.Setenv(k, v)
			}
		}
		return
	}
}
