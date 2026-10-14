/* Ari P2P engine — peer-assisted delivery for HLS and DASH.
 *
 * A SwarmCloud-style P2P layer that sits between the player and the network.
 * Viewers of the same stream form a WebRTC data-channel mesh (coordinated by the
 * engine's tracker at /p2p/ws), exchange video segments directly, and fall back
 * to the origin/CDN for anything a peer does not have.
 *
 * Usage (HLS via hls.js):
 *   const p2p = AriP2P.create({ channel: streamId });
 *   p2p.start();
 *   new Hls({ loader: p2p.hlsLoader(Hls), ... });
 *
 * Usage (DASH via dash.js):
 *   const p2p = AriP2P.create({ channel: streamId });
 *   p2p.start();
 *   const player = dashjs.MediaPlayer().create();
 *   p2p.setupDash(player);
 *   player.initialize(video, mpdUrl, true);
 */
(function (global) {
  "use strict";

  // Binary data-channel message types.
  var MSG_REQ = 1, MSG_RES = 2, MSG_CHUNK = 3, MSG_HAVE = 4, MSG_MISS = 5;

  function encodeText(bytes) {
    var out = new Uint8Array(bytes.length);
    out.set(bytes);
    return out;
  }

  function AriP2P(config) {
    config = config || {};
    this.wsPath = config.wsPath || "/p2p/ws";
    this.channel = config.channel || "";
    this.maxPeers = config.maxPeers || 4;
    this.peerTimeoutMs = config.peerTimeoutMs || 2500;
    this.log = config.debug ? console.log.bind(console) : function () {};

    this.peerId = null;
    this.ws = null;
    this.peers = {};          // peerId -> { pc, dc, have:Set, pending:Map, seq }
    this.segmentStore = {};   // url -> Uint8Array
    this.haveSet = {};        // url -> true
    this.inflight = {};       // url -> Promise
    this.reqSeq = 1;
    this.destroyed = false;
  }

  AriP2P.create = function (config) { return new AriP2P(config); };

  /* ------------------------------- tracker ------------------------------- */

  AriP2P.prototype.start = function () {
    var self = this;
    var proto = global.location.protocol === "https:" ? "wss:" : "ws:";
    var ws = new WebSocket(proto + "//" + global.location.host + this.wsPath);
    this.ws = ws;
    ws.onopen = function () { self.send({ type: "join", channel: self.channel }); };
    ws.onmessage = function (ev) {
      var m;
      try { m = JSON.parse(ev.data); } catch (e) { return; }
      self.onMessage(m);
    };
    ws.onclose = function () { self.ws = null; };
    ws.onerror = function () {};
    return this;
  };

  AriP2P.prototype.destroy = function () {
    this.destroyed = true;
    if (this.ws) { try { this.ws.close(); } catch (e) {} }
    var self = this;
    Object.keys(this.peers).forEach(function (pid) { self.dropPeer(pid); });
  };

  AriP2P.prototype.send = function (m) {
    if (this.ws && this.ws.readyState === 1) { this.ws.send(JSON.stringify(m)); }
  };

  AriP2P.prototype.onMessage = function (m) {
    var self = this;
    switch (m.type) {
      case "joined":
        this.peerId = m.peerId;
        (m.peers || []).slice(0, this.maxPeers).forEach(function (pid) { self.connectTo(pid, true); });
        break;
      case "peer-joined":
        break; // wait for the newcomer's offer
      case "peer-left":
        this.dropPeer(m.peerId);
        break;
      case "signal":
        this.handleSignal(m);
        break;
      default:
        break;
    }
  };

  /* ------------------------------- WebRTC -------------------------------- */

  AriP2P.prototype.connectTo = function (peerId, initiator) {
    if (this.peers[peerId]) { return this.peers[peerId]; }
    var self = this;
    var pc = new RTCPeerConnection({ iceServers: [] });
    var entry = { pc: pc, dc: null, have: {}, pending: {}, seq: 1 };
    this.peers[peerId] = entry;

    pc.onicecandidate = function (ev) {
      if (ev.candidate) {
        self.send({
          type: "signal", to: peerId,
          candidate: ev.candidate.candidate,
          sdpMid: ev.candidate.sdpMid,
          sdpMLineIndex: ev.candidate.sdpMLineIndex
        });
      }
    };
    pc.onconnectionstatechange = function () {
      if (pc.connectionState === "failed" || pc.connectionState === "closed") { self.dropPeer(peerId); }
    };
    pc.ondatachannel = function (ev) { self.setupDataChannel(peerId, ev.channel); };

    if (initiator) {
      var dc = pc.createDataChannel("segments", { ordered: true });
      this.setupDataChannel(peerId, dc);
      pc.createOffer()
        .then(function (offer) { return pc.setLocalDescription(offer); })
        .then(function () { self.send({ type: "signal", to: peerId, sdp: pc.localDescription.sdp, sdpType: "offer" }); })
        .catch(function () { self.dropPeer(peerId); });
    }
    return entry;
  };

  AriP2P.prototype.handleSignal = function (m) {
    var self = this;
    var entry = this.peers[m.from] || this.connectTo(m.from, false);
    var pc = entry.pc;

    if (m.sdp) {
      var desc = { type: m.sdpType, sdp: m.sdp };
      pc.setRemoteDescription(desc).then(function () {
        if (desc.type === "offer") {
          return pc.createAnswer()
            .then(function (answer) { return pc.setLocalDescription(answer); })
            .then(function () { self.send({ type: "signal", to: m.from, sdp: pc.localDescription.sdp, sdpType: "answer" }); });
        }
      }).catch(function () {});
    } else if (m.candidate) {
      pc.addIceCandidate(new RTCIceCandidate({ candidate: m.candidate, sdpMid: m.sdpMid, sdpMLineIndex: m.sdpMLineIndex }))
        .catch(function () {});
    }
  };

  AriP2P.prototype.dropPeer = function (peerId) {
    var entry = this.peers[peerId];
    if (!entry) { return; }
    delete this.peers[peerId];
    Object.keys(entry.pending).forEach(function (id) {
      var p = entry.pending[id];
      if (p && p.reject) { p.reject(new Error("peer gone")); }
    });
    try { if (entry.dc) { entry.dc.close(); } } catch (e) {}
    try { entry.pc.close(); } catch (e) {}
  };

  /* --------------------------- data channel ------------------------------ */

  AriP2P.prototype.setupDataChannel = function (peerId, dc) {
    var self = this;
    var entry = this.peers[peerId];
    if (!entry) { return; }
    entry.dc = dc;
    dc.binaryType = "arraybuffer";
    dc.onopen = function () {
      Object.keys(self.haveSet).forEach(function (url) { self.sendHave(peerId, url); });
    };
    dc.onmessage = function (ev) { self.onData(peerId, ev.data); };
    dc.onclose = function () { self.dropPeer(peerId); };
  };

  AriP2P.prototype.sendHave = function (peerId, url) {
    var entry = this.peers[peerId];
    if (!entry || !entry.dc || entry.dc.readyState !== "open") { return; }
    var urlBytes = new TextEncoder().encode(url);
    var buf = new Uint8Array(1 + urlBytes.length);
    buf[0] = MSG_HAVE;
    buf.set(urlBytes, 1);
    entry.dc.send(buf.buffer);
  };

  AriP2P.prototype.onData = function (peerId, data) {
    var entry = this.peers[peerId];
    if (!entry) { return; }
    var view = new Uint8Array(data);
    var type = view[0];
    var dv = new DataView(data);

    if (type === MSG_HAVE) {
      var url = new TextDecoder().decode(view.subarray(1));
      entry.have[url] = true;
      return;
    }
    if (type === MSG_REQ) {
      var reqId = dv.getUint32(1);
      var reqUrl = new TextDecoder().decode(view.subarray(5));
      this.serveRequest(peerId, reqId, reqUrl);
      return;
    }
    if (type === MSG_RES) {
      var rid = dv.getUint32(1);
      var ok = view[5] === 1;
      var total = dv.getUint32(6);
      var p = entry.pending[rid];
      if (!p) { return; }
      if (!ok) { delete entry.pending[rid]; p.reject(new Error("peer miss")); return; }
      p.expected = total;
      p.chunks = [];
      p.received = 0;
      return;
    }
    if (type === MSG_CHUNK) {
      var cid = dv.getUint32(1);
      var cp = entry.pending[cid];
      if (!cp) { return; }
      var payload = view.subarray(5);
      cp.chunks.push(payload);
      cp.received += payload.length;
      if (cp.received >= cp.expected) {
        delete entry.pending[cid];
        var out = new Uint8Array(cp.expected);
        var off = 0;
        cp.chunks.forEach(function (c) { out.set(c, off); off += c.length; });
        cp.resolve(out);
      }
      return;
    }
    if (type === MSG_MISS) {
      var mid = dv.getUint32(1);
      var mp = entry.pending[mid];
      if (mp) { delete entry.pending[mid]; mp.reject(new Error("peer miss")); }
      return;
    }
  };

  AriP2P.prototype.serveRequest = function (peerId, reqId, url) {
    var entry = this.peers[peerId];
    var data = this.segmentStore[url];
    var dc = entry && entry.dc;
    if (!dc || dc.readyState !== "open") { return; }

    var miss = new Uint8Array(5);
    miss[0] = MSG_MISS;
    new DataView(miss.buffer).setUint32(1, reqId);
    if (!data) { dc.send(miss.buffer); return; }

    var res = new Uint8Array(10);
    res[0] = MSG_RES;
    var rdv = new DataView(res.buffer);
    rdv.setUint32(1, reqId);
    res[5] = 1;
    rdv.setUint32(6, data.length);
    dc.send(res.buffer);

    var CHUNK = 16384;
    for (var off = 0; off < data.length; off += CHUNK) {
      var slice = data.subarray(off, Math.min(off + CHUNK, data.length));
      var frame = new Uint8Array(5 + slice.length);
      frame[0] = MSG_CHUNK;
      new DataView(frame.buffer).setUint32(1, reqId);
      frame.set(slice, 5);
      dc.send(frame.buffer);
    }
  };

  AriP2P.prototype.requestFromPeer = function (peerId, url) {
    var self = this;
    var entry = this.peers[peerId];
    if (!entry || !entry.dc || entry.dc.readyState !== "open") {
      return Promise.reject(new Error("peer not connected"));
    }
    return new Promise(function (resolve, reject) {
      var id = self.reqSeq++;
      var timer = setTimeout(function () {
        if (entry.pending[id]) { delete entry.pending[id]; reject(new Error("timeout")); }
      }, self.peerTimeoutMs);
      entry.pending[id] = {
        resolve: function (data) { clearTimeout(timer); resolve(data); },
        reject: function (e) { clearTimeout(timer); reject(e); }
      };
      var urlBytes = new TextEncoder().encode(url);
      var buf = new Uint8Array(5 + urlBytes.length);
      buf[0] = MSG_REQ;
      new DataView(buf.buffer).setUint32(1, id);
      buf.set(urlBytes, 5);
      entry.dc.send(buf.buffer);
    });
  };

  /* ------------------------------- fetching ------------------------------ */

  AriP2P.prototype.fetchFromPeers = function (url) {
    var self = this;
    var candidates = Object.keys(this.peers).filter(function (pid) {
      var e = self.peers[pid];
      return e.have[url] && e.dc && e.dc.readyState === "open";
    });
    if (!candidates.length) { return Promise.reject(new Error("no peers")); }

    var idx = 0;
    function attempt() {
      if (idx >= candidates.length) { return Promise.reject(new Error("all peers failed")); }
      var pid = candidates[idx++];
      return self.requestFromPeer(pid, url).catch(attempt);
    }
    return attempt();
  };

  AriP2P.prototype.fetchFromNetwork = function (url) {
    if (typeof fetch !== "function") { return Promise.reject(new Error("no fetch")); }
    return fetch(url, { credentials: "same-origin" }).then(function (res) {
      if (!res.ok) { throw new Error("http " + res.status); }
      return res.arrayBuffer();
    }).then(function (buf) { return new Uint8Array(buf); });
  };

  // fetch resolves a segment from a peer when possible, otherwise the network,
  // and then announces it to the mesh.
  AriP2P.prototype.fetch = function (url) {
    var self = this;
    if (this.segmentStore[url]) { return Promise.resolve(this.segmentStore[url]); }
    if (this.inflight[url]) { return this.inflight[url]; }

    var p = this.fetchFromPeers(url)
      .then(function (data) { self.log("p2p hit", url); return data; })
      .catch(function () { return self.fetchFromNetwork(url); })
      .then(function (data) {
        self.store(url, data);
        delete self.inflight[url];
        return data;
      }, function (e) {
        delete self.inflight[url];
        throw e;
      });
    this.inflight[url] = p;
    return p;
  };

  AriP2P.prototype.store = function (url, data) {
    if (!data || !data.length) { return; }
    if (!this.haveSet[url]) {
      this.haveSet[url] = true;
      this.send({ type: "have", segments: [url] });
      var self = this;
      Object.keys(this.peers).forEach(function (pid) { self.sendHave(pid, url); });
    }
    this.segmentStore[url] = data;
  };

  /* ---------------------------- player hooks ----------------------------- */

  // hlsLoader returns a hls.js loader class that sources fragments through P2P.
  AriP2P.prototype.hlsLoader = function (Hls) {
    var self = this;
    var DefaultLoader = Hls && Hls.DefaultConfig ? Hls.DefaultConfig.loader : null;

    function P2PLoader(config) {
      this.config = config;
      this.fallback = DefaultLoader ? new DefaultLoader(config) : null;
    }
    P2PLoader.prototype.load = function (context, config, callbacks) {
      var self2 = this;
      if (context.type === "fragment") {
        self.fetch(context.url).then(function (data) {
          var response = { url: context.url, data: data };
          callbacks.onSuccess(response, { trequest: 0, tfirst: 0, tload: 0, loaded: data.length, total: data.length }, context);
        }).catch(function () {
          if (self2.fallback) { self2.fallback.load(context, config, callbacks); }
          else { callbacks.onError({ code: 0, text: "p2p and network fetch failed" }, context); }
        });
        return;
      }
      if (this.fallback) { this.fallback.load(context, config, callbacks); }
      else { callbacks.onError({ code: 0, text: "no fallback loader" }, context); }
    };
    P2PLoader.prototype.abort = function () { if (this.fallback && this.fallback.abort) { this.fallback.abort(); } };
    P2PLoader.prototype.destroy = function () { if (this.fallback && this.fallback.destroy) { this.fallback.destroy(); } };
    return P2PLoader;
  };

  // setupDash overrides dash.js's XHRLoader so segment requests go through P2P.
  // Best-effort: dash.js versions expose this differently, so failures fall
  // back silently to the default loader.
  AriP2P.prototype.setupDash = function (player) {
    var self = this;
    try {
      var Dash = global.dashjs;
      if (!player || typeof player.extend !== "function" || !Dash || !Dash.dependencies || !Dash.dependencies.XHRLoader) {
        return false;
      }
      var Default = Dash.dependencies.XHRLoader;
      function P2PXHRLoader() { Default.call(this); }
      P2PXHRLoader.prototype = Object.create(Default.prototype);
      P2PXHRLoader.prototype.constructor = P2PXHRLoader;
      P2PXHRLoader.prototype.load = function (config, request, success, error, progress, timeout) {
        var url = typeof request === "string" ? request : (request && request.url) || "";
        if (typeof url !== "string" || url.indexOf(".mpd") !== -1 || url.indexOf(".mp4") === -1) {
          return Default.prototype.load.call(this, config, request, success, error, progress, timeout);
        }
        self.fetch(url).then(function (data) {
          if (typeof success === "function") { success(data, { url: url }, undefined); }
        }).catch(function () {
          Default.prototype.load.call(this, config, request, success, error, progress, timeout);
        }.bind(this));
      };
      player.extend("XHRLoader", P2PXHRLoader);
      return true;
    } catch (e) {
      return false;
    }
  };

  global.AriP2P = AriP2P;
})(typeof window !== "undefined" ? window : this);
