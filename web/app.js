const S = {
  api: localStorage.getItem("pg_api") || location.origin,
  token: localStorage.getItem("pg_token") || "",
  user: JSON.parse(localStorage.getItem("pg_user") || "null"),
  invite: localStorage.getItem("pg_invite") || "",
  firebase: JSON.parse(localStorage.getItem("pg_firebase") || "null"),
  tab: "home",
};

function saveAuth() {
  localStorage.setItem("pg_token", S.token || "");
  localStorage.setItem("pg_user", JSON.stringify(S.user));
  localStorage.setItem("pg_invite", S.invite || "");
  localStorage.setItem("pg_api", S.api);
  localStorage.setItem("pg_firebase", JSON.stringify(S.firebase));
}

function el(html) {
  const d = document.createElement("div");
  d.innerHTML = html.trim();
  return d.firstElementChild;
}

function rupees(paise) {
  return "₹" + (Number(paise || 0) / 100).toLocaleString("en-IN", { maximumFractionDigits: 0 });
}

async function api(path, opts = {}) {
  const headers = Object.assign({ Accept: "application/json" }, opts.headers || {});
  if (S.token) headers.Authorization = "Bearer " + S.token;
  if (opts.body && !(opts.body instanceof FormData) && !headers["Content-Type"]) {
    headers["Content-Type"] = "application/json";
  }
  const res = await fetch(S.api + path, Object.assign({}, opts, { headers }));
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!res.ok) {
    const err = new Error((data && data.error) || res.statusText);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

function render(node) {
  const app = document.getElementById("app");
  app.innerHTML = "";
  app.appendChild(node);
}

function banner(err) {
  return err ? `<p class="err">${esc(err.message || String(err))}</p>` : "";
}

function esc(s) {
  return String(s || "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function logout() {
  S.token = "";
  S.user = null;
  saveAuth();
  route();
}

async function route() {
  if (!S.token || !S.user) return showLanding();
  if (S.user.role === "owner") return showOwner();
  if (S.user.role === "tenant" && !S.user.tenant_id) return showWaiting();
  return showTenant();
}

function showLanding(err) {
  const n = el(`<main>
    <h1>PG / Hostel</h1>
    <p>Scan or enter the invite from your owner. Owners just sign in.</p>
    ${banner(err)}
    <div class="card">
      <label>Invite code</label>
      <input id="invite" value="${esc(S.invite)}" placeholder="e.g. DEVINV01" autocomplete="off">
      <div class="row">
        <button id="continue">Continue</button>
        <button class="secondary" id="owner">I am the owner</button>
      </div>
      <p class="muted" id="prop"></p>
    </div>
    <details class="card">
      <summary>Settings</summary>
      <label>API base</label>
      <input id="api" value="${esc(S.api)}">
      <label>Firebase apiKey</label>
      <input id="fk" value="${esc(S.firebase && S.firebase.apiKey)}">
      <label>Firebase authDomain</label>
      <input id="fd" value="${esc(S.firebase && S.firebase.authDomain)}">
      <label>Firebase projectId</label>
      <input id="fp" value="${esc(S.firebase && S.firebase.projectId)}">
      <div class="row"><button class="secondary" id="save">Save settings</button></div>
    </details>
  </main>`);
  n.querySelector("#continue").onclick = async () => {
    S.invite = n.querySelector("#invite").value.trim().toUpperCase();
    saveAuth();
    if (!S.invite) return showLanding(new Error("Enter the invite code from your owner."));
    try {
      const p = await api("/join/invite/" + encodeURIComponent(S.invite));
      n.querySelector("#prop").textContent = p.property_name + " · " + p.owner_name;
      showLogin();
    } catch (e) {
      showLanding(e);
    }
  };
  n.querySelector("#owner").onclick = () => {
    S.invite = "";
    saveAuth();
    showLogin();
  };
  n.querySelector("#save").onclick = () => {
    S.api = n.querySelector("#api").value.replace(/\/$/, "");
    S.firebase = {
      apiKey: n.querySelector("#fk").value.trim(),
      authDomain: n.querySelector("#fd").value.trim(),
      projectId: n.querySelector("#fp").value.trim(),
    };
    saveAuth();
  };
  render(n);
}

function showLogin(err) {
  const n = el(`<main>
    <h1>Sign in</h1>
    <p>Phone OTP or Google (linked phone). No role picker — invite means tenant.</p>
    ${banner(err)}
    <div class="card">
      <div id="recaptcha"></div>
      <label>Phone (+91…)</label>
      <input id="phone" placeholder="+91800…" inputmode="tel">
      <div class="row">
        <button id="otp">Send OTP</button>
      </div>
      <label>OTP</label>
      <input id="code" inputmode="numeric">
      <div class="row"><button id="verify">Verify</button></div>
    </div>
    <div class="card">
      <label>Or paste Firebase ID token</label>
      <textarea id="idtoken" rows="3"></textarea>
      <div class="row"><button class="secondary" id="paste">Use token</button></div>
    </div>
    <button class="ghost" id="back">Back</button>
  </main>`);
  let confirm = null;
  n.querySelector("#back").onclick = () => showLanding();
  n.querySelector("#otp").onclick = async () => {
    try {
      initFirebase();
      window.recaptchaVerifier = window.recaptchaVerifier || new firebase.auth.RecaptchaVerifier("recaptcha", { size: "invisible" });
      confirm = await firebase.auth().signInWithPhoneNumber(n.querySelector("#phone").value.trim(), window.recaptchaVerifier);
    } catch (e) { showLogin(e); }
  };
  n.querySelector("#verify").onclick = async () => {
    try {
      if (!confirm) throw new Error("Send OTP first");
      const cred = await confirm.confirm(n.querySelector("#code").value.trim());
      const idToken = await cred.user.getIdToken();
      await exchange(idToken);
    } catch (e) { showLogin(e); }
  };
  n.querySelector("#paste").onclick = async () => {
    try { await exchange(n.querySelector("#idtoken").value.trim()); }
    catch (e) { showLogin(e); }
  };
  render(n);
}

function initFirebase() {
  if (!S.firebase || !S.firebase.apiKey) throw new Error("Set Firebase keys in Settings");
  if (!firebase.apps.length) firebase.initializeApp(S.firebase);
}

async function exchange(idToken) {
  const body = { id_token: idToken };
  if (S.invite) body.invite_code = S.invite;
  const out = await api("/auth/firebase", { method: "POST", body: JSON.stringify(body) });
  S.token = out.token;
  S.user = out.user;
  saveAuth();
  route();
}

async function showWaiting(err) {
  let me = null;
  try { me = await api("/join/me"); } catch (e) { err = e; }
  const j = me && me.join;
  if (j && j.status === "approved") {
    const n = el(`<main><h1>You're in</h1><p>Sign in again so we can load your dues.</p>
      <div class="row"><button id="again">Continue</button></div></main>`);
    n.querySelector("#again").onclick = logout;
    render(n);
    return;
  }
  const n = el(`<main>
    <h1>Waiting</h1>
    <p>Owner will assign your room and rent. You cannot set rent.</p>
    ${banner(err)}
    <div class="card">
      <label>Your name</label>
      <input id="name" value="${esc(j && j.name)}">
      <label>Aadhaar last-4 (if no QR)</label>
      <input id="last4" maxlength="4" inputmode="numeric" value="${esc(j && j.aadhaar_last4)}">
      <label>Secure QR payload (from card QR scan)</label>
      <textarea id="qr" rows="3" placeholder="Paste scanned QR text"></textarea>
      <label class="muted"><input type="checkbox" id="consent"> I consent to store Aadhaar last-4 only</label>
      <div id="preview"></div>
      <div class="row">
        <button id="save">Save profile</button>
        <button class="secondary" id="decode">Preview QR</button>
      </div>
    </div>
    <div class="row">
      <button class="secondary" id="refresh">Refresh</button>
      <button class="ghost" id="out">Sign out</button>
    </div>
  </main>`);
  n.querySelector("#out").onclick = logout;
  n.querySelector("#refresh").onclick = () => showWaiting();
  n.querySelector("#decode").onclick = async () => {
    try {
      const out = await api("/join", { method: "POST", body: JSON.stringify({
        qr_payload: n.querySelector("#qr").value, consent: n.querySelector("#consent").checked, confirm: false,
      })});
      const a = out.aadhaar || {};
      n.querySelector("#preview").innerHTML = `<p class="ok">Confirm: ${esc(a.name)} · ${esc(a.gender)} · ${esc(a.dob || a.yob)} · ****${esc(a.uid_last4)} ${a.verified ? "(verified)" : ""}</p>`;
      if (a.name) n.querySelector("#name").value = a.name;
      if (a.uid_last4) n.querySelector("#last4").value = a.uid_last4;
    } catch (e) { showWaiting(e); }
  };
  n.querySelector("#save").onclick = async () => {
    try {
      const qr = n.querySelector("#qr").value.trim();
      await api("/join", { method: "POST", body: JSON.stringify({
        name: n.querySelector("#name").value.trim(),
        uid_last4: n.querySelector("#last4").value.trim(),
        qr_payload: qr,
        consent: n.querySelector("#consent").checked || !qr,
        confirm: true,
      })});
      showWaiting();
    } catch (e) { showWaiting(e); }
  };
  render(n);
}

async function showOwner(err) {
  const tab = S.tab || "joins";
  let joins = { join_requests: [] }, reports = { payment_reports: [] }, invite = {}, dues = { dues: [] };
  try {
    joins = await api("/owner/join-requests?status=pending");
    reports = await api("/owner/payment-reports?status=pending_review");
    invite = await api("/owner/invite");
    dues = await api("/owner/dues");
  } catch (e) { err = e; }
  const n = el(`<main>
    <h1>Owner</h1>
    ${banner(err)}
    <nav class="tabs">
      <button data-tab="joins" class="${tab === "joins" ? "on" : ""}">Join requests</button>
      <button data-tab="reports" class="${tab === "reports" ? "on" : ""}">UTR reports</button>
      <button data-tab="dues" class="${tab === "dues" ? "on" : ""}">Dues</button>
      <button data-tab="more" class="${tab === "more" ? "on" : ""}">More</button>
    </nav>
    <section id="joins" class="${tab === "joins" ? "" : "hidden"}"></section>
    <section id="reports" class="${tab === "reports" ? "" : "hidden"}"></section>
    <section id="dues" class="${tab === "dues" ? "" : "hidden"}"></section>
    <section id="more" class="${tab === "more" ? "" : "hidden"}"></section>
    <button class="ghost" id="out">Sign out</button>
  </main>`);
  n.querySelectorAll("[data-tab]").forEach((b) => {
    b.onclick = () => { S.tab = b.dataset.tab; showOwner(); };
  });
  n.querySelector("#out").onclick = logout;

  const jbox = n.querySelector("#joins");
  if (!(joins.join_requests || []).length) jbox.innerHTML = "<p>No pending join requests.</p>";
  (joins.join_requests || []).forEach((j) => {
    const c = el(`<div class="card">
      <strong>${esc(j.name || "(name pending)")}</strong>
      <p class="muted">${esc(j.phone)} ${j.aadhaar_last4 ? "· ****" + esc(j.aadhaar_last4) : ""}</p>
      <label>Room</label><input data-k="room" placeholder="12A">
      <label>Rent (paise)</label><input data-k="rent" inputmode="numeric" placeholder="1500000">
      <label>Due day (1–28)</label><input data-k="due" inputmode="numeric" placeholder="5">
      <label>Deposit (paise)</label><input data-k="dep" inputmode="numeric" placeholder="same as rent">
      <div class="row">
        <button data-act="go">Activate</button>
        <button class="secondary" data-act="no">Reject</button>
      </div>
    </div>`);
    c.querySelector("[data-act=go]").onclick = async () => {
      try {
        await api("/owner/join-requests/" + j.id + "/activate", { method: "POST", body: JSON.stringify({
          room_number: c.querySelector("[data-k=room]").value || null,
          rent_amount: Number(c.querySelector("[data-k=rent]").value),
          due_day: Number(c.querySelector("[data-k=due]").value),
          deposit_amount: Number(c.querySelector("[data-k=dep]").value) || undefined,
        })});
        showOwner();
      } catch (e) { showOwner(e); }
    };
    c.querySelector("[data-act=no]").onclick = async () => {
      try { await api("/owner/join-requests/" + j.id + "/reject", { method: "POST", body: "{}" }); showOwner(); }
      catch (e) { showOwner(e); }
    };
    jbox.appendChild(c);
  });

  const rbox = n.querySelector("#reports");
  if (!(reports.payment_reports || []).length) rbox.innerHTML = "<p>No UTR reports waiting.</p>";
  (reports.payment_reports || []).forEach((r) => {
    const c = el(`<div class="card">
      <p>UTR <strong>${esc(r.upi_txn_id)}</strong> · ${rupees(r.amount)}</p>
      <p class="muted">due ${esc(r.due_id)}</p>
      <div class="row">
        <button data-act="ok">Confirm</button>
        <button class="secondary" data-act="no">Reject</button>
      </div>
    </div>`);
    c.querySelector("[data-act=ok]").onclick = async () => {
      try { await api("/owner/payment-reports/" + r.id + "/confirm", { method: "POST", body: "{}" }); showOwner(); }
      catch (e) { showOwner(e); }
    };
    c.querySelector("[data-act=no]").onclick = async () => {
      try { await api("/owner/payment-reports/" + r.id + "/reject", { method: "POST", body: "{}" }); showOwner(); }
      catch (e) { showOwner(e); }
    };
    rbox.appendChild(c);
  });

  const dbox = n.querySelector("#dues");
  (dues.dues || []).filter((d) => d.status === "pending" || d.status === "partial").forEach((d) => {
    const c = el(`<div class="card">
      <p>${esc(d.kind)} ${esc(d.due_code)} · ${rupees(d.amount)} · ${esc(d.status)}</p>
      <div class="row">
        <button class="secondary" data-act="cash">Mark cash paid</button>
      </div>
    </div>`);
    c.querySelector("[data-act=cash]").onclick = async () => {
      try {
        await api("/owner/dues/" + d.id + "/mark-cash-paid", { method: "POST", body: JSON.stringify({ amount: d.amount }) });
        showOwner();
      } catch (e) { showOwner(e); }
    };
    dbox.appendChild(c);
  });

  n.querySelector("#more").innerHTML = `
    <div class="card">
      <p>Invite code: <strong>${esc(invite.invite_code)}</strong></p>
      <div class="row"><button id="rotate" class="secondary">Rotate invite</button></div>
      <p class="muted">Share this code/QR. Tenants never pick Owner vs Tenant.</p>
    </div>
    <details class="card">
      <summary>Add tenant (walk-in / phone-less / cash only)</summary>
      <label>Name</label><input id="wn">
      <label>Phone (optional)</label><input id="wp">
      <label>Room</label><input id="wr">
      <label>Rent (paise)</label><input id="wrent" inputmode="numeric">
      <label>Due day</label><input id="wdue" inputmode="numeric">
      <label>Deposit (paise)</label><input id="wdep" inputmode="numeric">
      <div class="row"><button id="wadd">Create walk-in tenant</button></div>
    </details>
    <div class="card">
      <label>Bank statement CSV</label>
      <input id="csv" type="file" accept=".csv,text/csv">
      <div class="row"><button class="secondary" id="imp">Import CSV</button></div>
    </div>`;
  n.querySelector("#rotate").onclick = async () => {
    try { await api("/owner/invite/rotate", { method: "POST", body: "{}" }); showOwner(); }
    catch (e) { showOwner(e); }
  };
  n.querySelector("#wadd").onclick = async () => {
    try {
      const phone = n.querySelector("#wp").value.trim();
      await api("/owner/tenants", { method: "POST", body: JSON.stringify({
        name: n.querySelector("#wn").value.trim(),
        phone: phone || null,
        room_number: n.querySelector("#wr").value.trim() || null,
        rent_amount: Number(n.querySelector("#wrent").value),
        due_day: Number(n.querySelector("#wdue").value),
        deposit_amount: Number(n.querySelector("#wdep").value) || undefined,
      })});
      showOwner();
    } catch (e) { showOwner(e); }
  };
  n.querySelector("#imp").onclick = async () => {
    const f = n.querySelector("#csv").files[0];
    if (!f) return;
    const fd = new FormData();
    fd.append("file", f);
    try { await api("/owner/statements/import", { method: "POST", body: fd, headers: {} }); showOwner(); }
    catch (e) { showOwner(e); }
  };
  render(n);
}

async function showTenant(err) {
  let me, dues;
  try {
    me = await api("/tenant/me");
    dues = await api("/tenant/dues");
  } catch (e) {
    if (e.status === 403 && String(e.message).includes("waiting")) return showWaiting(e);
    err = e;
    me = {}; dues = { dues: [] };
  }
  const n = el(`<main>
    <h1>${esc(me.name || "Tenant")}</h1>
    <p class="muted">${me.room_number ? "Room " + esc(me.room_number) : ""}</p>
    ${banner(err)}
    <div id="dues"></div>
    <details class="card">
      <summary>Aadhaar (optional)</summary>
      <label>Secure QR payload</label><textarea id="qr" rows="2"></textarea>
      <label>Or last-4</label><input id="last4" maxlength="4">
      <label class="muted"><input type="checkbox" id="consent"> Consent to store last-4 only</label>
      <div id="aprev"></div>
      <div class="row"><button class="secondary" id="adecode">Preview</button><button id="asave">Save</button></div>
    </details>
    <button class="ghost" id="out">Sign out</button>
  </main>`);
  n.querySelector("#out").onclick = logout;
  n.querySelector("#adecode").onclick = async () => {
    try {
      const out = await api("/tenant/aadhaar", { method: "POST", body: JSON.stringify({
        consent: n.querySelector("#consent").checked, qr_payload: n.querySelector("#qr").value, confirm: false,
      })});
      n.querySelector("#aprev").innerHTML = `<p class="ok">${esc(out.name)} · ****${esc(out.uid_last4)} ${out.verified ? "(verified)" : ""}</p>`;
    } catch (e) { showTenant(e); }
  };
  n.querySelector("#asave").onclick = async () => {
    try {
      await api("/tenant/aadhaar", { method: "POST", body: JSON.stringify({
        consent: n.querySelector("#consent").checked,
        qr_payload: n.querySelector("#qr").value,
        uid_last4: n.querySelector("#last4").value,
        confirm: true,
      })});
      showTenant();
    } catch (e) { showTenant(e); }
  };

  const box = n.querySelector("#dues");
  for (const d of dues.dues || []) {
    const c = el(`<div class="card" data-due="${d.id}">
      <p>${esc(d.kind)} · ${esc(d.status)}</p>
      <p class="amt">${rupees(d.amount)}</p>
      <div class="pay"></div>
    </div>`);
    box.appendChild(c);
    if (d.status === "paid" || d.status === "waived") {
      c.querySelector(".pay").innerHTML = `<p class="ok">Already ${esc(d.status)}. Do not reuse an old QR.</p>`;
      continue;
    }
    try {
      const pay = await api("/tenant/dues/" + d.id + "/pay");
      const payEl = c.querySelector(".pay");
      if (!pay.payable) {
        payEl.innerHTML = `<p class="ok">Not payable.</p>`;
        continue;
      }
      if (pay.mode === "cashfree") {
        payEl.innerHTML = `<p class="muted">UPI checkout — confirmed automatically. Personal VPA QR is not shown.</p>
          <div class="row"><button data-cf>Pay with UPI</button></div>`;
        payEl.querySelector("[data-cf]").onclick = () => startCashfree(pay.payment_session_id);
        continue;
      }
      let qrURL = "";
      try { qrURL = await authedBlob(pay.qr_png_url); } catch (_) {}
      payEl.innerHTML = `
        ${qrURL ? `<img class="qr" alt="UPI QR" src="${qrURL}">` : ""}
        <p class="muted">UPI ID ${esc(pay.vpa)} · Note ${esc(pay.note)}</p>
        <div class="row">
          <a class="btn" href="${esc(pay.upi_link)}">Open UPI</a>
          <a class="btn secondary" download="rent-${esc(pay.due_code)}.png" href="${qrURL}">Save QR</a>
          <button class="secondary" data-copy="${esc(pay.vpa)}">Copy UPI ID</button>
          <button class="secondary" data-copy="${esc(pay.note)}">Copy note</button>
        </div>
        <label>UTR / UPI txn ID</label>
        <input data-utr placeholder="Enter UTR after paying">
        <label>Screenshot (optional)</label>
        <input data-img type="file" accept="image/*">
        <div class="row"><button data-report>Submit UTR</button></div>`;
      payEl.querySelectorAll("[data-copy]").forEach((b) => {
        b.onclick = () => navigator.clipboard && navigator.clipboard.writeText(b.dataset.copy || "");
      });
      payEl.querySelector("[data-report]").onclick = async () => {
        const utr = payEl.querySelector("[data-utr]").value.trim();
        const file = payEl.querySelector("[data-img]").files[0];
        try {
          if (file) {
            const fd = new FormData();
            fd.append("upi_txn_id", utr);
            fd.append("image", file);
            await api("/tenant/dues/" + d.id + "/reports", { method: "POST", body: fd, headers: {} });
          } else {
            await api("/tenant/dues/" + d.id + "/reports", { method: "POST", body: JSON.stringify({ upi_txn_id: utr }) });
          }
          showTenant();
        } catch (e) { showTenant(e); }
      };
    } catch (e) {
      c.querySelector(".pay").innerHTML = `<p class="err">${esc(e.message)}</p>`;
    }
  }
  render(n);
}

async function startCashfree(sessionId) {
  if (!sessionId) return;
  if (!window.Cashfree) {
    await new Promise((res, rej) => {
      const s = document.createElement("script");
      s.src = "https://sdk.cashfree.com/js/v3/cashfree.js";
      s.onload = res;
      s.onerror = rej;
      document.head.appendChild(s);
    });
  }
  const mode = (S.firebase && S.cfEnv) || localStorage.getItem("pg_cf_env") || "sandbox";
  const cashfree = Cashfree({ mode: mode === "production" ? "production" : "sandbox" });
  cashfree.checkout({ paymentSessionId: sessionId, redirectTarget: "_self" });
}

async function authedBlob(path) {
  const res = await fetch(S.api + path, { headers: { Authorization: "Bearer " + S.token } });
  if (!res.ok) throw new Error("could not load QR");
  return URL.createObjectURL(await res.blob());
}

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/app/sw.js").catch(() => {});
}
route();
