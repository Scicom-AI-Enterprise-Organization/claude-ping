package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the resolved configuration for the SSH driver and the monitor.
//
// Precedence (matching the original bash/python): env PING_* > claude-ping.json
// (CLAUDE_PING_CONFIG, then ./claude-ping.json, then <exedir>/claude-ping.json) >
// built-in defaults.
type Config struct {
	Host          string
	Port          string
	User          string
	Key           string
	RemoteDir     string
	LocalDir      string
	TrainLog      string
	StatusJSON    string
	LaunchCmd     string
	BootstrapCmd  string
	SyncExcludes  []string
	SecretKeys    []string
	RemoteEnvFile string
	ProxyPorts    []string

	Monitor MonitorConfig

	// Source is the path of the config file that was loaded (empty if none).
	Source string
}

// MonitorConfig mirrors the optional "monitor" block of claude-ping.json.
type MonitorConfig struct {
	WandbProject string
	WandbRun     string
	WandbEntity  string
	Metric       string
	HFRepo       string
	HFFiles      []string
}

var defaultSyncExcludes = []string{
	".git", ".venv", "__pycache__", "*.pyc", "node_modules", ".DS_Store",
}

// configCandidates returns the ordered list of config-file paths to try.
func configCandidates() []string {
	var cands []string
	if c := os.Getenv("CLAUDE_PING_CONFIG"); c != "" {
		cands = append(cands, c)
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "claude-ping.json"))
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "claude-ping.json"))
	}
	return cands
}

// loadConfigFile returns the first existing config file parsed into a map, along
// with its path. A missing file yields (nil, "", nil); a malformed file yields a
// warning on stderr but does not abort (matching the original behavior).
func loadConfigFile() (map[string]any, string) {
	for _, c := range configCandidates() {
		if c == "" {
			continue
		}
		info, err := os.Stat(c)
		if err != nil || info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[claude-ping] cannot read config %s: %v\n", c, err)
			return nil, ""
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			fmt.Fprintf(os.Stderr, "[claude-ping] bad config %s: %v\n", c, err)
			return nil, ""
		}
		return m, c
	}
	return nil, ""
}

// LoadConfig resolves the full configuration.
func LoadConfig() Config {
	cfg, src := loadConfigFile()

	// g resolves a single value: env > cfg[key] > def.
	g := func(key, env, def string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		if v := mapStr(cfg, key); v != "" {
			return v
		}
		return def
	}

	c := Config{
		Host:         g("host", "PING_HOST", ""),
		Port:         g("port", "PING_PORT", "22"),
		User:         g("user", "PING_USER", "root"),
		Key:          g("key", "PING_KEY", "~/.ssh/id_rsa"),
		RemoteDir:    g("remote_dir", "PING_REMOTE_DIR", ""),
		LocalDir:     g("local_dir", "PING_LOCAL_DIR", "."),
		LaunchCmd:    g("launch_cmd", "PING_LAUNCH_CMD", ""),
		BootstrapCmd: g("bootstrap_cmd", "PING_BOOTSTRAP_CMD", ""),
		Source:       src,
	}

	// train_log / status_json fall back to <remote_dir>/{train,status}.{log,json}.
	c.TrainLog = g("train_log", "PING_TRAIN_LOG", "")
	if c.TrainLog == "" && c.RemoteDir != "" {
		c.TrainLog = c.RemoteDir + "/train.log"
	}
	c.StatusJSON = g("status_json", "PING_STATUS_JSON", "")
	if c.StatusJSON == "" && c.RemoteDir != "" {
		c.StatusJSON = c.RemoteDir + "/status.json"
	}

	c.SyncExcludes = mapStrSlice(cfg, "sync_excludes", defaultSyncExcludes)

	c.SecretKeys = mapStrSlice(cfg, "secret_keys", nil)
	if v := os.Getenv("PING_SECRET_KEYS"); v != "" {
		c.SecretKeys = splitComma(v)
	}
	c.RemoteEnvFile = g("remote_env_file", "PING_REMOTE_ENV_FILE", "")
	if c.RemoteEnvFile == "" && c.RemoteDir != "" {
		c.RemoteEnvFile = c.RemoteDir + "/.env"
	}

	c.ProxyPorts = mapStrSlice(cfg, "proxy_ports", nil)
	if v := os.Getenv("PING_PROXY_PORTS"); v != "" {
		c.ProxyPorts = splitComma(v)
	}

	if mon, ok := cfg["monitor"].(map[string]any); ok {
		c.Monitor = MonitorConfig{
			WandbProject: mapStr(mon, "wandb_project"),
			WandbRun:     mapStr(mon, "wandb_run"),
			WandbEntity:  mapStr(mon, "wandb_entity"),
			Metric:       mapStr(mon, "metric"),
			HFRepo:       mapStr(mon, "hf_repo"),
			HFFiles:      mapStrSlice(mon, "hf_files", nil),
		}
	}
	if c.Monitor.Metric == "" {
		c.Monitor.Metric = "val/loss"
	}

	// Expand a leading ~ in the key path.
	if strings.HasPrefix(c.Key, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			c.Key = home + c.Key[1:]
		}
	}
	return c
}

// mapStr fetches key from m and renders it as a string. JSON numbers are rendered
// without a trailing ".0"; null / missing / non-scalar values yield "".
func mapStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// mapStrSlice fetches key as a []string, falling back to def when absent.
func mapStrSlice(m map[string]any, key string, def []string) []string {
	if m == nil {
		return def
	}
	arr, ok := m[key].([]any)
	if !ok {
		return def
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// splitComma splits a comma-separated list, trimming spaces and dropping empties.
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
