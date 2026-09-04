// ── couch Stream Manager ─────────────────────────────────────────

// Strip leading / so paths are relative — works behind Caddy proxy with prefix stripping
function apiPath(p) { return p.replace(/^\/+/, ''); }

const API = {
  async get(path) {
    const r = await fetch(apiPath(path));
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  },
  async post(path, body) {
    const r = await fetch(apiPath(path), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error((await r.json()).error || r.statusText);
    return r.json();
  },
  async del(path, body) {
    const r = await fetch(apiPath(path), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error((await r.json()).error || r.statusText);
    return r.json();
  },
};

// ── DOM refs ─────────────────────────────────────────────────────

const $ = (s) => document.querySelector(s);
const $$ = (s) => document.querySelectorAll(s);

const el = {
  indicator: $('#status-indicator'),
  statusText: $('#status-text'),
  statusDetail: $('#status-detail'),
  playlist: $('#playlist'),
  plCount: $('#pl-count'),
  playlistSelect: $('#playlist-select'),
  dirtyIndicator: $('#dirty-indicator'),
  addUrl: $('#add-url'),
  addProvider: $('#add-provider'),
  ytPlaylistUrl: $('#yt-playlist-url'),
  dropZone: $('#drop-zone'),
  fileInput: $('#file-input'),
  uploadProgress: $('#upload-progress'),
  progressFill: $('#progress-fill'),
  progressText: $('#progress-text'),
  videoList: $('#video-list'),
  plexLibrary: $('#plex-library'),
  plexFilter: $('#plex-filter'),
  plexList: $('#plex-list'),
  configForm: $('#config-form'),
  configLoading: $('#config-loading'),
  configPlexServers: $('#config-plex-servers'),
  clock: $('#clock'),
};

// ── Init ─────────────────────────────────────────────────────────

window.addEventListener('DOMContentLoaded', () => {
  // Clock
  setInterval(() => {
    el.clock.textContent = new Date().toLocaleTimeString();
  }, 1000);

  // Poll status + playlist
  refresh();
  setInterval(refresh, 5000);

  // Buttons
  $('#btn-play').addEventListener('click', () => API.post('/api/control/play').catch(toast));
  $('#btn-pause').addEventListener('click', () => API.post('/api/control/pause').catch(toast));
  $('#btn-skip').addEventListener('click', () => {
    API.post('/api/control/skip').then(() => setTimeout(refresh, 1000)).catch(toast);
  });
  $('#btn-stop').addEventListener('click', () => {
    if (!confirm('Stop the stream? You can restart it from the command line.')) return;
    API.post('/api/control/stop')
      .then(() => { toast('success', 'Stream stopped'); refresh(); })
      .catch(toast);
  });
  $('#btn-save').addEventListener('click', () => {
    API.post('/api/playlist/save').then(d => { setPlaylistDirty(false); toast('success', `Saved ${d.file}`); fetchPlaylists(); }).catch(toast);
  });

  // Add video
  $('#btn-add').addEventListener('click', addVideo);
  el.addUrl.addEventListener('keydown', (e) => { if (e.key === 'Enter') addVideo(); });

  // YouTube playlist import
  $('#btn-ytplaylist').addEventListener('click', importYTPlaylist);
  el.ytPlaylistUrl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') importYTPlaylist();
  });

  // Upload
  $('#btn-pick-file').addEventListener('click', () => el.fileInput.click());
  el.fileInput.addEventListener('change', () => {
    if (el.fileInput.files.length) uploadFile(el.fileInput.files[0]);
  });

  // Drag & drop
  el.dropZone.addEventListener('dragover', (e) => { e.preventDefault(); el.dropZone.classList.add('dragover'); });
  el.dropZone.addEventListener('dragleave', () => el.dropZone.classList.remove('dragover'));
  el.dropZone.addEventListener('drop', (e) => {
    e.preventDefault();
    el.dropZone.classList.remove('dragover');
    const file = e.dataTransfer.files[0];
    if (file) uploadFile(file);
  });

  // Delegate clicks on playlist delete buttons
  el.playlist.addEventListener('click', (e) => {
    if (e.target.classList.contains('pl-del')) {
      const idx = parseInt(e.target.dataset.index, 10);
      removeVideo(idx);
    }
    if (e.target.classList.contains('pl-play')) playVideo(Number(e.target.dataset.index));
  });
  el.playlist.addEventListener('keydown', playlistKeydown);
  el.playlist.addEventListener('dragstart', playlistDragStart);
  el.playlist.addEventListener('dragend', playlistDragEnd);
  el.playlist.addEventListener('dragover', e => e.preventDefault());
  el.playlist.addEventListener('drop', playlistDrop);
  el.playlistSelect.addEventListener('change', switchPlaylist);
  $('#btn-pl-new').addEventListener('click', createPlaylist);
  $('#btn-pl-rename').addEventListener('click', renamePlaylist);
  $('#btn-pl-delete').addEventListener('click', deletePlaylist);
  $('#btn-pl-activate').addEventListener('click', activatePlaylist);

  $('#btn-plex-refresh').addEventListener('click', fetchPlexLibraries);
  el.plexLibrary.addEventListener('change', fetchPlexItems);
  el.plexFilter.addEventListener('input', renderPlexItems);
  $('#btn-config-add-plex').addEventListener('click', () => addPlexServerRow());
  el.configForm.addEventListener('submit', saveConfig);
  $('#btn-youtube-cookies').addEventListener('click', uploadYouTubeCookies);
  $('#btn-owncast-title').addEventListener('click', updateOwncastTitle);
  $('#btn-logs-refresh').addEventListener('click', fetchLogs);
  $('#btn-logout').addEventListener('click', logout);
  window.addEventListener('beforeunload', (event) => {
    if (!playlistDirty) return;
    event.preventDefault();
    event.returnValue = '';
  });
});

// ── Refresh ──────────────────────────────────────────────────────

let lastPlaylistLen = 0;
let playlistDirty = false;
let playlistFile = '';
let draggedIndex = -1;
let playlistRequestInFlight = false;

async function refresh() {
  if (playlistRequestInFlight || draggedIndex >= 0) return;
  playlistRequestInFlight = true;
  try {
    const [status, playlist] = await Promise.all([
      API.get('/api/status'),
      API.get('/api/playlist'),
    ]);
    renderStatus(status);
    renderPlaylist(playlist);
    playlistFile = playlist.file || playlistFile;
    $('#playlist-mode').textContent = playlist.file === playlist.activeFile ? 'Currently playing' : 'Editing only — playback unchanged';
    // Refresh video library on playlist changes
    const playlistLength = Array.isArray(playlist.videos) ? playlist.videos.length : 0;
    if (playlistLength !== lastPlaylistLen) {
      lastPlaylistLen = playlistLength;
      fetchVideos();
    }
  } catch (err) {
    el.statusText.textContent = 'API unreachable';
    el.indicator.className = 'dot stopped';
    el.statusDetail.textContent = '';
  } finally {
    playlistRequestInFlight = false;
  }
}

function renderStatus(s) {
  el.plCount.textContent = s.totalVideos;

  if (!s.playing) {
    el.indicator.className = 'dot stopped';
    el.statusText.textContent = 'Not streaming';
    el.statusDetail.textContent = s.playlistName ? `— ${s.playlistName}` : '';
  } else if (s.phase === 'downloading') {
    el.indicator.className = 'dot paused';
    el.statusText.textContent = 'Downloading';
    const progress = s.cacheTotalBytes > 0
      ? `${Math.floor((s.cacheBytes / s.cacheTotalBytes) * 100)}% · ${formatBytes(s.cacheBytes)} of ${formatBytes(s.cacheTotalBytes)}`
      : 'preparing media';
    el.statusDetail.textContent = `— ${progress}${s.currentUrl ? ` · ${trimUrl(s.currentUrl)}` : ''}`;
  } else if (s.phase === 'resolving') {
    el.indicator.className = 'dot paused';
    el.statusText.textContent = 'Resolving media';
    el.statusDetail.textContent = s.currentUrl ? `— ${trimUrl(s.currentUrl)}` : '';
  } else if (s.phase === 'retrying') {
    el.indicator.className = 'dot paused';
    el.statusText.textContent = 'Retrying';
    el.statusDetail.textContent = s.currentUrl ? `— ${trimUrl(s.currentUrl)}` : '';
  } else if (s.phase === 'holding') {
    el.indicator.className = 'dot paused';
    el.statusText.textContent = 'Between videos';
    el.statusDetail.textContent = '';
  } else if (s.paused) {
    el.indicator.className = 'dot paused';
    el.statusText.textContent = 'Paused';
    el.statusDetail.textContent = s.currentUrl
      ? `— #${s.currentIndex + 1}/${s.totalVideos} — ${trimUrl(s.currentUrl)}`
      : '';
  } else {
    el.indicator.className = 'dot streaming';
    el.statusText.textContent = 'Streaming';
    el.statusDetail.textContent = s.currentUrl
      ? `— #${s.currentIndex + 1}/${s.totalVideos} — ${trimUrl(s.currentUrl)}`
      : '';
  }
}

function renderPlaylist(p) {
  const videos = Array.isArray(p.videos) ? p.videos : [];
  const focusedIndex = el.playlist.querySelector('li:focus')?.dataset.index;
  const currentIdx = p.currentIndex;
  el.playlist.innerHTML = videos.length ? videos.map((v, i) => {
    const cls = p.file === p.activeFile && i === currentIdx ? 'now' : '';
    const display = v.title || trimUrl(v.url || '');
    const fullUrl = esc(v.url || '(empty)');
    return `<li class="${cls}" draggable="true" tabindex="0" data-index="${i}" aria-label="Queue item ${i + 1}: ${escAttr(display)}">
      <span class="pl-grip" title="Drag to reorder">⠿</span>
      <span class="pl-idx">${i + 1}</span>
      <span class="pl-provider">${v.provider || '?'}</span>
      <span class="pl-url" title="${fullUrl}">${esc(display || '(empty)')}</span>
      <button class="pl-play" data-index="${i}" title="Play now" aria-label="Play ${escAttr(display)} now">▶</button>
      <button class="pl-del" data-index="${i}" title="Remove" aria-label="Remove ${escAttr(display)}">✕</button>
    </li>`;
  }).join('') : '<li class="empty-state">This playlist is empty. Add a URL, upload a file, or browse Plex to get started.</li>';

  if (focusedIndex !== undefined) el.playlist.querySelector(`[data-index="${focusedIndex}"]`)?.focus({ preventScroll: true });
}

async function fetchPlaylists() {
  try {
    const lists = await API.get('/api/playlists');
    el.playlistSelect.innerHTML = lists.map(p => `<option value="${escAttr(p.file)}" ${p.editing ? 'selected' : ''}>${p.current ? '▶ ' : ''}${esc(p.name)} (${p.count})</option>`).join('');
    const editing = lists.find(p => p.editing); if (editing) playlistFile = editing.file;
    updatePlaylistActions(lists.length > 0);
  } catch (err) { toast('error', errorText(err)); }
}

async function activatePlaylist() {
  if (playlistDirty && !confirm('Play the current unsaved editor contents? Save first if you want these changes persisted.')) return;
  try { await API.post('/api/playlist/activate'); toast('success','Playlist is now playing'); await refresh(); await fetchPlaylists(); }
  catch (err) { toast('error', errorText(err)); }
}

async function switchPlaylist() {
  const file = el.playlistSelect.value;
  if (playlistDirty && !confirm('Discard unsaved queue changes and switch playlists?')) { el.playlistSelect.value = playlistFile; return; }
  try { await API.post('/api/playlist/load', { file }); setPlaylistDirty(false); playlistFile = file; await refresh(); await fetchPlaylists(); }
  catch (err) { toast('error', errorText(err)); }
}

async function createPlaylist() {
  const name = prompt('New playlist name:'); if (!name) return;
  try { const result = await API.post('/api/playlists', { action:'create', name }); setPlaylistDirty(false); playlistFile = result.file; await refresh(); await fetchPlaylists(); }
  catch (err) { toast('error', errorText(err)); }
}

async function renamePlaylist() {
  const currentText = el.playlistSelect.selectedOptions[0]?.textContent.replace(/ \(\d+\)$/, '') || '';
  const name = prompt('Playlist name:', currentText); if (!name) return;
  try { await API.post('/api/playlists', { action:'rename', file:playlistFile, name }); await refresh(); await fetchPlaylists(); }
  catch (err) { toast('error', errorText(err)); }
}

async function deletePlaylist() {
  const file = el.playlistSelect.value; if (!file || !confirm('Delete this playlist permanently?')) return;
  try { await API.post('/api/playlists', { action:'delete', file }); setPlaylistDirty(false); await fetchPlaylists(); await refresh(); toast('success', 'Playlist deleted'); }
  catch (err) { toast('error', errorText(err)); }
}

async function moveVideo(from, to) {
  if (from === to || to < 0) return;
  try { await API.post('/api/playlist/move', { from, to }); setPlaylistDirty(true); await refresh(); el.playlist.querySelector(`[data-index="${to}"]`)?.focus(); }
  catch (err) { toast('error', errorText(err)); }
}

function playVideo(index) { API.post('/api/playlist/play', { index }).then(() => setTimeout(refresh, 400)).catch(err => toast('error', errorText(err))); }
function playlistDragStart(e) { const row = e.target.closest('li[data-index]'); if (row) { draggedIndex = Number(row.dataset.index); row.classList.add('dragging'); e.dataTransfer.effectAllowed = 'move'; } }
function playlistDragEnd() { draggedIndex = -1; el.playlist.querySelector('.dragging')?.classList.remove('dragging'); }
function playlistDrop(e) { e.preventDefault(); const row = e.target.closest('li[data-index]'); if (row && draggedIndex >= 0) moveVideo(draggedIndex, Number(row.dataset.index)); draggedIndex = -1; }
function playlistKeydown(e) {
  const row = e.target.closest('li[data-index]'); if (!row) return; const index = Number(row.dataset.index);
  if (e.key === 'Enter') { e.preventDefault(); playVideo(index); }
  else if (e.key === 'Delete' || e.key === 'Backspace') { e.preventDefault(); removeVideo(index); }
  else if ((e.ctrlKey || e.metaKey) && e.key === 'ArrowUp') { e.preventDefault(); moveVideo(index, index - 1); }
  else if ((e.ctrlKey || e.metaKey) && e.key === 'ArrowDown') { e.preventDefault(); moveVideo(index, index + 1); }
  else if (e.key === 'ArrowUp') { e.preventDefault(); el.playlist.querySelector(`[data-index="${Math.max(0,index-1)}"]`)?.focus(); }
  else if (e.key === 'ArrowDown') { e.preventDefault(); el.playlist.querySelector(`[data-index="${index+1}"]`)?.focus(); }
}

// ── Actions ──────────────────────────────────────────────────────

async function addVideo() {
  const url = el.addUrl.value.trim();
  if (!url) return toast('error', 'Enter a URL or file path');
  const provider = el.addProvider.value;

  try {
    await API.post('/api/playlist/video', { url, provider: provider || undefined });
    el.addUrl.value = '';
    toast('success', 'Added to playlist');
    setPlaylistDirty(true);
    refresh();
  } catch (err) {
    toast('error', err.message);
  }
}

async function removeVideo(idx) {
  if (!confirm(`Remove video #${idx + 1}?`)) return;
  try {
    await API.del('/api/playlist/remove', { index: idx });
    toast('success', 'Removed');
    setPlaylistDirty(true);
    refresh();
  } catch (err) {
    toast('error', err.message);
  }
}

async function importYTPlaylist() {
  const url = el.ytPlaylistUrl.value.trim();
  if (!url) return toast('error', 'Enter a YouTube playlist URL');
  try {
    const result = await API.post('/api/playlist/add', { url });
    el.ytPlaylistUrl.value = '';
    toast('success', `Imported "${result.name}" (${result.count} videos)`);
    setPlaylistDirty(true);
    refresh();
  } catch (err) {
    toast('error', err.message);
  }
}

// ── Upload ───────────────────────────────────────────────────────

function uploadFile(file) {
  el.uploadProgress.classList.remove('hidden');
  el.progressFill.style.width = '0%';
  el.progressText.textContent = `Uploading ${file.name}…`;

  const form = new FormData();
  form.append('file', file);

  const xhr = new XMLHttpRequest();
  xhr.open('POST', apiPath('/api/upload'));

  xhr.upload.addEventListener('progress', (e) => {
    if (e.lengthComputable) {
      const pct = Math.round((e.loaded / e.total) * 100);
      el.progressFill.style.width = pct + '%';
      el.progressText.textContent = `${file.name} — ${pct}%`;
    }
  });

  xhr.addEventListener('load', () => {
    try {
      const data = JSON.parse(xhr.responseText);
      if (xhr.status >= 200 && xhr.status < 300) {
        const mb = (data.bytes / (1024 * 1024)).toFixed(1);
        toast('success', `Uploaded: ${data.file} (${mb} MB)`);
        el.uploadProgress.classList.add('hidden');
        el.fileInput.value = '';
        refresh();
      } else {
        toast('error', data.error || 'Upload failed');
        el.uploadProgress.classList.add('hidden');
      }
    } catch {
      toast('error', 'Upload failed');
      el.uploadProgress.classList.add('hidden');
    }
  });

  xhr.addEventListener('error', () => {
    toast('error', 'Upload error');
    el.uploadProgress.classList.add('hidden');
  });

  xhr.send(form);
}

// ── Video library ────────────────────────────────────────────────

async function fetchVideos() {
  try {
    const videos = await API.get('/api/videos');
    el.videoList.innerHTML = videos.length
      ? videos.map(v => {
          const mb = (v.size / (1024 * 1024)).toFixed(1);
          return `<li>
            <span class="vname" title="${esc(v.name)}">${esc(v.name)}</span>
            <span class="vsize">${mb} MB</span>
            <button class="vadd" data-path="${escAttr(v.name)}" title="Add to playlist">+</button>
          </li>`;
        }).join('')
      : '<li class="empty">No videos uploaded yet</li>';

    // Delegate add-to-playlist clicks
    el.videoList.querySelectorAll('.vadd').forEach(btn => {
      btn.addEventListener('click', () => {
        const path = '/opt/owncast/playlist-streamer/videos/' + btn.dataset.path;
        API.post('/api/playlist/video', { url: path, provider: 'local' })
          .then(() => { setPlaylistDirty(true); toast('success', 'Added'); refresh(); })
          .catch(toast);
      });
    });
  } catch (err) {
    el.videoList.innerHTML = '<li class="empty">Could not load library</li>';
  }
}

// Initial load
fetchVideos();
fetchPlaylists();

// ── Plex library ────────────────────────────────────────────────

let plexItems = [];

async function fetchPlexLibraries() {
  try {
    const libraries = await API.get('/api/plex/libraries');
    el.plexLibrary.innerHTML = '<option value="">Choose a library…</option>' + libraries.map(lib =>
      `<option value="${escAttr(lib.server + '|' + lib.key + '|' + lib.type)}">${esc(lib.server)} · ${esc(lib.title)}</option>`
    ).join('');
    if (!libraries.length) el.plexList.innerHTML = '<li class="empty">No movie or TV libraries found</li>';
  } catch (err) {
    el.plexList.innerHTML = `<li class="empty">${esc(errorText(err))}</li>`;
  }
}

async function fetchPlexItems() {
  const [server, section, type] = el.plexLibrary.value.split('|');
  if (!server || !section) return;
  el.plexList.innerHTML = '<li class="empty">Loading…</li>';
  try {
    plexItems = await API.get(`/api/plex/items?server=${encodeURIComponent(server)}&section=${encodeURIComponent(section)}&type=${encodeURIComponent(type)}`);
    renderPlexItems();
  } catch (err) {
    el.plexList.innerHTML = `<li class="empty">${esc(errorText(err))}</li>`;
  }
}

function renderPlexItems() {
  const filter = el.plexFilter.value.trim().toLowerCase();
  const items = plexItems.map((item, index) => ({ item, index })).filter(({ item }) => {
    if (!filter) return true;
    return [item.title, item.showTitle, item.seasonTitle, item.fileName]
      .filter(Boolean).some(value => value.toLowerCase().includes(filter));
  });
  const libraryType = el.plexLibrary.value.split('|')[2];
  el.plexList.innerHTML = items.length
    ? (libraryType === 'show' ? renderPlexShows(items, Boolean(filter)) : renderPlexMovies(items, Boolean(filter)))
    : '<li class="empty">No matching titles or files</li>';
  el.plexList.querySelectorAll('.plex-add').forEach(btn => btn.addEventListener('click', () => {
    const item = plexItems[Number(btn.dataset.index)];
    if (!item) return;
    API.post('/api/playlist/video', { url: item.url, provider: 'plex', title: item.title || '' })
      .then(() => { setPlaylistDirty(true); toast('success', 'Added from Plex'); refresh(); })
      .catch(err => toast('error', errorText(err)));
  }));
}

function renderPlexMovies(items, open) {
  return items.map(({ item, index }) => {
    const movie = `${item.title}${item.year ? ` (${item.year})` : ''}`;
    return `<li class="plex-branch"><details${open ? ' open' : ''}>
      <summary><span class="plex-folder">🎬</span><span>${esc(movie)}</span></summary>
      <ul class="plex-tree"><li class="plex-file">
        <span class="plex-file-icon">▤</span>
        <span class="vname" title="${escAttr(item.fileName || item.title)}">${esc(item.fileName || item.title)}</span>
        ${plexAddButton(index)}
      </li></ul>
    </details></li>`;
  }).join('');
}

function renderPlexShows(items, open) {
  const shows = new Map();
  items.forEach(({ item, index }) => {
    const showName = item.showTitle || 'Other TV';
    const seasonName = item.seasonTitle || (item.season ? `Season ${item.season}` : 'Episodes');
    if (!shows.has(showName)) shows.set(showName, new Map());
    const seasons = shows.get(showName);
    if (!seasons.has(seasonName)) seasons.set(seasonName, []);
    seasons.get(seasonName).push({ item, index });
  });
  return [...shows.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([showName, seasons]) =>
    `<li class="plex-branch"><details${open ? ' open' : ''}>
      <summary><span class="plex-folder">📺</span><span>${esc(showName)}</span><span class="plex-count">${[...seasons.values()].reduce((n, episodes) => n + episodes.length, 0)}</span></summary>
      <ul class="plex-tree">${[...seasons.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([seasonName, episodes]) =>
        `<li class="plex-branch"><details${open ? ' open' : ''}>
          <summary><span class="plex-folder">📁</span><span>${esc(seasonName)}</span><span class="plex-count">${episodes.length}</span></summary>
          <ul class="plex-tree">${episodes.sort((a, b) => (a.item.episode || 0) - (b.item.episode || 0)).map(({ item, index }) =>
            `<li class="plex-file">
              <span class="plex-file-icon">▤</span>
              <span class="plex-file-info">
                <span class="plex-item-title">${esc(plexEpisodeLabel(item))}</span>
                ${item.fileName ? `<span class="plex-file-name" title="${escAttr(item.fileName)}">${esc(item.fileName)}</span>` : ''}
              </span>
              ${plexAddButton(index)}
            </li>`
          ).join('')}</ul>
        </details></li>`
      ).join('')}</ul>
    </details></li>`
  ).join('');
}

function plexEpisodeLabel(item) {
  const number = item.season
    ? `S${String(item.season).padStart(2, '0')}E${String(item.episode || 0).padStart(2, '0')}`
    : item.episode ? `E${String(item.episode).padStart(2, '0')}` : '';
  const episodeTitle = item.title?.split(' · ').pop() || item.title || 'Episode';
  return [number, episodeTitle].filter(Boolean).join(' · ');
}

function plexAddButton(index) {
  return `<button class="vadd plex-add" data-index="${index}" title="Add to playlist">+</button>`;
}

function errorText(err) {
  try { return JSON.parse(err.message).error || err.message; } catch { return err.message; }
}

function setPlaylistDirty(value) {
  playlistDirty = value;
  el.dirtyIndicator.classList.toggle('hidden', !value);
  $('#btn-save').classList.toggle('has-changes', value);
}

function updatePlaylistActions(hasPlaylists) {
  ['#btn-pl-rename', '#btn-pl-delete', '#btn-pl-activate', '#btn-save'].forEach(selector => {
    $(selector).disabled = !hasPlaylists;
  });
}

// ── Settings ────────────────────────────────────────────────────

async function configRequest(method, body) {
  const response = await fetch(apiPath('/api/config'), {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!response.ok) throw new Error((await response.json()).error || response.statusText);
  return response.json();
}

async function fetchLogs() {
  try {
    const response = await fetch(apiPath('/api/logs?limit=300'));
    if (!response.ok) throw new Error((await response.json()).error || response.statusText);
    const data = await response.json();
    $('#log-output').textContent = (data.lines || []).join('\n') || 'No logs captured yet.';
    $('#log-output').scrollTop = $('#log-output').scrollHeight;
  } catch (err) { toast('error', errorText(err)); }
}

let configLoaded = false;

async function loadConfig() {
  if (configLoaded) return;
  el.configLoading.classList.remove('hidden');
  try {
    const settings = await configRequest('GET');
    renderConfig(settings);
    configLoaded = true;
    el.configLoading.classList.add('hidden');
    el.configForm.classList.remove('hidden');
  } catch (err) {
    el.configLoading.textContent = 'Could not load settings. Select the tab to retry.';
    toast('error', errorText(err));
  }
}

async function logout() {
  try { await fetch(apiPath('/api/auth/logout'), { method: 'POST' }); } catch {}
  location.href = 'login.html';
}

function renderConfig(settings) {
  el.configPlexServers.innerHTML = '';
  (settings.plex.servers || []).forEach(addPlexServerRow);
  $('#cfg-loop').checked = settings.streamer.loopPlaylist;
  $('#cfg-realtime').checked = settings.streamer.realtime;
  $('#cfg-subtitles').checked = settings.streamer.subtitles;
  $('#cfg-subtitle-lang').value = settings.streamer.subtitleLang || 'en';
  $('#cfg-max-retries').value = settings.streamer.maxRetries;
  $('#cfg-delay').value = settings.streamer.delayBetween;
  $('#cfg-plex-cache').checked = settings.streamer.plexCacheEnabled;
  $('#cfg-plex-cache-max').value = settings.streamer.plexCacheMaxGB;
  $('#cfg-plex-cache-reserve').value = settings.streamer.plexCacheMinFreeGB;
  $('#youtube-cookie-status').textContent = settings.youtube?.hasCookies ? 'A cookies.txt file is configured.' : 'No cookies configured; YouTube may reject downloads as bot traffic.';
  fetchOwncastTitle();
}

async function fetchOwncastTitle() {
  try {
    const response = await fetch(apiPath('/api/owncast/title'));
    if (!response.ok) throw new Error((await response.json()).error || response.statusText);
    $('#cfg-owncast-title').value = (await response.json()).title || '';
  } catch (err) { toast('error',errorText(err)); }
}

async function updateOwncastTitle() {
  const title = $('#cfg-owncast-title').value.trim();
  try {
    const response = await fetch(apiPath('/api/owncast/title'), { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({title}) });
    const result = await response.json(); if (!response.ok) throw new Error(result.error || response.statusText);
    toast('success','Front-page title updated');
  } catch (err) { toast('error',errorText(err)); }
}

async function uploadYouTubeCookies() {
  const file = $('#youtube-cookies-file').files[0];
  if (!file) return toast('error','Choose a cookies.txt file first');
  const form = new FormData(); form.append('file',file);
  try {
    const response = await fetch(apiPath('/api/admin/cookies'), { method:'POST', body:form });
    const result = await response.json(); if (!response.ok) throw new Error(result.error || response.statusText);
    $('#youtube-cookie-status').textContent = `Cookies configured (${result.entries} entries).`;
    $('#youtube-cookies-file').value = '';
    toast('success','YouTube cookies updated');
  } catch (err) { toast('error',errorText(err)); }
}

function addPlexServerRow(server = {}) {
  const row = document.createElement('div');
  row.className = 'plex-server-row';
  row.innerHTML = `<input class="cfg-plex-name" placeholder="Name (e.g. home)" value="${escAttr(server.name || '')}">
    <input class="cfg-plex-url" placeholder="https://server:32400" value="${escAttr(server.baseUrl || '')}">
    <input class="cfg-plex-token" type="password" autocomplete="new-password" placeholder="${server.hasToken ? 'Token saved — leave blank to keep' : 'X-Plex-Token'}">
    <button type="button" class="cfg-plex-remove" title="Remove">✕</button>`;
  row.querySelector('.cfg-plex-remove').addEventListener('click', () => row.remove());
  el.configPlexServers.appendChild(row);
}

async function saveConfig(event) {
  event.preventDefault();
  const servers = [...el.configPlexServers.querySelectorAll('.plex-server-row')].map(row => ({
    name: row.querySelector('.cfg-plex-name').value.trim(),
    baseUrl: row.querySelector('.cfg-plex-url').value.trim(),
    token: row.querySelector('.cfg-plex-token').value.trim(),
  }));
  const body = { plex: { servers }, streamer: {
    loopPlaylist: $('#cfg-loop').checked,
    realtime: $('#cfg-realtime').checked,
    subtitles: $('#cfg-subtitles').checked,
    subtitleLang: $('#cfg-subtitle-lang').value.trim(),
    maxRetries: Number($('#cfg-max-retries').value),
    delayBetween: Number($('#cfg-delay').value),
    plexCacheEnabled: $('#cfg-plex-cache').checked,
    plexCacheMaxGB: Number($('#cfg-plex-cache-max').value),
    plexCacheMinFreeGB: Number($('#cfg-plex-cache-reserve').value),
  }};
  if (!confirm('Save settings and restart the stream service?')) return;
  try {
    await configRequest('POST', body);
    toast('success', 'Settings saved; service is restarting');
    setTimeout(() => location.reload(), 3500);
  } catch (err) { toast('error', errorText(err)); }
}

// ── Helpers ──────────────────────────────────────────────────────

function trimUrl(u) {
  if (!u) return '';
  // For local files, show just the filename
  if (u.startsWith('/')) {
    const parts = u.split('/');
    return parts[parts.length - 1];
  }
  if (u.length > 60) return u.slice(0, 57) + '…';
  return u;
}

function formatBytes(value) {
  const bytes = Number(value) || 0;
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
  if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(0)} MB`;
  return `${Math.max(0, Math.round(bytes / 1024))} KB`;
}

function esc(s) { return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
function escAttr(s) { return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;'); }

function toast(kind, msg) {
  if (typeof kind !== 'string' || !msg) { msg = kind.message || String(kind); kind = 'error'; }
  const el = document.createElement('div');
  el.className = 'toast ' + (kind === 'error' ? 'error' : kind === 'success' ? 'success' : '');
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 3500);
}

// ── Tabs ─────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => {
  document.querySelectorAll('.tab').forEach(tab => {
    tab.addEventListener('click', () => {
      document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
      document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
      tab.classList.add('active');
      const panel = document.getElementById(tab.dataset.tab);
      if (panel) panel.classList.add('active');
      if (tab.dataset.tab === 'config-panel') loadConfig();
    });
  });
});

// ── Schedule ─────────────────────────────────────────────────────

let scheduleData = null;

async function fetchSchedule() {
  try {
    scheduleData = await API.get('/api/schedule');
    renderSchedule();
  } catch (err) {
    scheduleData = { name: 'Schedule', entries: [] };
    renderSchedule();
  }
}

function renderSchedule() {
  const grid = document.getElementById('schedule-grid');
  if (!grid) return;

  const hours = [];
  for (let h = 0; h < 24; h++) {
    const hh = String(h).padStart(2, '0');
    hours.push(hh + ':00');

    // Find entries at this hour
    const entries = (scheduleData && scheduleData.entries || [])
      .filter(e => e.time >= hh + ':00' && e.time < hh + ':30');

    // Range: entries that start between this half-hour slot
    // We group by half-hour slots for finer granularity
    // Actually, let's use full hours for the grid, show entries at their exact time
  }

  // Build a map: time → entry
  const entryMap = {};
  if (scheduleData && scheduleData.entries) {
    scheduleData.entries.forEach((e, i) => { entryMap[e.time] = { ...e, _idx: i }; });
  }

  let html = '';
  const halfHours = [];
  for (let h = 0; h < 24; h++) {
    halfHours.push(String(h).padStart(2, '0') + ':00');
    halfHours.push(String(h).padStart(2, '0') + ':30');
  }

  // Show labels at even hours, all rows clickable
  for (const time of halfHours) {
    const entry = entryMap[time];
    const isFullHour = time.endsWith(':00');
    const timeDisplay = isFullHour ? time : '';

    html += `<div class="sched-row" data-time="${time}">
      <div class="sched-time">${timeDisplay}</div>
      <div class="sched-slot">`;

    if (entry) {
      const name = entry.video
        ? trimUrl(entry.video.url || '')
        : (entry.cue ? `🎬 ${entry.cue.command} ${(entry.cue.args || []).join(' ')}` : '—');
      const cueBadge = entry.cue
        ? `<span class="cue-badge">fx</span>` : '';
      html += `<div class="sched-entry" data-idx="${entry._idx}">
        ${cueBadge}<span class="se-name">${esc(name)}</span>
        <button class="se-del" data-idx="${entry._idx}" title="Remove entry">✕</button>
      </div>`;
    } else {
      html += `<span class="sched-empty">+ add</span>`;
    }

    html += `</div></div>`;
  }

  grid.innerHTML = html;

  // Click handlers
  grid.querySelectorAll('.sched-row').forEach(row => {
    row.addEventListener('click', (e) => {
      // Ignore clicks on the delete button or entry itself
      if (e.target.classList.contains('se-del')) return;
      const time = row.dataset.time;

      // If there's an entry at this time, open edit modal
      if (entryMap[time]) {
        showScheduleModal(time, entryMap[time]);
      } else {
        showScheduleModal(time, null);
      }
    });
  });

  // Delete button handlers
  grid.querySelectorAll('.se-del').forEach(btn => {
    btn.addEventListener('click', async (e) => {
      e.stopPropagation();
      const idx = parseInt(btn.dataset.idx, 10);
      if (!confirm('Remove this schedule entry?')) return;
      try {
        await API.post('/api/schedule/remove', { index: idx });
        await fetchSchedule();
      } catch (err) { toast('error', err.message); }
    });
  });
}

// ── Schedule modal ───────────────────────────────────────────────

function showScheduleModal(time, existing) {
  const isEdit = existing != null;

  // Pre-fill values
  const videoUrl = (existing && existing.video) ? existing.video.url : '';
  const videoProvider = (existing && existing.video) ? (existing.video.provider || '') : '';
  const hasCue = existing && existing.cue != null;
  const cueCmd = hasCue ? existing.cue.command : '';
  const cueArgs = hasCue ? (existing.cue.args || []).join(' ') : '';

  const modal = document.createElement('div');
  modal.className = 'modal-overlay';
  modal.innerHTML = `
    <div class="modal">
      <h3>${isEdit ? 'Edit' : 'Add'} entry at ${time}</h3>

      <label>Video URL (optional)</label>
      <input id="mod-video-url" type="text" value="${escAttr(videoUrl)}" placeholder="Path or URL…">

      <label>Provider</label>
      <select id="mod-video-provider">
        <option value="">auto</option>
        ${['youtube','local','http','realdebrid','kick','twitch']
          .map(p => `<option value="${p}" ${videoProvider === p ? 'selected' : ''}>${p}</option>`).join('')}
      </select>

      <details class="cue-section" ${hasCue ? 'open' : ''}>
        <summary>🎬 Layer Cue (optional)</summary>
        <label>Command</label>
        <select id="mod-cue-cmd">
          <option value="">none</option>
          <option value="preset" ${cueCmd === 'preset' ? 'selected' : ''}>preset</option>
          <option value="layer" ${cueCmd === 'layer' ? 'selected' : ''}>layer</option>
          <option value="fx" ${cueCmd === 'fx' ? 'selected' : ''}>fx</option>
          <option value="clear" ${cueCmd === 'clear' ? 'selected' : ''}>clear</option>
          <option value="layers" ${cueCmd === 'layers' ? 'selected' : ''}>layers (on/off)</option>
        </select>
        <label>Arguments (space-separated)</label>
        <input id="mod-cue-args" type="text" value="${escAttr(cueArgs)}" placeholder="e.g. crt scanlineIntensity 0.8">
      </details>

      <div class="btn-row">
        ${isEdit ? '<button id="mod-delete">Remove</button>' : ''}
        <button id="mod-cancel">Cancel</button>
        <button id="mod-save">Save</button>
      </div>
    </div>`;

  document.body.appendChild(modal);

  // Focus video URL
  setTimeout(() => document.getElementById('mod-video-url')?.focus(), 50);

  document.getElementById('mod-cancel').addEventListener('click', () => modal.remove());
  modal.addEventListener('click', (e) => { if (e.target === modal) modal.remove(); });

  if (isEdit) {
    document.getElementById('mod-delete').addEventListener('click', async () => {
      modal.remove();
      try {
        await API.post('/api/schedule/remove', { index: existing._idx });
        await fetchSchedule();
      } catch (err) { toast('error', err.message); }
    });
  }

  document.getElementById('mod-save').addEventListener('click', async () => {
    const vUrl = document.getElementById('mod-video-url').value.trim();
    const vProv = document.getElementById('mod-video-provider').value;
    const cueCmdVal = document.getElementById('mod-cue-cmd').value;
    const cueArgsRaw = document.getElementById('mod-cue-args').value.trim();

    const entry = { time };

    if (vUrl) {
      entry.video = { url: vUrl };
      if (vProv) entry.video.provider = vProv;
    }
    if (cueCmdVal) {
      entry.cue = {
        command: cueCmdVal,
        args: cueArgsRaw ? cueArgsRaw.split(/\s+/) : [],
      };
    }
    if (!vUrl && !cueCmdVal) {
      toast('error', 'Add a video URL, a layer cue, or both');
      return;
    }

    try {
      if (isEdit) {
        // Remove old, add new
        await API.post('/api/schedule/remove', { index: existing._idx });
      }
      await API.post('/api/schedule/add', entry);
      modal.remove();
      await fetchSchedule();
      toast('success', isEdit ? 'Entry updated' : 'Entry added');
    } catch (err) {
      toast('error', err.message);
    }
  });
}

// ── Schedule save button ─────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => {
  const btnSave = document.getElementById('btn-sched-save');
  if (btnSave) {
    btnSave.addEventListener('click', async () => {
      if (!scheduleData || !scheduleData.entries.length) {
        toast('error', 'No schedule entries to save');
        return;
      }
      try {
        await API.post('/api/schedule', scheduleData);
        const result = await API.post('/api/schedule/save');
        toast('success', `Saved ${result.file}`);
      } catch (err) { toast('error', err.message); }
    });
  }

  // Schedule run/stop buttons
  const btnRun = document.getElementById('btn-sched-run');
  const btnStop = document.getElementById('btn-sched-stop');
  const statusEl = document.getElementById('sched-status');

  if (btnRun) {
    btnRun.addEventListener('click', async () => {
      try {
        await API.post('/api/schedule/run');
        btnRun.classList.add('hidden');
        btnStop.classList.remove('hidden');
        statusEl.classList.remove('hidden');
        toast('success', 'Schedule runner started');
      } catch (err) { toast('error', err.message); }
    });
  }
  if (btnStop) {
    btnStop.addEventListener('click', async () => {
      try {
        await API.post('/api/schedule/stop');
        btnRun.classList.remove('hidden');
        btnStop.classList.add('hidden');
        statusEl.classList.add('hidden');
        toast('success', 'Schedule runner stopped');
      } catch (err) { toast('error', err.message); }
    });
  }

  // Check schedule runner status
  (async () => {
    try {
      const s = await API.get('/api/schedule/status');
      if (s.running && btnRun && btnStop && statusEl) {
        btnRun.classList.add('hidden');
        btnStop.classList.remove('hidden');
        statusEl.classList.remove('hidden');
      }
    } catch (_) {}
  })();
});

// Initial schedule load
fetchSchedule();
