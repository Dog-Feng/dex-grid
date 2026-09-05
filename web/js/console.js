const FEE = 0.0004;
const DIR_LABEL = { long: "做多", short: "做空", neutral: "中性" };
const STATUS_LABEL = {
  running: "运行中",
  starting: "启动中",
  paused: "已暂停",
  reconnecting: "重连中",
  error: "异常",
  stopped: "已停止",
  idle: "空闲",
};
const PHASE_LABEL = {
  idle: "已停止",
  entering: "建仓中",
  running: "运行中",
  out_of_range: "区间外挂起",
  paused: "已暂停",
  stopped: "已停止",
};
const STOP_REASON_LABEL = {
  out_of_range: "价格突破区间",
  manual: "手动停止",
  circuit: "连续失败熔断",
  take_profit: "止盈",
  stop_loss: "止损",
  entry_failed: "建仓失败",
  shutdown: "进程退出",
  error: "内部错误",
};

const state = {
  tab: "overview",
  exchange: "",
  exchanges: [],
  statuses: {},
  config: {},
  status: null,
  symbols: [],
  levels: [],
  live: false,
  previewing: false,
  klines: [],
  klinesAt: 0,
};

let liveSeq = 0;

function $(id) {
  return document.getElementById(id);
}

function token() {
  const q = new URLSearchParams(location.search).get("token");
  if (q) localStorage.setItem("gridbot_token", q);
  return q || localStorage.getItem("gridbot_token") || "";
}

function headers() {
  const h = { Accept: "application/json" };
  const t = token();
  if (t) h.Authorization = "Bearer " + t;
  return h;
}

async function api(method, path, body, timeoutMs) {
  const ctrl = new AbortController();
  const ms = timeoutMs ?? (method === "GET" ? 8000 : 25000);
  const timer = setTimeout(() => ctrl.abort(), ms);
  try {
    const init = { method, headers: headers(), signal: ctrl.signal };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    const res = await fetch(path, init);
    const text = await res.text();
    let env = {};
    if (text) {
      try {
        env = JSON.parse(text);
      } catch {
        throw new Error(text || res.statusText);
      }
    }
    if (!res.ok || env.ok === false) {
      const err = (env.error && env.error.message) || res.statusText || "请求失败";
      const e = new Error(err);
      e.status = res.status;
      e.code = env.error && env.error.code;
      throw e;
    }
    return env.data;
  } catch (err) {
    if (err && err.name === "AbortError") {
      throw new Error("请求超时");
    }
    throw err;
  } finally {
    clearTimeout(timer);
  }
}

function switchTab(name) {
  state.tab = name;
  document.querySelectorAll(".tab-panel").forEach((el) => el.classList.remove("active"));
  document.querySelectorAll(".tab-btn").forEach((el) => {
    const on = name === "exchange" ? el.dataset.ex === state.exchange : el.dataset.tab === name && !el.dataset.ex;
    el.classList.toggle("active", !!on);
  });
  const panel = $(name === "exchange" ? "tab-exchange" : "tab-" + name);
  if (panel) panel.classList.add("active");
  if (name === "exchange") drawChart();
}

function exchangeLabel(name) {
  if (name === "rh_lighter") return "RH Lighter";
  if (name === "lighter") return "Lighter";
  if (name === "sodex") return "SODEx";
  return titleCase(name);
}

function toast(msg) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => el.classList.remove("show"), 2200);
}

function openModal(title, body, onOk) {
  $("modal-title").textContent = title;
  $("modal-body").textContent = body;
  $("modal").classList.add("show");
  $("modal-ok").onclick = () => {
    $("modal").classList.remove("show");
    onOk();
  };
}

function tickClock() {
  const el = $("clock");
  if (!el) return;
  const d = new Date();
  const p = (n) => String(n).padStart(2, "0");
  el.textContent = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function selected(group) {
  const el = document.querySelector(`.mode-btn.sel[data-group="${group}"]`);
  return el ? el.dataset.value : "";
}

function selectMode(group, value) {
  document.querySelectorAll(`.mode-btn[data-group="${group}"]`).forEach((el) => {
    el.classList.toggle("sel", el.dataset.value === value);
  });
}

function dec(v, digits) {
  if (v === undefined || v === null || v === "") return "";
  const n = Number(v);
  if (!Number.isFinite(n)) return String(v);
  if (digits === undefined) return String(v);
  return n.toFixed(digits);
}

function num(v, d) {
  const n = Number(v);
  return Number.isFinite(n) ? n.toFixed(d) : "—";
}

function signed(v) {
  const n = Number(v);
  if (!Number.isFinite(n)) return "—";
  const s = n.toFixed(2);
  return n > 0 ? "+" + s : s;
}

function setSigned(el, v) {
  if (!el) return;
  el.textContent = signed(v);
  el.classList.remove("up", "down");
  const n = Number(v);
  if (n > 0) el.classList.add("up");
  else if (n < 0) el.classList.add("down");
}

function fmtTime(v) {
  const d = v instanceof Date ? v : new Date(v);
  if (Number.isNaN(d.getTime())) return "—";
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtRuntime(resetAt, running) {
  if (!running || !resetAt) return "—";
  const start = new Date(resetAt).getTime();
  if (!Number.isFinite(start) || start < Date.parse("2020-01-01")) return "—";
  const sec = Math.floor((Date.now() - start) / 1000);
  if (!(sec >= 0)) return "—";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const p = (n) => String(n).padStart(2, "0");
  if (d > 0) return `${d}天 ${p(h)}:${p(m)}:${p(s)}`;
  if (h > 0) return `${h}小时 ${p(m)}:${p(s)}`;
  return `${m}分 ${p(s)}秒`;
}

function titleCase(name) {
  if (!name) return "—";
  return name.charAt(0).toUpperCase() + name.slice(1);
}

function strategySymbol() {
  const st = state.status || {};
  return st.symbol || (state.config && state.config.symbol) || "";
}

function isRunning(status) {
  return status === "running" || status === "starting" || status === "paused" || status === "reconnecting";
}

// 策略 phase 仍在运行态但实例 status 短暂不同步时（重连/竞态），仍应允许手动停止。
function canStop(st) {
  if (!st) return false;
  const status = st.status || "stopped";
  if (isRunning(status) || status === "error") return true;
  const phase = st.strategy && st.strategy.phase;
  return phase === "running" || phase === "entering" || phase === "out_of_range" || phase === "paused";
}

function isActiveInstance(st) {
  const s = (st && st.status) || "";
  return isRunning(s) || s === "error";
}

function toNum(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

function showBanner(msg, kind) {
  const el = $("banner");
  if (!msg) {
    el.hidden = true;
    el.textContent = "";
    return;
  }
  el.hidden = false;
  el.textContent = msg;
  el.classList.toggle("error", kind === "error");
  el.classList.toggle("warn", kind === "warn");
  el.classList.toggle("info", kind === "info");
}

function setConn(ok, text) {
  const badge = $("conn-badge");
  badge.classList.toggle("online", !!ok);
  badge.classList.toggle("warn", !ok);
  $("conn-text").textContent = text;
}

function isMartingale() {
  return (selected("kind") || "grid") === "martingale";
}

function principalOf(cfg) {
  if (!cfg) return 0;
  if (cfg.strategy === "martingale" || cfg.martingale) {
    return Number((cfg.martingale || {}).initial_margin) || 0;
  }
  const g = cfg.grid || {};
  if (g.sizing_mode === "margin") return Number(g.margin) || 0;
  const qty = Number(g.per_grid_qty) || 0;
  const count = Number(g.grid_count) || 0;
  const mid = (Number(g.lower_price) + Number(g.upper_price)) / 2;
  const lev = Number(cfg.leverage) || 1;
  if (qty > 0 && count > 0 && mid > 0 && lev > 0) return (qty * count * mid) / lev;
  return 0;
}

function annualizedReturn(pnl, capital, resetAt) {
  if (!(capital > 0) || !Number.isFinite(pnl)) return "";
  const start = resetAt ? new Date(resetAt).getTime() : NaN;
  if (!Number.isFinite(start)) return "";
  const days = (Date.now() - start) / 86400000;
  if (!(days > 1 / 1440)) return ""; // 不足 1 分钟不展示
  return ((pnl / capital) * (365 / days) * 100).toFixed(2);
}

// 空串不能交给后端的 decimal：JSON "" 无法转成数字。
function decOrOmit(v) {
  const s = String(v ?? "").trim();
  return s === "" ? undefined : s;
}

function collectParams() {
  const prev = state.config || {};
  const entry = { ...(prev.entry || {}) };
  const risk = { ...(prev.risk || {}) };
  delete entry.price;
  delete risk.stop_loss_price;
  delete risk.take_profit_price;
  entry.mode = $("entry-mode").value;
  if (entry.mode === "limit_price") {
    const px = decOrOmit($("entry-price").value);
    if (px) entry.price = px;
  }
  const kind = selected("kind") || "grid";
  const out = {
    ...prev,
    strategy: kind,
    symbol: $("symbol").value,
    direction: selected("direction") || "long",
    leverage: Number($("leverage").value) || 1,
    margin_mode: $("margin-mode").value,
    entry,
    risk,
    order: prev.order || {},
  };
  delete out.preset;
  if (kind === "martingale") {
    if (out.direction === "neutral") out.direction = "long";
    out.martingale = {
      ...(prev.martingale || {}),
      add_drop_pct: $("mg-drop").value,
      take_profit_pct: $("mg-tp").value,
      initial_margin: $("mg-initial").value,
      add_margin: $("mg-add").value,
      max_add_times: Number($("mg-times").value) || 1,
      add_multiplier: $("mg-mult").value,
      add_drop_mode: $("mg-mode").value,
      cycle_restart: $("mg-restart").value === "true",
      max_cycles: 0,
      preplace_adds: $("mg-preplace").value === "true",
    };
    if ($("mg-sl").value) out.risk = { ...out.risk, stop_loss_price: $("mg-sl").value };
    else if (out.risk) delete out.risk.stop_loss_price;
    return out;
  }
  const grid = { ...(prev.grid || {}) };
  grid.lower_price = decOrOmit($("lower").value);
  grid.upper_price = decOrOmit($("upper").value);
  grid.grid_count = Number($("count").value);
  grid.sizing_mode = $("sizing").value;
  if (grid.sizing_mode === "margin") {
    grid.margin = decOrOmit($("margin").value);
    delete grid.per_grid_qty;
  } else {
    grid.per_grid_qty = decOrOmit($("qty").value);
    delete grid.margin;
  }
  out.grid = grid;
  out.risk = { ...out.risk, out_of_range: $("out-of-range").value };
  return out;
}

function resetForm() {
  state.config = {};
  selectMode("kind", "grid");
  selectMode("direction", "long");
  setSymbol("", false);
  $("leverage").value = "10";
  $("margin-mode").value = "cross";
  $("lower").value = "72";
  $("upper").value = "77";
  $("count").value = "25";
  $("sizing").value = "margin";
  $("margin-wrap").style.display = "";
  $("qty-wrap").style.display = "none";
  $("margin").value = "1000";
  $("qty").value = "0.53";
  $("out-of-range").value = "pause";
  $("entry-mode").value = "maker_follow";
  $("entry-price").value = "";
  $("mg-drop").value = "2.0";
  $("mg-tp").value = "1.5";
  $("mg-initial").value = "50";
  $("mg-add").value = "50";
  $("mg-times").value = "5";
  $("mg-mult").value = "1.0";
  $("mg-mode").value = "from_last";
  $("mg-restart").value = "true";
  $("mg-preplace").value = "true";
  $("mg-sl").value = "";
  updateKind();
  updateEntryHint();
  updateDerived();
}

function fillForm(cfg) {
  if (!cfg || !cfg.symbol) {
    resetForm();
    return;
  }
  state.config = cfg;
  const kind = cfg.strategy === "martingale" || cfg.martingale ? "martingale" : "grid";
  selectMode("kind", kind);
  setSymbol(cfg.symbol, false);
  selectMode("direction", cfg.direction || "long");
  const g = cfg.grid || {};
  if (g.lower_price != null) $("lower").value = dec(g.lower_price);
  if (g.upper_price != null) $("upper").value = dec(g.upper_price);
  if (g.grid_count != null) $("count").value = String(g.grid_count);
  if (cfg.leverage != null) $("leverage").value = String(cfg.leverage);
  if (cfg.margin_mode) $("margin-mode").value = cfg.margin_mode;
  if (g.sizing_mode) $("sizing").value = g.sizing_mode;
  $("margin-wrap").style.display = $("sizing").value === "margin" ? "" : "none";
  $("qty-wrap").style.display = $("sizing").value === "margin" ? "none" : "";
  if (g.margin != null) $("margin").value = dec(g.margin);
  if (g.per_grid_qty != null) $("qty").value = dec(g.per_grid_qty);
  if (cfg.risk && cfg.risk.out_of_range) $("out-of-range").value = cfg.risk.out_of_range;
  if (cfg.entry && cfg.entry.mode) {
    $("entry-mode").value = cfg.entry.mode === "market" ? "maker_follow" : cfg.entry.mode;
  }
  if (cfg.entry && cfg.entry.price != null) $("entry-price").value = dec(cfg.entry.price);
  const mg = cfg.martingale || {};
  if (mg.add_drop_pct != null) $("mg-drop").value = dec(mg.add_drop_pct);
  if (mg.take_profit_pct != null) $("mg-tp").value = dec(mg.take_profit_pct);
  if (mg.initial_margin != null) $("mg-initial").value = dec(mg.initial_margin);
  if (mg.add_margin != null) $("mg-add").value = dec(mg.add_margin);
  if (mg.max_add_times != null) $("mg-times").value = String(mg.max_add_times);
  if (mg.add_multiplier != null) $("mg-mult").value = dec(mg.add_multiplier);
  if (mg.add_drop_mode) $("mg-mode").value = mg.add_drop_mode;
  if (mg.cycle_restart != null) $("mg-restart").value = String(mg.cycle_restart);
  if (mg.preplace_adds != null) $("mg-preplace").value = String(mg.preplace_adds);
  if (cfg.risk && cfg.risk.stop_loss_price) $("mg-sl").value = dec(cfg.risk.stop_loss_price);
  updateKind();
  updateEntryHint();
  updateDerived();
}

function applyExchangePanel(st) {
  state.status = st || null;
  const empty = !st;
  const mark = empty ? "" : st.mark;
  const pos = (st && st.position) || {};
  const acct = (st && st.account) || {};
  const strat = (st && st.strategy) || {};
  const stats = strat.stats || {};
  const cfg = state.config || {};
  const symbol = (st && st.symbol) || cfg.symbol || "";
  const status = (st && st.status) || "stopped";
  const dir = strat.direction || cfg.direction || "";
  const target = strat.order_target || 0;
  const resting = strat.order_resting || 0;
  const realized = stats.realized_pnl ?? stats.grid_profit;
  const unreal = stats.unrealized_pnl ?? pos.unrealized_pnl ?? acct.unrealized_pnl;
  const now = new Date();
  const running = isRunning(status);
  const ex = exchangeLabel(state.exchange);

  $("st-mark").textContent = mark ? dec(mark) : "—";
  $("st-pos").textContent = symbol ? `${dec(pos.size || 0)} / ${dec(pos.entry_price || mark || "—")}` : "—";
  $("st-bal").textContent = num(acct.balance, 2);
  $("st-eq").textContent = num(acct.equity, 2);
  setSigned($("st-real"), realized);
  setSigned($("st-unreal"), unreal);
  $("st-liq").textContent = pos.liquidation_price ? num(pos.liquidation_price, 2) : "—";
  const apr = annualizedReturn(Number(realized || 0) + Number(unreal || 0), principalOf(cfg), stats.reset_at);
  if (apr === "") {
    $("st-apr").textContent = "—";
    $("st-apr").classList.remove("up", "down");
  } else {
    setSigned($("st-apr"), apr);
    $("st-apr").textContent += "%";
  }
  $("st-grids").textContent = String(stats.completed_grids ?? 0);
  $("st-runtime").textContent = fmtRuntime(stats.reset_at, running);

  let phase = PHASE_LABEL[strat.phase] || STATUS_LABEL[status] || status || "—";
  const stopReason = st && st.stop_reason;
  if (!running && stopReason && STOP_REASON_LABEL[stopReason]) {
    phase = `已停止（${STOP_REASON_LABEL[stopReason]}）`;
  }
  const dirTxt = DIR_LABEL[dir] ? DIR_LABEL[dir] + "网格" : "网格";
  $("st-phase").textContent = running ? `${phase}（${dirTxt}）` : phase;
  $("st-conn").classList.toggle("online", running);
  $("st-conn").classList.toggle("warn", status === "error" || status === "reconnecting");
  if (!running && stopReason === "out_of_range") {
    showBanner("价格已突破区间，策略已按「停止并撤单」自动停止（仓位保留）。无需再点停止，可直接改配置后重新启动。", "info");
  } else if (running && strat.phase === "out_of_range") {
    showBanner("价格突破区间，网格已挂起等待回归（原有挂单保留）。可点「停止策略」撤单留仓。", "info");
  } else {
    showBanner("");
  }

  $("st-watch").innerHTML = `挂单看门狗 · 目标 <b>${target || "—"}</b> / 已确认 <b>${resting}</b> · ${symbol ? symbol + " · " : ""}更新于 ${fmtTime(now)}`;
  $("st-progress-txt").textContent = `挂单 ${resting} / 目标 ${target || "—"}`;
  $("st-progress-bar").style.width = target ? Math.min(100, (resting / target) * 100) + "%" : "0%";

  $("btn-start").textContent = isMartingale() ? `启动 ${ex} 马丁` : `启动 ${ex} 网格`;
  const gridsLbl = $("st-grids") && $("st-grids").previousElementSibling;
  if (gridsLbl) gridsLbl.textContent = isMartingale() ? "完成周期" : "完成格";
  lockForm(running, st);
  drawChart();
}

function pnlOf(st) {
  if (!st) return { realized: 0, unreal: 0, total: 0 };
  const pos = st.position || {};
  const acct = st.account || {};
  const stats = (st.strategy && st.strategy.stats) || {};
  const realized = toNum(stats.realized_pnl ?? stats.grid_profit);
  const unreal = toNum(stats.unrealized_pnl ?? pos.unrealized_pnl ?? acct.unrealized_pnl);
  return { realized, unreal, total: realized + unreal };
}

function instanceCardHTML(name, st) {
  const meta = state.exchanges.find((e) => e.name === name) || {};
  const label = exchangeLabel(name);
  const net = meta.network ? String(meta.network).toUpperCase() : "";
  const pos = (st && st.position) || {};
  const acct = (st && st.account) || {};
  const strat = (st && st.strategy) || {};
  const symbol = (st && st.symbol) || "";
  const status = (st && st.status) || "stopped";
  const dir = strat.direction || "";
  const lev = pos.leverage || "";
  const mm = pos.margin_mode || "";
  const lower = strat.lower_price;
  const upper = strat.upper_price;
  const count = strat.grid_count;
  const target = strat.order_target || 0;
  const resting = strat.order_resting || 0;
  const mark = st && st.mark;
  const running = isRunning(status);
  const dirLine = [DIR_LABEL[dir] || "—", lev ? lev + "x" : "", mm === "isolated" ? "逐仓" : mm === "cross" ? "全仓" : ""]
    .filter(Boolean)
    .join(" · ");
  const range = lower && upper ? `${dec(lower)} – ${dec(upper)} · ${count || "—"} 格` : "—";
  return `<article class="ov-card" data-ex="${name}">
    <div class="ov-exname"><span class="run-dot${running ? " on" : ""}"></span><span>${label}${net ? " " + net : ""}</span></div>
    <div class="kv"><span class="lbl">交易对</span><span class="val">${symbol || "—"}</span></div>
    <div class="kv"><span class="lbl">方向</span><span class="val">${dirLine}</span></div>
    <div class="kv"><span class="lbl">区间</span><span class="val">${range}</span></div>
    <div class="kv"><span class="lbl">最新价</span><span class="val">${mark ? dec(mark) : "—"}</span></div>
    <div class="kv"><span class="lbl">持仓</span><span class="val">${symbol ? `${dec(pos.size || 0)} ${symbol}` : "—"}</span></div>
    <div class="kv"><span class="lbl">挂单健康</span><span class="val">目标 ${target || "—"} / 已确认 ${resting}</span></div>
    <div class="kv"><span class="lbl">权益</span><span class="val">${num(acct.equity, 2)}</span></div>
    <div class="kv"><span class="lbl">状态</span><span class="val">${STATUS_LABEL[status] || status}</span></div>
    <button type="button" class="btn btn-ghost" data-goto-ex="${name}">进入 ${label} 控制台 →</button>
  </article>`;
}

function renderOverview() {
  const list = state.exchanges || [];
  const active = list.filter((ex) => isActiveInstance(state.statuses[ex.name]));
  const box = $("ov-instances");
  if (box) {
    box.hidden = active.length === 0;
    box.innerHTML = active.map((ex) => instanceCardHTML(ex.name, state.statuses[ex.name])).join("");
  }

  let equity = 0;
  let avail = 0;
  let realized = 0;
  let unreal = 0;
  let resting = 0;
  let completed = 0;
  let target = 0;
  let hasAcct = false;
  for (const ex of list) {
    const st = state.statuses[ex.name];
    if (!st) continue;
    const acct = st.account || {};
    if (acct.equity != null || acct.available != null) hasAcct = true;
    equity += toNum(acct.equity);
    avail += toNum(acct.available);
    const pnl = pnlOf(st);
    realized += pnl.realized;
    unreal += pnl.unreal;
    const strat = st.strategy || {};
    const stats = strat.stats || {};
    resting += toNum(strat.order_resting);
    completed += toNum(stats.completed_grids);
    target += toNum(strat.order_target);
  }

  $("ov-equity").textContent = hasAcct ? equity.toFixed(2) : "—";
  $("ov-avail").textContent = hasAcct ? avail.toFixed(2) : "—";
  if (hasAcct || active.length) {
    setSigned($("ov-pnl"), realized + unreal);
    setSigned($("ov-real"), realized);
    setSigned($("ov-unreal"), unreal);
  } else {
    ["ov-pnl", "ov-real", "ov-unreal"].forEach((id) => {
      const el = $(id);
      el.textContent = "—";
      el.classList.remove("up", "down");
    });
  }
  $("ov-orders").textContent = list.length ? `${resting} / ${completed}` : "—";
  $("ov-target").textContent = target ? String(target) : "—";

  if (!active.length) {
    $("ov-status").textContent = "未启动";
    $("ov-status-detail").textContent = "无运行中的策略";
    return;
  }
  if (active.length === 1) {
    const st = state.statuses[active[0].name] || {};
    const dir = (st.strategy && st.strategy.direction) || "";
    $("ov-status").textContent = STATUS_LABEL[st.status] || st.status || "运行中";
    $("ov-status-detail").textContent = [exchangeLabel(active[0].name), st.symbol, DIR_LABEL[dir] ? DIR_LABEL[dir] + "网格" : ""]
      .filter(Boolean)
      .join(" · ");
    return;
  }
  $("ov-status").textContent = `${active.length} 个运行中`;
  $("ov-status-detail").textContent = active.map((ex) => exchangeLabel(ex.name)).join(" · ");
}

function lockForm(running, st) {
  const freeze = ["symbol-trigger", "leverage", "margin-mode", "sizing", "margin", "qty", "entry-mode", "entry-price", "out-of-range", "mg-drop", "mg-tp", "mg-initial", "mg-add", "mg-times", "mg-mult", "mg-mode", "mg-restart", "mg-preplace", "mg-sl"];
  freeze.forEach((id) => {
    const el = $(id);
    if (el) el.disabled = running;
  });
  document.querySelectorAll('.mode-btn[data-group="direction"], .mode-btn[data-group="kind"]').forEach((el) => {
    el.style.pointerEvents = running ? "none" : "";
    el.style.opacity = running ? "0.55" : "";
  });
  $("btn-start").disabled = running;
  $("btn-stop").disabled = !canStop(st);
  $("btn-adjust").disabled = !running || isMartingale();
  $("btn-adjust").hidden = isMartingale();
  $("btn-cancel").disabled = !running;
  $("btn-refill").disabled = !running;
}

function renderTrades(list) {
  const body = $("trades-body");
  const rows = Array.isArray(list) ? list : [];
  if (!rows.length) {
    body.innerHTML = `<tr><td colspan="4" class="muted">暂无成交</td></tr>`;
    return;
  }
  body.innerHTML = rows
    .slice(0, 30)
    .map((t) => {
      const buy = String(t.side).toLowerCase() === "buy";
      return `<tr><td>${fmtTime(t.ts)}</td><td><span class="pill ${buy ? "pill-buy" : "pill-sell"}">${buy ? "买" : "卖"}</span></td><td>${t.price}</td><td>${t.qty}</td></tr>`;
    })
    .join("");
}

function renderLogs(list) {
  const box = $("log-box");
  const rows = Array.isArray(list) ? list : [];
  const sym = strategySymbol();
  const filtered = rows.filter((r) => {
    const a = r.attrs || {};
    return !sym || !a.symbol || a.symbol === sym;
  });
  if (!filtered.length) {
    box.innerHTML = `<div class="log-item">暂无日志</div>`;
    return;
  }
  box.innerHTML = filtered
    .slice(0, 40)
    .map((r) => {
      const lv = String(r.level || "").toLowerCase();
      const cls = lv === "error" ? "err" : lv === "warn" ? "warn" : lv === "info" ? "ok" : "";
      return `<div class="log-item ${cls}">${fmtTime(r.ts)} ${r.msg || ""}</div>`;
    })
    .join("");
}

function renderChips(system, exchanges) {
  const box = $("ex-chips");
  const sys = ((system && system.exchanges) || []).reduce((m, e) => {
    m[e.name] = e.status;
    return m;
  }, {});
  const list = exchanges && exchanges.length ? exchanges : [{ name: "—", network: "" }];
  box.innerHTML = list
    .map((e) => {
      const st = sys[e.name] || "idle";
      const cls = st === "running" || st === "starting" ? "ok" : st === "error" ? "error" : st === "reconnecting" ? "warn" : "idle";
      const label = exchangeLabel(e.name);
      const net = e.network ? String(e.network).toUpperCase() : "";
      return `<span class="ex-chip"><i class="hdot ${cls}"></i>${label}${net ? `<span class="ex-chip-net">${net}</span>` : ""}</span>`;
    })
    .join("");
}

function updateKind() {
  const mart = isMartingale();
  $("grid-fields").hidden = mart;
  $("martingale-fields").hidden = !mart;
  const neu = document.querySelector('.mode-btn[data-group="direction"][data-value="neutral"]');
  if (neu) neu.hidden = mart;
  if (mart && selected("direction") === "neutral") selectMode("direction", "long");
  $("btn-adjust").hidden = mart;
  drawChart();
}

function updateDerived() {
  const box = $("derived");
  const warn = $("warn");
  if (isMartingale()) {
    const drop = Number($("mg-drop").value);
    const times = Number($("mg-times").value);
    const initial = Number($("mg-initial").value);
    const add = Number($("mg-add").value);
    const mult = Number($("mg-mult").value) || 1;
    if (!(drop > 0) || times < 1 || !(initial > 0) || !(add > 0)) {
      box.innerHTML = "";
      warn.classList.add("show");
      warn.textContent = "加仓间距、次数与保证金必须大于 0。";
      return;
    }
    let total = initial;
    for (let k = 0; k < times; k++) total += add * Math.pow(mult, k);
    box.innerHTML = `加满仓保证金约 <b>${total.toFixed(2)} USDC</b> · 预挂 <b>${times + 1}</b> 笔（加仓 + 止盈）`;
    warn.classList.remove("show");
    schedulePreview();
    return;
  }
  const lower = Number($("lower").value);
  const upper = Number($("upper").value);
  const count = Number($("count").value);
  const leverage = Number($("leverage").value) || 1;
  if (!(upper > lower) || count < 2) {
    box.innerHTML = "";
    warn.classList.add("show");
    warn.textContent = "上边界必须大于下边界，格子数至少 2。";
    return;
  }
  const step = (upper - lower) / count;
  const stepPct = (step / ((lower + upper) / 2)) * 100;
  const sizing = $("sizing").value;
  let perQty;
  let margin;
  if (sizing === "margin") {
    margin = Number($("margin").value) || 0;
    const notional = margin * leverage;
    perQty = notional / count / ((lower + upper) / 2);
  } else {
    perQty = Number($("qty").value) || 0;
    margin = (perQty * count * ((lower + upper) / 2)) / leverage;
  }
  const gross = step * perQty;
  box.innerHTML =
    `单格间距 <b>${step.toFixed(3)}</b> / <b>${stepPct.toFixed(3)}%</b> · ` +
    `每格毛利 <b>${gross.toFixed(4)}</b> · ` +
    `约需保证金 <b>${margin.toFixed(2)} USDC</b> · ` +
    `满铺挂单 <b>${count}</b> 笔`;
  if (stepPct / 100 <= FEE) {
    warn.classList.add("show");
    warn.textContent = "单格间距可能盖不住双边手续费，网格越跑越亏。";
  } else {
    warn.classList.remove("show");
  }
  schedulePreview();
}

function applyPreview(d) {
  if (!d) return;
  const box = $("derived");
  const warn = $("warn");
  if (isMartingale() || d.levels) {
    const margin = Number(d.total_margin ?? d.margin_required);
    const liq = Number(d.liquidation_price);
    const orders = d.order_count;
    const dd = Number(d.max_drawdown_pct);
    box.innerHTML =
      `加满仓保证金 <b>${Number.isFinite(margin) ? margin.toFixed(2) : "—"} USDC</b> · ` +
      `强平价 <b>${Number.isFinite(liq) ? liq.toFixed(2) : "—"}</b> · ` +
      `最大回撤 <b>${Number.isFinite(dd) ? dd.toFixed(1) : "—"}%</b> · ` +
      `挂单 <b>${orders ?? "—"}</b> 笔`;
    const ws = Array.isArray(d.warnings) ? d.warnings : [];
    if (ws.length) {
      warn.classList.add("show");
      warn.textContent = ws.map((w) => w.message || w).join(" ");
    } else {
      warn.classList.remove("show");
    }
    return;
  }
  const step = Number(d.step);
  const stepPct = Number(d.step_pct);
  const profit = Number(d.net_grid_profit ?? d.grid_profit);
  const margin = Number(d.margin_required);
  const orders = d.order_count ?? d.grid_count;
  box.innerHTML =
    `单格间距 <b>${Number.isFinite(step) ? step.toFixed(4) : "—"}</b> / <b>${Number.isFinite(stepPct) ? stepPct.toFixed(3) : "—"}%</b> · ` +
    `每格净利 <b>${Number.isFinite(profit) ? profit.toFixed(4) : "—"}</b> · ` +
    `约需保证金 <b>${Number.isFinite(margin) ? margin.toFixed(2) : "—"} USDC</b> · ` +
    `满铺挂单 <b>${orders ?? "—"}</b> 笔`;
  const ws = Array.isArray(d.warnings) ? d.warnings : [];
  if (ws.length) {
    warn.classList.add("show");
    warn.textContent = ws.map((w) => w.message || w).join(" ");
  } else {
    warn.classList.remove("show");
    warn.textContent = "";
  }
}

function schedulePreview() {
  clearTimeout(schedulePreview._t);
  schedulePreview._t = setTimeout(runPreview, 450);
}

async function runPreview() {
  if (!state.exchange || !$("symbol").value) return;
  if (state.previewing) return;
  const params = collectParams();
  if (!params.symbol) return;
  state.previewing = true;
  try {
    const d = await api("POST", `/api/exchanges/${state.exchange}/preview`, params);
    applyPreview(d);
  } catch (err) {
    const warn = $("warn");
    warn.classList.add("show");
    warn.textContent = err.message;
  } finally {
    state.previewing = false;
  }
}

function updateEntryHint() {
  const dir = selected("direction");
  const mode = $("entry-mode").value;
  const hint = $("entry-hint");
  $("entry-price-wrap").hidden = mode !== "limit_price";
  if (dir === "neutral") {
    hint.textContent = "中性网格默认不建底仓，建仓方式在运行时不生效。";
    return;
  }
  const map = {
    maker_follow: "做多挂买一、做空挂卖一，盘口移动则撤单重挂。默认 post-only；建仓超时改市价吃剩余量。",
    market: "按 Maker 跟价限价挂，不主动吃单；建仓超时改市价吃剩余量。",
    limit_price: "在指定价挂 post-only。建仓超时改市价吃剩余量。",
  };
  hint.textContent = (isMartingale() ? "做多先建首单仓位。" : "做多先建底仓（上方卖格数量之和）。") + " " + map[mode];
}

function bindModes() {
  document.querySelectorAll(".mode-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(`.mode-btn[data-group="${btn.dataset.group}"]`).forEach((el) => el.classList.remove("sel"));
      btn.classList.add("sel");
      if (btn.dataset.group === "kind") updateKind();
      updateEntryHint();
      updateDerived();
    });
  });
}

function symbolName(s) {
  const t = s.type && s.type !== "perp" ? s.type : "永续";
  return `${s.symbol} ${t}`;
}

function renderSymbolOptions(q) {
  const box = $("symbol-options");
  const needle = (q || "").trim().toLowerCase();
  const items = (state.symbols || []).filter((s) => {
    const name = symbolName(s).toLowerCase();
    return !needle || s.symbol.toLowerCase().includes(needle) || name.includes(needle);
  });
  const cur = $("symbol").value;
  if (!items.length) {
    box.innerHTML = `<div class="combo-empty">无匹配交易对</div>`;
    return;
  }
  box.innerHTML = items
    .map((s) => `<div class="combo-option${s.symbol === cur ? " active" : ""}" data-s="${s.symbol}">${s.symbol}<span class="sub">${symbolName(s)}</span></div>`)
    .join("");
  box.querySelectorAll(".combo-option").forEach((el) => {
    el.onclick = () => setSymbol(el.dataset.s, true);
  });
}

function openCombo() {
  if ($("symbol-trigger").disabled) return;
  $("symbol-combo").classList.add("open");
  $("symbol-menu").hidden = false;
  $("symbol-trigger").setAttribute("aria-expanded", "true");
  $("symbol-filter").value = "";
  renderSymbolOptions("");
  $("symbol-filter").focus();
}

function closeCombo() {
  $("symbol-combo").classList.remove("open");
  $("symbol-menu").hidden = true;
  $("symbol-trigger").setAttribute("aria-expanded", "false");
}

function setSymbol(sym, fromUser) {
  $("symbol").value = sym;
  $("symbol-label").textContent = sym || "选择交易对";
  closeCombo();
  if (fromUser) {
    updateDerived();
    drawChart();
  }
}

function bindCombo() {
  $("symbol-trigger").onclick = (e) => {
    e.stopPropagation();
    if ($("symbol-combo").classList.contains("open")) closeCombo();
    else openCombo();
  };
  $("symbol-filter").addEventListener("input", () => renderSymbolOptions($("symbol-filter").value));
  $("symbol-clear").onclick = (e) => {
    e.stopPropagation();
    $("symbol-filter").value = "";
    renderSymbolOptions("");
    $("symbol-filter").focus();
  };
  document.addEventListener("click", (e) => {
    if (!e.target.closest("#symbol-combo")) closeCombo();
  });
}

function klinePoints() {
  const rows = Array.isArray(state.klines) ? state.klines : [];
  const pts = rows
    .map((k) => {
      const t = Date.parse(k.open_time);
      const close = Number(k.close);
      return { t, p: close, h: Number(k.high), l: Number(k.low) };
    })
    .filter((p) => Number.isFinite(p.t) && Number.isFinite(p.p) && p.p > 0);
  const mark = Number(state.status && state.status.mark);
  if (pts.length && Number.isFinite(mark) && mark > 0) {
    const last = pts[pts.length - 1];
    if (Date.now() - last.t > 30 * 1000) {
      pts.push({ t: Date.now(), p: mark, h: mark, l: mark });
    } else {
      last.p = mark;
    }
  }
  return pts;
}

function fmtAxisTime(ms) {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:00`;
}

function drawChart() {
  const canvas = $("chart");
  if (!canvas || !$("strategy-form")) return;
  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth;
  const h = canvas.clientHeight;
  if (!w || !h) return;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  ctx.fillStyle = "#0f1420";
  ctx.fillRect(0, 0, w, h);

  const st = state.status || {};
  const cfg = state.config || {};
  const strat = st.strategy || {};
  const cells = (state.levels && state.levels.length ? state.levels : strat.cells) || [];
  const formLower = Number($("lower").value);
  const formUpper = Number($("upper").value);
  const formCount = Number($("count").value) || 0;
  let gridLo = Number(strat.lower_price || (cfg.grid && cfg.grid.lower_price) || formLower);
  let gridHi = Number(strat.upper_price || (cfg.grid && cfg.grid.upper_price) || formUpper);
  if (cells.length) {
    gridLo = Math.min(...cells.map((c) => Number(c.low)));
    gridHi = Math.max(...cells.map((c) => Number(c.high)));
  }
  const mark = Number(st.mark);
  const pts = klinePoints();

  let yMin = Number.isFinite(gridLo) ? gridLo : NaN;
  let yMax = Number.isFinite(gridHi) ? gridHi : NaN;
  pts.forEach((p) => {
    yMin = Number.isFinite(yMin) ? Math.min(yMin, p.l || p.p, p.p) : p.p;
    yMax = Number.isFinite(yMax) ? Math.max(yMax, p.h || p.p, p.p) : p.p;
  });
  if (Number.isFinite(mark) && mark > 0) {
    yMin = Number.isFinite(yMin) ? Math.min(yMin, mark) : mark;
    yMax = Number.isFinite(yMax) ? Math.max(yMax, mark) : mark;
  }
  if (!(yMax > yMin)) {
    return;
  }
  const padY = (yMax - yMin) * 0.06 || 0.01;
  yMin -= padY;
  yMax += padY;

  const pad = { l: 10, r: 54, t: 12, b: 26 };
  const yOf = (p) => pad.t + ((yMax - p) / (yMax - yMin)) * (h - pad.t - pad.b);
  const t0 = pts.length ? pts[0].t : Date.now() - 72 * 3600 * 1000;
  const t1 = pts.length ? pts[pts.length - 1].t : Date.now();
  const span = Math.max(1, t1 - t0);
  const xOf = (t) => pad.l + ((t - t0) / span) * (w - pad.l - pad.r);

  ctx.strokeStyle = "#1e2d45";
  ctx.lineWidth = 1;
  ctx.beginPath();
  ctx.moveTo(pad.l, pad.t);
  ctx.lineTo(w - pad.r, pad.t);
  ctx.lineTo(w - pad.r, h - pad.b);
  ctx.lineTo(pad.l, h - pad.b);
  ctx.stroke();

  const levels = [];
  if (cells.length) {
    cells.forEach((c) => {
      const px = Number(c.price || (String(c.side).toLowerCase() === "sell" ? c.high : c.low));
      if (Number.isFinite(px)) levels.push({ px, sell: String(c.side).toLowerCase() === "sell", strong: c.state === "resting" || c.state === "open" });
    });
  } else if (Number.isFinite(gridLo) && Number.isFinite(gridHi) && gridHi > gridLo) {
    const count = formCount || Number(strat.grid_count) || 25;
    const step = (gridHi - gridLo) / count;
    for (let i = 0; i <= count; i++) {
      const px = gridLo + step * i;
      levels.push({ px, sell: Number.isFinite(mark) && px > mark, strong: false });
    }
  }
  levels.forEach((lv) => {
    const y = yOf(lv.px);
    ctx.beginPath();
    ctx.moveTo(pad.l, y);
    ctx.lineTo(w - pad.r, y);
    ctx.strokeStyle = lv.sell ? "#ef444455" : "#22c55e55";
    ctx.lineWidth = lv.strong ? 1.3 : 1;
    ctx.stroke();
  });

  if (pts.length >= 2) {
    ctx.beginPath();
    pts.forEach((p, i) => {
      const x = xOf(p.t);
      const y = yOf(p.p);
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.strokeStyle = "#60a5fa";
    ctx.lineWidth = 1.6;
    ctx.stroke();
  }

  if (pts.length >= 1) {
    const last = pts[pts.length - 1];
    const lx = xOf(last.t);
    const ly = yOf(last.p);
    const price = Number.isFinite(mark) && mark > 0 ? mark : last.p;
    const label = num(price, price >= 100 ? 2 : 4);
    ctx.beginPath();
    ctx.arc(lx, ly, 3.5, 0, Math.PI * 2);
    ctx.fillStyle = "#60a5fa";
    ctx.fill();
    ctx.strokeStyle = "#0f1420";
    ctx.lineWidth = 1.2;
    ctx.stroke();
    ctx.fillStyle = "#e2e8f0";
    ctx.font = "11px sans-serif";
    ctx.textAlign = "right";
    ctx.fillText(label, Math.max(pad.l + 4, lx - 8), ly - 8);
  }

  if (Number.isFinite(mark) && mark > 0) {
    const my = yOf(mark);
    ctx.setLineDash([4, 4]);
    ctx.beginPath();
    ctx.moveTo(pad.l, my);
    ctx.lineTo(w - pad.r, my);
    ctx.strokeStyle = "#93c5fd";
    ctx.lineWidth = 1;
    ctx.stroke();
    ctx.setLineDash([]);
  }

  ctx.fillStyle = "#6b82a0";
  ctx.font = "10px sans-serif";
  ctx.textAlign = "left";
  const yTicks = 5;
  for (let i = 0; i <= yTicks; i++) {
    const p = yMax - ((yMax - yMin) * i) / yTicks;
    const y = yOf(p);
    ctx.fillText(p.toFixed(p >= 100 ? 1 : 3), w - pad.r + 6, y + 3);
  }

  ctx.textAlign = "center";
  if (pts.length) {
    const n = Math.min(5, pts.length);
    for (let i = 0; i < n; i++) {
      const idx = n === 1 ? 0 : Math.round((i * (pts.length - 1)) / (n - 1));
      const p = pts[idx];
      ctx.fillText(fmtAxisTime(p.t), xOf(p.t), h - 8);
    }
  }
  ctx.textAlign = "left";
}

async function refreshKlines(force) {
  if (!state.exchange) return;
  const sym = strategySymbol();
  if (!sym) return;
  const now = Date.now();
  if (!force && state.klinesAt && now - state.klinesAt < 60000) return;
  try {
    const list = await api("GET", `/api/exchanges/${state.exchange}/klines?symbol=${encodeURIComponent(sym)}&interval=1h&limit=72`);
    state.klines = Array.isArray(list) ? list : [];
    state.klinesAt = now;
    drawChart();
  } catch {
    /* 保留上一批 K 线 */
  }
}

function bindForms() {
  ["lower", "upper", "count", "leverage", "margin", "qty", "entry-price", "mg-drop", "mg-tp", "mg-initial", "mg-add", "mg-times", "mg-mult", "mg-sl"].forEach((id) => {
    const el = $(id);
    if (!el) return;
    el.addEventListener("input", () => {
      updateDerived();
      drawChart();
    });
  });
  $("sizing").addEventListener("change", () => {
    const margin = $("sizing").value === "margin";
    $("margin-wrap").style.display = margin ? "" : "none";
    $("qty-wrap").style.display = margin ? "none" : "";
    updateDerived();
  });
  $("entry-mode").addEventListener("change", updateEntryHint);
  $("out-of-range").addEventListener("change", schedulePreview);
  $("margin-mode").addEventListener("change", schedulePreview);
  ["mg-mode", "mg-restart", "mg-preplace"].forEach((id) => {
    const el = $(id);
    if (el) el.addEventListener("change", schedulePreview);
  });
}

function renderExTabs() {
  const nav = $("main-tabs");
  if (!nav) return;
  nav.querySelectorAll(".tab-btn[data-ex]").forEach((el) => el.remove());
  for (const ex of state.exchanges) {
    const btn = document.createElement("button");
    btn.className = "tab-btn";
    btn.dataset.tab = "exchange";
    btn.dataset.ex = ex.name;
    btn.textContent = exchangeLabel(ex.name);
    btn.addEventListener("click", () => selectExchange(ex.name));
    nav.appendChild(btn);
  }
  switchTab(state.tab || "overview");
}

async function loadExchangeData(name) {
  state.exchange = name;
  state.klines = [];
  state.klinesAt = 0;
  state.levels = [];
  resetForm();
  applyExchangePanel(state.statuses[name] || null);
  renderTrades([]);
  renderLogs([]);
  const [cfg, symbols] = await Promise.all([
    api("GET", `/api/exchanges/${name}/config`).catch(() => ({})),
    api("GET", `/api/exchanges/${name}/symbols`).catch(() => []),
  ]);
  if (state.exchange !== name) return;
  state.symbols = Array.isArray(symbols) ? symbols : [];
  if (cfg && cfg.symbol && !state.symbols.some((s) => s.symbol === cfg.symbol)) {
    state.symbols.unshift({ symbol: cfg.symbol, type: "perp" });
  }
  fillForm(cfg && cfg.symbol ? cfg : {});
}

async function selectExchange(name) {
  if (state.exchange !== name) {
    await loadExchangeData(name);
  }
  switchTab("exchange");
  await refreshKlines(true);
}

async function loadBootstrap() {
  const [exchanges, system] = await Promise.all([
    api("GET", "/api/exchanges"),
    api("GET", "/api/system/status").catch(() => ({})),
  ]);
  state.exchanges = Array.isArray(exchanges) ? exchanges : [];
  if (!state.exchanges.length) throw new Error("没有已启用的交易所");
  renderChips(system, state.exchanges);
  renderExTabs();
  renderOverview();
  await loadExchangeData(state.exchanges[0].name);
}

async function refreshLive() {
  const names = (state.exchanges || []).map((e) => e.name);
  if (!names.length) return;
  const seq = ++liveSeq;
  const current = state.exchange;
  const statusReqs = names.map((n) => api("GET", `/api/exchanges/${n}/status`).catch(() => null));
  const extra = current
    ? [
        api("GET", `/api/exchanges/${current}/trades?limit=30`).catch(() => []),
        api("GET", `/api/exchanges/${current}/logs?limit=40`).catch(() => []),
        api("GET", `/api/exchanges/${current}/levels`).catch(() => []),
      ]
    : [Promise.resolve([]), Promise.resolve([]), Promise.resolve([])];
  const all = await Promise.all([
    api("GET", "/api/system/status").catch(() => ({})),
    ...statusReqs,
    ...extra,
  ]);
  if (seq !== liveSeq) return;
  const system = all[0];
  const map = {};
  names.forEach((n, i) => {
    map[n] = all[1 + i] || state.statuses[n] || null;
  });
  state.statuses = map;
  renderOverview();
  renderChips(system, state.exchanges);
  if (state.exchange) applyExchangePanel(map[state.exchange] || null);
  if (current && state.exchange === current) {
    renderTrades(all[1 + names.length]);
    renderLogs(all[2 + names.length]);
    state.levels = Array.isArray(all[3 + names.length]) ? all[3 + names.length] : [];
  }
  if (state.tab === "exchange") await refreshKlines(false);
  if (seq !== liveSeq) return;
  state.live = true;
  const cur = map[state.exchange];
  const curStatus = (cur && cur.status) || "stopped";
  setConn(true, STATUS_LABEL[curStatus] || "已连接");
}

async function tick() {
  if (tick.busy) return;
  tick.busy = true;
  try {
    await refreshLive();
  } catch (err) {
    state.live = false;
    setConn(false, "连接失败");
    showBanner(err.message || "无法连接后端 API", "error");
  } finally {
    tick.busy = false;
  }
}

function bindActions() {
  const overview = document.querySelector('.tab-btn[data-tab="overview"]');
  if (overview) {
    overview.addEventListener("click", () => switchTab("overview"));
  }
  const instances = $("ov-instances");
  if (instances) {
    instances.addEventListener("click", (e) => {
      const btn = e.target.closest("[data-goto-ex]");
      if (btn) selectExchange(btn.dataset.gotoEx);
    });
  }
  $("btn-start").onclick = () =>
    openModal("启动网格", "将保存当前配置并启动。校验通过后建仓、再铺网格。停止时只撤本交易对挂单、保留仓位。", async () => {
      try {
        const params = collectParams();
        if (!params.symbol) throw new Error("请选择交易对");
        await api("PUT", `/api/exchanges/${state.exchange}/config`, params);
        state.config = params;
        await api("POST", `/api/exchanges/${state.exchange}/start`);
        toast("已启动");
        await tick();
      } catch (err) {
        toast(err.message);
      }
    });
  $("btn-stop").onclick = () =>
    openModal("停止策略", "只撤销本交易对挂单，保留仓位。不会市价平仓。", async () => {
      try {
        await api("POST", `/api/exchanges/${state.exchange}/stop`, undefined, 35000);
        toast("已停止（撤单留仓）");
        await tick();
      } catch (err) {
        toast(err.message);
      }
    });
  $("btn-adjust").onclick = () =>
    openModal("调整区间", "全撤重铺到表单中的新区间，不停止实例。", async () => {
      try {
        await api("POST", `/api/exchanges/${state.exchange}/adjust-range`, {
          lower_price: $("lower").value,
          upper_price: $("upper").value,
          grid_count: Number($("count").value),
        });
        toast("已提交调区间");
        await tick();
      } catch (err) {
        toast(err.message);
      }
    });
  $("btn-cancel").onclick = () =>
    openModal("撤销挂单", "只撤本交易对挂单，仓位不动。", async () => {
      try {
        await api("POST", `/api/exchanges/${state.exchange}/cancel-orders`);
        toast("已撤单");
        await tick();
      } catch (err) {
        toast(err.message);
      }
    });
  $("btn-refill").onclick = () =>
    openModal("一键补格", "与看门狗相同：对账后缺补、多撤。现价所在格若会穿价仍跳过。", async () => {
      try {
        await api("POST", `/api/exchanges/${state.exchange}/refill`);
        toast("已补格");
        await tick();
      } catch (err) {
        toast(err.message);
      }
    });
  $("modal-cancel").onclick = () => $("modal").classList.remove("show");
}

document.addEventListener("DOMContentLoaded", async () => {
  bindModes();
  bindCombo();
  bindForms();
  bindActions();
  updateKind();
  updateDerived();
  updateEntryHint();
  tickClock();
  setInterval(tickClock, 1000);
  window.addEventListener("resize", drawChart);
  try {
    await loadBootstrap();
    setConn(true, "已连接");
    await tick();
  } catch (err) {
    setConn(false, "连接失败");
    showBanner(err.message || "无法连接后端。请通过 gridbot 提供的地址打开本页。", "error");
  }
  setInterval(tick, 1000);
});
