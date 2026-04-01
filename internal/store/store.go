// Package store menangani persistensi lintas-scan untuk sistem attribution.
//
// Data disimpan sebagai JSON flat-file (zero external dependencies).
// Setiap kali scan selesai, sinyal cluster-level (favicon_hash, header_hash,
// jarm, asn, tls_issuer) di-extract dan disimpan per target.
//
// ComputeCorrelations kemudian mencari sinyal yang sama di antara target
// yang berbeda dan menghitung confidence score untuk setiap korelasi.
package store

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/yourusername/inframapper/internal/model"
)

// signalWeights adalah bobot kepercayaan per tipe sinyal.
// Nilai lebih tinggi = lebih unik / lebih sulit dipalsukan.
var signalWeights = map[string]float64{
	"favicon_hash": 0.55,
	"header_hash":  0.50,
	"jarm":         0.45,
	"asn":          0.30,
	"tls_issuer":   0.20,
}

// ─── Data types ──────────────────────────────────────────────────────────────

// Signal adalah satu fingerprint cluster-level dari sebuah scan.
type Signal struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	AssetCount int    `json:"asset_count"`
}

// ScanRecord menyimpan metadata dan sinyal dari satu scan target.
type ScanRecord struct {
	ID           string    `json:"id"`
	Target       string    `json:"target"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	AssetCount   int       `json:"asset_count"`
	ClusterCount int       `json:"cluster_count"`
	Signals      []Signal  `json:"signals"`
}

// AttrNode adalah node dalam attribution graph — merepresentasikan satu target scan.
type AttrNode struct {
	Target     string    `json:"target"`
	AssetCount int       `json:"asset_count"`
	ScanCount  int       `json:"scan_count"`
	LastSeen   time.Time `json:"last_seen"`
	TopSignals []string  `json:"top_signals"`
}

// AttrEdge adalah edge dalam attribution graph — sinyal yang sama antara dua target.
type AttrEdge struct {
	TargetA     string  `json:"target_a"`
	TargetB     string  `json:"target_b"`
	SignalType  string  `json:"signal_type"`
	SignalValue string  `json:"signal_value"`
	Confidence  float64 `json:"confidence"`
}

// AttributionGraph adalah output akhir korelasi lintas-target.
type AttributionGraph struct {
	UpdatedAt    time.Time  `json:"updated_at"`
	Nodes        []AttrNode `json:"nodes"`
	Edges        []AttrEdge `json:"edges"`
	TotalScans   int        `json:"total_scans"`
	TotalTargets int        `json:"total_targets"`
}

// TargetSummary adalah ringkasan satu target yang tersimpan.
type TargetSummary struct {
	Target       string    `json:"target"`
	AssetCount   int       `json:"asset_count"`
	ClusterCount int       `json:"cluster_count"`
	SignalCount  int       `json:"signal_count"`
	FinishedAt   time.Time `json:"finished_at"`
}

// dbFile adalah struktur yang ditulis ke disk.
type dbFile struct {
	Version int          `json:"version"`
	Scans   []ScanRecord `json:"scans"`
}

// ─── Store ───────────────────────────────────────────────────────────────────

// Store menangani persistensi sinyal attribution.
type Store struct {
	mu   sync.RWMutex
	path string
	data dbFile
}

const dbVersion = 1

// Open membuka atau membuat store di path yang diberikan.
// Tidak error jika file belum ada — akan dibuat saat pertama SaveScan.
func Open(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: dbFile{Version: dbVersion},
	}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil // file baru, mulai kosong
	}
	if err != nil {
		return nil, fmt.Errorf("store open %q: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("store parse %q: %w", path, err)
	}
	return s, nil
}

// SaveScan mengekstrak sinyal dari pipeline result dan menyimpannya.
// Hanya menyimpan satu record per target — scan terbaru menggantikan yang lama.
func (s *Store) SaveScan(result *model.PipelineResult) error {
	if result == nil || result.Target == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	signals := extractSignals(result)

	scanID := fmt.Sprintf("%s_%d", result.Target, result.StartedAt.UnixNano())

	// Hapus record lama untuk target yang sama
	filtered := s.data.Scans[:0]
	for _, sc := range s.data.Scans {
		if sc.Target != result.Target {
			filtered = append(filtered, sc)
		}
	}

	rec := ScanRecord{
		ID:           scanID,
		Target:       result.Target,
		StartedAt:    result.StartedAt,
		FinishedAt:   result.FinishedAt,
		AssetCount:   len(result.AliveAssets),
		ClusterCount: len(result.Clusters),
		Signals:      signals,
	}
	s.data.Scans = append(filtered, rec)

	return s.flush()
}

// GetAttributionGraph membangun attribution graph dari semua scan yang tersimpan.
// Hanya korelasi dengan confidence >= minConfidence yang dimasukkan ke edges.
func (s *Store) GetAttributionGraph(minConfidence float64) *AttributionGraph {
	s.mu.RLock()
	defer s.mu.RUnlock()

	g := &AttributionGraph{
		UpdatedAt:  time.Now(),
		TotalScans: len(s.data.Scans),
	}

	if len(s.data.Scans) < 2 {
		// Butuh minimal 2 target untuk ada korelasi
		for _, sc := range s.data.Scans {
			g.Nodes = append(g.Nodes, scanToNode(sc))
		}
		g.TotalTargets = len(g.Nodes)
		return g
	}

	// Build node map
	nodeMap := make(map[string]*AttrNode, len(s.data.Scans))
	for _, sc := range s.data.Scans {
		n := scanToNode(sc)
		nodeMap[sc.Target] = &n
	}

	// Find shared signals between all target pairs
	type edgeKey struct{ a, b, sigType, sigVal string }
	edgeMap := make(map[edgeKey]*AttrEdge)

	for i := 0; i < len(s.data.Scans); i++ {
		for j := i + 1; j < len(s.data.Scans); j++ {
			sa, sb := s.data.Scans[i], s.data.Scans[j]

			// Index signals of scan A
			sigA := make(map[string]map[string]int) // type → value → count
			for _, sig := range sa.Signals {
				if sigA[sig.Type] == nil {
					sigA[sig.Type] = make(map[string]int)
				}
				sigA[sig.Type][sig.Value] = sig.AssetCount
			}

			// Check each signal of scan B against scan A
			for _, sigB := range sb.Signals {
				cntA, shared := sigA[sigB.Type][sigB.Value]
				if !shared {
					continue
				}

				w := signalWeights[sigB.Type]
				minCnt := cntA
				if sigB.AssetCount < cntA {
					minCnt = sigB.AssetCount
				}
				conf := w * math.Min(1.0, float64(minCnt)/3.0)
				if conf < minConfidence {
					continue
				}

				// Canonical ordering: alphabetically smaller target = A
				ta, tb := sa.Target, sb.Target
				if ta > tb {
					ta, tb = tb, ta
				}

				k := edgeKey{ta, tb, sigB.Type, sigB.Value}
				if ex, ok := edgeMap[k]; ok {
					if conf > ex.Confidence {
						ex.Confidence = conf
					}
				} else {
					edgeMap[k] = &AttrEdge{
						TargetA:     ta,
						TargetB:     tb,
						SignalType:  sigB.Type,
						SignalValue: sigB.Value,
						Confidence:  conf,
					}
				}
			}
		}
	}

	// Only include nodes that appear in at least one edge (or all if no edges)
	connectedTargets := make(map[string]bool)
	for _, e := range edgeMap {
		connectedTargets[e.TargetA] = true
		connectedTargets[e.TargetB] = true
	}

	for target, node := range nodeMap {
		if len(edgeMap) == 0 || connectedTargets[target] {
			g.Nodes = append(g.Nodes, *node)
		}
	}

	// Sort nodes by target name for stable output
	sort.Slice(g.Nodes, func(i, j int) bool {
		return g.Nodes[i].Target < g.Nodes[j].Target
	})

	for _, e := range edgeMap {
		g.Edges = append(g.Edges, *e)
	}

	// Sort edges by confidence descending
	sort.Slice(g.Edges, func(i, j int) bool {
		return g.Edges[i].Confidence > g.Edges[j].Confidence
	})

	g.TotalTargets = len(g.Nodes)
	return g
}

// GetTargets mengembalikan ringkasan semua target yang tersimpan.
func (s *Store) GetTargets() []TargetSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]TargetSummary, 0, len(s.data.Scans))
	for _, sc := range s.data.Scans {
		result = append(result, TargetSummary{
			Target:       sc.Target,
			AssetCount:   sc.AssetCount,
			ClusterCount: sc.ClusterCount,
			SignalCount:  len(sc.Signals),
			FinishedAt:   sc.FinishedAt,
		})
	}
	return result
}

// Close adalah no-op — data di-flush pada setiap SaveScan.
func (s *Store) Close() {}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func scanToNode(sc ScanRecord) AttrNode {
	sigTypes := make(map[string]bool)
	for _, sig := range sc.Signals {
		sigTypes[sig.Type] = true
	}
	var tops []string
	// Ordered by weight (highest first)
	for _, t := range []string{"favicon_hash", "header_hash", "jarm", "asn", "tls_issuer"} {
		if sigTypes[t] {
			tops = append(tops, t)
		}
	}
	return AttrNode{
		Target:     sc.Target,
		AssetCount: sc.AssetCount,
		ScanCount:  1,
		LastSeen:   sc.FinishedAt,
		TopSignals: tops,
	}
}

// extractSignals mengekstrak sinyal cluster-level dari PipelineResult.
// Sinyal yang terlalu umum (ASN0, JARM "0"*60) di-skip.
func extractSignals(result *model.PipelineResult) []Signal {
	counts := make(map[string]map[string]int) // type → value → count

	for _, a := range result.AliveAssets {
		// favicon hash
		if a.FaviconHash != "" && a.FaviconHash != "0" {
			incSig(counts, "favicon_hash", a.FaviconHash)
		}

		// FOFA signals
		if a.FOFAData != nil {
			if a.FOFAData.HeaderHash != "" {
				incSig(counts, "header_hash", a.FOFAData.HeaderHash)
			}
			if a.FOFAData.JARM != "" && !isBlankJARM(a.FOFAData.JARM) {
				incSig(counts, "jarm", a.FOFAData.JARM)
			}
		}

		// TLS JARM (from TLSInfo if FOFA didn't set it)
		if a.TLSCert != nil {
			if a.TLSCert.JARM != "" && !isBlankJARM(a.TLSCert.JARM) {
				incSig(counts, "jarm", a.TLSCert.JARM)
			}
			if a.TLSCert.Issuer != "" {
				incSig(counts, "tls_issuer", a.TLSCert.Issuer)
			}
		}

		// ASN — from ASNInfo, ShodanData, or FOFAData (in that priority)
		asn := ""
		if a.ASNInfo != nil && a.ASNInfo.ASN != "" {
			asn = a.ASNInfo.ASN
		} else if a.ShodanData != nil && a.ShodanData.ASN != "" {
			asn = a.ShodanData.ASN
		} else if a.FOFAData != nil && a.FOFAData.ASN != "" {
			asn = a.FOFAData.ASN
		}
		if asn != "" && asn != "AS0" && asn != "0" {
			incSig(counts, "asn", asn)
		}
	}

	var signals []Signal
	for sigType, vals := range counts {
		for val, cnt := range vals {
			signals = append(signals, Signal{
				Type:       sigType,
				Value:      val,
				AssetCount: cnt,
			})
		}
	}

	// Sort for deterministic output
	sort.Slice(signals, func(i, j int) bool {
		if signals[i].Type != signals[j].Type {
			return signals[i].Type < signals[j].Type
		}
		return signals[i].Value < signals[j].Value
	})

	return signals
}

func incSig(counts map[string]map[string]int, sigType, val string) {
	if counts[sigType] == nil {
		counts[sigType] = make(map[string]int)
	}
	counts[sigType][val]++
}

// isBlankJARM returns true for all-zero JARM fingerprints (unresolvable host).
func isBlankJARM(jarm string) bool {
	if len(jarm) < 10 {
		return true
	}
	for _, c := range jarm {
		if c != '0' {
			return false
		}
	}
	return true
}

// flush menulis state saat ini ke disk secara atomic (write temp → rename).
func (s *Store) flush() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("store marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return fmt.Errorf("store write %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// Fallback: write directly jika rename gagal (e.g. cross-device)
		return os.WriteFile(s.path, raw, 0600)
	}
	return nil
}
