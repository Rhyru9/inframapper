# InfraMapper

Passive infrastructure discovery and clustering tool.

InfraMapper maps the attack surface of a target domain through passive reconnaissance,
certificate analysis, and behavioral fingerprinting. It does not perform active scanning,
exploitation, or vulnerability assessment. Its sole responsibility is finding and grouping
assets — everything else is out of scope by design.

---

## Table of Contents

- [Architecture](#architecture)
- [Pipeline](#pipeline)
- [Build](#build)
- [External Dependencies](#external-dependencies)
- [Credentials](#credentials)
- [Usage](#usage)
- [Neural Graph UI](#neural-graph-ui)
- [Output](#output)
- [Configuration Reference](#configuration-reference)
- [Design Decisions](#design-decisions)
- [Out of Scope](#out-of-scope)

---

## Architecture

```
Target: domain.com
       |
       v
+------------------------------+
|  Layer 1 — Seed Discovery    |  subfinder | amass | assetfinder | crt.sh
|  parallel fan-out            |  -> deduplicated subdomains with source attribution
+------------------------------+
       |
       v
+------------------------------+
|  Layer 2 — HTTP Enrichment   |  goroutine pool, configurable concurrency
|                              |  -> alive hosts: status, title, IP, server, port
+------------------------------+
       |
       v  (geo enrichment pass — lat/lon/city/country, non-blocking)
       |
       v
+------------------------------------------------------+
|  Layer 3 — Pivot: Favicon + Shodan + FOFA (parallel) |
|                                                      |
|  Shodan:  icon_hash -> new IPs, org, ASN             |
|  FOFA:    icon_hash    (confirm)                     |
|           header_hash  (FOFA-exclusive)              |
|           cert.domain  -> seeded into Layer 4        |
|           jarm, icp, product                         |
|                                                      |
|  Output: new IPs + enrichment metadata + cert domains|
+------------------------------------------------------+
       |
       v
+------------------------------+
|  Layer 4 — TLS SAN Pivot     |  parse SAN from certificates
|  loop, max 2 iterations      |  -> domains not resolvable via public DNS
+------------------------------+
       |
       v
+------------------------------+
|  Layer 5 — ASN Sweep         |  CONDITIONAL: only runs if new IPs found
|                              |  -> responsive IPs within the same CIDRs
+------------------------------+
       |
       |  <- SCOPE BOUNDARY: no outbound network calls after this line
       v
+------------------------------+
|  Layer 6 — Clustering        |  signals: favicon | header | jarm | asn | tls
|  + Orphan Detection          |  -> sorted clusters + orphan IP annotation
+------------------------------+
       |
       v
+------------------------------+
|  Layer 7 — Export            |  JSON | CSV | Markdown
+------------------------------+
```

---

## Pipeline

### Layer 1 — Seed Discovery

Enumerates subdomains from passive sources in parallel. Sources are configurable and
independently enabled or disabled. Results are deduplicated before passing downstream.

| Source       | Flag          | Default | Notes                          |
|--------------|---------------|---------|--------------------------------|
| subfinder    | `-subfinder`  | on      | Requires binary in PATH        |
| assetfinder  | `-assetfinder`| on      | Requires binary in PATH        |
| crt.sh       | `-crtsh`      | on      | HTTP API, no binary needed     |
| amass        | `-amass`      | off     | Slow; disabled by default      |

### Layer 2 — HTTP Enrichment

Probes each subdomain with a configurable goroutine pool. Extracts HTTP status code,
response title, server header, resolved IP, port, and HTTPS flag. Dead hosts are
separated and not passed to further layers.

Default concurrency: 50 workers. Default timeout: 10 seconds per request.

### Geo Enrichment Pass

Fills latitude, longitude, city, and country for all alive assets. Runs after Layer 2,
before Layer 3. Results are cached per IP to avoid duplicate API calls.

- Primary source: ipinfo.io (requires token, `-ipinfo-token`)
- Fallback: ip-api.com (free, 45 requests/minute limit)

### Layer 3 — Favicon, Shodan, FOFA

Three parallel operations on the alive asset set.

**Favicon extraction**: Downloads each asset's favicon and computes a MurmurHash3
fingerprint (Shodan-compatible format). This hash is the primary clustering signal
and the query key for external pivots.

**Shodan pivot** (requires `-shodan-key`): Queries `http.favicon.hash` for each unique
favicon hash. Returns new IPs, ports, ASN, organization, and CVE tags.

**FOFA pivot** (requires `-fofa-email` and `-fofa-key`): Runs three query types per asset,
rate-limited to 2 requests/second.

| FOFA query    | Purpose                                      | Exclusive to FOFA |
|---------------|----------------------------------------------|-------------------|
| `icon_hash`   | Cross-confirms Shodan favicon findings        | No                |
| `header_hash` | HTTP header fingerprint — identifies same infra behind CDN | Yes |
| `cert.domain` | Domains extracted from certificates — seeds Layer 4 | Yes         |

FOFA fields extracted: IP, port, ASN, organization, title, server, JARM, country, ICP.

### Layer 4 — TLS SAN Pivot

Connects to each alive asset and extracts Subject Alternative Names from its TLS
certificate. Filters results to subdomains of the target domain, deduplicates against
known assets, and re-probes new entries through the Layer 2 HTTP enrichment path.

Pre-seeds from FOFA `cert.domain` results before iterating.
Maximum iterations: configurable via `-san-iter` (default 2). Stops early if no
new domains are found.

### Layer 5 — ASN Sweep (Conditional)

Only executes if Layers 3 and 4 produced at least one IP address that was not in the
original DNS resolution set. If the threshold is not met, the layer is skipped entirely
and `L5ASNSkipped: true` is recorded in stats.

When active: resolves the ASN and CIDR for each new IP, then probes all hosts within
CIDRs up to the configured maximum size (`-asn-max-cidr`, default 256 hosts).

### Layer 6 — Clustering and Orphan Detection

Pure data processing — no network calls.

Groups assets by shared infrastructure signals. Each cluster is scored and sorted by
confidence. Orphan detection marks any asset whose IP was not present in the original
DNS seed set.

| Signal        | Score | Notes                                                    |
|---------------|-------|----------------------------------------------------------|
| favicon_hash  | 0.55  | Strongest signal; identical favicon implies shared origin |
| header_hash   | 0.50  | FOFA-exclusive; two identical header hashes imply identical server config |
| jarm          | 0.45  | TLS behavioral fingerprint; identifies same TLS stack regardless of certificate |
| asn           | 0.30  | Medium confidence; same AS does not guarantee same operator |
| tls_issuer    | 0.20  | Weakest; shared issuer (e.g., Let's Encrypt) is not a strong signal |

**JARM** is a TLS handshake fingerprint based on cipher suite ordering and extension
behavior, not certificate content. Assets with identical JARM but different certificates
share the same underlying TLS stack.

**header_hash** is the most operationally useful FOFA-exclusive signal. Two servers with
identical HTTP response header hashes are almost certainly the same infrastructure behind
a load balancer or identically configured CDN.

### Layer 7 — Export

Writes results to disk in one or more formats.

| Format   | Flag value   | Content                                      |
|----------|--------------|----------------------------------------------|
| JSON     | `json`       | Full structured output for downstream tooling |
| CSV      | `csv`        | Flat tabular format for spreadsheet analysis  |
| Markdown | `markdown`   | Human-readable summary report                |

Filename pattern: `<target>_<YYYYMMDD-HHMMSS>.<ext>`

---

## Build

```bash
# Single static binary, no runtime dependencies
go build -o inframapper ./cmd/inframapper

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o inframapper-linux ./cmd/inframapper

# Cross-compile for Windows
GOOS=windows GOARCH=amd64 go build -o inframapper.exe ./cmd/inframapper
```

Requires Go 1.22.2 or later.

---

## External Dependencies

The following external binaries are optional. InfraMapper detects them at runtime via
PATH and silently skips sources that are unavailable.

```bash
# subfinder (recommended)
go install -v github.com/projectdiscovery/subfinder/v2/cmd/subfinder@latest

# assetfinder
go install github.com/tomnomnom/assetfinder@latest

# amass (optional, slower)
go install -v github.com/owasp-amass/amass/v4/...@master
```

crt.sh requires no binary — it is queried via its HTTP API.

---

## Credentials

API keys are never required. Each source gracefully degrades when credentials are absent.
Keys can be supplied via a `.sec` file, environment variables, or CLI flags.

### Credential loading order

Each level overrides the one below it.

```
~/.sec  ->  ./.sec  ->  environment variables  ->  CLI flags
```

### Creating a .sec template

```bash
./inframapper -init-sec
vim .sec
chmod 600 .sec
echo '.sec' >> .gitignore
```

### .sec file format

```
# Lines beginning with # are ignored

shodan        = "your_shodan_key"
fofa_email    = "you@example.com"
fofa_key      = "your_fofa_key"
censys_id     = "your_censys_api_id"
censys_secret = "your_censys_api_secret"
ipinfo_token  = "your_ipinfo_token"
```

The file parser accepts common key variations (`shodan`, `shodan_key`, `shodan_api_key`,
etc.). The file must be readable only by the owner (`chmod 600`); a warning is emitted
if group or world read bits are set.

### Environment variables

```
SHODAN_API_KEY
FOFA_EMAIL
FOFA_KEY
IPINFO_TOKEN
```

---

## Usage

```bash
# Minimal run — no API keys required
./inframapper -target example.com

# With Shodan
./inframapper -target example.com -shodan-key $SHODAN_API_KEY

# With all pivots enabled, verbose output
./inframapper -target example.com -v

# Disable SAN loop and ASN sweep (fastest, no external APIs)
./inframapper -target example.com -san-reseed=false -asn-sweep=false

# All output formats
./inframapper -target example.com -format json,csv,markdown

# High concurrency (watch for rate limits)
./inframapper -target example.com -concurrent 100 -timeout 15

# Load credentials from a custom path
./inframapper -target example.com -sec /home/user/secrets/recon.sec

# Neural Graph UI — live visualization during scan
./inframapper -target example.com --expose 3221

# View a previous scan result in the UI without re-scanning
./inframapper --view output/example.com_20260401-050959.json --expose 3221
```

---

## Neural Graph UI

Activated with `--expose <port>`. Opens a browser-based visualization of the asset graph.

Endpoints:

| Path             | Description                                      |
|------------------|--------------------------------------------------|
| `GET /`          | Single-page application shell                    |
| `GET /static/*`  | CSS and JavaScript assets                        |
| `GET /api/graph` | REST polling endpoint (JSON graph snapshot)      |
| `WS /ws`         | WebSocket — real-time push from the pipeline     |

### Modes

**MAP** — World map (CartoDB Dark Matter tiles) with assets plotted at their
geolocated coordinates. Leaflet markers are colored by pivot signal.

**2D** — Force-directed graph on a dark canvas. Nodes are repelled from each other
and attracted along edges. Drag to reposition nodes.

**3D** — Rotating globe. Assets with geolocation are placed at their geographic
coordinates on a sphere. Co-located nodes (same datacenter) are spread using a
Fibonacci/golden-angle distribution to avoid overlap.

### Interacting with nodes

Click any node to open the detail card. The card shows:

- Hostname and IP
- HTTP status, server header, port, protocol
- Pivot signal, cluster label, JARM fingerprint
- ASN, country, city, coordinates
- Badges: seed root, https, orphan, pivot type
- Relations: every directly connected node, sorted by edge strength. Click a relation
  to navigate to that node.

Click empty canvas or the close button to dismiss the card.

### View mode

Replay a saved scan without re-running discovery:

```bash
./inframapper --view output/example.com_20260401-050959.json --expose 3221
```

The pipeline is not executed. The saved JSON is loaded and served to the UI.

---

## Output

```
output/
├── example.com_20260401-050959.json
├── example.com_20260401-050959.csv
└── example.com_20260401-050959.md
```

### JSON structure

```json
{
  "target": "example.com",
  "alive_assets": [
    {
      "host": "api.example.com",
      "ip": "1.2.3.4",
      "port": 443,
      "status_code": 200,
      "server": "nginx",
      "https": true,
      "source": "subfinder",
      "favicon_hash": "-123456789",
      "fofa_data": {
        "header_hash": "abcd1234",
        "jarm": "27d3ed3d...",
        "icp": "",
        "product": "nginx"
      },
      "tls_cert": {
        "jarm": "27d3ed3d...",
        "sans": ["api.example.com", "staging.example.com"],
        "issuer": "Let's Encrypt"
      },
      "asn": "AS13335",
      "country": "US",
      "city": "San Francisco",
      "lat": 37.77,
      "lon": -122.41,
      "cluster": "fav-1",
      "cluster_label": "AS13335 Cloudflare",
      "orphan": false
    }
  ],
  "clusters": [
    {
      "id": "fav-1",
      "pivot": "favicon_hash",
      "score": 0.88,
      "label": "favicon-a3f2b1",
      "assets": ["api.example.com", "www.example.com"]
    }
  ],
  "stats": {
    "l1_seed_count": 342,
    "l2_alive_count": 87,
    "l3_favicon_count": 85,
    "l3_shodan_hits": 5,
    "l3_fofa_hits": 12,
    "l3_fofa_new_ips": 3,
    "l3_fofa_cert_doms": 8,
    "l4_san_new_domains": 20,
    "l4_reseeds": 2,
    "l5_asn_skipped": false,
    "l5_cidr_count": 2,
    "l5_new_ips": 4,
    "l6_cluster_count": 7
  },
  "started_at": "2026-04-01T05:09:59Z",
  "finished_at": "2026-04-01T05:14:22Z"
}
```

---

## Configuration Reference

### Flags

| Flag              | Type   | Default        | Description                                              |
|-------------------|--------|----------------|----------------------------------------------------------|
| `-target` / `-t`  | string | (required)     | Target domain. Strips scheme and trailing slash.         |
| `-subfinder`      | bool   | true           | Enable subfinder source                                  |
| `-amass`          | bool   | false          | Enable amass passive (slow)                              |
| `-assetfinder`    | bool   | true           | Enable assetfinder source                                |
| `-crtsh`          | bool   | true           | Enable crt.sh certificate transparency                   |
| `-concurrent`     | int    | 50             | HTTP probe goroutine pool size                           |
| `-timeout`        | int    | 10             | HTTP request timeout in seconds                          |
| `-shodan-key`     | string | (from .sec)    | Shodan API key                                           |
| `-fofa-email`     | string | (from .sec)    | FOFA account email                                       |
| `-fofa-key`       | string | (from .sec)    | FOFA API key                                             |
| `-fofa`           | bool   | auto           | Enable FOFA pivot (auto-enabled if both credentials set) |
| `-ipinfo-token`   | string | (from .sec)    | ipinfo.io token for geolocation                          |
| `-san-reseed`     | bool   | true           | Enable TLS SAN re-seed loop                              |
| `-san-iter`       | int    | 2              | Maximum SAN re-seed iterations                           |
| `-asn-sweep`      | bool   | true           | Enable ASN/CIDR sweep                                    |
| `-asn-max-cidr`   | int    | 256            | Maximum hosts per CIDR to sweep                          |
| `-sec`            | string | (auto)         | Custom path to .sec credentials file                     |
| `-output`         | string | `./output`     | Output directory                                         |
| `-format`         | string | `json,markdown`| Output formats, comma-separated                          |
| `-expose`         | int    | 0              | Enable Neural Graph UI on this port                      |
| `-view`           | string | (none)         | Load saved JSON result into UI without re-scanning       |
| `-v`              | bool   | false          | Verbose logging                                          |
| `-debug`          | bool   | false          | Debug logging (more detail than -v)                      |
| `-version`        | bool   | false          | Print version and exit                                   |

### Subcommands

```bash
./inframapper -init-sec [path]    # Generate .sec template (default: ./.sec)
./inframapper -version            # Print version
```

---

## Design Decisions

**SAN re-seed loop**

TLS SANs frequently contain hostnames that do not appear in public DNS — internal
aliases, staging domains, and legacy names. Feeding these back into Layer 1 often
yields a second wave of assets. The hard limit of 2 iterations prevents infinite loops
on misconfigured or adversarially crafted certificates.

**Layer 5 conditional execution**

The ASN sweep only runs when Layers 3 and 4 produce at least one IP that was not
resolvable from the seed set. This prevents the tool from performing expensive broad
sweeps on targets where no new infrastructure was discovered through pivots. The
condition is explicit and logged.

**Scope boundary**

After Layer 5, no outbound network connections are made. Layers 6 and 7 are pure
data processing. This boundary allows the tool to be audited for network behavior
and ensures it cannot silently become an active scanner.

**FOFA and Shodan are complementary, not redundant**

| Signal          | Shodan | FOFA |
|-----------------|--------|------|
| favicon hash    | yes    | yes  |
| header hash     | no     | yes (exclusive) |
| JARM            | no     | yes (exclusive) |
| cert.domain     | partial| yes  |
| ICP filing      | no     | yes  |
| product field   | partial| yes  |

**Credential priority chain**

CLI flags take final precedence. This allows a team to maintain a shared `.sec` file
in `~/.sec` while overriding individual keys per engagement without modifying the file.

---

## Out of Scope

InfraMapper will not perform and is not designed to perform:

- Port scanning beyond the HTTP/HTTPS probe in Layer 2
- Vulnerability scanning or CVE exploitation
- Active fingerprinting that requires sending unexpected payloads
- Web UI beyond the Neural Graph visualization
- Supply chain analysis
- Authentication testing

These functions belong to dedicated tools. InfraMapper produces structured asset data
that serves as input to those tools.

---

## Why Go

- **Concurrency**: The Layer 2 probe and Layer 3 pivot queries run hundreds of
  goroutines in parallel. Go's scheduler handles this with a flat memory profile.
- **Single binary**: `go build` produces one self-contained executable. No interpreter,
  no dependency manager, no version conflicts.
- **Ecosystem alignment**: The reconnaissance tooling community (subfinder, httpx, nuclei,
  amass) is predominantly Go. A Go binary integrates directly into existing pipelines
  and CI workflows without adaptation.
