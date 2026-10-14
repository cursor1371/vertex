// ── HTTP 日志页：分页列表 + 扩展筛选 + 列显隐 + 单条详情展开 ──

let httpLogPage = 1;
let httpLogLimit = 20;
let httpLogDetailId = null;
let httpLogExportFmt = 'json';

// HTTP_LOG_COLUMNS 定义全部可显示列；httpLogVisibleCols 记录用户显隐选择（localStorage 持久化）。
const HTTP_LOG_COLUMNS = [
  { key: 'time', label: '时间' },
  { key: 'req_id', label: 'request_id' },
  { key: 'scope', label: '范围' },
  { key: 'node', label: '节点' },
  { key: 'peer_ip', label: '对端 IP' },
  { key: 'protocol', label: '协议' },
  { key: 'reused', label: '复用' },
  { key: 'http_status', label: 'HTTP状态码' },
  { key: 'http_result', label: 'HTTP结果' },
  { key: 'resp_code', label: '响应状态码' },
  { key: 'resp_result', label: '响应结果' },
  { key: 'err_type', label: '失败类型' },
  { key: 'elapsed', label: '耗时' },
  { key: 'actions', label: '' },
];

function httpLogDefaultCols() {
  return HTTP_LOG_COLUMNS.map(c => c.key);
}

function httpLogVisibleCols() {
  try {
    const raw = localStorage.getItem('httplog.cols');
    if (!raw) return httpLogDefaultCols();
    const parsed = JSON.parse(raw);
    const valid = HTTP_LOG_COLUMNS.map(c => c.key);
    const kept = valid.filter(k => parsed.indexOf(k) >= 0);
    // 始终保留操作列与时间列
    ['time', 'actions'].forEach(k => { if (kept.indexOf(k) < 0) kept.unshift(k); });
    return kept;
  } catch (e) {
    return httpLogDefaultCols();
  }
}

function httpLogSaveCols(cols) {
  try { localStorage.setItem('httplog.cols', JSON.stringify(cols)); } catch (e) {}
}

function httpLogColVisible(key) {
  return httpLogVisibleCols().indexOf(key) >= 0;
}

function fmtHTTPTime(ts) {
  const d = new Date(ts * 1000);
  return `${d.getFullYear()}-${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')} ${String(d.getHours()).padStart(2,'0')}:${String(d.getMinutes()).padStart(2,'0')}:${String(d.getSeconds()).padStart(2,'0')}`;
}

// httpLogFilterQS 收集全部筛选条件（列表与导出共用）。
function httpLogFilterQS() {
  let qs = `page=${httpLogPage}&limit=${httpLogLimit}`;
  const val = id => { const el = $('#' + id); return el ? el.value.trim() : ''; };
  const succ = val('httpLogSuccess');
  if (succ === '1' || succ === '0') qs += `&success=${succ}`;
  const scope = val('httpLogScope');
  if (scope) qs += `&scope=${encodeURIComponent(scope)}`;
  const rid = val('httpLogReqID');
  if (rid) qs += `&request_id=${encodeURIComponent(rid)}`;
  const node = val('httpLogNode');
  if (node) qs += `&node=${encodeURIComponent(node)}`;
  const peer = val('httpLogPeerIP');
  if (peer) qs += `&peer_ip=${encodeURIComponent(peer)}`;
  const hs = val('httpLogHTTPStatus');
  if (hs && /^\d+$/.test(hs)) qs += `&http_status=${hs}`;
  const rc = val('httpLogRespCode');
  if (rc && /^\d+$/.test(rc)) qs += `&response_code=${rc}`;
  const et = val('httpLogErrorType');
  if (et) qs += `&error_type=${encodeURIComponent(et)}`;
  const from = datetimeLocalToUnix(val('httpLogFrom'));
  if (from > 0) qs += `&from=${from}`;
  const to = datetimeLocalToUnix(val('httpLogTo'));
  if (to > 0) qs += `&to=${to}`;
  return qs;
}

// datetimeLocalToUnix 把 datetime-local 输入值转 Unix 秒（空/非法返回 0）。
function datetimeLocalToUnix(v) {
  if (!v) return 0;
  const ms = new Date(v).getTime();
  return isNaN(ms) ? 0 : Math.floor(ms / 1000);
}

function resetHTTPLogFilters() {
  ['httpLogSuccess', 'httpLogScope', 'httpLogReqID', 'httpLogNode', 'httpLogPeerIP',
    'httpLogHTTPStatus', 'httpLogRespCode', 'httpLogErrorType', 'httpLogFrom', 'httpLogTo'].forEach(id => {
    const el = $('#' + id);
    if (el) el.value = '';
  });
  httpLogPage = 1;
  loadHTTPLog();
}

// ── 列显隐控制 ──

function toggleHTTPLogCols() {
  const panel = $('#httpLogColPanel');
  if (!panel) return;
  panel.classList.toggle('hidden');
  if (!panel.classList.contains('hidden')) renderHTTPLogColControls();
}

function renderHTTPLogColControls() {
  const panel = $('#httpLogColPanel');
  if (!panel) return;
  const cols = httpLogVisibleCols();
  panel.innerHTML = `<div class="card glass httplog-cols-card">
    <div class="field-heading" style="margin:0 0 8px;">列显示控制</div>
    <div class="httplog-cols-grid">
      ${HTTP_LOG_COLUMNS.map(c => `<label class="httplog-col-item">
        <input type="checkbox" data-col="${c.key}" ${cols.indexOf(c.key) >= 0 ? 'checked' : ''}
          onchange="setHTTPLogCol('${c.key}', this.checked)">
        <span>${esc(c.label) || '(操作)'}</span>
      </label>`).join('')}
    </div>
    <div style="display:flex;gap:8px;margin-top:10px;">
      <button class="btn ghost" onclick="httpLogColsAll(true)">全选</button>
      <button class="btn ghost" onclick="httpLogColsAll(false)">全不选</button>
    </div>
  </div>`;
}

function setHTTPLogCol(key, visible) {
  let cols = httpLogVisibleCols();
  if (key === 'time' || key === 'actions') return; // 时间与操作列不可隐藏
  const idx = cols.indexOf(key);
  if (visible && idx < 0) cols.push(key);
  if (!visible && idx >= 0) cols.splice(idx, 1);
  httpLogSaveCols(cols);
  loadHTTPLog();
}

function httpLogColsAll(v) {
  const cols = v ? HTTP_LOG_COLUMNS.map(c => c.key) : ['time', 'actions'];
  httpLogSaveCols(cols);
  const panel = $('#httpLogColPanel');
  if (panel) panel.querySelectorAll('input[data-col]').forEach(inp => {
    if (inp.dataset.col === 'time' || inp.dataset.col === 'actions') inp.checked = true;
    else inp.checked = v;
  });
  loadHTTPLog();
}

// renderHTTPLogHead 按可见列渲染表头。
function renderHTTPLogHead() {
  const head = $('#httpLogHead');
  if (!head) return;
  const cols = httpLogVisibleCols();
  const labels = {};
  HTTP_LOG_COLUMNS.forEach(c => labels[c.key] = c.label);
  head.innerHTML = cols.map(k => `<th>${esc(labels[k] || '')}</th>`).join('');
}

// httpLogCell 构造单列单元格（按可见列调用方负责过滤）。
function httpLogCell(key, r) {
  switch (key) {
    case 'time': return `<td style="white-space:nowrap;">${fmtHTTPTime(r.ts)}</td>`;
    case 'req_id': {
      const rid = r.request_id || '-';
      const short = rid.length > 16 ? rid.slice(0, 16) + '…' : rid;
      return `<td data-rid="${esc(rid)}" title="${esc(rid)}" style="cursor:pointer;" onclick="copyText(this.dataset.rid, this)">${esc(short)}</td>`;
    }
    case 'scope': return `<td>${esc(r.scope || '-')}</td>`;
    case 'node': return `<td title="${esc(r.node_uri)}">${esc(r.node_name || r.node_uri || '-')}</td>`;
    case 'peer_ip': return `<td>${esc(r.peer_ip || '-')}</td>`;
    case 'protocol': return `<td>${esc(r.http_version || '-')}</td>`;
    case 'reused': return r.tcp_reused ? '<td><span class="req-status-ok">复用</span></td>' : '<td><span class="req-stream-badge n">新建</span></td>';
    case 'http_status': return `<td>${r.status_code || '-'}</td>`;
    case 'http_result': {
      const cls = r.success ? 'req-status-ok' : 'req-status-err';
      const txt = r.success ? '成功' : ('失败' + (r.status_code ? ' (' + r.status_code + ')' : ''));
      return `<td class="${cls}" title="${esc(r.error || '')}">${txt}</td>`;
    }
    case 'resp_code': return `<td>${r.response_code || (r.success ? '0' : '-')}</td>`;
    case 'resp_result': {
      const cls = r.success && (!r.response_code || r.response_code < 400) ? 'req-status-ok' : 'req-status-err';
      const txt = r.response_result ? esc(r.response_result) : (r.success ? 'OK' : '-');
      return `<td class="${cls}" title="${esc(r.error || '')}">${txt}</td>`;
    }
    case 'err_type': {
      const et = r.response_error_type ? esc(r.response_error_type) : '-';
      const em = r.response_error_message ? esc(r.response_error_message) : '';
      return `<td title="${em}">${et}</td>`;
    }
    case 'elapsed': return `<td>${r.elapsed_ms ? (r.elapsed_ms/1000).toFixed(2)+'s' : '-'}</td>`;
    case 'actions': return `<td><button class="btn ghost" style="padding:2px 8px;" onclick="loadHTTPLogDetail(${r.id})">详情</button></td>`;
    default: return '<td>-</td>';
  }
}

// reset=true 时回到第 1 页：此前 admin.html 的「筛选/重置」等按钮传的参数被忽略，
// 当前在第 N 页点筛选会停留在夹取后的页码，不符合"筛选后从头看"的直觉。
async function loadHTTPLog(reset) {
  if (reset) httpLogPage = 1;
  const tbody = $('#httpLogTbody');
  if (!tbody) return;
  renderHTTPLogHead();
  let data;
  try {
    data = await API.httpLog.list(httpLogFilterQS());
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="${httpLogVisibleCols().length}" class="desc">加载失败: ${esc(e.message)}</td></tr>`;
    return;
  }
  const items = (data && data.items) || [];
  const total = (data && data.total) || 0;
  const totalPages = Math.max(1, Math.ceil(total / httpLogLimit));
  if (httpLogPage > totalPages) httpLogPage = totalPages;
  $('#httpLogTotal').textContent = '共 ' + total + ' 条';
  $('#httpLogPageInfo').textContent = httpLogPage + ' / ' + totalPages;

  if (!items.length) {
    tbody.innerHTML = `<tr><td colspan="${httpLogVisibleCols().length}" class="desc">暂无记录（需在设置中开启 HTTP 日志）</td></tr>`;
    return;
  }
  const cols = httpLogVisibleCols();
  const rows = items.map(r => `<tr>${cols.map(k => httpLogCell(k, r)).join('')}</tr>`).join('');
  tbody.innerHTML = rows;
}

function goHTTPLogPage(p) {
  if (p < 1) return;
  httpLogPage = p;
  loadHTTPLog();
}

async function loadHTTPLogDetail(id) {
  const box = $('#httpLogDetail');
  if (!box) return;
  if (httpLogDetailId === id) {
    httpLogDetailId = null;
    box.innerHTML = '';
    return;
  }
  httpLogDetailId = id;
  box.innerHTML = '<div class="card glass"><div class="desc">加载中…</div></div>';
  let data;
  try {
    data = await API.httpLog.detail(id);
  } catch (e) {
    box.innerHTML = `<div class="card glass"><div class="desc">详情加载失败: ${esc(e.message)}</div></div>`;
    return;
  }
  const r = data && data.item;
  if (!r) {
    box.innerHTML = '<div class="card glass"><div class="desc">记录不存在</div></div>';
    return;
  }
  const kv = (label, val) => `<div class="field-heading" style="margin-top:12px;">${label}</div><pre class="httplog-pre">${esc(val || '(空)')}</pre>`;
  const errBox = r.error ? `<div class="req-status-err" style="margin-top:10px;">⚠ 传输层失败: ${esc(r.error)}</div>` : '';
  const bizErr = (r.response_error_type || r.response_error_message)
    ? `<div class="req-status-err" style="margin-top:10px;">⚠ 业务失败: ${esc(r.response_error_type || '')}${r.response_error_message ? ' — ' + esc(r.response_error_message) : ''}</div>` : '';
  const statusBox = `<div class="grid grid-2" style="gap:10px;margin-top:10px;">
    <div class="card glass" style="padding:10px 12px;">
      <div class="desc">传输层 HTTP 状态</div>
      <div style="font-size:calc(var(--base-font-size) * 15 / 14);font-weight:600;margin-top:4px;">${r.status_code || '-'} · ${r.success ? '成功' : '失败'}</div>
      <div class="desc">协议: ${esc(r.http_version || '-')} · 复用: ${r.tcp_reused ? '是' : '否'}</div>
    </div>
    <div class="card glass" style="padding:10px 12px;">
      <div class="desc">业务响应状态</div>
      <div style="font-size:calc(var(--base-font-size) * 15 / 14);font-weight:600;margin-top:4px;">${r.response_code || (r.success ? '0' : '-')} · ${esc(r.response_result || (r.success ? 'OK' : '-'))}</div>
      <div class="desc">来自响应体 error.code / finishReason</div>
    </div>
  </div>`;
  box.innerHTML = `<div class="card glass">
    <div class="req-toolbar" style="margin-bottom:4px;">
      <div class="field-heading" style="margin:0;">请求详情 <span class="label-gold">#${r.id}</span></div>
      <button class="btn ghost" onclick="loadHTTPLogDetail(${r.id})" style="padding:4px 10px;">收起</button>
    </div>
    ${errBox}
    ${bizErr}
    ${statusBox}
    <div class="grid grid-2" style="gap:10px;">
      ${kv('最终 URL', r.final_url)}
      ${kv('代理节点', (r.node_name || r.node_uri || '-') + (r.peer_ip ? ' / IP: ' + r.peer_ip : ''))}
      ${kv('请求头', r.request_headers)}
      ${kv('响应头', r.response_headers)}
    </div>
    ${kv('请求体', r.request_body)}
    ${kv('响应体', r.response_body)}
  </div>`;
}

// ── 导出 / 导入 ──

function httpLogExport() {
  const qs = httpLogFilterQS().replace(/&?page=\d+/, '').replace(/&?limit=\d+/, '');
  const url = API.httpLog.exportUrl(qs + '&format=' + httpLogExportFmt);
  const a = document.createElement('a');
  a.href = url;
  a.download = '';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

function httpLogImport() {
  const input = document.createElement('input');
  input.type = 'file';
  input.accept = '.json,.txt';
  input.onchange = async () => {
    const file = input.files && input.files[0];
    if (!file) return;
    try {
      const text = await file.text();
      const parsed = JSON.parse(text);
      const items = Array.isArray(parsed) ? parsed : (parsed.items || []);
      if (!items.length) throw new Error('文件内容为空或格式不符');
      const res = await API.httpLog.importData(items);
      toast('导入完成: 新增 ' + (res.inserted||0) + ' 条' + (res.skipped ? '，跳过重复 ' + res.skipped + ' 条' : ''));
      loadHTTPLog();
    } catch (e) {
      toast('导入失败: ' + e.message);
    }
  };
  input.click();
}

async function resetHTTPLog() {
  showConfirm('确定清空全部 HTTP 日志吗？此操作不可恢复。', async () => {
    try {
      await API.httpLog.reset();
      toast('已清空 HTTP 日志');
      httpLogPage = 1;
      httpLogDetailId = null;
      $('#httpLogDetail').innerHTML = '';
      loadHTTPLog();
    } catch (e) {
      toast('清空失败: ' + e.message);
    }
  });
}
