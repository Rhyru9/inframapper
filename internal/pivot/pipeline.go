// Package pivot berisi orchestrator utama InfraMapper.
package pivot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/inframapper/internal/layer"
	"github.com/yourusername/inframapper/internal/model"
	"github.com/yourusername/inframapper/internal/output"
)

// WebPusher adalah interface agar pipeline tidak hard-depend ke *web.Server.
// Memudahkan testing dan juga memungkinkan nil-safe call.
type WebPusher interface {
	Push(result *model.PipelineResult)
}

// StoreSaver adalah interface untuk persistensi sinyal attribution lintas-scan.
// Implementasinya ada di internal/store. Boleh nil — fitur attribution di-skip jika nil.
type StoreSaver interface {
	SaveScan(result *model.PipelineResult) error
}

// Run adalah entry point pipeline InfraMapper.
// webSrv boleh nil — jika tidak nil, hasil tiap layer di-push ke Neural Graph UI.
// storeSaver boleh nil — jika tidak nil, sinyal scan disimpan untuk attribution.
func Run(ctx context.Context, cfg model.Config, webSrv WebPusher, storeSaver StoreSaver) (*model.PipelineResult, error) {
	result := &model.PipelineResult{
		Target:    cfg.Target,
		StartedAt: time.Now(),
	}

	log.Printf("=== InfraMapper: target %s ===", cfg.Target)

	// ====================================================
	// LAYER 1 — SEED (iterasi 0)
	// ====================================================
	log.Printf("[pipeline] Layer 1: seed discovery")
	seedResult, err := layer.RunSeed(ctx, cfg, cfg.Target, 0)
	if err != nil {
		return nil, fmt.Errorf("L1 seed: %w", err)
	}
	result.Stats.L1SeedCount = seedResult.Stats.Total
	log.Printf("[pipeline] L1 selesai: %d subdomain", seedResult.Stats.Total)

	// Semua asset yang diketahui, dipakai untuk dedup re-seed
	allSubdomains := seedResult.Subdomains
	knownDomains := layer.BuildKnownDomainSet(nil, allSubdomains)

	// ====================================================
	// LAYER 2 — HTTPX ENRICHMENT
	// ====================================================
	log.Printf("[pipeline] Layer 2: httpx probe (%d subdomain)", len(allSubdomains))
	httpxResult, err := layer.RunHTTPX(ctx, cfg, allSubdomains)
	if err != nil {
		return nil, fmt.Errorf("L2 httpx: %w", err)
	}
	result.Stats.L2AliveCount = httpxResult.Stats.Alive
	result.DeadSubdomains = httpxResult.Dead

	allAssets := httpxResult.Alive
	log.Printf("[pipeline] L2 selesai: %d alive, %d dead", len(allAssets), len(httpxResult.Dead))
	result.AliveAssets = allAssets
	webPush(webSrv, result) // Push snapshot awal ke UI

	// ====================================================
	// GEO ENRICHMENT PASS — setelah L2, sebelum L3
	// Fix Bug #1: lookupASN() sebelumnya hanya bisa diakses lewat RunASNSweep().
	// Kalau L5 di-skip karena newIPs=0, seluruh assets keluar tanpa koordinat.
	// Pass ini enrich geo untuk semua alive assets secara independen dari L5.
	// ====================================================
	log.Printf("[pipeline] Geo enrichment: %d alive assets", len(allAssets))
	geoEnrichResult, geoErr := layer.RunGeoEnrichment(ctx, cfg, allAssets)
	if geoErr != nil {
		log.Printf("[pipeline] geo enrichment error (non-fatal): %v", geoErr)
	} else if cfg.Verbose {
		log.Printf("[pipeline] geo enrichment: %d enriched, %d failed, %d skipped",
			geoEnrichResult.Enriched, geoEnrichResult.Failed, geoEnrichResult.Skipped)
	}

	// Track IP awal dari DNS — dipakai orphan detection di layer 6
	originalDNSIPs := make(map[string]bool)
	for _, a := range allAssets {
		if a.IP != "" {
			originalDNSIPs[a.IP] = true
		}
	}

	// ====================================================
	// LAYER 3 — FAVICON HASH + SHODAN + FOFA PIVOT
	// Shodan dan FOFA berjalan paralel setelah favicon hash dikumpulkan.
	// Keduanya berkontribusi ke: new IPs, asset enrichment, cert domains.
	// ====================================================
	log.Printf("[pipeline] Layer 3: favicon hash + Shodan/FOFA pivot (paralel)")

	// Step 3a: Kumpulkan favicon hash dulu — dipakai oleh Shodan dan FOFA pivot
	if cfg.FaviconEnable {
		favResult, err := layer.RunFaviconHash(ctx, cfg, allAssets)
		if err != nil {
			log.Printf("[pipeline] L3 favicon error (non-fatal): %v", err)
		} else {
			result.Stats.L3FaviconCount = favResult.Enriched
		}
	}

	// Step 3b: Shodan + FOFA paralel
	type l3Result struct {
		shodanNewIPs   []string
		shodanHits     int
		fofaNewIPs     []string
		fofaNewIPInfos []layer.FOFANewIPInfo // enriched: IP + FaviconHash/JARM yang menemukannya
		fofaHits       int
		fofaCertDoms   []string
	}
	l3Ch := make(chan l3Result, 1)

	go func() {
		var r l3Result

		// Shodan dan FOFA berjalan di goroutine terpisah
		var wg3 sync.WaitGroup
		var mu3 sync.Mutex

		wg3.Add(1)
		go func() {
			defer wg3.Done()
			sr, err := layer.RunShodanPivot(ctx, cfg, allAssets)
			if err != nil {
				log.Printf("[pipeline] L3 Shodan error (non-fatal): %v", err)
				return
			}
			mu3.Lock()
			r.shodanHits = sr.Hits
			r.shodanNewIPs = sr.NewIPs
			mu3.Unlock()
		}()

		wg3.Add(1)
		go func() {
			defer wg3.Done()
			fr, err := layer.RunFOFAPivot(ctx, cfg, allAssets)
			if err != nil {
				log.Printf("[pipeline] L3 FOFA error (non-fatal): %v", err)
				return
			}
			mu3.Lock()
			r.fofaHits = fr.Hits
			r.fofaNewIPs = fr.NewIPs
			r.fofaNewIPInfos = fr.NewIPInfos
			r.fofaCertDoms = fr.CertDomains
			mu3.Unlock()
		}()

		wg3.Wait()
		l3Ch <- r
	}()

	l3 := <-l3Ch
	result.Stats.L3ShodanHits = l3.shodanHits
	result.Stats.L3FOFAHits = l3.fofaHits
	result.Stats.L3FOFANewIPs = len(l3.fofaNewIPs)
	result.Stats.L3FOFACertDoms = len(l3.fofaCertDoms)

	// ── FOFA new IP → Asset backfill ──────────────────────────────────────────
	// IP yang ditemukan FOFA via icon_hash pivot dikonversi ke *model.Asset
	// dengan FaviconHash + JARM yang dipakai untuk menemukannya.
	// Tanpa ini, IP tersebut tidak punya sinyal di ATTR graph dan tidak muncul
	// di favicon_hash / jarm hub meski secara faktual berbagi fingerprint yang sama.
	existingIPSet := make(map[string]bool, len(allAssets))
	for _, a := range allAssets {
		if a.IP != "" {
			existingIPSet[a.IP] = true
		}
		if a.Host != "" {
			existingIPSet[a.Host] = true
		}
	}
	for _, info := range l3.fofaNewIPInfos {
		if existingIPSet[info.IP] {
			// IP sudah ada di allAssets — pastikan FaviconHash ter-backfill
			for _, a := range allAssets {
				if (a.IP == info.IP || a.Host == info.IP) && a.FaviconHash == "" && info.FaviconHash != "" {
					a.FaviconHash = info.FaviconHash
				}
			}
			continue
		}
		existingIPSet[info.IP] = true
		newAsset := &model.Asset{
			Host:         info.IP,
			IP:           info.IP,
			Source:       model.SourceFOFA,
			Alive:        true, // FOFA confirmed responsive
			FaviconHash:  info.FaviconHash,
			DiscoveredAt: time.Now(),
		}
		if info.JARM != "" {
			newAsset.TLSCert = &model.TLSInfo{JARM: info.JARM}
		}
		if info.ASN != "" || info.Country != "" {
			newAsset.FOFAData = &model.FOFAResult{
				IconHash: info.FaviconHash,
				JARM:     info.JARM,
				ASN:      info.ASN,
				Country:  info.Country,
			}
		}
		allAssets = append(allAssets, newAsset)
		log.Printf("[pipeline] L3 FOFA new IP asset: %s (favicon=%s)", info.IP, info.FaviconHash)
	}

	// Gabungkan semua new IPs dari Shodan + FOFA untuk trigger L5
	shodanNewIPs := append(l3.shodanNewIPs, l3.fofaNewIPs...)

	if cfg.Verbose {
		log.Printf("[pipeline] L3 selesai: Shodan=%d hits, FOFA=%d hits, %d new IPs total, %d cert domains",
			l3.shodanHits, l3.fofaHits, len(shodanNewIPs), len(l3.fofaCertDoms))
	}

	// ====================================================
	// LAYER 4 — TLS SAN PIVOT + RE-SEED LOOP
	// Hard limit: maksimal SANMaxIter iterasi (default 2)
	// ====================================================
	log.Printf("[pipeline] Layer 4: TLS SAN pivot + re-seed loop (max iter=%d)", cfg.SANMaxIter)

	totalNewDomainsFromSAN := 0

	// Seed awal untuk L4: domain dari FOFA cert.domain (dari L3) juga masuk ke loop
	// ini equivalent dengan "re-seed dari sumber eksternal" — diberi iter=0 tapi source=fofa
	if len(l3.fofaCertDoms) > 0 {
		var fofaCertSubs []*model.Subdomain
		for _, d := range l3.fofaCertDoms {
			if !knownDomains[strings.ToLower(d)] {
				fofaCertSubs = append(fofaCertSubs, &model.Subdomain{
					Domain:    d,
					Source:    model.SourceFOFA,
					Iteration: 0,
				})
				knownDomains[strings.ToLower(d)] = true
			}
		}
		if len(fofaCertSubs) > 0 {
			log.Printf("[pipeline] L4 pre-seed: %d domain dari FOFA cert.domain", len(fofaCertSubs))
			fofaHTTPX, err := layer.RunHTTPX(ctx, cfg, fofaCertSubs)
			if err != nil {
				log.Printf("[pipeline] L4 FOFA cert pre-seed httpx error (non-fatal): %v", err)
			} else {
				allAssets = append(allAssets, fofaHTTPX.Alive...)
				result.DeadSubdomains = append(result.DeadSubdomains, fofaHTTPX.Dead...)
				result.Stats.L2AliveCount += fofaHTTPX.Stats.Alive
				totalNewDomainsFromSAN += len(fofaCertSubs)
				log.Printf("[pipeline] L4 FOFA cert pre-seed: %d alive", fofaHTTPX.Stats.Alive)
			}
		}
	}

	if cfg.SANReseedEnable {
		for iter := 1; iter <= cfg.SANMaxIter; iter++ {
			sanResult, err := layer.RunTLSPivot(ctx, cfg, allAssets, knownDomains)
			if err != nil {
				log.Printf("[pipeline] L4 iter %d error: %v", iter, err)
				break
			}

			if len(sanResult.NewDomains) == 0 {
				log.Printf("[pipeline] L4 iter %d: tidak ada domain baru, stop re-seed", iter)
				break
			}

			log.Printf("[pipeline] L4 iter %d: %d domain baru dari SAN", iter, len(sanResult.NewDomains))
			totalNewDomainsFromSAN += len(sanResult.NewDomains)
			result.Stats.L4Reseeds = iter

			// Re-seed: buat subdomain baru dengan flag source=SAN
			newSubs := layer.MergeSeedResults(sanResult.NewDomains, iter)

			// Update known domains sebelum httpx
			for _, s := range newSubs {
				knownDomains[strings.ToLower(s.Domain)] = true
			}

			// Probe subdomain baru
			newHTTPX, err := layer.RunHTTPX(ctx, cfg, newSubs)
			if err != nil {
				log.Printf("[pipeline] L4 re-seed httpx error: %v", err)
				break
			}

			allAssets = append(allAssets, newHTTPX.Alive...)
			result.DeadSubdomains = append(result.DeadSubdomains, newHTTPX.Dead...)
			result.Stats.L2AliveCount += newHTTPX.Stats.Alive

			log.Printf("[pipeline] L4 iter %d: %d new alive dari re-seed", iter, newHTTPX.Stats.Alive)
		}
	}

	result.Stats.L4SANNewDomains = totalNewDomainsFromSAN

	// ====================================================
	// LAYER 5 — ASN SWEEP (CONDITIONAL)
	// Hanya berjalan jika ada IP baru yang ditemukan di layer sebelumnya
	// ====================================================
	log.Printf("[pipeline] Layer 5: ASN sweep (conditional)")

	// Kumpulkan semua "new IPs" sebagai trigger kondisi layer 5
	allNewIPs := append(shodanNewIPs, collectNewIPs(allAssets, originalDNSIPs)...)

	if cfg.ASNSweepEnable {
		asnResult, err := layer.RunASNSweep(ctx, cfg, allAssets, allNewIPs)
		if err != nil {
			log.Printf("[pipeline] L5 ASN error (non-fatal): %v", err)
		} else {
			result.Stats.L5ASNSkipped = asnResult.Skipped
			result.Stats.L5CIDRCount = asnResult.CIDRCount
			result.Stats.L5NewIPs = len(asnResult.NewAssets)

			if !asnResult.Skipped {
				allAssets = append(allAssets, asnResult.NewAssets...)
				log.Printf("[pipeline] L5: %d IP baru dari sweep CIDR", len(asnResult.NewAssets))
			} else {
				log.Printf("[pipeline] L5 SKIPPED: %s", asnResult.SkipReason)
			}
		}
	} else {
		result.Stats.L5ASNSkipped = true
		log.Printf("[pipeline] L5 SKIPPED: disabled via config")
	}

	// ====================================================
	// SCOPE BOUNDARY — setelah ini tidak ada lagi network call
	// Layer 6-7 adalah pure data processing
	// ====================================================
	log.Printf("[pipeline] --- scope boundary: mulai correlation layer ---")

	result.AliveAssets = allAssets
	result.TotalSubdomains = result.Stats.L1SeedCount + totalNewDomainsFromSAN

	// ====================================================
	// LAYER 6 — CLUSTERING + ORPHAN DETECTION
	// ====================================================
	log.Printf("[pipeline] Layer 6: clustering")

	clusterResult, err := layer.RunClustering(cfg, allAssets, originalDNSIPs)
	if err != nil {
		log.Printf("[pipeline] L6 error (non-fatal): %v", err)
	} else {
		result.Clusters = clusterResult.Clusters
		result.Stats.L6ClusterCount = len(clusterResult.Clusters)
		log.Printf("[pipeline] L6: %d cluster, %d orphan", len(clusterResult.Clusters), len(clusterResult.Orphans))
	}

	// ====================================================
	// LAYER 7 — OUTPUT EXPORT
	// ====================================================
	result.FinishedAt = time.Now()

	log.Printf("[pipeline] Layer 7: output export")
	writer := output.New(cfg)
	if err := writer.WriteAll(result); err != nil {
		return nil, fmt.Errorf("L7 output: %w", err)
	}

	// Final push ke UI dengan cluster info lengkap
	webPush(webSrv, result)

	// Attribution: simpan sinyal ke store jika aktif
	if storeSaver != nil {
		if err := storeSaver.SaveScan(result); err != nil {
			log.Printf("[pipeline] attribution store save error (non-fatal): %v", err)
		} else if cfg.Verbose {
			log.Printf("[pipeline] attribution signals saved for %s", cfg.Target)
		}
	}

	log.Printf("=== Pipeline selesai dalam %s ===", result.FinishedAt.Sub(result.StartedAt).Round(time.Second))

	return result, nil
}

// webPush mengirim snapshot result ke UI jika webSrv tidak nil.
func webPush(webSrv WebPusher, result *model.PipelineResult) {
	if webSrv != nil {
		webSrv.Push(result)
	}
}

// collectNewIPs mengambil IP dari assets yang tidak ada di DNS awal.
func collectNewIPs(assets []*model.Asset, originalDNSIPs map[string]bool) []string {
	var newIPs []string
	seen := make(map[string]bool)
	for _, a := range assets {
		if a.IP != "" && !originalDNSIPs[a.IP] && !seen[a.IP] {
			seen[a.IP] = true
			newIPs = append(newIPs, a.IP)
		}
	}
	return newIPs
}
