// Package web adalah Layer 8 InfraMapper — Neural Graph Mapper UI.
//
// Aktifkan dengan flag --expose <port>:
//
//	inframapper -target example.com --expose 3221
//
// Server ini:
//   - Serve UI (HTML/JS single-page) di /
//   - Push hasil pipeline via WebSocket di /ws (JSON)
//   - Expose REST endpoint GET /api/graph untuk polling fallback
package web

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/yourusername/inframapper/internal/model"
)

// Server adalah Layer 8 web server.
type Server struct {
	port    int
	mu      sync.RWMutex
	graph   *GraphData
	clients map[chan []byte]struct{}
	srv     *http.Server
}

// New membuat Server baru untuk port yang diberikan.
func New(port int) *Server {
	return &Server{
		port:    port,
		clients: make(map[chan []byte]struct{}),
	}
}

// Start menjalankan HTTP server di background. Non-blocking.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/api/graph", s.handleAPIGraph)
	mux.HandleFunc("/ws", s.handleWS)
	// Serve CSS dan JS secara terpisah (di-load oleh HTML via /static/)
	// Ini memungkinkan edit UI tanpa recompile binary saat development.
	mux.HandleFunc("/static/style.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(staticCSS))
	})
	mux.HandleFunc("/static/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(staticJS))
	})

	s.srv = &http.Server{Addr: fmt.Sprintf(":%d", s.port), Handler: mux}

	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("web server listen :%d: %w", s.port, err)
	}

	go func() {
		log.Printf("[web] Neural Graph UI → http://localhost:%d", s.port)
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[web] server error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()

	return nil
}

// Push mengirim hasil pipeline ke semua client WebSocket yang terhubung.
// Thread-safe — bisa dipanggil dari goroutine manapun.
func (s *Server) Push(result *model.PipelineResult) {
	g := BuildGraph(result)

	s.mu.Lock()
	s.graph = g
	s.mu.Unlock()

	data, err := json.Marshal(g)
	if err != nil {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.clients {
		select {
		case ch <- data:
		default:
			// client lambat — skip, jangan block pipeline
		}
	}
}

// ─── HTTP handlers ──────────────────────────────────────────────────────────

func (s *Server) handleAPIGraph(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if g == nil {
		_, _ = w.Write([]byte(`{"status":"pending"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(g)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgradeWS(w, r)
	if err != nil {
		return
	}
	defer conn.Close()

	ch := make(chan []byte, 4)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
	}()

	// Kirim snapshot saat ini jika sudah ada
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	if g != nil {
		if data, err := json.Marshal(g); err == nil {
			_ = wsWriteText(conn, data)
		}
	}

	// Ping ticker
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := wsWriteText(conn, msg); err != nil {
				return
			}
		case <-ping.C:
			if err := wsWritePing(conn); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(uiHTML))
}

// ─── Graph data model ───────────────────────────────────────────────────────

// GraphData adalah format JSON yang dikonsumsi frontend.
type GraphData struct {
	Target    string      `json:"target"`
	UpdatedAt time.Time   `json:"updated_at"`
	Stats     interface{} `json:"stats"`
	Nodes     []GraphNode `json:"nodes"`
	Edges     []GraphEdge `json:"edges"`
}

// GraphNode adalah satu node di neural graph.
// BUG FIX: versi lama tidak expose field geo (lat/lon/city/country) ke frontend,
// sehingga world map tidak bisa dirender meski data sudah ada di ASNInfo.
type GraphNode struct {
	ID           int      `json:"id"`
	Host         string   `json:"host"`
	IP           string   `json:"ip"`
	Port         int      `json:"port,omitempty"`
	StatusCode   int      `json:"status_code"`
	Server       string   `json:"server,omitempty"`
	Source       string   `json:"source"`
	Cluster      string   `json:"cluster"`
	ClusterLabel string   `json:"cluster_label"`
	Pivot        string   `json:"pivot"`
	Orphan       bool     `json:"orphan"`
	FaviconHash  string   `json:"favicon_hash,omitempty"`
	JARM         string   `json:"jarm,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Score        float64  `json:"score"`
	HTTPS        bool     `json:"https"`
	// Geo fields — diisi dari ASNInfo setelah ipinfo.io lookup
	Lat     float64 `json:"lat,omitempty"`
	Lon     float64 `json:"lon,omitempty"`
	City    string  `json:"city,omitempty"`
	Country string  `json:"country,omitempty"`
	ASN     string  `json:"asn,omitempty"`
}

// GraphEdge adalah satu edge antar node, dengan kekuatan pivot sebagai weight.
type GraphEdge struct {
	Source   int     `json:"source"`
	Target   int     `json:"target"`
	Strength float64 `json:"strength"`
	Pivot    string  `json:"pivot"`
}

// BuildGraph mengkonversi PipelineResult ke format yang dimengerti frontend.
//
// BUG FIX: versi lama tidak mengisi geo coordinates (lat/lon/city/country)
// ke GraphNode meski ASNInfo sudah tersedia di Asset — sehingga world map
// rendering di frontend tidak punya data posisi untuk tiap node.
//
// BUG FIX: field Port, Server, HTTPS tidak di-export ke frontend.
//
// BUG FIX: edge star-topology bisa panic jika members kosong (len check hilang).
func BuildGraph(result *model.PipelineResult) *GraphData {
	if result == nil {
		return &GraphData{UpdatedAt: time.Now()}
	}

	g := &GraphData{
		Target:    result.Target,
		UpdatedAt: time.Now(),
		Stats:     result.Stats,
	}

	hostToID := make(map[string]int, len(result.AliveAssets))

	for i, a := range result.AliveAssets {
		node := GraphNode{
			ID:          i,
			Host:        a.Host,
			IP:          a.IP,
			Port:        a.Port,
			StatusCode:  a.StatusCode,
			Server:      a.Server,
			Source:      string(a.Source),
			Tags:        a.Tags,
			FaviconHash: a.FaviconHash,
			HTTPS:       a.HTTPS,
		}
		if a.FOFAData != nil && a.FOFAData.JARM != "" {
			node.JARM = a.FOFAData.JARM
		} else if a.TLSCert != nil {
			node.JARM = a.TLSCert.JARM
		}
		for _, t := range a.Tags {
			if t == "orphan" {
				node.Orphan = true
			}
		}

		// BUG FIX: populate geo dari ASNInfo
		if a.ASNInfo != nil {
			node.Lat = a.ASNInfo.Lat
			node.Lon = a.ASNInfo.Lon
			node.City = a.ASNInfo.City
			node.Country = a.ASNInfo.Country
			node.ASN = a.ASNInfo.ASN
		} else if a.ShodanData != nil {
			node.Country = a.ShodanData.Country
			node.ASN = a.ShodanData.ASN
		} else if a.FOFAData != nil {
			node.Country = a.FOFAData.Country
			node.ASN = a.FOFAData.ASN
		}

		// Fallback geo: gunakan country-center coords jika lat/lon belum ada
		// (common case: ipinfo.io rate-limited atau ASN sweep di-skip)
		if node.Lat == 0 && node.Lon == 0 && node.Country != "" {
			if lat, lon, ok := countryCenter(node.Country); ok {
				node.Lat = lat
				node.Lon = lon
			}
		}

		g.Nodes = append(g.Nodes, node)
		hostToID[a.Host] = i
	}

	// Assign cluster info dan buat edges dari clusters
	for _, cl := range result.Clusters {
		// BUG FIX: guard against empty cluster (panic di versi lama)
		if len(cl.Assets) == 0 {
			continue
		}
		members := make([]int, 0, len(cl.Assets))
		for _, ca := range cl.Assets {
			if id, ok := hostToID[ca.Host]; ok {
				g.Nodes[id].Cluster = cl.ID
				g.Nodes[id].ClusterLabel = cl.Label
				g.Nodes[id].Pivot = cl.Pivot
				g.Nodes[id].Score = cl.Score
				members = append(members, id)
			}
		}

		// Star topology: semua member connect ke hub (members[0])
		for i := 1; i < len(members); i++ {
			g.Edges = append(g.Edges, GraphEdge{
				Source: members[0], Target: members[i],
				Strength: cl.Score, Pivot: cl.Pivot,
			})
		}
		// Tambah cross-edges untuk look lebih organic
		for i := 1; i+1 < len(members); i += 2 {
			g.Edges = append(g.Edges, GraphEdge{
				Source: members[i], Target: members[i+1],
				Strength: cl.Score * 0.45, Pivot: cl.Pivot,
			})
		}
	}

	return g
}

// ─── View mode: replay saved output JSON ────────────────────────────────────

// savedOutput mirrors the subset of Layer 7 JSON output that the GUI needs.
// Only fields used by BuildGraph are decoded; the rest are ignored.
type savedOutput struct {
	Target      string            `json:"target"`
	Stats       model.LayerStats  `json:"stats"`
	AliveAssets []*model.Asset    `json:"alive_assets"`
	Clusters    []*model.Cluster  `json:"clusters"`
}

// LoadOutputJSON reads a previously saved output JSON file, converts it back
// into a PipelineResult, and pushes it to the GUI exactly as if the pipeline
// had just finished.  Used by the --view flag to replay a scan without re-running.
func (s *Server) LoadOutputJSON(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	var saved savedOutput
	if err := json.Unmarshal(raw, &saved); err != nil {
		return fmt.Errorf("parse %q: %w", path, err)
	}
	if saved.Target == "" {
		return fmt.Errorf("%q: missing \"target\" field — is this a valid InfraMapper output JSON?", path)
	}
	result := &model.PipelineResult{
		Target:      saved.Target,
		AliveAssets: saved.AliveAssets,
		Clusters:    saved.Clusters,
		Stats:       saved.Stats,
	}
	s.Push(result)
	log.Printf("[view] %s — %d assets, %d clusters loaded from %s",
		saved.Target, len(saved.AliveAssets), len(saved.Clusters), path)
	return nil
}

// countryCenter mengembalikan koordinat tengah negara berdasarkan ISO 2-letter code.
// Digunakan sebagai fallback geo ketika ipinfo.io tidak memberikan loc yang spesifik.
// Hanya mencakup negara yang paling sering muncul dalam OSINT recon.
func countryCenter(cc string) (lat, lon float64, ok bool) {
	centers := map[string][2]float64{
		"US": {37.09, -95.71}, "CN": {35.86, 104.19}, "RU": {61.52, 105.31},
		"DE": {51.16, 10.45},  "GB": {55.37, -3.43},  "FR": {46.22, 2.21},
		"NL": {52.13, 5.29},   "JP": {36.20, 138.25}, "SG": {1.35, 103.81},
		"AU": {-25.27, 133.77},"IN": {20.59, 78.96},  "BR": {-14.23, -51.92},
		"CA": {56.13, -106.34},"KR": {35.90, 127.76}, "ID": {-0.78, 113.92},
		"MY": {4.21, 101.97},  "HK": {22.39, 114.10}, "UA": {48.37, 31.16},
		"PL": {51.91, 19.14},  "SE": {60.12, 18.64},  "BG": {42.73, 25.48},
		"MX": {23.63, -102.55},"ZA": {-30.55, 22.93}, "TR": {38.96, 35.24},
		"IT": {41.87, 12.56},  "ES": {40.46, -3.74},  "NZ": {-40.90, 174.88},
		"AR": {-38.41, -63.61},"CL": {-35.67, -71.54},"TH": {15.87, 100.99},
		"VN": {14.05, 108.27}, "PH": {12.87, 121.77}, "PK": {30.37, 69.34},
	}
	if v, found := centers[cc]; found {
		return v[0], v[1], true
	}
	return 0, 0, false
}

// ─── Minimal WebSocket (RFC 6455, text frames only) ─────────────────────────

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func upgradeWS(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "not a websocket upgrade", 400)
		return nil, fmt.Errorf("not websocket")
	}
	key := r.Header.Get("Sec-Websocket-Key")
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("hijacking not supported")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := buf.WriteString(resp); err != nil {
		return nil, err
	}
	return conn, buf.Flush()
}

func wsWriteText(conn net.Conn, data []byte) error {
	n := len(data)
	header := []byte{0x81}
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n < 65536:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write(append(header, data...))
	return err
}

func wsWritePing(conn net.Conn) error {
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write([]byte{0x89, 0x00})
	return err
}
