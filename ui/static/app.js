// bsc dashboard.
//
// Two sources, deliberately: /ui/state is the operator read model (wei, flows,
// solvency — everything §27 keeps away from apps), and /v1/apps/... is the real
// app contract. The sandbox halves of this page drive that contract exactly as
// an integrating service would, so what you see work here is what an app gets.
// Nothing on this page has a private door into the service.
//
// The third source is /metrics, for the two gauges that are not in the store:
// the master's native and token balances. A dry master stops every pipeline, so
// it belongs on the strip even though it costs a little text parsing.

const $ = (id) => document.getElementById(id);
const POLL_MS = 2000;

let selected = null;      // slug of the open detail panel
let detailTab = 'deposits';

// ---------- formatting ----------

// Cents arrive as decimal strings and must stay that way: they are int64 and a
// JSON number would have been a float. Parsing one into a Number to divide by
// 100 would reintroduce exactly the bug the wire format exists to prevent, so
// the split is done on the string.
function money(cents) {
  const s = String(cents ?? '0');
  const neg = s.startsWith('-');
  const digits = (neg ? s.slice(1) : s).padStart(3, '0');
  const whole = digits.slice(0, -2).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  const frac = digits.slice(-2);
  return `${neg ? '-' : ''}${whole}<span class="cents">.${frac}</span>`;
}

// Wei is only ever shown, never computed on, so BigInt-to-fixed is enough.
//
// The decimals fall back to 18 when they are zero as well as when they are
// absent: the service reads them from the token itself and a database that has
// not met its chain yet reports 0, which would render every balance as a raw
// wei integer rather than an amount.
function token(wei, decimals) {
  if (wei === undefined || wei === null) return '—';
  let v;
  try { v = BigInt(wei); } catch { return String(wei); }
  const neg = v < 0n;
  if (neg) v = -v;
  const places = Number(decimals) || 18;
  const unit = 10n ** BigInt(places);
  const whole = (v / unit).toString();
  const frac = (v % unit).toString().padStart(places, '0').slice(0, 4);
  return `${neg ? '-' : ''}${whole}.${frac}`;
}

const short = (s, head = 6, tail = 4) =>
  !s ? '' : s.length <= head + tail + 1 ? s : `${s.slice(0, head)}…${s.slice(-tail)}`;

const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

const pill = (s) => `<span class="pill ${esc(s)}">${esc(s)}</span>`;

const ago = (iso) => {
  if (!iso) return '';
  const secs = Math.round((Date.now() - Date.parse(iso)) / 1000);
  if (!Number.isFinite(secs)) return '';
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
};

// The explorer is the one place the page sends you off-box, and it is also the
// only reason a tx hash is on the app contract at all. An unknown chain gets no
// link rather than a wrong one — a bscscan URL for a chain that is not BSC would
// silently show somebody else's transaction.
function explorer(chainID) {
  return chainID === 56 ? 'https://bscscan.com'
    : chainID === 97 ? 'https://testnet.bscscan.com'
    : null;
}

function txLink(hash, chainID) {
  if (!hash) return '<span class="zero">—</span>';
  const base = explorer(chainID);
  const label = esc(short(hash, 10, 6));
  return base
    ? `<a class="tx" href="${base}/tx/${esc(hash)}" target="_blank" rel="noreferrer noopener">${label}</a>`
    : label;
}

// Addresses are rendered in full and never shortened. The two things anyone
// wants from an address on this page are to copy it and to open it in an
// explorer, and a truncated one can do neither — so both are one click, side by
// side, rather than a shortened string that is only good for recognising.
function addrCell(addr, chainID, { short: abbreviate = false } = {}) {
  if (!addr) return '<span class="zero">—</span>';
  const base = explorer(chainID);
  const label = esc(abbreviate ? short(addr, 10, 8) : addr);
  const link = base
    ? `<a class="addr" href="${base}/address/${esc(addr)}" target="_blank" rel="noreferrer noopener" title="open in explorer">${label}</a>`
    : `<span class="addr plain">${label}</span>`;
  return `${link}<button class="copy" data-copy="${esc(addr)}" title="copy">copy</button>`;
}

// One delegated listener rather than one per rendered row: the tables are
// rebuilt every couple of seconds, and per-row handlers would leak with them.
document.addEventListener('click', async (e) => {
  const btn = e.target.closest('[data-copy]');
  if (!btn) return;
  e.stopPropagation(); // never let a copy also select the app row
  try {
    await navigator.clipboard.writeText(btn.dataset.copy);
    btn.textContent = 'copied';
    btn.classList.add('done');
    setTimeout(() => { btn.textContent = 'copy'; btn.classList.remove('done'); }, 1200);
  } catch {
    // Clipboard access can be refused; selecting the text still works.
    btn.textContent = 'ctrl-c';
    setTimeout(() => { btn.textContent = 'copy'; }, 1200);
  }
});

// ---------- fetching ----------

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* non-JSON error body */ }
  if (!res.ok) {
    // The API's own error shape, which is what an integrator sees too.
    throw new Error(data?.error || text || `${res.status}`);
  }
  return data;
}

function say(el, msg, kind = '') {
  el.className = `status ${kind}`;
  el.textContent = msg;
}

// The master's balances live in the metrics registry rather than the store, so
// they come from the scrape endpoint. One regex per gauge beats plumbing a
// metrics reader through the API for two numbers.
async function masterGauges() {
  try {
    const text = await (await fetch('/metrics')).text();
    const read = (name) => {
      const m = text.match(new RegExp(`^${name}\\s+([0-9.e+-]+)$`, 'm'));
      return m ? Number(m[1]) : null;
    };
    return {
      bnb: read('bsc_master_bnb_wei'),
      usdt: read('bsc_master_usdt_wei'),
      inFlight: read('bsc_transactions_in_flight'),
    };
  } catch {
    return {};
  }
}

// ---------- the strip ----------

function renderStrip(state, gauges) {
  const svc = state.service;
  $('s-chain').textContent = svc.chain_id ? `${svc.chain_id}` : '—';
  $('s-block').textContent = svc.block ? svc.block.toLocaleString() : '—';

  const behind = svc.blocks_behind ?? 0;
  $('s-behind').textContent = behind.toLocaleString();
  const behindStat = $('s-behind-stat');
  behindStat.className = 'stat' + (svc.lagging ? ' alert' : behind > 20 ? ' warn' : '');
  behindStat.title = svc.lagging
    ? `more than MAX_LAG_BLOCKS (${svc.max_lag_blocks}) behind — withdrawals are being refused`
    : `withdrawals are refused past ${svc.max_lag_blocks}`;

  const bnb = gauges.bnb === null || gauges.bnb === undefined ? '—' : (gauges.bnb / 1e18).toFixed(4);
  $('s-bnb').textContent = bnb;
  // The master pays every transfer's gas, so a low one is the condition that
  // quietly stops the whole service.
  $('s-bnb').parentElement.className =
    'stat' + (gauges.bnb === null || gauges.bnb === undefined ? '' : gauges.bnb < 5e16 ? ' alert' : gauges.bnb < 1e17 ? ' warn' : '');

  $('s-usdt').textContent = gauges.usdt === null || gauges.usdt === undefined
    ? '—' : token(BigInt(Math.round(gauges.usdt)).toString(), svc.decimals);
  $('s-inflight').textContent = gauges.inFlight === null || gauges.inFlight === undefined
    ? '—' : String(gauges.inFlight);

  $('s-master').innerHTML = addrCell(svc.master, svc.chain_id);
  // An unset collector means the master collects, which is the default and
  // worth saying rather than showing the same address twice.
  $('s-collector').innerHTML = svc.collector
    ? addrCell(svc.collector, svc.chain_id)
    : '<span class="zero">the master</span>';
}

// ---------- apps ----------

function renderApps(state) {
  const body = $('apps-body');
  if (!state.apps.length) {
    body.innerHTML = '<tr><td colspan="10" class="zero">No apps yet — register one above.</td></tr>';
    return;
  }
  body.innerHTML = state.apps.map((a) => {
    const b = a.balance;
    // A negative excess is the one finding that nothing self-corrects: the
    // wallet holds less than the app is owed.
    const excess = a.solvent
      ? `<span class="zero">${esc(token(a.excess_wei, state.service.decimals))}</span>`
      : `<span style="color:var(--bad)">${esc(token(a.excess_wei, state.service.decimals))} short</span>`;
    return `<tr class="clickable${selected === a.slug ? ' selected' : ''}" data-slug="${esc(a.slug)}">
      <td>${esc(a.slug)} ${a.paused ? pill('paused') : ''}</td>
      <td>${addrCell(a.address, state.service.chain_id)}</td>
      <td class="num money">${money(b.available_cents)}</td>
      <td class="num money">${money(b.reserved_cents)}</td>
      <td class="num money">${money(b.pending_cents)}</td>
      <td class="num money">${money(b.total_cents)}</td>
      <td class="num">${excess}</td>
      <td class="num">${a.addresses}</td>
      <td>${money(a.fee.flat_cents)}</td>
      <td><button class="ghost tiny" data-pause="${esc(a.slug)}">${a.paused ? 'resume' : 'pause'}</button></td>
    </tr>`;
  }).join('');
}

function renderFlows(state) {
  const body = $('flows-body');
  $('flows-empty').hidden = state.flows.length > 0;
  body.innerHTML = state.flows.map((f) => `<tr>
    <td>${esc(f.kind)}</td>
    <td>${pill(f.state)}</td>
    <td>${esc(f.app)}</td>
    <td>${f.address ? addrCell(f.address, state.service.chain_id, { short: true }) : esc(short(f.wallet, 8, 6))}</td>
    <td class="num">${f.attempt}</td>
    <td>${f.retry_after ? esc(ago(f.retry_after).replace(' ago', '')) : '<span class="zero">—</span>'}</td>
    <td>${txLink(f.tx_hash, state.service.chain_id)}</td>
    <td class="err">${esc(f.error || '')}</td>
  </tr>`).join('');
}

// ---------- the detail panel ----------

async function renderDetail(chainID) {
  if (!selected) { $('detail').hidden = true; return; }
  $('detail').hidden = false;
  $('d-slug').textContent = selected;

  const [addrs, deposits, withdrawals] = await Promise.all([
    api('GET', `/v1/apps/${encodeURIComponent(selected)}/addresses?limit=100`),
    api('GET', `/v1/apps/${encodeURIComponent(selected)}/deposits?limit=50`),
    api('GET', `/v1/apps/${encodeURIComponent(selected)}/withdrawals?status=all&limit=50`),
  ]);

  $('addrs-body').innerHTML = (addrs.addresses || []).map((a) =>
    `<tr><td>${esc(a.ref)}</td><td>${addrCell(a.address, chainID)}</td></tr>`).join('')
    || '<tr><td colspan="2" class="zero">none yet</td></tr>';

  $('deposits-body').innerHTML = (deposits.deposits || []).map((d) => `<tr>
    <td>${esc(d.ref)}</td>
    <td class="num money">${money(d.amount_cents)}</td>
    <td>${pill(d.status)}</td>
    <td>${addrCell(d.from, chainID, { short: true })}</td>
    <td>${txLink(d.tx_hash, chainID)}</td>
    <td>${esc(ago(d.created_at))}</td>
  </tr>`).join('') || '<tr><td colspan="6" class="zero">nothing has arrived yet</td></tr>';

  $('withdrawals-body').innerHTML = (withdrawals.withdrawals || []).map((w) => `<tr>
    <td>${addrCell(w.destination, chainID, { short: true })}</td>
    <td class="num money">${money(w.payout_cents)}</td>
    <td class="num money">${money(w.fee_cents)}</td>
    <td>${pill(w.status)}</td>
    <td class="num">${w.attempts > 3 ? `<span style="color:var(--warn)">${w.attempts}</span>` : w.attempts}</td>
    <td>${txLink(w.tx_hash, chainID)}</td>
    <td class="err">${esc(w.last_error || '')}</td>
  </tr>`).join('') || '<tr><td colspan="7" class="zero">none yet</td></tr>';
}

// ---------- the poll loop ----------

let inFlightPoll = false;

async function poll() {
  if (inFlightPoll) return;
  inFlightPoll = true;
  try {
    const [state, gauges] = await Promise.all([api('GET', '/ui/state'), masterGauges()]);
    renderStrip(state, gauges);
    renderApps(state);
    renderFlows(state);
    await renderDetail(state.service.chain_id);
    $('tick').textContent = `v${state.service.version} · refreshed ${new Date().toLocaleTimeString()}`;
    $('tick').className = 'hint';
  } catch (err) {
    $('tick').textContent = `unreachable: ${err.message}`;
    $('tick').className = 'hint status bad';
  } finally {
    inFlightPoll = false;
  }
}

// ---------- actions ----------

$('register-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const slug = $('register-slug').value.trim();
  if (!slug) return;
  try {
    await api('PUT', `/v1/apps/${encodeURIComponent(slug)}`, {});
    $('register-slug').value = '';
    selected = slug;
    await poll();
  } catch (err) {
    alert(`register: ${err.message}`);
  }
});

$('apps-body').addEventListener('click', async (e) => {
  const pause = e.target.closest('[data-pause]');
  if (pause) {
    e.stopPropagation();
    const slug = pause.dataset.pause;
    const wasPaused = pause.textContent.trim() === 'resume';
    try {
      await api('PUT', `/v1/apps/${encodeURIComponent(slug)}`, { paused: !wasPaused });
      await poll();
    } catch (err) {
      alert(`pause: ${err.message}`);
    }
    return;
  }
  const row = e.target.closest('[data-slug]');
  if (row) {
    selected = selected === row.dataset.slug ? null : row.dataset.slug;
    poll();
  }
});

$('d-close').addEventListener('click', () => { selected = null; $('detail').hidden = true; });

$('addr-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const ref = $('addr-ref').value.trim();
  if (!ref || !selected) return;
  try {
    const made = await api('POST', `/v1/apps/${encodeURIComponent(selected)}/addresses`, { ref });
    $('addr-ref').value = '';
    say($('addr-status'), `${made.ref} → ${made.address}`, 'ok');
    await poll();
  } catch (err) {
    say($('addr-status'), err.message, 'bad');
  }
});

$('wd-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!selected) return;
  const amount = String($('wd-amount').value);
  const deductFee = $('wd-deduct').checked;
  // The two endpoints take different bodies and the API rejects unknown fields
  // on purpose, so a misspelling in an integrator's payload is an error rather
  // than a silently ignored setting. Pricing needs no destination; only the
  // payout does. Sending one body to both is exactly the mistake that strictness
  // is there to catch, and it caught this one.
  const quoteBody = { amount_cents: amount, deduct_fee: deductFee };
  const createBody = { ...quoteBody, destination: $('wd-dest').value.trim() };
  try {
    // Quote first, so the fee is shown against the same policy the create will
    // use. A refusal here is the whole error path for a withdrawal (§28).
    const q = await api('POST', `/v1/apps/${encodeURIComponent(selected)}/withdrawals/quote`, quoteBody);
    const wd = await api('POST', `/v1/apps/${encodeURIComponent(selected)}/withdrawals`, createBody);
    say($('wd-status'),
      `${wd.status} — payout ${q.payout_cents}c, fee ${q.fee_cents}c, debits ${q.debit_cents}c`, 'ok');
    await poll();
  } catch (err) {
    say($('wd-status'), err.message, 'bad');
  }
});

document.querySelectorAll('.tab').forEach((tab) => {
  tab.addEventListener('click', () => {
    detailTab = tab.dataset.tab;
    document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t === tab));
    $('tab-deposits').hidden = detailTab !== 'deposits';
    $('tab-withdrawals').hidden = detailTab !== 'withdrawals';
  });
});

poll();
setInterval(poll, POLL_MS);
