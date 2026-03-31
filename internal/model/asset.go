package model

import "time"

// Source menandai dari mana sebuah asset pertama kali ditemukan.
type Source string

const (
	SourceSubfinder   Source = "subfinder"
	SourceAmass       Source = "amass"
	SourceAssetfinder Source = "assetfinder"
	SourceCrtSh       Source = "crt.sh"
	SourceSAN         Source = "san"   // re-seed dari TLS SAN
	SourceASN         Source = "asn"   // sweep dari ASN/CIDR
	SourceFOFA        Source = "fofa"  // domain dari FOFA cert.domain
	SourceManual      Source = "manual"
)

// Subdomain adalah hasil dari layer 1 sebelum di-resolve.
type Subdomain struct {
	Domain    string
	Source    Source
	Iteration int // 0 = seed awal, 1-2 = re-seed dari SAN
}

// Asset adalah unit data utama setelah layer 2 (httpx enrichment).
// Setiap field yang belum diketahui dibiarkan zero-value, bukan di-null.
type Asset struct {
	// Identitas
	Host   string // subdomain atau IP
	Source Source
	Iter   int // iterasi re-seed

	// Hasil httpx (layer 2)
	Alive       bool
	StatusCode  int
	Title       string
	ContentType string
	Server      string
	IP          string // IP yang di-resolve httpx
	Port        int
	HTTPS       bool
	FaviconHash string // MurmurHash3 favicon, diisi layer 3

	// Hasil Shodan (layer 3)
	ShodanData *ShodanResult

	// Hasil FOFA (layer 3) — berjalan paralel dengan Shodan
	FOFAData *FOFAResult

	// Hasil TLS (layer 3)
	TLSCert *TLSInfo

	// Hasil ASN sweep (layer 5)
	ASNInfo *ASNInfo

	// Metadata
	DiscoveredAt time.Time
	Tags         []string // label bebas: "cdn", "cloud", "orphan", dll
}

// ShodanResult menyimpan data yang relevan dari Shodan API.
// Kita tidak menyimpan raw response supaya output tetap lean.
type ShodanResult struct {
	IPStr   string
	Ports   []int
	Org     string
	ISP     string
	ASN     string
	Country string
	Tags    []string // dari Shodan: "cloud", "cdn", dll
	Vulns   []string // CVE list (hanya ID, bukan detail)
}

// TLSInfo menyimpan data sertifikat yang berguna untuk pivot.
type TLSInfo struct {
	CommonName  string
	SANs        []string // Subject Alternative Names — ini yang di-pivot
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string // SHA256
	JARM        string // JARM fingerprint (diisi oleh FOFA jika tersedia)
}

// FOFAResult menyimpan data enrichment dari FOFA API.
// Field dipilih yang relevan untuk discovery & clustering — bukan raw dump.
type FOFAResult struct {
	// Pivot signals
	IconHash    string // sama dengan FaviconHash, dipakai konfirmasi silang
	HeaderHash  string // http response header fingerprint — sinyal unik FOFA
	JARM        string // TLS behavioral fingerprint

	// Identity enrichment
	ICP         string // ICP filing number (berguna untuk target China/Asia)
	Product     string // product name dari FOFA fingerprint
	Country     string
	ASN         string
	Org         string

	// Geo — diisi oleh GeoEnrichment pass setelah FOFA pivot
	// (FOFA API sendiri tidak return lat/lon, tapi Country bisa jadi fallback)
	Lat         float64
	Lon         float64
	City        string

	// Peer IPs: IP lain yang ditemukan FOFA dengan icon_hash atau header_hash yang sama
	PeerIPs     []string

	// CertDomains: domain dari cert.domain field FOFA — lebih kaya dari SAN manual
	CertDomains []string
}

// ASNInfo hasil lookup ASN dari IP.
type ASNInfo struct {
	Number      int
	ASN         string // format "AS12345"
	Name        string
	CIDR        string
	Country     string
	City        string  // kota dari ipinfo.io
	Region      string  // region/state
	Lat         float64 // latitude dari ipinfo.io "loc" field
	Lon         float64 // longitude dari ipinfo.io "loc" field
	IPsInRange  int     // jumlah IP dalam CIDR (untuk estimasi noise)
}

// GeoPoint adalah koordinat lat/lon untuk rendering di world map.
// Diisi dari ASNInfo setelah lookup selesai.
type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	IP  string  `json:"ip"`
}

// Cluster adalah hasil layer 6 — pengelompokan asset berdasarkan sinyal.
type Cluster struct {
	ID       string
	Label    string   // e.g. "cloudflare-cdn", "aws-us-east", "on-prem-asn1234"
	Assets   []*Asset
	Pivot    string   // sinyal yang membentuk cluster: "favicon_hash", "asn", "tls_issuer"
	Score    float64  // 0.0-1.0, dihitung scoring engine
	Orphan   bool     // true jika tidak ada di DNS awal
}

// PipelineResult adalah output final dari seluruh pipeline.
type PipelineResult struct {
	Target     string
	StartedAt  time.Time
	FinishedAt time.Time

	TotalSubdomains int
	AliveAssets     []*Asset
	DeadSubdomains  []string
	Clusters        []*Cluster

	// Statistik per layer
	Stats LayerStats
}

// LayerStats merekam berapa item masuk dan keluar dari tiap layer.
type LayerStats struct {
	L1SeedCount     int
	L2AliveCount    int
	L3FaviconCount  int // asset yang punya favicon hash
	L3ShodanHits    int
	L3FOFAHits      int    // pivot sukses dari FOFA
	L3FOFANewIPs    int    // IP baru eksklusif dari FOFA
	L3FOFACertDoms  int    // domain baru dari FOFA cert.domain
	L4SANNewDomains int    // domain baru dari SAN pivot
	L4Reseeds       int    // berapa kali re-seed dijalankan
	L5ASNSkipped    bool
	L5CIDRCount     int
	L5NewIPs        int
	L6ClusterCount  int
}
