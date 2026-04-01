package layer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/inframapper/internal/model"
)

// FOFANewIPInfo menyimpan IP baru beserta sinyal yang digunakannya ditemukan.
// Dipakai pipeline untuk backfill FaviconHash/JARM ke asset yang baru dibuat.
type FOFANewIPInfo struct {
	IP          string
	FaviconHash string // hash yang dipakai saat query icon_hash → found this IP
	JARM        string // JARM dari hasil FOFA (jika ada)
	ASN         string
	Country     string
}

// FOFAPivotResult adalah output pivot FOFA dari layer 3.
type FOFAPivotResult struct {
	Hits        int
	NewIPs      []string       // IP baru yang belum ada di asset list (backward compat)
	NewIPInfos  []FOFANewIPInfo // enriched: IP + signal yang menemukannya
	CertDomains []string       // domain dari cert.domain yang bisa di-re-seed ke L1/L4
}

// RunFOFAPivot menjalankan FOFA pivot secara paralel dengan Shodan.
// Tiga pivot dijalankan berurutan per asset (tapi assets diproses paralel):
//  1. icon_hash    — konfirmasi & extend Shodan favicon pivot
//  2. header_hash  — sinyal unik FOFA, tidak ada di Shodan
//  3. cert.domain  — enrich domain list dari sertifikat
//
// Rate limit FOFA: max 2 req/s → ticker 600ms memberi margin aman.
func RunFOFAPivot(ctx context.Context, cfg model.Config, assets []*model.Asset) (*FOFAPivotResult, error) {
	if !cfg.FOFAEnable || cfg.FOFAEmail == "" || cfg.FOFAKey == "" {
		if cfg.Verbose {
			log.Printf("[L3/fofa] disabled atau credentials kosong, skip")
		}
		return &FOFAPivotResult{}, nil
	}

	client := &http.Client{Timeout: 20 * time.Second}
	result := &FOFAPivotResult{}
	var mu sync.Mutex

	// Kumpulkan IP yang sudah dikenal untuk dedup "new IPs"
	existingIPs := make(map[string]bool)
	for _, a := range assets {
		if a.IP != "" {
			existingIPs[a.IP] = true
		}
	}

	// --- Pivot 1: icon_hash ---
	// Kumpulkan unique icon_hash dari asset yang sudah punya favicon hash
	iconHashGroups := make(map[string][]*model.Asset)
	for _, a := range assets {
		if a.FaviconHash != "" {
			iconHashGroups[a.FaviconHash] = append(iconHashGroups[a.FaviconHash], a)
		}
	}

	// Rate limiter: 2 req/s dengan margin → 1 query setiap 600ms
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()

	log.Printf("[L3/fofa] icon_hash pivot: %d unique hash", len(iconHashGroups))

	for hash, hashAssets := range iconHashGroups {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-ticker.C:
		}

		// FOFA query: icon_hash="-12345" (integer signed, bukan hex)
		query := fmt.Sprintf(`icon_hash="%s"`, hash)
		matches, err := queryFOFA(ctx, client, cfg, query,
			"host,ip,port,asn,org,title,server,jarm,country,icp",
		)
		if err != nil {
			if cfg.Debug {
				log.Printf("[L3/fofa] icon_hash %s: %v", hash, err)
			}
			continue
		}

		result.Hits++
		mu.Lock()
		for _, m := range matches {
			// Enrich asset dengan data FOFA
			for _, a := range hashAssets {
				if a.FOFAData == nil {
					a.FOFAData = &model.FOFAResult{
						IconHash: hash,
						JARM:     m.JARM,
						Country:  m.Country,
						ASN:      m.ASN,
						Org:      m.Org,
						ICP:      m.ICP,
					}
					// Propagasi JARM ke TLSInfo jika ada
					if m.JARM != "" && a.TLSCert != nil {
						a.TLSCert.JARM = m.JARM
					}
				}
			}

			// Catat IP baru beserta sinyal yang menemukannya
			if m.IP != "" && !existingIPs[m.IP] {
				existingIPs[m.IP] = true
				result.NewIPs = append(result.NewIPs, m.IP)
				result.NewIPInfos = append(result.NewIPInfos, FOFANewIPInfo{
					IP:          m.IP,
					FaviconHash: hash, // hash icon_hash yang dipakai query ini
					JARM:        m.JARM,
					ASN:         m.ASN,
					Country:     m.Country,
				})
			}
		}
		mu.Unlock()

		if cfg.Debug {
			log.Printf("[L3/fofa] icon_hash %s → %d hasil, %d new IPs", hash, len(matches), len(result.NewIPs))
		}
	}

	// --- Pivot 2: header_hash ---
	// Ini sinyal yang tidak ada di Shodan — fingerprint HTTP response header.
	// Sangat berguna untuk mendeteksi CDN custom atau reverse proxy yang sama.
	// Kita query per-asset yang belum di-enrich dengan header_hash.
	// Header hash kita ambil dari respons httpx (pakai hash dari Server+Content-Type+header pattern).
	//
	// Catatan: header_hash di FOFA adalah hash dari seluruh response header.
	// Kita tidak bisa menghitungnya sendiri dari data yang ada — jadi pivot ini
	// berjalan terbalik: kita lookup asset per IP di FOFA dan ambil header_hash-nya,
	// lalu gunakan sebagai sinyal clustering di L6.
	log.Printf("[L3/fofa] header_hash enrichment untuk %d assets", len(assets))

	headerHashGroups := make(map[string][]*model.Asset)
	for _, a := range assets {
		if !a.Alive || a.IP == "" {
			continue
		}

		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-ticker.C:
		}

		// Query FOFA by IP untuk mendapatkan header_hash
		query := fmt.Sprintf(`ip="%s"`, a.IP)
		matches, err := queryFOFA(ctx, client, cfg, query, "ip,header_hash,jarm,cert.domain,icp")
		if err != nil {
			if cfg.Debug {
				log.Printf("[L3/fofa] ip=%s: %v", a.IP, err)
			}
			continue
		}

		for _, m := range matches {
			if m.HeaderHash != "" {
				// Simpan ke asset
				if a.FOFAData == nil {
					a.FOFAData = &model.FOFAResult{}
				}
				a.FOFAData.HeaderHash = m.HeaderHash
				a.FOFAData.JARM = m.JARM
				if a.TLSCert != nil && m.JARM != "" {
					a.TLSCert.JARM = m.JARM
				}

				// Group by header_hash untuk clustering nanti
				headerHashGroups[m.HeaderHash] = append(headerHashGroups[m.HeaderHash], a)
			}

			// Kumpulkan cert domains untuk di-feed ke L4 re-seed
			mu.Lock()
			for _, cd := range m.CertDomains {
				cd = strings.ToLower(strings.TrimSpace(cd))
				if cd != "" {
					result.CertDomains = append(result.CertDomains, cd)
				}
			}
			mu.Unlock()
		}
	}

	// Deduplikasi CertDomains
	mu.Lock()
	result.CertDomains = uniqueStringsLocal(result.CertDomains)
	mu.Unlock()

	if cfg.Verbose {
		log.Printf("[L3/fofa] selesai: %d hits, %d new IPs, %d cert domains baru",
			result.Hits, len(result.NewIPs), len(result.CertDomains))
	}

	return result, nil
}

// fofaMatch adalah satu baris hasil dari FOFA API response.
type fofaMatch struct {
	Host        string
	IP          string
	Port        string
	ASN         string
	Org         string
	Title       string
	Server      string
	JARM        string
	Country     string
	ICP         string
	HeaderHash  string
	IconHash    string
	CertDomains []string
}

// queryFOFA mengirim satu query ke FOFA search API dan mengembalikan parsed results.
// fields harus sesuai dengan Appendix 1 di dokumentasi FOFA.
func queryFOFA(ctx context.Context, client *http.Client, cfg model.Config, query, fields string) ([]fofaMatch, error) {
	// FOFA requires query di-base64 encode
	qb64 := base64.StdEncoding.EncodeToString([]byte(query))

	params := url.Values{}
	params.Set("email", cfg.FOFAEmail)
	params.Set("key", cfg.FOFAKey)
	params.Set("qbase64", qb64)
	params.Set("fields", fields)
	params.Set("size", "100")   // default 100, cukup untuk pivot discovery
	params.Set("page", "1")
	params.Set("full", "false") // hanya data dalam 1 tahun terakhir

	apiURL := "https://fofa.info/api/v1/search/all?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "InfraMapper/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("FOFA request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("FOFA auth gagal (HTTP %d) — cek email/key", resp.StatusCode)
	}
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("FOFA rate limited — kurangi concurrency atau tambah delay")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FOFA HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // max 2MB
	if err != nil {
		return nil, err
	}

	// FOFA response format:
	// { "error": false, "size": N, "results": [["field1_val", "field2_val", ...], ...] }
	var raw struct {
		Error   bool              `json:"error"`
		ErrMsg  string            `json:"errmsg"`
		Size    int               `json:"size"`
		Results [][]interface{}   `json:"results"` // array of arrays — urutan sesuai `fields` param
	}

	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("FOFA parse response: %w", err)
	}
	if raw.Error {
		return nil, fmt.Errorf("FOFA API error: %s", raw.ErrMsg)
	}

	// Parse fields secara positional — urutan sesuai parameter `fields`
	fieldList := strings.Split(fields, ",")
	fieldIdx := make(map[string]int, len(fieldList))
	for i, f := range fieldList {
		fieldIdx[strings.TrimSpace(f)] = i
	}

	var matches []fofaMatch
	for _, row := range raw.Results {
		if len(row) < len(fieldList) {
			continue // row tidak lengkap, skip
		}

		m := fofaMatch{}
		for field, idx := range fieldIdx {
			val := stringVal(row[idx])
			switch field {
			case "host":
				m.Host = val
			case "ip":
				m.IP = val
			case "port":
				m.Port = val
			case "asn":
				m.ASN = val
			case "org":
				m.Org = val
			case "title":
				m.Title = val
			case "server":
				m.Server = val
			case "jarm":
				m.JARM = val
			case "country", "country_name":
				m.Country = val
			case "icp":
				m.ICP = val
			case "header_hash":
				m.HeaderHash = val
			case "icon_hash":
				m.IconHash = val
			case "cert.domain":
				// cert.domain di FOFA bisa berupa string atau array
				m.CertDomains = parseCertDomains(row[idx])
			}
		}

		if m.IP != "" || m.Host != "" {
			matches = append(matches, m)
		}
	}

	return matches, nil
}

// stringVal mengkonversi interface{} ke string dengan aman.
func stringVal(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprintf("%.0f", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// parseCertDomains menangani cert.domain yang bisa string atau array dari FOFA.
func parseCertDomains(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case string:
		// Kadang FOFA return string dengan newline separator
		var out []string
		for _, d := range strings.Fields(t) {
			d = strings.Trim(d, `"'[]`)
			if d != "" {
				out = append(out, d)
			}
		}
		return out
	case []interface{}:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

// uniqueStringsLocal adalah local dedup utility untuk menghindari import cycle.
func uniqueStringsLocal(ss []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// GetHeaderHashGroups mengekstrak map header_hash → assets dari asset list.
// Dipakai layer 6 untuk clustering by header_hash.
func GetHeaderHashGroups(assets []*model.Asset) map[string][]*model.Asset {
	groups := make(map[string][]*model.Asset)
	for _, a := range assets {
		if a.FOFAData != nil && a.FOFAData.HeaderHash != "" {
			groups[a.FOFAData.HeaderHash] = append(groups[a.FOFAData.HeaderHash], a)
		}
	}
	return groups
}

// GetJARMGroups mengekstrak map jarm → assets.
// JARM adalah TLS behavioral fingerprint — asset dengan JARM sama hampir pasti
// pakai TLS library / konfigurasi yang identik (sinyal infrastruktur sama).
func GetJARMGroups(assets []*model.Asset) map[string][]*model.Asset {
	groups := make(map[string][]*model.Asset)
	for _, a := range assets {
		jarm := ""
		if a.FOFAData != nil && a.FOFAData.JARM != "" {
			jarm = a.FOFAData.JARM
		} else if a.TLSCert != nil && a.TLSCert.JARM != "" {
			jarm = a.TLSCert.JARM
		}
		if jarm == "" || isGenericJARM(jarm) {
			continue // skip JARM all-zero atau known generic
		}
		groups[jarm] = append(groups[jarm], a)
	}
	return groups
}

// isGenericJARM mendeteksi JARM yang terlalu umum untuk dijadikan sinyal pivot.
// JARM all-zero artinya koneksi ditutup sebelum TLS handshake selesai.
func isGenericJARM(jarm string) bool {
	// JARM all zeros = tidak ada TLS atau koneksi gagal
	if strings.Trim(jarm, "0") == "" {
		return true
	}
	return false
}
