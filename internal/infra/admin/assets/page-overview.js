// ── 概览页：顶部统计卡片 + 两个子页（请求监控 / 统计摘要） ──

let ovCurrentTab = 'monitor';
// 请求监控轮询控制：
// ovLive 是用户意图开关（默认开）；手动暂停后任何情况都不自动恢复，只能再点按钮。
let ovLive = true;
// active 与 rt-token 拆成两个 timer：两者停止条件不同——rt 另有"翻页浏览"与"补货暂停"
// 两个独立条件，且暂停期间只做 10 秒一次的低频探测（probe=1，只问状态不拉事件）。
let ovMonitorTimer = null;  // /stats/active 的 3 秒轮询
let ovRtTimer = null;       // /stats/rt-token 的 3 秒轮询
let ovRtProbeTimer = null;  // rT 补货暂停期间的 10 秒低频探测
let ovRtPaused = false;     // 预取池"无收益自停"锁存状态（WarmSnapshot.Paused）
let ovRtPauses = 0;         // 累计暂停次数（展示用）
let histPage = 1;
let histLimit = 20;
// rT 记录流分页：卡片默认只占 5 行，避免把下面的历史请求表推得太长；每页条数可自行调整。
// 窗口一次拉回后本地分页（翻页不额外请求）；翻到第 2 页起暂停自动刷新——滚动窗口下新事件
// 会把当前页内容整体挤走，翻页就失去意义了，回第 1 页即恢复实时。
let rtPage = 1;
let rtLimit = 5;
let rtEvents = [];

// by_node 表的客户端排序状态（默认请求数降序）与当前锁定的节点 URI。
let byNodeSort = { key: 'requests', desc: true };
let byNodePage = 1;
let byNodeLimit = 20;
let ovSubTab = 'nodes'; // 统计摘要子标签：nodes / racing / requests
let nodeDays = 30;      // 节点统计时间窗口（0 = 全部）
let racingDays = 1;
let racingBucket = 'hour';
let ovActiveNodeURI = '';
let ovByNodeRows = [];
let ovNodeRows = [];

// shortNodeAddr 从原始 URI 提取可读的 host:port，用于把"名称"与"具体节点"一起展示。
// 解析失败时返回空串（不阻断渲染）。
function shortNodeAddr(uri) {
  if (!uri) return '';
  try {
    if (uri.startsWith('vmess://')) {
      var b64 = uri.slice(8).split(/[?#]/)[0];
      var b = atob(b64.replace(/-/g, '+').replace(/_/g, '/'));
      var info = JSON.parse(b);
      return (info.add || '') + (info.port ? ':' + info.port : '');
    }
    var u = new URL(uri);
    return u.hostname + (u.port ? ':' + u.port : '');
  } catch (e) {
    return '';
  }
}

function card(label, value, cls, sub) {
  return `<div class="card glass hoverable stat"><div class="label">${label}</div><div class="value ${cls||''}">${value}</div>${sub?`<div class="sub">${sub}</div>`:''}</div>`;
}

async function loadOverview() {
  const [keysD, modelsD, nodesD, statsD, settingsD] = await Promise.all([
    API.keys.list().catch(() => ({keys:[]})),
    API.models.get().catch(() => ({models:[]})),
    API.nodes.list().catch(() => ({nodes:[]})),
    API.stats.get().catch(() => null),
    API.settings.get().catch(() => null),
  ]);
  const keys = (keysD.keys || []).length;
  const models = (modelsD.models || []).length;
  const nodes = (nodesD.nodes || []).length;
  ovActiveNodeURI = (settingsD && settingsD.settings && settingsD.settings.active_node_uri) || '';
  $('#ovCards').innerHTML =
    card('服务状态', '运行中', 'green', 'Gemini REST 原生代理') +
    card('API 密钥', keys, 'gold') +
    card('模型', models, 'blue') +
    card('代理节点', nodes, '');
  renderTodayCards(statsD);
  showOvTab(ovCurrentTab);
}

// renderTodayCards 恒定展示「今日概况」（数据来自 /api/admin/stats 的 today 汇总）。
// 原来这些卡片挤在「统计摘要」里，现上移到概览常显区，摘要页因此得以拆分为子标签。
function renderTodayCards(statsD) {
  const box = $('#ovToday');
  if (!box) return;
  const t = statsD && statsD.today ? statsD.today : null;
  if (!t) {
    box.innerHTML = '<div class="desc">统计不可用</div>';
    return;
  }
  const fmtMs = v => v ? (Number(v) / 1000).toFixed(2) + 's' : '-';
  box.innerHTML =
    card('今日请求', t.requests || 0, 'gold') +
    card('今日成功', t.success || 0, 'green') +
    card('今日失败', t.errors || 0, 'red') +
    card('今日成功率', pct(t.success, t.requests), '') +
    card('输入 Tokens', fmtNum(t.prompt_tokens), 'blue') +
    card('输出 Tokens', fmtNum(t.candidates_tokens), 'blue') +
    card('总 Tokens', fmtNum(t.total_tokens), 'blue') +
    card('平均总耗时', fmtMs(t.avg_total_ms), '') +
    card('平均首字', fmtMs(t.avg_upstream_ttfb_ms), '', '仅统计已出首字');
}

function showOvTab(name) {
  ovCurrentTab = name;
  // 只切换顶层标签：选择器收窄到 .ov-tabs 下的 [data-ovtab]，避免误清子标签（.ov-subtab）的 active。
  document.querySelectorAll('.ov-tabs > .ov-tab[data-ovtab]').forEach(b => b.classList.toggle('active', b.dataset.ovtab === name));
  $('#ovMonitor').classList.toggle('hidden', name !== 'monitor');
  $('#ovSummary').classList.toggle('hidden', name !== 'summary');
  if (name === 'monitor') {
    startMonitor();
  } else {
    stopMonitor();
    initSummaryShell();
    showOvSubTab(ovSubTab);
  }
}

// initSummaryShell 建立「统计摘要」的子标签骨架（仅首次进入时执行一次）。
// 原先今日/累计/趋势/模型/Key/节点/Racing 全堆一页导致信息过载，现拆为三个子标签。
function initSummaryShell() {
  const box = $('#ovSummary');
  if (!box || box.dataset.inited) return;
  box.dataset.inited = '1';
  box.innerHTML = `
    <div class="ov-tabs">
      <button class="btn ov-subtab active" data-ovsubtab="nodes" onclick="showOvSubTab('nodes')">🖧 节点统计</button>
      <button class="btn ov-subtab" data-ovsubtab="racing" onclick="showOvSubTab('racing')">🏁 Racing 统计</button>
      <button class="btn ov-subtab" data-ovsubtab="requests" onclick="showOvSubTab('requests')">📈 请求统计</button>
    </div>
    <div id="ovSubNodes" class="grid"></div>
    <div id="ovSubRacing" class="grid hidden"></div>
    <div id="ovSubRequests" class="grid hidden"></div>`;
}

// showOvSubTab 切换子标签并惰性加载对应面板（只拉取当前标签的数据）。
function showOvSubTab(name) {
  ovSubTab = name;
  document.querySelectorAll('.ov-subtab').forEach(b => b.classList.toggle('active', b.dataset.ovsubtab === name));
  const panels = { nodes: '#ovSubNodes', racing: '#ovSubRacing', requests: '#ovSubRequests' };
  Object.keys(panels).forEach(k => {
    const el = $(panels[k]);
    if (el) el.classList.toggle('hidden', k !== name);
  });
  if (name === 'nodes') loadNodeStats();
  else if (name === 'racing') loadRacingPanel();
  else loadRequestStats();
}

// ── 子页 1：请求监控（在途实时 + 历史分页） ──

// monitorVisible 判断监控面板当前是否可见（概览页 + 请求监控子页都未隐藏）。
// 用 DOM 判断而非读 admin.js 的顶层 curPage（顶层 let 不上 window，跨脚本有时序风险），
// 与 page-logs.js 的既有写法同款。
function monitorVisible() {
  const page = document.getElementById('page-overview');
  const mon = document.getElementById('ovMonitor');
  return !!page && !!mon && !page.classList.contains('hidden') && !mon.classList.contains('hidden');
}

// canPoll 是两个轮询共同的门控：用户开关开 + 监控页可见 + 浏览器标签可见。
function canPoll() {
  return ovLive && monitorVisible() && !document.hidden;
}

// startTimers 按当前条件启动 active/rt 两个轮询（条件不满足则一个都不起）。
// 调用前必先 stopTimers：重复进入/恢复时清掉旧 interval，避免叠加。
function startTimers() {
  stopTimers();
  if (!canPoll()) return;
  ovMonitorTimer = setInterval(loadActiveRequests, 3000);
  startRtTimer();
}

// startRtTimer 单独管理 rt 轮询：翻页浏览（rtPage>1）不起（翻页窗口已冻结）；
// 补货暂停中不起事件轮询，改为 10 秒一次的 probe，等 paused 翻回 false 立即恢复实时。
function startRtTimer() {
  if (ovRtTimer) { clearInterval(ovRtTimer); ovRtTimer = null; }
  if (ovRtProbeTimer) { clearInterval(ovRtProbeTimer); ovRtProbeTimer = null; }
  if (!canPoll()) return;
  if (ovRtPaused) {
    ovRtProbeTimer = setInterval(probeRTPaused, 10000);
    return;
  }
  if (rtPage > 1) return;
  ovRtTimer = setInterval(loadRTTokens, 3000);
}

// stopTimers 清掉全部三个 timer（stopMonitor 是对外稳定名，admin.js 切页/登出都会调）。
function stopTimers() {
  if (ovMonitorTimer) { clearInterval(ovMonitorTimer); ovMonitorTimer = null; }
  if (ovRtTimer) { clearInterval(ovRtTimer); ovRtTimer = null; }
  if (ovRtProbeTimer) { clearInterval(ovRtProbeTimer); ovRtProbeTimer = null; }
}

function stopMonitor() {
  stopTimers();
}

// toggleMonitorLive 切换实时刷新开关：暂停立即停全部轮询；重新开启立即补拉一次再起 timer。
function toggleMonitorLive() {
  ovLive = !ovLive;
  updateLiveButton();
  if (ovLive) {
    loadActiveRequests();
    if (rtPage === 1 && !ovRtPaused) loadRTTokens();
    startTimers();
  } else {
    stopTimers();
  }
}

// updateLiveButton 同步监控工具条的按钮文案与状态行。
function updateLiveButton() {
  const btn = document.getElementById('ovLiveBtn');
  if (btn) {
    btn.textContent = ovLive ? '⏸ 暂停刷新' : '▶ 开始刷新';
    btn.classList.toggle('active', ovLive);
  }
  const st = document.getElementById('ovLiveState');
  if (st) st.textContent = ovLive ? '实时刷新中 · 每 3 秒' : '刷新已暂停';
}

// 浏览器标签隐藏/恢复：隐藏即停全部轮询（避免后台空转刷接口）；
// 恢复时按当前开关与页面状态重启——用户手动暂停的不受影响（ovLive 仍为 false）。
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    stopTimers();
  } else if (canPoll()) {
    startTimers();
    loadActiveRequests(); // 回到前台立即补一次快照，不必等下一个 3 秒
    if (rtPage === 1 && !ovRtPaused) loadRTTokens();
  }
});

function startMonitor() {
  initMonitorShell();
  rtPage = 1; // 每次进入该标签都回到第 1 页：实时视图优先，翻页状态不跨标签保留
  loadActiveRequests();
  loadRTTokens();
  loadHistoryRecords();
  startTimers();
  updateLiveButton();
}

// initMonitorShell 建立监控页容器骨架（仅在首次进入时执行一次），
// 之后 loadActiveRequests 只更新 #ovActive，避免每 3 秒清空历史表。
function initMonitorShell() {
  const box = $('#ovMonitor');
  if (!box || box.dataset.inited) return;
  box.dataset.inited = '1';
  box.innerHTML = `
    <div class="req-toolbar" style="margin-bottom:10px;">
      <div class="field-heading" style="margin:0;">请求监控 <span class="label-gold" id="ovLiveState"></span></div>
      <button class="btn ghost" id="ovLiveBtn" onclick="toggleMonitorLive()" style="padding:4px 14px;"></button>
    </div>
    <div id="ovActive"></div>
    <div id="ovRTTokens"></div>
    <div class="card glass" id="histCard">
      <div class="req-toolbar">
        <div class="field-heading" style="margin:0;">历史请求 <span class="label-gold">(时间倒序)</span></div>
        <div style="display:flex;align-items:center;gap:10px;">
          <span class="desc" style="white-space:nowrap;">每页</span>
          <select onchange="changeHistLimit(this.value)" style="width:auto;margin:0;">
            ${[20,50,100].map(n => `<option value="${n}" ${n===histLimit?'selected':''}>${n}</option>`).join('')}
          </select>
          <span class="desc" style="white-space:nowrap;">条</span>
          <span class="desc" style="white-space:nowrap;" id="histTotal"></span>
          <button class="btn ghost" onclick="goHistPage(histPage-1)" style="padding:4px 10px;">上一页</button>
          <span class="desc" id="histPageInfo"></span>
          <button class="btn ghost" onclick="goHistPage(histPage+1)" style="padding:4px 10px;">下一页</button>
        </div>
      </div>
      <div id="histTable"></div>
    </div>`;
  updateLiveButton();
}

// modelCell 渲染模型列：请求的原始模型名（含变体前缀）与实际发往上游的模型不同时才并列，
// 相同则只显示一个，避免表格被无意义重复占满。
function modelCell(requested, actual) {
  const req = requested || '';
  const act = actual || '-';
  if (!req || req === act) return esc(act);
  return `${esc(req)}<div class="req-model-actual">→ ${esc(act)}</div>`;
}

// responseModeCell 渲染"请求回应"列：请求形态 → 实际接收形态。
// 上游侧恒为流式，故只有客户端侧存在该维度；流式端点被聚合时可直接读出降级。
function responseModeCell(streaming, mode) {
  const req = streaming ? '流式' : '非流式';
  const badgeCls = streaming ? 's' : 'n';
  const badge = `<span class="req-stream-badge ${badgeCls}">${req}</span>`;
  const m = mode || '';
  if (!m || (m === '增量' && streaming) || (m === '单次' && !streaming)) return badge;
  return `${badge}→<span class="req-mode-tag">${esc(m)}</span>`;
}

// ttfbCell 渲染首字耗时：只展示上游首字节耗时。
// 不展示客户端首帧：非流式/聚合流下客户端首帧与总耗时相同，并列只会造成冗余。
// 老数据可能没有上游值（列为后补），此时回退客户端值以免显示为空。
function ttfbCell(upstreamMs, clientMs) {
  const up = upstreamMs ? (upstreamMs / 1000).toFixed(2) + 's' : '';
  if (up) return esc(up);
  return clientMs ? esc((clientMs / 1000).toFixed(2) + 's') : '-';
}

// antiTruncateCell 渲染"抗截断"列：是否走 StreamChatCollect 竞速链路。
function antiTruncateCell(on) {
  return on ? '<span class="req-anti-tag">抗截断</span>' : '<span class="desc">-</span>';
}

// integrityLabel 把响应完整性根因分类映射为可读标注（与后端 stream_integrity.go 枚举同口径）。
function integrityLabel(outcome) {
  const map = {
    complete: '完整',
    length: '长度受限',
    content_filter: '内容拦截',
    tool_calls: '工具调用',
    empty: '空回复',
    incomplete: '未收尾'
  };
  return map[outcome] || '';
}

// truncationLabel 把上游终止原因映射为截断标注；空串表示非截断（正常收尾）。
// 与后端 transform.TruncationLabel 保持同一口径（前端无法复用 Go 实现）。
function truncationLabel(finishReason) {
  const fr = (finishReason || '').trim().toUpperCase();
  if (fr === 'MAX_TOKENS') return '输出上限截断';
  if (fr === 'MALFORMED_FUNCTION_CALL') return '工具调用畸形';
  return '';
}

// statusCell 渲染历史记录状态列：成功时若上游主动异常终止则追加标注，
// 例如「成功(输出上限截断)」。只做标注、不改判成败——撞输出上限仍是 HTTP 成功；
// 失败时状态列已有失败码与原因，不再叠后缀以免噪音。
function statusCell(r) {
  const label = r.success ? truncationLabel(r.finish_reason) : '';
  const text = r.success ? '成功' + (label ? '(' + label + ')' : '') : '失败 (' + r.status_code + ')';
  const title = [r.error_reason || '', r.finish_reason || ''].filter(Boolean).join(' | ');
  return { text, title };
}

async function loadActiveRequests() {
  const box = $('#ovActive');
  if (!box) return;
  let data;
  try {
    data = await API.stats.active();
  } catch (e) {
    return;
  }
  const list = (data && data.active) || [];
  const rows = list.map(a => {
    const statusCls = a.success === null ? 'req-status-pending' : (a.success ? 'req-status-ok' : 'req-status-err');
    const statusTxt = a.success === null ? '处理中' : (a.success ? '成功' : '失败');
    return `<tr class="req-live-row">
      <td><button class="btn ghost" style="padding:2px 8px;font-size:12px;color:var(--red,#e06c75);white-space:nowrap;" onclick="stopActiveRequest('${esc(a.req_id)}', this)">强制停止</button></td>
      <td title="${esc(a.req_id)}">${esc((a.req_id||'').slice(0,12))}</td>
      <td>${modelCell(a.raw_model, a.model)}</td>
      <td>${esc(a.useid || a.api_key_name || '-')}</td>
      <td>${esc(a.node_name || '-')}</td>
      <td>${responseModeCell(a.streaming, a.response_mode)}</td>
      <td>${antiTruncateCell(a.anti_truncate)}</td>
      <td>${fmtNum(a.prompt_tokens)} / ${fmtNum(a.candidates_tokens)}</td>
      <td>${ttfbCell(a.upstream_ttfb_ms, a.ttfb_ms)}</td>
      <td>${(a.elapsed_ms/1000).toFixed(1)}s</td>
      <td class="${statusCls}">${statusTxt}</td>
    </tr>`;
  }).join('');

  box.innerHTML = `<div class="card glass">
    <div class="field-heading">当前在途请求 <span class="label-gold">(${list.length})</span> <span class="desc">${ovLive ? '每 3 秒自动刷新' : '刷新已暂停'}</span></div>
    ${list.length ? `<div class="table-wrap"><table class="stats-table"><thead><tr>
      <th>操作</th><th>请求 ID</th><th>模型</th><th>用户</th><th>节点</th><th>请求回应</th><th>抗截断</th><th>输入/输出</th><th>首字</th><th>已用</th><th>状态</th>
    </tr></thead><tbody>${rows}</tbody></table></div>` : '<div class="desc">暂无进行中的请求</div>'}
  </div>`;
}

// stopActiveRequest 强制停止一条在途请求：取消其上下文 → 上游流/竞速候选当场中断，
// 客户端收到明确的 503（"请求已被管理员强制停止"）。停止后立即刷新列表，行随即消失。
async function stopActiveRequest(reqId, btn) {
  if (!reqId) return;
  if (btn) { btn.disabled = true; btn.textContent = '停止中'; }
  try {
    const res = await API.stats.stopRequest(reqId);
    toast(res && res.stopped ? '已强制停止请求 ' + String(reqId).slice(0, 12) : '该请求已结束');
    loadActiveRequests();
  } catch (e) {
    toast('停止失败: ' + e.message);
    if (btn) { btn.disabled = false; btn.textContent = '强制停止'; }
  }
}

// rtTokenWindow 是一次拉回的记录条数上限（引擎侧环形缓冲为 100，取满即可本地分页）。
const rtTokenWindow = 100;

// loadRTTokens 拉取 rT token 抓取记录流（请求路径 + 后台预取）并渲染当前页。
// 翻到第 2 页起只重绘已冻结的窗口，不再拉取（见文件顶部 rtPage 注释）。
async function loadRTTokens() {
  const box = $('#ovRTTokens');
  if (!box) return;
  if (rtPage > 1) {
    renderRTTokens();
    return;
  }
  try {
    const data = await API.stats.rtTokens(rtTokenWindow);
    rtEvents = (data && data.events) || [];
    // 暂停状态随响应带回：翻转时立即按新状态重配 rt 轮询（暂停→probe，恢复→实时）。
    const wasPaused = ovRtPaused;
    ovRtPaused = !!(data && data.paused);
    ovRtPauses = (data && data.pauses) || 0;
    if (wasPaused !== ovRtPaused) startRtTimer();
  } catch (e) {
    return;
  }
  renderRTTokens();
}

// probeRTPaused 是补货暂停期间的低频探测：只问 paused 状态、不拉事件表。
// 预取池因"无收益"自停后确实没有补货活动可看；新请求到达会解除自停锁存，
// 探测到恢复立即回到 3 秒实时刷新并补拉一次。
async function probeRTPaused() {
  let data;
  try {
    data = await API.stats.rtProbe();
  } catch (e) {
    return;
  }
  if (data && !data.paused && ovRtPaused) {
    ovRtPaused = false;
    startRtTimer();
    loadRTTokens();
  }
}

// renderRTTokens 渲染 rT 记录流的当前页（本地分页，翻页/改每页条数都不额外请求）。
function renderRTTokens() {
  const box = $('#ovRTTokens');
  if (!box) return;
  const total = rtEvents.length;
  const totalPages = Math.max(1, Math.ceil(total / rtLimit));
  if (rtPage > totalPages) rtPage = totalPages;
  const start = (rtPage - 1) * rtLimit;
  const pageEvents = rtEvents.slice(start, start + rtLimit);
  const rows = pageEvents.map(e => {
    const t = e.at ? new Date(e.at).toLocaleTimeString('zh-CN', { hour12: false }) : '-';
    const errTxt = e.err ? String(e.err) : '';
    const result = e.canceled ? '⊘ 已取消' : (e.ok ? '✓' : '✗ ' + esc(errTxt.slice(0, 60)));
    return `<tr>
      <td>${esc(t)}</td>
      <td>${esc(e.source || '-')}</td>
      <td title="${esc(e.node_uri || '')}">${esc(e.node_name || '-')}</td>
      <td title="选点时刻的 rT 降权层：0 健康 / 1 对 rT 差或慢 / 2 rT 冷却中。降权节点只会在好节点不足时被尝试">${e.rt_rank || 0}</td>
      <td>${fmtMs(e.latency_ms)}</td>
      <td class="${e.ok ? 'req-status-ok' : (e.canceled ? 'desc' : 'req-status-err')}" title="${esc(errTxt)}">${result}</td>
    </tr>`;
  }).join('');
  const liveHint = ovRtPaused
    ? `⏸ rT 补货已暂停(第 ${ovRtPauses} 次)，刷新已停，恢复后自动继续`
    : (rtPage === 1 ? '每 3 秒刷新' : '翻页浏览中已暂停刷新，回到第 1 页恢复');

  box.innerHTML = `<div class="card glass">
    <div class="req-toolbar">
      <div class="field-heading" style="margin:0;">♻️ RT Token 实时记录 <span class="label-gold">(${total})</span> <span class="desc">${liveHint}；含请求路径与后台预取</span></div>
      <div style="display:flex;align-items:center;gap:10px;">
        <span class="desc" style="white-space:nowrap;">每页</span>
        <select onchange="changeRTLimit(this.value)" style="width:auto;margin:0;">
          ${[5, 10, 20, 50].map(n => `<option value="${n}" ${n === rtLimit ? 'selected' : ''}>${n}</option>`).join('')}
        </select>
        <span class="desc" style="white-space:nowrap;">条</span>
        <button class="btn ghost" onclick="goRTPage(rtPage-1)" style="padding:4px 10px;">上一页</button>
        <span class="desc">${rtPage} / ${totalPages}</span>
        <button class="btn ghost" onclick="goRTPage(rtPage+1)" style="padding:4px 10px;">下一页</button>
      </div>
    </div>
    ${total ? `<div class="table-wrap"><table class="stats-table"><thead><tr>
      <th>时间</th><th>来源</th><th>节点</th><th>rT档</th><th>耗时</th><th>结果</th>
    </tr></thead><tbody>${rows}</tbody></table></div>` : '<div class="desc">暂无 rT Token 抓取记录</div>'}
  </div>`;
}

// changeRTLimit 调整每页条数并回到第 1 页（回到第 1 页即恢复实时刷新）。
function changeRTLimit(v) {
  rtLimit = Number(v) || 5;
  rtPage = 1;
  loadRTTokens();
  startRtTimer(); // 翻页状态已复位：按当前条件重配 rt 轮询（起实时或起 probe）
}

// goRTPage 翻页：回第 1 页恢复实时刷新，其余页渲染已冻结的窗口。
function goRTPage(p) {
  if (p < 1) return;
  rtPage = p;
  startRtTimer(); // 离开第 1 页停事件轮询，回到第 1 页按条件恢复（暂停中则维持 probe）
  if (p === 1) {
    loadRTTokens();
    return;
  }
  renderRTTokens();
}

async function loadHistoryRecords() {
  const box = $('#histTable');
  if (!box) return;
  let data;
  try {
    data = await API.stats.records('page=' + histPage + '&limit=' + histLimit);
  } catch (e) {
    box.innerHTML = '<div class="desc">历史记录加载失败: ' + esc(e.message) + '</div>';
    return;
  }
  const items = (data && data.items) || [];
  const total = (data && data.total) || 0;
  const totalPages = Math.max(1, Math.ceil(total / histLimit));
  if (histPage > totalPages) histPage = totalPages;
  $('#histTotal').textContent = '共 ' + total + ' 条';
  $('#histPageInfo').textContent = histPage + ' / ' + totalPages;

  if (!items.length) {
    box.innerHTML = '<div class="desc">暂无记录</div>';
    return;
  }
  const rows = items.map(r => {
    const statusCls = r.success ? 'req-status-ok' : 'req-status-err';
    const st = statusCell(r);
    const d = new Date(r.ts * 1000);
    const time = `${d.getFullYear()}-${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')} ${String(d.getHours()).padStart(2,'0')}:${String(d.getMinutes()).padStart(2,'0')}:${String(d.getSeconds()).padStart(2,'0')}`;
    return `<tr>
      <td>${time}</td>
      <td title="${esc(r.req_id)}">${esc((r.req_id||'').slice(0,12))}</td>
      <td>${modelCell(r.requested_model, r.model)}</td>
      <td>${esc(r.useid || r.api_key_name || '-')}</td>
      <td class="ov-cell-name" title="${esc(r.node_uri || '')}">${esc(r.node_name || '-')}${r.node_uri ? '<div style="font-size:11px;color:var(--text-dim);">' + esc(shortNodeAddr(r.node_uri)) + '</div>' : ''}</td>
      <td>${responseModeCell(r.streaming, r.response_mode)}</td>
      <td>${antiTruncateCell(r.anti_truncate)}${r.anti_truncate_transport ? '<div style="font-size:11px;color:var(--text-dim);" title="正文经合成工具参数传输">工具传输</div>' : ''}</td>
      <td>${fmtNum(r.prompt_tokens)} / ${fmtNum(r.candidates_tokens)}</td>
      <td>${ttfbCell(r.upstream_ttfb_ms, r.ttfb_ms)}</td>
      <td>${r.total_ms ? (r.total_ms/1000).toFixed(2)+'s' : '-'}</td>
      <td class="${statusCls}" title="${esc(st.title)}">${st.text}${integrityCell(r.integrity)}</td>
    </tr>`;
  }).join('');
  box.innerHTML = `<div class="table-wrap"><table class="stats-table"><thead><tr>
      <th>时间</th><th>请求 ID</th><th>模型</th><th>用户</th><th>节点</th><th>请求回应</th><th>抗截断</th><th>输入/输出</th><th>首字</th><th>总耗时</th><th>状态</th>
    </tr></thead><tbody>${rows}</tbody></table></div>`;
}

// integrityCell 把完整性分类渲染为状态列的附加小字标注（完整时不显示，避免噪音）。
function integrityCell(integrity) {
  const label = integrityLabel(integrity);
  if (!label || label === '完整') return '';
  return '<div style="font-size:11px;color:var(--text-dim);">' + esc(label) + '</div>';
}

function changeHistLimit(v) {
  histLimit = Number(v);
  histPage = 1;
  loadHistoryRecords();
}

function goHistPage(p) {
  if (p < 1) return;
  histPage = p;
  loadHistoryRecords();
}

// ── 子页 2：统计摘要（今日/累计/趋势/分组） ──

let trendDays = 14;

// loadRequestStats 渲染「请求统计」子标签：累计汇总 + 请求趋势 + 按模型 / 按 API Key。
async function loadRequestStats() {
  const box = $('#ovSubRequests');
  if (!box) return;
  let d;
  try {
    d = await API.stats.get();
  } catch (e) {
    box.innerHTML = `<div class="card glass"><div class="field-heading">请求统计</div><div class="desc">统计加载失败: ${esc(e.message)}</div></div>`;
    return;
  }
  const all = d.all || {};
  const fmtMs = v => v ? (Number(v) / 1000).toFixed(2) + 's' : '-';
  const summary = `
    <div class="card glass">
      <div class="field-heading">累计 <span class="label-gold">全部</span></div>
      <div class="grid stats" style="grid-template-columns:repeat(auto-fill,minmax(160px,1fr));margin-top:10px;">
        ${card('请求', all.requests || 0, 'gold')}
        ${card('成功率', pct(all.success, all.requests), '')}
        ${card('Prompt Tokens', fmtNum(all.prompt_tokens), 'blue')}
        ${card('Candidates Tokens', fmtNum(all.candidates_tokens), 'blue')}
        ${card('Total Tokens', fmtNum(all.total_tokens), 'blue')}
        ${card('平均总耗时', fmtMs(all.avg_total_ms), '')}
        ${card('平均首字', fmtMs(all.avg_upstream_ttfb_ms), '', '仅统计已出首字')}
        ${card('平均输出', fmtMs(all.avg_output_ms), '')}
      </div>
      <div class="toolbar m-0" style="margin-top:16px;justify-content:flex-end;">
        <button class="btn danger" onclick="resetStats()">清空统计</button>
      </div>
    </div>`;
  box.innerHTML = `${summary}
    <div id="ovTrend"></div>
    <div class="grid grid-2">
      ${groupTable('按模型 (近30天)', d.by_model)}
      ${groupTable('按 API Key (近30天)', d.by_key)}
    </div>`;
  loadTrendChart();
}

// groupTable 渲染按维度分组的统计表（模型 / API Key 共用）。
function groupTable(title, rows) {
  if (!rows || !rows.length) {
    return `<div class="card glass"><div class="field-heading">${title}</div><div class="desc">暂无数据</div></div>`;
  }
  const body = rows.map(r => `<tr>
      <td class="ov-cell-name" title="${esc(r.name)}">${esc(r.name)}</td>
      <td>${r.requests}</td>
      <td class="green">${r.success}</td>
      <td class="red">${r.errors}</td>
      <td>${fmtNum(r.total_tokens)}</td>
      <td>${pct(r.success, r.requests)}</td>
    </tr>`).join('');
  return `<div class="card glass">
    <div class="field-heading">${title}</div>
    <div class="table-wrap"><table class="stats-table"><thead><tr>
      <th>名称</th><th>请求</th><th>成功</th><th>失败</th><th>Tokens</th><th>成功率</th>
    </tr></thead><tbody>${body}</tbody></table></div>
  </div>`;
}

// ── 按代理节点表：客户端排序 + 锁定使用 ──

function nodeSortVal(r, key) {
  if (key === 'success_rate') {
    return r.requests ? (r.success / r.requests) : 0;
  }
  return Number(r[key] || 0);
}

function nodeSortedRows() {
  const rows = (ovByNodeRows || []).slice();
  const { key, desc } = byNodeSort;
  rows.sort((a, b) => {
    const va = nodeSortVal(a, key);
    const vb = nodeSortVal(b, key);
    if (va !== vb) return desc ? vb - va : va - vb;
    // 并列时按请求数降序，保证排序稳定可预期。
    return (b.requests || 0) - (a.requests || 0);
  });
  return rows;
}

function sortByNode(key) {
  if (byNodeSort.key === key) {
    byNodeSort.desc = !byNodeSort.desc;
  } else {
    byNodeSort.key = key;
    byNodeSort.desc = true;
  }
  byNodePage = 1;
  renderNodeTable();
}

// 节点统计时间窗口：0 表示全部（下界取保留期）。
const NODE_WINDOW_LABEL = { 0: '全部', 1: '近1天', 3: '近3天', 7: '近7天', 15: '近15天', 30: '近30天' };

// loadNodeStats 拉取指定时间窗口的按节点聚合统计，并交给 renderNodeTable 渲染。
async function loadNodeStats() {
  const box = $('#ovSubNodes');
  if (!box) return;
  let d;
  try {
    d = await API.stats.nodes(nodeDays);
  } catch (e) {
    box.innerHTML = `<div class="card glass"><div class="field-heading">节点统计</div><div class="desc">加载失败: ${esc(e.message)}</div></div>`;
    return;
  }
  ovByNodeRows = d.by_node || [];
  byNodePage = 1;
  renderNodeTable();
}

function setNodeDays(n) {
  nodeDays = n;
  byNodePage = 1;
  loadNodeStats();
}

function renderNodeTable() {
  const box = $('#ovSubNodes');
  if (!box) return;
  const rows = nodeSortedRows();
  ovNodeRows = rows;
  const winLabel = NODE_WINDOW_LABEL[nodeDays] || ('近' + nodeDays + '天');
  const winSel = [1, 3, 7, 15, 30, 0]
    .map(n => `<button class="btn ghost ${n === nodeDays ? 'ov-tab active' : ''}" style="padding:4px 10px;" onclick="setNodeDays(${n})">${NODE_WINDOW_LABEL[n]}</button>`)
    .join('');
  const head = `<div class="req-toolbar">
      <div class="field-heading" style="margin:0;">节点统计 <span class="label-gold">${winLabel}</span> <span class="desc">点击列头排序</span></div>
      <div style="display:flex;align-items:center;gap:6px;flex-wrap:wrap;">${winSel}</div>
    </div>`;
  if (!rows.length) {
    box.innerHTML = `<div class="card glass">${head}<div class="desc">暂无数据</div></div>`;
    return;
  }
  const totalPages = Math.max(1, Math.ceil(rows.length / byNodeLimit));
  if (byNodePage > totalPages) byNodePage = totalPages;
  const start = (byNodePage - 1) * byNodeLimit;
  const pageRows = rows.slice(start, start + byNodeLimit);
  const arrow = (k) => byNodeSort.key === k ? (byNodeSort.desc ? ' ↓' : ' ↑') : '';
  const th = (label, k) => `<th class="sortable" style="cursor:pointer;user-select:none;" onclick="sortByNode('${k}')" title="点击排序">${label}${arrow(k)}</th>`;
  const body = pageRows.map((r, i) => {
    const idx = start + i;
    const name = r.name ? esc(r.name) : '<span class="desc">未归属</span>';
    const addr = shortNodeAddr(r.raw_uri);
    const addrCell = addr
      ? `<code style="font-size:11px;background:rgba(0,0,0,0.25);color:var(--blue);padding:1px 4px;border-radius:4px;">${esc(addr)}</code>`
      : '<span class="desc">-</span>';
    const canAct = !!r.raw_uri;
    const disabledAttr = canAct ? '' : 'disabled title="无法定位节点 URI"';
    const dimStyle = canAct ? '' : 'color:var(--text-dim);';
    const useBtn = (canAct && r.raw_uri === ovActiveNodeURI)
      ? `<button class="btn ghost" style="padding:4px 8px;font-size:12px;color:var(--gold);" onclick="ovToggleNodeLock(${idx}, true)">取消锁定</button>`
      : `<button class="btn ghost" style="padding:4px 8px;font-size:12px;${dimStyle}" ${disabledAttr} onclick="ovToggleNodeLock(${idx}, false)">锁定使用</button>`;
    const lockBtn = r.locked
      ? `<button class="btn ghost" style="padding:4px 8px;font-size:12px;color:var(--gold);" ${canAct ? '' : 'disabled'} onclick="ovToggleNodeLocked(${idx}, true)">解除防删</button>`
      : `<button class="btn ghost" style="padding:4px 8px;font-size:12px;${dimStyle}" ${disabledAttr} onclick="ovToggleNodeLocked(${idx}, false)">防删</button>`;
    // 防删锁定的节点豁免手动/批量禁用（后端语义），故禁用按钮在已锁定且已禁用时不提供"启用"之外的操作。
    // 注意：健康触发的自动禁用不受防删保护——节点实际连不通时后端会自行禁用，需手动启用恢复。
    const enBtn = r.disabled
      ? `<button class="btn ghost" style="padding:4px 8px;font-size:12px;color:var(--green);" ${canAct ? '' : 'disabled'} onclick="ovToggleNodeDisabled(${idx}, true)">启用</button>`
      : `<button class="btn ghost" style="padding:4px 8px;font-size:12px;${dimStyle}" ${disabledAttr} onclick="ovToggleNodeDisabled(${idx}, false)">禁用</button>`;
    const badges = (r.locked ? ' <span style="color:var(--gold);font-size:11px;" title="已防删">🔒</span>' : '') +
      (r.disabled ? ' <span class="desc" style="font-size:11px;">(已禁用)</span>' : '');
    return `<tr>
      <td class="ov-cell-name" title="${esc(r.raw_uri || r.name || '')}">${name}${badges}</td>
      <td>${addrCell}</td>
      <td>${r.requests}</td>
      <td class="green">${r.success}</td>
      <td class="red">${r.errors}</td>
      <td>${fmtNum(r.prompt_tokens)}</td>
      <td>${fmtNum(r.candidates_tokens)}</td>
      <td>${fmtNum(r.total_tokens)}</td>
      <td>${pct(r.success, r.requests)}</td>
      <td style="white-space:nowrap;">${useBtn} ${lockBtn} ${enBtn}</td>
    </tr>`;
  }).join('');
  box.innerHTML = `<div class="card glass">
    ${head}
    <div class="req-toolbar" style="justify-content:flex-end;">
      <span class="desc" style="white-space:nowrap;">每页</span>
      <select onchange="changeByNodeLimit(this.value)" style="width:auto;margin:0;">
        ${[10,20,50,100].map(n => `<option value="${n}" ${n===byNodeLimit?'selected':''}>${n}</option>`).join('')}
      </select>
      <span class="desc" style="white-space:nowrap;">条 · 共 ${rows.length} 个节点</span>
      <button class="btn ghost" onclick="goByNodePage(byNodePage-1)" style="padding:4px 10px;">上一页</button>
      <span class="desc">${byNodePage} / ${totalPages}</span>
      <button class="btn ghost" onclick="goByNodePage(byNodePage+1)" style="padding:4px 10px;">下一页</button>
    </div>
    <div class="table-wrap"><table class="stats-table"><thead><tr>
      <th>名称</th><th>节点地址</th>${th('请求','requests')}${th('成功','success')}${th('失败','errors')}<th>输入</th><th>输出</th><th>Tokens</th>${th('成功率','success_rate')}<th>操作</th>
    </tr></thead><tbody>${body}</tbody></table></div>
  </div>`;
}

// ovToggleNodeLocked 在概览「节点统计」表内快速添加/解除单个节点的防删锁。
// 与「锁定使用」不同：防删只保护节点不被删除/禁用，不改变路由与并发池。
async function ovToggleNodeLocked(idx, locked) {
  const row = (ovNodeRows || [])[idx];
  if (!row || !row.raw_uri) return;
  try {
    await API.nodes.lock([row.raw_uri], !locked);
    toast(!locked ? '已添加防删锁' : '已解除防删锁');
    row.locked = !locked; // 行对象与 ovByNodeRows 共享引用，就地更新后重渲染即可
    renderNodeTable();
  } catch (e) {
    toast('操作失败: ' + e.message);
  }
}

// ovToggleNodeDisabled 在概览「节点统计」表内快速禁用/启用单个节点，
// 便于直接摘掉多次不成功的节点（防删锁定的节点豁免手动/批量禁用；健康触发的自动禁用不豁免）。
async function ovToggleNodeDisabled(idx, disabled) {
  const row = (ovNodeRows || [])[idx];
  if (!row || !row.raw_uri) return;
  try {
    if (disabled) {
      await API.nodes.batchEnable([row.raw_uri]);
      toast('已启用该节点');
    } else {
      await API.nodes.batchDisable([row.raw_uri]);
      toast('已禁用该节点');
    }
    row.disabled = !disabled; // 行对象与 ovByNodeRows 共享引用，就地更新后重渲染即可
    renderNodeTable();
  } catch (e) {
    toast('操作失败: ' + e.message);
  }
}

function changeByNodeLimit(v) {
  byNodeLimit = Number(v);
  byNodePage = 1;
  renderNodeTable();
}

function goByNodePage(p) {
  if (p < 1) return;
  byNodePage = p;
  renderNodeTable();
}

async function ovToggleNodeLock(idx, locked) {
  const row = (ovNodeRows || [])[idx];
  if (!row) return;
  try {
    if (locked) {
      await API.useNode('');
      toast('已取消锁定，并恢复并发池');
    } else {
      await API.useNode(row.raw_uri);
      toast('已锁定使用该节点，并关闭并发池');
    }
    await loadOverview();
  } catch (e) {
    toast('操作失败: ' + e.message);
  }
}

// loadTrendChart 拉取指定天数的时间序列并渲染趋势图（含每个时间段成功率）。
async function loadTrendChart() {
  const box = $('#ovTrend');
  if (!box) return;
  let data;
  try {
    data = await API.stats.history('days=' + trendDays + '&bucket=day');
  } catch (e) {
    box.innerHTML = `<div class="card glass"><div class="field-heading">趋势</div><div class="desc">加载失败: ${esc(e.message)}</div></div>`;
    return;
  }
  const pts = (data && data.series) || [];
  const chart = renderTrendChart(pts);
  box.innerHTML = `<div class="card glass">
    <div class="req-toolbar" style="margin-bottom:4px;">
      <div class="field-heading" style="margin:0;">请求趋势 <span class="label-gold">(蓝=请求 / 青=Tokens)</span></div>
      <div style="display:flex;align-items:center;gap:8px;">
        ${[7,14,30,90].map(n => `<button class="btn ghost ${n===trendDays?'ov-tab active':''}" style="padding:4px 10px;" onclick="setTrendDays(${n})">${n}天</button>`).join('')}
      </div>
    </div>
    ${chart}
  </div>`;
}

function setTrendDays(n) {
  trendDays = n;
  loadTrendChart();
}

// fmtMs 渲染毫秒耗时：>=1s 转成秒（两位小数），否则原样毫秒。
// 不能复用 fmtNum——那是给 token 数量写的紧凑格式（1.2k/3.4M），套到耗时上会显示成 "1.2kms"。
function fmtMs(ms) {
  const n = Number(ms || 0);
  if (n >= 1000) return (n / 1000).toFixed(2) + 's';
  return n + 'ms';
}

function fmtNum(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

function pct(a, b) {
  if (!b) return '0%';
  return Math.round((a / b) * 100) + '%';
}

function renderTrendChart(history) {
  const pts = (history || []);
  if (!pts.length) return `<div class="desc">暂无趋势数据</div>`;
  const maxReq = Math.max(1, ...pts.map(p => p.requests));
  const maxTok = Math.max(1, ...pts.map(p => p.total_tokens));
  const bars = pts.map(p => {
    const d = new Date(p.ts * 1000);
    const label = `${d.getMonth()+1}/${d.getDate()}`;
    const hReq = Math.round((p.requests / maxReq) * 80) + 2;
    const hTok = Math.round((p.total_tokens / maxTok) * 80) + 2;
    const rate = pct(p.success, p.requests);
    const rateCls = p.requests ? (p.success / p.requests >= 0.9 ? 'req-status-ok' : (p.success / p.requests >= 0.5 ? '' : 'req-status-err')) : '';
    return `<div class="trend-col" title="${label}: ${p.requests} 请求 / ${fmtNum(p.total_tokens)} tokens (成功 ${p.success}, 失败 ${p.errors}, 成功率 ${rate})">
      <div class="trend-rate ${rateCls}">${rate}</div>
      <div class="bar-wrap"><div class="bar bar-tok" style="height:${hTok}%"></div><div class="bar bar-req" style="height:${hReq}%"></div></div>
      <div class="trend-label">${label}</div>
    </div>`;
  }).join('');
  return `<div class="trend-chart">${bars}</div>
    <div class="desc" style="margin-top:8px;">每列上方为当日成功率，颜色随成功率变化（绿≥90%、黄 50–89%、红&lt;50%）。</div>`;
}

async function resetStats() {
  showConfirm('确定清空全部统计记录吗？此操作不可恢复。', async () => {
    try {
      await API.stats.reset();
      toast('已清空统计');
      loadOverview();
    } catch (e) {
      toast('清空失败: ' + e.message);
    }
  });
}

// ── Racing 统计面板：按节点胜率 + 时间区间胜率 ──
// 口径：一次批量竞速只有胜出者算成功，窗口内其余候选（未胜出/超时/失败）一律视为不成功，
// 因此竞速成功率 = 胜出 / 候选。

let racingData = null;
let raceSort = { key: 'attempts', desc: true };
// Racing 节点表分页：节点多时全量渲染会明显卡顿，故与节点统计同样分页。
let racePage = 1;
let raceLimit = 20;
// 隐藏开关：滤掉已受保护（防删）与已停用（禁用）的节点，便于聚焦待处理的低质量节点。
let raceHideLocked = false;
let raceHideDisabled = false;

async function loadRacingPanel() {
  const box = $('#ovSubRacing');
  if (!box) return;
  try {
    racingData = await API.stats.racing(racingDays, racingBucket);
  } catch (e) {
    racingData = null;
    box.innerHTML = `<div class="card glass"><div class="field-heading">Racing 统计</div><div class="desc">加载失败: ${esc(e.message)}</div></div>`;
    return;
  }
  renderRacingPanel();
}

// renderRacingPanel 从缓存的 racingData 渲染；排序只重渲染，不再重新请求。
function renderRacingPanel() {
  const box = $('#ovSubRacing');
  if (!box || !racingData) return;
  const d = racingData;
  const s = d.summary || {};
  const attempts = s.attempts || 0;
  const wins = s.wins || 0;
  const notWon = (s.lost || 0) + (s.collects || 0);
  const rateLimits = s.rate_limits || 0;
  // 成功率分母扣除 429 限流：429 是上游正常竞争限流而非节点失败。
  const effAttempts = attempts - rateLimits;
  const rate = effAttempts > 0 ? racePct(wins, effAttempts) : '0%';
  const cards = `
    <div class="grid stats" style="grid-template-columns:repeat(auto-fill,minmax(150px,1fr));margin-top:10px;">
      ${card('候选总次数', attempts, 'gold')}
      ${card('胜出', wins, 'green')}
      ${card('未胜出', notWon, 'blue')}
      ${card('超时/取消', s.timeouts || 0, '')}
      ${card('失败', s.errors || 0, 'red')}
      ${card('429 限流', rateLimits, '')}
      ${card('竞速成功率', rate, 'green')}
    </div>`;

  const totalNodes = (d.by_node || []).length;
  const allRows = racingSortedRows(d.by_node || []);
  const hiddenCount = totalNodes - allRows.length;
  const totalPages = Math.max(1, Math.ceil(allRows.length / raceLimit));
  if (racePage > totalPages) racePage = totalPages;
  const start = (racePage - 1) * raceLimit;
  const rows = allRows.slice(start, start + raceLimit);
  const arrow = (k) => raceSort.key === k ? (raceSort.desc ? ' ↓' : ' ↑') : '';
  const th = (label, k) => `<th class="sortable" style="cursor:pointer;user-select:none;" onclick="sortRacingBy('${k}')" title="点击排序">${label}${arrow(k)}</th>`;
  const nodeBody = rows.length ? rows.map((r, i) => {
    const idx = start + i;
    const att = r.attempts || 0;
    const addr = shortNodeAddr(r.raw_uri);
    const addrCell = addr
      ? `<code style="font-size:11px;background:rgba(0,0,0,0.25);color:var(--blue);padding:1px 4px;border-radius:4px;">${esc(addr)}</code>`
      : '<span class="desc">-</span>';
    const canAct = !!r.raw_uri;
    const lockBtn = r.locked
      ? `<button class="btn ghost" style="padding:4px 8px;font-size:12px;color:var(--gold);" ${canAct ? '' : 'disabled'} onclick="ovToggleRacingNodeLocked(${idx}, true)">解除防删</button>`
      : `<button class="btn ghost" style="padding:4px 8px;font-size:12px;${canAct ? '' : 'color:var(--text-dim);'}" ${canAct ? '' : 'disabled title="无法定位节点 URI"'} onclick="ovToggleRacingNodeLocked(${idx}, false)">防删</button>`;
    // 便于快速摘掉多次不成功的节点；防删锁定的节点豁免手动/批量禁用（健康触发的自动禁用不豁免）。
    const enBtn = r.disabled
      ? `<button class="btn ghost" style="padding:4px 8px;font-size:12px;color:var(--green);" ${canAct ? '' : 'disabled'} onclick="ovToggleRacingNodeDisabled(${idx}, true)">启用</button>`
      : `<button class="btn ghost" style="padding:4px 8px;font-size:12px;${canAct ? '' : 'color:var(--text-dim);'}" ${canAct ? '' : 'disabled title="无法定位节点 URI"'} onclick="ovToggleRacingNodeDisabled(${idx}, false)">禁用</button>`;
    const lockBadge = (r.locked ? ' <span style="color:var(--gold);font-size:11px;" title="已防删">🔒</span>' : '') +
      (r.disabled ? ' <span class="desc" style="font-size:11px;">(已禁用)</span>' : '');
    return `<tr>
      <td class="ov-cell-name" title="${esc(r.raw_uri || r.name || '')}">${esc(r.name || '未归属')}${lockBadge}</td>
      <td>${addrCell}</td>
      <td>${att}</td>
      <td class="green">${r.wins || 0}</td>
      <td>${(r.lost || 0) + (r.collects || 0)}</td>
      <td>${r.timeouts || 0}</td>
      <td class="red">${r.errors || 0}</td>
      <td>${racePct(r.wins || 0, att - (r.rate_limits || 0))}</td>
      <td>${racePct(r.timeouts || 0, att)}</td>
      <td>${r.avg_ms ? Math.round(r.avg_ms) + 'ms' : '-'}</td>
      <td style="white-space:nowrap;">${lockBtn} ${enBtn}</td>
    </tr>`;
  }).join('') : '<tr><td colspan="11" class="desc" style="text-align:center;">暂无竞速数据</td></tr>';
  const pager = `<div class="req-toolbar" style="justify-content:flex-end;">
      <span class="desc" style="white-space:nowrap;">每页</span>
      <select onchange="changeRaceLimit(this.value)" style="width:auto;margin:0;">
        ${[10,20,50,100].map(n => `<option value="${n}" ${n===raceLimit?'selected':''}>${n}</option>`).join('')}
      </select>
      <span class="desc" style="white-space:nowrap;">条 · 共 ${totalNodes} 个节点${hiddenCount > 0 ? '（已隐藏 ' + hiddenCount + '）' : ''}</span>
      <button class="btn ghost" onclick="goRacePage(racePage-1)" style="padding:4px 10px;">上一页</button>
      <span class="desc">${racePage} / ${totalPages}</span>
      <button class="btn ghost" onclick="goRacePage(racePage+1)" style="padding:4px 10px;">下一页</button>
    </div>`;
  const nodeTable = `${pager}<div class="table-wrap" style="margin-top:12px;"><table class="stats-table"><thead><tr>
      <th>节点</th><th>节点地址</th>${th('候选','attempts')}${th('胜出','wins')}${th('未胜出','not_won')}${th('超时','timeouts')}${th('失败','errors')}${th('成功率','success_rate')}${th('超时率','timeout_rate')}${th('平均耗时','avg_ms')}<th>操作</th>
    </tr></thead><tbody>${nodeBody}</tbody></table></div>`;

  const winLabel = NODE_WINDOW_LABEL[racingDays] || ('近' + racingDays + '天');
  const winSel = [1, 3, 7, 15, 30, 0]
    .map(n => `<button class="btn ghost ${n === racingDays ? 'ov-tab active' : ''}" style="padding:4px 10px;" onclick="setRacingDays(${n})">${NODE_WINDOW_LABEL[n]}</button>`)
    .join('');

  box.innerHTML = `<div class="card glass">
    <div class="req-toolbar">
      <div class="field-heading" style="margin:0;">Racing 统计 <span class="label-gold">${winLabel}</span> <span class="desc">仅胜出算成功，其余候选视为不成功；点击列头排序</span></div>
      <div style="display:flex;align-items:center;gap:6px;flex-wrap:wrap;">
        ${winSel}
        <select onchange="setRacingBucket(this.value)" style="width:auto;margin:0;">
          <option value="hour" ${racingBucket==='hour'?'selected':''}>按小时</option>
          <option value="day" ${racingBucket==='day'?'selected':''}>按天</option>
        </select>
        <button class="btn danger" style="padding:4px 10px;" onclick="resetRacingStats()">清空 Racing</button>
      </div>
    </div>
    <div class="req-toolbar">
      <span class="desc">显示过滤（仅影响本视图）：</span>
      <label class="desc" style="display:flex;align-items:center;gap:4px;white-space:nowrap;cursor:pointer;">
        <input type="checkbox" ${raceHideLocked ? 'checked' : ''} onchange="setRaceHide('locked', this.checked)">隐藏防删
      </label>
      <label class="desc" style="display:flex;align-items:center;gap:4px;white-space:nowrap;cursor:pointer;">
        <input type="checkbox" ${raceHideDisabled ? 'checked' : ''} onchange="setRaceHide('disabled', this.checked)">隐藏禁用
      </label>
    </div>
    ${cards}
    ${renderRaceSeries(d.series || [])}
    ${nodeTable}
  </div>`;
}

// racePct 以百分比展示竞速成功率，保留四位小数（≈万分之一精度）：
// 多节点竞速下单节点胜率常常很低（如 1/8），整数百分比会全部塌成 0%/13% 难以横向比较。
function racePct(wins, attempts) {
  if (!attempts) return '0%';
  const v = wins / attempts * 100;
  // 恰好为整数时不带尾随小数，避免 "100.0000%" 之类的冗余。
  if (Number.isInteger(v)) return v + '%';
  return v.toFixed(4) + '%';
}

// ovToggleRacingNodeLocked 在 Racing 面板内快速添加/解除单个节点的防删锁。
async function ovToggleRacingNodeLocked(idx, locked) {
  const row = racingSortedRows((racingData && racingData.by_node) || [])[idx];
  if (!row || !row.raw_uri) return;
  try {
    await API.nodes.lock([row.raw_uri], !locked);
    toast(!locked ? '已添加防删锁' : '已解除防删锁');
    row.locked = !locked; // 行对象与缓存共享引用，就地更新后重渲染即可
    renderRacingPanel();
  } catch (e) {
    toast('操作失败: ' + e.message);
  }
}

// racingSortedRows 先按"隐藏防删/禁用"开关过滤，再按所选列排序（默认候选次数降序）。
// 过滤置于排序之前，使渲染出的行与行内操作（按索引回取行对象）指向同一份视图。
function racingSortedRows(rows) {
  let arr = (rows || []).slice();
  if (raceHideLocked) arr = arr.filter(r => !r.locked);
  if (raceHideDisabled) arr = arr.filter(r => !r.disabled);
  const key = raceSort.key;
  const val = (r) => {
    const att = r.attempts || 0;
    switch (key) {
      case 'success_rate': return att ? (r.wins || 0) / att : -1;
      case 'timeout_rate': return att ? (r.timeouts || 0) / att : -1;
      // 未胜出列展示的是"被抢先 + 成功未胜出"之和，排序须与展示口径一致。
      case 'not_won': return (r.lost || 0) + (r.collects || 0);
      default: return Number(r[key] || 0);
    }
  };
  arr.sort((a, b) => {
    const va = val(a);
    const vb = val(b);
    if (va !== vb) return raceSort.desc ? vb - va : va - vb;
    return (b.attempts || 0) - (a.attempts || 0);
  });
  return arr;
}

// setRaceHide 切换"隐藏防删 / 隐藏禁用"开关：只影响当前视图，不重新请求。
// 用途：一键滤掉已受保护的（防删）与已停用的节点，剩下的即可快速处理的低质量节点。
function setRaceHide(kind, on) {
  if (kind === 'locked') {
    raceHideLocked = !!on;
  } else {
    raceHideDisabled = !!on;
  }
  racePage = 1;
  renderRacingPanel();
}

// ovToggleRacingNodeDisabled 在 Racing 面板内快速禁用/启用单个节点
// （防删锁定的节点豁免手动/批量禁用，故对锁定节点调用禁用不会生效；健康触发的自动禁用不豁免）。
async function ovToggleRacingNodeDisabled(idx, disabled) {
  const row = racingSortedRows((racingData && racingData.by_node) || [])[idx];
  if (!row || !row.raw_uri) return;
  try {
    if (disabled) {
      await API.nodes.batchEnable([row.raw_uri]);
      toast('已启用该节点');
    } else {
      await API.nodes.batchDisable([row.raw_uri]);
      toast('已禁用该节点');
    }
    row.disabled = !disabled; // 行对象与缓存共享引用，就地更新后重渲染即可
    renderRacingPanel();
  } catch (e) {
    toast('操作失败: ' + e.message);
  }
}

function changeRaceLimit(v) {
  raceLimit = Number(v);
  racePage = 1;
  renderRacingPanel();
}

function goRacePage(p) {
  if (p < 1) return;
  racePage = p;
  renderRacingPanel();
}

function sortRacingBy(key) {
  if (raceSort.key === key) {
    raceSort.desc = !raceSort.desc;
  } else {
    raceSort.key = key;
    raceSort.desc = true;
  }
  racePage = 1; // 换排序键后回到第 1 页，避免停在越界页
  renderRacingPanel();
}

function setRacingDays(n) { racingDays = n; racePage = 1; loadRacingPanel(); }
function setRacingBucket(b) { racingBucket = b; racePage = 1; loadRacingPanel(); }

// renderRaceSeries 用柱状图展示各时间桶的竞速成功率（胜出/候选）：柱高与颜色随成功率变化，
// 便于横向比较"哪个时间区间胜率明显更高"。
function renderRaceSeries(pts) {
  if (!pts || !pts.length) return '<div class="desc" style="margin-top:10px;">暂无时间区间数据</div>';
  const bars = pts.map(p => {
    const att = p.attempts || 0;
    const wins = p.wins || 0;
    const notWon = (p.lost || 0) + (p.collects || 0);
    const rate = att ? wins / att : 0;
    const d = new Date(p.ts * 1000);
    const label = racingBucket === 'day'
      ? `${d.getMonth()+1}/${d.getDate()}`
      : `${String(d.getHours()).padStart(2,'0')}:00`;
    const h = Math.round(rate * 80) + 2;
    const cls = att ? (rate >= 0.9 ? 'req-status-ok' : (rate >= 0.5 ? '' : 'req-status-err')) : '';
    const pctTxt = racePct(wins, att);
    return `<div class="trend-col" title="${label}: 候选 ${att} / 胜出 ${wins} / 未胜出 ${notWon} / 超时 ${p.timeouts||0} / 失败 ${p.errors||0} (成功率 ${pctTxt})">
      <div class="trend-rate ${cls}">${pctTxt}</div>
      <div class="bar-wrap"><div class="bar bar-req" style="height:${h}%"></div></div>
      <div class="trend-label">${label}</div>
    </div>`;
  }).join('');
  return `<div class="field-heading" style="margin-top:16px;">时间区间成功率 <span class="desc">胜出/候选，柱高与颜色随成功率变化（绿≥90%、黄 50–89%、红&lt;50%）</span></div>
    <div class="trend-chart">${bars}</div>`;
}

async function resetRacingStats() {
  showConfirm('确定清空全部 Racing 统计记录吗？此操作不可恢复。', async () => {
    try {
      await API.stats.racingReset();
      toast('已清空 Racing 统计');
      loadRacingPanel();
    } catch (e) {
      toast('清空失败: ' + e.message);
    }
  });
}
