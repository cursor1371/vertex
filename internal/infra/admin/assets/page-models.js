// 模型管理页：上游目录获取（差异确认导入/移除）+ 本地模型表格化管理。
//
// 概念分离（与后端契约一致）：
//   - 上游目录：只读快照（获取只写目录快照，绝不改动本地配置）；
//   - 本地已知模型：models（启用=对外暴露）∪ disabled（已停用）；
//   - 模型级开关为三态：explicit 只保存"用户显式设置"的值，未设置=跟随家族默认；
//     保存时只回写 explicit，避免把默认值固化成永久配置。
//
// 四操作语义：导入=加入已知集合（默认停用）→ 启用=移入 models → 停用=移入 disabled
// → 移除=从全部集合与关联配置清理。表格编辑与差异勾选都只做**前端暂存**，
// 点「保存」才落盘（PUT 完整快照）。

const MODEL_FAMILY_LABELS = { text: '文本', image: '图片', audio: '音频', vedio: '视频' };

const MODELS_STATE = {
  loaded: false,
  models: [],
  disabled: [],
  custom: [],
  aliasMap: {},
  familyMap: {},
  explicit: {},
  effective: {},
  meta: {},
  catalog: null,
  diff: null,
  filter: 'all',
  selected: {},
  diffAdd: {},
  diffEnableOnImport: {},
  diffRemove: {},
  lastFetch: null,
};

function modelsStateReset() {
  MODELS_STATE.loaded = false;
  MODELS_STATE.models = [];
  MODELS_STATE.disabled = [];
  MODELS_STATE.custom = [];
  MODELS_STATE.aliasMap = {};
  MODELS_STATE.familyMap = {};
  MODELS_STATE.explicit = {};
  MODELS_STATE.effective = {};
  MODELS_STATE.meta = {};
  MODELS_STATE.catalog = null;
  MODELS_STATE.diff = null;
  MODELS_STATE.filter = 'all';
  MODELS_STATE.selected = {};
  MODELS_STATE.diffAdd = {};
  MODELS_STATE.diffEnableOnImport = {};
  MODELS_STATE.diffRemove = {};
  MODELS_STATE.lastFetch = null;
  window.hasUnsavedModels = false;
}

async function loadModels() {
  modelsStateReset();
  const d = await API.models.get();
  MODELS_STATE.models = (d.models || []).slice();
  MODELS_STATE.disabled = (d.disabled_models || []).slice();
  MODELS_STATE.custom = (d.custom_models || []).slice();
  MODELS_STATE.aliasMap = Object.assign({}, d.alias_map || {});
  MODELS_STATE.familyMap = Object.assign({}, d.family_map || {});
  MODELS_STATE.explicit = Object.assign({}, d.model_options || {});
  MODELS_STATE.effective = Object.assign({}, d.model_options_effective || {});
  MODELS_STATE.meta = Object.assign({}, d.model_meta || {});
  MODELS_STATE.families = d.families || ['text', 'image', 'audio', 'vedio'];
  MODELS_STATE.catalog = d.catalog || null;
  MODELS_STATE.diff = d.diff || null;
  MODELS_STATE.loaded = true;
  window.hasUnsavedModels = false;
  renderModelsPage();
}

// ---------- 渲染 ----------

function renderModelsPage() {
  renderModelsSyncStatus();
  renderModelsDiff();
  renderModelsFamilyTabs();
  renderModelsTable();
  updateModelsDirtyHint();
}

function knownModelNames() {
  return MODELS_STATE.models.concat(MODELS_STATE.disabled.filter(m => !MODELS_STATE.models.includes(m)));
}

function isEnabled(name) { return MODELS_STATE.models.includes(name); }

function modelFamily(name) {
  const meta = MODELS_STATE.meta[name];
  if (meta && meta.family) return meta.family;
  const override = MODELS_STATE.familyMap[name];
  if (override) return override;
  return null;
}

function familyLabel(name) {
  const fam = modelFamily(name);
  return fam ? (MODEL_FAMILY_LABELS[fam] || fam) : '文本（自动）';
}

function modelMetaFor(name) {
  const meta = MODELS_STATE.meta[name];
  if (meta) return meta;
  // 目录条目（尚未导入）：用最近一次获取返回的 entries 元数据
  const entries = (MODELS_STATE.lastFetch && MODELS_STATE.lastFetch.entries) || [];
  const hit = entries.find(e => e.id === name);
  if (hit) return { family: hit.family, supported: hit.supported, unavailable_reason: hit.unavailable_reason, custom: false };
  return { family: null, supported: true, custom: false };
}

function renderModelsSyncStatus() {
  const el = $('#modelsSyncStatus');
  if (!el) return;
  const cat = MODELS_STATE.catalog;
  const lf = MODELS_STATE.lastFetch;
  const parts = [];
  if (cat) {
    parts.push('目录快照：' + esc(cat.count) + ' 个模型，最后成功获取 ' + esc(relTime2(cat.fetched_at)));
    if (cat.stale) {
      parts.push('<span style="color:var(--red,#e06c75);">最近一次获取失败：' + esc(cat.last_error || '未知原因') + '（"已移除"暂不可判定）</span>');
    }
    if (cat.confirmed_at) {
      parts.push('异常快照已由管理员确认接受（' + esc(relTime2(cat.confirmed_at)) + '）');
    }
  } else {
    parts.push('尚未获取过上游目录。点击「获取上游模型」由服务端经节点池拉取（只更新目录快照，不会改动本地配置）。');
  }
  if (lf) {
    if (lf.ok) {
      parts.push('本次：发现 ' + esc(lf.discovered) + ' / 候选 ' + esc(lf.kept) + ' / 跳过 ' + esc(lf.skipped) + '，via ' + esc(lf.via || '-'));
    } else if (lf.error) {
      parts.push('<span style="color:var(--red,#e06c75);">本次获取失败：' + esc(lf.error) + '</span>');
    }
    if (lf.attempts && lf.attempts.length) {
      const failed = lf.attempts.filter(a => a.error).map(a => a.target + ': ' + a.error);
      if (failed.length) parts.push('<span class="desc">节点尝试：' + esc(failed.join('；')) + '</span>');
    }
  }
  el.innerHTML = parts.join('　·　');
}

function renderModelsFamilyTabs() {
  const box = $('#modelsFamilyTabs');
  if (!box) return;
  const tabs = [['all', '全部']].concat(
    (MODELS_STATE.families || ['text', 'image', 'audio', 'vedio']).map(f => [f, MODEL_FAMILY_LABELS[f] || f])
  );
  box.innerHTML = tabs.map(([key, label]) =>
    '<button class="fault-tab' + (MODELS_STATE.filter === key ? ' active' : '') + '" onclick="setModelsFilter(\'' + key + '\')">' + esc(label) + '</button>'
  ).join('');
}

function setModelsFilter(key) {
  MODELS_STATE.filter = key;
  renderModelsFamilyTabs();
  renderModelsTable();
}

function modelsVisibleNames() {
  const q = ($('#modelsSearch') ? $('#modelsSearch').value : '').trim().toLowerCase();
  const filter = MODELS_STATE.filter;
  return knownModelNames().filter(name => {
    if (filter !== 'all' && (modelFamily(name) || 'text') !== filter) return false;
    if (!q) return true;
    if (name.toLowerCase().includes(q)) return true;
    return Object.keys(MODELS_STATE.aliasMap).some(a => MODELS_STATE.aliasMap[a] === name && a.toLowerCase().includes(q));
  });
}

function aliasesOf(name) {
  return Object.keys(MODELS_STATE.aliasMap).filter(a => MODELS_STATE.aliasMap[a] === name);
}

function optionValue(name, key) {
  const eff = MODELS_STATE.effective[name] || {};
  return !!eff[key];
}

function isExplicit(name, key) {
  const exp = MODELS_STATE.explicit[name];
  return !!(exp && typeof exp[key] === 'boolean');
}

function toggleHtml(name, key, title) {
  const meta = modelMetaFor(name);
  const applicable = (meta.family || 'text') === 'text';
  const on = optionValue(name, key);
  const explicit = isExplicit(name, key);
  if (!applicable) {
    return '<span class="desc" title="' + esc(title) + '">—</span>';
  }
  return '<label style="display:inline-flex;align-items:center;gap:6px;cursor:pointer;">' +
    '<input type="checkbox" style="width:auto;"' + (on ? ' checked' : '') +
    ' onchange="setModelOption(\'' + esc(name) + '\',\'' + key + '\',this.checked)" title="' + esc(title) + '">' +
    (explicit ? '<a href="javascript:void(0)" onclick="resetModelOption(\'' + esc(name) + '\',\'' + key + '\')" title="恢复默认（清除显式设置）" style="color:var(--gold);text-decoration:none;">↩</a>' : '') +
    '</label>';
}

function renderModelsTable() {
  const body = $('#modelsBody');
  if (!body) return;
  const names = modelsVisibleNames();
  const hint = $('#modelsTableHint');
  if (hint) {
    const total = knownModelNames().length;
    hint.textContent = '共 ' + total + ' 个本地模型' + (names.length !== total ? '，当前筛选显示 ' + names.length + ' 个' : '') +
      '（「全部」可查看所有家族；表格编辑需点「保存」落盘）';
  }
  body.innerHTML = names.map(name => rowHtml(name)).join('') ||
    '<tr><td colspan="9" class="desc" style="text-align:center;padding:22px;">没有匹配的模型</td></tr>';
  const all = $('#modelsSelectAll');
  if (all) all.checked = names.length > 0 && names.every(n => MODELS_STATE.selected[n]);
}

function rowHtml(name) {
  const enabled = isEnabled(name);
  const meta = modelMetaFor(name);
  const isVideo = (meta.family || '') === 'vedio';
  const isCustom = MODELS_STATE.custom.includes(name) || !!meta.custom;
  const override = MODELS_STATE.familyMap[name] || '';

  const badges = [];
  if (isCustom) badges.push('<span class="policy-badge" title="上游目录未发现（本地自定义）">自定义</span>');
  if (isVideo) badges.push('<span class="policy-badge" style="color:var(--red,#e06c75);" title="' + esc(meta.unavailable_reason || '后端能力尚未实现') + '">目录可见 / 暂不可用</span>');

  const aliasChips = aliasesOf(name).map(a =>
    '<span class="model-dd-tag" style="margin-left:0;">' + esc(a) +
    ' <a href="javascript:void(0)" onclick="removeModelAlias(\'' + esc(a) + '\')" style="color:inherit;">×</a></span>'
  ).join(' ');

  const familyOptions = [['', '自动'], ['text', '文本'], ['image', '图片'], ['audio', '音频'], ['vedio', '视频']]
    .map(([val, label]) => '<option value="' + val + '"' + (override === val ? ' selected' : '') + '>' + label + '</option>').join('');

  const enableToggle = isVideo
    ? '<input type="checkbox" style="width:auto;" disabled title="视频模型暂不支持启用（视频生成 API 尚未实现）">'
    : '<input type="checkbox" style="width:auto;"' + (enabled ? ' checked' : '') +
      ' onchange="setModelEnabled(\'' + esc(name) + '\',this.checked)" title="对客户端 API 暴露">';

  return '<tr>' +
    '<td><input type="checkbox" style="width:auto;"' + (MODELS_STATE.selected[name] ? ' checked' : '') +
    ' onchange="toggleModelSelected(\'' + esc(name) + '\',this.checked)"></td>' +
    '<td><div class="font-mono ov-cell-name" title="' + esc(name) + '" style="white-space:nowrap;overflow:hidden;text-overflow:ellipsis;">' + esc(name) + '</div>' +
    '<div>' + badges.join(' ') + '</div></td>' +
    '<td><select onchange="setModelFamily(\'' + esc(name) + '\',this.value)" style="width:100%;">' + familyOptions + '</select></td>' +
    '<td><div style="display:flex;flex-wrap:wrap;gap:4px;align-items:center;">' + aliasChips +
    '<input class="font-mono" placeholder="+别名" style="width:88px;padding:2px 6px;font-size:12px;" ' +
    'onkeydown="if(event.key===\'Enter\'){addModelAlias(\'' + esc(name) + '\',this.value);this.value=\'\';}"></div></td>' +
    '<td>' + toggleHtml(name, 'anti_truncate', '暴露「抗截断-」变体') + '</td>' +
    '<td>' + toggleHtml(name, 'non_stream_prefix', '暴露「假非流-」变体') + '</td>' +
    '<td>' + toggleHtml(name, 'trailing_turn_fix', '历史以模型回合结尾时追加「继续」') + '</td>' +
    '<td class="desc">' + esc(enabled ? '已启用' : '已停用') + ' · ' + esc(familyLabel(name)) + '</td>' +
    '<td>' + enableToggle + '</td>' +
    '</tr>';
}

function renderModelsDiff() {
  const card = $('#modelsDiffCard');
  const box = $('#modelsDiffBody');
  const actions = $('#modelsDiffActions');
  const hint = $('#modelsDiffHint');
  if (!card || !box || !actions || !hint) return;
  const diff = MODELS_STATE.diff;
  if (!diff) {
    card.classList.add('hidden');
    return;
  }
  card.classList.remove('hidden');

  const confirmed = diff.removed_confirmed === true;
  hint.innerHTML = '以上游目录快照为基准（最近一次可信获取）。勾选后点下方按钮合并到列表，' +
    '再点右上「保存」才会写入配置；未勾选的项一律保留原样。';

  const newList = diff.new || [];
  const existing = diff.existing || [];
  const removed = diff.removed || [];
  const customUnseen = diff.custom_unseen || [];

  const newRows = newList.map(id => {
    const meta = modelMetaFor(id);
    const isVideo = meta.family === 'vedio';
    const enableChecked = MODELS_STATE.diffEnableOnImport[id] && !isVideo;
    return '<label style="display:flex;align-items:center;gap:9px;padding:4px 0;">' +
      '<input type="checkbox" style="width:auto;"' + (MODELS_STATE.diffAdd[id] ? ' checked' : '') +
      ' onchange="MODELS_STATE.diffAdd[\'' + esc(id) + '\']=this.checked;renderModelsDiff();">' +
      '<span class="font-mono">' + esc(id) + '</span>' +
      '<span class="desc">' + esc(MODEL_FAMILY_LABELS[meta.family] || meta.family || '') + '</span>' +
      (isVideo
        ? '<span class="desc" style="color:var(--red,#e06c75);">暂不可用：' + esc(meta.unavailable_reason || '视频生成 API 尚未实现') + '（可导入查看，不可启用）</span>'
        : '<label style="display:flex;align-items:center;gap:5px;margin-left:auto;"><input type="checkbox" style="width:auto;"' + (enableChecked ? ' checked' : '') +
          ' onchange="MODELS_STATE.diffEnableOnImport[\'' + esc(id) + '\']=this.checked;renderModelsDiff();">导入后立即启用</label>') +
      '</label>';
  }).join('') || '<div class="desc">无新模型</div>';

  const removedRows = removed.map(id => {
    if (!confirmed) {
      return '<div style="padding:4px 0;" class="desc"><span class="font-mono">' + esc(id) +
        '</span>（最近一次目录获取失败，暂不可判定是否下架，未提供移除勾选）</div>';
    }
    return '<label style="display:flex;align-items:center;gap:9px;padding:4px 0;">' +
      '<input type="checkbox" style="width:auto;"' + (MODELS_STATE.diffRemove[id] ? ' checked' : '') +
      ' onchange="MODELS_STATE.diffRemove[\'' + esc(id) + '\']=this.checked;renderModelsDiff();">' +
      '<span class="font-mono">' + esc(id) + '</span>' +
      '<span class="desc">' + esc(familyLabel(id)) + ' · 上游已不存在</span></label>';
  }).join('') || '<div class="desc">无</div>';

  const customRows = customUnseen.map(id =>
    '<div style="padding:3px 0;"><span class="font-mono">' + esc(id) + '</span>' +
    '<span class="desc"> · 本地自定义（上游未发现），默认保留</span></div>'
  ).join('') || '<div class="desc">无</div>';

  box.innerHTML =
    '<details open><summary style="cursor:pointer;">新模型（' + newList.length + '）—— 默认勾选导入，可逐项取消</summary><div style="margin:8px 0 4px;">' + newRows + '</div></details>' +
    '<details style="margin-top:8px;"><summary style="cursor:pointer;">已有模型（' + existing.length + '）—— 本地配置保留不变</summary><div style="margin:8px 0 4px;" class="desc">' + (existing.length ? esc(existing.join('、')) : '无') + '</div></details>' +
    '<details style="margin-top:8px;"><summary style="cursor:pointer;">已移除模型（' + removed.length + '）—— 仅勾选项会被移除，默认保留</summary><div style="margin:8px 0 4px;">' + removedRows + '</div></details>' +
    '<details style="margin-top:8px;"><summary style="cursor:pointer;">上游未发现（本地自定义）（' + customUnseen.length + '）</summary><div style="margin:8px 0 4px;">' + customRows + '</div></details>';

  const lf = MODELS_STATE.lastFetch;
  const actionBtns = [];
  actionBtns.push('<button class="btn" onclick="applyDiffImport()">导入所选新模型</button>');
  actionBtns.push('<button class="btn btn-red" onclick="applyDiffRemove()"' + (confirmed ? '' : ' disabled title="最近一次目录获取失败，暂不可执行移除"') + '>移除所选模型</button>');
  if (lf && lf.suspicious && !lf.snapshot_written) {
    actionBtns.push('<button class="btn btn-red" onclick="acceptAnomalySnapshot()">确认接受此异常快照</button>');
    actionBtns.push('<span class="desc" style="color:var(--red,#e06c75);">上游目录数量异常骤降（' + esc(lf.kept) + ' 条），疑似分页/结构异常：快照未更新、已移除项不可执行。确认无误后可接受。</span>');
  }
  actions.innerHTML = actionBtns.join(' ');
}

function setModelOption(name, key, value) {
  const exp = Object.assign({}, MODELS_STATE.explicit[name] || {});
  exp[key] = !!value;
  MODELS_STATE.explicit[name] = exp;
  const eff = Object.assign({}, MODELS_STATE.effective[name] || {});
  eff[key] = !!value;
  MODELS_STATE.effective[name] = eff;
  markModelsDirty();
  renderModelsTable();
}

function resetModelOption(name, key) {
  const exp = Object.assign({}, MODELS_STATE.explicit[name] || {});
  delete exp[key];
  if (Object.keys(exp).length) {
    MODELS_STATE.explicit[name] = exp;
  } else {
    delete MODELS_STATE.explicit[name];
  }
  // 生效值回退为家族默认（文本：抗截断/非流开、尾部关）
  const eff = Object.assign({}, MODELS_STATE.effective[name] || {});
  if (key === 'anti_truncate' || key === 'non_stream_prefix') eff[key] = true;
  if (key === 'trailing_turn_fix') eff[key] = false;
  MODELS_STATE.effective[name] = eff;
  markModelsDirty();
  renderModelsTable();
}

function setModelEnabled(name, enabled) {
  const meta = modelMetaFor(name);
  if (enabled && meta.family === 'vedio') {
    toast('视频模型暂不支持启用（视频生成 API 尚未实现）', 'err');
    renderModelsTable();
    return;
  }
  if (enabled) {
    MODELS_STATE.disabled = MODELS_STATE.disabled.filter(m => m !== name);
    if (!MODELS_STATE.models.includes(name)) MODELS_STATE.models.push(name);
  } else {
    MODELS_STATE.models = MODELS_STATE.models.filter(m => m !== name);
    if (!MODELS_STATE.disabled.includes(name)) MODELS_STATE.disabled.push(name);
  }
  markModelsDirty();
  renderModelsTable();
}

function setModelFamily(name, value) {
  if (value) {
    MODELS_STATE.familyMap[name] = value;
  } else {
    delete MODELS_STATE.familyMap[name];
  }
  if (value === 'vedio' && isEnabled(name)) {
    MODELS_STATE.models = MODELS_STATE.models.filter(m => m !== name);
    if (!MODELS_STATE.disabled.includes(name)) MODELS_STATE.disabled.push(name);
    toast('已自动停用：视频模型不可启用', 'err');
  }
  markModelsDirty();
  renderModelsPage();
}

function addModelAlias(name, alias) {
  alias = (alias || '').trim();
  if (!alias) return;
  if (MODELS_STATE.aliasMap[alias] && MODELS_STATE.aliasMap[alias] !== name) {
    toast('别名 ' + alias + ' 已存在（指向 ' + MODELS_STATE.aliasMap[alias] + '）', 'err');
    return;
  }
  MODELS_STATE.aliasMap[alias] = name;
  markModelsDirty();
  renderModelsTable();
}

function removeModelAlias(alias) {
  delete MODELS_STATE.aliasMap[alias];
  markModelsDirty();
  renderModelsTable();
}

function toggleModelSelected(name, checked) {
  if (checked) MODELS_STATE.selected[name] = true;
  else delete MODELS_STATE.selected[name];
  renderModelsTable();
}

function toggleSelectAllModels(checked) {
  const names = modelsVisibleNames();
  if (checked) names.forEach(n => { MODELS_STATE.selected[n] = true; });
  else names.forEach(n => { delete MODELS_STATE.selected[n]; });
  renderModelsTable();
}

function selectedNames() {
  return Object.keys(MODELS_STATE.selected).filter(n => MODELS_STATE.selected[n]);
}

function bulkSetModelsEnabled(enabled) {
  const names = selectedNames();
  if (!names.length) { toast('请先勾选模型'); return; }
  names.forEach(n => {
    const meta = modelMetaFor(n);
    if (enabled && meta.family === 'vedio') {
      toast('视频模型不可启用，已跳过：' + n, 'err');
      return;
    }
    setModelEnabledInternal(n, enabled);
  });
  markModelsDirty();
  renderModelsTable();
}

function setModelEnabledInternal(name, enabled) {
  if (enabled) {
    MODELS_STATE.disabled = MODELS_STATE.disabled.filter(m => m !== name);
    if (!MODELS_STATE.models.includes(name)) MODELS_STATE.models.push(name);
  } else {
    MODELS_STATE.models = MODELS_STATE.models.filter(m => m !== name);
    if (!MODELS_STATE.disabled.includes(name)) MODELS_STATE.disabled.push(name);
  }
}

function removeModelFromState(name) {
  MODELS_STATE.models = MODELS_STATE.models.filter(m => m !== name);
  MODELS_STATE.disabled = MODELS_STATE.disabled.filter(m => m !== name);
  MODELS_STATE.custom = MODELS_STATE.custom.filter(m => m !== name);
  delete MODELS_STATE.explicit[name];
  delete MODELS_STATE.familyMap[name];
  Object.keys(MODELS_STATE.aliasMap).forEach(a => { if (MODELS_STATE.aliasMap[a] === name) delete MODELS_STATE.aliasMap[a]; });
  delete MODELS_STATE.selected[name];
}

function bulkRemoveModels() {
  const names = selectedNames();
  if (!names.length) { toast('请先勾选模型'); return; }
  showConfirm('将从本地移除 ' + names.length + ' 个模型（含别名、家族覆盖与开关配置），点「保存」后生效。继续吗？', () => {
    names.forEach(removeModelFromState);
    markModelsDirty();
    renderModelsPage();
  });
}

// ---------- 差异确认（只做前端暂存，保存时落盘） ----------

function applyDiffImport() {
  const ids = Object.keys(MODELS_STATE.diffAdd).filter(id => MODELS_STATE.diffAdd[id]);
  if (!ids.length) { toast('请先勾选要导入的新模型'); return; }
  let enabledCount = 0;
  ids.forEach(id => {
    const meta = modelMetaFor(id);
    const enable = !!MODELS_STATE.diffEnableOnImport[id] && meta.family !== 'vedio';
    if (enable) {
      if (!MODELS_STATE.models.includes(id)) MODELS_STATE.models.push(id);
      enabledCount++;
    } else if (!MODELS_STATE.disabled.includes(id) && !MODELS_STATE.models.includes(id)) {
      MODELS_STATE.disabled.push(id);
    }
    delete MODELS_STATE.diffAdd[id];
    delete MODELS_STATE.diffEnableOnImport[id];
  });
  markModelsDirty();
  renderModelsPage();
  toast('已导入 ' + ids.length + ' 个模型' + (enabledCount ? '（其中 ' + enabledCount + ' 个启用）' : '（默认停用）') + '，点「保存」写入配置');
}

function applyDiffRemove() {
  const diff = MODELS_STATE.diff || {};
  if (diff.removed_confirmed !== true) { toast('最近一次目录获取失败，暂不可执行移除', 'err'); return; }
  const ids = Object.keys(MODELS_STATE.diffRemove).filter(id => MODELS_STATE.diffRemove[id]);
  if (!ids.length) { toast('请先勾选要移除的模型'); return; }
  showConfirm('将从本地移除 ' + ids.length + ' 个模型（含别名、家族覆盖与开关配置），点「保存」后生效。继续吗？', () => {
    ids.forEach(id => { removeModelFromState(id); delete MODELS_STATE.diffRemove[id]; });
    markModelsDirty();
    renderModelsPage();
  });
}

function acceptAnomalySnapshot() {
  showConfirm('确认接受本次异常快照？接受后"已移除模型"将以此为准（数量骤降可能是上游结构性变化，也可能是分页异常）。', async () => {
    try {
      const r = await API.models.fetchUpstream({ accept_anomaly: true });
      MODELS_STATE.lastFetch = r;
      await loadModels();
      MODELS_STATE.lastFetch = r;
      renderModelsPage();
      toast(r.ok ? '已接受异常快照并更新目录' : ('失败：' + (r.error || '未知原因')), r.ok ? 'ok' : 'err');
    } catch (e) {
      toast('失败：' + (e.message || e), 'err');
    }
  });
}

// ---------- 获取上游目录 ----------

async function fetchUpstreamModels() {
  const btn = $('#modelsFetchBtn');
  if (btn) { btn.disabled = true; btn.textContent = '获取中…'; }
  try {
    const r = await API.models.fetchUpstream({});
    MODELS_STATE.lastFetch = r;
    if (r.ok) {
      // 更新目录状态与差异（本地列表不动，等待用户确认导入/移除）
      MODELS_STATE.diff = {
        new: r.new || [],
        existing: r.existing || [],
        removed: r.removed || [],
        custom_unseen: r.custom_unseen || [],
        removed_confirmed: r.removed_confirmed === true,
      };
      MODELS_STATE.diffAdd = {};
      (r.new || []).forEach(id => { MODELS_STATE.diffAdd[id] = true; }); // 新模型默认选中导入
      MODELS_STATE.diffRemove = {};
      try { MODELS_STATE.catalog = (await API.models.get()).catalog || MODELS_STATE.catalog; } catch (e) { /* 状态行稍后刷新 */ }
      toast('获取成功：发现 ' + r.discovered + ' / 候选 ' + r.kept + ' / 新增 ' + (r.new || []).length + (r.suspicious ? '（数量骤降，待确认）' : ''));
    } else {
      toast('获取失败：' + (r.error || '未知原因'), 'err');
    }
    renderModelsPage();
  } catch (e) {
    toast('获取失败：' + (e.message || e), 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '获取上游模型'; }
  }
}

// ---------- 脏标记与保存 ----------

function markModelsDirty() {
  window.hasUnsavedModels = true;
  updateModelsDirtyHint();
}

function updateModelsDirtyHint() {
  const el = $('#modelsDirtyHint');
  if (el) el.textContent = window.hasUnsavedModels ? '有未保存的修改，点右上「保存」生效' : '';
}

function buildModelsPayload() {
  return {
    models: MODELS_STATE.models.slice(),
    alias_map: Object.assign({}, MODELS_STATE.aliasMap),
    family_map: Object.assign({}, MODELS_STATE.familyMap),
    disabled_models: MODELS_STATE.disabled.slice(),
    custom_models: MODELS_STATE.custom.slice(),
    model_options: Object.assign({}, MODELS_STATE.explicit),
  };
}

async function saveModels() {
  const payload = buildModelsPayload();
  if (!payload.models.length) {
    toast('启用清单不能为空：至少保留一个模型', 'err');
    return;
  }
  try {
    await API.models.put(payload);
    toast('模型已保存并立即生效', 'ok');
    await loadModels();
  } catch (e) {
    toast('保存失败：' + (e.message || e), 'err');
  }
}

// ---------- 添加模型 ----------

function openAddModelDialog() {
  $('#addModelErr').textContent = '';
  $('#addModelName').value = '';
  $('#addModelAlias').value = '';
  $('#addModelFamily').value = '';
  $('#addModelEnable').checked = true;
  $('#addModelAnti').checked = false;
  $('#addModelNonStream').checked = false;
  $('#addModelTrailing').checked = false;
  $('#addModelModal').classList.remove('hidden');
}

function closeAddModelDialog() {
  $('#addModelModal').classList.add('hidden');
}

function submitAddModel() {
  const name = ($('#addModelName').value || '').trim();
  const alias = ($('#addModelAlias').value || '').trim();
  const family = $('#addModelFamily').value;
  const enable = $('#addModelEnable').checked;
  const errEl = $('#addModelErr');
  if (!name) { errEl.textContent = '模型名不能为空'; return; }
  if (knownModelNames().includes(name)) { errEl.textContent = '该模型已在列表中'; return; }
  if (alias && MODELS_STATE.aliasMap[alias] && MODELS_STATE.aliasMap[alias] !== name) {
    errEl.textContent = '别名已存在且指向其它模型';
    return;
  }
  const meta = { family: family || null };
  if (family === 'vedio' && enable) {
    errEl.textContent = '视频模型不可启用（后端能力尚未实现），请取消"立即启用"';
    return;
  }
  if (family) MODELS_STATE.familyMap[name] = family;
  if (alias) MODELS_STATE.aliasMap[alias] = name;
  // 开关按"新建模型初始化规则"写入显式值（文本家族：抗截断/非流默认开）
  const exp = {};
  if ($('#addModelAnti').checked) exp.anti_truncate = true;
  if ($('#addModelNonStream').checked) exp.non_stream_prefix = true;
  if ($('#addModelTrailing').checked) exp.trailing_turn_fix = true;
  if (Object.keys(exp).length) MODELS_STATE.explicit[name] = exp;
  MODELS_STATE.meta[name] = { family: family || 'text', family_source: family ? 'override' : 'heuristic', supported: family !== 'vedio', custom: true };
  MODELS_STATE.effective[name] = {
    applicable: (family || 'text') === 'text',
    anti_truncate: $('#addModelAnti').checked,
    non_stream_prefix: $('#addModelNonStream').checked,
    trailing_turn_fix: $('#addModelTrailing').checked,
  };
  if (!MODELS_STATE.custom.includes(name)) MODELS_STATE.custom.push(name);
  setModelEnabledInternal(name, enable);
  markModelsDirty();
  closeAddModelDialog();
  renderModelsPage();
  toast('已添加 ' + name + (enable ? '（已启用）' : '（未启用）') + '，点「保存」写入配置');
}

// relTime2：相对时间展示（与节点页 relTime 同构的轻量实现，避免跨页依赖）。
function relTime2(iso) {
  if (!iso) return '-';
  const t = Date.parse(iso);
  if (!t) return iso;
  const diff = Date.now() - t;
  if (diff < 0) return '刚刚';
  const sec = Math.floor(diff / 1000);
  if (sec < 60) return sec + ' 秒前';
  const min = Math.floor(sec / 60);
  if (min < 60) return min + ' 分钟前';
  const hour = Math.floor(min / 60);
  if (hour < 24) return hour + ' 小时前';
  return Math.floor(hour / 24) + ' 天前';
}
