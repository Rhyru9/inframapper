package layer

// GeoEnrichment adalah pass terpisah yang mengisi lat/lon/city ke semua alive assets.
//
// Masalah yang diperbaiki (Bug #1):
//   Sebelumnya satu-satunya cara mendapat geo coords adalah lewat lookupASN() yang
//   hanya berjalan di dalam RunASNSweep(). Kalau L5 di-skip (karena newIPs < minimum),
//   seluruh 103 assets keluar dengan ASNInfo == nil → semua node tanpa koordinat.
//
//   Solusi: jalankan geo enrichment langsung dari IP yang sudah diketahui SETELAH L2,
//   tanpa syarat apapun. Pass ini call ipinfo.io hanya untuk IP yang belum punya coords.

import (
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourusername/inframapper/internal/model"
)

// GeoEnrichResult merangkum hasil geo enrichment pass.
type GeoEnrichResult struct {
	Enriched int // jumlah asset yang berhasil dapat lat/lon
	Skipped  int // sudah punya ASNInfo atau IP kosong
	Failed   int // call gagal / timeout
}

// RunGeoEnrichment mengisi ASNInfo.Lat/Lon/City/Country untuk semua alive assets
// yang IP-nya belum ter-enrich. Dipanggil setelah L2, sebelum L3.
//
// Concurrency: sama dengan cfg.HTTPConcurrent, biar tidak flood ipinfo.io.
// Idempotent: asset yang sudah punya ASNInfo dengan Lat != 0 di-skip.
func RunGeoEnrichment(ctx context.Context, cfg model.Config, assets []*model.Asset) (*GeoEnrichResult, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	result := &GeoEnrichResult{}

	// Kumpulkan asset yang perlu di-enrich
	var toEnrich []*model.Asset
	for _, a := range assets {
		if !a.Alive || a.IP == "" {
			result.Skipped++
			continue
		}
		// Sudah punya coords yang valid → skip
		if a.ASNInfo != nil && (a.ASNInfo.Lat != 0 || a.ASNInfo.Lon != 0) {
			result.Skipped++
			continue
		}
		toEnrich = append(toEnrich, a)
	}

	if len(toEnrich) == 0 {
		return result, nil
	}

	if cfg.Verbose {
		log.Printf("[geo] enriching %d assets (skipping %d already done)", len(toEnrich), result.Skipped)
	}

	// Dedup by IP — jangan call ipinfo.io dua kali untuk IP yang sama
	type geoResult struct {
		info *model.ASNInfo
		err  error
	}
	ipCache := make(map[string]*geoResult)
	var cacheMu sync.Mutex

	concurrency := cfg.HTTPConcurrent
	if concurrency <= 0 {
		concurrency = 20
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var enriched, failed int64

	for _, asset := range toEnrich {
		a := asset
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			select {
			case <-ctx.Done():
				return
			default:
			}

			// Check cache first
			cacheMu.Lock()
			cached, hit := ipCache[a.IP]
			cacheMu.Unlock()

			var info *model.ASNInfo
			var err error

			if hit {
				info = cached.info
				err = cached.err
			} else {
				info, err = lookupASN(ctx, client, a.IP, cfg.IPInfoToken)
				cacheMu.Lock()
				ipCache[a.IP] = &geoResult{info: info, err: err}
				cacheMu.Unlock()
			}

			if err != nil {
				if cfg.Debug {
					log.Printf("[geo] lookup %s: %v", a.IP, err)
				}
				atomic.AddInt64(&failed, 1)
				return
			}

			if info == nil || (info.Lat == 0 && info.Lon == 0 && info.Country == "") {
				atomic.AddInt64(&failed, 1)
				return
			}

			// Merge: jika asset sudah punya ASNInfo dari L5 sweep (jarang),
			// jangan overwrite — hanya isi yang kosong.
			if a.ASNInfo == nil {
				a.ASNInfo = info
			} else {
				if a.ASNInfo.Lat == 0 {
					a.ASNInfo.Lat = info.Lat
				}
				if a.ASNInfo.Lon == 0 {
					a.ASNInfo.Lon = info.Lon
				}
				if a.ASNInfo.City == "" {
					a.ASNInfo.City = info.City
				}
				if a.ASNInfo.Country == "" {
					a.ASNInfo.Country = info.Country
				}
			}
			atomic.AddInt64(&enriched, 1)
		}()
	}

	wg.Wait()
	result.Enriched = int(atomic.LoadInt64(&enriched))
	result.Failed = int(atomic.LoadInt64(&failed))

	if cfg.Verbose {
		log.Printf("[geo] done: %d enriched, %d failed, %d skipped",
			result.Enriched, result.Failed, result.Skipped)
	}
	return result, nil
}
