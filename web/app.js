const S = {
  api: localStorage.getItem("pg_api") || location.origin.replace(/\/$/, "") + "/api",
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
  if (S.user.role === "manager") return showManager();
  if (S.user.role === "tenant" && !S.user.tenant_id) return showWaiting();
  return showTenant();
}

function showLanding(err) {
  const n = el(`<main>
    <h1>PG / Hostel</h1>
    <p>Scan or enter the invite from your owner. Owners and registered tenants sign in below.</p>
    ${banner(err)}
    <div class="card">
      <label>New tenant? Enter invite code</label>
      <input id="invite" value="${esc(S.invite)}" placeholder="e.g. DEVINV01" autocomplete="off">
      <div class="row">
        <button id="continue">Continue with invite</button>
      </div>
      <p class="muted" id="prop"></p>
    </div>
    <div class="card">
      <label>Already registered or owner?</label>
      <div class="row">
        <button class="secondary" id="tenant-login">Tenant sign in</button>
        <button class="secondary" id="owner">Owner sign in</button>
      </div>
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
      showLogin(null, "invite");
    } catch (e) {
      showLanding(e);
    }
  };
  n.querySelector("#tenant-login").onclick = () => {
    S.invite = "";
    saveAuth();
    showLogin(null, "tenant");
  };
  n.querySelector("#owner").onclick = () => {
    S.invite = "";
    saveAuth();
    showLogin(null, "owner");
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

function showLogin(err, mode = (S.invite ? "invite" : "owner")) {
  const isOwner = (mode === "owner");
  let title = "Sign in";
  let sub = "Sign in to continue.";
  if (isOwner) {
    title = "Owner sign in";
    sub = "Owners sign in with Google or Phone OTP.";
  } else if (mode === "tenant") {
    title = "Tenant sign in";
    sub = "Tenants sign in with Phone OTP to view rent dues and receipts.";
  } else if (mode === "invite") {
    title = "Join property";
    sub = "Sign in with Phone OTP to complete your tenant registration.";
  }

  const n = el(`<main>
    <h1>${title}</h1>
    <p>${sub}</p>
    ${banner(err)}
    ${isOwner ? `
    <div class="card">
      <button class="secondary" id="google" style="display:flex;align-items:center;justify-content:center;gap:10px;width:100%;font-weight:600;padding:10px;">
        <svg width="18" height="18" viewBox="0 0 24 24"><path fill="#4285F4" d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92c-.26 1.37-1.04 2.53-2.21 3.31v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.09z"/><path fill="#34A853" d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"/><path fill="#FBBC05" d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.06H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.94l2.85-2.22.81-.63z"/><path fill="#EA4335" d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.06l3.66 2.84c.87-2.6 3.3-4.52 6.16-4.52z"/></svg>
        Sign in with Google
      </button>
    </div>` : ""}
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
  const googleBtn = n.querySelector("#google");
  if (googleBtn) {
    googleBtn.onclick = async () => {
      try {
        initFirebase();
        const provider = new firebase.auth.GoogleAuthProvider();
        const cred = await firebase.auth().signInWithPopup(provider);
        const idToken = await cred.user.getIdToken();
        await exchange(idToken);
      } catch (e) { showLogin(e, mode); }
    };
  }
  n.querySelector("#otp").onclick = async () => {
    try {
      initFirebase();
      window.recaptchaVerifier = window.recaptchaVerifier || new firebase.auth.RecaptchaVerifier("recaptcha", { size: "invisible" });
      confirm = await firebase.auth().signInWithPhoneNumber(n.querySelector("#phone").value.trim(), window.recaptchaVerifier);
    } catch (e) { showLogin(e, mode); }
  };
  n.querySelector("#verify").onclick = async () => {
    try {
      if (!confirm) throw new Error("Send OTP first");
      const cred = await confirm.confirm(n.querySelector("#code").value.trim());
      const idToken = await cred.user.getIdToken();
      await exchange(idToken);
    } catch (e) { showLogin(e, mode); }
  };
  n.querySelector("#paste").onclick = async () => {
    try { await exchange(n.querySelector("#idtoken").value.trim()); }
    catch (e) { showLogin(e, mode); }
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
      <button data-tab="gamification" class="${tab === "gamification" ? "on" : ""}">Gamification</button>
      <button data-tab="more" class="${tab === "more" ? "on" : ""}">More</button>
    </nav>
    <section id="joins" class="${tab === "joins" ? "" : "hidden"}"></section>
    <section id="reports" class="${tab === "reports" ? "" : "hidden"}"></section>
    <section id="dues" class="${tab === "dues" ? "" : "hidden"}"></section>
    <section id="gamification" class="${tab === "gamification" ? "" : "hidden"}"></section>
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

  const gbox = n.querySelector("#gamification");
  if (gbox) {
    gbox.innerHTML = `
      <div class="card">
        <h2>Warden / Manager Delegation</h2>
        <p class="muted">Add a warden with scoped operational permissions (inspections, meter readings, meal attendance).</p>
        <label>Warden Phone (+91...)</label><input id="mgr_phone" placeholder="+919000012345">
        <div class="row"><button id="add_mgr">Create Warden Account</button></div>
        <div id="mgr_status"></div>
      </div>
      <div class="card">
        <h2>Floors & Rooms</h2>
        <label>Floor Number</label><input id="fl_num" type="number" placeholder="1">
        <label>Floor Name</label><input id="fl_name" placeholder="First Floor">
        <div class="row"><button id="add_fl" class="secondary">Add Floor</button></div>
      </div>`;
    const addMgr = gbox.querySelector("#add_mgr");
    if (addMgr) {
      addMgr.onclick = async () => {
        const p = gbox.querySelector("#mgr_phone").value.trim();
        try {
          await api("/owner/managers", { method: "POST", body: JSON.stringify({ phone: p }) });
          gbox.querySelector("#mgr_status").innerHTML = `<p class="ok">Warden account created for ${esc(p)}. Warden can now sign in.</p>`;
        } catch (e) {
          gbox.querySelector("#mgr_status").innerHTML = `<p class="err">${esc(e.message)}</p>`;
        }
      };
    }
    const addFl = gbox.querySelector("#add_fl");
    if (addFl) {
      addFl.onclick = async () => {
        try {
          const fn = Number(gbox.querySelector("#fl_num").value);
          const name = gbox.querySelector("#fl_name").value.trim();
          await api("/owner/floors", { method: "POST", body: JSON.stringify({ floor_number: fn, name }) });
          showOwner();
        } catch (e) { showOwner(e); }
      };
    }
  }

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
  let me, dues, pts = {}, rsvp = {}, rewards = { rewards: [] }, insp = { inspections: [] }, viols = { violations: [] };
  try {
    me = await api("/tenant/me");
    dues = await api("/tenant/dues");
    try { pts = await api("/tenant/points"); } catch (_) {}
    try { rsvp = await api("/tenant/meal-rsvp"); } catch (_) {}
    try { rewards = await api("/tenant/rewards"); } catch (_) {}
    try { insp = await api("/tenant/inspections"); } catch (_) {}
    try { viols = await api("/tenant/violations"); } catch (_) {}
  } catch (e) {
    if (e.status === 403 && String(e.message).includes("waiting")) return showWaiting(e);
    err = e;
    me = {}; dues = { dues: [] };
  }
  const n = el(`<main>
    <h1>${esc(me.name || "Tenant")}</h1>
    <p class="muted">${me.room_number ? "Room " + esc(me.room_number) : ""}</p>
    ${banner(err)}

    <!-- Points & Streak Banner -->
    <div class="points-banner">
      <div>
        <span class="pts">${pts.balance || 0}</span> <small>Points</small>
        ${pts.expiring_soon ? `<p style="margin:0.2rem 0 0;font-size:0.8rem;color:#fde047;">${pts.expiring_soon} points expire soon</p>` : ""}
      </div>
      <div class="streak-pill">🔥 ${pts.on_time_months || 0}-Month Streak</div>
    </div>

    ${(viols.violations || []).length ? `
      <div class="card" style="border-left: 4px solid #ef4444; background: #fef2f2;">
        <strong style="color:#b91c1c;">Notice from Warden</strong>
        ${viols.violations.map(v => `<p style="margin:0.3rem 0;color:#7f1d1d;">Step ${v.step} (${esc(v.severity)}): ${esc(v.description)}</p>`).join("")}
      </div>
    ` : ""}

    <!-- Meal RSVP Card -->
    <div class="card">
      <h2>Tomorrow's Meals · ${esc(rsvp.date || "")}</h2>
      <p class="muted">Confirm by 8:00 PM for +2 points (helps kitchen prep exact portions & prevent waste).</p>
      <div class="checklist-item">
        <span>Breakfast</span>
        <label class="toggle-switch"><input type="checkbox" id="rsvp_b" ${(rsvp.breakfast && rsvp.breakfast.attending) ? "checked" : ""}><span class="slider"></span></label>
      </div>
      <div class="checklist-item">
        <span>Lunch</span>
        <label class="toggle-switch"><input type="checkbox" id="rsvp_l" ${(rsvp.lunch && rsvp.lunch.attending) ? "checked" : ""}><span class="slider"></span></label>
      </div>
      <div class="checklist-item">
        <span>Dinner</span>
        <label class="toggle-switch"><input type="checkbox" id="rsvp_d" ${(rsvp.dinner && rsvp.dinner.attending) ? "checked" : ""}><span class="slider"></span></label>
      </div>
      <div class="row"><button id="save_rsvp">Save RSVP</button></div>
      <div id="rsvp_status"></div>
    </div>

    <div id="dues"></div>

    <!-- Rewards Shop -->
    <details class="card">
      <summary>Rewards Shop (Rent Credit & Perks)</summary>
      <p class="muted">1 Point = ₹1. Cash rent credits require 3 consecutive on-time rent months.</p>
      <div id="rewards_list"></div>
    </details>

    <!-- Cleanliness Inspections & Disputes -->
    <details class="card">
      <summary>Cleanliness & Room Inspections</summary>
      <p class="muted">Recent room & floor inspections. Dispute failed items within 48h.</p>
      <div id="inspections_list"></div>
    </details>

    <!-- Report Hazard -->
    <details class="card">
      <summary>Report Hazard / Leak (Earn 25 pts)</summary>
      <p class="muted">Privately report water leaks, RO purifier breakdown, exposed wires, or gas smell.</p>
      <label>Category</label>
      <select id="hz_cat">
        <option value="water_leak">Water Leak / Broken Pipe</option>
        <option value="ro_purifier">Drinking Water Purifier Issue</option>
        <option value="electrical_wire">Electrical Spark / Exposed Wire</option>
        <option value="gas_smell">Gas Smell</option>
        <option value="other">Other</option>
      </select>
      <label>Description</label>
      <textarea id="hz_desc" rows="2" placeholder="e.g. 2nd floor corridor pipe dripping"></textarea>
      <label>Photo (optional)</label>
      <input type="file" id="hz_photo" accept="image/*">
      <div class="row"><button id="save_hz">Submit Report</button></div>
      <div id="hz_status"></div>
    </details>

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

  // Meal RSVP handlers
  n.querySelector("#save_rsvp").onclick = async () => {
    const statusBox = n.querySelector("#rsvp_status");
    try {
      const date = rsvp.date || new Date(Date.now() + 86400000).toISOString().slice(0, 10);
      await api("/tenant/meal-rsvp", { method: "POST", body: JSON.stringify({
        date: date, slot: "breakfast", attending: n.querySelector("#rsvp_b").checked
      })});
      await api("/tenant/meal-rsvp", { method: "POST", body: JSON.stringify({
        date: date, slot: "lunch", attending: n.querySelector("#rsvp_l").checked
      })});
      await api("/tenant/meal-rsvp", { method: "POST", body: JSON.stringify({
        date: date, slot: "dinner", attending: n.querySelector("#rsvp_d").checked
      })});
      statusBox.innerHTML = `<p class="ok">RSVP saved! Points updated.</p>`;
    } catch (e) {
      statusBox.innerHTML = `<p class="err">${esc(e.message)}</p>`;
    }
  };

  // Rewards list rendering & redemption
  const rList = n.querySelector("#rewards_list");
  if (!(rewards.rewards || []).length) {
    rList.innerHTML = `<p class="muted">No perks currently available.</p>`;
  } else {
    (rewards.rewards || []).forEach(r => {
      const row = el(`<div class="reward-card">
        <div>
          <strong>${esc(r.title)}</strong>
          <p class="muted" style="margin:0.2rem 0;">${esc(r.description)}</p>
          <small class="badge ${r.eligible ? 'ok' : 'warn'}">${r.eligible ? 'Eligible' : esc(r.reason)}</small>
        </div>
        <div>
          <button data-redeem="${r.id}" ${(!r.eligible || (pts.balance || 0) < r.points_cost) ? 'disabled' : ''} class="${(!r.eligible || (pts.balance || 0) < r.points_cost) ? 'secondary' : ''}">
            ${r.points_cost} Pts
          </button>
        </div>
      </div>`);
      row.querySelector("[data-redeem]").onclick = async () => {
        try {
          await api("/tenant/rewards/" + r.id + "/redeem", { method: "POST", body: "{}" });
          alert("Perk redeemed successfully!");
          showTenant();
        } catch (e) { alert("Redemption failed: " + e.message); }
      };
      rList.appendChild(row);
    });
  }

  // Inspections rendering & disputes
  const iList = n.querySelector("#inspections_list");
  if (!(insp.inspections || []).length) {
    iList.innerHTML = `<p class="muted">No inspections recorded yet.</p>`;
  } else {
    (insp.inspections || []).forEach(inItem => {
      const c = el(`<div class="card" style="margin:0.5rem 0;">
        <div style="display:flex; justify-content:space-between;">
          <strong>${esc(inItem.inspection_type.toUpperCase())} · ${inItem.score_percent}% Score</strong>
          <span class="badge ${inItem.passed ? 'ok' : 'danger'}">${inItem.passed ? 'PASSED' : 'FAILED'}</span>
        </div>
        <p class="muted">${new Date(inItem.inspected_at).toLocaleDateString()}</p>
        <div class="items"></div>
      </div>`);
      const itemsBox = c.querySelector(".items");
      (inItem.items || []).forEach(it => {
        const itRow = el(`<div style="display:flex; justify-content:space-between; align-items:center; margin:0.3rem 0; font-size:0.9rem;">
          <span>${it.passed ? '✅' : '❌'} ${esc(it.description)}</span>
          ${(!it.passed && it.resolution_status === 'none') ? `<button class="secondary" data-disp="${it.id}" style="padding:0.2rem 0.5rem;font-size:0.75rem;">Dispute (48h)</button>` : ''}
          ${it.resolution_status === 'disputed' ? `<span class="badge warn">Disputed</span>` : ''}
        </div>`);
        const dispBtn = itRow.querySelector("[data-disp]");
        if (dispBtn) {
          dispBtn.onclick = async () => {
            const note = prompt("Enter dispute explanation (e.g. tap was repaired this morning):");
            if (!note) return;
            try {
              await api("/tenant/inspections/items/" + it.id + "/dispute", { method: "POST", body: JSON.stringify({ dispute_note: note }) });
              alert("Dispute submitted for warden review.");
              showTenant();
            } catch (e) { alert(e.message); }
          };
        }
        itemsBox.appendChild(itRow);
      });
      iList.appendChild(c);
    });
  }

  // Hazard reporting
  n.querySelector("#save_hz").onclick = async () => {
    const cat = n.querySelector("#hz_cat").value;
    const desc = n.querySelector("#hz_desc").value.trim();
    const photoFile = n.querySelector("#hz_photo").files[0];
    const statusBox = n.querySelector("#hz_status");
    if (!desc) {
      statusBox.innerHTML = `<p class="err">Please enter description</p>`;
      return;
    }
    try {
      let b64Photo = "";
      if (photoFile) {
        b64Photo = await new Promise((res, rej) => {
          const reader = new FileReader();
          reader.onload = () => res(reader.result.split(",")[1]);
          reader.onerror = rej;
          reader.readAsDataURL(photoFile);
        });
      }
      await api("/tenant/hazards", { method: "POST", body: JSON.stringify({
        category: cat, description: desc, photo_base64: b64Photo
      })});
      statusBox.innerHTML = `<p class="ok">Hazard reported anonymously! 25 points will be awarded when resolved.</p>`;
      n.querySelector("#hz_desc").value = "";
    } catch (e) {
      statusBox.innerHTML = `<p class="err">${esc(e.message)}</p>`;
    }
  };

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

// --- Warden / Manager Dashboard ---
async function showManager(err) {
  const tab = S.tab || "headcount";
  let propID = (S.user && S.user.property_id) || "";
  let headcount = {}, hazards = { hazards: [] }, floors = { floors: [] }, rooms = { rooms: [] };
  try {
    headcount = await api("/manager/kitchen/headcount?property_id=" + propID);
    hazards = await api("/manager/hazards?property_id=" + propID);
    floors = await api("/owner/floors?property_id=" + propID);
    rooms = await api("/owner/rooms?property_id=" + propID);
  } catch (e) { err = e; }

  const n = el(`<main>
    <h1>Warden / Manager</h1>
    ${banner(err)}
    <nav class="tabs">
      <button data-tab="headcount" class="${tab === "headcount" ? "on" : ""}">Headcount</button>
      <button data-tab="inspect" class="${tab === "inspect" ? "on" : ""}">Inspection</button>
      <button data-tab="meter" class="${tab === "meter" ? "on" : ""}">Meters</button>
      <button data-tab="hazards" class="${tab === "hazards" ? "on" : ""}">Hazards</button>
    </nav>
    <section id="headcount" class="${tab === "headcount" ? "" : "hidden"}"></section>
    <section id="inspect" class="${tab === "inspect" ? "" : "hidden"}"></section>
    <section id="meter" class="${tab === "meter" ? "" : "hidden"}"></section>
    <section id="hazards" class="${tab === "hazards" ? "" : "hidden"}"></section>
    <button class="ghost" id="out">Sign out</button>
  </main>`);

  n.querySelectorAll("[data-tab]").forEach(b => {
    b.onclick = () => { S.tab = b.dataset.tab; showManager(); };
  });
  n.querySelector("#out").onclick = logout;

  // 1. Headcount tab
  const hcBox = n.querySelector("#headcount");
  const hc = headcount.headcount || {};
  hcBox.innerHTML = `
    <div class="card">
      <h2>Kitchen Headcount · ${esc(hc.meal_date || "Today")}</h2>
      <p class="muted">Live RSVP attendance from tenants to guide meal preparation.</p>
      <div class="checklist-item"><span>Breakfast</span><strong>${hc.breakfast || 0} meals</strong></div>
      <div class="checklist-item"><span>Lunch</span><strong>${hc.lunch || 0} meals</strong></div>
      <div class="checklist-item"><span>Dinner</span><strong>${hc.dinner || 0} meals</strong></div>
      <div class="checklist-item" style="border-top:1px solid var(--line); font-weight:700;">
        <span>Total RSVP</span><strong>${hc.total || 0}</strong>
      </div>
    </div>`;

  // 2. Inspection tab
  const insBox = n.querySelector("#inspect");
  insBox.innerHTML = `
    <div class="card">
      <h2>Conduct Inspection</h2>
      <label>Inspection Type</label>
      <select id="ins_type">
        <option value="room">Room Inspection</option>
        <option value="floor">Floor / Common Area Inspection</option>
      </select>
      <label>Room</label>
      <select id="ins_room">
        ${(rooms.rooms || []).map(r => `<option value="${r.id}">Room ${esc(r.room_number)}</option>`).join("")}
      </select>
      <label>Checklist Items</label>
      <div id="ins_checklist">
        <div class="checklist-item" data-key="bed_clear"><span>Bed / Desk clear</span><input type="checkbox" checked></div>
        <div class="checklist-item" data-key="no_food_waste"><span>No food waste in room</span><input type="checkbox" checked></div>
        <div class="checklist-item" data-key="dustbin_segregated"><span>Dustbin segregated (wet/dry)</span><input type="checkbox" checked></div>
        <div class="checklist-item" data-key="shoes_on_rack"><span>Footwear on rack</span><input type="checkbox" checked></div>
        <div class="checklist-item" data-key="no_appliances"><span>No prohibited appliances (heaters/induction)</span><input type="checkbox" checked></div>
      </div>
      <div class="photo-fail-box hidden" id="photo_fail_box">
        <label style="color:#b91c1c;font-weight:700;">Photo proof is mandatory for any failed item:</label>
        <input type="file" id="ins_photo" accept="image/*">
      </div>
      <div class="row"><button id="submit_ins">Submit Inspection</button></div>
      <div id="ins_status"></div>
    </div>`;

  const photoBox = insBox.querySelector("#photo_fail_box");
  insBox.querySelectorAll("#ins_checklist input[type=checkbox]").forEach(cb => {
    cb.onchange = () => {
      let anyFailed = false;
      insBox.querySelectorAll("#ins_checklist input[type=checkbox]").forEach(c => { if (!c.checked) anyFailed = true; });
      if (anyFailed) photoBox.classList.remove("hidden");
      else photoBox.classList.add("hidden");
    };
  });

  insBox.querySelector("#submit_ins").onclick = async () => {
    const statusEl = insBox.querySelector("#ins_status");
    try {
      const type = insBox.querySelector("#ins_type").value;
      const roomId = insBox.querySelector("#ins_room").value;
      const photoFile = insBox.querySelector("#ins_photo").files[0];
      let b64 = "";
      if (photoFile) {
        b64 = await new Promise((res, rej) => {
          const r = new FileReader();
          r.onload = () => res(r.result.split(",")[1]);
          r.onerror = rej;
          r.readAsDataURL(photoFile);
        });
      }

      const items = [];
      insBox.querySelectorAll("#ins_checklist .checklist-item").forEach(row => {
        const key = row.dataset.key;
        const desc = row.querySelector("span").textContent;
        const passed = row.querySelector("input").checked;
        items.push({
          item_key: key,
          description: desc,
          passed: passed,
          photo_base64: (!passed ? b64 : ""),
        });
      });

      await api("/manager/inspections", { method: "POST", body: JSON.stringify({
        property_id: propID,
        room_id: (type === "room" ? roomId : null),
        inspection_type: type,
        items: items,
      })});
      statusEl.innerHTML = `<p class="ok">Inspection saved! Score computed & points awarded.</p>`;
    } catch (e) {
      statusEl.innerHTML = `<p class="err">${esc(e.message)}</p>`;
    }
  };

  // 3. Meter Readings tab
  const mBox = n.querySelector("#meter");
  mBox.innerHTML = `
    <div class="card">
      <h2>Record Meter Reading</h2>
      <label>Utility Kind</label>
      <select id="m_kind"><option value="electricity">Room Electricity (kWh)</option><option value="water">Floor Water (Litres)</option></select>
      <label>Room</label>
      <select id="m_room">
        ${(rooms.rooms || []).map(r => `<option value="${r.id}">Room ${esc(r.room_number)}</option>`).join("")}
      </select>
      <label>Current Meter Reading</label>
      <input type="number" step="0.1" id="m_val" placeholder="e.g. 1520.5">
      <div class="row"><button id="submit_meter">Record Reading</button></div>
      <div id="m_status"></div>
    </div>`;

  mBox.querySelector("#submit_meter").onclick = async () => {
    const statusEl = mBox.querySelector("#m_status");
    try {
      const kind = mBox.querySelector("#m_kind").value;
      const roomId = mBox.querySelector("#m_room").value;
      const val = Number(mBox.querySelector("#m_val").value);
      const res = await api("/manager/meter-readings", { method: "POST", body: JSON.stringify({
        property_id: propID,
        room_id: roomId,
        kind: kind,
        reading_value: val,
      })});
      const r = res.result || {};
      statusEl.innerHTML = `<p class="ok">Saved! Delta: ${r.delta_units} units. Excess over quota: ${r.excess_units} units (${rupees(r.billable_paise)}).</p>`;
    } catch (e) {
      statusEl.innerHTML = `<p class="err">${esc(e.message)}</p>`;
    }
  };

  // 4. Hazards tab
  const hzBox = n.querySelector("#hazards");
  if (!(hazards.hazards || []).length) {
    hzBox.innerHTML = `<div class="card"><p class="muted">No open hazard reports.</p></div>`;
  } else {
    (hazards.hazards || []).forEach(h => {
      const c = el(`<div class="card">
        <strong>${esc(h.category.toUpperCase())} · Status: ${esc(h.status)}</strong>
        <p>${esc(h.description)}</p>
        <p class="muted">${new Date(h.created_at).toLocaleString()}</p>
        ${h.status === 'open' ? `<div class="row"><button data-res="${h.id}">Mark Resolved & Award 25 Pts</button></div>` : ''}
      </div>`);
      const btn = c.querySelector("[data-res]");
      if (btn) {
        btn.onclick = async () => {
          try {
            await api("/manager/hazards/" + h.id + "/resolve", { method: "POST", body: JSON.stringify({ status: "resolved" }) });
            alert("Hazard marked resolved. Points credited to reporter.");
            showManager();
          } catch (e) { alert(e.message); }
        };
      }
      hzBox.appendChild(c);
    });
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
