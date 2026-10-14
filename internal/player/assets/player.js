/* Ari Media Engine — built-in HTML5 player runtime.
 * Chooses WebRTC for sub-second latency and transparently falls back to HLS/DASH.
 * No build step, no framework: drop it in an iframe and go. */
(function () {
  "use strict";

  var cfgEl = document.getElementById("ari-config");
  var cfg = {};
  try { cfg = JSON.parse((cfgEl && cfgEl.textContent) || "{}"); } catch (e) { cfg = {}; }

  var video = document.getElementById("ari-video");
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
    mode: "none",
    lowLatency: cfg.lowLatency !== false,
    latencyTimer: null,
    fallbackTried: false
  };

  function setStatus(kind, text) {
    statusEl.dataset.state = kind;
    statusText.textContent = text;
  }

  function signalingURL() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + (cfg.wsPath || "/ws");
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
      video.play().catch(function () {});
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
          if (state.mode === "webrtc") { fallback("connection lost"); }
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
          .catch(function () { fallback("negotiation failed"); });
      } else if (msg.type === "error") {
        fallback(msg.error || "signaling error");
      }
    };

    ws.onerror = function () { fallback("signaling unavailable"); };
    ws.onclose = function () {
      if (state.mode === "webrtc") { fallback("signaling closed"); }
    };
  }

  function teardownWebRTC() {
    if (state.ws) { try { state.ws.close(); } catch (e) {} state.ws = null; }
    if (state.pc) { try { state.pc.close(); } catch (e) {} state.pc = null; }
    stopLatency();
  }

  /* ---------------------------- HLS / DASH ------------------------------ */

  function fallback(reason) {
    if (state.fallbackTried) { return; }
    state.fallbackTried = true;
    teardownWebRTC();

    var url = cfg.hlsUrl;
    if (!url) { setStatus("error", "No fallback source available"); return; }

    state.mode = "hls";
    setStatus("connecting", "Falling back · HLS");

    if (window.Hls && window.Hls.isSupported()) {
      var hls = new window.Hls({
        lowLatencyMode: state.lowLatency,
        liveSyncDurationCount: state.lowLatency ? 2 : 3
      });
      state.hls = hls;
      hls.on(window.Hls.Events.MANIFEST_PARSED, function () {
        setStatus("live", "Live · HLS");
        startLatency();
        video.play().catch(function () {});
      });
      hls.on(window.Hls.Events.ERROR, function (_, data) {
        if (data && data.fatal) { setStatus("error", "HLS playback error"); }
      });
      hls.loadSource(url);
      hls.attachMedia(video);
      return;
    }

    video.src = url;
    video.addEventListener("loadedmetadata", function () {
      setStatus("live", "Live · HLS");
      startLatency();
    }, { once: true });
    video.addEventListener("error", function () { setStatus("error", "Playback failed"); }, { once: true });
    video.play().catch(function () {});
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
    if (video.paused) { video.play().catch(function () {}); } else { video.pause(); }
  });
  video.addEventListener("click", function () {
    if (video.paused) { video.play().catch(function () {}); } else { video.pause(); }
  });
  video.addEventListener("play", function () { playBtn.textContent = "❚❚"; });
  video.addEventListener("pause", function () { playBtn.textContent = "▶"; });

  muteBtn.addEventListener("click", function () {
    video.muted = !video.muted;
    muteBtn.textContent = video.muted ? "🔇" : "🔊";
    volumeEl.value = video.muted ? 0 : video.volume;
  });
  volumeEl.addEventListener("input", function () {
    video.volume = parseFloat(volumeEl.value);
    video.muted = video.volume === 0;
    muteBtn.textContent = video.muted ? "🔇" : "🔊";
  });

  latencyBtn.addEventListener("click", function () { applyLowLatency(!state.lowLatency); });

  /* -------------------------------- boot -------------------------------- */

  applyLowLatency(state.lowLatency);

  if (!window.RTCPeerConnection) {
    fallback("WebRTC unsupported");
  } else {
    startWebRTC();
  }
})();
