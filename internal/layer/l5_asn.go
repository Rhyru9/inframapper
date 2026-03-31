package layer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourusername/inframapper/internal/model"
)

// ASNSweepResult adalah output layer 5.
type ASNSweepResult struct {
	Skipped    bool   // true jika layer di-skip karena tidak ada IP baru
	SkipReason string
	NewAssets  []*model.Asset // IP baru yang responsif
	CIDRCount  int
	ProbeCount int
}

// RunASNSweep menjalankan sweep CIDR berdasarkan ASN yang ditemukan di layer sebelumnya.
// Ini adalah layer CONDITIONAL — hanya berjalan jika newIPsFromPrevious >= cfg.ASNMinNewIPs.
//
// Desain: layer ini "lazy" — hanya berjalan kalau ada sinyal baru yang bernilai.
// Ini mencegah tool jadi noisy API call factory.
func RunASNSweep(ctx context.Context, cfg model.Config, assets []*model.Asset, newIPsFromPrevious []string) (*ASNSweepResult, error) {
	// Guard: jika tidak ada IP baru yang bermakna, skip
	if len(newIPsFromPrevious) < cfg.ASNMinNewIPs {
		reason := fmt.Sprintf("IP baru dari layer sebelumnya (%d) < minimum (%d)", len(newIPsFromPrevious), cfg.ASNMinNewIPs)
		if cfg.Verbose {
			log.Printf("[L5] SKIPPED: %s", reason)
		}
		return &ASNSweepResult{Skipped: true, SkipReason: reason}, nil
	}

	if cfg.Verbose {
		log.Printf("[L5] ASN sweep dimulai dengan %d IP seed baru", len(newIPsFromPrevious))
	}

	// Step 1: Lookup ASN untuk tiap IP baru
	asnCIDRs := make(map[string]string) // ASN -> CIDR
	client := &http.Client{Timeout: 10 * time.Second}

	for _, ip := range newIPsFromPrevious {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		asnInfo, err := lookupASN(ctx, client, ip, cfg.IPInfoToken)
		if err != nil {
			if cfg.Debug {
				log.Printf("[L5] ASN lookup %s: %v", ip, err)
			}
			continue
		}

		if asnInfo.CIDR == "" {
			continue
		}

		// Batasi ukuran CIDR — kita tidak mau sweep /16 atau lebih besar
		cidrSize, err := countCIDR(asnInfo.CIDR)
		if err != nil || cidrSize > cfg.ASNMaxCIDR {
			if cfg.Debug {
				log.Printf("[L5] CIDR %s terlalu besar (%d), skip", asnInfo.CIDR, cidrSize)
			}
			continue
		}

		asnCIDRs[asnInfo.ASN] = asnInfo.CIDR

		// Enrich asset yang punya IP ini
		for _, a := range assets {
			if a.IP == ip {
				a.ASNInfo = asnInfo
			}
		}
	}

	if len(asnCIDRs) == 0 {
		return &ASNSweepResult{Skipped: false, SkipReason: "tidak ada CIDR valid ditemukan"}, nil
	}

	// Step 2: Probe IP dalam CIDR yang ditemukan
	existingIPs := buildIPSet(assets)
	result := &ASNSweepResult{CIDRCount: len(asnCIDRs)}

	var mu sync.Mutex
	sem := make(chan struct{}, cfg.HTTPConcurrent)
	var wg sync.WaitGroup
	var probeCount int64

	httpClient := buildHTTPClient(cfg)

	for _, cidr := range asnCIDRs {
		ips, err := expandCIDR(cidr, cfg.ASNMaxCIDR)
		if err != nil {
			log.Printf("[L5] expand CIDR %s: %v", cidr, err)
			continue
		}

		for _, ip := range ips {
			if existingIPs[ip] {
				continue // sudah dikenal, skip
			}

			wg.Add(1)
			go func(ipAddr string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				atomic.AddInt64(&probeCount, 1)

				// Probe sederhana: apakah port 80/443 terbuka?
				asset, err := probeIPDirect(ctx, httpClient, ipAddr)
				if err != nil || asset == nil {
					return
				}

				mu.Lock()
				result.NewAssets = append(result.NewAssets, asset)
				mu.Unlock()
			}(ip)
		}
	}

	wg.Wait()
	result.ProbeCount = int(atomic.LoadInt64(&probeCount))

	if cfg.Verbose {
		log.Printf("[L5] ASN sweep: %d CIDR, %d IP diprobe, %d asset baru", result.CIDRCount, result.ProbeCount, len(result.NewAssets))
	}

	return result, nil
}

// lookupASN menggunakan ipinfo.io untuk mendapatkan ASN, CIDR, dan geo-koordinat dari IP.
//
// BUG FIX: versi lama tidak mengambil field "loc" (lat,lon), "city", dan "region"
// dari respons ipinfo.io — padahal field tersebut dibutuhkan untuk world map rendering.
// Sekarang semua field geo di-extract dan disimpan di ASNInfo.
//
// BUG FIX: status code 401 (token expired/invalid) tidak di-handle, menyebabkan
// partial unmarshal silent failure tanpa error yang informatif.
// lookupASN mengambil geo + ASN info dari IP.
// Strategi multi-provider (Bug #6 fix):
//   - Jika token ipinfo.io tersedia: pakai ipinfo.io (akurat, rate limit tinggi)
//   - Fallback: ip-api.com (gratis, ~45 req/mnt, tidak butuh token)
// Dengan fallback ini geo enrichment berjalan bahkan tanpa konfigurasi apapun.
func lookupASN(ctx context.Context, client *http.Client, ip, ipinfoToken string) (*model.ASNInfo, error) {
	if ipinfoToken != "" {
		info, err := lookupIPInfo(ctx, client, ip, ipinfoToken)
		if err == nil {
			return info, nil
		}
		// Fallback ke ip-api jika ipinfo gagal (rate limit, token expired, dll)
	}
	return lookupIPAPI(ctx, client, ip)
}

// lookupIPInfo menggunakan ipinfo.io (butuh token untuk reliabilitas tinggi).
func lookupIPInfo(ctx context.Context, client *http.Client, ip, token string) (*model.ASNInfo, error) {
	url := fmt.Sprintf("https://ipinfo.io/%s/json", ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "InfraMapper/1.0")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 429:
		return nil, fmt.Errorf("rate limited oleh ipinfo.io")
	case 401, 403:
		return nil, fmt.Errorf("ipinfo.io auth gagal (status %d)", resp.StatusCode)
	}

	var data struct {
		Org     string `json:"org"`      // format: "AS12345 Cloudflare, Inc."
		Country string `json:"country"`
		Network string `json:"network"`  // CIDR, e.g. "104.16.0.0/12"
		Loc     string `json:"loc"`      // "lat,lon" e.g. "37.3861,-122.0839" — BARU
		City    string `json:"city"`     // kota — BARU
		Region  string `json:"region"`   // region/state — BARU
		Bogon   bool   `json:"bogon"`    // true untuk private/reserved IP
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("ipinfo parse error untuk IP %s: %w", ip, err)
	}

	// BUG FIX: bogon IP (private/loopback) tidak perlu di-lookup lebih lanjut
	if data.Bogon {
		return &model.ASNInfo{
			ASN:     "BOGON",
			Name:    "Private/Reserved IP",
			Country: "PRIVATE",
		}, nil
	}

	// Parse ASN number dari format "AS12345 Name"
	var asnNum int
	var asnName string
	if strings.HasPrefix(data.Org, "AS") {
		parts := strings.SplitN(data.Org[2:], " ", 2)
		if len(parts) >= 1 {
			fmt.Sscanf(parts[0], "%d", &asnNum)
		}
		if len(parts) >= 2 {
			asnName = parts[1]
		}
	}

	// Parse "lat,lon" dari field "loc"
	var lat, lon float64
	if data.Loc != "" {
		parts := strings.SplitN(data.Loc, ",", 2)
		if len(parts) == 2 {
			fmt.Sscanf(strings.TrimSpace(parts[0]), "%f", &lat)
			fmt.Sscanf(strings.TrimSpace(parts[1]), "%f", &lon)
		}
	}

	return &model.ASNInfo{
		Number:  asnNum,
		Name:    asnName,
		CIDR:    data.Network,
		Country: data.Country,
		City:    data.City,
		Region:  data.Region,
		Lat:     lat,
		Lon:     lon,
		ASN:     fmt.Sprintf("AS%d", asnNum),
	}, nil
}

// lookupIPAPI menggunakan ip-api.com sebagai fallback geo provider.
// Tidak butuh token, gratis, ~45 req/menit.
// Field yang dikembalikan: country, region/city, org/asn, lat/lon.
func lookupIPAPI(ctx context.Context, client *http.Client, ip string) (*model.ASNInfo, error) {
	// ip-api.com format: http://ip-api.com/json/{ip}?fields=status,country,countryCode,region,city,lat,lon,org,as
	url := fmt.Sprintf("http://ip-api.com/json/%s?fields=status,message,country,countryCode,regionName,city,lat,lon,org,as", ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "InfraMapper/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("ip-api.com rate limited")
	}

	var data struct {
		Status      string  `json:"status"`
		Message     string  `json:"message"` // isi saat status=fail
		Country     string  `json:"country"`
		CountryCode string  `json:"countryCode"`
		Region      string  `json:"regionName"`
		City        string  `json:"city"`
		Lat         float64 `json:"lat"`
		Lon         float64 `json:"lon"`
		Org         string  `json:"org"`    // e.g. "AS13335 Cloudflare, Inc."
		AS          string  `json:"as"`     // e.g. "AS13335 Cloudflare, Inc."
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("ip-api parse error: %w", err)
	}
	if data.Status != "success" {
		return nil, fmt.Errorf("ip-api fail untuk %s: %s", ip, data.Message)
	}

	// Parse ASN number dari field "as" (format "AS13335 Cloudflare, Inc.")
	var asnNum int
	var asnName string
	if strings.HasPrefix(data.AS, "AS") {
		parts := strings.SplitN(data.AS[2:], " ", 2)
		if len(parts) >= 1 {
			fmt.Sscanf(parts[0], "%d", &asnNum)
		}
		if len(parts) >= 2 {
			asnName = parts[1]
		}
	}

	return &model.ASNInfo{
		Number:  asnNum,
		ASN:     fmt.Sprintf("AS%d", asnNum),
		Name:    asnName,
		Country: data.CountryCode,
		City:    data.City,
		Region:  data.Region,
		Lat:     data.Lat,
		Lon:     data.Lon,
	}, nil
}

// probeIPDirect mencoba koneksi langsung ke IP (tanpa hostname resolution).
func probeIPDirect(ctx context.Context, client *http.Client, ip string) (*model.Asset, error) {
	// Coba port 443 dulu, lalu 80
	for _, port := range []int{443, 80} {
		scheme := "https"
		if port == 80 {
			scheme = "http"
		}

		url := fmt.Sprintf("%s://%s", scheme, ip)
		if port != 80 && port != 443 {
			url = fmt.Sprintf("%s://%s:%d", scheme, ip, port)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "InfraMapper/1.0")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()

		return &model.Asset{
			Host:         ip,
			IP:           ip,
			Source:       model.SourceASN,
			Alive:        true,
			StatusCode:   resp.StatusCode,
			Port:         port,
			HTTPS:        port == 443,
			Server:       resp.Header.Get("Server"),
			DiscoveredAt: time.Now(),
		}, nil
	}

	return nil, fmt.Errorf("tidak responsif")
}

// expandCIDR menghasilkan daftar IP dari CIDR block.
// limit menentukan jumlah maksimal IP yang di-expand (dari cfg.ASNMaxCIDR).
func expandCIDR(cidr string, limit int) ([]string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 256
	}

	var ips []string
	for ip := ipNet.IP.Mask(ipNet.Mask); ipNet.Contains(ip); incrementIP(ip) {
		ips = append(ips, ip.String())
		if len(ips) >= limit {
			break
		}
	}

	return ips, nil
}

func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] != 0 {
			break
		}
	}
}

func countCIDR(cidr string) (int, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, err
	}
	ones, bits := ipNet.Mask.Size()
	return 1 << (bits - ones), nil
}

func buildIPSet(assets []*model.Asset) map[string]bool {
	set := make(map[string]bool)
	for _, a := range assets {
		if a.IP != "" {
			set[a.IP] = true
		}
	}
	return set
}
