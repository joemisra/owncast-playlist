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
  addUrl: $('#add-url'),
  addProvider: $('#add-provider'),
  ytPlaylistUrl: $('#yt-playlist-url'),
  dropZone: $('#drop-zone'),
  fileInput: $('#file-input'),
  uploadProgress: $('#upload-progress'),
  progressFill: $('#progress-fill'),
  progressText: $('#progress-text'),
  videoList: $('#video-list'),
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
    API.post('/api/playlist/save').then(d => toast('success', `Saved ${d.file}`)).catch(toast);
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
  });
});

// ── Refresh ──────────────────────────────────────────────────────

let lastPlaylistLen = 0;

async function refresh() {
  try {
    const [status, playlist] = await Promise.all([
      API.get('/api/status'),
      API.get('/api/playlist'),
    ]);
    renderStatus(status);
    renderPlaylist(playlist);
    // Refresh video library on playlist changes
    if (playlist.videos.length !== lastPlaylistLen) {
      lastPlaylistLen = playlist.videos.length;
      fetchVideos();
    }
  } catch (err) {
    el.statusText.textContent = 'API unreachable';
    el.indicator.className = 'dot stopped';
    el.statusDetail.textContent = '';
  }
}

function renderStatus(s) {
  el.plCount.textContent = s.totalVideos;

  if (!s.playing) {
    el.indicator.className = 'dot stopped';
    el.statusText.textContent = 'Not streaming';
    el.statusDetail.textContent = s.playlistName ? `— ${s.playlistName}` : '';
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
  const currentIdx = p.currentIndex;
  el.playlist.innerHTML = p.videos.map((v, i) => {
    const cls = i === currentIdx ? 'now' : '';
    const display = trimUrl(v.url || '');
    const fullUrl = esc(v.url || '(empty)');
    return `<li class="${cls}">
      <span class="pl-idx">${i + 1}</span>
      <span class="pl-provider">${v.provider || '?'}</span>
      <span class="pl-url" title="${fullUrl}">${esc(display || '(empty)')}</span>
      <button class="pl-del" data-index="${i}" title="Remove">✕</button>
    </li>`;
  }).join('');

  // Scroll to current
  const now = el.playlist.querySelector('.now');
  if (now) now.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
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
          .then(() => { toast('success', 'Added'); refresh(); })
          .catch(toast);
      });
    });
  } catch (err) {
    el.videoList.innerHTML = '<li class="empty">Could not load library</li>';
  }
}

// Initial load
fetchVideos();

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
