package util

import (
	"regexp"
	"strings"
)

var titleRe = regexp.MustCompile(`(?i)<title[^>]*>(.*?)</title>`)

// ExtractTitle mengambil konten tag <title> dari HTML body.
func ExtractTitle(body string) string {
	matches := titleRe.FindStringSubmatch(body)
	if len(matches) < 2 {
		return ""
	}
	title := strings.TrimSpace(matches[1])
	// Strip HTML entities sederhana
	title = strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
	).Replace(title)
	if len(title) > 120 {
		title = title[:117] + "..."
	}
	return title
}

// IsSubdomainOf mengecek apakah host adalah subdomain dari parent.
// "api.example.com" → IsSubdomainOf("example.com") = true
// "evil.com" → IsSubdomainOf("example.com") = false
func IsSubdomainOf(host, parent string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	parent = strings.ToLower(strings.TrimSpace(parent))

	if host == parent {
		return true
	}
	return strings.HasSuffix(host, "."+parent)
}

// UniqueStrings mengembalikan slice string tanpa duplikat, mempertahankan urutan.
func UniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ChunkSlice memecah slice menjadi potongan berukuran n.
// Berguna untuk batching API call.
func ChunkSlice[T any](slice []T, size int) [][]T {
	var chunks [][]T
	for size < len(slice) {
		slice, chunks = slice[size:], append(chunks, slice[0:size:size])
	}
	return append(chunks, slice)
}
