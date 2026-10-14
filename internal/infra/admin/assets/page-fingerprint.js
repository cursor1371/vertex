// 浏览器指纹独立栏目：TLS 策略与请求头（UA）两个独立维度 + 地区语言预设。
// 保存走通用 /api/admin/settings；预设清单、TLS 池解析与生效状态来自只读
// /api/admin/fingerprint。预设只读（后端常量），手动导入存独立配置键、可编辑，
// 不可能覆盖预设。
let fpState = null;

async function loadFingerprint() {
  try {
    const d = await API.fingerprint.get();
    fpState = {
      enabled: !!d.enabled,
      mode: d.mode === 'custom' ? 'custom' : 'preset',
      preset: d.preset || 'chrome_150',
      custom: d.custom || '',
      tls: d.tls || {},
      locale: d.locale || 'zh-CN',
      locales: d.locales || [],
      tlsProfiles: d.tls_profiles || [],
      presets: d.presets || [],
      effective: d.effective || {},
    };
  } catch (e) {
    toast('加载浏览器指纹失败: ' + e.message, 'err');
    return;
  }

  $('#fpEnabled').checked = fpState.enabled;
  $('#fpTLSMode').value = fpState.tls.mode || 'follow';
  $('#fpTLSSingle').innerHTML = fpState.tlsProfiles.map(p => `<option ${p === fpState.tls.profile ? 'selected' : ''}>${esc(p)}</option>`).join('');
  $('#fpTLSMin').value = fpState.tls.min || '';
  $('#fpTLSMax').value = fpState.tls.max || '';
  $('#fpTLSListWrap').innerHTML = fpState.tlsProfiles.map(p => {
    const checked = (fpState.tls.pool || []).includes(p);
    return `<label class="desc" style="cursor:pointer;"><input type="checkbox" class="fp-tls-pool-item" value="${esc(p)}" ${checked ? 'checked' : ''} onchange="fpMarkDirty(); fpRenderTLSPool()"> ${esc(p)}</label>`;
  }).join('');
  $('#fpLocale').innerHTML = fpState.locales.map(l => `<option ${l === fpState.locale ? 'selected' : ''}>${esc(l)}</option>`).join('');
  $('#fpMode').value = fpState.mode;

  fpRenderTLSMode();
  fpRenderMode();
  fpUpdateToggleState();
  fpRenderEffective();
  window.hasUnsavedFingerprint = false;

  if (!window._hasFpUnloadListener) {
    window.addEventListener('beforeunload', (e) => {
      if (window.hasUnsavedFingerprint) {
        e.preventDefault();
        e.returnValue = '';
      }
    });
    window._hasFpUnloadListener = true;
  }
}

// fpMarkDirty 标记脏状态并去抖刷新生效预览（input/change 事件高频触发）。
function fpMarkDirty() {
  window.hasUnsavedFingerprint = true;
  clearTimeout(fpMarkDirty._t);
  fpMarkDirty._t = setTimeout(fpRenderEffective, 300);
}

// fpUpdateToggleState 总开关联动：关闭时置灰下方全部控件（与设置页并发池联动同构）。
function fpUpdateToggleState() {
  const on = $('#fpEnabled').checked;
  ['#fpTLSModeField', '#fpLocaleField', '#fpModeField', '#fpPresetArea', '#fpCustomArea'].forEach(sel => {
    const el = $(sel);
    if (!el) return;
    el.style.opacity = on ? '1' : '0.5';
    el.style.pointerEvents = on ? '' : 'none';
  });
  ['#fpTLSMode', '#fpLocale', '#fpMode'].forEach(sel => { const el = $(sel); if (el) el.disabled = !on; });
  ['#fpTLSMin', '#fpTLSMax', '#fpTLSSingle'].forEach(sel => { const el = $(sel); if (el) el.disabled = !on; });
  document.querySelectorAll('.fp-tls-pool-item').forEach(cb => { cb.disabled = !on; });
}

function fpOnEnabledChange() { fpMarkDirty(); fpUpdateToggleState(); }

// fpCurrentTLSPolicy 从表单收集当前 TLS 策略。
function fpCurrentTLSPolicy() {
  const pool = Array.from(document.querySelectorAll('.fp-tls-pool-item')).filter(cb => cb.checked).map(cb => cb.value);
  const num = (id) => { const v = parseInt($(id).value, 10); return isNaN(v) ? 0 : v; };
  return {
    mode: $('#fpTLSMode').value,
    profile: $('#fpTLSSingle').value,
    min: num('#fpTLSMin'),
    max: num('#fpTLSMax'),
    pool,
  };
}

// fpRenderTLSPool 按当前表单即时计算实际随机池（走后端解析口径，避免两端漂移）：
// 这里用前端镜像逻辑展示预览，保存后由 GET /fingerprint 的 resolved_pool 权威校准。
function fpRenderTLSPool() {
  const el = $('#fpTLSPoolResolved');
  if (!el) return;
  const policy = fpCurrentTLSPolicy();
  let pool = [];
  const all = fpState.tlsProfiles;
  const verOf = (n) => parseInt(n.replace('chrome_', ''), 10);
  switch (policy.mode) {
    case 'legacy': pool = ['chrome_144', 'chrome_146']; break;
    case 'single': pool = all.includes(policy.profile) ? [policy.profile] : []; break;
    case 'range': {
      let lo = Math.min(policy.min, policy.max), hi = Math.max(policy.min, policy.max);
      pool = all.filter(p => { const v = verOf(p); return v >= lo && v <= hi; });
      break;
    }
    case 'list': pool = all.filter(p => policy.pool.includes(p)); break;
    default: {
      // follow：池 = 所选预设自带 TLS（custom 模式下 fpPresetSel 也写回 fpState.preset）。
      const preset = fpState.presets.find(x => x.name === fpState.preset);
      pool = preset ? preset.tls_pool : [];
    }
  }
  el.textContent = pool.length
    ? `实际随机池（每上游请求随机选一）: ${pool.join(', ')}`
    : '实际随机池为空，保存后将回退为"跟随预设"。';
}

// fpRenderTLSMode 按模式显示对应控件。
function fpRenderTLSMode() {
  const mode = $('#fpTLSMode').value;
  $('#fpTLSSingleWrap').classList.toggle('hidden', mode !== 'single');
  $('#fpTLSRangeWrap').classList.toggle('hidden', mode !== 'range');
  $('#fpTLSListWrap').classList.toggle('hidden', mode !== 'list');
  fpRenderTLSPool();
}

// fpRenderMode 按"请求头来源"渲染预设选择区或手动导入区。
function fpRenderMode() {
  if (fpState.mode === 'custom') {
    $('#fpPresetArea').innerHTML = fpPresetPickerHtml();
    $('#fpCustomArea').innerHTML = fpCustomAreaHtml();
    const ta = $('#fpCustomJson');
    ta.value = fpState.custom;
    ta.addEventListener('input', fpMarkDirty);
    if (!fpState.custom) ta.placeholder = fpCustomPlaceholder();
  } else {
    $('#fpCustomArea').innerHTML = '';
    $('#fpPresetArea').innerHTML = fpPresetCardsHtml();
  }
  fpRenderTLSPool();
}

// custom 模式下仍需选一个预设作为"跟随预设"的 TLS 与导入起点。
function fpPresetPickerHtml() {
  const opts = fpState.presets.map(p => `<option ${p.name === fpState.preset ? 'selected' : ''}>${esc(p.name)}</option>`).join('');
  return `<div class="field" style="margin-top:10px;">
    <label for="fpPresetSel">预设（作为 TLS"跟随预设"与手动包导入起点）</label>
    <select id="fpPresetSel" onchange="fpSelectPreset(this.value)">${opts}</select>
  </div>`;
}

function fpPresetCardsHtml() {
  const cards = fpState.presets.map(p => {
    const selected = fpState.preset === p.name;
    const badge = p.verified
      ? '<span class="desc" style="color:var(--ok,#5bd08a);">✓ 实测头集</span>'
      : '<span class="desc">△ 派生头集（未实测，仅供测试）</span>';
    const rows = Object.entries(p.headers || {})
      .map(([k, v]) => `<div style="display:flex;gap:8px;"><span style="flex:0 0 190px;color:var(--muted,#9a9a9a);">${esc(k)}</span><span style="word-break:break-all;">${esc(v)}</span></div>`)
      .join('');
    return `
      <div class="field" style="margin-top:10px; background:rgba(255,255,255,0.03); padding:14px; border-radius:10px; border:1px solid ${selected ? 'var(--accent,#7aa2f7)' : 'var(--stroke)'}; cursor:pointer;" onclick="fpSelectPreset('${esc(p.name)}')">
        <div style="display:flex; justify-content:space-between; align-items:center; gap:8px;">
          <div style="display:flex; align-items:center; gap:10px;">
            <input type="radio" name="fpPreset" value="${esc(p.name)}" ${selected ? 'checked' : ''} onchange="fpSelectPreset('${esc(p.name)}')">
            <label style="font-weight:600; font-size:14px; cursor:pointer;">${esc(p.name)}</label>
          </div>
          ${badge}
        </div>
        <div class="desc" style="margin-top:6px;">TLS: ${(p.tls_pool || []).join(', ')}</div>
        <details style="margin-top:8px;" onclick="event.stopPropagation()">
          <summary class="desc" style="cursor:pointer;">查看请求头（只读）</summary>
          <div class="desc" style="margin-top:6px; display:grid; gap:4px; font-family:monospace; font-size:12px;">${rows}</div>
        </details>
      </div>`;
  }).join('');
  return `<div class="field-heading" style="margin-top:8px;">预设（只读，点选切换）</div>${cards}`;
}

function fpSelectPreset(name) {
  if (fpState.preset !== name) { fpState.preset = name; fpMarkDirty(); }
  if (fpState.mode === 'custom') {
    // custom 模式下仅刷新 TLS 池解析，不重渲染——重渲染会清掉编辑中的手动包 textarea。
    fpRenderTLSPool();
    return;
  }
  fpRenderMode();
}

function fpCustomAreaHtml() {
  return `
    <div class="field-heading" style="margin-top:8px;">手动导入包（JSON，可编辑）</div>
    <div class="desc" style="margin-bottom:8px;">稀疏覆盖 chrome_150 基线，未提供的字段继承基线；未知字段会被拒绝。TLS 指纹不在此包内——请用上方「TLS 指纹策略」配置。x-browser-validation 始终由 user-agent 自动计算，无需填写。</div>
    <label for="fpCustomJson" class="sr-only">手动导入包 JSON</label>
    <textarea id="fpCustomJson" rows="10" spellcheck="false" style="width:100%; font-family:monospace; font-size:12px;"></textarea>
    <div class="flex-gap-11" style="margin-top:10px;">
      <button class="btn ghost" onclick="fpImportPresetToCustom()">从预设导入为手动包</button>
      <button class="btn ghost" onclick="fpSaveCustom()">校验并保存手动包</button>
    </div>`;
}

function fpCustomPlaceholder() {
  return JSON.stringify({
    user_agent: 'Mozilla/5.0 …',
    sec_ch_ua: '"Chromium";v="150", "Google Chrome";v="150"',
    accept_language: 'zh-CN,zh;q=0.9,en;q=0.8',
  }, null, 2);
}

// fpImportPresetToCustom 把所选预设序列化成手动包 JSON 填入 textarea 作为编辑起点。
// 只填充输入框，不落盘——手动包在显式保存前不影响任何配置。
function fpImportPresetToCustom() {
  const p = fpState.presets.find(x => x.name === fpState.preset) || fpState.presets[0];
  if (!p) return;
  $('#fpCustomJson').value = JSON.stringify(p.headers || {}, null, 2);
  fpMarkDirty();
  toast('已导入预设 ' + p.name + ' 为编辑起点');
}

// fpSaveCustom 仅保存并校验手动包（不动开关/TLS/预设）；400 时展示后端具体原因。
async function fpSaveCustom() {
  const raw = $('#fpCustomJson').value;
  try {
    await API.settings.put({ browser_fingerprint_custom: raw });
    fpState.custom = raw;
    toast('手动包已保存并通过校验', 'ok');
    window.hasUnsavedFingerprint = false;
    await loadFingerprint();
  } catch (e) {
    toast('校验失败: ' + e.message, 'err');
  }
}

// saveFingerprint 保存全部指纹配置（TLS 策略 / 地区 / 请求头来源）。
// custom 模式连带当前 textarea 内容一起提交，由后端强校验；preset 模式不动已存手动包。
async function saveFingerprint() {
  const out = {
    browser_fingerprint_enabled: $('#fpEnabled').checked,
    browser_fingerprint_tls_mode: $('#fpTLSMode').value,
    browser_fingerprint_locale: $('#fpLocale').value,
    browser_fingerprint_mode: $('#fpMode').value,
    browser_fingerprint_preset: fpState.preset,
  };
  const tls = fpCurrentTLSPolicy();
  if (tls.mode === 'single') out.browser_fingerprint_tls_profile = tls.profile;
  if (tls.mode === 'range') { out.browser_fingerprint_tls_min = tls.min; out.browser_fingerprint_tls_max = tls.max; }
  if (tls.mode === 'list') out.browser_fingerprint_tls_pool = tls.pool;
  if (out.browser_fingerprint_mode === 'custom') {
    out.browser_fingerprint_custom = $('#fpCustomJson') ? $('#fpCustomJson').value : fpState.custom;
  }
  try {
    await API.settings.put(out);
  } catch (e) {
    toast('保存失败: ' + e.message, 'err');
    return;
  }
  toast('浏览器指纹已保存并应用', 'ok');
  window.hasUnsavedFingerprint = false;
  await loadFingerprint();
}

function fpRenderEffective() {
  const e = fpState.effective || {};
  let text;
  if (!e.active) {
    text = '未启用自定义指纹 —— 当前为默认行为：TLS 指纹在 Chrome 144/146 间随机，请求头为 Chrome 150。与升级前逐字节一致。';
  } else {
    const src = e.source === 'custom' ? '手动导入包' : '预设';
    const pool = (e.tls_pool || []).join(', ');
    text = `已启用（来源: ${src} · ${esc(e.name || '')}）—— TLS 随机池: ${esc(pool || '?')} · UA 主版本: Chrome/${esc((e.ua || '').match(/Chrome\/(\d+)/)?.[1] || '?')}${e.verified ? ' · 头集已实测' : ' · 头集未实测（仅供测试）'}`;
  }
  const dirty = window.hasUnsavedFingerprint ? '（有未保存修改，预览反映的是已保存配置）' : '';
  $('#fpEffective').innerHTML = text + ' ' + dirty;
}
