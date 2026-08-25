(() => {
  const RELOAD_COOLDOWN_MS = 20000;
  const TRANSITION_GRACE_MS = 7000;
  const STALL_LIMIT_MS = 10000;
  let lastItem = '';
  let transitionAt = 0;
  let troubleAt = 0;
  let lastVideoTime = -1;
  let lastProgressAt = Date.now();
  let autoplayStarted = false;
  let autoplayPending = false;

  async function startAutoplay(video) {
    if (autoplayStarted || autoplayPending || video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;
    autoplayPending = true;
    video.autoplay = true;
    video.playsInline = true;
    try {
      await video.play();
      autoplayStarted = true;
    } catch (_) {
      // Browsers generally allow autoplay only when muted.
      video.muted = true;
      video.defaultMuted = true;
      try {
        await video.play();
        autoplayStarted = true;
        console.info('[couch] Autoplay started muted; use player controls to enable audio.');
      } catch (_) {
        // Retry after the HLS player has attached enough media.
      }
    } finally {
      autoplayPending = false;
    }
  }

  function reloadPlayer(reason) {
    const now = Date.now();
    const previous = Number(sessionStorage.getItem('couchPlayerReloadAt') || 0);
    if (now - previous < RELOAD_COOLDOWN_MS) return;
    sessionStorage.setItem('couchPlayerReloadAt', String(now));
    console.warn('[couch] Reloading stalled Owncast player:', reason);
    location.reload();
  }

  function inspectVideo() {
    const video = document.querySelector('video');
    if (!video) return;
    startAutoplay(video);
    const now = Date.now();
    if (video.currentTime > lastVideoTime + 0.15) {
      lastVideoTime = video.currentTime;
      lastProgressAt = now;
      troubleAt = 0;
      if (transitionAt && now - transitionAt > 1000) transitionAt = 0;
      return;
    }
    if (!video.paused && !video.ended && video.readyState < HTMLMediaElement.HAVE_FUTURE_DATA) {
      if (!troubleAt) troubleAt = now;
    }
    if (transitionAt && now - transitionAt > TRANSITION_GRACE_MS && now - lastProgressAt > 5000) {
      reloadPlayer('playlist transition did not resume');
    } else if (troubleAt && now - troubleAt > STALL_LIMIT_MS && now - lastProgressAt > STALL_LIMIT_MS) {
      reloadPlayer('video remained stalled');
    }
  }

  async function inspectPlaylist() {
    try {
      const response = await fetch('/stream/api/status', { cache: 'no-store' });
      if (!response.ok) return;
      const status = await response.json();
      const item = status.currentUrl || '';
      if (lastItem && item && item !== lastItem) {
        transitionAt = Date.now();
        troubleAt = transitionAt;
        lastVideoTime = -1;
        lastProgressAt = transitionAt;
        console.info('[couch] Playlist transition detected; monitoring player recovery.');
      }
      if (item) lastItem = item;
    } catch (_) {
      // A temporary control API failure should not disturb video playback.
    }
  }

  setInterval(inspectVideo, 1000);
  setInterval(inspectPlaylist, 1500);
  inspectPlaylist();
})();
