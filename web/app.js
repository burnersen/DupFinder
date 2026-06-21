// DupFinder — Required Notice: Copyright (c) 2026 burnersen — DupFinder
// Licensed under the PolyForm Noncommercial License 1.0.0 (non-commercial use only).
// Full terms: LICENSE.md · https://polyformproject.org/licenses/noncommercial/1.0.0
'use strict';

let currentState = null;
let lastStateHash = '';
let focusedClusterIdx = 0;
let pendingActions = {};

let pollTimer = null;
let progressTimer = null;

// ============================================================================
// Helpers
// ============================================================================

function formatBytes(b) {
  if (!b) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = b, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(i === 0 ? 0 : 2) + ' ' + units[i];
}

function formatDuration(s) {
  if (!s || !isFinite(s) || s <= 0) return '–';
  s = Math.round(s);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  return h > 0
    ? `${h}h${String(m).padStart(2, '0')}m${String(sec).padStart(2, '0')}s`
    : `${m}m${String(sec).padStart(2, '0')}s`;
}

function showToast(msg, type = 'info', timeout = 3000) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = `toast ${type}`;
  el.classList.remove('hidden');
  clearTimeout(showToast._t);
  showToast._t = setTimeout(() => el.classList.add('hidden'), timeout);
}

async function api(path, opts = {}) {
  // Bei mutierenden Requests immer application/json setzen – passend zum
  // serverseitigen CSRF-Schutz (requireJSON). Erzwingt einen CORS-Preflight,
  // den der Browser für fremde Websites blockiert.
  const method = (opts.method || 'GET').toUpperCase();
  if (method !== 'GET' && method !== 'HEAD') {
    opts.headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  }
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

function hashState(s) {
  if (!s) return '';
  const parts = [
    s.pendingCount, s.decidedCount, s.ignoredCount, s.totalSavings,
    s.scanning ? 1 : 0,
    (s.roots || []).join('|'),
  ];
  for (const cl of (s.clusters || [])) {
    parts.push(cl.id, cl.decided ? 1 : 0, cl.videos.length);
    for (const v of cl.videos) parts.push(v.path, v.size);
  }
  return parts.join(';');
}

// ============================================================================
// Render
// ============================================================================

function findBestPerColumn(videos) {
  const codecRank = c => {
    c = (c || '').toLowerCase();
    if (c.includes('av1')) return 3;
    if (c.includes('hevc') || c.includes('h265')) return 2;
    if (c.includes('h264') || c.includes('avc')) return 1;
    return 0;
  };
  let bestRes = -1, bestResIdx = -1;
  let bestBr = -1, bestBrIdx = -1;
  let bestCodec = -1, bestCodecIdx = -1;
  videos.forEach((v, i) => {
    const res = v.width * v.height;
    if (res > bestRes) { bestRes = res; bestResIdx = i; }
    if (v.bitrateKbps > bestBr) { bestBr = v.bitrateKbps; bestBrIdx = i; }
    const cr = codecRank(v.codec);
    if (cr > bestCodec) { bestCodec = cr; bestCodecIdx = i; }
  });
  return { resolution: bestResIdx, bitrate: bestBrIdx, codec: bestCodecIdx };
}

function defaultActions(cluster) {
  const actions = {};
  cluster.videos.forEach((v, i) => {
    actions[v.path] = (i === 0) ? 'keep' : 'delete';
  });
  return actions;
}

function getActionsForCluster(cluster) {
  if (!pendingActions[cluster.id]) {
    pendingActions[cluster.id] = defaultActions(cluster);
  }
  return pendingActions[cluster.id];
}

// Helfer für /api/play und /api/reveal. Stoppt das Bubble-Up des Klicks,
// damit nicht aus Versehen die Action-Buttons (keep/delete) getriggert werden.
async function callPathAction(endpoint, path, ev) {
  if (ev) {
    ev.preventDefault();
    ev.stopPropagation();
  }
  try {
    await api(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path }),
    });
  } catch (e) {
    showToast('Fehler: ' + e.message, 'error');
  }
}

function renderCluster(cluster, idx) {
  const div = document.createElement('div');
  div.className = 'cluster';
  div.dataset.clusterId = cluster.id;
  div.dataset.idx = idx;
  if (cluster.decided) div.classList.add('decided');

  const best = findBestPerColumn(cluster.videos);

  const header = document.createElement('div');
  header.className = 'cluster-header';
  header.innerHTML = `
    <span class="cluster-id">#${cluster.id}</span>
    <span class="cluster-meta">${cluster.videos.length} Dateien · gesamt ${formatBytes(cluster.totalSize)}</span>
    <span class="cluster-savings">↓ ${formatBytes(cluster.savings)} sparbar</span>
  `;
  div.appendChild(header);

  if (cluster.decided) {
    const sum = document.createElement('div');
    sum.className = 'decided-summary';
    const counts = { keep: 0, delete: 0, ignore: 0 };
    cluster.decisions.forEach(d => counts[d.action] = (counts[d.action] || 0) + 1);
    sum.innerHTML = `
      <span>✓ Entschieden: ${counts.keep || 0} behalten · ${counts.delete || 0} gelöscht · ${counts.ignore || 0} ignoriert</span>
      <button class="btn-undo" data-cluster-id="${cluster.id}">↶ Rückgängig</button>
    `;
    div.appendChild(sum);
    return div;
  }

  const body = document.createElement('div');
  body.className = 'cluster-body';

  const row = document.createElement('div');
  row.className = 'videos-row';

  const actions = getActionsForCluster(cluster);

  cluster.videos.forEach((v, i) => {
    const card = document.createElement('div');
    card.className = `video-card action-${actions[v.path]}`;
    card.dataset.path = v.path;
    card.dataset.videoIdx = i;

    const num = document.createElement('div');
    num.className = 'video-num';
    num.textContent = i + 1;
    card.appendChild(num);

    const thumbs = document.createElement('div');
    thumbs.className = 'video-thumbs';
    (v.thumbIds || []).forEach(tid => {
      const img = document.createElement('img');
      img.src = `/api/thumb?h=${v.pathHash}&i=${tid}`;
      img.alt = '';
      img.loading = 'lazy';
      img.addEventListener('error', () => {
        img.style.visibility = 'hidden';
      });
      thumbs.appendChild(img);
    });
    card.appendChild(thumbs);

    const meta = document.createElement('div');
    meta.className = 'video-meta';
    const fileName = document.createElement('div');
    fileName.className = 'filename';
    fileName.textContent = v.fileName;
    fileName.title = v.path;
    meta.appendChild(fileName);

    const grid = document.createElement('div');
    grid.className = 'meta-grid';
    const cells = [
      ['Größe', formatBytes(v.size), false],
      ['Bitrate', `${v.bitrateKbps} kbps`, i === best.bitrate],
      ['Auflösung', `${v.width}×${v.height}`, i === best.resolution],
      ['Codec', v.codec || '–', i === best.codec],
      ['Dauer', formatDuration(v.durationSec), false],
    ];
    cells.forEach(([k, val, isBest]) => {
      const kEl = document.createElement('span');
      kEl.className = 'k'; kEl.textContent = k;
      const vEl = document.createElement('span');
      vEl.className = 'v' + (isBest ? ' best' : '');
      vEl.textContent = val;
      grid.appendChild(kEl);
      grid.appendChild(vEl);
    });
    meta.appendChild(grid);

    const path = document.createElement('div');
    path.className = 'path';
    path.textContent = v.path;
    meta.appendChild(path);
    card.appendChild(meta);

    // ----- video-tools: Abspielen / Ordner öffnen -----
    // Sitzt zwischen meta und video-actions.
    const tools = document.createElement('div');
    tools.className = 'video-tools';

    const btnPlay = document.createElement('button');
    btnPlay.className = 'btn-play';
    btnPlay.type = 'button';
    btnPlay.textContent = '▶ Abspielen';
    btnPlay.title = 'Mit dem Standard-Player öffnen';
    btnPlay.addEventListener('click', (ev) => callPathAction('/api/play', v.path, ev));
    tools.appendChild(btnPlay);

    const btnReveal = document.createElement('button');
    btnReveal.className = 'btn-reveal';
    btnReveal.type = 'button';
    btnReveal.textContent = '📁 Ordner';
    btnReveal.title = 'Im Explorer markieren';
    btnReveal.addEventListener('click', (ev) => callPathAction('/api/reveal', v.path, ev));
    tools.appendChild(btnReveal);

    card.appendChild(tools);
    // ----- /video-tools -----

    const act = document.createElement('div');
    act.className = 'video-actions';
    ['keep', 'delete', 'ignore'].forEach(a => {
      const b = document.createElement('button');
      b.textContent = { keep: '✓ Behalten', delete: '✗ Löschen', ignore: '○ Ignorieren' }[a];
      b.dataset.action = a;
      if (actions[v.path] === a) b.classList.add('active', a);
      b.addEventListener('click', () => {
        actions[v.path] = a;
        rerenderClusterInPlace(cluster.id);
      });
      act.appendChild(b);
    });
    card.appendChild(act);

    row.appendChild(card);
  });

  body.appendChild(row);
  div.appendChild(body);

  const footer = document.createElement('div');
  footer.className = 'cluster-footer';
  const counts = { keep: 0, delete: 0, ignore: 0 };
  let bytesToDelete = 0;
  cluster.videos.forEach(v => {
    const a = actions[v.path];
    counts[a]++;
    if (a === 'delete') bytesToDelete += v.size;
  });
  footer.innerHTML = `
    <div class="summary">
      <strong>${counts.keep}</strong> behalten ·
      <strong>${counts.delete}</strong> löschen (${formatBytes(bytesToDelete)}) ·
      <strong>${counts.ignore}</strong> ignorieren
    </div>
    <div class="buttons">
      <button class="btn-skip">Skip</button>
      <button class="btn-confirm primary">Bestätigen ⏎</button>
    </div>
  `;
  div.appendChild(footer);

  return div;
}

function rerenderClusterInPlace(clusterId) {
  const old = document.querySelector(`.cluster[data-cluster-id="${clusterId}"]`);
  if (!old || !currentState) return;
  const idx = parseInt(old.dataset.idx, 10);
  const cl = currentState.clusters.find(c => c.id === clusterId);
  if (!cl) return;
  const fresh = renderCluster(cl, idx);
  if (idx === focusedClusterIdx) fresh.classList.add('focused');
  bindClusterEvents(fresh);
  old.replaceWith(fresh);
}

function bindClusterEvents(el) {
  el.querySelectorAll('.btn-confirm').forEach(b => {
    b.addEventListener('click', e => {
      const cl = e.target.closest('.cluster');
      confirmCluster(cl.dataset.clusterId);
    });
  });
  el.querySelectorAll('.btn-skip').forEach(b => {
    b.addEventListener('click', e => {
      const cl = e.target.closest('.cluster');
      skipCluster(cl.dataset.clusterId);
    });
  });
  el.querySelectorAll('.btn-undo').forEach(b => {
    b.addEventListener('click', e => {
      undoCluster(e.target.dataset.clusterId);
    });
  });
}

function renderStatsAndRoots() {
  if (!currentState) return;
  document.getElementById('pendingCount').textContent = currentState.pendingCount;
  document.getElementById('decidedCount').textContent = currentState.decidedCount;
  document.getElementById('ignoredCount').textContent = currentState.ignoredCount;
  document.getElementById('totalSavings').textContent = formatBytes(currentState.totalSavings);

  const reStat = document.getElementById('readErrorsStat');
  if (reStat) {
    if (currentState.readErrors > 0) {
      document.getElementById('readErrors').textContent = currentState.readErrors;
      reStat.classList.remove('hidden');
    } else {
      reStat.classList.add('hidden');
    }
  }

  const rootsBar = document.getElementById('rootsBar');
  const roots = currentState.roots || [];
  // Sicher gegen HTML im Pfadnamen: keine innerHTML-Konkatenation, sondern
  // textContent pro Tag (verhindert DOM-XSS bei Pfaden wie <img onerror=...>).
  rootsBar.replaceChildren();
  rootsBar.append('Suchpfade: ');
  roots.forEach(r => {
    const span = document.createElement('span');
    span.className = 'root-tag';
    span.textContent = r;
    rootsBar.appendChild(span);
  });
}

function renderAll() {
  const container = document.getElementById('clustersContainer');
  if (!currentState) return;
  renderStatsAndRoots();

  if (!currentState.clusters || currentState.clusters.length === 0) {
    container.innerHTML = '';
    const empty = document.createElement('div');
    empty.className = 'empty-state';
    empty.innerHTML = currentState.scanning
      ? `<h2>Scan läuft…</h2><p>Bitte warten, bis Cluster gefunden werden.</p>`
      : `<h2>Keine Duplikate gefunden 🎉</h2><p>Alle deine Videos scheinen einzigartig zu sein.</p>`;
    container.appendChild(empty);
    return;
  }

  container.innerHTML = '';

  // Trenne pending und decided. Die idx-Werte beziehen sich weiter auf
  // currentState.clusters (für Tastatur-Navigation und focusedClusterIdx).
  const pending = [];
  const decided = [];
  currentState.clusters.forEach((cl, i) => {
    (cl.decided ? decided : pending).push({ cl, i });
  });

  pending.forEach(({ cl, i }) => {
    const el = renderCluster(cl, i);
    if (i === focusedClusterIdx) el.classList.add('focused');
    bindClusterEvents(el);
    container.appendChild(el);
  });

  if (decided.length > 0) {
    const drawer = document.createElement('details');
    drawer.className = 'decided-drawer';
    // Zustand merken (offen/zu) zwischen Re-Renders
    drawer.open = renderAll._drawerOpen === true;
    drawer.addEventListener('toggle', () => {
      renderAll._drawerOpen = drawer.open;
    });

    const summary = document.createElement('summary');
    summary.className = 'decided-drawer-summary';
    summary.innerHTML = `
      <span class="drawer-icon">✓</span>
      <span class="drawer-title">Bereits entschieden</span>
      <span class="drawer-count">${decided.length}</span>
    `;
    drawer.appendChild(summary);

    const drawerBody = document.createElement('div');
    drawerBody.className = 'decided-drawer-body';
    decided.forEach(({ cl, i }) => {
      const el = renderCluster(cl, i);
      if (i === focusedClusterIdx) el.classList.add('focused');
      bindClusterEvents(el);
      drawerBody.appendChild(el);
    });
    drawer.appendChild(drawerBody);
    container.appendChild(drawer);
  }
}


// ============================================================================
// Aktionen
// ============================================================================

async function confirmCluster(clusterId) {
  const cl = currentState.clusters.find(c => c.id === clusterId);
  if (!cl) return;
  // Button-Fokus loswerden, damit kein Update geblockt wird
  if (document.activeElement && document.activeElement.blur) {
    document.activeElement.blur();
  }

  const actions = getActionsForCluster(cl);
  const decisions = cl.videos.map(v => ({ path: v.path, action: actions[v.path] }));

  const keepCount = decisions.filter(d => d.action === 'keep').length;
  const deleteCount = decisions.filter(d => d.action === 'delete').length;

  // Warnung nur anzeigen, wenn gelöscht wird, aber nichts behalten wird!
  if (keepCount === 0 && deleteCount > 0) {
    if (!confirm('Du hast keine Datei zum Behalten markiert, löschst aber Dateien! Wirklich fortfahren?')) return;
  }

  try {
    const r = await api('/api/decide', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clusterId, decisions }),
    });
    if (r.errors && r.errors.length) {
      showToast(`Bestätigt, aber ${r.errors.length} Fehler aufgetreten.`, 'error', 5000);
    } else {
      showToast(`✓ ${deleteCount} Datei(en) in Papierkorb verschoben.`, 'success');
    }
    delete pendingActions[clusterId];
    await refresh(true);
    advanceFocus();
  } catch (e) {
    showToast('Fehler: ' + e.message, 'error');
  }
}

async function skipCluster(clusterId) {
  const cl = currentState.clusters.find(c => c.id === clusterId);
  if (!cl) return;
  pendingActions[clusterId] = {};
  cl.videos.forEach(v => pendingActions[clusterId][v.path] = 'keep');
  await confirmCluster(clusterId);
}

async function undoCluster(clusterId) {
  try {
    const r = await api('/api/undo', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ clusterId }),
    });
    showToast(r.note || 'Rückgängig gemacht.', 'info', 4000);
    delete pendingActions[clusterId];
    await refresh(true);
  } catch (e) {
    showToast('Fehler: ' + e.message, 'error');
  }
}

function advanceFocus() {
  if (!currentState || !currentState.clusters.length) return;
  for (let i = focusedClusterIdx + 1; i < currentState.clusters.length; i++) {
    if (!currentState.clusters[i].decided) {
      focusedClusterIdx = i;
      updateFocusVisuals();
      return;
    }
  }
  for (let i = 0; i < currentState.clusters.length; i++) {
    if (!currentState.clusters[i].decided) {
      focusedClusterIdx = i;
      updateFocusVisuals();
      return;
    }
  }
}

function updateFocusVisuals() {
  document.querySelectorAll('.cluster.focused').forEach(el => el.classList.remove('focused'));
  // Den aktiven Cluster über sein stabiles data-idx finden, NICHT über die
  // Position in der DOM-Liste: Entschiedene Cluster wandern in die Schublade
  // "Bereits entschieden" ans Ende, dadurch passt die Listen-Position nicht mehr
  // zum Index aus currentState.clusters. Über das data-idx (= genau dieser Index)
  // treffen Markierung und Zentrieren immer den richtigen Cluster.
  const el = document.querySelector('.cluster[data-idx="' + focusedClusterIdx + '"]');
  if (el) {
    el.classList.add('focused');
    el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }
}

// ============================================================================
// Tastenkürzel
// ============================================================================

document.addEventListener('keydown', async (e) => {
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;

  const cluster = currentState && currentState.clusters[focusedClusterIdx];
  if (!cluster) return;

  if (e.key >= '1' && e.key <= '9') {
    e.preventDefault();
    const idx = parseInt(e.key, 10) - 1;
    if (idx >= cluster.videos.length) return;
    const v = cluster.videos[idx];
    const actions = getActionsForCluster(cluster);
    if (e.shiftKey) {
      actions[v.path] = 'ignore';
    } else {
      actions[v.path] = (actions[v.path] === 'keep') ? 'delete' : 'keep';
    }
    rerenderClusterInPlace(cluster.id);
    return;
  }

  switch (e.key) {
    case 'Enter':
      e.preventDefault();
      if (!cluster.decided) confirmCluster(cluster.id);
      break;
    case 's': case 'S':
      e.preventDefault();
      if (!cluster.decided) skipCluster(cluster.id);
      break;
    case 'u': case 'U':
      e.preventDefault();
      for (let i = focusedClusterIdx; i >= 0; i--) {
        if (currentState.clusters[i].decided) {
          undoCluster(currentState.clusters[i].id);
          focusedClusterIdx = i;
          return;
        }
      }
      showToast('Nichts zum Rückgängigmachen.', 'info');
      break;
    case 'ArrowDown':
      e.preventDefault();
      if (focusedClusterIdx < currentState.clusters.length - 1) {
        focusedClusterIdx++;
        updateFocusVisuals();
      }
      break;
    case 'ArrowUp':
      e.preventDefault();
      if (focusedClusterIdx > 0) {
        focusedClusterIdx--;
        updateFocusVisuals();
      }
      break;
    case '?':
      e.preventDefault();
      document.getElementById('helpPanel').classList.toggle('hidden');
      break;
  }
});

// ============================================================================
// Topbar-Buttons
// ============================================================================

document.getElementById('btnHelp').addEventListener('click', () => {
  document.getElementById('helpPanel').classList.toggle('hidden');
});

document.getElementById('btnRescan').addEventListener('click', async () => {
  try {
    await api('/api/rescan', { method: 'POST' });
    showToast('Rescan gestartet.', 'info');
    setTimeout(startPolling, 500);
  } catch (e) {
    showToast('Fehler: ' + e.message, 'error');
  }
});

document.getElementById('btnAddRoot').addEventListener('click', () => {
  const dlg = document.getElementById('addRootDialog');
  document.getElementById('addRootInput').value = '';
  dlg.showModal();
});

document.getElementById('addRootDialog').addEventListener('close', async (e) => {
  const dlg = e.target;
  if (dlg.returnValue !== 'ok') return;
  const path = document.getElementById('addRootInput').value.trim();
  if (!path) return;
  try {
    await api('/api/add-root', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path }),
    });
    showToast('Pfad hinzugefügt. Starte Rescan…', 'success');
    await api('/api/rescan', { method: 'POST' });
    setTimeout(startPolling, 500);
  } catch (err) {
    showToast('Fehler: ' + err.message, 'error');
  }
});

document.getElementById('btnShutdown').addEventListener('click', async () => {
  if (!confirm('DupFinder beenden? Bereits getroffene Entscheidungen sind gespeichert.')) return;
  try {
    await api('/api/shutdown', { method: 'POST' });
    document.body.innerHTML = '<div class="empty-state"><h2>Programm beendet.</h2><p>Du kannst dieses Fenster schließen.</p></div>';
  } catch (e) {
    showToast('Fehler: ' + e.message, 'error');
  }
});

// ============================================================================
// Polling – nur während Scan aktiv
// ============================================================================

async function refresh(force = false) {
  try {
    const newState = await api('/api/state');
    const newHash = hashState(newState);

    if (!force && newHash === lastStateHash) {
      currentState = newState;
      renderStatsAndRoots();
      return;
    }

    currentState = newState;
    lastStateHash = newHash;
    if (focusedClusterIdx >= currentState.clusters.length) {
      focusedClusterIdx = Math.max(0, currentState.clusters.length - 1);
    }
    renderAll();
  } catch (e) {
    console.error(e);
  }
}

function applyProgress(p) {
  const bar = document.getElementById('progress');
  const text = document.getElementById('progressText');
  const fill = document.getElementById('progressFill');
  if (p.scanning || p.phase === 'Done') {
    bar.classList.remove('hidden');
    const pct = p.total > 0 ? (p.current / p.total * 100) : 0;
    fill.style.width = pct + '%';
    text.textContent = `${p.phase}: ${p.message} (${p.current}/${p.total})`;
    if (!p.scanning && p.phase === 'Done') {
      setTimeout(() => bar.classList.add('hidden'), 2000);
    }
  } else {
    bar.classList.add('hidden');
  }
}

async function pollProgress() {
  try {
    applyProgress(await api('/api/progress'));
  } catch {}
}

async function tickRefresh() {
  await refresh();
  if (currentState && currentState.scanning) {
    pollTimer = setTimeout(tickRefresh, 3000);
  } else {
    pollTimer = null;
  }
}

async function tickProgress() {
  await pollProgress();
  if (currentState && currentState.scanning) {
    progressTimer = setTimeout(tickProgress, 1000);
  } else {
    progressTimer = null;
  }
}

function startPolling() {
  if (evtSource && evtSource.readyState !== EventSource.CLOSED) return; // SSE aktiv → kein Polling
  if (!pollTimer) tickRefresh();
  if (!progressTimer) tickProgress();
}

// ============================================================================
// SSE – Push statt Polling (mit automatischem Fallback aufs Polling)
// ============================================================================

let evtSource = null;
let sseFailed = false;

function startSSE() {
  if (typeof EventSource === 'undefined') { startPolling(); return; }
  try {
    evtSource = new EventSource('/api/events');
  } catch {
    startPolling();
    return;
  }
  evtSource.addEventListener('open', () => {
    sseFailed = false;
    // Client-Polling stoppen – der Server pusht jetzt.
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
    if (progressTimer) { clearTimeout(progressTimer); progressTimer = null; }
  });
  evtSource.addEventListener('state', () => { refresh(true); });
  evtSource.addEventListener('progress', (e) => {
    try { applyProgress(JSON.parse(e.data)); } catch {}
  });
  evtSource.addEventListener('error', () => {
    // EventSource reconnectet von selbst. Scheitert es dauerhaft, einmalig
    // auf das bewährte Polling zurückfallen – kein stiller Stillstand.
    if (!sseFailed) {
      sseFailed = true;
      setTimeout(() => {
        if (!evtSource || evtSource.readyState === EventSource.CLOSED) {
          startPolling();
        }
      }, 8000);
    }
  });
}

(async () => {
  await refresh(true);
  await pollProgress(); // erster Progress-Snapshot
  startSSE();           // primär: Server-Push; Polling nur als Fallback
})();