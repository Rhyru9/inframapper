/* ═══════════════════════════════════════════════════════════
   InfraMapper — Neural World Map  |  app.js
   Depends on: Leaflet (window.L), loaded in HTML before this
   ═══════════════════════════════════════════════════════════ */

'use strict';

// ── PIVOT COLOR MAP ─────────────────────────────────────────
const PC = {
  favicon_hash: '#e89c32',
  header_hash:  '#32a0e8',
  jarm:         '#8c32e8',
  asn:          '#32e39a',
  tls_issuer:   '#e83272',
  seed:         '#39c9ff',
  default:      '#bfcfe3'
};

function pivotColor(n) {
  if (n.id === 0) return PC.seed;
  return PC[n.pivot] || PC.default;
}

// ── GLOBAL STATE ────────────────────────────────────────────
let nodes = [], edges = [];
let selected = null, hovered = null;
let mode = 'map';
let tick = 0, layoutStable = false;
let arcPhase = 0;
let activeFilters = new Set(['all','favicon_hash','header_hash','jarm','asn','tls_issuer']);
let showOrphansOnly = false;
let leafletMap = null;
let leafletMarkers = [];
let leafletLines = [];

// 2D pan/zoom/drag state
let camX = 0, camY = 0, camZ = 1.0;
let panning = false, panStart = {x:0,y:0}, panOrigin = {x:0,y:0};
let dragging = null, dragOff = {x:0,y:0};

// 3D state
let pts3 = [], rot3 = {ax:0.2, ay:0, vx:0, vy:0.003};
let drag3 = false, drag3Moved = false, last3 = {x:0,y:0}, pending3 = null;
let zoom3 = 1.0;
let pinchDist3 = 0;

// Canvas refs
let overlayCanvas, overlayCtx, W2, H2; // overlay on leaflet
let mainCanvas, mainCtx, W, H;          // 2D/3D canvas

// ── CACHE SYSTEM ────────────────────────────────────────────
const CACHE_VERSION = 2;

const Cache = {
  key(target) { return 'im_cache_v' + CACHE_VERSION + '_' + target; },

  save(target, data) {
    try {
      const payload = { ts: Date.now(), target, data };
      sessionStorage.setItem(this.key(target), JSON.stringify(payload));
      return true;
    } catch(e) { return false; }
  },

  load(target) {
    try {
      const raw = sessionStorage.getItem(this.key(target));
      if (!raw) return null;
      const payload = JSON.parse(raw);
      // Expire after 30 minutes
      if (Date.now() - payload.ts > 30 * 60 * 1000) {
        sessionStorage.removeItem(this.key(target));
        return null;
      }
      return payload.data;
    } catch(e) { return null; }
  },

  clear(target) {
    sessionStorage.removeItem(this.key(target));
  },

  age(target) {
    try {
      const raw = sessionStorage.getItem(this.key(target));
      if (!raw) return null;
      const payload = JSON.parse(raw);
      const sec = Math.round((Date.now() - payload.ts) / 1000);
      if (sec < 60) return sec + 's ago';
      if (sec < 3600) return Math.round(sec/60) + 'm ago';
      return Math.round(sec/3600) + 'h ago';
    } catch(e) { return null; }
  }
};

// ── INIT FROM GRAPH DATA ────────────────────────────────────
let currentTarget = '';

function initFromData(data, fromCache) {
  currentTarget = data.target || '';
  nodes = (data.nodes || []).map(n => {
    const col = pivotColor(n);
    const r = n.id === 0 ? 10 : (n.orphan ? 5 : (n.cluster ? 7 : 5));
    return {
      ...n, color: col, r,
      pulse: Math.random() * Math.PI * 2,
      vx: 0, vy: 0,
      x: W/2 + (Math.random()-.5)*360,
      y: H/2 + (Math.random()-.5)*240,
      visible: true
    };
  });
  edges = data.edges || [];

  // Save to cache (only fresh data)
  if (!fromCache && currentTarget) Cache.save(currentTarget, data);

  updateHUD();
  applyFilters();
  updateCachePanel(fromCache);
  addLog((fromCache ? '[cache] ' : '') + 'graph loaded — ' + nodes.length + ' nodes, ' + edges.length + ' edges');
  addLog('geo: ' + nodes.filter(n=>n.lat&&n.lon).length + ' geolocated, ' + nodes.filter(n=>n.orphan).length + ' orphans');

  if (mode === 'map') renderLeafletMarkers();
  if (mode === 'attr') buildAttrFromScan();
}

function updateHUD() {
  const cls = new Set(nodes.map(n=>n.cluster).filter(Boolean));
  setText('sn', nodes.length);
  setText('se', edges.length);
  setText('sc', cls.size);
  setText('so', nodes.filter(n=>n.orphan).length);
  setText('sg', nodes.filter(n=>n.lat&&n.lon).length);
  const tgt = document.getElementById('htgt');
  if (tgt) tgt.textContent = currentTarget || '—';
}

function updateCachePanel(fromCache) {
  const age = currentTarget ? Cache.age(currentTarget) : null;
  const el = document.getElementById('cache-status');
  const el2 = document.getElementById('cache-age');
  const el3 = document.getElementById('cache-target');
  if (el)  { el.textContent = fromCache ? 'HIT' : 'MISS'; el.className = 'cache-v ' + (fromCache ? 'hit' : 'miss'); }
  if (el2) el2.textContent = age || '—';
  if (el3) el3.textContent = currentTarget || '—';
}

function setText(id, val) {
  const el = document.getElementById(id);
  if (el) el.textContent = val;
}

// ── FILTERS ──────────────────────────────────────────────────
function applyFilters() {
  nodes.forEach(n => {
    if (showOrphansOnly) { n.visible = !!n.orphan; return; }
    if (activeFilters.has('all')) { n.visible = true; return; }
    n.visible = activeFilters.has(n.pivot || 'default') || n.id === 0;
  });
  if (mode === 'map') renderLeafletMarkers();
  layoutStable = false;
}

function toggleFilter(piv) {
  const allBtn = document.querySelector('[data-piv="all"]');
  if (piv === 'all') {
    const was = activeFilters.has('all');
    if (was) {
      activeFilters.clear();
      allBtn && allBtn.classList.remove('on');
    } else {
      activeFilters = new Set(['all','favicon_hash','header_hash','jarm','asn','tls_issuer']);
      document.querySelectorAll('.fpill').forEach(b => b.classList.add('on'));
    }
  } else {
    if (activeFilters.has(piv)) activeFilters.delete(piv);
    else activeFilters.add(piv);
    const btn = document.querySelector('[data-piv="' + piv + '"]');
    if (btn) btn.classList.toggle('on', activeFilters.has(piv));
    if (activeFilters.size === 5) { activeFilters.add('all'); allBtn && allBtn.classList.add('on'); }
    else { activeFilters.delete('all'); allBtn && allBtn.classList.remove('on'); }
  }
  applyFilters();
}

function toggleOrphans() {
  showOrphansOnly = !showOrphansOnly;
  const btn = document.getElementById('tborp');
  if (btn) btn.classList.toggle('on', showOrphansOnly);
  applyFilters();
}

// ── SELECT NODE ──────────────────────────────────────────────
function ncSet(id, val, cls) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = val;
  el.className = 'nc-v' + (cls ? ' ' + cls : '');
}
function ncTxt(id, val) {
  const el = document.getElementById(id);
  if (el) el.textContent = val;
}

function selectNode(n) {
  selected = n;
  const card = document.getElementById('node-card');
  if (!card) return;

  const cachePanel = document.getElementById('cache-panel');
  if (!n) {
    card.classList.add('hidden');
    if (cachePanel) cachePanel.style.display = '';
    return;
  }
  if (cachePanel) cachePanel.style.display = 'none';

  // Color accent bar
  const bar = document.getElementById('nc-bar');
  if (bar) bar.style.background = n.color || '#39c9ff';

  // Header
  ncTxt('nc-title',    n.host || n.ip || '—');
  ncTxt('nc-subtitle', n.host && n.ip ? n.ip : '');

  // Details
  const sc = n.status_code;
  const scCls = sc >= 200 && sc < 300 ? 'ok' : sc >= 400 ? 'err' : 'warn';
  ncSet('nc-status',  sc ? String(sc) + (n.server ? ' · ' + n.server : '') : '—', sc ? scCls : '');
  ncSet('nc-port',    n.port ? ':' + n.port + (n.https ? ' https' : ' http') : '—', '');
  ncSet('nc-pivot',   n.pivot || '—', '');
  ncSet('nc-cluster', n.cluster_label || n.cluster || '—', '');
  ncSet('nc-asn',     n.asn || '—', '');
  ncSet('nc-jarm',    n.jarm ? n.jarm.slice(0,16) + '…' : '—', '');

  // Geo
  const geoStr = (n.lat && n.lon) ? n.lat.toFixed(2) + ', ' + n.lon.toFixed(2) : '—';
  ncSet('nc-country',  [n.country, n.city].filter(Boolean).join(' · ') || '—', '');
  ncSet('nc-geo',      geoStr, '');
  ncSet('nc-source',   n.source || '—', '');

  // Badges
  const bdg = document.getElementById('nc-badges');
  if (bdg) {
    const list = [];
    if (n.id === 0)  list.push(['seed root', '#39c9ff']);
    if (n.https)     list.push(['https',     '#32e39a']);
    if (n.orphan)    list.push(['orphan ⚠',  '#e8e032']);
    if (n.pivot)     list.push([n.pivot,     PC[n.pivot] || '#bfcfe3']);
    bdg.innerHTML = list.map(([lbl, col]) =>
      '<span class="nc-badge" style="color:' + col + ';border-color:' + col + '44">' + lbl + '</span>'
    ).join('');
  }

  // Relations
  const relsEl = document.getElementById('nc-rels');
  if (relsEl) {
    const connected = [];
    edges.forEach(e => {
      let peer = null;
      if (e.source === n.id) peer = nodes.find(x => x.id === e.target);
      else if (e.target === n.id) peer = nodes.find(x => x.id === e.source);
      if (peer) connected.push({ peer, via: e.pivot || '?', strength: e.strength || 0 });
    });
    connected.sort((a, b) => b.strength - a.strength);
    if (!connected.length) {
      relsEl.innerHTML = '<div class="nc-rel-empty">no direct relations</div>';
    } else {
      relsEl.innerHTML = connected.map(c =>
        '<div class="nc-rel-item" onclick="pickNode(' + c.peer.id + ')">' +
        '<div class="nc-rel-dot" style="background:' + c.peer.color + '"></div>' +
        '<span class="nc-rel-name">' + (c.peer.host || c.peer.ip) + '</span>' +
        '<span class="nc-rel-via">' + c.via + '</span>' +
        '</div>'
      ).join('');
    }
  }

  card.classList.remove('hidden');

  // Pan map to node
  if (mode === 'map' && leafletMap && n.lat && n.lon) {
    leafletMap.panTo([n.lat, n.lon], { animate: true, duration: 0.6 });
  }
}

// ── LEAFLET MAP MODE ─────────────────────────────────────────
function initLeafletMap() {
  if (leafletMap) return;

  leafletMap = L.map('leaflet-map', {
    center: [20, 0],
    zoom: 2.5,
    zoomControl: false,
    attributionControl: false,
    minZoom: 2,
    maxZoom: 8,
    preferCanvas: true
  });

  // OpenStreetMap tile — dark-filtered via CSS
  // CartoDB Dark Matter — native dark map, white labels, visible borders
  L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
    maxZoom: 19,
    subdomains: ['a','b','c','d']
  }).addTo(leafletMap);

  // Zoom control bottom-right
  L.control.zoom({ position: 'bottomright' }).addTo(leafletMap);

  // Map click to deselect
  leafletMap.on('click', () => selectNode(null));

  // Resize overlay canvas on map move/resize
  leafletMap.on('moveend zoomend resize', () => {
    resizeOverlay();
    drawLeafletOverlay();
  });

  resizeOverlay();
  addLog('leaflet map initialized (OSM tiles)');
}

function resizeOverlay() {
  if (!overlayCanvas || !leafletMap) return;
  const container = leafletMap.getContainer();
  W2 = container.offsetWidth;
  H2 = container.offsetHeight;
  overlayCanvas.width  = W2 * devicePixelRatio;
  overlayCanvas.height = H2 * devicePixelRatio;
  overlayCanvas.style.width  = W2 + 'px';
  overlayCanvas.style.height = H2 + 'px';
  overlayCtx.setTransform(devicePixelRatio, 0, 0, devicePixelRatio, 0, 0);
}

function renderLeafletMarkers() {
  if (!leafletMap) return;

  // Clear old markers and lines
  leafletMarkers.forEach(m => m.remove());
  leafletLines.forEach(l => l.remove());
  leafletMarkers = [];
  leafletLines = [];

  // BUG FIX #3: versi lama tidak pernah buat L.circleMarker() — array leafletMarkers
  // selalu kosong, peta Leaflet tampil tapi tanpa satu pun node dot.
  const visNodes = nodes.filter(n => n.visible && n.lat && n.lon);
  visNodes.forEach(n => {
    const m = L.circleMarker([n.lat, n.lon], {
      radius: Math.max(5, n.r || 6),
      color: n.color || '#39c9ff',
      fillColor: n.color || '#39c9ff',
      fillOpacity: 0.82,
      weight: 1.2,
      opacity: 0.9
    }).addTo(leafletMap);

    // Tooltip on hover
    m.bindTooltip(n.host || n.ip || '?', {
      permanent: false,
      direction: 'top',
      className: 'nm-tooltip'
    });

    // Click to select
    m.on('click', () => {
      selectNode(n);
      drawLeafletOverlay();
    });

    leafletMarkers.push(m);
  });

  // Draw overlay arcs on canvas
  drawLeafletOverlay();
}

// Convert lat/lon to canvas pixel via Leaflet's projection
function latLonToCanvas(lat, lon) {
  const point = leafletMap.latLngToContainerPoint(L.latLng(lat, lon));
  return { x: point.x, y: point.y };
}

// Country relation groups — translucent halo + label for countries with 2+ nodes
function drawCountryGroups() {
  if (!overlayCtx || !leafletMap) return;
  const byCC = {};
  nodes.filter(n => n.visible && n.lat && n.lon && n.country && n.country.length > 0).forEach(n => {
    if (!byCC[n.country]) byCC[n.country] = [];
    byCC[n.country].push(n);
  });
  Object.entries(byCC).forEach(([cc, grp]) => {
    if (grp.length < 2) return;
    const clat = grp.reduce((s, n) => s + n.lat, 0) / grp.length;
    const clon = grp.reduce((s, n) => s + n.lon, 0) / grp.length;
    const cp = latLonToCanvas(clat, clon);
    let maxR = 30;
    grp.forEach(n => {
      const p = latLonToCanvas(n.lat, n.lon);
      const d = Math.hypot(p.x - cp.x, p.y - cp.y);
      if (d + 22 > maxR) maxR = d + 22;
    });
    const dominant = grp.slice().sort((a, b) => (b.score || 0) - (a.score || 0))[0];
    const col = PC[dominant.pivot] || PC.default;
    overlayCtx.save();
    overlayCtx.beginPath();
    overlayCtx.arc(cp.x, cp.y, maxR, 0, Math.PI * 2);
    overlayCtx.fillStyle = col + '0d';
    overlayCtx.fill();
    overlayCtx.strokeStyle = col + '4a';
    overlayCtx.lineWidth = 1.5;
    overlayCtx.setLineDash([5, 7]);
    overlayCtx.stroke();
    overlayCtx.setLineDash([]);
    overlayCtx.font = 'bold 10px JetBrains Mono, monospace';
    overlayCtx.textAlign = 'center';
    overlayCtx.shadowColor = col;
    overlayCtx.shadowBlur = 5;
    overlayCtx.fillStyle = col + 'cc';
    overlayCtx.fillText(cc + ' \xd7' + grp.length, cp.x, cp.y - maxR - 4);
    overlayCtx.shadowBlur = 0;
    overlayCtx.textAlign = 'left';
    overlayCtx.restore();
  });
}

let animFrameId = null;
function drawLeafletOverlay() {
  if (!overlayCtx || !leafletMap) return;
  overlayCtx.clearRect(0, 0, W2, H2);
  arcPhase += 0.018;
  drawCountryGroups();

  const visNodes = nodes.filter(n => n.visible && n.lat && n.lon);

  // Draw neuron arcs (edges between geolocated nodes)
  edges.forEach(e => {
    const a = nodes[e.source], b = nodes[e.target];
    if (!a || !b || !a.visible || !b.visible) return;
    if (!a.lat || !b.lat) return;
    if (!activeFilters.has('all') && !activeFilters.has(e.pivot)) return;

    const pa = latLonToCanvas(a.lat, a.lon);
    const pb = latLonToCanvas(b.lat, b.lon);
    const col = PC[e.pivot] || PC.default;
    const s   = e.strength || 0.4;

    const mx = (pa.x + pb.x) / 2;
    const my = (pa.y + pb.y) / 2;
    const dist = Math.hypot(pb.x - pa.x, pb.y - pa.y);
    const lift = Math.min(dist * 0.38, 110);
    const cpx = mx, cpy = my - lift;

    const alpha = Math.round(s * 185).toString(16).padStart(2, '0');
    const dashLen = Math.max(20, dist * 0.55);

    overlayCtx.strokeStyle = col + alpha;
    overlayCtx.lineWidth   = Math.max(1.5, s * 2.8);
    overlayCtx.lineCap     = 'round';
    overlayCtx.setLineDash([dashLen * 0.6, dashLen * 0.4]);
    overlayCtx.lineDashOffset = -(arcPhase * dashLen * 0.45);
    overlayCtx.beginPath();
    overlayCtx.moveTo(pa.x, pa.y);
    overlayCtx.quadraticCurveTo(cpx, cpy, pb.x, pb.y);
    overlayCtx.stroke();
    overlayCtx.setLineDash([]);

    // Travelling dot along arc — glowing
    const t = (Math.sin(arcPhase * 0.6 + e.source * 0.4) * 0.5 + 0.5);
    const bx = (1-t)*(1-t)*pa.x + 2*(1-t)*t*cpx + t*t*pb.x;
    const by = (1-t)*(1-t)*pa.y + 2*(1-t)*t*cpy + t*t*pb.y;
    overlayCtx.fillStyle = col;
    overlayCtx.shadowColor = col;
    overlayCtx.shadowBlur = 7;
    overlayCtx.beginPath();
    overlayCtx.arc(bx, by, 3, 0, Math.PI * 2);
    overlayCtx.fill();
    overlayCtx.shadowBlur = 0;
  });

  // Draw node circles on top of arcs
  visNodes.forEach(n => {
    n.pulse = (n.pulse || 0) + 0.03;
    const p = latLonToCanvas(n.lat, n.lon);
    const isSel = n === selected;
    const isHov = n === hovered;
    const pr = n.r + Math.sin(n.pulse) * (isSel ? 2.5 : 0.7);

    // Glow
    const g = overlayCtx.createRadialGradient(p.x, p.y, 0, p.x, p.y, pr * 4.5);
    g.addColorStop(0, n.color + '66');
    g.addColorStop(1, n.color + '00');
    overlayCtx.fillStyle = g;
    overlayCtx.beginPath();
    overlayCtx.arc(p.x, p.y, pr * 4.5, 0, Math.PI * 2);
    overlayCtx.fill();

    // Body
    overlayCtx.fillStyle = isSel ? n.color : (isHov ? n.color + 'ee' : n.color + 'cc');
    overlayCtx.beginPath();
    overlayCtx.arc(p.x, p.y, pr, 0, Math.PI * 2);
    overlayCtx.fill();
    overlayCtx.strokeStyle = n.color + 'dd';
    overlayCtx.lineWidth = 1.8;
    overlayCtx.stroke();

    // Orphan ring
    if (n.orphan) {
      overlayCtx.strokeStyle = '#e8e038aa';
      overlayCtx.lineWidth = 1;
      overlayCtx.setLineDash([2,3]);
      overlayCtx.beginPath();
      overlayCtx.arc(p.x, p.y, pr + 5, 0, Math.PI * 2);
      overlayCtx.stroke();
      overlayCtx.setLineDash([]);
    }

    // Seed crown ring
    if (n.id === 0) {
      overlayCtx.strokeStyle = n.color + '66';
      overlayCtx.lineWidth = 1.2;
      overlayCtx.setLineDash([2,4]);
      overlayCtx.beginPath();
      overlayCtx.arc(p.x, p.y, pr + 9, 0, Math.PI * 2);
      overlayCtx.stroke();
      overlayCtx.setLineDash([]);
    }

    // Label for selected/hovered
    if (isSel || isHov) drawCanvasLabel(overlayCtx, p.x, p.y, pr, n.host || n.ip || '?');
  });

  // No-geo nodes: cluster bottom-left corner
  const noGeo = nodes.filter(n => n.visible && (!n.lat || !n.lon));
  if (noGeo.length > 0) {
    overlayCtx.font = '9px JetBrains Mono, monospace';
    overlayCtx.fillStyle = '#5a7a9a'; // BUG FIX: was #2e4a68 — too dark, invisible on dark bg
    overlayCtx.fillText('no-geo: ' + noGeo.length, 16, H2 - 80);
    noGeo.forEach((n, i) => {
      const nx = 16 + (i % 10) * 16;
      const ny = H2 - 65 + Math.floor(i / 10) * 16;
      n._ngx = nx; n._ngy = ny;
      overlayCtx.fillStyle = n.color + '88';
      overlayCtx.beginPath();
      overlayCtx.arc(nx, ny, 4, 0, Math.PI * 2);
      overlayCtx.fill();
      if (n === selected || n === hovered) drawCanvasLabel(overlayCtx, nx, ny, 4, n.host || n.ip || '?');
    });
  }
}

// ── 2D FORCE GRAPH ───────────────────────────────────────────
const SIM = { repel: 3000, attract: 0.00013, elen: 115, damp: 0.85, grav: 0.003 };

function simStep() {
  if (layoutStable) return;
  const vis = nodes.filter(n => n.visible);
  let maxV = 0;

  for (let i = 0; i < vis.length; i++) {
    for (let j = i + 1; j < vis.length; j++) {
      const a = vis[i], b = vis[j];
      const dx = b.x-a.x, dy = b.y-a.y, d2 = dx*dx+dy*dy+1, d = Math.sqrt(d2);
      const f = SIM.repel / d2;
      a.vx -= f*dx/d; a.vy -= f*dy/d;
      b.vx += f*dx/d; b.vy += f*dy/d;
    }
  }

  edges.forEach(e => {
    const a = nodes[e.source], b = nodes[e.target];
    if (!a || !b || !a.visible || !b.visible) return;
    const dx = b.x-a.x, dy = b.y-a.y, d = Math.sqrt(dx*dx+dy*dy)+1;
    const f = SIM.attract * (d - SIM.elen) * (e.strength || 0.5);
    a.vx += f*dx/d; a.vy += f*dy/d;
    b.vx -= f*dx/d; b.vy -= f*dy/d;
  });

  vis.forEach(n => {
    if (n === dragging) return;
    n.vx += (W/2 - n.x) * SIM.grav;
    n.vy += (H/2 - n.y) * SIM.grav;
    n.vx *= SIM.damp; n.vy *= SIM.damp;
    n.x += n.vx; n.y += n.vy;
    n.x = Math.max(50, Math.min(W-50, n.x));
    n.y = Math.max(50, Math.min(H-50, n.y));
    maxV = Math.max(maxV, Math.abs(n.vx), Math.abs(n.vy));
  });

  if (maxV < 0.12 && tick > 100) layoutStable = true;
}

function draw2D() {
  mainCtx.save();
  mainCtx.clearRect(0, 0, W, H);
  mainCtx.translate(camX, camY);
  mainCtx.scale(camZ, camZ);
  simStep();

  // Edges
  edges.forEach(e => {
    const a = nodes[e.source], b = nodes[e.target];
    if (!a || !b || !a.visible || !b.visible) return;
    if (!activeFilters.has('all') && !activeFilters.has(e.pivot)) return;
    const s = e.strength || 0.3;
    const al = Math.round(s * 175).toString(16).padStart(2,'0');
    mainCtx.strokeStyle = (PC[e.pivot] || PC.default) + al;
    mainCtx.lineWidth = Math.max(1.5, s * 3.5);
    mainCtx.lineCap = 'round';
    mainCtx.setLineDash(e.pivot === 'seed' ? [4,4] : []);
    mainCtx.beginPath(); mainCtx.moveTo(a.x, a.y); mainCtx.lineTo(b.x, b.y); mainCtx.stroke();
    mainCtx.setLineDash([]);
  });

  // Nodes
  nodes.filter(n => n.visible).forEach(n => {
    n.pulse = (n.pulse||0) + 0.035;
    const isSel = n === selected, isHov = n === hovered;
    const pr = n.r + Math.sin(n.pulse) * (isSel ? 2.5 : 0.8);
    const g = mainCtx.createRadialGradient(n.x, n.y, 0, n.x, n.y, pr*4);
    g.addColorStop(0, n.color+'55'); g.addColorStop(1, n.color+'00');
    mainCtx.fillStyle = g; mainCtx.beginPath(); mainCtx.arc(n.x, n.y, pr*4, 0, Math.PI*2); mainCtx.fill();
    mainCtx.fillStyle = isSel ? n.color : (isHov ? n.color+'ee' : n.color+'bb');
    mainCtx.beginPath(); mainCtx.arc(n.x, n.y, pr, 0, Math.PI*2); mainCtx.fill();
    mainCtx.strokeStyle = n.color+'ee'; mainCtx.lineWidth = 2; mainCtx.stroke();
    if (n.orphan) {
      mainCtx.strokeStyle = '#e8e038bb'; mainCtx.lineWidth = 1.2; mainCtx.setLineDash([3,3]);
      mainCtx.beginPath(); mainCtx.arc(n.x, n.y, pr+5, 0, Math.PI*2); mainCtx.stroke();
      mainCtx.setLineDash([]);
    }
    if (isSel || isHov) drawCanvasLabel(mainCtx, n.x, n.y, n.r, n.host || n.ip || '?');
  });

  mainCtx.restore();
}

// ── 3D GLOBE ─────────────────────────────────────────────────
function build3D() {
  const GLOBE_R = 200;
  const GOLDEN  = Math.PI * (3 - Math.sqrt(5)); // golden angle ≈137.5°

  // Count nodes per lat/lon bucket (1° resolution)
  const locCount = {}, locUsed = {};
  nodes.forEach(n => {
    if (!n.lat || !n.lon) return;
    const key = Math.round(n.lat) + ',' + Math.round(n.lon);
    locCount[key] = (locCount[key] || 0) + 1;
  });

  pts3 = nodes.map((n, i) => {
    let ox, oy, oz;
    if (n.lat && n.lon) {
      const key = Math.round(n.lat) + ',' + Math.round(n.lon);
      const cnt = locCount[key] || 1;
      const idx = locUsed[key] = (locUsed[key] || 0);
      locUsed[key]++;

      let phi   = (90 - n.lat) * Math.PI / 180;
      let theta = (n.lon + 180) * Math.PI / 180;

      // Fibonacci/golden-angle spiral spread for co-located nodes.
      // Node 0 at centre, rest spiral outward — avoids stacking.
      if (idx > 0) {
        const jAngle  = idx * GOLDEN;
        const jRadius = Math.sqrt(idx / cnt) * 0.22; // radians, grows with sqrt
        phi   += Math.cos(jAngle) * jRadius;
        theta += Math.sin(jAngle) * jRadius;
        // keep phi in valid range
        phi = Math.max(0.04, Math.min(Math.PI - 0.04, phi));
      }

      ox = GLOBE_R * Math.sin(phi) * Math.cos(theta);
      oy = GLOBE_R * Math.cos(phi);
      oz = GLOBE_R * Math.sin(phi) * Math.sin(theta);
    } else {
      // No-geo nodes: Fibonacci sphere surface at slightly smaller radius
      const t     = i / Math.max(nodes.length - 1, 1);
      const phi   = Math.acos(1 - 2 * t);
      const theta = i * GOLDEN;
      const r     = GLOBE_R - 20;
      ox = r * Math.sin(phi) * Math.cos(theta);
      oy = r * Math.cos(phi);
      oz = r * Math.sin(phi) * Math.sin(theta);
    }
    return { ox, oy, oz, n };
  });
}

function proj3(x, y, z) {
  const cx = Math.cos(rot3.ax), sx = Math.sin(rot3.ax);
  const cy = Math.cos(rot3.ay), sy = Math.sin(rot3.ay);
  const y1 = y*cx - z*sx, z1 = y*sx + z*cx;
  const x2 = x*cy + z1*sy, z2 = -x*sy + z1*cy;
  const fov = 650, sc = fov / (fov + z2 + 250);
  const sz = sc * zoom3;
  return { x: W/2 + x2*sz, y: H/2 + y1*sz, z: z2, sc: sz };
}

function draw3D() {
  mainCtx.clearRect(0, 0, W, H);
  if (!drag3) {
    rot3.ay += rot3.vy; rot3.ax += rot3.vx * 0.4;
    rot3.vy *= 0.974; rot3.vx *= 0.974;
    if (Math.abs(rot3.vy) < 0.0008) rot3.vy += 0.0004;
  }
  rot3.ax = Math.max(-1.45, Math.min(1.45, rot3.ax));

  const proj = pts3.map(p => {
    const r = proj3(p.ox, p.oy, p.oz);
    return { ...p, px: r.x, py: r.y, pz: r.z, sc: r.sc };
  });

  // Globe rings — sized to match GLOBE_R=200
  [220, 170, 110].forEach((r, i) => {
    const alphas = ['30','22','14'];
    mainCtx.strokeStyle = '#39c9ff' + alphas[i];
    mainCtx.lineWidth = 0.9;
    [true, false].forEach(isEq => {
      mainCtx.beginPath();
      for (let a = 0; a <= Math.PI*2; a += 0.04) {
        const p = isEq ? proj3(Math.cos(a)*r, 0, Math.sin(a)*r)
                       : proj3(0, Math.cos(a)*r, Math.sin(a)*r);
        a === 0 ? mainCtx.moveTo(p.x, p.y) : mainCtx.lineTo(p.x, p.y);
      }
      mainCtx.stroke();
    });
  });

  // Edges — thin, low alpha so bundles don't dominate
  mainCtx.lineCap = 'round';
  edges.forEach(e => {
    const a = proj[e.source], b = proj[e.target];
    if (!a || !b || !a.n.visible || !b.n.visible) return;
    if (!activeFilters.has('all') && !activeFilters.has(e.pivot)) return;
    const isSel = a.n === selected || b.n === selected;
    mainCtx.strokeStyle = (PC[e.pivot] || PC.default) + (isSel ? 'bb' : '44');
    mainCtx.lineWidth   = isSel ? 1.5 : Math.max(0.6, (e.strength || 0.5) * 1.0);
    mainCtx.beginPath(); mainCtx.moveTo(a.px, a.py); mainCtx.lineTo(b.px, b.py); mainCtx.stroke();
  });

  // Nodes (depth sorted, back→front)
  proj.slice().sort((a,b) => a.pz - b.pz).forEach(p => {
    if (!p.n.visible) return;
    const n = p.n;
    n.pulse = (n.pulse||0) + 0.028;
    const isSel = n === selected;
    // Smaller, tighter dots — sc already encodes depth perspective
    const r2 = Math.max(2, n.r * p.sc * 1.1 + Math.sin(n.pulse) * p.sc * 0.35);

    // Subtle glow — only for selected or seed node
    if (isSel || n.id === 0) {
      const glowR = r2 * (isSel ? 3.5 : 2.2);
      const g3 = mainCtx.createRadialGradient(p.px, p.py, 0, p.px, p.py, glowR);
      g3.addColorStop(0, n.color + (isSel ? '66' : '33')); g3.addColorStop(1, n.color + '00');
      mainCtx.fillStyle = g3; mainCtx.beginPath(); mainCtx.arc(p.px, p.py, glowR, 0, Math.PI*2); mainCtx.fill();
    }

    // Solid dot
    mainCtx.fillStyle   = n.color + (isSel ? 'ff' : 'cc');
    mainCtx.beginPath(); mainCtx.arc(p.px, p.py, r2, 0, Math.PI*2); mainCtx.fill();
    mainCtx.strokeStyle = n.color + '88'; mainCtx.lineWidth = 1; mainCtx.stroke();

    // Orphan dashed ring
    if (n.orphan) {
      mainCtx.strokeStyle = '#e8e03866'; mainCtx.lineWidth = 0.8; mainCtx.setLineDash([2,3]);
      mainCtx.beginPath(); mainCtx.arc(p.px, p.py, r2 + 3, 0, Math.PI*2); mainCtx.stroke();
      mainCtx.setLineDash([]);
    }

    if (isSel) drawCanvasLabel(mainCtx, p.px, p.py, r2, n.host || n.ip || '?');
  });

  // 3D click resolution
  if (pending3) {
    const mx = pending3.x, my = pending3.y; pending3 = null;
    let best = null, bestD = Infinity;
    proj.forEach(p => {
      if (!p.n.visible) return;
      const r2 = Math.max(2, p.n.r * p.sc * 1.1);
      const d = Math.hypot(p.px - mx, p.py - my);
      if (d < r2 + 10 && d < bestD) { bestD = d; best = p.n; }
    });
    selectNode(best || null);
  }
}

// ── DRAW HELPERS ─────────────────────────────────────────────
function drawCanvasLabel(ctx, x, y, r, label) {
  ctx.font = 'bold 11px JetBrains Mono, monospace';
  const tw = ctx.measureText(label).width;
  const bx = x - tw/2 - 5, by = y - r - 24, bw = tw + 10, bh = 16;
  ctx.fillStyle = 'rgba(4,7,12,0.93)';
  ctx.strokeStyle = 'rgba(57,201,255,0.38)';
  ctx.lineWidth = 1;
  ctx.fillRect(bx, by, bw, bh);
  ctx.strokeRect(bx, by, bw, bh);
  ctx.shadowColor = '#39c9ff';
  ctx.shadowBlur = 6;
  ctx.fillStyle = '#ffffff';
  ctx.textAlign = 'center';
  ctx.fillText(label, x, y - r - 11);
  ctx.shadowBlur = 0;
  ctx.textAlign = 'left';
}

// ── MAP MODE: click detection ─────────────────────────────────
function getNodeAtOverlay(mx, my) {
  let best = null, bestD = Infinity;
  nodes.filter(n => n.visible).forEach(n => {
    if (!n.lat || !n.lon) {
      if (n._ngx != null) {
        const d = Math.hypot(n._ngx - mx, n._ngy - my);
        if (d < 8 && d < bestD) { bestD = d; best = n; }
      }
      return;
    }
    const p = latLonToCanvas(n.lat, n.lon);
    const d = Math.hypot(p.x - mx, p.y - my);
    if (d < n.r + 10 && d < bestD) { bestD = d; best = n; }
  });
  return best;
}

// ── 2D helpers ───────────────────────────────────────────────
function worldXY(cx, cy) { return { x: (cx-camX)/camZ, y: (cy-camY)/camZ }; }
function getNodeAt2D(mx, my) {
  const w = worldXY(mx, my);
  return nodes.filter(n => n.visible).find(n => Math.hypot(n.x-w.x, n.y-w.y) < n.r+10) || null;
}

// ── MODE SWITCH ──────────────────────────────────────────────
function setMode(m) {
  mode = m;
  ['map','2d','3d','attr'].forEach(k => {
    const btn = document.getElementById('b' + k);
    if (btn) btn.classList.toggle('on', k === m);
  });

  const leafletEl = document.getElementById('leaflet-map');
  const overlayEl = document.getElementById('overlay-canvas');
  const mainEl    = document.getElementById('main-canvas');
  const attrEl    = document.getElementById('attr-canvas');
  const attrInfoEl= document.getElementById('attr-info');
  const filtersEl = document.getElementById('filters');
  const toolbarEl = document.getElementById('toolbar');

  // Hide all canvases first
  if (leafletEl) leafletEl.style.display = 'none';
  if (overlayEl) overlayEl.style.display = 'none';
  if (mainEl)    mainEl.style.display    = 'none';
  if (attrEl)    attrEl.style.display    = 'none';

  const legendEl    = document.getElementById('legend');
  const searchEl    = document.getElementById('search-wrap');
  const cacheEl     = document.getElementById('cache-panel');

  if (m === 'attr') {
    // Dismiss any open scan node card before entering attr mode
    selectNode(null);
    // Hide scan-specific panels and toolbar
    if (filtersEl) filtersEl.style.display  = 'none';
    if (legendEl)  legendEl.style.display   = 'none';
    if (searchEl)  searchEl.style.display   = 'none';
    if (cacheEl)   cacheEl.style.display    = 'none';
    if (toolbarEl) toolbarEl.style.display  = 'none';
    if (attrEl)    attrEl.style.display     = 'block';
    resizeAttr();
    buildAttrFromScan();
  } else {
    // Restore scan-specific panels and toolbar
    if (filtersEl) filtersEl.style.display  = '';
    if (legendEl)  legendEl.style.display   = '';
    if (searchEl)  searchEl.style.display   = '';
    if (cacheEl)   cacheEl.style.display    = '';
    if (toolbarEl) toolbarEl.style.display  = '';
    if (attrInfoEl) attrInfoEl.style.display = 'none';
    var attrEmptyEl = document.getElementById('attr-empty');
    if (attrEmptyEl) attrEmptyEl.style.display = 'none';

    if (m === 'map') {
      if (leafletEl) leafletEl.style.display = 'block';
      if (overlayEl) overlayEl.style.display = 'block';
      initLeafletMap();
      renderLeafletMarkers();
    } else {
      if (mainEl) mainEl.style.display = 'block';
      // BUG FIX #7: mainCanvas baru visible setelah display='block'.
      resizeMain();
      // Grid bg only in 2D
      if (mainEl) mainEl.classList.toggle('grid-bg', m === '2d');
      if (m === '3d') build3D();
      if (m === '2d') {
        const cx = W / 2, cy = H / 2;
        nodes.forEach(n => {
          if (!n.x || !n.y || (n.x === 0 && n.y === 0)) {
            n.x = cx + (Math.random() - 0.5) * 400;
            n.y = cy + (Math.random() - 0.5) * 280;
          }
        });
        layoutStable = false;
      }
    }
  }
  addLog('mode → ' + m);
}

// ── TOOLBAR ACTIONS ──────────────────────────────────────────
function resetLayout() {
  nodes.forEach(n => {
    n.x = W/2 + (Math.random()-.5)*350;
    n.y = H/2 + (Math.random()-.5)*240;
    n.vx = 0; n.vy = 0;
  });
  camX = 0; camY = 0; camZ = 1.0;
  zoom3 = 1.0;
  layoutStable = false;
  addLog('layout reset');
}

function zoomFit() {
  if (mode === 'map') { leafletMap && leafletMap.setView([20, 0], 2.5, { animate: true }); return; }
  const vis = nodes.filter(n => n.visible);
  if (!vis.length) return;
  const xs = vis.map(n => n.x), ys = vis.map(n => n.y);
  const pw = Math.max(...xs) - Math.min(...xs) + 100;
  const ph = Math.max(...ys) - Math.min(...ys) + 100;
  camZ = Math.min(W/pw, H/ph, 1.5);
  camX = W/2 - (Math.min(...xs) + pw/2) * camZ;
  camY = H/2 - (Math.min(...ys) + ph/2) * camZ;
}

function clearCache() {
  if (currentTarget) {
    Cache.clear(currentTarget);
    updateCachePanel(false);
    addLog('cache cleared for ' + currentTarget);
  }
}

function exportPNG() {
  const canvas = mode === 'map' ? overlayCanvas : mainCanvas;
  if (!canvas) return;
  const a = document.createElement('a');
  a.download = 'inframapper_' + mode + '.png';
  a.href = canvas.toDataURL('image/png');
  a.click();
  addLog('exported PNG (' + mode + ' mode)');
}

// ── LOG ──────────────────────────────────────────────────────
function addLog(msg) {
  const ts  = new Date().toTimeString().slice(0,8);
  const el  = document.getElementById('log-entries');
  if (!el) return;
  const row = document.createElement('div');
  row.className = 'log-entry';
  row.innerHTML = '<span class="log-ts">' + ts + '</span><span class="log-msg">' + msg + '</span>';
  el.insertBefore(row, el.firstChild);
  while (el.children.length > 20) el.removeChild(el.lastChild);
}

// ── SEARCH ───────────────────────────────────────────────────
function doSearch(q) {
  const res = document.getElementById('search-results');
  if (!res) return;
  if (!q) { res.style.display = 'none'; return; }
  const ql = q.toLowerCase();
  const m = nodes.filter(n =>
    (n.host    && n.host.toLowerCase().includes(ql))  ||
    (n.ip      && n.ip.includes(ql))                  ||
    (n.country && n.country.toLowerCase().includes(ql))||
    (n.city    && n.city.toLowerCase().includes(ql))
  ).slice(0, 7);
  if (!m.length) { res.style.display = 'none'; return; }
  res.innerHTML = m.map(n =>
    '<div class="sr-item" onclick="pickNode(' + n.id + ')">' +
    '<span>' + (n.host || n.ip) + '</span>' +
    '<span class="sr-flag">' + (n.country||'') + (n.city ? ' · '+n.city : '') + '</span>' +
    '</div>'
  ).join('');
  res.style.display = 'block';
}

function pickNode(id) {
  const n = nodes.find(x => x.id === id);
  if (n) { selectNode(n); document.getElementById('search-results').style.display = 'none'; }
}

// ── CANVAS EVENT WIRING ──────────────────────────────────────
function wireCanvasEvents() {
  // Overlay (map mode) click
  overlayCanvas.addEventListener('click', e => {
    if (mode !== 'map') return;
    const r = overlayCanvas.getBoundingClientRect();
    const n = getNodeAtOverlay(e.clientX - r.left, e.clientY - r.top);
    if (n) { selectNode(n); e.stopPropagation(); }
  });

  overlayCanvas.addEventListener('mousemove', e => {
    if (mode !== 'map') return;
    const r = overlayCanvas.getBoundingClientRect();
    hovered = getNodeAtOverlay(e.clientX - r.left, e.clientY - r.top);
    overlayCanvas.style.cursor = hovered ? 'pointer' : '';
  });

  // Main canvas (2D/3D)
  mainCanvas.addEventListener('mousemove', e => {
    const r = mainCanvas.getBoundingClientRect();
    const mx = e.clientX - r.left, my = e.clientY - r.top;
    if (mode === '3d') {
      if (drag3) {
        const dx = mx - last3.x, dy = my - last3.y;
        rot3.ay += dx * 0.007; rot3.ax += dy * 0.004;
        rot3.vy = dx * 0.005; rot3.vx = dy * 0.003;
        if (Math.abs(dx) > 0.5 || Math.abs(dy) > 0.5) drag3Moved = true;
        last3 = { x: mx, y: my };
      }
      return;
    }
    if (dragging) {
      const w = worldXY(mx, my);
      dragging.x = w.x + dragOff.x; dragging.y = w.y + dragOff.y;
      dragging.vx = 0; dragging.vy = 0; layoutStable = false; return;
    }
    if (panning) { camX = panOrigin.x+(mx-panStart.x); camY = panOrigin.y+(my-panStart.y); return; }
    hovered = getNodeAt2D(mx, my);
    mainCanvas.style.cursor = hovered ? 'pointer' : 'default';
  });

  mainCanvas.addEventListener('mousedown', e => {
    const r = mainCanvas.getBoundingClientRect();
    const mx = e.clientX - r.left, my = e.clientY - r.top;
    if (mode === '3d') { drag3 = true; drag3Moved = false; last3 = {x:mx,y:my}; return; }
    const n = getNodeAt2D(mx, my);
    if (n) { const w = worldXY(mx, my); dragging = n; dragOff = {x: n.x-w.x, y: n.y-w.y}; }
    else { panning = true; panStart = {x:mx,y:my}; panOrigin = {x:camX,y:camY}; }
  });

  mainCanvas.addEventListener('mouseup', e => {
    const r = mainCanvas.getBoundingClientRect();
    const mx = e.clientX - r.left, my = e.clientY - r.top;
    if (mode === '3d') { drag3 = false; if (!drag3Moved) pending3 = {x:mx,y:my}; drag3Moved = false; return; }
    if (dragging) {
      const w = worldXY(mx, my);
      if (Math.hypot(dragging.x-(w.x+dragOff.x), dragging.y-(w.y+dragOff.y)) < 4) selectNode(dragging);
    } else if (!panning || Math.hypot(mx-panStart.x, my-panStart.y) < 4) {
      selectNode(getNodeAt2D(mx, my));
    }
    dragging = null; panning = false;
  });

  mainCanvas.addEventListener('mouseleave', () => { dragging=null; panning=false; drag3=false; });

  mainCanvas.addEventListener('wheel', e => {
    e.preventDefault();
    if (mode === '2d') {
      const r = mainCanvas.getBoundingClientRect();
      const mx = e.clientX - r.left, my = e.clientY - r.top;
      const fac = e.deltaY < 0 ? 1.12 : 0.89;
      const nz = Math.max(0.2, Math.min(4, camZ * fac));
      camX = mx - (mx-camX)*nz/camZ; camY = my - (my-camY)*nz/camZ; camZ = nz;
    } else if (mode === '3d') {
      const fac = e.deltaY < 0 ? 1.1 : 0.91;
      zoom3 = Math.max(0.25, Math.min(4.0, zoom3 * fac));
    }
  }, { passive: false });

  // Touch (3D rotation)
  mainCanvas.addEventListener('touchmove', e => {
    e.preventDefault();
    if (mode === '3d') {
      if (e.touches.length === 1) {
        const t = e.touches[0];
        const dx = t.clientX-last3.x, dy = t.clientY-last3.y;
        rot3.ay += dx*0.007; rot3.ax += dy*0.004;
        rot3.vy = dx*0.005; rot3.vx = dy*0.003;
        drag3Moved = true;
        last3 = {x:t.clientX, y:t.clientY};
      } else if (e.touches.length === 2) {
        const d = Math.hypot(
          e.touches[0].clientX - e.touches[1].clientX,
          e.touches[0].clientY - e.touches[1].clientY
        );
        if (pinchDist3 > 0) zoom3 = Math.max(0.25, Math.min(4.0, zoom3 * (d / pinchDist3)));
        pinchDist3 = d;
      }
    }
  }, { passive: false });
  mainCanvas.addEventListener('touchstart', e => {
    if (mode === '3d') {
      if (e.touches.length === 1) { drag3=true; drag3Moved=false; last3={x:e.touches[0].clientX,y:e.touches[0].clientY}; }
      if (e.touches.length === 2) { pinchDist3=Math.hypot(e.touches[0].clientX-e.touches[1].clientX,e.touches[0].clientY-e.touches[1].clientY); }
    }
  });
  mainCanvas.addEventListener('touchend', () => { drag3=false; pinchDist3=0; });
}

function wireKeyboard() {
  document.addEventListener('keydown', e => {
    if (e.target.tagName === 'INPUT') return;
    if (e.key === 'Escape') { selected=null; selectNode(null); }
    if (e.key === 'f' || e.key === 'F') layoutStable = false;
    if (e.key === 'r' || e.key === 'R') resetLayout();
    if (e.key === 'z' || e.key === 'Z') zoomFit();
    if (e.key === '1') setMode('map');
    if (e.key === '2') setMode('2d');
    if (e.key === '3') setMode('3d');
    if (e.key === '4') setMode('attr');
    if (e.key === 'c') clearCache();
  });
}

// ── CANVAS RESIZE ─────────────────────────────────────────────
function resizeMain() {
  W = mainCanvas.clientWidth; H = mainCanvas.clientHeight;
  mainCanvas.width  = W * devicePixelRatio;
  mainCanvas.height = H * devicePixelRatio;
  mainCtx.setTransform(devicePixelRatio, 0, 0, devicePixelRatio, 0, 0);
  layoutStable = false;
}

// ── MAIN RENDER LOOP ──────────────────────────────────────────
function renderLoop() {
  tick++;
  if (mode === 'map') {
    drawLeafletOverlay();
  } else if (mode === '2d') {
    draw2D();
  } else if (mode === '3d') {
    draw3D();
  } else if (mode === 'attr') {
    drawAttr();
  }
  requestAnimationFrame(renderLoop);
}

// ── WEBSOCKET / DATA LOAD ─────────────────────────────────────
function connectWS() {
  const wsUrl = 'ws://' + location.host + '/ws';
  let ws;
  try { ws = new WebSocket(wsUrl); } catch(e) { useFallbackData(); return; }

  ws.onopen = () => {
    document.getElementById('wsdot').classList.add('live');
    addLog('websocket connected to ' + location.host);
    // Check cache first, show it immediately, then update when fresh data arrives
    if (currentTarget) {
      const cached = Cache.load(currentTarget);
      if (cached) { initFromData(cached, true); addLog('showing cached data while fetching…'); }
    }
  };

  ws.onmessage = e => {
    try {
      const d = JSON.parse(e.data);
      if (d.nodes) {
        const cached = Cache.load(d.target);
        // BUG FIX #5: fresh data dengan node > 0 selalu menang atas cache apapun.
        // Sebelumnya: cache lama 0-node "menang" melawan data baru 0-node karena
        // kondisi length-equality terpenuhi → UI stuck di state kosong selamanya.
        const freshHasData  = d.nodes.length > 0;
        const cacheIsEmpty  = !cached || cached.nodes.length === 0;
        const dataChanged   = !cached
          || cached.nodes.length !== d.nodes.length
          || cached.target !== d.target;

        if (freshHasData || cacheIsEmpty || dataChanged) {
          initFromData(d, false);
        } else {
          addLog('[cache] data unchanged, using cache');
        }
      }
    } catch(ex) { addLog('ws parse error: ' + ex.message); }
  };

  ws.onerror = () => ws.close();
  ws.onclose = () => {
    document.getElementById('wsdot').classList.remove('live');
    if (nodes.length === 0) useFallbackData();
    addLog('ws disconnected, retry in 5s…');
    setTimeout(connectWS, 5000);
  };
}

// Fallback demo data (same shape as GraphData from server.go)
function useFallbackData() {
  const DEMO_TARGET = 'demo.target.io';
  const cached = Cache.load(DEMO_TARGET);
  if (cached) { initFromData(cached, true); return; }

  fetch('/api/graph')
    .then(r => r.json())
    .then(d => { if (d.nodes) initFromData(d, false); else loadDemoData(); })
    .catch(() => loadDemoData());
}

function loadDemoData() {
  // Minimal demo — real data comes from Go pipeline via /ws or /api/graph
  addLog('no server — loading built-in demo data');
  // Demo data is injected by HTML via window.DEMO_DATA
  if (window.DEMO_DATA) initFromData(window.DEMO_DATA, false);
}

// ── BOOT ──────────────────────────────────────────────────────
function boot() {
  mainCanvas   = document.getElementById('main-canvas');
  mainCtx      = mainCanvas.getContext('2d');
  overlayCanvas = document.getElementById('overlay-canvas');
  overlayCtx   = overlayCanvas.getContext('2d');

  resizeMain();
  window.addEventListener('resize', () => {
    resizeMain(); resizeOverlay();
    if (mode === 'attr') resizeAttr();
  });

  attrCanvas = document.getElementById('attr-canvas');
  if (attrCanvas) {
    attrCtx = attrCanvas.getContext('2d');
    wireAttrEvents();
  }

  wireCanvasEvents();
  wireKeyboard();

  document.getElementById('search-input').addEventListener('input', e => doSearch(e.target.value));

  document.getElementById('wsdot').classList.remove('live');

  addLog('InfraMapper ready — shortcut keys: 1=MAP 2=2D 3=3D 4=ATTR R=reset Z=fit C=cache');

  connectWS();

  // Start in map mode
  setMode('map');
  renderLoop();
}

// ── ATTRIBUTION GRAPH SYSTEM ─────────────────────────────────
//
// Signal-centric view: built directly from the current scan data.
//
// Graph structure:
//   SIGNAL NODES  — large hub circles, one per unique signal value
//                   (favicon_hash, jarm, asn, header_hash, tls_issuer)
//                   Size = number of assets that share it
//                   Color = signal type color
//
//   ASSET NODES   — small satellite dots connected to their signal hubs
//                   Click → navigates back to that asset in MAP/2D/3D
//
//   EDGES         — signal hub ↔ asset (thin, colored by signal type)
//                   Hub-to-hub edges when assets appear in multiple clusters
//
// No API call needed — uses the nodes[] already in memory from the scan.
// ─────────────────────────────────────────────────────────────

const ATTR_SIGNAL_COLORS = {
  favicon_hash: '#e89c32',
  header_hash:  '#32a0e8',
  jarm:         '#8c32e8',
  asn:          '#32e39a',
  tls_issuer:   '#e83272',
};

let attrCanvas, attrCtx, attrW = 800, attrH = 600;

// attrN  = signal hub nodes + asset satellite nodes (mixed, isHub flag differentiates)
// attrE  = hub→asset edges + hub↔hub edges
let attrN = [], attrE = [];
let attrSel = null, attrHov = null;
let attrCamX = 0, attrCamY = 0, attrCamZ = 1.0;
let attrPanning = false, attrPanStart = {x:0,y:0}, attrPanOrigin = {x:0,y:0};
let attrDragging = null, attrDragOff = {x:0,y:0};
let attrLayoutStable = false;
let attrLoaded = false;

// Hub sim: strong repel between hubs, spring attraction to their satellites
const ATTR_SIM = { hubRepel: 18000, satRepel: 800, spring: 0.00025, restLen: 120, damp: 0.78, grav: 0.0015 };

// ─── Build graph from current scan's nodes[] ────────────────
//
// Signal hub = one node per unique (type, value) cluster signal.
// Satellite  = asset node connected to its hub(s).
// Hub↔hub edges = two hubs share the same asset (infrastructure overlap).
//
function buildAttrFromScan() {
  if (!nodes || !nodes.length) return false;

  // ── Step 1: group assets by their cluster signal ──────────
  // Use cluster ID + pivot as the signal key.
  // Fall back to explicit signal fields for unclustered assets.
  var hubMap = {};   // key → {type, value, label, color, assetIds:[]}

  function addToHub(key, type, value, label, assetId) {
    if (!value || value === '0' || value === 'AS0') return;
    var col = ATTR_SIGNAL_COLORS[type] || '#bfcfe3';
    if (!hubMap[key]) hubMap[key] = { key: key, type: type, value: value, label: label, color: col, assetIds: [] };
    if (hubMap[key].assetIds.indexOf(assetId) < 0) hubMap[key].assetIds.push(assetId);
  }

  nodes.forEach(function(n) {
    // Cluster-based signal (already computed by Go pipeline)
    if (n.cluster && n.pivot) {
      var label = n.cluster_label || n.cluster;
      addToHub('clust:' + n.cluster, n.pivot, n.cluster, label, n.id);
    }
    // Explicit signal fields — includes unclustered assets (e.g. raw IPs from FOFA)
    if (n.favicon_hash && n.favicon_hash !== '0') {
      addToHub('fav:' + n.favicon_hash, 'favicon_hash', n.favicon_hash, 'favicon:' + n.favicon_hash.slice(0,8), n.id);
    }
    if (n.header_hash && n.header_hash !== '0') {
      addToHub('hh:' + n.header_hash, 'header_hash', n.header_hash, 'header:' + n.header_hash.slice(0,8), n.id);
    }
    if (n.jarm && n.jarm.length > 8) {
      addToHub('jarm:' + n.jarm, 'jarm', n.jarm, 'jarm:' + n.jarm.slice(0,12) + '…', n.id);
    }
    if (n.asn && n.asn !== 'AS0') {
      addToHub('asn:' + n.asn, 'asn', n.asn, n.asn, n.id);
    }
  });

  // ── Step 2: discard single-asset hubs (not attribution) ───
  var hubs = Object.values(hubMap).filter(function(h) { return h.assetIds.length >= 2; });
  if (!hubs.length) return false;

  // ── Step 3: layout ─────────────────────────────────────────
  // Hubs form an inner ring; satellites orbit their primary hub.
  var cx = attrW / 2, cy = attrH / 2;
  var hubR = Math.min(attrW, attrH) * 0.22;
  attrN = [];
  attrE = [];

  // Hub nodes
  hubs.forEach(function(h, i) {
    var angle = (i / hubs.length) * Math.PI * 2;
    attrN.push({
      id:       'hub:' + h.key,
      isHub:    true,
      hubKey:   h.key,
      sigType:  h.type,
      sigValue: h.value,
      label:    h.label,
      color:    h.color,
      assetIds: h.assetIds,
      r:        Math.max(20, Math.min(44, 14 + h.assetIds.length * 2.2)),
      x:        cx + Math.cos(angle) * hubR,
      y:        cy + Math.sin(angle) * hubR,
      vx: 0, vy: 0,
      pulse: Math.random() * Math.PI * 2,
    });
  });

  // Asset satellite nodes — only assets that belong to at least one hub
  var assetInHub = {};
  hubs.forEach(function(h) {
    h.assetIds.forEach(function(id) { assetInHub[id] = true; });
  });

  var satMap = {}; // scanNodeId → attrN satellite node
  nodes.forEach(function(n) {
    if (!assetInHub[n.id]) return;
    // Find primary hub (first hub that contains this asset)
    var primaryHub = null;
    for (var hi = 0; hi < attrN.length; hi++) {
      if (attrN[hi].assetIds.indexOf(n.id) >= 0) { primaryHub = attrN[hi]; break; }
    }
    if (!primaryHub) return;
    var jitter = Math.random() * Math.PI * 2;
    var satR   = primaryHub.r + 40 + Math.random() * 40;
    var satNode = {
      id:        'sat:' + n.id,
      isSat:     true,
      scanId:    n.id,
      label:     n.host || n.ip || String(n.id),
      color:     n.color || '#bfcfe3',
      r:         4,
      x:         primaryHub.x + Math.cos(jitter) * satR,
      y:         primaryHub.y + Math.sin(jitter) * satR,
      vx: 0, vy: 0,
      pulse: Math.random() * Math.PI * 2,
    };
    attrN.push(satNode);
    satMap[n.id] = satNode;
  });

  // ── Step 4: edges ─────────────────────────────────────────
  // Hub → satellite edges
  hubs.forEach(function(h) {
    var hubNode = attrN.find(function(an) { return an.hubKey === h.key; });
    if (!hubNode) return;
    h.assetIds.forEach(function(assetId) {
      var sat = satMap[assetId];
      if (sat) {
        attrE.push({ from: hubNode, to: sat, type: 'hub-sat', color: h.color, strength: 0.6 });
      }
    });
  });

  // Hub ↔ hub edges — when they share an asset (infrastructure overlap)
  for (var i = 0; i < hubs.length; i++) {
    for (var j = i+1; j < hubs.length; j++) {
      var shared = hubs[i].assetIds.filter(function(id) {
        return hubs[j].assetIds.indexOf(id) >= 0;
      });
      if (shared.length > 0) {
        var hi = attrN.find(function(an) { return an.hubKey === hubs[i].key; });
        var hj = attrN.find(function(an) { return an.hubKey === hubs[j].key; });
        if (hi && hj) {
          var conf = Math.min(1, shared.length / Math.min(hubs[i].assetIds.length, hubs[j].assetIds.length));
          attrE.push({ from: hi, to: hj, type: 'hub-hub', color: '#c0d0e4', strength: conf, sharedCount: shared.length });
        }
      }
    }
  }

  attrLayoutStable = false;
  attrLoaded = true;

  var emptyEl = document.getElementById('attr-empty');
  if (emptyEl) emptyEl.style.display = 'none';

  var hubCount = hubs.length;
  var satCount = Object.keys(satMap).length;
  var overlapEdges = attrE.filter(function(e) { return e.type === 'hub-hub'; }).length;
  updateAttrHUD(hubCount, satCount, overlapEdges);
  addLog('attr: ' + hubCount + ' signal hubs · ' + satCount + ' assets · ' + overlapEdges + ' overlaps');
  return true;
}

function updateAttrHUD(hubs, assets, overlaps) {
  var el = document.getElementById('attr-info');
  if (!el) return;
  el.textContent = hubs + ' signal hubs  ·  ' + assets + ' assets  ·  ' + overlaps + ' infrastructure overlaps';
  el.style.display = 'block';
}

// ─── Force simulation ────────────────────────────────────────
function attrSimStep() {
  if (attrLayoutStable) return;

  var hubs = attrN.filter(function(n) { return n.isHub; });
  var sats = attrN.filter(function(n) { return n.isSat; });
  var maxV = 0;

  // Hub↔hub repulsion
  for (var i = 0; i < hubs.length; i++) {
    for (var j = i+1; j < hubs.length; j++) {
      var a = hubs[i], b = hubs[j];
      var dx = b.x-a.x, dy = b.y-a.y, d2 = dx*dx+dy*dy+1, d = Math.sqrt(d2);
      var f = ATTR_SIM.hubRepel / d2;
      a.vx -= f*dx/d; a.vy -= f*dy/d;
      b.vx += f*dx/d; b.vy += f*dy/d;
    }
  }

  // Satellite↔satellite soft repulsion (within same hub cluster)
  for (var i = 0; i < sats.length; i++) {
    for (var j = i+1; j < sats.length; j++) {
      var a = sats[i], b = sats[j];
      var dx = b.x-a.x, dy = b.y-a.y, d2 = dx*dx+dy*dy+1, d = Math.sqrt(d2);
      if (d < 30) {
        var f = ATTR_SIM.satRepel / d2;
        a.vx -= f*dx/d; a.vy -= f*dy/d;
        b.vx += f*dx/d; b.vy += f*dy/d;
      }
    }
  }

  // Spring forces along edges
  attrE.forEach(function(e) {
    var a = e.from, b = e.to;
    var dx = b.x-a.x, dy = b.y-a.y, d = Math.sqrt(dx*dx+dy*dy)+1;
    var rest = e.type === 'hub-hub' ? ATTR_SIM.restLen * 2.2 : ATTR_SIM.restLen;
    var f = ATTR_SIM.spring * (d - rest) * (e.strength || 0.5);
    // Only satellites move toward hubs; hubs are moved by hub-hub springs
    if (e.type === 'hub-sat') {
      b.vx += f*dx/d; b.vy += f*dy/d;
      a.vx += (f*dx/d) * 0.15; a.vy += (f*dy/d) * 0.15;
    } else {
      a.vx += f*dx/d; a.vy += f*dy/d;
      b.vx -= f*dx/d; b.vy -= f*dy/d;
    }
  });

  // Integrate
  attrN.forEach(function(n) {
    if (n === attrDragging) return;
    n.vx += (attrW/2 - n.x) * ATTR_SIM.grav;
    n.vy += (attrH/2 - n.y) * ATTR_SIM.grav;
    n.vx *= ATTR_SIM.damp; n.vy *= ATTR_SIM.damp;
    n.x += n.vx; n.y += n.vy;
    var pad = n.r + 8;
    n.x = Math.max(pad, Math.min(attrW - pad, n.x));
    n.y = Math.max(pad, Math.min(attrH - pad, n.y));
    maxV = Math.max(maxV, Math.abs(n.vx), Math.abs(n.vy));
  });

  if (maxV < 0.07) attrLayoutStable = true;
}

// ─── Drawing ─────────────────────────────────────────────────
function drawAttr() {
  if (!attrCtx || !attrCanvas) return;
  attrCtx.save();
  attrCtx.clearRect(0, 0, attrW, attrH);
  attrCtx.translate(attrCamX, attrCamY);
  attrCtx.scale(attrCamZ, attrCamZ);

  if (!attrN.length) { attrCtx.restore(); return; }

  attrSimStep();

  // Draw hub-hub overlap edges first (behind everything)
  attrE.forEach(function(e) {
    if (e.type !== 'hub-hub') return;
    var conf = e.strength || 0.4;
    var al   = Math.round(conf * 160).toString(16).padStart(2,'0');
    attrCtx.strokeStyle = e.color + al;
    attrCtx.lineWidth   = Math.max(1.5, conf * 5);
    attrCtx.lineCap     = 'round';
    attrCtx.setLineDash([6, 5]);
    attrCtx.beginPath();
    attrCtx.moveTo(e.from.x, e.from.y);
    attrCtx.lineTo(e.to.x,   e.to.y);
    attrCtx.stroke();
    attrCtx.setLineDash([]);
    // Shared-count badge at midpoint
    var mx = (e.from.x + e.to.x) / 2, my = (e.from.y + e.to.y) / 2;
    attrCtx.font      = '8px JetBrains Mono, monospace';
    attrCtx.fillStyle = '#c0d0e4aa';
    attrCtx.textAlign = 'center';
    attrCtx.fillText(e.sharedCount + ' shared', mx, my - 4);
    attrCtx.textAlign = 'left';
  });

  // Draw hub→satellite edges
  attrE.forEach(function(e) {
    if (e.type !== 'hub-sat') return;
    var isSel = e.to === attrSel || e.from === attrSel;
    attrCtx.strokeStyle = e.color + (isSel ? 'bb' : '38');
    attrCtx.lineWidth   = isSel ? 1.5 : 0.8;
    attrCtx.lineCap     = 'round';
    attrCtx.beginPath();
    attrCtx.moveTo(e.from.x, e.from.y);
    attrCtx.lineTo(e.to.x,   e.to.y);
    attrCtx.stroke();
  });

  // Draw satellite asset dots
  attrN.forEach(function(n) {
    if (!n.isSat) return;
    n.pulse = (n.pulse || 0) + 0.04;
    var isSel = n === attrSel, isHov = n === attrHov;
    var pr = n.r + (isSel ? 3 : 0);
    attrCtx.fillStyle = isSel ? n.color : (isHov ? n.color + 'ee' : n.color + 'aa');
    attrCtx.beginPath(); attrCtx.arc(n.x, n.y, pr, 0, Math.PI*2); attrCtx.fill();
    if (isSel || isHov) {
      attrCtx.strokeStyle = n.color; attrCtx.lineWidth = 1.2;
      attrCtx.stroke();
      // Label
      attrCtx.font      = '9px JetBrains Mono, monospace';
      attrCtx.fillStyle = '#ffffff';
      attrCtx.textAlign = 'center';
      attrCtx.fillText(n.label, n.x, n.y - pr - 5);
      attrCtx.textAlign = 'left';
    }
  });

  // Draw signal hub nodes (on top)
  attrN.forEach(function(n) {
    if (!n.isHub) return;
    n.pulse = (n.pulse || 0) + 0.018;
    var isSel = n === attrSel, isHov = n === attrHov;
    var pr    = n.r + Math.sin(n.pulse) * (isSel ? 3 : 1);

    // Outer glow
    var g = attrCtx.createRadialGradient(n.x, n.y, 0, n.x, n.y, pr * 3.2);
    g.addColorStop(0, n.color + (isSel ? '55' : '22'));
    g.addColorStop(1, n.color + '00');
    attrCtx.fillStyle = g;
    attrCtx.beginPath(); attrCtx.arc(n.x, n.y, pr * 3.2, 0, Math.PI*2); attrCtx.fill();

    // Circle body
    attrCtx.fillStyle   = isSel ? n.color : (isHov ? n.color + 'ee' : n.color + 'bb');
    attrCtx.beginPath(); attrCtx.arc(n.x, n.y, pr, 0, Math.PI*2); attrCtx.fill();
    attrCtx.strokeStyle = n.color + (isSel ? 'ff' : 'cc');
    attrCtx.lineWidth   = isSel ? 2.5 : 1.8;
    attrCtx.stroke();

    // Asset count badge (centre)
    attrCtx.font      = 'bold 11px JetBrains Mono, monospace';
    attrCtx.fillStyle = isSel ? '#06090f' : '#ffffff';
    attrCtx.textAlign = 'center';
    attrCtx.fillText(n.assetIds.length, n.x, n.y + 4);

    // Signal type label (top)
    attrCtx.font      = '8px JetBrains Mono, monospace';
    attrCtx.fillStyle = n.color + (isSel ? 'ff' : 'bb');
    attrCtx.fillText(n.sigType, n.x, n.y - pr - 16);

    // Cluster / signal label (bottom)
    attrCtx.font      = '9px JetBrains Mono, monospace';
    attrCtx.fillStyle = isSel ? '#ffffff' : (isHov ? '#c0d0e4' : '#6a8aab');
    var lbl = n.label.length > 22 ? n.label.slice(0, 20) + '…' : n.label;
    attrCtx.fillText(lbl, n.x, n.y + pr + 14);
    attrCtx.textAlign = 'left';

    // Selected dashed ring
    if (isSel) {
      attrCtx.strokeStyle = n.color + '66';
      attrCtx.lineWidth   = 1;
      attrCtx.setLineDash([3,4]);
      attrCtx.beginPath(); attrCtx.arc(n.x, n.y, pr + 9, 0, Math.PI*2); attrCtx.stroke();
      attrCtx.setLineDash([]);
    }
  });

  attrCtx.restore();
}

// ─── Node card for ATTR mode ─────────────────────────────────
function selectAttrNode(an) {
  attrSel = an;
  var card = document.getElementById('node-card');
  if (!card) return;
  var cachePanel = document.getElementById('cache-panel');

  if (!an) {
    card.classList.add('hidden');
    if (cachePanel) cachePanel.style.display = '';
    return;
  }
  if (cachePanel) cachePanel.style.display = 'none';

  var bar = document.getElementById('nc-bar');

  if (an.isHub) {
    // ── Signal hub card ────────────────────────────────────
    if (bar) bar.style.background = an.color;

    ncTxt('nc-title',    an.label);
    ncTxt('nc-subtitle', an.sigType + '  ·  ' + an.assetIds.length + ' assets');

    ncSet('nc-status',  an.sigValue.slice(0, 24) + (an.sigValue.length > 24 ? '…' : ''), '');
    ncSet('nc-port',    '—', '');
    ncSet('nc-pivot',   an.sigType, '');
    ncSet('nc-cluster', '—', '');
    ncSet('nc-asn',     '—', '');
    ncSet('nc-jarm',    an.sigType === 'jarm' ? an.sigValue.slice(0,24) + '…' : '—', '');
    ncSet('nc-country', '—', '');
    ncSet('nc-geo',     '—', '');
    ncSet('nc-source',  'signal hub', '');

    var bdg = document.getElementById('nc-badges');
    if (bdg) {
      bdg.innerHTML = '<span class="nc-badge" style="color:' + an.color + ';border-color:' + an.color + '44">' + an.sigType + '</span>';
    }

    // Relations: the asset nodes attached to this hub
    var relsEl = document.getElementById('nc-rels');
    if (relsEl) {
      var members = nodes.filter(function(n) { return an.assetIds.indexOf(n.id) >= 0; });
      members.sort(function(a,b) { return (b.score||0) - (a.score||0); });
      if (!members.length) {
        relsEl.innerHTML = '<div class="nc-rel-empty">no assets</div>';
      } else {
        relsEl.innerHTML = '<div class="nc-rel-hdr">assets sharing this signal (' + members.length + ')</div>' +
          members.map(function(m) {
            return '<div class="nc-rel-item" onclick="jumpToAsset(' + m.id + ')">' +
              '<div class="nc-rel-dot" style="background:' + m.color + '"></div>' +
              '<span class="nc-rel-name">' + (m.host || m.ip || String(m.id)) + '</span>' +
              '<span class="nc-rel-via">' + (m.country || m.asn || '') + '</span>' +
              '</div>';
          }).join('');
      }
    }

  } else if (an.isSat) {
    // ── Asset satellite card: proxy to the scan node card ──
    var scanNode = nodes.find(function(n) { return n.id === an.scanId; });
    if (scanNode) { selectNode(scanNode); return; }
    if (bar) bar.style.background = an.color;
    ncTxt('nc-title',    an.label);
    ncTxt('nc-subtitle', 'asset');
    ncSet('nc-source',   'scan asset', '');
  }

  card.classList.remove('hidden');
}

// Jump to asset: switch to 2D mode and highlight the node
function jumpToAsset(id) {
  selectAttrNode(null);
  setMode('2d');
  var n = nodes.find(function(x) { return x.id === id; });
  if (n) { setTimeout(function() { selectNode(n); }, 80); }
}

function pickAttrNode(id) {
  var n = attrN.find(function(x) { return x.id === id; });
  if (n) selectAttrNode(n);
}

function getAttrNodeAt(mx, my) {
  var wx = (mx - attrCamX) / attrCamZ;
  var wy = (my - attrCamY) / attrCamZ;
  var best = null, bestD = Infinity;
  attrN.forEach(function(n) {
    var d = Math.hypot(n.x - wx, n.y - wy);
    if (d < n.r + 10 && d < bestD) { bestD = d; best = n; }
  });
  return best;
}

function resizeAttr() {
  if (!attrCanvas) return;
  attrW = attrCanvas.clientWidth  || 800;
  attrH = attrCanvas.clientHeight || 600;
  attrCanvas.width  = attrW * devicePixelRatio;
  attrCanvas.height = attrH * devicePixelRatio;
  attrCtx.setTransform(devicePixelRatio, 0, 0, devicePixelRatio, 0, 0);
  attrLayoutStable = false;
}

function wireAttrEvents() {
  if (!attrCanvas) return;

  attrCanvas.addEventListener('mousemove', function(e) {
    if (mode !== 'attr') return;
    var r  = attrCanvas.getBoundingClientRect();
    var mx = e.clientX - r.left, my = e.clientY - r.top;
    if (attrDragging) {
      var wx = (mx - attrCamX) / attrCamZ, wy = (my - attrCamY) / attrCamZ;
      attrDragging.x = wx + attrDragOff.x;
      attrDragging.y = wy + attrDragOff.y;
      attrDragging.vx = 0; attrDragging.vy = 0;
      attrLayoutStable = false;
      return;
    }
    if (attrPanning) {
      attrCamX = attrPanOrigin.x + (mx - attrPanStart.x);
      attrCamY = attrPanOrigin.y + (my - attrPanStart.y);
      return;
    }
    attrHov = getAttrNodeAt(mx, my);
    attrCanvas.style.cursor = attrHov ? 'pointer' : 'default';
  });

  attrCanvas.addEventListener('mousedown', function(e) {
    if (mode !== 'attr') return;
    var r  = attrCanvas.getBoundingClientRect();
    var mx = e.clientX - r.left, my = e.clientY - r.top;
    var n  = getAttrNodeAt(mx, my);
    if (n) {
      var wx = (mx - attrCamX) / attrCamZ, wy = (my - attrCamY) / attrCamZ;
      attrDragging = n; attrDragOff = { x: n.x - wx, y: n.y - wy };
    } else {
      attrPanning = true;
      attrPanStart  = { x: mx, y: my };
      attrPanOrigin = { x: attrCamX, y: attrCamY };
    }
  });

  attrCanvas.addEventListener('mouseup', function(e) {
    if (mode !== 'attr') return;
    var r  = attrCanvas.getBoundingClientRect();
    var mx = e.clientX - r.left, my = e.clientY - r.top;
    if (attrDragging) {
      var wx = (mx - attrCamX) / attrCamZ, wy = (my - attrCamY) / attrCamZ;
      if (Math.hypot(attrDragging.x - (wx + attrDragOff.x), attrDragging.y - (wy + attrDragOff.y)) < 4) {
        selectAttrNode(attrDragging);
      }
    } else if (!attrPanning || Math.hypot(mx - attrPanStart.x, my - attrPanStart.y) < 4) {
      var clicked = getAttrNodeAt(mx, my);
      selectAttrNode(clicked || null);
    }
    attrDragging = null; attrPanning = false;
  });

  attrCanvas.addEventListener('mouseleave', function() { attrDragging = null; attrPanning = false; });

  attrCanvas.addEventListener('wheel', function(e) {
    if (mode !== 'attr') return;
    e.preventDefault();
    var r   = attrCanvas.getBoundingClientRect();
    var mx  = e.clientX - r.left, my = e.clientY - r.top;
    var fac = e.deltaY < 0 ? 1.12 : 0.89;
    var nz  = Math.max(0.2, Math.min(4.0, attrCamZ * fac));
    attrCamX = mx - (mx - attrCamX) * nz / attrCamZ;
    attrCamY = my - (my - attrCamY) * nz / attrCamZ;
    attrCamZ = nz;
  }, { passive: false });
}

// Export globals needed by HTML onclick attributes
window.setMode       = setMode;
window.toggleFilter  = toggleFilter;
window.toggleOrphans = toggleOrphans;
window.resetLayout   = resetLayout;
window.zoomFit       = zoomFit;
window.clearCache    = clearCache;
window.exportPNG     = exportPNG;
window.pickNode      = pickNode;
window.pickAttrNode  = pickAttrNode;

