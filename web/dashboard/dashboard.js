    const $ = (id) => document.getElementById(id);
    let base = location.origin;

    function toast(msg) {
      const t = $("toast");
      t.textContent = msg;
      t.classList.add("show");
      setTimeout(() => t.classList.remove("show"), 2200);
    }

    async function api(path, opts) {
      const res = await fetch(path, Object.assign({ headers: { "Content-Type": "application/json" }, credentials: "same-origin" }, opts));
      if (res.status === 401) { location.href = "/login.html"; throw new Error("authentication required"); }
      if (res.status === 204) return null;
      const text = await res.text();
      let body = null;
      try { body = text ? JSON.parse(text) : null; } catch (e) { body = text; }
      if (!res.ok) throw new Error((body && body.message) || res.statusText);
      return body;
    }

    async function ensureAuth() {
      try {
        const s = await api("/api/v1/session");
        if (s.authRequired) {
          const btn = document.getElementById("logout-btn");
          if (btn) { btn.hidden = false; }
          if (!s.authenticated) { location.href = "/login.html"; return false; }
        }
        return true;
      } catch (e) {
        return false;
      }
    }

    async function logout() {
      try { await api("/api/v1/logout", { method: "POST" }); } catch (e) { /* ignore */ }
      location.href = "/login.html";
    }

    function fmtUptime(sec) {
      sec = Math.max(0, sec | 0);
      const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
      return (h ? h + "h " : "") + (m ? m + "m " : "") + s + "s";
    }

    async function loadHealth() {
      try {
        const h = await api("/healthz");
        $("health").textContent = h.status === "ok" ? "online" : "unknown";
      } catch (e) { $("health").textContent = "offline"; }
    }

    async function loadStats() {
      try {
        const s = await api("/api/v1/stats");
        $("s-total").textContent = s.streamsTotal;
        $("s-live").textContent = s.streamsLive;
        $("s-viewers").textContent = s.viewers;
        $("s-uptime").textContent = fmtUptime(s.uptimeSeconds);
        $("s-recording").textContent = s.recording || 0;
        $("s-p2ppeers").textContent = s.p2pPeers || 0;
      } catch (e) { toast("stats: " + e.message); }
    }

    async function loadStreams() {
      const tbody = $("stream-rows");
      try {
        const data = await api("/api/v1/streams");
        if (!data.streams.length) { tbody.innerHTML = '<tr><td colspan="6" class="muted">No streams yet.</td></tr>'; return; }
        tbody.innerHTML = "";
        for (const s of data.streams) {
          const tr = document.createElement("tr");
          tr.innerHTML =
            '<td><code>' + escapeHtml(s.streamId) + '</code></td>' +
            '<td>' + escapeHtml(s.type) + '</td>' +
            '<td><span class="pill ' + s.status + '">' + s.status + '</span></td>' +
            '<td>' + deliverySelect(s.id, s.delivery || "all") + '</td>' +
            '<td>' + (s.viewerCount || 0) + '</td>' +
            '<td><div class="row-actions">' +
              '<button class="ghost" data-open="' + escapeHtml(s.streamId) + '">Player</button>' +
              '<button class="ghost" data-token="' + escapeHtml(s.id) + '">Token</button>' +
              '<button class="ghost" data-embed="' + escapeHtml(s.streamId) + '">Embed</button>' +
              '<button class="danger" data-del="' + escapeHtml(s.id) + '">Delete</button>' +
            '</div></td>';
          tbody.appendChild(tr);
        }
        tbody.querySelectorAll("[data-open]").forEach(b => b.onclick = () => openPlayer(b.dataset.open));
        tbody.querySelectorAll("[data-token]").forEach(b => b.onclick = () => showToken(b.dataset.token));
        tbody.querySelectorAll("[data-embed]").forEach(b => b.onclick = () => showEmbed(b.dataset.embed));
        tbody.querySelectorAll("[data-del]").forEach(b => b.onclick = () => deleteStream(b.dataset.del));
        tbody.querySelectorAll("[data-delivery]").forEach(sel => sel.onchange = () => setDelivery(sel.dataset.delivery, sel.value));
      } catch (e) { tbody.innerHTML = '<tr><td colspan="5" class="muted">' + escapeHtml(e.message) + '</td></tr>'; }
    }

    function openPlayer(streamId) {
      window.open("/player.html?id=" + encodeURIComponent(streamId), "_blank");
    }

    function embedCode(streamId) {
      return '<iframe src="' + base + '/player.html?id=' + streamId +
        '" width="960" height="540" frameborder="0" allow="autoplay"></iframe>';
    }

    function showEmbed(streamId) {
      $("quickstart").textContent = embedCode(streamId);
      navigator.clipboard && navigator.clipboard.writeText(embedCode(streamId)).then(
        () => toast("Embed code copied"),
        () => toast("Embed code ready below")
      );
    }

    async function createStream(ev) {
      ev.preventDefault();
      try {
        const body = { streamId: $("c-id").value.trim(), name: $("c-name").value.trim(), type: $("c-type").value, delivery: $("c-delivery").value };
        await api("/api/v1/streams", { method: "POST", body: JSON.stringify(body) });
        $("c-id").value = ""; $("c-name").value = "";
        toast("Stream created");
        loadAll();
      } catch (e) { toast("create: " + e.message); }
    }

    async function deleteStream(id) {
      if (!confirm("Delete this stream?")) return;
      try { await api("/api/v1/streams/" + encodeURIComponent(id), { method: "DELETE" }); toast("Deleted"); loadAll(); }
      catch (e) { toast("delete: " + e.message); }
    }

    async function loadDomains() {
      try {
        const d = await api("/api/v1/security/domains");
        $("allow-all").checked = !!d.allowAll;
        $("domains").value = (d.allowedDomains || []).join("\n");
      } catch (e) { toast("security: " + e.message); }
    }

    async function saveDomains() {
      try {
        const list = $("domains").value.split("\n").map(s => s.trim()).filter(Boolean);
        await api("/api/v1/security/domains", {
          method: "PUT",
          body: JSON.stringify({ allowedDomains: list, allowAll: $("allow-all").checked })
        });
        toast("Policy saved");
        loadDomains();
      } catch (e) { toast("save: " + e.message); }
    }

    function escapeHtml(s) {
      return String(s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
    }

    function deliverySelect(id, mode) {
      const opts = ["all", "webrtc", "hls", "dash"];
      let html = '<select data-delivery="' + escapeHtml(id) + '" style="flex:0; min-width:96px">';
      for (const o of opts) {
        html += '<option value="' + o + '"' + (o === mode ? " selected" : "") + '>' + o + '</option>';
      }
      return html + "</select>";
    }

    async function setDelivery(id, mode) {
      try {
        await api("/api/v1/streams/" + encodeURIComponent(id) + "/delivery", {
          method: "PUT", body: JSON.stringify({ delivery: mode })
        });
        toast("Delivery set to " + mode);
      } catch (e) { toast("delivery: " + e.message); }
    }

    async function showToken(id) {
      try {
        const d = await api("/api/v1/streams/" + encodeURIComponent(id) + "/token", { method: "POST" });
        const lines = ["stream: " + d.streamId + "   delivery: " + d.delivery];
        if (d.tokenEnabled) { lines.push("token: " + d.token); } else { lines.push("tokenization is off"); }
        if (d.hlsUrl) { lines.push("HLS:  " + base + d.hlsUrl); }
        if (d.dashUrl) { lines.push("DASH: " + base + d.dashUrl); }
        $("quickstart").textContent = lines.join("\n");
        toast("Tokenized URL ready below");
      } catch (e) { toast("token: " + e.message); }
    }

    async function loadTokenConfig() {
      try {
        const t = await api("/api/v1/security/token");
        $("token-enabled").checked = !!t.enabled;
        if (t.ttlSeconds) { $("token-ttl").value = t.ttlSeconds; }
      } catch (e) { toast("token config: " + e.message); }
    }

    async function saveTokenConfig() {
      try {
        await api("/api/v1/security/token", {
          method: "PUT",
          body: JSON.stringify({
            enabled: $("token-enabled").checked,
            ttlSeconds: parseInt($("token-ttl").value, 10) || 3600
          })
        });
        toast("Token settings saved");
        loadTokenConfig();
      } catch (e) { toast("save token: " + e.message); }
    }

    function fmtBytes(n) {
      n = n || 0;
      const u = ["B", "KB", "MB", "GB"];
      let i = 0;
      while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
      return n.toFixed(i === 0 ? 0 : 1) + " " + u[i];
    }

    async function loadRecordings() {
      const tbody = $("recording-rows");
      try {
        const data = await api("/api/v1/recordings");
        if (!data.enabled) { tbody.innerHTML = '<tr><td colspan="4" class="muted">Recording storage disabled.</td></tr>'; return; }
        if (!data.recordings.length) { tbody.innerHTML = '<tr><td colspan="4" class="muted">No recordings yet.</td></tr>'; return; }
        tbody.innerHTML = "";
        for (const r of data.recordings) {
          const tr = document.createElement("tr");
          const link = r.url ? '<a href="' + escapeHtml(r.url) + '" target="_blank" rel="noopener">open</a>' : "";
          tr.innerHTML =
            '<td><code>' + escapeHtml(r.key) + '</code></td>' +
            '<td>' + fmtBytes(r.size) + '</td>' +
            '<td>' + escapeHtml((r.modifiedAt || "").replace("T", " ").slice(0, 19)) + '</td>' +
            '<td><div class="row-actions">' + link +
              '<button class="danger" data-delrec="' + escapeHtml(r.key) + '">Delete</button></div></td>';
          tbody.appendChild(tr);
        }
        tbody.querySelectorAll("[data-delrec]").forEach(b => b.onclick = () => deleteRecording(b.dataset.delrec));
      } catch (e) {
        tbody.innerHTML = '<tr><td colspan="4" class="muted">' + escapeHtml(e.message) + '</td></tr>';
      }
    }

    async function deleteRecording(key) {
      if (!confirm("Delete recording " + key + "?")) return;
      try {
        await api("/api/v1/recordings/" + key.split("/").map(encodeURIComponent).join("/"), { method: "DELETE" });
        toast("Recording deleted");
        loadRecordings();
      } catch (e) { toast("delete: " + e.message); }
    }

    async function loadP2P() {
      try {
        const p = await api("/api/v1/p2p");
        $("p2p-enabled").checked = !!p.enabled;
        $("p2p-count").textContent = p.peers || 0;
      } catch (e) { toast("p2p: " + e.message); }
    }

    async function saveP2P() {
      try {
        await api("/api/v1/p2p", { method: "PUT", body: JSON.stringify({ enabled: $("p2p-enabled").checked }) });
        toast("P2P settings saved");
        loadP2P();
      } catch (e) { toast("p2p: " + e.message); }
    }

    function loadAll() { loadHealth(); loadStats(); loadStreams(); loadRecordings(); loadDomains(); loadTokenConfig(); loadP2P(); }

    // Wire controls via addEventListener (inline handlers are blocked by the CSP).
    function wireControls() {
      const on = (id, fn) => { const el = document.getElementById(id); if (el) { el.addEventListener("click", fn); } };
      on("refresh-btn", loadAll);
      on("logout-btn", logout);
      on("save-domains-btn", saveDomains);
      on("reset-domains-btn", loadDomains);
      on("save-token-btn", saveTokenConfig);
      on("reset-token-btn", loadTokenConfig);
      on("save-p2p-btn", saveP2P);
      on("reset-p2p-btn", loadP2P);
    }
    wireControls();

    $("create-form").addEventListener("submit", createStream);
    $("quickstart").textContent =
      '1. Create a stream above.\n' +
      '2. Publish via WebRTC or RTMP (rtmp://' + location.hostname + ':1935/live/<streamId>).\n' +
      '3. Embed: ' + base + '/player.html?id=<streamId>';

    ensureAuth().then((ok) => {
      if (!ok) return;
      loadAll();
      setInterval(() => { loadStats(); loadStreams(); }, 5000);
    });