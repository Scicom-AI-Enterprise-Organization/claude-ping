package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"
)

// wandbStatus prints the latest summary metrics (and optionally history) for a run,
// querying the WandB GraphQL API directly — no `wandb` package required.
//
// Auth: HTTP Basic with username "api" and password WANDB_API_KEY. Base URL is
// WANDB_BASE_URL (default https://api.wandb.ai).
func wandbStatus(project, runName, entity, metric string, history int) {
	if project == "" {
		fmt.Println("WandB   (skip: no project configured — set monitor.wandb_project or --project)")
		return
	}
	key := os.Getenv("WANDB_API_KEY")
	if key == "" {
		fmt.Println("WandB   (skip: set WANDB_API_KEY)")
		return
	}
	c := &wandbClient{key: key, base: envOr("WANDB_BASE_URL", "https://api.wandb.ai")}

	if entity == "" {
		entity = os.Getenv("WANDB_ENTITY")
	}
	if entity == "" {
		ent, err := c.defaultEntity()
		if err != nil {
			fmt.Printf("WandB   (cannot resolve entity: %v)\n", err)
			return
		}
		entity = ent
	}
	path := entity + "/" + project

	run, err := c.findRun(entity, project, runName)
	if err != nil {
		fmt.Printf("WandB   (no run %q in %s: %v)\n", runName, path, err)
		return
	}

	summary := map[string]any{}
	if run.SummaryMetrics != "" {
		_ = json.Unmarshal([]byte(run.SummaryMetrics), &summary)
	}

	fmt.Printf("WandB   %s/%s  state=%s\n", path, run.DisplayName, run.State)
	if step, ok := summary["_step"]; ok && step != nil {
		if f, ok := step.(float64); ok {
			fmt.Printf("  %-16s %s\n", "step", fmtNum(f))
		}
	}

	nums := map[string]float64{}
	for k, v := range summary {
		if len(k) > 0 && k[0] == '_' {
			continue
		}
		if f, ok := v.(float64); ok { // JSON numbers only (bools are bool, not float64)
			nums[k] = f
		}
	}
	keys := make([]string, 0, len(nums))
	for k := range nums {
		keys = append(keys, k)
	}
	// metric first, then alphabetical (matches Python's sort key).
	sort.Slice(keys, func(i, j int) bool {
		mi, mj := keys[i] != metric, keys[j] != metric
		if mi != mj {
			return !mi
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 14 {
		keys = keys[:14]
	}
	for _, k := range keys {
		fmt.Printf("  %-16s %.4f\n", k, nums[k])
	}

	if history > 0 && metric != "" {
		rows, err := c.sampledHistory(entity, project, run.Name, metric, history)
		if err != nil {
			fmt.Printf("  (history unavailable: %v)\n", err)
			return
		}
		var pts []map[string]any
		for _, r := range rows {
			if v, ok := r[metric]; ok && v != nil {
				pts = append(pts, r)
			}
		}
		if len(pts) > 0 {
			fmt.Printf("  --- last %d %s ---\n", len(pts), metric)
			for _, r := range pts {
				step := ""
				if s, ok := r["_step"].(float64); ok {
					step = fmtNum(s)
				}
				if v, ok := r[metric].(float64); ok {
					fmt.Printf("    step %s: %s=%.4f\n", step, metric, v)
				}
			}
		}
	}
}

type wandbClient struct {
	key  string
	base string
}

type wandbRun struct {
	Name           string // run id (used for history lookups)
	DisplayName    string
	State          string
	SummaryMetrics string
}

// graphql POSTs a query and unmarshals response.data into out.
func (c *wandbClient) graphql(query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("api", c.key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("HTTP %d: %v", resp.StatusCode, err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("%s", env.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func (c *wandbClient) defaultEntity() (string, error) {
	var out struct {
		Viewer struct {
			Entity string `json:"entity"`
		} `json:"viewer"`
	}
	if err := c.graphql(`query { viewer { entity } }`, nil, &out); err != nil {
		return "", err
	}
	if out.Viewer.Entity == "" {
		return "", fmt.Errorf("no default entity")
	}
	return out.Viewer.Entity, nil
}

func (c *wandbClient) findRun(entity, project, runName string) (*wandbRun, error) {
	const q = `query Run($entity: String!, $project: String!, $filters: JSONString, $order: String) {
      project(name: $project, entityName: $entity) {
        runs(filters: $filters, order: $order, first: 1) {
          edges { node { name displayName state summaryMetrics } }
        }
      }
    }`
	filters, _ := json.Marshal(map[string]string{"display_name": runName})
	vars := map[string]any{
		"entity":  entity,
		"project": project,
		"filters": string(filters),
		"order":   "-created_at",
	}
	var out struct {
		Project *struct {
			Runs struct {
				Edges []struct {
					Node struct {
						Name           string `json:"name"`
						DisplayName    string `json:"displayName"`
						State          string `json:"state"`
						SummaryMetrics string `json:"summaryMetrics"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"runs"`
		} `json:"project"`
	}
	if err := c.graphql(q, vars, &out); err != nil {
		return nil, err
	}
	if out.Project == nil || len(out.Project.Runs.Edges) == 0 {
		return nil, fmt.Errorf("not found")
	}
	n := out.Project.Runs.Edges[0].Node
	return &wandbRun{Name: n.Name, DisplayName: n.DisplayName, State: n.State, SummaryMetrics: n.SummaryMetrics}, nil
}

func (c *wandbClient) sampledHistory(entity, project, runID, metric string, samples int) ([]map[string]any, error) {
	const q = `query Hist($entity: String!, $project: String!, $run: String!, $specs: [JSONString!]!) {
      project(name: $project, entityName: $entity) {
        run(name: $run) { sampledHistory(specs: $specs) }
      }
    }`
	spec, _ := json.Marshal(map[string]any{"keys": []string{"_step", metric}, "samples": samples})
	vars := map[string]any{
		"entity":  entity,
		"project": project,
		"run":     runID,
		"specs":   []string{string(spec)},
	}
	var out struct {
		Project struct {
			Run struct {
				SampledHistory [][]map[string]any `json:"sampledHistory"`
			} `json:"run"`
		} `json:"project"`
	}
	if err := c.graphql(q, vars, &out); err != nil {
		return nil, err
	}
	if len(out.Project.Run.SampledHistory) == 0 {
		return nil, nil
	}
	return out.Project.Run.SampledHistory[0], nil
}

// fmtNum renders a float without a trailing ".0" when it is integral.
func fmtNum(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
