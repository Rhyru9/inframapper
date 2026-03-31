package layer

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/inframapper/internal/model"
)

// FaviconResult adalah output enrichment layer 3.
type FaviconResult struct {
	Enriched int
	Errors   int
}

// RunFaviconHash mengambil favicon tiap asset yang alive dan menghitung MurmurHash3.
// Hash ini adalah sinyal utama untuk Shodan pivot.
func RunFaviconHash(ctx context.Context, cfg model.Config, assets []*model.Asset) (*FaviconResult, error) {
	sem := make(chan struct{}, 20) // favicon lebih lambat dari httpx, concurrency lebih rendah
	var mu sync.Mutex
	var wg sync.WaitGroup
	result := &FaviconResult{}

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}

	for _, asset := range assets {
		if !asset.Alive {
			continue
		}

		wg.Add(1)
		go func(a *model.Asset) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			hash, err := fetchFaviconHash(ctx, client, a)
			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				result.Errors++
				if cfg.Debug {
					log.Printf("[L3/favicon] %s: %v", a.Host, err)
				}
				return
			}

			a.FaviconHash = hash
			result.Enriched++
		}(asset)
	}

	wg.Wait()

	if cfg.Verbose {
		log.Printf("[L3] favicon: %d hash ditemukan, %d error", result.Enriched, result.Errors)
	}

	return result, nil
}

// fetchFaviconHash mengambil /favicon.ico dan menghitung Shodan-compatible MurmurHash3.
func fetchFaviconHash(ctx context.Context, client *http.Client, asset *model.Asset) (string, error) {
	scheme := "https"
	if !asset.HTTPS {
		scheme = "http"
	}

	faviconURL := fmt.Sprintf("%s://%s/favicon.ico", scheme, asset.Host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, faviconURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "InfraMapper/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // max 1MB
	if err != nil {
		return "", err
	}

	if len(data) == 0 {
		return "", fmt.Errorf("favicon kosong")
	}

	// Shodan menggunakan MurmurHash3 dari base64-encoded favicon
	b64 := base64.StdEncoding.EncodeToString(data)
	// Tambahkan newline setiap 76 karakter (MIME encoding style) — ini yang dipakai Shodan
	b64 = insertNewlines(b64, 76)
	hash := murmur3Hash([]byte(b64))

	return fmt.Sprintf("%d", int32(hash)), nil
}

// insertNewlines menyisipkan \n setiap n karakter.
func insertNewlines(s string, n int) string {
	var sb strings.Builder
	for i, c := range s {
		if i > 0 && i%n == 0 {
			sb.WriteRune('\n')
		}
		sb.WriteRune(c)
	}
	sb.WriteRune('\n')
	return sb.String()
}

// murmur3Hash adalah implementasi MurmurHash3 32-bit (kompatibel dengan Shodan).
func murmur3Hash(data []byte) uint32 {
	const (
		c1 = 0xcc9e2d51
		c2 = 0x1b873593
		r1 = 15
		r2 = 13
		m  = 5
		n  = 0xe6546b64
	)

	length := len(data)
	h1 := uint32(0)
	nblocks := length / 4

	for i := 0; i < nblocks; i++ {
		k1 := uint32(data[i*4]) | uint32(data[i*4+1])<<8 | uint32(data[i*4+2])<<16 | uint32(data[i*4+3])<<24
		k1 *= c1
		k1 = (k1 << r1) | (k1 >> (32 - r1))
		k1 *= c2
		h1 ^= k1
		h1 = (h1 << r2) | (h1 >> (32 - r2))
		h1 = h1*m + n
	}

	tail := data[nblocks*4:]
	var k1 uint32
	switch len(tail) {
	case 3:
		k1 ^= uint32(tail[2]) << 16
		fallthrough
	case 2:
		k1 ^= uint32(tail[1]) << 8
		fallthrough
	case 1:
		k1 ^= uint32(tail[0])
		k1 *= c1
		k1 = (k1 << r1) | (k1 >> (32 - r1))
		k1 *= c2
		h1 ^= k1
	}

	h1 ^= uint32(length)
	h1 ^= h1 >> 16
	h1 *= 0x85ebca6b
	h1 ^= h1 >> 13
	h1 *= 0xc2b2ae35
	h1 ^= h1 >> 16

	return h1
}

// --- Shodan Pivot ---

// ShodanPivotResult adalah output pivot Shodan berdasarkan favicon hash.
type ShodanPivotResult struct {
	Hits    int
	NewIPs  []string // IP yang ditemukan via Shodan tapi belum ada di asset list
}

// RunShodanPivot query Shodan untuk tiap unique favicon hash yang ditemukan.
// Hasilnya dipakai untuk memperkaya asset dan juga bisa jadi input layer 5.
func RunShodanPivot(ctx context.Context, cfg model.Config, assets []*model.Asset) (*ShodanPivotResult, error) {
	if cfg.ShodanAPIKey == "" {
		log.Printf("[L3/shodan] API key kosong, skip Shodan pivot")
		return &ShodanPivotResult{}, nil
	}

	// Kumpulkan unique hash
	hashToAssets := make(map[string][]*model.Asset)
	for _, a := range assets {
		if a.FaviconHash != "" {
			hashToAssets[a.FaviconHash] = append(hashToAssets[a.FaviconHash], a)
		}
	}

	if len(hashToAssets) == 0 {
		return &ShodanPivotResult{}, nil
	}

	// Track IP yang sudah ada supaya kita tahu mana yang "baru"
	existingIPs := make(map[string]bool)
	for _, a := range assets {
		if a.IP != "" {
			existingIPs[a.IP] = true
		}
	}

	result := &ShodanPivotResult{}
	client := &http.Client{Timeout: 20 * time.Second}

	// Rate limit: Shodan free tier = 1 req/sec
	ticker := time.NewTicker(1100 * time.Millisecond)
	defer ticker.Stop()

	for hash, hashAssets := range hashToAssets {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-ticker.C:
		}

		shodanIPs, org, asn, err := queryShodanByFavicon(ctx, client, cfg.ShodanAPIKey, hash)
		if err != nil {
			if cfg.Debug {
				log.Printf("[L3/shodan] hash %s: %v", hash, err)
			}
			continue
		}

		result.Hits++

		// Enrich semua asset yang punya hash ini
		for _, a := range hashAssets {
			if a.ShodanData == nil {
				a.ShodanData = &model.ShodanResult{
					Org: org,
					ASN: asn,
				}
			}
		}

		// Catat IP baru yang ditemukan Shodan
		for _, ip := range shodanIPs {
			if !existingIPs[ip] {
				result.NewIPs = append(result.NewIPs, ip)
				existingIPs[ip] = true
			}
		}
	}

	if cfg.Verbose {
		log.Printf("[L3/shodan] %d hash pivoted, %d new IPs ditemukan", result.Hits, len(result.NewIPs))
	}

	return result, nil
}

// queryShodanByFavicon query Shodan search API dengan filter http.favicon.hash.
func queryShodanByFavicon(ctx context.Context, client *http.Client, apiKey, hash string) ([]string, string, string, error) {
	url := fmt.Sprintf(
		"https://api.shodan.io/shodan/host/search?key=%s&query=http.favicon.hash:%s&facets=org,asn",
		apiKey, hash,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return nil, "", "", fmt.Errorf("Shodan API key tidak valid")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("Shodan HTTP %d", resp.StatusCode)
	}

	var sr struct {
		Matches []struct {
			IPStr string `json:"ip_str"`
			Org   string `json:"org"`
			ASN   string `json:"asn"`
		} `json:"matches"`
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}

	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, "", "", err
	}

	var ips []string
	var org, asn string
	for _, m := range sr.Matches {
		ips = append(ips, m.IPStr)
		if org == "" {
			org = m.Org
		}
		if asn == "" {
			asn = m.ASN
		}
	}

	return ips, org, asn, nil
}
