package layer

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/inframapper/internal/model"
	"github.com/yourusername/inframapper/internal/util"
)

// SANResult adalah output layer 4.
type SANResult struct {
	NewDomains []string // domain dari SAN yang belum pernah ada di seed
	TLSInfos   int      // jumlah asset yang berhasil di-parse TLS-nya
}

// RunTLSPivot mengambil sertifikat TLS dari tiap asset dan mengekstrak SAN.
// Domain baru yang ditemukan akan di-feed balik ke layer 1 (re-seed).
func RunTLSPivot(ctx context.Context, cfg model.Config, assets []*model.Asset, knownDomains map[string]bool) (*SANResult, error) {
	sem := make(chan struct{}, 30)
	var mu sync.Mutex
	var wg sync.WaitGroup

	result := &SANResult{}
	newDomainSet := make(map[string]bool)

	for _, asset := range assets {
		if !asset.Alive {
			continue
		}

		wg.Add(1)
		go func(a *model.Asset) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			tlsInfo, err := fetchTLSInfo(ctx, a.Host)
			if err != nil {
				if cfg.Debug {
					log.Printf("[L4/TLS] %s: %v", a.Host, err)
				}
				return
			}

			mu.Lock()
			defer mu.Unlock()

			a.TLSCert = tlsInfo
			result.TLSInfos++

			// Cari domain baru dari SAN
			for _, san := range tlsInfo.SANs {
				san = strings.ToLower(strings.TrimSpace(san))
				san = strings.TrimPrefix(san, "*.")

				// Skip jika bukan subdomain target atau sudah dikenal
				if !util.IsSubdomainOf(san, cfg.Target) {
					continue
				}
				if knownDomains[san] || newDomainSet[san] {
					continue
				}

				newDomainSet[san] = true
				result.NewDomains = append(result.NewDomains, san)
			}
		}(asset)
	}

	wg.Wait()

	if cfg.Verbose {
		log.Printf("[L4] TLS pivot: %d cert dibaca, %d domain baru ditemukan",
			result.TLSInfos, len(result.NewDomains))
	}

	return result, nil
}

// fetchTLSInfo membuka koneksi TLS dan mengekstrak data sertifikat.
func fetchTLSInfo(ctx context.Context, host string) (*model.TLSInfo, error) {
	// Pastikan port ada
	target := host
	if !strings.Contains(host, ":") {
		target = host + ":443"
	}

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec — kita mau data cert meski expired
			ServerName:         strings.Split(host, ":")[0],
		},
	}

	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("TLS dial: %w", err)
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return nil, fmt.Errorf("bukan TLS connection")
	}

	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("tidak ada sertifikat")
	}

	leaf := certs[0]

	// Kumpulkan semua SAN (DNS names + IP addresses)
	var sans []string
	sans = append(sans, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}

	return &model.TLSInfo{
		CommonName:  leaf.Subject.CommonName,
		SANs:        sans,
		Issuer:      leaf.Issuer.CommonName,
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Fingerprint: fmt.Sprintf("%x", leaf.Raw[:8]), // abbreviated fingerprint
	}, nil
}

// MergeSeedResults menggabungkan hasil re-seed dengan asset yang sudah ada.
// Mengembalikan daftar subdomain baru sebagai []*model.Subdomain dengan flag source=SAN.
func MergeSeedResults(newDomains []string, iteration int) []*model.Subdomain {
	var out []*model.Subdomain
	for _, d := range newDomains {
		out = append(out, &model.Subdomain{
			Domain:    d,
			Source:    model.SourceSAN,
			Iteration: iteration,
		})
	}
	return out
}

// BuildKnownDomainSet membuat map lookup dari asset yang sudah diproses.
func BuildKnownDomainSet(assets []*model.Asset, subs []*model.Subdomain) map[string]bool {
	known := make(map[string]bool)
	for _, a := range assets {
		known[strings.ToLower(a.Host)] = true
	}
	for _, s := range subs {
		known[strings.ToLower(s.Domain)] = true
	}
	return known
}
