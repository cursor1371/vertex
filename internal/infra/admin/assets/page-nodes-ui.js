// page-nodes-ui.js — 页面 DOM 渲染与交互
document.getElementById('nodesBody').addEventListener('click', function (e) {
  var btn = e.target.closest('[data-action]');
  if (!btn) return;
  var uri = btn.dataset.uri;
  var action = btn.dataset.action;
  if (action === 'use-node') useNode(uri);
  else if (action === 'unuse-node') unuseNode(uri);
  else if (action === 'delete-node') delNode(uri);
  else if (action === 'test-node') testSingleNode(uri);
  else if (action === 'enable-node') enableNode(uri);
  else if (action === 'disable-node') disableNode(uri);
  else if (action === 'lock-node') lockNode(uri, btn.dataset.locked !== '1');
});

var curNodePage = 1;
var nodePageSize = 50;
var totalNodePages = 1;
var cachedNodesList = [];
window.selectedNodeURIs = window.selectedNodeURIs || new Set();
var testProgressTimer = null;
window.nodeSortDesc = localStorage.getItem('vproxy_node_sort_desc') === '1';

// 搜索 / 统计排序状态。注意：变量名不能叫 nodeSortDesc——非模块脚本下 var 即 window 属性，
// 会与上面「按延迟排序」的 window.nodeSortDesc 冲突，故统一加 Stat 前缀区分。
var nodeSearchQuery = '';
var nodeSearchTimer = null;
var nodeStatSortKey = 'default'; // default | requests | success_rate | race_wins | race_rate | rt_rank
var nodeStatSortDesc = true;
// 「防删节点」显示过滤三态：'' 忽略 / 'only' 只看防删 / 'hide' 隐藏防删。
// 只影响当前视图，不重新请求后端，与搜索/排序串联。
var nodeLockedFilter = '';
// 「rT 档位」显示过滤：'' 全部 / '0' 仅健康 / '1' 仅偏差 / '2' 仅冷却 / 'abnormal' 仅异常(≥1)。
// 值来自后端 rt_rank（rtRankOf 判据），前端不做二次判定。
var nodeRTFilter = '';
// 「启用状态」显示过滤四态：'' 全部 / 'enabled' 只看启用 / 'disabled_health' 只看禁用（健康·可自愈）
// / 'disabled_manual' 只看禁用（需人工恢复）。可自愈性取自健康映射（只有 health 来源为真），
// 见 disabledByHealthOf；人工/admin 测试/协议不支持/永久禁用都归入"需人工恢复"。
var nodeDisabledFilter = '';
// 「只看 rT 异常」旧开关变量：已被 nodeRTFilter 取代，保留赋值点仅为兼容旧 localStorage 键。
var nodeOnlyRTAbnormal = false;
var proxyOnlyLocked = false;
window.nodeStatsMap = window.nodeStatsMap || {};
window.lastNodesList = [];
window.lastHealthMap = {};
window.lastRTRankMap = {};
window.lastProxyHealth = window.lastProxyHealth || {};

// serverInfoFor 从原始 URI 提取可读的 host:port（vmess 走 base64 JSON，其余走 URL 解析）。
// 节点展示与「按地址搜索」共用同一套推导，避免两处逻辑漂移。
function serverInfoFor(rawURI) {
  try {
    if (rawURI.startsWith('vmess://')) {
      var b64Str = rawURI.slice(8).split(/[?#]/)[0];
      var b = atob(b64Str.replace(/-/g, '+').replace(/_/g, '/'));
      var info = JSON.parse(b);
      return (info.add || 'unknown') + ':' + (info.port || '');
    }
    var urlObj = new URL(rawURI);
    return urlObj.hostname + ':' + (urlObj.port || '443');
  } catch (e) {
    return '\u914D\u7F6E\u683C\u5F0F\u590D\u6742';
  }
}

// nodeStatVal 取某节点在某统计维度上的排序值；无数据返回 -1，保证排在最后。
function nodeStatVal(uri, key) {
  if (key === 'rt_rank') {
    // rT 降权层来自后端快照：0 健康 / 1 差 / 2 冷却中。降序=异常在前。
    return window.lastRTRankMap[uri] || 0;
  }
  var s = window.nodeStatsMap[uri];
  if (!s) return -1;
  switch (key) {
    case 'requests': return s.success || 0;
    case 'success_rate': return s.requests ? (s.success || 0) / s.requests : -1;
    case 'race_wins': return s.wins || 0;
    case 'race_rate': {
      var att = s.attempts || 0;
      return att ? ((s.wins || 0) + (s.collects || 0)) / att : -1;
    }
    default: return 0;
  }
}

// rtAbnormal 判定节点是否「rT 异常」：直接消费后端导出的 rt_rank（rtRankOf 判据，
// 0 健康 / 1 差 / 2 冷却中）。此前前端用 rt_cooldown_until/token_fail_streak/rt_last_ms
// 自算——当前后端实现下结果碰巧一致，但阈值（rtSlowMsThreshold）与判据改动即漂移，
// 且排序也需要同一份数据，故统一收敛到后端权威值。
function rtAbnormal(uri) {
  return (window.lastRTRankMap[uri] || 0) >= 1;
}

// disableInfoOf 返回节点禁用来源对应的徽章文案与悬停说明。节点对象自带 disable_source
//（后端 Node.DisableSource）：健康映射只有布尔标记，无法区分 permanent / admin_test /
// unsupported 三种"不可自动恢复"的来源，故只作兜底。
function disableInfoOf(n) {
  var src = n && n.disable_source ? n.disable_source : '';
  if (!src && disabledByHealthOf(n.raw_uri)) src = 'health';
  var reason = (n && n.disable_reason) ? n.disable_reason : '';
  var at = (n && n.disabled_at) ? ('禁用时间 ' + faultAbsTime(n.disabled_at)) : '';
  var tip = function (base) { return [base, reason, at].filter(Boolean).join('\n'); };
  switch (src) {
    case 'health':
      return {
        label: '已禁用（健康·可自动恢复）',
        title: tip('连续节点级连通性失败触发的自动禁用；自动探测成功后会自动恢复')
      };
    case 'permanent':
      return {
        label: '已禁用（持续不可恢复）',
        title: tip('自动探测连续失败达阈值，判定为持续不可恢复；只能手动启用')
      };
    case 'admin_test':
      return {
        label: '已禁用（测试失败）',
        title: tip('管理端测试失败后直接禁用；需手动启用')
      };
    case 'unsupported':
      return {
        label: '已禁用（协议不支持）',
        title: tip('节点协议不支持或解析失败，无法参与调度')
      };
    default:
      return {
        label: '已禁用（人工）',
        title: tip('人工禁用，不会自动恢复，需手动启用')
      };
  }
}

// faultClassLabelOf / faultRelTime / faultAbsTime：复用故障视图（page-node-faults.js）的
// 展示辅助。节点表可能先于该脚本初始化，故做存在性兜底，避免加载顺序变化导致整表渲染失败。
function faultClassLabelOf(cls) {
  return (typeof faultClassLabel === 'function') ? faultClassLabel(cls) : cls;
}
function faultRelTime(ts) {
  return (typeof relTime === 'function') ? relTime(ts) : '-';
}
function faultAbsTime(ts) {
  return (typeof absTime === 'function') ? absTime(ts) : '-';
}

// renderGlobalFaultBanner 渲染"疑似公共依赖/环境故障"提示条：命中期间该类别暂停自动禁用，
// 让管理员知道"这批失败可能不是节点的问题"，而不是去逐个换节点。
function renderGlobalFaultBanner(map) {
  var el = $('#globalFaultBanner');
  if (!el) return;
  var classes = Object.keys(map || {});
  if (classes.length === 0) {
    el.classList.add('hidden');
    el.innerHTML = '';
    return;
  }
  var parts = classes.map(function (c) { return faultClassLabelOf(c) + '（' + map[c] + ' 个节点）'; });
  el.classList.remove('hidden');
  el.className = 'global-fault-banner';
  el.textContent = '疑似公共依赖/环境故障：' + parts.join('、') +
    ' 在短时间内集中失败，已暂停该类别节点的自动禁用。若持续出现，请先排查本机网络/DNS 与上游公共依赖。';
}

// disabledByHealthOf 读节点的禁用来源。权威数据在健康映射（NodeHealth.disabled_by_health，
// 随 /nodes 的 health 字段下发），不在节点对象上——此前直接读 n.disabled_by_health 恒为
// undefined，导致「健康·可自动恢复」徽章从未显示、所有禁用节点都被当成人工禁用。
// 健康条目缺失（如 MergeNodes 未建健康行）时按人工处理，与修复前行为一致。
function disabledByHealthOf(uri) {
  var h = (window.lastHealthMap || {})[uri];
  return !!(h && h.disabled_by_health);
}

// filteredNodes 依次应用：启用状态四态过滤 → 防删三态过滤 → rT 档位过滤 → 搜索词
// （匹配节点名 / 地址 / 原始 URI，大小写不敏感）。
function filteredNodes(nodes) {
  var out = nodes;
  if (nodeDisabledFilter === 'enabled') {
    out = out.filter(function (n) { return !n.disabled; });
  } else if (nodeDisabledFilter === 'disabled_health') {
    out = out.filter(function (n) { return !!n.disabled && disabledByHealthOf(n.raw_uri); });
  } else if (nodeDisabledFilter === 'disabled_manual') {
    out = out.filter(function (n) { return !!n.disabled && !disabledByHealthOf(n.raw_uri); });
  }
  if (nodeLockedFilter === 'only') {
    out = out.filter(function (n) { return !!n.locked; });
  } else if (nodeLockedFilter === 'hide') {
    out = out.filter(function (n) { return !n.locked; });
  }
  if (nodeRTFilter !== '') {
    var ranks = window.lastRTRankMap || {};
    out = out.filter(function (n) {
      var rank = ranks[n.raw_uri] || 0;
      if (nodeRTFilter === 'abnormal') return rank >= 1;
      return String(rank) === nodeRTFilter;
    });
  }
  var q = (nodeSearchQuery || '').trim().toLowerCase();
  if (!q) return out;
  return out.filter(function (n) {
    var name = (n.name || '').toLowerCase();
    var addr = serverInfoFor(n.raw_uri).toLowerCase();
    var uri = (n.raw_uri || '').toLowerCase();
    return name.indexOf(q) !== -1 || addr.indexOf(q) !== -1 || uri.indexOf(q) !== -1;
  });
}

// sortedNodes 按统计维度排序；'default' 保持后端给出的原始顺序。
function sortedNodes(nodes) {
  if (nodeStatSortKey === 'default') return nodes;
  var arr = nodes.slice();
  arr.sort(function (a, b) {
    var va = nodeStatVal(a.raw_uri, nodeStatSortKey);
    var vb = nodeStatVal(b.raw_uri, nodeStatSortKey);
    if (va !== vb) return nodeStatSortDesc ? vb - va : va - vb;
    return (a.name || '').localeCompare(b.name || '');
  });
  return arr;
}

function onNodeSearchInput(v) {
  if (nodeSearchTimer) clearTimeout(nodeSearchTimer);
  nodeSearchTimer = setTimeout(function () {
    nodeSearchQuery = v || '';
    curNodePage = 1;
    renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
  }, 200);
}

function clearNodeSearch() {
  var el = document.getElementById('nodeSearch');
  if (el) el.value = '';
  nodeSearchQuery = '';
  curNodePage = 1;
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}

function onNodeSortChange(v) {
  nodeStatSortKey = v || 'default';
  curNodePage = 1;
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}

// setNodeRTFilter 切换「rT 档位」筛选：'' 全部 / '0' 仅健康 / '1' 仅偏差 / '2' 仅冷却 /
// 'abnormal' 仅异常(≥1)。只影响当前视图，不重新请求后端。
function setNodeRTFilter(v) {
  nodeRTFilter = v || '';
  curNodePage = 1;
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}

// setNodeLockedFilter 切换「防删节点」三态：'' 忽略 / 'only' 只看 / 'hide' 隐藏。
// 只影响当前视图，不重新请求后端。
function setNodeLockedFilter(v) {
  nodeLockedFilter = v || '';
  curNodePage = 1;
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}

// setNodeDisabledFilter 切换「启用状态」四态：'' 全部 / 'enabled' 只看启用 /
// 'disabled_health' 只看禁用（健康·可自愈）/ 'disabled_manual' 只看禁用（需人工恢复）。
// 只影响当前视图，不重新请求后端。
function setNodeDisabledFilter(v) {
  nodeDisabledFilter = v || '';
  curNodePage = 1;
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}

// setProxyOnlyLocked 切换前置代理池的「只看防删节点」：同样只影响当前视图。
function setProxyOnlyLocked(on) {
  proxyOnlyLocked = !!on;
  renderProxyNodes(window.lastProxyNodes || [], window.lastProxyHealth || {});
}

function toggleNodeSortDir() {
  nodeStatSortDesc = !nodeStatSortDesc;
  var b = document.getElementById('btnNodeSortDir');
  if (b) b.textContent = nodeStatSortDesc ? '降序' : '升序';
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}

// sortNodesByRTRank 点击「rT」表头按档位排序：首次点击默认降序（异常在前，排障直觉），
// 再点反向；同步排序下拉与 aria-sort。同一键再次点击即翻方向。
function sortNodesByRTRank() {
  if (nodeStatSortKey === 'rt_rank') {
    nodeStatSortDesc = !nodeStatSortDesc;
  } else {
    nodeStatSortKey = 'rt_rank';
    nodeStatSortDesc = true;
  }
  curNodePage = 1;
  var sel = document.getElementById('nodeSortSelect');
  if (sel) sel.value = 'rt_rank';
  var th = document.getElementById('nodeThRTRank');
  if (th) th.setAttribute('aria-sort', nodeStatSortDesc ? 'descending' : 'ascending');
  var arrow = document.getElementById('nodeRTRankArrow');
  if (arrow) arrow.textContent = nodeStatSortDesc ? ' ↓' : ' ↑';
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}

function changeNodePage(p) {
  if (p < 1) p = 1;
  if (p > totalNodePages) p = totalNodePages;
  curNodePage = p;
  loadNodes();
}

function updateSelectHeaderAndBanner() {
  var mainCb = document.getElementById('selectAllNodesCheckbox');
  var banner = document.getElementById('crossPageSelectBanner');
  var bannerText = document.getElementById('crossPageSelectText');
  var bannerTotal = document.getElementById('crossPageSelectTotal');

  if (!cachedNodesList.length) {
    if (mainCb) mainCb.checked = false;
    if (banner) banner.style.display = 'none';
    return;
  }

  var startIdx = (curNodePage - 1) * nodePageSize;
  var endIdx = Math.min(startIdx + nodePageSize, cachedNodesList.length);
  var pageNodes = cachedNodesList.slice(startIdx, endIdx);

  var allPageChecked = pageNodes.length > 0 && pageNodes.every(function (n) { return window.selectedNodeURIs.has(n.raw_uri); });
  if (mainCb) mainCb.checked = allPageChecked;

  if (allPageChecked && cachedNodesList.length > pageNodes.length && window.selectedNodeURIs.size < cachedNodesList.length) {
    if (banner) {
      banner.style.display = 'block';
      if (bannerText) bannerText.textContent = '当前已选择本页 ' + pageNodes.length + ' 个节点。';
      if (bannerTotal) bannerTotal.textContent = cachedNodesList.length;
    }
  } else {
    if (banner) banner.style.display = 'none';
  }
  updateSelCount();
}

function selectAllNodesAcrossPages() {
  cachedNodesList.forEach(function (n) { window.selectedNodeURIs.add(n.raw_uri); });
  var cbs = document.querySelectorAll('.node-select-cb');
  cbs.forEach(function (cb) { cb.checked = true; });
  var banner = document.getElementById('crossPageSelectBanner');
  if (banner) banner.style.display = 'none';
  var mainCb = document.getElementById('selectAllNodesCheckbox');
  if (mainCb) mainCb.checked = true;
  toast('已选择全部 ' + window.selectedNodeURIs.size + ' 个节点');
}

async function loadNodes() {
  const results = await Promise.allSettled([
    API.settings.get(),
    API.proxyNodes.list(),
    API.nodes.list(),
    API.nodes.testProgress(),
    API.nodes.stats()
  ]);

  const sd = results[0].status === 'fulfilled' ? results[0].value : null;
  if (sd && typeof curSettings !== 'undefined') {
    curSettings = sd.settings || sd;
  }

  const pd = results[1].status === 'fulfilled' ? results[1].value : null;
  if (pd) renderProxyNodes(pd.nodes || [], pd.health || {});

  updateSortBtnLabel();

  const d = results[2].status === 'fulfilled' ? results[2].value : null;
  const nodes = d && d.nodes ? d.nodes : [];
  const health = d && d.health ? d.health : {};
  cachedNodesList = nodes;
  // rt_rank 由后端 RTRanks 按后端判据（rtRankOf）统一给出：筛选与排序消费该
  // 权威值，前端不再自算（自算需复制 rtSlowMsThreshold 等阈值，极易漂移）。
  window.lastRTRankMap = d && d.rt_rank ? d.rt_rank : {};
  // global_fault：命中时后端已暂停该类别的自动禁用，前端据实提示（否则用户只会看到
  // "节点没被禁用"却不知道为什么）。
  renderGlobalFaultBanner(d && d.global_fault ? d.global_fault : {});

  const statD = results[4] && results[4].status === 'fulfilled' ? results[4].value : null;
  window.nodeStatsMap = statD && statD.stats ? statD.stats : {};

  const prog = results[3].status === 'fulfilled' ? results[3].value : null;
  if (prog && prog.running) {
    showTestProgressUI(prog);
    startTestProgressPolling();
  } else if (!testProgressTimer) {
    const progressEl = document.getElementById('testProgress');
    if (progressEl) progressEl.style.display = 'none';
  }

  renderNodesTable(nodes, health);
}

function renderNodesTable(nodes, health) {
  window.lastNodesList = nodes;
  window.lastHealthMap = health;
  const enabledCount = nodes.filter(n => !n.disabled).length;
  const disabledCount = nodes.filter(n => n.disabled).length;
  // 禁用细分计数（健康=测速成功自动恢复 / 人工=需手动启用），与「启用状态」四态筛选对应。
  const healthDisabledCount = nodes.filter(n => n.disabled && disabledByHealthOf(n.raw_uri)).length;

  // 顺序：启用状态/防删/rT/搜索过滤 → 统计排序 → 分页。选择与分页都以筛选后的视图为准。
  const view = sortedNodes(filteredNodes(nodes));
  cachedNodesList = view;
  document.getElementById('nodesSummary').textContent = '\u5F53\u524D\u5171 ' + nodes.length + ' \u4E2A\u8282\u70B9\uFF08\u542F\u7528 ' + enabledCount + ' / \u7981\u7528 ' + disabledCount + (disabledCount > 0 ? '\uFF1A\u5065\u5EB7 ' + healthDisabledCount + ' \u00B7 \u9700\u4EBA\u5DE5\u6062\u590D ' + (disabledCount - healthDisabledCount) : '') + '\uFF09' + (view.length !== nodes.length ? ' \u00B7 \u7B5B\u9009\u540E ' + view.length + ' \u4E2A' : '');

  totalNodePages = Math.max(1, Math.ceil(view.length / nodePageSize));
  if (curNodePage > totalNodePages) curNodePage = totalNodePages;

  const startIdx = (curNodePage - 1) * nodePageSize;
  const endIdx = Math.min(startIdx + nodePageSize, view.length);
  const pageNodes = view.slice(startIdx, endIdx);

  const tbody = document.getElementById('nodesBody');
  const frag = document.createDocumentFragment();

  if (pageNodes.length === 0) {
    var tr = document.createElement('tr');
    var td = document.createElement('td');
    td.colSpan = 8;
    td.style.cssText = 'color:var(--text-dim); text-align:center;';
    td.textContent = nodes.length === 0 ? '\u6682\u65E0\u8282\u70B9' : '\u6CA1\u6709\u7B26\u5408\u7B5B\u9009\u6761\u4EF6\u7684\u8282\u70B9';
    tr.appendChild(td);
    frag.appendChild(tr);
  } else {
    for (var i = 0; i < pageNodes.length; i++) {
      var n = pageNodes[i];
      var tr = document.createElement('tr');

      var cbTd = document.createElement('td');
      cbTd.dataset.label = '选择节点';
      cbTd.style.cssText = 'text-align:center;vertical-align:middle;';
      var cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.className = 'node-select-cb';
      cb.dataset.uri = n.raw_uri;
      cb.checked = window.selectedNodeURIs.has(n.raw_uri);
      cb.setAttribute('aria-label', '选择节点 ' + n.name);
      cb.onchange = function () {
        if (this.checked) window.selectedNodeURIs.add(this.dataset.uri);
        else window.selectedNodeURIs.delete(this.dataset.uri);
        updateSelectHeaderAndBanner();
      };
      cbTd.appendChild(cb);
      tr.appendChild(cbTd);

      var nameTd = document.createElement('td');
      nameTd.dataset.label = '节点名称';
      var nameDiv = document.createElement('div');
      nameDiv.style.cssText = 'font-weight:600;font-size:13.5px;color:var(--text);';
      // 名称文本包一层截断 span（.node-cell-name 省略号），badge 在 span 之外
      // 独立换行展示——nowrap 若作用于整个 nameDiv 会把徽章挤出。
      var nameSpan = document.createElement('span');
      nameSpan.className = 'node-cell-name';
      nameSpan.textContent = n.name;
      nameSpan.title = n.name || '';
      nameDiv.appendChild(nameSpan);
      var isLocked = n.raw_uri === curSettings.active_node_uri;
      if (isLocked) {
        var badge = document.createElement('span');
        badge.className = 'pill on';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;';
        badge.textContent = '\u9501\u5B9A\u4F7F\u7528\u4E2D';
        nameDiv.appendChild(badge);
      } else if (n.disabled) {
        var badge = document.createElement('span');
        badge.className = 'pill off';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(236,138,124,0.16);color:var(--red);';
        // 禁用来源区分（优先读节点自带的 disable_source，缺失时回退健康映射）：
        // health 可被自动探测恢复；permanent 是自动探测判定的持续不可恢复（只能人工启用）；
        // admin_test / unsupported 是测试与解析路径直接禁用（同样只能人工启用）。
        var die = disableInfoOf(n);
        badge.textContent = die.label;
        badge.title = die.title;
        nameDiv.appendChild(badge);
      } else {
        var badge = document.createElement('span');
        badge.className = 'pill off';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(143,208,232,0.15);color:var(--blue);';
        badge.textContent = '\u5019\u9009';
        nameDiv.appendChild(badge);
      }
      if (n.locked) {
        var lockBadge = document.createElement('span');
        lockBadge.className = 'pill on';
        lockBadge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(212,175,55,0.18);color:var(--gold);';
        lockBadge.textContent = '\uD83D\uDD12 \u9632\u5220';
        nameDiv.appendChild(lockBadge);
      }
      nameTd.appendChild(nameDiv);

      var serverInfo = serverInfoFor(n.raw_uri);

      var addrDiv = document.createElement('div');
      addrDiv.style.cssText = 'font-size:11px;color:var(--text-dim);margin-top:4px;';
      addrDiv.appendChild(document.createTextNode('\u5730\u5740: '));
      var code = document.createElement('code');
      code.className = 'node-cell-addr';
      code.style.cssText = 'font-size:11px;background:rgba(0,0,0,0.25);color:var(--blue);padding:1px 4px;border-radius:4px;';
      code.textContent = serverInfo;
      code.title = serverInfo || '';
      addrDiv.appendChild(code);
      nameTd.appendChild(addrDiv);
      tr.appendChild(nameTd);

      var typeTd = document.createElement('td');
      typeTd.dataset.label = '协议类型';
      var typeCode = document.createElement('code');
      typeCode.textContent = _tmap[n.type.toLowerCase()] || n.type.toUpperCase();
      typeTd.appendChild(typeCode);
      tr.appendChild(typeTd);

      // 请求级统计列（成功次数/成功率）与竞速统计列（Racing 成功/成功率）。
      var st = window.nodeStatsMap[n.raw_uri] || null;
      var successTd = document.createElement('td');
      successTd.dataset.label = '成功/成功率';
      if (st && st.requests) {
        successTd.textContent = (st.success || 0) + ' / ' + Math.round((st.success || 0) / st.requests * 100) + '%';
      } else {
        successTd.className = 'desc';
        successTd.textContent = '-';
      }
      tr.appendChild(successTd);

      var raceTd = document.createElement('td');
      raceTd.dataset.label = 'Racing 成功/成功率';
      if (st && st.attempts) {
        var raceGood = (st.wins || 0) + (st.collects || 0);
        // 成功率分母扣除 429 限流（rate_limits）：429 是上游正常竞争限流而非节点失败，
        // 计入分母会凭空拉低节点成功率。老数据无该字段按 0 处理。
        var effAtt = st.attempts - (st.rate_limits || 0);
        raceTd.textContent = (st.wins || 0) + ' / ' + (effAtt > 0 ? Math.round(raceGood / effAtt * 100) : 0) + '%';
      } else {
        raceTd.className = 'desc';
        raceTd.textContent = '-';
      }
      tr.appendChild(raceTd);

      var statusTd = document.createElement('td');
      statusTd.dataset.label = '状态/延迟';
      var healthEntry = health[n.raw_uri];
      var nowSec = Math.floor(Date.now() / 1000);
      if (!healthEntry || (!healthEntry.last_success_at && !healthEntry.last_fail_at)) {
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(195,182,164,0.15);color:var(--text-dim);';
        pill.textContent = '\u672A\u6D4B\u8BD5';
        statusTd.appendChild(pill);
      } else if ((healthEntry.cooldown_until || 0) > nowSec) {
        // 冷却态分支：冷却中的节点 consecutive_failures 可能已清零也可能没有，
        // 此前会落入下面的红色"测试失败"——与前置代理表（有独立冷却分支）不一致。
        // 冷却是 429 短时退避，节点本身未被禁用。
        var coolLeft = (healthEntry.cooldown_until || 0) - nowSec;
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(230,200,120,0.16);color:var(--gold);margin-right:5px;';
        pill.textContent = '❄ 冷却 ' + coolLeft + 's';
        pill.title = '429 限流短时退避中（约 ' + coolLeft + 's 后恢复参与调度），节点未被禁用';
        statusTd.appendChild(pill);
        var afterCool = document.createElement('span');
        afterCool.style.cssText = 'color:var(--gold);font-weight:600;font-size:11px;';
        afterCool.textContent = '暂歇';
        statusTd.appendChild(afterCool);
      } else if (healthEntry.consecutive_failures === 0 &&
        (healthEntry.last_429_at || 0) > (healthEntry.last_success_at || 0)) {
        // 429 待复测中间态：429 冷却已过期但之后尚无任何成功（请求成功/测速成功
        // 都会清 last_429_at）。此前该场景会显示绿色"测试通过/可用"，与真实
        // 调度层级（可能仍在被降权观察）不符；节点未被禁用，仅提示尚未复测。
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(143,208,232,0.15);color:var(--blue);margin-right:5px;';
        pill.textContent = '429 待复测';
        pill.title = '近期发生过 429 限流且此后尚无成功请求；节点未被禁用，正常竞争限流不算失败';
        statusTd.appendChild(pill);
        var avail = document.createElement('span');
        avail.style.cssText = 'color:var(--text-dim);font-weight:600;font-size:11px;';
        avail.textContent = '可用';
        statusTd.appendChild(avail);
      } else if (healthEntry.consecutive_failures === 0) {
        var ms = healthEntry.last_test_ms ? Math.round(healthEntry.last_test_ms) + 'ms' : '';
        var pill = document.createElement('span');
        pill.className = 'pill on';
        pill.style.cssText = 'background:rgba(132,214,160,0.16);color:var(--green);margin-right:5px;';
        pill.textContent = '\u6D4B\u8BD5\u901A\u8FC7 ' + ms;
        statusTd.appendChild(pill);
        var avail = document.createElement('span');
        avail.style.cssText = 'color:var(--green);font-weight:600;font-size:11px;';
        avail.textContent = '\u53EF\u7528';
        statusTd.appendChild(avail);
      } else {
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(236,138,124,0.16);color:var(--red);margin-right:5px;';
        // 连败次数可见：管理员需要区分"刚失败一次"与"即将被自动禁用"。阈值文案必须读实际
        // 配置值——此前硬编码"阈值 3 次"，而可配置阈值当前默认是 2，文案与实际判据脱节。
        var fails = healthEntry.consecutive_failures || 0;
        var thr = (typeof curSettings !== 'undefined' && curSettings && curSettings.health_probe_fail_threshold) || 2;
        pill.textContent = '测试失败 ×' + fails;
        pill.title = '连续失败 ' + fails + ' 次；达到阈值 ' + thr +
            ' 次可归因失败会被自动摘出（按「处理策略」表：DNS 只记录、403/网络类默认摘出并进入自动探测）';
        statusTd.appendChild(pill);

        // 失败类别徽章：让"到底在报什么错"一眼可见（完整原文见「故障记录」）。
        if (healthEntry.last_failure_class) {
          var clsSpan = document.createElement('span');
          clsSpan.className = 'node-fault-class';
          clsSpan.textContent = faultClassLabelOf(healthEntry.last_failure_class) +
            (healthEntry.last_http_status ? ' ' + healthEntry.last_http_status : '');
          statusTd.appendChild(clsSpan);
        }

        if (healthEntry.last_test_error) {
          var errSpan = document.createElement('div');
          errSpan.className = 'node-err-msg';
          errSpan.textContent = healthEntry.last_test_error;
          statusTd.appendChild(errSpan);
        }
        if (healthEntry.last_test_at) {
          var atSpan = document.createElement('div');
          atSpan.style.cssText = 'color:var(--text-dim);font-size:11px;';
          atSpan.textContent = '最近检查 ' + faultRelTime(healthEntry.last_test_at);
          atSpan.title = faultAbsTime(healthEntry.last_test_at);
          statusTd.appendChild(atSpan);
        }
      }
      // 在途请求数（in_flight 由后端导出，LoadHealth 深拷贝时 atomic 读取）：>0 才显示，
      // 反映该节点当前正在承载的候选数，排查热点时用。
      var inFlight = (healthEntry && healthEntry.in_flight) || 0;
      if (inFlight > 0) {
        var inflightSpan = document.createElement('div');
        inflightSpan.style.cssText = 'color:var(--text-dim);font-size:11px;';
        inflightSpan.textContent = '\u5728\u9014 ' + inFlight;
        inflightSpan.title = '\u8BE5\u8282\u70B9\u5F53\u524D\u6B63\u5728\u627F\u8F7D\u7684\u7ADE\u901F\u5019\u9009\u6570';
        statusTd.appendChild(inflightSpan);
      }
      tr.appendChild(statusTd);

      // rT 列：rT 抓取健康度。只影响 **rT 抓取选点**，不影响该节点对普通请求的可用性
      // （rT 失败不再禁用节点）；档位徽章来自后端 rt_rank（rtRankOf 判据），
      // 展示文案优先级：rT 冷却中 > 连败 > 最近耗时 > 无记录。
      var rtTd = document.createElement('td');
      rtTd.dataset.label = 'rT 档位';
      var rtNow = Math.floor(Date.now() / 1000);
      var rtCool = (healthEntry && healthEntry.rt_cooldown_until) || 0;
      var rtStreak = (healthEntry && healthEntry.token_fail_streak) || 0;
      var rtMs = (healthEntry && healthEntry.rt_last_ms) || 0;
      var rtRank = (window.lastRTRankMap && window.lastRTRankMap[n.raw_uri]) || 0;
      var rtText = '-';
      var rtColor = 'var(--text-dim)';
      var rtTitle = '暂无 rT 抓取记录';
      if (rtCool > rtNow) {
        rtText = '⏸ 冷却 ' + (rtCool - rtNow) + 's';
        rtColor = 'var(--gold)';
        rtTitle = 'rT 抓取冷却中（仅 rT 选点避让，不禁用节点）';
      } else if (rtStreak > 0) {
        rtText = 'rT ✗' + rtStreak;
        rtColor = 'var(--gold)';
        rtTitle = 'rT 抓取连续失败 ' + rtStreak + ' 次（仅 rT 选点降权，不禁用节点）';
      } else if (rtMs > 0) {
        rtText = rtMs >= 1000 ? (rtMs / 1000).toFixed(2) + 's' : rtMs + 'ms';
        if (rtMs > 5000) { rtColor = 'var(--gold)'; }
        rtTitle = '最近一次 rT 抓取耗时 ' + rtMs + 'ms';
      }
      // 档位徽章：与后端 rt_rank 一致的三态（0 绿 / 1 黄 / 2 红），排序数据同源。
      var rankBadge = document.createElement('span');
      rankBadge.style.cssText = 'display:inline-block;margin-right:5px;padding:0 6px;border-radius:8px;'
        + 'font-size:11px;font-weight:600;';
      if (rtRank === 2) {
        rankBadge.style.background = 'rgba(236,138,124,0.16)';
        rankBadge.style.color = 'var(--red)';
        rankBadge.textContent = '档2';
        rankBadge.title = 'rT 专属冷却中（rt_rank=2）';
      } else if (rtRank === 1) {
        rankBadge.style.background = 'rgba(230,200,120,0.16)';
        rankBadge.style.color = 'var(--gold)';
        rankBadge.textContent = '档1';
        rankBadge.title = 'rT 偏差：连败/挂住或最近抓取超慢（rt_rank=1）';
      } else {
        rankBadge.style.background = 'rgba(132,214,160,0.16)';
        rankBadge.style.color = 'var(--green)';
        rankBadge.textContent = '档0';
        rankBadge.title = 'rT 健康（rt_rank=0）';
      }
      rtTd.appendChild(rankBadge);
      var rtSpan = document.createElement('span');
      rtSpan.style.cssText = 'font-size:12px;color:' + rtColor + ';';
      rtSpan.title = rtTitle;
      rtSpan.textContent = rtText;
      rtTd.appendChild(rtSpan);
      tr.appendChild(rtTd);

      var actionTd = document.createElement('td');
      actionTd.dataset.label = '管理操作';
      actionTd.style.cssText = 'text-align:right;white-space:nowrap;';
      var testBtn = document.createElement('button');
      testBtn.className = 'btn ghost';
      testBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;';
      testBtn.dataset.action = 'test-node';
      testBtn.dataset.uri = n.raw_uri;
      testBtn.textContent = '\u6D4B\u8BD5';
      actionTd.appendChild(testBtn);
      if (n.disabled) {
        var enableBtn = document.createElement('button');
        enableBtn.className = 'btn ghost';
        enableBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--green);';
        enableBtn.dataset.action = 'enable-node';
        enableBtn.dataset.uri = n.raw_uri;
        enableBtn.textContent = '\u542F\u7528';
        actionTd.appendChild(enableBtn);
      } else if (!n.locked) {
        // 行内禁用按钮：仅对启用且未加防删锁的节点渲染——后端 batch-disable 会
        // 跳过防删节点，渲染出来只会静默无效；防删节点需先解除防删。
        var disableBtn = document.createElement('button');
        disableBtn.className = 'btn ghost';
        disableBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--red);';
        disableBtn.dataset.action = 'disable-node';
        disableBtn.dataset.uri = n.raw_uri;
        disableBtn.textContent = '\u7981\u7528';
        actionTd.appendChild(disableBtn);
      }
      if (isLocked) {
        var unuseBtn = document.createElement('button');
        unuseBtn.className = 'btn ghost';
        unuseBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--gold);';
        unuseBtn.dataset.action = 'unuse-node';
        unuseBtn.dataset.uri = n.raw_uri;
        unuseBtn.textContent = '取消锁定';
        actionTd.appendChild(unuseBtn);
      } else {
        var useBtn = document.createElement('button');
        useBtn.className = 'btn ghost';
        useBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;';
        useBtn.dataset.action = 'use-node';
        useBtn.dataset.uri = n.raw_uri;
        useBtn.textContent = '\u9501\u5B9A\u4F7F\u7528';
        actionTd.appendChild(useBtn);
      }
      var lockBtn = document.createElement('button');
      lockBtn.className = 'btn ghost';
      lockBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--gold);';
      lockBtn.dataset.action = 'lock-node';
      lockBtn.dataset.uri = n.raw_uri;
      lockBtn.dataset.locked = n.locked ? '1' : '0';
      lockBtn.textContent = n.locked ? '\u89E3\u9664\u9632\u5220' : '\u9632\u5220';
      actionTd.appendChild(lockBtn);
      var delBtn = document.createElement('button');
      delBtn.className = 'btn danger';
      delBtn.style.cssText = 'padding:4px 10px;font-size:12px;';
      delBtn.dataset.action = 'delete-node';
      delBtn.dataset.uri = n.raw_uri;
      delBtn.textContent = '\u5220\u9664';
      actionTd.appendChild(delBtn);
      tr.appendChild(actionTd);
      frag.appendChild(tr);
    }
  }

  tbody.textContent = '';
  tbody.appendChild(frag);

  const pageNumDisplay = document.getElementById('nodesPageNumDisplay');
  if (pageNumDisplay) pageNumDisplay.textContent = curNodePage + ' / ' + totalNodePages;
  const pageInfo = document.getElementById('nodesPaginationInfo');
  if (pageInfo) pageInfo.textContent = view.length > 0 ? ('显示第 ' + (startIdx + 1) + ' - ' + endIdx + ' 条，共 ' + view.length + ' 条') : '共 0 条';

  const btnFirst = document.getElementById('btnPageFirst');
  const btnPrev = document.getElementById('btnPagePrev');
  const btnNext = document.getElementById('btnPageNext');
  const btnLast = document.getElementById('btnPageLast');
  if (btnFirst) btnFirst.disabled = curNodePage <= 1;
  if (btnPrev) btnPrev.disabled = curNodePage <= 1;
  if (btnNext) btnNext.disabled = curNodePage >= totalNodePages;
  if (btnLast) btnLast.disabled = curNodePage >= totalNodePages;

  updateSelectHeaderAndBanner();
}
let currentTestPaused = false;
function showTestProgressUI(prog) {
  const progressEl = document.getElementById('testProgress');
  const progressText = document.getElementById('testProgressText');
  const progressFill = document.getElementById('testProgressFill');
  const progressDetail = document.getElementById('testProgressDetail');
  const btnPause = document.getElementById('btnTestPauseResume');
  if (!progressEl) return;
  progressEl.style.display = 'block';
  currentTestPaused = !!prog.paused;
  if (btnPause) {
    btnPause.textContent = currentTestPaused ? '恢复' : '暂停';
    btnPause.className = 'btn ghost';
  }
  if (currentTestPaused && testProgressTimer) {
    clearInterval(testProgressTimer);
    testProgressTimer = null;
  }
  const done = prog.done || 0;
  const total = prog.total || 1;
  const ok = prog.ok_count || 0;
  const failed = prog.fail_count || 0;
  progressFill.style.width = Math.round(done / total * 100) + '%';
  const statusStr = currentTestPaused ? '已暂停' : '测试中';
  progressText.textContent = statusStr + ' ' + done + '/' + total + ' \u00B7 \u901A\u8FC7 ' + ok + ' \u00B7 \u5931\u8D25 ' + failed;
  progressDetail.textContent = '当前状态: ' + (prog.current_node || '');
}
async function toggleTestPauseResume() {
  try {
    if (currentTestPaused) {
      await API.nodes.testResume();
      currentTestPaused = false;
      const btnPause = document.getElementById('btnTestPauseResume');
      if (btnPause) {
        btnPause.textContent = '暂停';
        btnPause.className = 'btn ghost';
      }
      const progressText = document.getElementById('testProgressText');
      if (progressText && progressText.textContent.startsWith('已暂停')) {
        progressText.textContent = progressText.textContent.replace(/^已暂停/, '测试中');
      }
      startTestProgressPolling();
      toast('已恢复批量测速');
    } else {
      await API.nodes.testPause();
      currentTestPaused = true;
      if (testProgressTimer) {
        clearInterval(testProgressTimer);
        testProgressTimer = null;
      }
      const btnPause = document.getElementById('btnTestPauseResume');
      if (btnPause) {
        btnPause.textContent = '恢复';
        btnPause.className = 'btn ghost';
      }
      const progressText = document.getElementById('testProgressText');
      if (progressText && progressText.textContent.startsWith('测试中')) {
        progressText.textContent = progressText.textContent.replace(/^测试中/, '已暂停');
      }
      toast('批量测速已暂停');
    }
  } catch (e) {
    toast(e.message || '操作失败');
  }
}
function startTestProgressPolling() {
  if (testProgressTimer) return;
  testProgressTimer = setInterval(async function () {
    try {
      const prog = await API.nodes.testProgress();
      if (prog && prog.running) {
        showTestProgressUI(prog);
      } else {
        clearInterval(testProgressTimer);
        testProgressTimer = null;
        const progressEl = document.getElementById('testProgress');
        if (progressEl) progressEl.style.display = 'none';
        toast('全局批量测速结束！');
        loadNodes();
      }
    } catch (e) { }
  }, 1000);
}
function updateSortBtnLabel() {
  const b = document.getElementById('btnSortByLatency');
  if (b) b.textContent = window.nodeSortDesc ? '按延迟降序排序' : '按延迟升序排序';
}
function getSelectedNodeURIs() {
  return Array.from(window.selectedNodeURIs);
}
// toggleSelectAllNodes「全选/反选」：作用于**当前筛选后的视图**（cachedNodesList 即
// filteredNodes+排序的结果）。此前用 size === length 判断，跨页子集选中时点击会
// 全量选中而非反选，与按钮文案不符。改为对视图内每个 URI 求补。
function toggleSelectAllNodes() {
  if (!cachedNodesList.length) return;
  var allSelected = cachedNodesList.every(function (n) { return window.selectedNodeURIs.has(n.raw_uri); });
  cachedNodesList.forEach(function (n) {
    if (allSelected) window.selectedNodeURIs.delete(n.raw_uri);
    else window.selectedNodeURIs.add(n.raw_uri);
  });
  updateNodeSelectionUI();
}
function toggleSelectAllNodesCheckbox(mainCb) {
  const startIdx = (curNodePage - 1) * nodePageSize;
  const endIdx = Math.min(startIdx + nodePageSize, cachedNodesList.length);
  const pageNodes = cachedNodesList.slice(startIdx, endIdx);

  pageNodes.forEach(function (n) {
    if (mainCb.checked) window.selectedNodeURIs.add(n.raw_uri);
    else window.selectedNodeURIs.delete(n.raw_uri);
  });
  updateNodeSelectionUI();
}

// updateNodeSelectionUI 纯本地刷新选择状态（行 checkbox、表头主 checkbox、跨页
// banner、选中计数）。此前这两个入口各自调 loadNodes()——为刷新一个 checkbox
// 重发 5 个并发请求并整页重建 DOM，滚动位置与焦点丢失。
function updateNodeSelectionUI() {
  var boxes = document.querySelectorAll('#nodesBody input[type="checkbox"][data-uri]');
  for (var i = 0; i < boxes.length; i++) {
    boxes[i].checked = window.selectedNodeURIs.has(boxes[i].dataset.uri);
  }
  var startIdx = (curNodePage - 1) * nodePageSize;
  var pageNodes = cachedNodesList.slice(startIdx, Math.min(startIdx + nodePageSize, cachedNodesList.length));
  var mainCb = document.getElementById('selectAllNodesCheckbox');
  if (mainCb && pageNodes.length) {
    mainCb.checked = pageNodes.every(function (n) { return window.selectedNodeURIs.has(n.raw_uri); });
  }
  updateSelectHeaderAndBanner();
  updateSelCount();
}

// updateSelCount 常驻选中计数：批量操作前先看清自己选了多少个。
function updateSelCount() {
  var el = document.getElementById('selCount');
  if (el) el.textContent = String(window.selectedNodeURIs.size || 0);
}
function renderProxyNodes(nodes, health) {
  window.lastProxyNodes = nodes || [];
  window.lastProxyHealth = health || {};
  var view = proxyOnlyLocked
    ? window.lastProxyNodes.filter(function (n) { return !!n.locked; })
    : window.lastProxyNodes;
  const tbody = document.getElementById('proxyNodesBody');
  const frag = document.createDocumentFragment();

  if (!view.length) {
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    td.colSpan = 4;
    td.style.cssText = 'color:var(--text-dim);text-align:center;';
    td.textContent = window.lastProxyNodes.length
      ? '没有符合筛选条件的前置代理节点'
      : '暂无前置代理节点（可在此导入节点后自动启用为第一跳 SOCKS5 通道）';
    tr.appendChild(td);
    frag.appendChild(tr);
  } else {
    for (const n of view) {
      const h = health[n.raw_uri] || {};
      const tr = document.createElement('tr');

      const nameTd = document.createElement('td');
      const nameDiv = document.createElement('div');
      nameDiv.style.cssText = 'font-weight:600;font-size:13.5px;color:var(--text);';
      nameDiv.textContent = n.name || n.raw_uri;
      if (n.disabled) {
        const badge = document.createElement('span');
        badge.className = 'pill off';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;';
        badge.textContent = '已禁用';
        nameDiv.appendChild(badge);
      }
      if (n.locked) {
        const lockBadge = document.createElement('span');
        lockBadge.className = 'pill on';
        lockBadge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(212,175,55,0.18);color:var(--gold);';
        lockBadge.textContent = '🔒 防删';
        nameDiv.appendChild(lockBadge);
      }
      nameTd.appendChild(nameDiv);
      tr.appendChild(nameTd);

      const typeTd = document.createElement('td');
      const typeCode = document.createElement('code');
      typeCode.textContent = (n.type || '').toUpperCase();
      typeTd.appendChild(typeCode);
      tr.appendChild(typeTd);

      const statusTd = document.createElement('td');
      if (h.cooldown_until > Math.floor(Date.now() / 1000)) {
        const pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(236,138,124,0.16);color:var(--gold);';
        pill.textContent = '冷却中';
        statusTd.appendChild(pill);
      } else if ((h.last_success_at || 0) > (h.last_fail_at || 0)) {
        const pill = document.createElement('span');
        pill.className = 'pill on';
        pill.style.cssText = 'background:rgba(132,214,160,0.16);color:var(--green);';
        const ms = h.last_test_ms ? Math.round(h.last_test_ms) + 'ms' : '';
        pill.textContent = '测试通过 ' + ms;
        statusTd.appendChild(pill);
      } else if (h.last_fail_at > 0) {
        const pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(236,138,124,0.16);color:var(--red);margin-right:5px;';
        pill.textContent = '测试失败';
        statusTd.appendChild(pill);
        if (h.last_test_error) {
          const errSpan = document.createElement('div');
          errSpan.style.cssText = 'font-size:11px;color:var(--red);margin-top:2px;';
          errSpan.textContent = h.last_test_error;
          statusTd.appendChild(errSpan);
        }
      } else {
        const pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(195,182,164,0.15);color:var(--text-dim);';
        pill.textContent = '未测试';
        statusTd.appendChild(pill);
      }
      tr.appendChild(statusTd);

      const actionTd = document.createElement('td');
      actionTd.style.cssText = 'text-align:right;white-space:nowrap;';

      const testBtn = document.createElement('button');
      testBtn.className = 'btn ghost';
      testBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;';
      testBtn.textContent = '测试';
      testBtn.onclick = function () { testProxyNode(n.raw_uri); };
      actionTd.appendChild(testBtn);

      const toggleBtn = document.createElement('button');
      toggleBtn.className = 'btn ghost';
      toggleBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;' + (n.disabled ? 'color:var(--green);' : 'color:var(--gold);');
      toggleBtn.textContent = n.disabled ? '启用' : '禁用';
      toggleBtn.onclick = function () { toggleProxyNode(n.raw_uri, !n.disabled); };
      actionTd.appendChild(toggleBtn);

      const lockBtn = document.createElement('button');
      lockBtn.className = 'btn ghost';
      lockBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--gold);';
      lockBtn.textContent = n.locked ? '解除防删' : '防删';
      lockBtn.onclick = function () { lockProxyNode(n.raw_uri, !n.locked); };
      actionTd.appendChild(lockBtn);

      const delBtn = document.createElement('button');
      delBtn.className = 'btn danger';
      delBtn.style.cssText = 'padding:4px 10px;font-size:12px;';
      delBtn.textContent = '删除';
      delBtn.onclick = function () { deleteProxyNode(n.raw_uri); };
      actionTd.appendChild(delBtn);

      tr.appendChild(actionTd);
      frag.appendChild(tr);
    }
  }

  tbody.textContent = '';
  tbody.appendChild(frag);
}
