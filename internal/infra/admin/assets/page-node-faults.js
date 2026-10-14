// 故障记录视图：按失败类别统计 + 展开查看完整详情 + 一键复制。
//
// 评审要求：故障分类统计保留「其他」，但不能只显示一个数量——展开后必须列出每条未归类
// 故障的完整错误码、HTTP 状态码、原始错误内容/响应摘要、首次/最近发生时间、节点与发生次数，
// 并提供一键复制，便于直接把完整信息反馈出去。已分类的错误同样保留原文作为展开项。
//
// 脱敏在后端完成（nodestore.MaskSecrets：URL query 整体替换、Authorization/Cookie/API Key
// 等的值替换为 <redacted>），因此这里展示与复制的 raw_error 都是可安全外传的。

var faultClassFilter = '';

var FAULT_CLASS_LABELS = {
  node_network: '节点网络故障',
  dns: 'DNS 解析失败',
  auth: '认证失败',
  permission: '权限/策略拒绝',
  ratelimit: '限流 429',
  timeout: '超时',
  upstream: '上游 5xx',
  probe_flow: '探测/构造错误',
  unsupported: '协议不支持',
  other: '其他',
};

var FAULT_SOURCE_LABELS = {
  request: '生产请求',
  admin_single: '手动单测',
  admin_batch: '批量测速',
  auto_probe: '自动探测',
  unsupported: '协议检查',
};

var FAULT_CLASS_COLORS = {
  node_network: 'var(--red)',
  dns: 'var(--blue)',
  auth: 'var(--gold)',
  permission: 'var(--gold)',
  ratelimit: 'var(--text-dim)',
  timeout: 'var(--text-dim)',
  upstream: 'var(--gold)',
  probe_flow: 'var(--text-dim)',
  unsupported: 'var(--text-dim)',
  other: 'var(--red)',
};

function faultClassLabel(cls) {
  return FAULT_CLASS_LABELS[cls] || ('未知(' + (cls || '') + ')');
}

function faultSourceLabel(src) {
  return FAULT_SOURCE_LABELS[src] || (src || '-');
}

function fmtFaultTime(ts) {
  if (!ts) return '-';
  return relTime(ts);
}

// 该视图已合并到侧栏独立页面「节点故障策略与记录」（见 page-fault-policies.js）。
// 保留这些入口函数名作为兼容别名：外部书签/其它脚本若还按旧名调用，不应炸掉。
function openNodeFaultsModal() { go('faults'); }

function closeNodeFaultsModal() { /* 已是独立页面，无弹窗可关 */ }

async function refreshNodeFaults() {
  const body = $('#nodeFaultsBody');
  if (body) body.innerHTML = '<div class="desc" style="padding:20px;text-align:center;">加载中...</div>';
  let d;
  try {
    d = await API.nodeFaults.list(1000);
  } catch (e) {
    if (body) body.innerHTML = '<div class="err" style="padding:20px;text-align:center;">加载失败：' + esc(String(e && e.message || e)) + '</div>';
    return;
  }
  window.lastFaultData = d || { classes: [], rows: [], retention_days: 1 };
  renderNodeFaults();
}

function renderNodeFaults() {
  const d = window.lastFaultData || { classes: [], rows: [], retention_days: 1 };
  const rows = d.rows || [];
  const classes = d.classes || [];

  const head = $('#nodeFaultsHead');
  if (head) {
    const days = d.retention_days || 1;
    // 总量必须用后端给的全表聚合值（total_count / total_rows），不能用返回集自己求和——
    // 数据超过查询上限时那会把"最近 N 条"显示成"共 N 次"，让用户以为看到了全部历史。
    const haveTotals = (d.total_count !== undefined && d.total_count !== null);
    const totalCount = haveTotals ? d.total_count : rows.reduce(function (a, r) { return a + (r.count || 0); }, 0);
    const totalRows = haveTotals ? d.total_rows : rows.length;
    let html = '共 <b style="color:var(--text);">' + totalCount + '</b> 次故障、' +
      totalRows + ' 条聚合记录（记录仅保留最近 <b style="color:var(--text);">' + days +
      '</b> 天；上方的分类统计为全量，下方列表最多显示 ' + (d.limit || rows.length) + ' 条）。';
    if (d.truncated) {
      html += ' <span style="color:var(--gold);font-weight:600;">当前仅列出最近 ' + rows.length +
        ' 条 / 共 ' + totalRows + ' 条，并非全部</span>；可用「一键复制全部」导出当前列出的部分。';
    }
    html += ' 展开任一行可看完整错误码、HTTP 状态码、脱敏原文与首末发生时间。';
    head.innerHTML = html;
  }

  const chips = $('#nodeFaultsClasses');
  if (chips) {
    chips.innerHTML = '';
    if (classes.length === 0) {
      chips.innerHTML = '<span class="desc">暂无故障记录</span>';
    } else {
      classes.forEach(function (c) {
        const b = document.createElement('button');
        b.className = 'fault-chip' + (faultClassFilter === c.class ? ' active' : '');
        b.style.color = FAULT_CLASS_COLORS[c.class] || 'var(--text)';
        b.textContent = faultClassLabel(c.class) + ' ' + c.count;
        b.title = c.count + ' 次，' + c.nodes + ' 个节点，' + c.rows + ' 条聚合记录';
        b.onclick = function () {
          faultClassFilter = (faultClassFilter === c.class) ? '' : c.class;
          renderNodeFaults();
        };
        chips.appendChild(b);
      });
      const reset = document.createElement('button');
      reset.className = 'fault-chip';
      reset.textContent = '显示全部';
      reset.onclick = function () { faultClassFilter = ''; renderNodeFaults(); };
      chips.appendChild(reset);
    }
  }

  const list = $('#nodeFaultsList');
  if (!list) return;
  list.innerHTML = '';
  const shown = rows.filter(function (r) { return !faultClassFilter || r.failure_class === faultClassFilter; });
  if (shown.length === 0) {
    list.innerHTML = '<tr><td colspan="8" class="desc" style="text-align:center;padding:18px;">无匹配记录</td></tr>';
    return;
  }
  shown.forEach(function (r) {
    list.appendChild(buildFaultRow(r));
  });
}

// buildFaultRow 生成一行"摘要 + 可展开详情"的故障记录。摘要行展示节点、类别、次数与时间；
// 展开行展示完整错误码、HTTP 状态码、探测目标、来源与脱敏原文。
function buildFaultRow(r) {
  const tr = document.createElement('tr');
  const cls = r.failure_class || 'other';
  const color = FAULT_CLASS_COLORS[cls] || 'var(--text)';

  function td(text, title) {
    const c = document.createElement('td');
    c.textContent = text;
    if (title) c.title = title;
    return c;
  }

  const nameTd = document.createElement('td');
  const nameWrap = document.createElement('div');
  nameWrap.style.cssText = 'display:flex;align-items:center;gap:6px;flex-wrap:wrap;';
  const toggle = document.createElement('button');
  toggle.className = 'fault-toggle';
  toggle.textContent = '▸';
  toggle.title = '展开完整故障详情';
  const nameText = document.createElement('span');
  nameText.textContent = r.node_name || r.node_uri || '(未知节点)';
  nameText.title = r.node_uri || '';
  nameWrap.appendChild(toggle);
  nameWrap.appendChild(nameText);
  nameTd.appendChild(nameWrap);
  tr.appendChild(nameTd);

  const classTd = td(faultClassLabel(cls));
  classTd.style.color = color;
  classTd.style.fontWeight = '600';
  classTd.title = '归因类别：' + cls;
  tr.appendChild(classTd);

  tr.appendChild(td(r.error_code || '-', r.error_code || '未提取到错误码'));
  tr.appendChild(td(r.http_status ? String(r.http_status) : '-'));
  tr.appendChild(td('×' + (r.count || 0)));
  tr.appendChild(td(fmtFaultTime(r.first_seen_at)));
  tr.appendChild(td(fmtFaultTime(r.last_seen_at)));
  tr.appendChild(td(faultSourceLabel(r.source)));

  const detailTr = document.createElement('tr');
  detailTr.className = 'fault-detail-row hidden';
  const detailTd = document.createElement('td');
  detailTd.colSpan = 8;
  detailTd.innerHTML = faultDetailHTML(r);
  detailTr.appendChild(detailTd);

  toggle.onclick = function () {
    const hidden = detailTr.classList.toggle('hidden');
    toggle.textContent = hidden ? '▸' : '▾';
  };

  const frag = document.createDocumentFragment();
  frag.appendChild(tr);
  frag.appendChild(detailTr);
  return frag;
}

function faultDetailHTML(r) {
  const scope = r.scope === 'entry' ? '前置代理池' : '出口节点池';
  const parts = [];
  parts.push('<div class="fault-detail-grid">');
  parts.push('<div><b>节点</b>：' + esc(r.node_name || '-') + ' <span class="desc">' + esc(r.node_uri || '') + '</span></div>');
  parts.push('<div><b>所属</b>：' + scope + '</div>');
  parts.push('<div><b>类别</b>：' + esc(faultClassLabel(r.failure_class)) + ' <span class="desc">' + esc(r.failure_class || '') + '</span></div>');
  parts.push('<div><b>错误码</b>：' + esc(r.error_code || '未提取到') + '</div>');
  parts.push('<div><b>HTTP 状态码</b>：' + (r.http_status ? r.http_status : '无') + '</div>');
  parts.push('<div><b>探测目标</b>：' + esc(r.target || '未记录') + '</div>');
  parts.push('<div><b>首次发生</b>：' + esc(absTime(r.first_seen_at)) + '</div>');
  parts.push('<div><b>最近发生</b>：' + esc(absTime(r.last_seen_at)) + '</div>');
  parts.push('<div><b>发生次数</b>：' + (r.count || 0) + '</div>');
  parts.push('<div><b>发现来源</b>：' + esc(faultSourceLabel(r.source)) + '</div>');
  parts.push('</div>');
  parts.push('<div style="margin-top:8px;"><b>原始错误（已脱敏）</b></div>');
  parts.push('<pre class="fault-raw">' + esc(r.raw_error || '(无原文)') + '</pre>');
  parts.push('<div class="fault-detail-actions">');
  parts.push('<button class="btn ghost" style="padding:3px 10px;font-size:12px;" onclick="copyFaultRow(' + r.id + ')">复制本条详情</button>');
  parts.push('</div>');
  return parts.join('');
}

// faultRowText 把一条故障记录拍平成可直接粘贴给他人的纯文本。
function faultRowText(r) {
  const lines = [
    '节点：' + (r.node_name || '-') + ' (' + (r.node_uri || '') + ')',
    '所属：' + (r.scope === 'entry' ? '前置代理池' : '出口节点池'),
    '类别：' + faultClassLabel(r.failure_class) + ' [' + (r.failure_class || '') + ']',
    '错误码：' + (r.error_code || '未提取到'),
    'HTTP 状态码：' + (r.http_status || '无'),
    '探测目标：' + (r.target || '未记录'),
    '首次发生：' + absTime(r.first_seen_at),
    '最近发生：' + absTime(r.last_seen_at),
    '发生次数：' + (r.count || 0),
    '发现来源：' + faultSourceLabel(r.source),
    '原始错误（已脱敏）：' + (r.raw_error || '(无原文)'),
  ];
  return lines.join('\n');
}

function copyFaultRow(id) {
  const d = window.lastFaultData || { rows: [] };
  const r = (d.rows || []).filter(function (x) { return x.id === id; })[0];
  if (!r) return;
  copyTextToClipboard(faultRowText(r), '已复制该条故障详情');
}

function copyAllFaults() {
  const d = window.lastFaultData || { rows: [] };
  const rows = (d.rows || []).filter(function (r) { return !faultClassFilter || r.failure_class === faultClassFilter; });
  if (rows.length === 0) {
    toast('没有可复制的故障记录');
    return;
  }
  const head = '故障记录导出（' + absTime(Math.floor(Date.now() / 1000)) + '，仅含最近 ' +
    ((d.retention_days || 1)) + ' 天，敏感信息已脱敏）\n' + '='.repeat(48) + '\n';
  copyTextToClipboard(head + rows.map(faultRowText).join('\n\n'), '已复制 ' + rows.length + ' 条故障详情');
}

async function clearNodeFaults() {
  if (!confirm('确认清空全部故障记录？清空后无法恢复。')) return;
  try {
    const d = await API.nodeFaults.clear();
    toast('已清空 ' + ((d && d.deleted) || 0) + ' 条故障记录');
    await refreshNodeFaults();
  } catch (e) {
    toast('清空失败：' + String(e && e.message || e));
  }
}

// copyTextToClipboard 复用 utils.js 的 copyText（navigator.clipboard + execCommand 降级）：
// 局域网 HTTP 部署下 clipboard API 不可用，降级路径保证"一键复制"仍然可用。
function copyTextToClipboard(text, okMsg) {
  copyText(text, null);
  if (okMsg) toast(okMsg);
}

// absTime 绝对时间（故障排查需要精确到分钟，相对时间只用于列表速览）。
function absTime(ts) {
  if (!ts) return '-';
  const d = new Date(ts * 1000);
  function p(n) { return (n < 10 ? '0' : '') + n; }
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

// relTime 相对时间（列表速览用；详情面板一律用绝对时间，排查需要精确到秒）。
function relTime(ts) {
  const diff = Math.floor(Date.now() / 1000) - ts;
  if (diff < 60) return diff + ' 秒前';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  return Math.floor(diff / 86400) + ' 天前';
}
