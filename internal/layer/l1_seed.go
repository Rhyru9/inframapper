// Package layer berisi implementasi tiap layer pipeline InfraMapper.
package layer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/inframapper/internal/model"
	"github.com/yourusername/inframapper/internal/util"
)

// SeedResult adalah output layer 1: daftar subdomain yang sudah deduplicated.
type SeedResult struct {
	Subdomains []*model.Subdomain
	Stats      struct {
		PerSource map[model.Source]int
		Total     int
		Dupes     int
	}
}

// RunSeed menjalankan semua seed source secara paralel dan menggabungkan hasilnya.
// iteration menentukan apakah ini seed awal (0) atau re-seed dari SAN (1, 2).
func RunSeed(ctx context.Context, cfg model.Config, target string, iteration int) (*SeedResult, error) {
	type sourceFunc func(context.Context, string) ([]string, error)

	// Kumpulkan source yang diaktifkan
	sources := map[model.Source]sourceFunc{}
	if cfg.Subfinder {
		sources[model.SourceSubfinder] = runSubfinder
	}
	if cfg.Amass {
		sources[model.SourceAmass] = runAmass
	}
	if cfg.Assetfinder {
		sources[model.SourceAssetfinder] = runAssetfinder
	}
	if cfg.CrtSh {
		sources[model.SourceCrtSh] = runCrtSh
	}

	if len(sources) == 0 {
		return nil, fmt.Errorf("tidak ada seed source yang diaktifkan")
	}

	// Fan-out: jalankan semua source secara paralel
	type sourceResult struct {
		source model.Source
		hosts  []string
		err    error
	}

	resultCh := make(chan sourceResult, len(sources))
	var wg sync.WaitGroup

	for src, fn := range sources {
		wg.Add(1)
		go func(s model.Source, f sourceFunc) {
			defer wg.Done()
			hosts, err := f(ctx, target)
			resultCh <- sourceResult{source: s, hosts: hosts, err: err}
		}(src, fn)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Kumpulkan dan deduplicate
	seen := make(map[string]bool)
	var subdomains []*model.Subdomain
	perSource := make(map[model.Source]int)
	totalRaw := 0
	dupes := 0

	for res := range resultCh {
		if res.err != nil {
			log.Printf("[L1] source %s error: %v", res.source, res.err)
			continue
		}
		perSource[res.source] = len(res.hosts)
		totalRaw += len(res.hosts)

		for _, h := range res.hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || !strings.Contains(h, ".") {
				continue
			}
			if seen[h] {
				dupes++
				continue
			}
			seen[h] = true
			subdomains = append(subdomains, &model.Subdomain{
				Domain:    h,
				Source:    res.source,
				Iteration: iteration,
			})
		}
	}

	result := &SeedResult{Subdomains: subdomains}
	result.Stats.PerSource = perSource
	result.Stats.Total = len(subdomains)
	result.Stats.Dupes = dupes

	if cfg.Verbose {
		log.Printf("[L1] seed selesai: %d subdomain unik dari %d raw (iter=%d)", len(subdomains), totalRaw, iteration)
	}

	return result, nil
}

// --- Implementasi tiap source ---

// runSubfinder menjalankan binary subfinder jika tersedia, fallback ke HTTP API.
func runSubfinder(ctx context.Context, target string) ([]string, error) {
	path, err := exec.LookPath("subfinder")
	if err != nil {
		// Subfinder tidak ada di PATH — log dan skip
		log.Printf("[L1/subfinder] binary tidak ditemukan, skip")
		return nil, nil
	}

	cmd := exec.CommandContext(ctx, path, "-d", target, "-silent", "-all")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("subfinder: %w", err)
	}

	return splitLines(string(out)), nil
}

// runAmass menjalankan binary amass dalam mode passive.
func runAmass(ctx context.Context, target string) ([]string, error) {
	path, err := exec.LookPath("amass")
	if err != nil {
		log.Printf("[L1/amass] binary tidak ditemukan, skip")
		return nil, nil
	}

	cmd := exec.CommandContext(ctx, path, "enum", "-passive", "-d", target, "-silent")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("amass: %w", err)
	}

	return splitLines(string(out)), nil
}

// runAssetfinder menjalankan binary assetfinder.
func runAssetfinder(ctx context.Context, target string) ([]string, error) {
	path, err := exec.LookPath("assetfinder")
	if err != nil {
		log.Printf("[L1/assetfinder] binary tidak ditemukan, skip")
		return nil, nil
	}

	cmd := exec.CommandContext(ctx, path, "--subs-only", target)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("assetfinder: %w", err)
	}

	return splitLines(string(out)), nil
}

// runCrtSh query crt.sh JSON API.
// Rate limit crt.sh cukup longgar untuk passive recon normal.
func runCrtSh(ctx context.Context, target string) ([]string, error) {
	url := fmt.Sprintf("https://crt.sh/?q=%%25.%s&output=json", target)

	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "InfraMapper/1.0 passive-recon")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("crt.sh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crt.sh HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// crt.sh returns array of {name_value, common_name, ...}
	var entries []struct {
		NameValue string `json:"name_value"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("crt.sh parse: %w", err)
	}

	seen := make(map[string]bool)
	var hosts []string
	for _, e := range entries {
		// name_value bisa berisi multiple domain dipisah newline
		for _, h := range splitLines(e.NameValue) {
			h = strings.TrimPrefix(h, "*.")
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}

	return hosts, nil
}

// splitLines memisahkan output multi-baris menjadi slice string.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// FilterByScope membuang subdomain yang tidak termasuk scope target.
// Dipakai saat re-seed dari SAN untuk menghindari out-of-scope domain.
func FilterByScope(subs []*model.Subdomain, targetDomain string) []*model.Subdomain {
	var filtered []*model.Subdomain
	for _, s := range subs {
		if util.IsSubdomainOf(s.Domain, targetDomain) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}
