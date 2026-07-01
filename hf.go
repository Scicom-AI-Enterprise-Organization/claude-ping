package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// hfStatus prints HuggingFace repo freshness + file sizes, reading the public HTTP
// API directly (no huggingface_hub package). HF_TOKEN is used if present.
func hfStatus(repo string, files []string) {
	if repo == "" {
		fmt.Println("HF      (skip: no repo configured — set monitor.hf_repo or --repo)")
		return
	}
	info, err := hfRepoInfo(repo)
	if err != nil {
		fmt.Printf("HF      (%s: %v)\n", repo, err)
		return
	}

	when := info.LastModified
	if lm, perr := time.Parse(time.RFC3339, info.LastModified); perr == nil {
		when = fmt.Sprintf("%s (%s)", lm.UTC().Format("2006-01-02 15:04:05 UTC"), ageString(lm))
	}
	fmt.Printf("HF      %s  last push: %s\n", repo, when)

	want := map[string]bool{}
	for _, f := range files {
		want[f] = true
	}
	for _, sib := range info.Siblings {
		if len(want) == 0 || want[sib.RFilename] {
			gb := float64(sib.Size) / 1e9
			fmt.Printf("  %-24s %.2f GB\n", sib.RFilename, gb)
		}
	}
}

type hfInfo struct {
	LastModified string `json:"lastModified"`
	Siblings     []struct {
		RFilename string `json:"rfilename"`
		Size      int64  `json:"size"`
	} `json:"siblings"`
}

func hfRepoInfo(repo string) (*hfInfo, error) {
	endpoint := os.Getenv("HF_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://huggingface.co"
	}
	// blobs=true so siblings carry file sizes. The repo ("org/name") is a path — do
	// not URL-encode its slash.
	u := fmt.Sprintf("%s/api/models/%s?blobs=true", endpoint, repo)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if tok := os.Getenv("HF_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(string(body)))
	}
	var info hfInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ageString renders a human "N ago" string (mirrors train_status.py _age).
func ageString(dt time.Time) string {
	secs := time.Since(dt).Seconds()
	switch {
	case secs < 90:
		return fmt.Sprintf("%ds ago", int(secs))
	case secs < 5400:
		return fmt.Sprintf("%dm ago", int(secs/60))
	default:
		return fmt.Sprintf("%.1fh ago", secs/3600)
	}
}
