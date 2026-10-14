    var form = document.getElementById("login-form");
    var errEl = document.getElementById("error");

    form.addEventListener("submit", function (e) {
      e.preventDefault();
      errEl.hidden = true;
      var body = {
        username: document.getElementById("username").value,
        password: document.getElementById("password").value
      };
      fetch("/api/v1/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "same-origin",
        body: JSON.stringify(body)
      }).then(function (res) {
        if (res.ok) { location.href = "/"; return null; }
        return res.json().catch(function () { return {}; }).then(function (data) {
          errEl.textContent = data.message || "Sign-in failed";
          errEl.hidden = false;
        });
      }).catch(function () {
        errEl.textContent = "Network error";
        errEl.hidden = false;
      });
    });

    // If authentication is disabled, skip straight to the console.
    fetch("/api/v1/session", { credentials: "same-origin" })
      .then(function (r) { return r.json(); })
      .then(function (s) { if (!s.authRequired) { location.href = "/"; } })
      .catch(function () {});