/* Ari Media Engine — built-in HTML5 player runtime.
 * Selects a playback protocol from the stream's delivery mode:
 *   webrtc -> WebRTC only
 *   hls    -> HLS only
 *   dash   -> DASH only
 *   all    -> WebRTC first, then HLS, then DASH
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
  var latencyBtn = document.getElementById("ari-latency");
  var latencyOut = document.getElementById("ari-latency-readout");

  var state = {
    pc: null,
    ws: null,
    hls: null,
    dash: null,
    mode: "none",
    lowLatency: cfg.lowLatency !== false,
    latencyTimer: null,
    triedHLS: false,
    triedDASH: false
  };

  function setStatus(kind, text) {
    statusEl.dataset.state = kind;
    statusText.textContent = text;
  }

  function signalingURL() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + (cfg.wsPath || "/ws");
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

  function requestPlay() {
    var p = video.play();
    if (p && typeof p.then === "function") {
      p.then(hideTap).catch(function () { showTap(); });
    }
  }

  /* ------------------------------- WebRTC ------------------------------- */

  function startWebRTC() {
    setStatus("connecting", "Connecting · WebRTC");
    state.mode = "webrtc";

    var pc = new RTCPeerConnection({ iceServers: [] });
    state.pc = pc;

    pc.ontrack = function (ev) {
      if (ev.streams && ev.streams[0]) {
        video.srcObject = ev.streams[0];
      } else {
        if (!video.srcObject) { video.srcObject = new MediaStream(); }
        video.srcObject.addTrack(ev.track);
      }
      requestPlay();
    };

    pc.onconnectionstatechange = function () {
      switch (pc.connectionState) {
        case "connected":
          setStatus("live", "Live · WebRTC");
          startLatency();
          break;
        case "failed":
        case "disconnected":
        case "closed":
          if (state.mode === "webrtc") { nextFallback("connection lost"); }
          break;
      }
    };

    var ws = new WebSocket(signalingURL());
    state.ws = ws;

    ws.onopen = function () {
      ws.send(JSON.stringify({ type: "subscribe", streamId: cfg.streamId }));
    };

    ws.onmessage = function (ev) {
      var msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      if (msg.type === "offer" && msg.sdp) {
        pc.setRemoteDescription({ type: "offer", sdp: msg.sdp })
          .then(function () { return pc.createAnswer(); })
          .then(function (answer) { return pc.setLocalDescription(answer); })
          .then(function () {
            ws.send(JSON.stringify({
              type: "subscriber-answer",
              streamId: cfg.streamId,
              sdp: pc.localDescription.sdp
            }));
          })
          .catch(function () { nextFallback("negotiation failed"); });
      } else if (msg.type === "error") {
        nextFallback(msg.error || "signaling error");
      }
    };

    ws.onerror = function () { nextFallback("signaling unavailable"); };
    ws.onclose = function () {
      if (state.mode === "webrtc") { nextFallback("signaling closed"); }
    };
  }

  function teardownWebRTC() {
    if (state.ws) { try { state.ws.close(); } catch (e) {} state.ws = null; }
    if (state.pc) { try { state.pc.close(); } catch (e) {} state.pc = null; }
  }

  /* ---------------------------- HLS / DASH ------------------------------ */

  // nextFallback walks the delivery chain: HLS, then DASH, then error.
  function nextFallback(reason) {
    teardownWebRTC();
    stopLatency();
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
      var hls = new window.Hls({
        lowLatencyMode: state.lowLatency,
        liveSyncDurationCount: state.lowLatency ? 2 : 3
      });
      state.hls = hls;
      hls.on(window.Hls.Events.MANIFEST_PARSED, function () {
        setStatus("live", "Live · HLS");
        startLatency();
        requestPlay();
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
      startLatency();
    }, { once: true });
    video.addEventListener("error", function () { nextFallback("HLS playback error"); }, { once: true });
    requestPlay();
  }

  function startDASH() {
    state.mode = "dash";
    setStatus("connecting", "Connecting · DASH");
    var url = cfg.dashUrl;
    if (!url) { nextFallback("no DASH source"); return; }

    if (window.dashjs && window.dashjs.MediaPlayer) {
      var player = window.dashjs.MediaPlayer().create();
      state.dash = player;
      player.initialize(video, url, true);
      player.on(window.dashjs.MediaPlayer.events.PLAYBACK_PLAYING, function () {
        setStatus("live", "Live · DASH");
        startLatency();
      });
      player.on(window.dashjs.MediaPlayer.events.ERROR, function () {
        nextFallback("DASH error");
      });
      return;
    }
    setStatus("error", "DASH playback requires dash.js (set player.dash_js_url)");
  }

  /* ------------------------------ latency ------------------------------- */

  function startLatency() {
    stopLatency();
    state.latencyTimer = setInterval(function () {
      try {
        if (video.buffered && video.buffered.length) {
          var end = video.buffered.end(video.buffered.length - 1);
          var behind = Math.max(0, end - video.currentTime);
          latencyOut.textContent = Math.round(behind * 1000) + " ms";
        }
      } catch (e) { /* ignore */ }
    }, 1000);
  }

  function stopLatency() {
    if (state.latencyTimer) { clearInterval(state.latencyTimer); state.latencyTimer = null; }
    latencyOut.textContent = "–";
  }

  /* ------------------------------ controls ------------------------------ */

  function applyLowLatency(on) {
    state.lowLatency = on;
    latencyBtn.setAttribute("aria-pressed", on ? "true" : "false");
    video.playbackRate = on ? 1.05 : 1.0;
    if ("jitterBufferTarget" in video) {
      try { video.jitterBufferTarget = on ? 200 : 1000; } catch (e) { /* ignore */ }
    }
    if (state.hls) { state.hls.lowLatencyMode = on; }
  }

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

  latencyBtn.addEventListener("click", function () { applyLowLatency(!state.lowLatency); });

  ["pointerdown", "touchstart", "keydown"].forEach(function (evt) {
    document.addEventListener(evt, function onGesture() {
      document.removeEventListener(evt, onGesture);
      if (video.paused) { requestPlay(); }
    }, { passive: true });
  });

  /* -------------------------------- boot -------------------------------- */

  enforceInlinePlayback();
  applyLowLatency(state.lowLatency);

  if (isIOS()) {
    playerEl.classList.add("ari-ios");
    volumeEl.disabled = true;
    volumeEl.setAttribute("aria-hidden", "true");
  }

  switch (cfg.delivery) {
    case "hls":
      state.triedHLS = true;
      startHLS();
      break;
    case "dash":
      state.triedDASH = true;
      startDASH();
      break;
    case "webrtc":
      if (window.RTCPeerConnection) { startWebRTC(); } else { setStatus("error", "WebRTC is not supported by this browser"); }
      break;
    default: // "all"
      if (window.RTCPeerConnection) { startWebRTC(); } else { nextFallback("WebRTC unsupported"); }
  }
})();
