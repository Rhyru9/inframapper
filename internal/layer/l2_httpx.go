package layer

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourusername/inframapper/internal/model"
	"github.com/yourusername/inframapper/internal/util"
)

// HTTPXResult adalah output layer 2.
type HTTPXResult struct {
	Alive   []*model.Asset
	Dead    []string
	Stats   struct {
		Probed  int
		Alive   int
		Dead    int
		Elapsed time.Duration
	}
}

// RunHTTPX memprobe setiap subdomain secara paralel dan mengisi field Asset.
// concurrency dikontrol lewat semaphore agar tidak membanjiri target.
func RunHTTPX(ctx context.Context, cfg model.Config, subs []*model.Subdomain) (*HTTPXResult, error) {
	sem := make(chan struct{}, cfg.HTTPConcurrent)
	var (
		mu      sync.Mutex
		alive   []*model.Asset
		dead    []string
		probed  int64
		aliveN  int64
	)

	start := time.Now()
	var wg sync.WaitGroup
	client := buildHTTPClient(cfg)

	for _, sub := range subs {
		wg.Add(1)
		go func(s *model.Subdomain) {
			defer wg.Done()

			// Acquire semaphore
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			asset, err := probeHost(ctx, client, s, cfg)
			atomic.AddInt64(&probed, 1)

			mu.Lock()
			defer mu.Unlock()

			if err != nil || asset == nil || !asset.Alive {
				dead = append(dead, s.Domain)
				if cfg.Debug && err != nil {
					log.Printf("[L2] dead %s: %v", s.Domain, err)
				}
				return
			}

			atomic.AddInt64(&aliveN, 1)
			alive = append(alive, asset)
		}(sub)
	}

	wg.Wait()

	result := &HTTPXResult{Alive: alive, Dead: dead}
	result.Stats.Probed = int(probed)
	result.Stats.Alive = int(aliveN)
	result.Stats.Dead = len(dead)
	result.Stats.Elapsed = time.Since(start)

	if cfg.Verbose {
		log.Printf("[L2] httpx: %d probed, %d alive, %d dead dalam %.1fs",
			result.Stats.Probed, result.Stats.Alive, result.Stats.Dead,
			result.Stats.Elapsed.Seconds())
	}

	return result, nil
}

// probeHost mengirim HTTP request dan mengekstrak metadata asset.
func probeHost(ctx context.Context, client *http.Client, sub *model.Subdomain, cfg model.Config) (*model.Asset, error) {
	asset := &model.Asset{
		Host:         sub.Domain,
		Source:       sub.Source,
		Iter:         sub.Iteration,
		DiscoveredAt: time.Now(),
	}

	// Coba HTTPS dulu, fallback HTTP
	url, https := resolveURL(sub.Domain)
	asset.HTTPS = https

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "InfraMapper/1.0 passive-recon")

	resp, err := client.Do(req)
	if err != nil {
		// Coba HTTP jika HTTPS gagal
		if https {
			url = "http://" + sub.Domain
			asset.HTTPS = false
			req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			req2.Header.Set("User-Agent", "InfraMapper/1.0 passive-recon")
			resp, err = client.Do(req2)
			if err != nil {
				return asset, err
			}
		} else {
			return asset, err
		}
	}
	defer resp.Body.Close()

	asset.Alive = true
	asset.StatusCode = resp.StatusCode
	asset.Server = resp.Header.Get("Server")
	asset.ContentType = resp.Header.Get("Content-Type")

	// Ekstrak port dari URL jika non-standard
	if resp.Request != nil {
		portStr := resp.Request.URL.Port()
		if portStr != "" {
			if p, err := strconv.Atoi(portStr); err == nil {
				asset.Port = p
			}
		} else if asset.HTTPS {
			asset.Port = 443
		} else {
			asset.Port = 80
		}
	}

	// Resolve IP — gunakan DNS lookup sebagai sumber utama, lebih reliable
	if addrs, err := net.LookupHost(sub.Domain); err == nil && len(addrs) > 0 {
		asset.IP = addrs[0]
	} else if resp.Request != nil {
		// Fallback: ambil dari request host jika DNS lookup gagal
		if host, _, err := net.SplitHostPort(resp.Request.Host); err == nil {
			asset.IP = host
		}
	}

	// Baca body secukupnya untuk title
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // max 64KB
	if err == nil {
		asset.Title = util.ExtractTitle(string(body))
	}

	return asset, nil
}

// resolveURL memilih HTTPS by default.
func resolveURL(host string) (string, bool) {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return host, strings.HasPrefix(host, "https://")
	}
	return "https://" + host, true
}

// buildHTTPClient membuat HTTP client yang dipakai seluruh layer 2.
// Skip TLS verify karena target mungkin punya cert expired — kita tetap mau data-nya.
func buildHTTPClient(cfg model.Config) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(cfg.HTTPTimeout) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   cfg.HTTPConcurrent,
		ResponseHeaderTimeout: time.Duration(cfg.HTTPTimeout) * time.Second,
		DisableKeepAlives:     false,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.HTTPTimeout+5) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("terlalu banyak redirect")
			}
			return nil
		},
	}
}
