package layer

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/yourusername/inframapper/internal/model"
)

// ClusterResult adalah output layer 6.
type ClusterResult struct {
	Clusters []*model.Cluster
	Orphans  []*model.Asset // IP yang tidak ada di DNS seed awal
}

// RunClustering mengelompokkan asset berdasarkan sinyal yang tersedia.
// Prinsip: setiap cluster harus bisa dijelaskan dengan SATU sinyal pivot.
// Layer ini adalah pure data processing — tidak ada network call.
func RunClustering(cfg model.Config, assets []*model.Asset, originalDNSIPs map[string]bool) (*ClusterResult, error) {
	result := &ClusterResult{}
	var clusters []*model.Cluster

	// --- Pivot 1: Favicon Hash ---
	if cfg.ClusterByFavicon {
		faviconClusters := clusterByFaviconHash(assets)
		clusters = append(clusters, faviconClusters...)
	}

	// --- Pivot 2: ASN ---
	if cfg.ClusterByASN {
		asnClusters := clusterByASN(assets)
		clusters = append(clusters, asnClusters...)
	}

	// --- Pivot 3: TLS Issuer ---
	if cfg.ClusterByTLS {
		tlsClusters := clusterByTLSIssuer(assets)
		clusters = append(clusters, tlsClusters...)
	}

	// --- Pivot 4: Header Hash (FOFA-exclusive) ---
	if cfg.FOFAEnable {
		hhClusters := clusterByHeaderHash(assets)
		clusters = append(clusters, hhClusters...)
	}

	// --- Pivot 5: JARM TLS fingerprint ---
	jarmClusters := clusterByJARM(assets)
	clusters = append(clusters, jarmClusters...)

	// Scoring: beri bobot tiap cluster berdasarkan ukuran dan sinyal quality
	for _, c := range clusters {
		c.Score = calculateScore(c)
	}

	// Sort by score desc
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].Score > clusters[j].Score
	})

	// Orphan detection: IP yang tidak ada di DNS seed awal
	for _, a := range assets {
		if a.IP != "" && !originalDNSIPs[a.IP] {
			a.Tags = append(a.Tags, "orphan")
			result.Orphans = append(result.Orphans, a)
		}
	}

	result.Clusters = clusters

	if cfg.Verbose {
		log.Printf("[L6] clustering: %d cluster, %d orphan ditemukan", len(clusters), len(result.Orphans))
	}

	return result, nil
}

// clusterByFaviconHash mengelompokkan asset yang berbagi favicon hash yang sama.
// Ini adalah sinyal terkuat — favicon hash identik hampir pasti infrastruktur sama.
func clusterByFaviconHash(assets []*model.Asset) []*model.Cluster {
	groups := make(map[string][]*model.Asset)
	for _, a := range assets {
		if a.FaviconHash != "" {
			groups[a.FaviconHash] = append(groups[a.FaviconHash], a)
		}
	}

	var clusters []*model.Cluster
	i := 0
	for hash, group := range groups {
		if len(group) < 2 {
			continue // cluster 1 anggota tidak bermakna
		}
		i++
		clusters = append(clusters, &model.Cluster{
			ID:     fmt.Sprintf("fav-%d", i),
			Label:  fmt.Sprintf("favicon-hash-%s", hash),
			Assets: group,
			Pivot:  "favicon_hash",
		})
	}
	return clusters
}

// clusterByASN mengelompokkan asset dalam ASN yang sama.
func clusterByASN(assets []*model.Asset) []*model.Cluster {
	groups := make(map[string][]*model.Asset)
	for _, a := range assets {
		if a.ASNInfo != nil && a.ASNInfo.ASN != "" {
			groups[a.ASNInfo.ASN] = append(groups[a.ASNInfo.ASN], a)
		} else if a.ShodanData != nil && a.ShodanData.ASN != "" {
			groups[a.ShodanData.ASN] = append(groups[a.ShodanData.ASN], a)
		}
	}

	var clusters []*model.Cluster
	i := 0
	for asn, group := range groups {
		if len(group) < 2 {
			continue
		}
		i++
		name := asn
		if len(group) > 0 && group[0].ASNInfo != nil {
			name = fmt.Sprintf("%s (%s)", asn, group[0].ASNInfo.Name)
		}
		clusters = append(clusters, &model.Cluster{
			ID:     fmt.Sprintf("asn-%d", i),
			Label:  name,
			Assets: group,
			Pivot:  "asn",
		})
	}
	return clusters
}

// clusterByTLSIssuer mengelompokkan asset yang punya CA/issuer yang sama.
// Berguna untuk mendeteksi infra yang pakai internal CA atau satu penyedia TLS.
func clusterByTLSIssuer(assets []*model.Asset) []*model.Cluster {
	groups := make(map[string][]*model.Asset)
	for _, a := range assets {
		if a.TLSCert != nil && a.TLSCert.Issuer != "" {
			// Normalize issuer — strip common noise
			issuer := normalizeTLSIssuer(a.TLSCert.Issuer)
			groups[issuer] = append(groups[issuer], a)
		}
	}

	var clusters []*model.Cluster
	i := 0
	for issuer, group := range groups {
		if len(group) < 3 {
			continue // TLS issuer clustering butuh threshold lebih tinggi (banyak pakai Let's Encrypt)
		}
		i++
		clusters = append(clusters, &model.Cluster{
			ID:     fmt.Sprintf("tls-%d", i),
			Label:  fmt.Sprintf("tls-issuer-%s", issuer),
			Assets: group,
			Pivot:  "tls_issuer",
		})
	}
	return clusters
}

// normalizeTLSIssuer menyederhanakan nama issuer untuk grouping.
func normalizeTLSIssuer(issuer string) string {
	issuer = strings.ToLower(issuer)
	// Jangan cluster berdasarkan CA yang sangat umum — ini tidak informatif
	if strings.Contains(issuer, "let's encrypt") || strings.Contains(issuer, "letsencrypt") {
		return "letsencrypt"
	}
	if strings.Contains(issuer, "digicert") {
		return "digicert"
	}
	if strings.Contains(issuer, "sectigo") || strings.Contains(issuer, "comodo") {
		return "sectigo"
	}
	return issuer
}


// clusterByHeaderHash mengelompokkan asset yang punya HTTP response header fingerprint sama.
// Ini adalah sinyal EKSKLUSIF dari FOFA — tidak ada di Shodan.
// Header hash yang identik menunjukkan reverse proxy atau CDN konfigurasi yang sama.
func clusterByHeaderHash(assets []*model.Asset) []*model.Cluster {
	groups := GetHeaderHashGroups(assets)
	var clusters []*model.Cluster
	i := 0
	for hash, group := range groups {
		if len(group) < 2 {
			continue
		}
		i++
		clusters = append(clusters, &model.Cluster{
			ID:     fmt.Sprintf("hh-%d", i),
			Label:  fmt.Sprintf("header-hash-%s", hash[:minInt(8, len(hash))]),
			Assets: group,
			Pivot:  "header_hash",
		})
	}
	return clusters
}

// clusterByJARM mengelompokkan asset berdasarkan JARM TLS fingerprint.
// JARM mengidentifikasi TLS stack (library + konfigurasi) — bukan sertifikat.
// Asset dengan JARM sama hampir pasti jalan di infrastruktur yang sama.
func clusterByJARM(assets []*model.Asset) []*model.Cluster {
	groups := GetJARMGroups(assets)
	var clusters []*model.Cluster
	i := 0
	for jarm, group := range groups {
		if len(group) < 2 {
			continue
		}
		i++
		clusters = append(clusters, &model.Cluster{
			ID:     fmt.Sprintf("jarm-%d", i),
			Label:  fmt.Sprintf("jarm-%s", jarm[:minInt(16, len(jarm))]),
			Assets: group,
			Pivot:  "jarm",
		})
	}
	return clusters
}

// minInt returns the smaller of two ints.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// calculateScore memberi skor 0.0-1.0 pada sebuah cluster.
// Faktor: ukuran cluster, kekuatan sinyal pivot, adanya orphan.
func calculateScore(c *model.Cluster) float64 {
	score := 0.0

	// Bobot berdasarkan pivot type
	// favicon_hash & header_hash: sinyal paling spesifik — hampir pasti infra sama
	// jarm: TLS behavioral fingerprint — sangat spesifik tapi bisa false positive di CDN besar
	// asn: medium — banyak asset bisa berbagi ASN tanpa berarti infra sama
	// tls_issuer: lemah karena Let's Encrypt / DigiCert dipakai jutaan domain
	pivotWeight := map[string]float64{
		"favicon_hash": 0.55,
		"header_hash":  0.50, // FOFA-exclusive, sangat spesifik
		"jarm":         0.45, // TLS behavioral fingerprint
		"asn":          0.30,
		"tls_issuer":   0.20,
	}
	score += pivotWeight[c.Pivot]

	// Bonus ukuran cluster (diminishing returns)
	size := float64(len(c.Assets))
	if size > 1 {
		score += 0.3 * (1 - 1/size)
	}

	// Bonus jika ada orphan dalam cluster (lebih menarik untuk investigasi)
	for _, a := range c.Assets {
		for _, tag := range a.Tags {
			if tag == "orphan" {
				score += 0.2
				break
			}
		}
	}

	if score > 1.0 {
		score = 1.0
	}
	return score
}
