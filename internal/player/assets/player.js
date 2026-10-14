/* Ari Media Engine — built-in HTML5 player runtime.
 * Plays the stream with Clappr (HLS; Clappr bundles hls.js and falls back to
 * native playback on iOS), and dash.js for DASH. Autoplay is disabled so
 * playback starts (with audio) on a user tap. When tokenization is enabled the
 * page fetches a fresh signed token a few seconds before the current one
 * expires and reloads the source in place. No build step. */
(function () {
  "use strict";

  var cfgEl = document.getElementById("ari-config");
  var cfg = {};
  try { cfg = JSON.parse((cfgEl && cfgEl.textContent) || "{}"); } catch (e) { cfg = {}; }

  var container = document.getElementById("ari-player");

  var state = {
    player: null,
    dash: null,
    video: null,
    mode: "none",
    tokenTimer: null
  };

  // hls.js configuration passed through Clappr to its bundled hls.js.
  var hlsjsConfig = {
    maxBufferSize: 0,
    maxBufferLength: 10,
    liveSyncDurationCount: 3
  };

  // Peer-assisted delivery: Clappr bundles its own hls.js, so a custom loader
  // cannot be injected; the tracker is still started for other players.
  if (cfg.p2p && cfg.p2p.enabled && window.AriP2P) {
    try {
      var p2p = window.AriP2P.create({ channel: cfg.streamId, wsPath: cfg.p2p.wsPath });
      p2p.start();
    } catch (e) { /* ignore */ }
  }

  function nativeVideo(url) {
    var v = document.createElement("video");
    v.setAttribute("playsinline", "");
    v.setAttribute("webkit-playsinline", "");
    v.setAttribute("controls", "");
    v.setAttribute("preload", "auto");
    v.style.width = "100%";
    v.style.height = "100%";
    v.style.objectFit = "contain";
    v.style.background = "#000";
    v.src = url;
    container.appendChild(v);
    state.video = v;
    return v;
  }

  function useHLS(url) {
    state.mode = "hls";
    if (!window.Clappr) { nativeVideo(url); return; }
    state.player = new window.Clappr.Player({
      source: url,
      parentId: "#ari-player",
      autoPlay: false,
      mute: false,
      width: "100%",
      height: "100%",
      playback: { hlsjsConfig: hlsjsConfig }
    });
  }

  function useDASH(url) {
    state.mode = "dash";
    if (!window.dashjs || !window.dashjs.MediaPlayer) { nativeVideo(url); return; }
    var video = nativeVideo("");
    var player = window.dashjs.MediaPlayer().create();
    player.initialize(video, url, false);
    state.dash = player;
  }

  function start() {
    if (cfg.delivery === "dash" && cfg.dashUrl) { useDASH(cfg.dashUrl); return; }
    if (cfg.hlsUrl) { useHLS(cfg.hlsUrl); return; }
    if (cfg.dashUrl) { useDASH(cfg.dashUrl); return; }
  }

  /* --------------------------- token refresh ---------------------------- */

  function reloadSource() {
    if (state.mode === "hls" && cfg.hlsUrl) {
      if (state.player) {
        try { state.player.load(cfg.hlsUrl); } catch (e) { /* ignore */ }
      } else if (state.video) {
        state.video.src = cfg.hlsUrl;
      }
    } else if (state.mode === "dash" && state.dash && cfg.dashUrl) {
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

  /* -------------------------------- boot -------------------------------- */

  start();
  scheduleTokenRefresh();
})();
