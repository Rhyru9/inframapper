package web

// uiHTML adalah HTML shell untuk InfraMapper Neural World Map.
//
// Arsitektur file terpisah:
//   - HTML: struktur DOM minimal (file ini)
//   - CSS:  static/style.css  (served oleh server.go /static/)
//   - JS:   static/app.js     (berisi semua logic UI, render, cache, WS)
//   - Map:  Leaflet.js CDN    (real OSM world map tiles)
//
// BUG FIX vs versi lama:
//   - Tidak ada backtick JS di dalam Go raw string (syntax error)
//   - Tidak ada karakter dollar di dalam Go raw string (invalid char)
//   - CSS/JS dipisah → mudah edit tanpa recompile
//   - Leaflet menggantikan polygon tangan → real world map
//   - Cache (sessionStorage 30min) → skip rescan jika data ada
const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>InfraMapper</title>
<link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/leaflet/1.9.4/leaflet.min.css">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;600&display=swap">
<link rel="stylesheet" href="/static/style.css">
</head>
<body>

<div id="hud">
  <span class="logo">InfraMapper</span>
  <span class="tgt" id="htgt">connecting...</span>
  <div class="hud-sep"></div>
  <div class="stat"><div class="sv" id="sn">0</div><div class="sk">assets</div></div>
  <div class="stat"><div class="sv" id="se">0</div><div class="sk">edges</div></div>
  <div class="stat"><div class="sv" id="sc">0</div><div class="sk">clusters</div></div>
  <div class="stat"><div class="sv" id="so">0</div><div class="sk">orphans</div></div>
  <div class="stat"><div class="sv" id="sg">0</div><div class="sk">geo</div></div>
  <div class="hud-space"></div>
  <div class="wsdot" id="wsdot"></div>
  <div id="mode-btns">
    <button class="mbtn" id="bmap" onclick="setMode('map')">MAP</button>
    <button class="mbtn" id="b2d"  onclick="setMode('2d')">2D</button>
    <button class="mbtn" id="b3d"  onclick="setMode('3d')">3D</button>
  </div>
</div>

<div id="stage">
  <div id="leaflet-map"></div>
  <canvas id="overlay-canvas"></canvas>
  <canvas id="main-canvas" class="grid-bg"></canvas>

  <div id="loader">
    <div class="loader-logo">INFRAMAPPER</div>
    <div class="loader-bar"><div class="loader-fill"></div></div>
    <div class="loader-txt" id="loader-txt">loading map tiles...</div>
  </div>

  <div id="search-wrap">
    <input id="search-input" type="text" placeholder="host / ip / country / city..." autocomplete="off">
    <div id="search-results"></div>
  </div>

  <div id="filters">
    <button class="fpill on" data-piv="all"          style="color:#bfcfe3;border-color:#2e4a68" onclick="toggleFilter('all')">all</button>
    <button class="fpill on" data-piv="favicon_hash" style="color:#e89c32;border-color:#e89c3266" onclick="toggleFilter('favicon_hash')">favicon</button>
    <button class="fpill on" data-piv="header_hash"  style="color:#32a0e8;border-color:#32a0e866" onclick="toggleFilter('header_hash')">header</button>
    <button class="fpill on" data-piv="jarm"         style="color:#8c32e8;border-color:#8c32e866" onclick="toggleFilter('jarm')">jarm</button>
    <button class="fpill on" data-piv="asn"          style="color:#32e39a;border-color:#32e39a66" onclick="toggleFilter('asn')">asn</button>
    <button class="fpill on" data-piv="tls_issuer"   style="color:#e83272;border-color:#e8327266" onclick="toggleFilter('tls_issuer')">tls</button>
  </div>

  <div id="node-card" class="hidden">
    <div class="nc-bar" id="nc-bar"></div>
    <div class="nc-header">
      <div class="nc-title" id="nc-title">—</div>
      <div class="nc-subtitle" id="nc-subtitle"></div>
      <button class="nc-close" onclick="selectNode(null)">×</button>
    </div>
    <div class="nc-body">
      <div class="nc-sec">
        <div class="nc-row"><span class="nc-k">status</span><span class="nc-v" id="nc-status">—</span></div>
        <div class="nc-row"><span class="nc-k">port</span>  <span class="nc-v" id="nc-port">—</span></div>
        <div class="nc-row"><span class="nc-k">pivot</span> <span class="nc-v" id="nc-pivot">—</span></div>
        <div class="nc-row"><span class="nc-k">cluster</span><span class="nc-v" id="nc-cluster">—</span></div>
        <div class="nc-row"><span class="nc-k">jarm</span>  <span class="nc-v" id="nc-jarm">—</span></div>
        <div class="nc-row"><span class="nc-k">source</span><span class="nc-v" id="nc-source">—</span></div>
      </div>
      <div class="nc-sec">
        <div class="nc-row"><span class="nc-k">asn</span>    <span class="nc-v" id="nc-asn">—</span></div>
        <div class="nc-row"><span class="nc-k">location</span><span class="nc-v" id="nc-country">—</span></div>
        <div class="nc-row"><span class="nc-k">lat/lon</span><span class="nc-v" id="nc-geo">—</span></div>
      </div>
      <div class="nc-sec">
        <div class="nc-badges" id="nc-badges"></div>
      </div>
      <div class="nc-sec">
        <div class="nc-rel-hdr">relations</div>
        <div id="nc-rels"><div class="nc-rel-empty">—</div></div>
      </div>
    </div>
  </div>

  <div class="panel" id="legend">
    <div class="panel-hdr">pivot signals</div>
    <div class="leg-item"><div class="leg-dot" style="background:#e89c32"></div>favicon_hash</div>
    <div class="leg-item"><div class="leg-dot" style="background:#32a0e8"></div>header_hash</div>
    <div class="leg-item"><div class="leg-dot" style="background:#8c32e8"></div>jarm</div>
    <div class="leg-item"><div class="leg-dot" style="background:#32e39a"></div>asn</div>
    <div class="leg-item"><div class="leg-dot" style="background:#e83272"></div>tls_issuer</div>
    <div class="leg-item"><div class="leg-dot" style="background:#39c9ff"></div>seed root</div>
    <div class="leg-item"><div class="leg-dot" style="background:#e8e032;border:1px dashed #e8e03288"></div>orphan</div>
    <div class="leg-hint">1=MAP 2=2D 3=3D R=reset Z=fit C=cache</div>
  </div>

  <div class="panel" id="cache-panel">
    <div class="panel-hdr">scan cache</div>
    <div class="cache-row"><span class="cache-k">target</span><span class="cache-v" id="cache-target">--</span></div>
    <div class="cache-row"><span class="cache-k">status</span><span class="cache-v miss" id="cache-status">--</span></div>
    <div class="cache-row"><span class="cache-k">age</span>   <span class="cache-v" id="cache-age">--</span></div>
    <div class="cache-row"><span class="cache-k">action</span>
      <button class="tbtn" style="font-size:8px;padding:2px 8px;margin:0" onclick="clearCache()">clear</button>
    </div>
  </div>

  <div class="panel" id="log-panel">
    <div class="panel-hdr">pipeline log</div>
    <div id="log-entries"></div>
  </div>

  <div id="toolbar">
    <button class="tbtn" onclick="resetLayout()">reset</button>
    <button class="tbtn" id="tborp" onclick="toggleOrphans()">orphans</button>
    <button class="tbtn" onclick="zoomFit()">fit</button>
    <button class="tbtn" onclick="clearCache()">clear cache</button>
    <button class="tbtn" onclick="exportPNG()">export</button>
  </div>
</div>

<script>
window.DEMO_DATA = {
  target: "demo.target.io",
  nodes: [
    {id:0,  host:"demo.target.io",            ip:"104.21.10.1",    port:443,  status_code:200, server:"cloudflare", https:true,  source:"seed",        cluster:"asn-1",  cluster_label:"AS13335 Cloudflare", pivot:"asn",          asn:"AS13335", orphan:false, score:1.0,  lat:37.77,  lon:-122.41, city:"San Francisco", country:"US"},
    {id:1,  host:"api.demo.target.io",        ip:"104.21.10.2",    port:443,  status_code:200, server:"nginx",      https:true,  source:"subfinder",   cluster:"fav-1",  cluster_label:"favicon-a3f2",       pivot:"favicon_hash", asn:"AS13335", orphan:false, score:0.88, lat:37.77,  lon:-122.41, city:"San Francisco", country:"US"},
    {id:2,  host:"mail.demo.target.io",       ip:"52.14.22.100",   port:443,  status_code:200, server:"apache",     https:true,  source:"crt.sh",      cluster:"asn-2",  cluster_label:"AS14618 AWS",        pivot:"asn",          asn:"AS14618", orphan:false, score:0.72, lat:39.96,  lon:-82.99,  city:"Columbus",      country:"US"},
    {id:3,  host:"cdn.demo.target.io",        ip:"104.21.10.5",    port:443,  status_code:200, server:"cloudflare", https:true,  source:"subfinder",   cluster:"asn-1",  cluster_label:"AS13335 Cloudflare", pivot:"asn",          asn:"AS13335", orphan:false, score:0.74, lat:51.51,  lon:-0.13,   city:"London",        country:"GB"},
    {id:4,  host:"blog.demo.target.io",       ip:"52.14.22.101",   port:443,  status_code:200, server:"nginx",      https:true,  source:"crt.sh",      cluster:"fav-2",  cluster_label:"favicon-b8e4",       pivot:"favicon_hash", asn:"AS14618", orphan:false, score:0.82, lat:1.35,   lon:103.82,  city:"Singapore",     country:"SG"},
    {id:5,  host:"auth.demo.target.io",       ip:"104.21.10.6",    port:443,  status_code:200, server:"cloudflare", https:true,  source:"subfinder",   cluster:"hh-1",   cluster_label:"header-9f2a",        pivot:"header_hash",  asn:"AS13335", orphan:false, score:0.80, lat:48.85,  lon:2.35,    city:"Paris",         country:"FR"},
    {id:6,  host:"files.demo.target.io",      ip:"52.14.22.103",   port:443,  status_code:200, server:"nginx",      https:true,  source:"fofa",        cluster:"hh-1",   cluster_label:"header-9f2a",        pivot:"header_hash",  asn:"AS14618", orphan:false, score:0.77, lat:50.11,  lon:8.68,    city:"Frankfurt",     country:"DE"},
    {id:7,  host:"vpn.demo.target.io",        ip:"185.220.101.10", port:1194, status_code:0,   server:"",           https:false, source:"fofa",        cluster:"jarm-1", cluster_label:"jarm-2ad2ad",        pivot:"jarm",         asn:"AS60781", orphan:true,  score:0.55, lat:52.37,  lon:4.90,    city:"Amsterdam",     country:"NL"},
    {id:8,  host:"legacy.demo.target.io",     ip:"203.14.55.10",   port:80,   status_code:200, server:"iis/8.5",    https:false, source:"amass",       cluster:"tls-1",  cluster_label:"tls-letsencrypt",    pivot:"tls_issuer",   asn:"AS0",     orphan:true,  score:0.52, lat:22.39,  lon:114.10,  city:"Hong Kong",     country:"HK"},
    {id:9,  host:"shop.demo.target.io",       ip:"52.14.22.102",   port:443,  status_code:200, server:"apache",     https:true,  source:"assetfinder", cluster:"fav-2",  cluster_label:"favicon-b8e4",       pivot:"favicon_hash", asn:"AS14618", orphan:false, score:0.79, lat:-33.86, lon:151.20,  city:"Sydney",        country:"AU"},
    {id:10, host:"monitoring.demo.target.io", ip:"195.10.22.5",    port:443,  status_code:200, server:"grafana",    https:true,  source:"fofa",        cluster:"fav-2",  cluster_label:"favicon-b8e4",       pivot:"favicon_hash", asn:"AS8560",  orphan:false, score:0.76, lat:42.50,  lon:27.46,   city:"Sofia",         country:"BG"},
    {id:11, host:"ws.demo.target.io",         ip:"104.21.10.8",    port:443,  status_code:101, server:"cloudflare", https:true,  source:"subfinder",   cluster:"hh-1",   cluster_label:"header-9f2a",        pivot:"header_hash",  asn:"AS13335", orphan:false, score:0.78, lat:47.60,  lon:-122.33, city:"Seattle",       country:"US"},
    {id:12, host:"search.demo.target.io",     ip:"52.14.22.105",   port:443,  status_code:200, server:"nginx",      https:true,  source:"crt.sh",      cluster:"tls-1",  cluster_label:"tls-letsencrypt",    pivot:"tls_issuer",   asn:"AS14618", orphan:false, score:0.60, lat:28.63,  lon:77.22,   city:"New Delhi",     country:"IN"},
    {id:13, host:"dev.demo.target.io",        ip:"10.0.0.15",      port:3000, status_code:200, server:"nodejs",     https:false, source:"subfinder",   cluster:null,     cluster_label:"",                   pivot:null,           asn:"PRIVATE", orphan:true,  score:0.45, lat:0,      lon:0,       city:"",              country:""},
    {id:14, host:"staging.demo.target.io",    ip:"10.10.0.5",      port:80,   status_code:200, server:"apache",     https:false, source:"san",         cluster:null,     cluster_label:"",                   pivot:null,           asn:"PRIVATE", orphan:true,  score:0.48, lat:0,      lon:0,       city:"",              country:""}
  ],
  edges: [
    {source:0,  target:1,  strength:0.88, pivot:"favicon_hash"},
    {source:0,  target:3,  strength:0.72, pivot:"asn"},
    {source:0,  target:11, strength:0.72, pivot:"asn"},
    {source:1,  target:9,  strength:0.85, pivot:"favicon_hash"},
    {source:2,  target:9,  strength:0.68, pivot:"asn"},
    {source:4,  target:9,  strength:0.81, pivot:"favicon_hash"},
    {source:4,  target:10, strength:0.76, pivot:"favicon_hash"},
    {source:5,  target:6,  strength:0.79, pivot:"header_hash"},
    {source:5,  target:11, strength:0.78, pivot:"header_hash"},
    {source:6,  target:11, strength:0.77, pivot:"header_hash"},
    {source:7,  target:8,  strength:0.52, pivot:"jarm"},
    {source:8,  target:12, strength:0.42, pivot:"tls_issuer"}
  ]
};
</script>

<script src="https://cdnjs.cloudflare.com/ajax/libs/leaflet/1.9.4/leaflet.min.js"></script>
<script src="/static/app.js"></script>
<script>
(function() {
  var steps = ['loading map tiles...','connecting pipeline...','checking cache...','ready'];
  var si = 0;
  var ltxt = document.getElementById('loader-txt');
  var intv = setInterval(function() {
    si++;
    if (si >= steps.length) {
      clearInterval(intv);
      var loader = document.getElementById('loader');
      loader.style.transition = 'opacity 0.5s';
      loader.style.opacity = '0';
      setTimeout(function() { loader.style.display = 'none'; }, 520);
      boot();
      return;
    }
    ltxt.textContent = steps[si];
  }, 450);
})();
</script>
</body>
</html>`
