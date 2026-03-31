// Package sec menangani pembacaan file .sec — credential store lokal InfraMapper.
//
// Format file .sec:
//
//	# komentar diawali #
//	shodan   = "your_shodan_key"
//	fofa_email = "you@mail.com"
//	fofa_key   = "your_fofa_key"
//	censys_id  = "your_censys_id"
//	censys_secret = "your_censys_secret"
//
// Prioritas loading (dari terendah ke tertinggi — yang lebih tinggi override):
//  1. .sec di home directory   (~/.sec)
//  2. .sec di direktori kerja  (./.sec)
//  3. Environment variable
//  4. CLI flag (tertinggi)
//
// File .sec TIDAK boleh di-commit ke git.
// Tambahkan `.sec` ke .gitignore.
package sec

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yourusername/inframapper/internal/model"
)

// Creds adalah hasil parse dari file .sec.
type Creds struct {
	ShodanKey    string
	FOFAEmail    string
	FOFAKey      string
	CensysID     string
	CensysSecret string
	IPInfoToken  string // ipinfo.io token untuk geo lookup (opsional)
	// Slot untuk API key lain yang ditambahkan di masa depan
	Extra map[string]string
}

// Load mencoba membaca .sec dari dua lokasi secara berurutan:
// ~/.sec (global) lalu ./.sec (project-level, override global).
// Tidak ada error fatal jika file tidak ditemukan — creds kosong dikembalikan.
func Load() (*Creds, []string) {
	creds := &Creds{Extra: make(map[string]string)}
	var loaded []string

	// 1. Global: ~/.sec
	if home, err := os.UserHomeDir(); err == nil {
		globalPath := filepath.Join(home, ".sec")
		if err := parseFile(globalPath, creds); err == nil {
			loaded = append(loaded, globalPath)
		}
	}

	// 2. Local: ./.sec (override global)
	localPath := ".sec"
	if err := parseFile(localPath, creds); err == nil {
		loaded = append(loaded, localPath)
	}

	return creds, loaded
}

// LoadFrom membaca dari path eksplisit.
func LoadFrom(path string) (*Creds, error) {
	creds := &Creds{Extra: make(map[string]string)}
	if err := parseFile(path, creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// Apply mengisi cfg dengan nilai dari creds.
// Hanya mengisi field yang masih kosong — tidak override nilai yang sudah ada
// (misalnya dari env var atau CLI flag yang sudah diparse sebelumnya).
func (c *Creds) Apply(cfg *model.Config) {
	if cfg.ShodanAPIKey == "" && c.ShodanKey != "" {
		cfg.ShodanAPIKey = c.ShodanKey
	}
	if cfg.FOFAEmail == "" && c.FOFAEmail != "" {
		cfg.FOFAEmail = c.FOFAEmail
	}
	if cfg.FOFAKey == "" && c.FOFAKey != "" {
		cfg.FOFAKey = c.FOFAKey
	}
	if cfg.CensysAPIID == "" && c.CensysID != "" {
		cfg.CensysAPIID = c.CensysID
	}
	if cfg.CensysSecret == "" && c.CensysSecret != "" {
		cfg.CensysSecret = c.CensysSecret
	}
	if cfg.IPInfoToken == "" && c.IPInfoToken != "" {
		cfg.IPInfoToken = c.IPInfoToken
	}
}

// parseFile membaca dan mem-parse satu file .sec ke dalam creds.
func parseFile(path string, creds *Creds) error {
	f, err := os.Open(path)
	if err != nil {
		return err // file tidak ada — bukan error fatal
	}
	defer f.Close()

	// Periksa permission — .sec seharusnya hanya readable oleh owner (0600)
	if info, err := f.Stat(); err == nil {
		mode := info.Mode().Perm()
		if mode&0o077 != 0 {
			fmt.Fprintf(os.Stderr,
				"[!] WARNING: %s terbaca oleh group/others (mode %04o). "+
					"Jalankan: chmod 600 %s\n",
				path, mode, path,
			)
		}
	}

	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip baris kosong dan komentar
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Format: key = "value" atau key = value
		key, val, ok := parseLine(line)
		if !ok {
			// Baris tidak valid — log tapi lanjut
			fmt.Fprintf(os.Stderr, "[sec] %s:%d: format tidak valid, skip: %q\n", path, lineNum, line)
			continue
		}

		assignCred(creds, key, val)
	}

	return scanner.Err()
}

// parseLine mem-parse satu baris format `key = "value"` atau `key = value`.
// Return key (lowercase, trimmed), value (stripped dari quotes), ok.
func parseLine(line string) (string, string, bool) {
	idx := strings.IndexByte(line, '=')
	if idx < 0 {
		return "", "", false
	}

	key := strings.ToLower(strings.TrimSpace(line[:idx]))
	val := strings.TrimSpace(line[idx+1:])

	if key == "" {
		return "", "", false
	}

	// Strip optional quotes: "value" atau 'value'
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}

	return key, val, true
}

// assignCred mengisi field creds berdasarkan key.
// Key yang tidak dikenal masuk ke Extra map.
func assignCred(creds *Creds, key, val string) {
	// Normalisasi: strip dash/underscore variations
	// "shodan_key", "shodan-key", "shodan" → semua valid
	switch key {
	case "shodan", "shodan_key", "shodan-key", "shodan_api_key":
		creds.ShodanKey = val

	case "fofa_email", "fofa-email":
		creds.FOFAEmail = val

	case "fofa_key", "fofa-key", "fofa_api_key", "fofa":
		creds.FOFAKey = val

	case "censys_id", "censys-id", "censys_api_id":
		creds.CensysID = val

	case "censys_secret", "censys-secret":
		creds.CensysSecret = val

	case "ipinfo_token", "ipinfo-token", "ipinfo_key", "ipinfo":
		creds.IPInfoToken = val

	default:
		// Simpan untuk kebutuhan masa depan atau custom key
		creds.Extra[key] = val
	}
}

// GenerateTemplate menulis template .sec ke path yang diberikan.
// Dipakai oleh subcommand `inframapper -init-sec`.
func GenerateTemplate(path string) error {
	// Jangan overwrite jika sudah ada
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s sudah ada, tidak di-overwrite", path)
	}

	content := `# InfraMapper credentials file
# Simpan file ini dengan permission 600: chmod 600 .sec
# JANGAN commit file ini ke git — tambahkan .sec ke .gitignore
#
# Format: key = "value"
# Komentar diawali dengan #

# Shodan — https://account.shodan.io/
shodan = ""

# FOFA — https://fofa.info/user/users/api
fofa_email = ""
fofa_key   = ""

# Censys — https://search.censys.io/account/api
censys_id     = ""
censys_secret = ""

# ipinfo.io — https://ipinfo.io/account/token
# Opsional: tanpa token pakai ip-api.com (gratis, 45 req/mnt).
# Dengan token: rate limit lebih tinggi & data lebih akurat.
ipinfo_token = ""
`

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("gagal buat %s: %w", path, err)
	}
	defer f.Close()

	_, err = f.WriteString(content)
	return err
}

// Redacted mengembalikan representasi sensor dari creds untuk logging.
// Tidak pernah print nilai aktual ke log.
func (c *Creds) Redacted() string {
	parts := []string{}
	if c.ShodanKey != "" {
		parts = append(parts, fmt.Sprintf("shodan=%s", maskVal(c.ShodanKey)))
	}
	if c.FOFAEmail != "" {
		parts = append(parts, fmt.Sprintf("fofa_email=%s", maskEmail(c.FOFAEmail)))
	}
	if c.FOFAKey != "" {
		parts = append(parts, fmt.Sprintf("fofa_key=%s", maskVal(c.FOFAKey)))
	}
	if c.CensysID != "" {
		parts = append(parts, fmt.Sprintf("censys_id=%s", maskVal(c.CensysID)))
	}
	if len(parts) == 0 {
		return "(kosong)"
	}
	return strings.Join(parts, ", ")
}

// maskVal menyembunyikan sebagian besar karakter API key.
// "abcdef123456" → "abc…456"
func maskVal(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:3] + "…" + s[len(s)-3:]
}

// maskEmail menyembunyikan bagian tengah email.
// "user@example.com" → "us…@example.com"
func maskEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at < 2 {
		return "***"
	}
	return s[:2] + "…" + s[at:]
}

// MaskVal adalah versi public dari maskVal untuk dipakai di package lain (main).
func MaskVal(s string) string { return maskVal(s) }

// MaskEmail adalah versi public dari maskEmail untuk dipakai di package lain (main).
func MaskEmail(s string) string { return maskEmail(s) }
