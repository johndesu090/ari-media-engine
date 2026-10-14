/* Ari Media Engine — built-in HTML5 player runtime.
 * Plays the stream over HLS, falling back to DASH:
 *   hls    -> HLS only
 *   dash   -> DASH only
 *   all    -> HLS first, then DASH
 * Autoplay is disabled so playback starts (with audio) on a user tap.
 * When tokenization is enabled the page fetches a fresh signed token a few
 * seconds before the current one expires and reloads the source in place.
 * Playback is forced inline (never fullscreen) on every platform. No build step. */
(function () {
  "use strict";

  var cfgEl = document.getElementById("ari-config");
  var cfg = {};
  try { cfg = JSON.parse((cfgEl && cfgEl.textContent) || "{}"); } catch (e) { cfg = {}; }

  var playerEl = document.getElementById("ari-player");
  var video = document.getElementById("ari-video");
  var tapEl = document.getElementById("ari-tap");
  var statusEl = document.getElementById("ari-status");
  var statusText = document.getElementById("ari-status-text");
  var playBtn = document.getElementById("ari-play");
  var muteBtn = document.getElementById("ari-mute");
  var volumeEl = document.getElementById("ari-volume");

  var state = {
    hls: null,
    dash: null,
    mode: "none",
    tokenTimer: null,
    triedHLS: false,
    triedDASH: false
  };

  function setStatus(kind, text) {
    statusEl.dataset.state = kind;
    statusText.textContent = text;
  }

  // Peer-assisted delivery (HLS/DASH segments shared over WebRTC data channels).
  var p2p = null;
  if (cfg.p2p && cfg.p2p.enabled && window.AriP2P) {
    try {
      p2p = window.AriP2P.create({ channel: cfg.streamId, wsPath: cfg.p2p.wsPath });
      p2p.start();
    } catch (e) { p2p = null; }
  }

  /* --------------------------- platform support -------------------------- */

  function isIOS() {
    var ua = navigator.userAgent || "";
    if (/iPad|iPhone|iPod/.test(ua)) { return true; }
    return navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1;
  }

  // Force inline playback everywhere (iOS/Android/WebViews/desktop).
  function enforceInlinePlayback() {
    video.setAttribute("playsinline", "");
    video.setAttribute("webkit-playsinline", "");
    video.setAttribute("x5-playsinline", "");
    video.setAttribute("x5-video-player-type", "h5-page");
    video.playsInline = true;
    video.controls = false;
    if (typeof video.webkitEnterFullscreen === "function") {
      video.webkitEnterFullscreen = function () {};
    }
    if (typeof video.webkitSupportsFullscreen !== "undefined") {
      video.webkitSupportsFullscreen = false;
    }
  }

  function showTap() { if (tapEl) { tapEl.hidden = false; } }
  function hideTap() { if (tapEl) { tapEl.hidden = true; } }

  // requestPlay must be called from a user gesture so the stream starts with
  // audio (browsers block un-muted autoplay).
  function requestPlay() {
    video.muted = false;
    var p = video.play();
    if (p && typeof p.then === "function") {
      p.then(hideTap).catch(function () { showTap(); });
    }
  }

  /* ---------------------------- HLS / DASH ------------------------------ */

  // nextFallback walks the delivery chain: HLS, then DASH, then error.
  function nextFallback(reason) {
    if (!state.triedHLS && cfg.hlsUrl) { state.triedHLS = true; startHLS(); return; }
    if (!state.triedDASH && cfg.dashUrl) { state.triedDASH = true; startDASH(); return; }
    setStatus("error", "Playback failed: " + reason);
  }

  function startHLS() {
    state.mode = "hls";
    setStatus("connecting", "Connecting · HLS");
    var url = cfg.hlsUrl;
    if (!url) { nextFallback("no HLS source"); return; }

    if (window.Hls && window.Hls.isSupported()) {
      // Keep the player close to the live edge: start ~2 segments behind it and
      // speed up (up to 1.5x) when it drifts further, so latency stays low.
      var hlsConfig = {
        lowLatencyMode: true,
        liveSyncDurationCount: 2,
        liveMaxLatencyDurationCount: 5,
        maxLiveSyncPlaybackRate: 1.5,
        backBufferLength: 0,
        enableWorker: true
      };
      if (p2p) { hlsConfig.loader = p2p.hlsLoader(window.Hls); }
      var hls = new window.Hls(hlsConfig);
      state.hls = hls;
      hls.on(window.Hls.Events.MANIFEST_PARSED, function () {
        setStatus("live", "Live · HLS");
      });
      hls.on(window.Hls.Events.ERROR, function (_, data) {
        if (data && data.fatal) { nextFallback("HLS error"); }
      });
      hls.loadSource(url);
      hls.attachMedia(video);
      return;
    }

    // Native HLS (Safari/iOS).
    video.src = url;
    video.addEventListener("loadedmetadata", function () {
      setStatus("live", "Live · HLS");
    }, { once: true });
    video.addEventListener("error", function () { nextFallback("HLS playback error"); }, { once: true });
  }

  function startDASH() {
    state.mode = "dash";
    setStatus("connecting", "Connecting · DASH");
    var url = cfg.dashUrl;
    if (!url) { nextFallback("no DASH source"); return; }

    if (window.dashjs && window.dashjs.MediaPlayer) {
      var player = window.dashjs.MediaPlayer().create();
      if (p2p) { p2p.setupDash(player); }
      state.dash = player;
      player.initialize(video, url, false);
      player.on(window.dashjs.MediaPlayer.events.PLAYBACK_PLAYING, function () {
        setStatus("live", "Live · DASH");
      });
      player.on(window.dashjs.MediaPlayer.events.ERROR, function () {
        nextFallback("DASH error");
      });
      return;
    }
    setStatus("error", "DASH playback requires dash.js (set player.dash_js_url)");
  }

  /* --------------------------- token refresh ---------------------------- */

  // reloadSource restarts the active source with the (possibly new) URL while
  // keeping the same player instance.
  function reloadSource() {
    if (state.mode === "hls") {
      if (state.hls) {
        try { state.hls.stopLoad(); state.hls.loadSource(cfg.hlsUrl); state.hls.startLoad(); } catch (e) { /* ignore */ }
      } else if (cfg.hlsUrl) {
        video.src = cfg.hlsUrl;
      }
    } else if (state.mode === "dash" && state.dash) {
      try { state.dash.attachSource(cfg.dashUrl); } catch (e) { /* ignore */ }
    }
  }

  function scheduleTokenRefresh() {
    if (!cfg.tokenEnabled || !cfg.tokenExpiresAt) { return; }
    if (state.tokenTimer) { clearTimeout(state.tokenTimer); state.tokenTimer = null; }
    var lead = cfg.refreshLeadMs || 5000;
    var ms = cfg.tokenExpiresAt - Date.now() - lead;
    if (ms < 0) { ms = 0; }
    state.tokenTimer = setTimeout(refreshToken, ms);
  }

  function refreshToken() {
    var base = cfg.tokenPath || "/player/token";
    var url = base + "?id=" + encodeURIComponent(cfg.streamId);
    fetch(url, { credentials: "omit", cache: "no-store" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (d && d.tokenEnabled) {
          if (d.hlsUrl) { cfg.hlsUrl = d.hlsUrl; }
          if (d.dashUrl) { cfg.dashUrl = d.dashUrl; }
          if (d.expiresAt) { cfg.tokenExpiresAt = d.expiresAt; }
          if (d.refreshLeadMs) { cfg.refreshLeadMs = d.refreshLeadMs; }
          reloadSource();
        }
      })
      .catch(function () { /* ignore; retry on next schedule */ })
      .then(scheduleTokenRefresh);
  }

  /* ------------------------------ controls ------------------------------ */

  playBtn.addEventListener("click", function () {
    if (video.paused) { requestPlay(); } else { video.pause(); }
  });
  video.addEventListener("click", function () {
    if (video.paused) { requestPlay(); } else { video.pause(); }
  });
  video.addEventListener("play", function () { playBtn.textContent = "❚❚"; });
  video.addEventListener("playing", hideTap);
  video.addEventListener("pause", function () { playBtn.textContent = "▶"; });

  if (tapEl) {
    tapEl.addEventListener("click", function () { requestPlay(); });
  }

  muteBtn.addEventListener("click", function () {
    video.muted = !video.muted;
    muteBtn.textContent = video.muted ? "🔇" : "🔊";
    if (!isIOS()) { volumeEl.value = video.muted ? 0 : video.volume; }
  });
  volumeEl.addEventListener("input", function () {
    video.volume = parseFloat(volumeEl.value);
    video.muted = video.volume === 0;
    muteBtn.textContent = video.muted ? "🔇" : "🔊";
  });

  /* -------------------------------- boot -------------------------------- */

  enforceInlinePlayback();
  showTap();

  if (isIOS()) {
    playerEl.classList.add("ari-ios");
    volumeEl.disabled = true;
    volumeEl.setAttribute("aria-hidden", "true");
  }

  switch (cfg.delivery) {
    case "dash":
      state.triedDASH = true;
      startDASH();
      break;
    case "hls":
      state.triedHLS = true;
      startHLS();
      break;
    default: // "all" (or any legacy value) -> HLS first, then DASH
      startHLS();
  }

  scheduleTokenRefresh();
})();
