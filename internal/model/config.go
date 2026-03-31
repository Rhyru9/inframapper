package model

// Config adalah konfigurasi tunggal yang dioper ke seluruh pipeline.
// Diisi dari flag CLI atau file YAML.
type Config struct {
	// Target
	Target string // domain utama, e.g. "example.com"

	// Layer 1
	Subfinder   bool
	Amass       bool
	Assetfinder bool
	CrtSh       bool

	// Layer 2
	HTTPTimeout    int  // detik
	HTTPConcurrent int  // jumlah goroutine paralel httpx
	HTTPRetry      int  // retry per host

	// Layer 3
	ShodanAPIKey  string
	CensysAPIID   string
	CensysSecret  string
	FaviconEnable bool

	// Layer 3 - FOFA
	FOFAEmail     string // email akun FOFA
	FOFAKey       string // API key FOFA
	FOFAEnable    bool   // master switch
	// Field yang di-request dari FOFA (subset dari Appendix 1 yang relevan untuk scope kita)
	// Default: host,ip,port,asn,org,title,server,jarm,icon_hash,header_hash,cert.domain,icp,country
	FOFAFields    string

	// Layer 4 - TLS SAN re-seed
	SANReseedEnable bool
	SANMaxIter      int // default 2, hard cap

	// Geo enrichment token
	IPInfoToken       string // ipinfo.io token (opsional). Tanpa token: fallback ke ip-api.com (free)

	// Layer 5 - ASN sweep (conditional)
	ASNSweepEnable   bool
	ASNMaxCIDR       int // max CIDR yang di-sweep, default 256 (/24)
	ASNMinNewIPs     int // min IP baru dari layer 4 agar layer 5 jalan, default 1

	// Layer 6 - Clustering
	ClusterByFavicon bool
	ClusterByASN     bool
	ClusterByTLS     bool

	// Layer 7 - Output
	OutputDir    string
	OutputFormat []string // "json", "csv", "markdown"
	StorageType  string   // "sqlite", "duckdb", "memory"

	// Optional enrichment (berjalan setelah pipeline selesai, non-blocking)
	WaybackEnable bool

	// Verbosity
	Verbose bool
	Debug   bool
}

// DefaultConfig mengembalikan config dengan nilai sensible default.
func DefaultConfig() Config {
	return Config{
		Subfinder:        true,
		Amass:            false, // heavy, off by default
		Assetfinder:      true,
		CrtSh:            true,
		HTTPTimeout:      10,
		HTTPConcurrent:   50,
		HTTPRetry:        2,
		FaviconEnable:    true,
		FOFAEnable:       false, // off by default — butuh API key
		FOFAFields:       "host,ip,port,asn,org,title,server,jarm,icon_hash,header_hash,cert.domain,icp,country",
		SANReseedEnable:  true,
		SANMaxIter:       2,
		ASNSweepEnable:   true,
		ASNMaxCIDR:       256,
		ASNMinNewIPs:     1,
		ClusterByFavicon: true,
		ClusterByASN:     true,
		ClusterByTLS:     true,
		OutputDir:        "./output",
		OutputFormat:     []string{"json", "markdown"},
		StorageType:      "sqlite",
	}
}
