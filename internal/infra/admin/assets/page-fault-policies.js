// 故障处理策略视图：按类别配置处理方式与探测失败阈值。
//
// 设计依据：Issue #5「策略可配置化：实施计划基准」§5：
//   - 至少能查看与调整 故障类别 / 当前处理方式 / 探测失败次数阈值（0 = 无限）/ 当前生效值与默认值；
//   - 必须给出明确文字提示，避免用户误解"单次禁用"与"禁用后恢复探测"的区别；
//   - 前置池第一版不执行自动探测，UI 必须显式提示（不能只写在文档里）。
//
// 这里只做展示与提交，动作语义由后端 PolicySet 解析（单一真源），
// note 文案也由服务端下发，避免前后端各写一份、逐渐漂移。

var policyOptions = [
  { v: 'record_only', label: '不禁用' },
  { v: 'single_shot', label: '单次禁用' },
  { v: 'disable_then_probe', label: '禁用后恢复探测' },
];

var faultPolicyData = null;

// 当前页签（'records' | 'policies' | 'params'）：刷新按钮与页面 loader 都据此只刷当前视图。
var faultTab = 'records';

// loadFaultsPage 是侧栏页面「节点故障策略与记录」的 loader（admin.js LOADERS 注册）。
function loadFaultsPage() {
  return refreshFaultsPage();
}

// refreshFaultsPage 刷新当前页签的数据。
async function refreshFaultsPage() {
  if (faultTab === 'policies') {
    await refreshFaultPolicies();
    return;
  }
  if (faultTab === 'params') {
    await loadFaultParams();
    return;
  }
  await refreshNodeFaults();
}

// ---- 探测与保护参数（原「设置」页的健康相关项已迁入本页，避免跨页操作）----

// 这些参数与「处理策略」强相关：阈值决定"探测失败几次转永久"，
// 自愈参数决定"多久探一次、探几个"，全局保护决定"多节点同因失败时是否暂停摘除"。
// 放在同一页是为了让调整策略与调整参数无需来回切换。
var faultParamsSettings = null;

function faultParamFields() {
  return SETTINGS_FIELDS.filter(function (f) { return f.group === 'fault'; });
}

async function loadFaultParams() {
  var host = $('#faultParamsFields');
  if (host) host.innerHTML = '<div class="desc" style="padding:10px;">加载中...</div>';
  var head = $('#faultParamsHead');
  if (head) {
    head.innerHTML = '这些是自动探测与全局故障保护的运行参数（同类参数已从「设置」页迁入本页）。' +
      '按类别的处理方式在「处理策略」页签配置；此处是<b>全局默认值</b>与探测节奏。';
  }
  try {
    var d = await API.settings.get();
    faultParamsSettings = d.settings || d;
  } catch (e) {
    if (host) host.innerHTML = '<div class="err" style="padding:10px;">加载失败：' + esc(String(e && e.message || e)) + '</div>';
    return;
  }
  renderFaultParams();
}

function renderFaultParams() {
  var host = $('#faultParamsFields');
  if (!host) return;
  var fields = faultParamFields();
  var numFields = fields.filter(function (f) { return f.type !== 'bool'; });
  var boolFields = fields.filter(function (f) { return f.type === 'bool'; });
  var parts = [];
  if (numFields.length) {
    parts.push('<div class="grid grid-2">' + numFields.map(function (f) {
      return settingFieldHTML(f, faultParamsSettings || {}, []);
    }).join('') + '</div>');
  }
  if (boolFields.length) {
    parts.push('<div class="grid grid-2" style="margin-top:10px;">' + boolFields.map(function (f) {
      return settingFieldHTML(f, faultParamsSettings || {}, []);
    }).join('') + '</div>');
  }
  host.innerHTML = parts.join('');
  // 变更即标记未保存，避免改完以为已生效
  host.querySelectorAll('input, select').forEach(function (el) {
    el.addEventListener('input', markFaultParamsDirty);
    el.addEventListener('change', markFaultParamsDirty);
  });
  markFaultParamsDirty(false);
}

function markFaultParamsDirty(dirty) {
  var on = (dirty !== false);
  // 全局标记供 admin.js 的 go() 在离开本页时拦截（同设置页的 hasUnsavedSettings）。
  window.hasUnsavedFaultParams = on;
  var el = $('#faultParamsDirty');
  if (el) el.textContent = on ? '有未保存的修改' : '';
}

async function saveFaultParams() {
  var out = {};
  var fields = faultParamFields();
  for (var i = 0; i < fields.length; i++) {
    var v = collectSettingValue(fields[i]);
    if (v === undefined) continue;
    out[fields[i].k] = v;
  }
  if (Object.keys(out).length === 0) {
    toast('没有可保存的参数');
    return;
  }
  try {
    await API.settings.put(out);
    toast('参数已保存', 'ok');
    markFaultParamsDirty(false);
    // 阈值/保护参数改动会影响「处理策略」页签展示的全局默认值，顺手刷新保持两页一致。
    await refreshFaultPolicies();
  } catch (e) {
    toast('保存失败：' + String(e && e.message || e), 'err');
  }
}

function switchFaultTab(tab) {
  var next = (tab === 'policies' || tab === 'params') ? tab : 'records';
  // 未保存的参数改动不得静默丢失：离开参数页签前二次确认（与设置页同一套交互）。
  if (faultTab === 'params' && next !== 'params' && window.hasUnsavedFaultParams) {
    showConfirm('「探测与保护参数」有未保存的修改，离开将丢失这些更改。确认离开吗？', function () {
      window.hasUnsavedFaultParams = false;
      switchFaultTab(next);
    });
    return;
  }
  faultTab = next;
  var panes = { records: 'faultPaneRecords', policies: 'faultPanePolicies', params: 'faultPaneParams' };
  var tabs = { records: 'faultTabRecords', policies: 'faultTabPolicies', params: 'faultTabParams' };
  Object.keys(panes).forEach(function (k) {
    var pane = $('#' + panes[k]);
    if (pane) pane.classList.toggle('hidden', k !== faultTab);
    var btn = $('#' + tabs[k]);
    if (btn) btn.classList.toggle('active', k === faultTab);
  });
  if (faultTab === 'policies') refreshFaultPolicies();
  else if (faultTab === 'params') loadFaultParams();
}

async function refreshFaultPolicies() {
  var list = $('#policiesList');
  if (list) list.innerHTML = '<tr><td colspan="6" class="desc" style="text-align:center;padding:16px;">加载中...</td></tr>';
  try {
    faultPolicyData = await API.faultPolicies.list();
  } catch (e) {
    if (list) list.innerHTML = '<tr><td colspan="6" class="err" style="text-align:center;padding:16px;">加载失败：' + esc(String(e && e.message || e)) + '</td></tr>';
    return;
  }
  renderFaultPolicies();
}

function renderFaultPolicies() {
  var d = faultPolicyData || { policies: [] };
  var head = $('#policiesHead');
  if (head) {
    head.innerHTML = '按故障类别选择处理方式。默认策略下 <b>节点网络故障 / 403 / 认证失败</b> 会在首次失败时' +
      '移出可用池并进入自动探测；<b>DNS 解析失败</b>与「其他」只记录、不移出。' +
      '自动探测阶段连续失败达阈值即转<b>永久禁用</b>（需手动启用）；阈值为 <b>0</b> 表示无限探测。' +
      '当前全局默认阈值：<b>' + (d.global_probe_fail_threshold != null ? d.global_probe_fail_threshold : '-') + '</b>。';
  }
  var note = $('#policiesEntryNote');
  if (note) {
    if (d.entry_pool_probes === false) {
      note.classList.remove('hidden');
      note.textContent = '前置代理池第一版不执行自动探测：「禁用后恢复探测」在前置池等价于"禁用，靠测速成功或人工启用恢复"。' +
        '前置节点不会像出口节点那样按周期自动回来。';
    } else {
      note.classList.add('hidden');
    }
  }

  var list = $('#policiesList');
  if (!list) return;
  list.innerHTML = '';
  (d.policies || []).forEach(function (p) {
    list.appendChild(buildPolicyRow(p, d));
  });
}

function buildPolicyRow(p, d) {
  var tr = document.createElement('tr');

  function td() { return document.createElement('td'); }

  var classTd = td();
  classTd.innerHTML = '<b>' + esc(faultClassLabel(p.class)) + '</b><div class="desc" style="font-size:11px;">' + esc(p.class) + '</div>';
  tr.appendChild(classTd);

  // 处理方式下拉
  var actionTd = td();
  var sel = document.createElement('select');
  sel.className = 'policy-select';
  policyOptions.forEach(function (o) {
    var opt = document.createElement('option');
    opt.value = o.v;
    opt.textContent = o.label;
    if (o.v === p.action) opt.selected = true;
    sel.appendChild(opt);
  });
  sel.onchange = function () {
    var probing = sel.value === 'disable_then_probe';
    inp.disabled = !probing;
    inp.value = '';
    inp.placeholder = probing ? '跟随全局' : '不适用';
    thrHint.textContent = probing ? ('跟随全局 ' + (d.global_probe_fail_threshold != null ? d.global_probe_fail_threshold : '-'))
      : '不适用（不进入自动探测）';
  };
  actionTd.appendChild(sel);
  tr.appendChild(actionTd);

  // 探测失败阈值（空 = 跟随全局；0 = 无限）
  var thrTd = td();
  var inp = document.createElement('input');
  inp.type = 'number';
  inp.min = '0';
  inp.max = '10';
  inp.className = 'policy-threshold';
  inp.placeholder = '跟随全局';
  inp.value = p.follows_global ? '' : String(p.probe_fail_threshold);
  inp.title = '留空 = 跟随全局默认（' + (d.global_probe_fail_threshold != null ? d.global_probe_fail_threshold : '-') + '）；0 = 无限探测';
  // 只有「禁用后恢复探测」会进入探测阶段，其余动作的阈值无意义——直接禁用输入并改写提示，
  // 避免用户填了数字却看到"跟随全局"而困惑。
  if (p.action !== 'disable_then_probe') {
    inp.disabled = true;
    inp.value = '';
    inp.placeholder = '不适用';
    inp.title = '该处理方式不进入自动探测，因此没有探测失败阈值';
  }
  var thrHint = document.createElement('div');
  thrHint.className = 'desc';
  thrHint.style.fontSize = '11px';
  if (p.action !== 'disable_then_probe') {
    thrHint.textContent = '不适用（不进入自动探测）';
  } else {
    thrHint.textContent = p.follows_global
      ? ('跟随全局 ' + (d.global_probe_fail_threshold != null ? d.global_probe_fail_threshold : '-'))
      : (p.probe_fail_threshold === 0 ? '无限探测' : '连续失败 ' + p.probe_fail_threshold + ' 次转永久');
  }
  thrTd.appendChild(inp);
  thrTd.appendChild(thrHint);
  tr.appendChild(thrTd);

  // 生效来源
  var srcTd = td();
  var badge = document.createElement('span');
  badge.className = 'policy-badge' + (p.custom ? ' custom' : '');
  badge.textContent = p.custom ? '自定义' : '默认';
  badge.title = p.custom ? '该类别的处理方式或阈值已被自定义' : '完全按内置默认策略运行';
  srcTd.appendChild(badge);
  tr.appendChild(srcTd);

  // 默认值
  var defTd = td();
  var defLabel = policyOptions.filter(function (o) { return o.v === p.default_action; });
  defTd.innerHTML = '<span class="desc">' + esc(defLabel.length ? defLabel[0].label : p.default_action) + '</span>';
  tr.appendChild(defTd);

  // 操作：保存 / 还原该类别
  var opTd = td();
  var saveBtn = document.createElement('button');
  saveBtn.className = 'btn ghost';
  saveBtn.style.cssText = 'padding:3px 10px;font-size:12px;';
  saveBtn.textContent = '保存';
  saveBtn.onclick = function () { saveFaultPolicy(p.class, sel.value, inp.value); };
  opTd.appendChild(saveBtn);
  if (p.custom) {
    var resetBtn = document.createElement('button');
    resetBtn.className = 'btn ghost';
    resetBtn.style.cssText = 'padding:3px 10px;font-size:12px;margin-left:6px;';
    resetBtn.textContent = '还原默认';
    resetBtn.onclick = function () { resetFaultPolicyClass(p.class); };
    opTd.appendChild(resetBtn);
  }
  tr.appendChild(opTd);

  // 语义提示行：把"单次禁用"与"禁用后恢复探测"的区别写在用户眼前
  var noteTr = document.createElement('tr');
  var noteTd = document.createElement('td');
  noteTd.colSpan = 6;
  noteTd.className = 'desc';
  noteTd.style.cssText = 'font-size:11px;padding-top:0;';
  noteTd.textContent = p.note || '';
  noteTr.appendChild(noteTd);

  var frag = document.createDocumentFragment();
  frag.appendChild(tr);
  frag.appendChild(noteTr);
  return frag;
}

async function saveFaultPolicy(cls, action, thresholdRaw) {
  var payload = { class: cls, action: action };
  var raw = String(thresholdRaw == null ? '' : thresholdRaw).trim();
  if (raw !== '') {
    var n = Number(raw);
    if (!isFinite(n) || n < 0 || n > 10 || Math.floor(n) !== n) {
      toast('探测失败阈值必须是 0~10 的整数（0 = 无限探测）', 'err');
      return;
    }
    payload.probe_fail_threshold = n; // 显式 0 = 无限探测
  }
  try {
    await API.faultPolicies.save([payload]);
    toast('已保存 ' + faultClassLabel(cls) + ' 的处理策略', 'ok');
    await refreshFaultPolicies();
  } catch (e) {
    toast('保存失败：' + String(e && e.message || e), 'err');
  }
}

async function resetFaultPolicyClass(cls) {
  // 单类别还原 = **删除该类别的自定义行**（稀疏覆盖设计：删行即回落内置默认）。
  // 此前误用 saveFaultPolicy() 把默认动作写回成一条自定义行，结果是"看着恢复了默认、
  // 库里仍留着自定义记录、刷新后又显示自定义"——这里改为调用真正的删除接口。
  try {
    await API.faultPolicies.resetClass(cls);
    toast('已还原 ' + faultClassLabel(cls) + ' 为默认策略', 'ok');
    await refreshFaultPolicies();
  } catch (e) {
    toast('还原失败：' + String(e && e.message || e), 'err');
  }
}

async function resetAllFaultPolicies() {
  if (!confirm('确认把全部故障处理策略还原为默认？已自定义的类别会丢失。')) return;
  try {
    var d = await API.faultPolicies.reset();
    toast('已还原 ' + ((d && d.reset) || 0) + ' 条自定义策略', 'ok');
    await refreshFaultPolicies();
  } catch (e) {
    toast('还原失败：' + String(e && e.message || e), 'err');
  }
}
