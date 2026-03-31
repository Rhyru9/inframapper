package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yourusername/inframapper/internal/model"
	"github.com/yourusername/inframapper/internal/pivot"
	"github.com/yourusername/inframapper/internal/sec"
	"github.com/yourusername/inframapper/internal/web"
)

const version = "0.1.0"

func main() {
	cfg := model.DefaultConfig()

	// ─────────────────────────────────────────────────────────────────
	// STEP 1: Handle subcommand sederhana sebelum flag.Parse()
	// ─────────────────────────────────────────────────────────────────
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-init-sec", "--init-sec", "init-sec":
			path := ".sec"
			if len(os.Args) > 2 && !strings.HasPrefix(os.Args[2], "-") {
				path = os.Args[2]
			}
			if err := sec.GenerateTemplate(path); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("[+] Template credentials dibuat: %s\n", path)
			fmt.Printf("[!] Isi nilai API key, lalu: chmod 600 %s\n", path)
			fmt.Printf("[!] Tambahkan ke .gitignore: echo '.sec' >> .gitignore\n")
			os.Exit(0)

		case "-version", "--version", "version":
			fmt.Printf("InfraMapper v%s\n", version)
			os.Exit(0)
		}
	}

	// ─────────────────────────────────────────────────────────────────
	// STEP 2: Load .sec SEBELUM flag default di-set
	// Prioritas (terendah → tertinggi):
	//   ~/.sec  →  ./.sec  →  env var  →  CLI flag
	// ─────────────────────────────────────────────────────────────────
	secPath := customSecPath(os.Args[1:])

	var creds *sec.Creds
	var secLoaded []string

	if secPath != "" {
		c, err := sec.LoadFrom(secPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error membaca %s: %v\n", secPath, err)
			os.Exit(1)
		}
		creds = c
		secLoaded = []string{secPath}
	} else {
		creds, secLoaded = sec.Load()
	}

	// Apply .sec ke cfg — mengisi field yang kosong
	creds.Apply(&cfg)

	// ─────────────────────────────────────────────────────────────────
	// STEP 3: Definisi flags — default value sudah include nilai dari .sec
	// ─────────────────────────────────────────────────────────────────
	flag.StringVar(&cfg.Target, "target", cfg.Target, "Target domain (wajib). Contoh: example.com")
	flag.StringVar(&cfg.Target, "t", cfg.Target, "Alias untuk -target")

	flag.String("sec", "", "Path ke file .sec credentials (default: ~/.sec lalu ./.sec)")

	flag.BoolVar(&cfg.Subfinder, "subfinder", cfg.Subfinder, "Gunakan subfinder (butuh binary di PATH)")
	flag.BoolVar(&cfg.Amass, "amass", cfg.Amass, "Gunakan amass passive (lambat, off by default)")
	flag.BoolVar(&cfg.Assetfinder, "assetfinder", cfg.Assetfinder, "Gunakan assetfinder")
	flag.BoolVar(&cfg.CrtSh, "crtsh", cfg.CrtSh, "Gunakan crt.sh certificate transparency")

	flag.IntVar(&cfg.HTTPConcurrent, "concurrent", cfg.HTTPConcurrent, "Jumlah goroutine paralel untuk httpx probe")
	flag.IntVar(&cfg.HTTPTimeout, "timeout", cfg.HTTPTimeout, "Timeout HTTP per request (detik)")

	// API keys — default dari .sec atau env var, bisa di-override CLI flag
	shodanKey := flag.String("shodan-key", firstNonEmpty(cfg.ShodanAPIKey, os.Getenv("SHODAN_API_KEY")), "Shodan API key (atau .sec/env SHODAN_API_KEY)")
	fofaEmail  := flag.String("fofa-email", firstNonEmpty(cfg.FOFAEmail, os.Getenv("FOFA_EMAIL")), "FOFA email (atau .sec/env FOFA_EMAIL)")
	fofaKey    := flag.String("fofa-key",   firstNonEmpty(cfg.FOFAKey, os.Getenv("FOFA_KEY")), "FOFA API key (atau .sec/env FOFA_KEY)")
	flag.BoolVar(&cfg.FOFAEnable, "fofa", cfg.FOFAEnable, "Aktifkan FOFA pivot")
	ipinfoToken := flag.String("ipinfo-token", firstNonEmpty(cfg.IPInfoToken, os.Getenv("IPINFO_TOKEN")), "ipinfo.io token untuk geo lookup (opsional, fallback ke ip-api.com)")

	flag.BoolVar(&cfg.SANReseedEnable, "san-reseed", cfg.SANReseedEnable, "Aktifkan TLS SAN re-seed loop")
	flag.IntVar(&cfg.SANMaxIter, "san-iter", cfg.SANMaxIter, "Maksimal iterasi re-seed dari SAN (hard cap)")

	flag.BoolVar(&cfg.ASNSweepEnable, "asn-sweep", cfg.ASNSweepEnable, "Aktifkan ASN/CIDR sweep (conditional)")
	flag.IntVar(&cfg.ASNMaxCIDR, "asn-max-cidr", cfg.ASNMaxCIDR, "Maksimal host per CIDR yang di-sweep")

	flag.StringVar(&cfg.OutputDir, "output", cfg.OutputDir, "Direktori output")
	outputFormats := flag.String("format", "json,markdown", "Format output: json,csv,markdown (comma-separated)")

	exposePort := flag.Int("expose", 0, "Aktifkan Neural Graph UI di port ini (contoh: --expose 3221)")
	viewFile   := flag.String("view", "", "Tampilkan hasil scan yang sudah ada di GUI tanpa re-scan.\n\tContoh: --view output/garuda-indonesia.com_20260401-050959.json --expose 3221")

	flag.BoolVar(&cfg.Verbose, "v", cfg.Verbose, "Verbose logging")
	flag.BoolVar(&cfg.Debug, "debug", cfg.Debug, "Debug logging (lebih detail dari -v)")

	showVersion := flag.Bool("version", false, "Tampilkan versi")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `InfraMapper v%s — passive infra discovery & clustering tool

Usage:
  inframapper -target example.com [options]       jalankan scan baru
  inframapper -view <file.json> --expose <port>   tampilkan hasil scan lama di GUI
  inframapper -init-sec [path]                    buat template .sec

Options:
`, version)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Credentials (.sec file):
  Simpan API key di .sec agar tidak perlu pass via flag setiap saat.

  Buat template:
    inframapper -init-sec

  Format .sec:
    # komentar
    shodan        = "your_shodan_key"
    fofa_email    = "you@mail.com"
    fofa_key      = "your_fofa_key"
    censys_id     = "your_id"
    censys_secret = "your_secret"

  Load order (tertinggi override terendah):
    ~/.sec  →  ./.sec  →  env var  →  CLI flag

Examples:
  # Setup sekali pakai
  inframapper -init-sec
  vim .sec          # isi API key
  chmod 600 .sec

  # Jalankan — credentials otomatis dari .sec
  inframapper -target example.com -v

  # .sec di path custom
  inframapper -target example.com -sec ~/secrets/recon.sec

  # Override satu key dari .sec via flag
  inframapper -target example.com -shodan-key OTHER_KEY

  # Phase 1 MVP: tanpa API eksternal
  inframapper -target example.com -san-reseed=false -asn-sweep=false

Environment variables (alternatif .sec):
  SHODAN_API_KEY    FOFA_EMAIL    FOFA_KEY

Scope:
  Tool ini hanya bertanggung jawab sampai "menemukan dan mengelompokkan aset".
  Tidak ada: full port scan, vuln scanning, exploitation, web UI.
`)
	}

	flag.Parse()

	if *showVersion {
		fmt.Printf("InfraMapper v%s\n", version)
		os.Exit(0)
	}

	// ─────────────────────────────────────────────────────────────────
	// STEP 4: Terapkan nilai final dari CLI flag (menang atas .sec)
	// ─────────────────────────────────────────────────────────────────
	cfg.ShodanAPIKey = *shodanKey
	cfg.FOFAEmail    = *fofaEmail
	cfg.FOFAKey      = *fofaKey
	cfg.IPInfoToken  = *ipinfoToken

	if cfg.FOFAEmail != "" && cfg.FOFAKey != "" {
		cfg.FOFAEnable = true
	}

	// ─────────────────────────────────────────────────────────────────
	// STEP 5: Validasi
	// ─────────────────────────────────────────────────────────────────
	if cfg.Target == "" && *viewFile == "" {
		fmt.Fprintf(os.Stderr, "Error: -target wajib diisi (atau gunakan -view <file.json> untuk replay hasil scan)\n\n")
		flag.Usage()
		os.Exit(1)
	}

	if cfg.Target != "" {
		cfg.Target = strings.ToLower(strings.TrimSpace(cfg.Target))
		cfg.Target = strings.TrimPrefix(cfg.Target, "http://")
		cfg.Target = strings.TrimPrefix(cfg.Target, "https://")
		cfg.Target = strings.TrimSuffix(cfg.Target, "/")
	}

	cfg.OutputFormat = strings.Split(*outputFormats, ",")

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	// ─────────────────────────────────────────────────────────────────
	// STEP 6: Log info credentials (verbose, nilai di-mask)
	// ─────────────────────────────────────────────────────────────────
	if cfg.Verbose {
		if len(secLoaded) > 0 {
			log.Printf("[sec] loaded dari: %s", strings.Join(secLoaded, ", "))
		} else {
			log.Printf("[sec] tidak ada file .sec ditemukan (gunakan: inframapper -init-sec)")
		}
		if cfg.ShodanAPIKey != "" {
			log.Printf("[sec] shodan: aktif (%s)", sec.MaskVal(cfg.ShodanAPIKey))
		}
		if cfg.FOFAEnable {
			log.Printf("[sec] fofa: aktif (%s)", sec.MaskEmail(cfg.FOFAEmail))
		}
	}

	// ─────────────────────────────────────────────────────────────────
	// STEP 7: Run pipeline
	// ─────────────────────────────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[!] Interrupt diterima, graceful shutdown...")
		cancel()
	}()

	// ─────────────────────────────────────────────────────────────────
	// STEP 8: Jalankan Neural Graph UI jika --expose diisi
	//
	// PENTING: webPusher harus bertipe interface (pivot.WebPusher), bukan
	// *web.Server. Jika kita pass *web.Server(nil) ke interface, Go akan
	// membungkusnya menjadi {type=*web.Server, value=nil} yang TIDAK sama
	// dengan interface nil — sehingga nil-check di webPush() akan selalu
	// true dan menyebabkan nil pointer dereference saat Push() dipanggil.
	// ─────────────────────────────────────────────────────────────────
	var webPusher pivot.WebPusher // nil interface murni secara default
	var webSrv *web.Server        // hanya untuk hold reference (kept alive)
	if *exposePort > 0 {
		webSrv = web.New(*exposePort)
		if err := webSrv.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[web] gagal start: %v\n", err)
			webSrv = nil // Non-fatal — webPusher tetap nil interface
		} else {
			webPusher = webSrv // assign ke interface hanya jika sukses
		}
	}

	// ─────────────────────────────────────────────────────────────────
	// VIEW MODE: load saved JSON and serve it in GUI — skip pipeline
	// ─────────────────────────────────────────────────────────────────
	if *viewFile != "" {
		if webSrv == nil {
			fmt.Fprintf(os.Stderr, "Error: --view membutuhkan --expose <port>\n")
			fmt.Fprintf(os.Stderr, "  Contoh: inframapper --view %s --expose 3221\n", *viewFile)
			os.Exit(1)
		}
		if err := webSrv.LoadOutputJSON(*viewFile); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n[view] Menampilkan hasil scan dari: %s\n", *viewFile)
		fmt.Printf("[web]  Neural Graph UI → http://localhost:%d\n", *exposePort)
		fmt.Println("[web]  Tekan Ctrl+C untuk keluar.")
		<-ctx.Done()
		return
	}

	result, err := pivot.Run(ctx, cfg, webPusher)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Println("[!] Pipeline dihentikan oleh user")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "Pipeline error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n=== Summary ===\n")
	fmt.Printf("Target:          %s\n", result.Target)
	fmt.Printf("Subdomains:      %d\n", result.TotalSubdomains)
	fmt.Printf("Alive assets:    %d\n", len(result.AliveAssets))
	fmt.Printf("Clusters:        %d\n", len(result.Clusters))
	fmt.Printf("SAN new domains: %d\n", result.Stats.L4SANNewDomains)
	fmt.Printf("FOFA new IPs:    %d\n", result.Stats.L3FOFANewIPs)
	fmt.Printf("ASN skipped:     %v\n", result.Stats.L5ASNSkipped)
	fmt.Printf("Duration:        %s\n", result.FinishedAt.Sub(result.StartedAt).Round(1e9))
	fmt.Printf("Output dir:      %s\n", cfg.OutputDir)

	// Jika UI aktif, tahan proses agar user bisa explore graph
	if webSrv != nil {
		fmt.Printf("\n[web] Neural Graph UI aktif di http://localhost:%d\n", *exposePort)
		fmt.Println("[web] Tekan Ctrl+C untuk keluar.")
		<-ctx.Done()
	}
}

// customSecPath melakukan pre-scan args untuk -sec <path> sebelum flag.Parse().
func customSecPath(args []string) string {
	for i, arg := range args {
		if (arg == "-sec" || arg == "--sec") && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "-sec=") {
			return strings.TrimPrefix(arg, "-sec=")
		}
		if strings.HasPrefix(arg, "--sec=") {
			return strings.TrimPrefix(arg, "--sec=")
		}
	}
	return ""
}

// firstNonEmpty mengembalikan string pertama yang tidak kosong.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
