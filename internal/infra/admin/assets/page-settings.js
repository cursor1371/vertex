const SETTINGS_FIELDS = [
  // 🚀 Group: pool (并发与连接)
  { k: 'parallel_pool_enabled', label: '并发请求池', type: 'bool', group: 'pool', desc: '同时请求多个健康节点，首包到达即采纳，降低延迟' },
  { k: 'parallel_pool_size', label: '并发数', type: 'number', max: 100, min: 1, group: 'pool', desc: '并发抢跑的节点数 (默认 15，最大 100)' },
  { k: 'max_concurrent', label: '入站全局并发闸门', type: 'number', min: 0, group: 'pool', desc: '同时处理的最大入站请求数，超过返回 429+Retry-After，压制上游限流 (0=不限，默认 64)' },
  { k: 'health_probe_fail_threshold', label: '探测失败阈值（默认）', type: 'number', min: 0, max: 10, group: 'fault', desc: '自动探测阶段连续失败 N 次即把节点转为永久禁用（默认 2，范围 0~10）。**0 = 无限探测**（永不因探测失败转永久，探测成功仍恢复）。它不控制首次移出——首次移出由「处理策略」表按类别决定。每个类别可在策略页单独覆盖此值' },
  { k: 'health_selfheal_enabled', label: '自动探查并恢复被禁节点', type: 'bool', group: 'fault', desc: '默认开。后台周期性对"被自动禁用"的出口节点发起真实探查（完整 rT token 抓取，抓取成功才算通过，不是单纯连通性），成功即自动解除禁用。仅出口池生效，人工/admin 测试/协议不支持的禁用不会被探查。关闭后被禁节点只能靠管理页手动测速或「启用」按钮恢复；同一节点连续探查失败达阈值后转永久禁用，不再重复探查' },
  { k: 'health_selfheal_interval_minutes', label: '自动探查间隔(分钟)', type: 'number', min: 1, max: 1440, group: 'fault', desc: '自动探查的周期（默认 120，范围 1~1440）。上一轮探查结束后按当前配置重新等待，修改后下一轮生效，无需重启。被禁节点最长需一个周期才可能被自动恢复' },
  { k: 'health_selfheal_probe_concurrency', label: '自动探查并发度', type: 'number', min: 1, max: 20, group: 'fault', desc: '单轮自动探查的并发节点数（默认 5，范围 1~20）。探查含完整 rT 抓取、单节点可能持续数秒，调大更快探完但会与线上请求争出口带宽' },
  { k: 'health_global_fault_guard_enabled', label: '全局故障保护', type: 'bool', group: 'fault', desc: '默认开。窗口内多个不同节点同时出现同类节点级故障时，判定为疑似本机网络/DNS 或上游公共依赖故障，暂停自动禁用。取消它会导致一次全局抖动把整池节点逐个禁掉' },
  { k: 'health_global_fault_min_nodes', label: '全局故障判定节点数', type: 'number', min: 2, max: 50, group: 'fault', desc: '触发全局故障判定所需的"不同节点"数量（默认 3，范围 2~50）。下界 2：单个节点同因失败属节点自身故障，设成 1 会让自动禁用彻底失效' },
  { k: 'health_global_fault_window_seconds', label: '全局故障判定窗口(秒)', type: 'number', min: 10, max: 3600, group: 'fault', desc: '统计"多少秒内多个节点同因失败"的滑动窗口（默认 120，范围 10~3600）。窗口过后若不再出现同类失败，自动禁用恢复生效' },

  // 🔐 Group: rt (rT Token 抓取策略)
  { k: 'recaptcha_try_entry_or_direct', label: '优先前置/直连抓取 RT', type: 'bool', group: 'rt', desc: '默认关。开启时获取 reCAPTCHA Token 优先尝试前置代理/直连；关闭或失败时顺次轮询健康候选节点（两种情况下最后都仍会兜底直连一次，除非开启下方"禁止直连抓 rT"）' },
  { k: 'recaptcha_no_direct_fetch', label: '禁止直连抓 rT', type: 'bool', group: 'rt', desc: '硬开关（默认关）。开启后 rT 抓取只走出口候选节点，"入口/直连"整条路径一律不尝试——含前置代理（该路径在代码里与真·直连同属一档，无法区分），并覆盖上一条"优先前置/直连"。没有可用出口候选时抓取直接失败（候选以 infra 错误出局、请求失败），不再静默降级成直连。仅在明确要求 rT 必须走出口节点时开启' },
  { k: 'recaptcha_live_budget_sec', label: '实时 rT 单轮预算(秒)', type: 'number', min: 5, max: 120, group: 'rt', desc: '请求路径（实时抓取）单轮 rT 抓取的超时（默认 15）。注意它在关键路径上：候选建联与取 token 是并行发起、双双完成才发上游请求，所以这个值等于客户端最坏多等多久；实际生效值还受候选剩余时限压制（= min(剩余, 该值)），补位后发的候选可能用不到。调大不解决上游慢，反而可能因并发抓取持续更久而加剧限流（范围 5~120）' },
  { k: 'rt_compensation_ms', label: 'RT 首发补偿间隔(ms)', type: 'number', min: 0, group: 'rt', desc: '每个真实请求第一发必被上游拒绝，需以同一 token 重发第二发；此为两发之间的静置间隔（默认 500ms，实测与成功率无关，0=立即重发）' },

  // ♻️ Group: rtpool (rT Token 预取池)
  { k: 'recaptcha_token_prefetch', label: 'rT Token 预取池', type: 'bool', group: 'rtpool', desc: '开启后后台按消费节奏预抓 reCAPTCHA Token，请求命中即取用，省去 0.5~2 秒的关键路径抓取耗时（默认关）。命中失败一律回退实时抓取，行为与未开启时一致' },
  { k: 'recaptcha_prefetch_depth', label: '预取池深度', type: 'number', min: 1, max: 100, group: 'rtpool', desc: '池中常备 token 数（默认 5）。热 token 候选必然先到达，深度取并发数三分之一左右即可吃到绝大部分延迟收益' },
  { k: 'recaptcha_prefetch_ttl_sec', label: '预取 Token 存活(秒)', type: 'number', min: 1, max: 110, group: 'rtpool', desc: '池内 token 超过该年龄即丢弃并回退实时抓取（默认 90）。Google 侧硬上限 2 分钟，过期 token 会以 timeout-or-duplicate 失败，故必须留足余量（上限 110）' },
  { k: 'recaptcha_prefetch_budget_sec', label: '预取单轮预算(秒)', type: 'number', min: 5, max: 180, group: 'rtpool', desc: '后台补货单次抓取的超时预算（默认 15，与请求路径一致）。开启"禁止直连抓 rT"后抓取只能走出口候选（每候选最多 3 次尝试 × 2~3 个 HTTP），15 秒常连一个候选都跑不完，可上调（如 45）。后台没有请求在等，调大不影响请求延迟，只影响后台会话持有时长（范围 5~180）' },
  { k: 'recaptcha_prefetch_adaptive', label: '预取无收益自停', type: 'bool', group: 'rtpool', desc: '默认开。池内补满后出现过期 token（说明这池预取根本没被消费）即暂停补货，避免稀疏流量下每来一次请求就白抓一池 token（命中恒 0、过期只增、放大上游限流）；新的连接请求一到达就立刻恢复补货。关闭即退回“有请求就补满”的旧行为' },

  // 🛠 Group: core (核心控制与基础参数)
  { k: 'max_retries', label: '补位轮数', type: 'number', group: 'core', desc: '窗口竞速的补位轮数；总发射预算 = (此值+1) × 并发数，失败即瞬时换点补位' },
  { k: 'max_n', label: '最大候选数 (max_n)', type: 'number', group: 'core', desc: '限制客户端一次生成回答的条数上限，防滥用刷量 (默认 8)' },
  { k: 'max_spill_mb', label: '最大内存缓冲 (MB)', type: 'number', group: 'core', desc: '上传大文件时，超过此大小将写入磁盘，防爆内存 (默认 2048)' },
  { k: 'aggregate_stream', label: '聚合流式', type: 'bool', group: 'core', desc: '拦截流式请求，改为一次性返回完整结果的单块流（解决部分客户端单字流式卡顿问题）' },
  { k: 'debug_mode', label: 'Debug 日志', type: 'bool', group: 'core', desc: '开启更详细的错误与负载调试日志' },
  { k: 'admin_api_log_enabled', label: 'Admin API 请求日志', type: 'bool', group: 'core', desc: '在运行日志打印每个 /api/admin 请求一行（默认关：请求监控等页面每 3 秒轮询，开着会刷屏）。排查 admin 侧问题时再开；/stats/active 与 /stats/rt-token 两个轮询端点无论开关都不打' },

  { k: 'default_image_size', label: '默认图片清晰度', type: 'select', group: 'core', opts: ['512', '1K', '2K', '4K'], desc: '图模型请求未指定清晰度时的默认档位（按模型能力自动降级）' },
  { k: 'default_thinking_level', label: '默认思考等级', type: 'select', group: 'core', opts: ['自动', '最低', '低', '中', '高'], desc: '文本/图模型请求未指定思考参数时的默认档位（按模型能力自动适配）' },
  { k: 'default_response_modalities', label: '默认图片输出模态', type: 'select', group: 'core', opts: ['图文', '仅图片'], desc: '图模型未指定响应内容时的默认类型（图文 = 图片+文本，仅图片 = 仅纯图片）' },
  { k: 'stream_idle_timeout_seconds', label: '流式包间空闲超时(秒)', type: 'number', group: 'core', min: 1, desc: '流式传输中连续无数据字节的超时时间（默认 20 秒），防止网络波动卡死' },
  { k: 'request_timeout_seconds', label: '请求总超时(秒)', type: 'number', group: 'core', min: 100, max: 600, desc: '单个候选的最大总超时限制（下限 100，默认 180 秒）' },
  { k: 'openai_sse_heartbeat_seconds', label: 'OpenAI 流式心跳间隔(秒)', type: 'number', group: 'core', min: 0, max: 600, desc: '仅作用于 /v1/chat/completions 的流式响应：每隔该秒数发一行心跳注释，防止长思考期间下游网关（new-api / Nginx 等）先行断流。0 = 关闭（默认）' },

  // 🛡 Group: security (安全增强与模型策略)
  // 注：尾部模型回合兼容已迁移为「模型」页的逐模型开关（model_settings.json），
  // 不再出现在设置页；旧配置键由服务端自动迁移（见 config.migrateTrailingFixToModelSettings）。
  { k: 'drop_max_tokens', label: '移除 maxOutputTokens', type: 'bool', group: 'security', desc: '移除输出 token 上限，让模型自由输出' },
  { k: 'forced_tool_call_mode', label: '强制工具调用', type: 'select', group: 'security', opts: ['关闭', '优先', '强制'], desc: '注入合成工具，让模型把回答经 tool call 承载，回程再解包为纯文本交付。优先＝仅注入不强制，模型可直接返回普通文本；强制＝强制模型进入工具调用路径（客户端无自带工具时最终回答必定经合成工具返回；客户端自带工具时模型可能先调用真实工具，最终回答不保证经合成工具返回）。带内置工具（Google 搜索/地图/代码执行等）或客户端显式关闭函数调用的请求一律跳过' },
  { k: 'forced_tool_call_models', label: '强制工具调用模型清单', type: 'model_select', group: 'security', desc: '仅对勾选的模型生效；留空表示全部文本模型生效。强制档下模型被要求必须调用某个函数，若同一候选同时出现普通文本与合成调用，普通文本会被丢弃以保持单一真源' },

  // 🌐 Group: network (IPv6 出口策略)
  { k: 'force_node_ipv6', label: '强制节点 IPv6', type: 'bool', group: 'network', desc: '出口节点 server 为域名时强制 IPv6 拨号（仅解析 AAAA）。节点为 IP 时无效；开启后若节点无 IPv6 地址将拨号失败' },
  { k: 'host_ipv6_interface', label: '宿主机 IPv6 网卡', type: 'text', group: 'network', desc: '绑定出口到指定网卡（如 eth0）。与地址同填时以地址优先' },
  { k: 'host_ipv6_address', label: '宿主机 IPv6 地址', type: 'text', group: 'network', desc: '绑定出口到指定 IPv6 地址（优先于网卡绑定）。留空且网卡留空则不绑定' },

  // 📋 Group: httplog (HTTP 请求级日志)
  { k: 'http_log_enabled', label: '记录 HTTP 日志', type: 'bool', group: 'httplog', desc: '默认关。开启后记录每次上游请求的完整 URL/请求头/请求体/响应头/响应体（含敏感信息，仅 admin 可见）。关闭时不记录' },
  { k: 'http_log_retention_days', label: 'HTTP 日志保留天数', type: 'number', group: 'httplog', min: 1, desc: 'http_log 表独立保留天数（默认 30），超出自动清理。完整 body 存储会占用较多磁盘，建议按需调小' },
  { k: 'http_log_body_cap_kb', label: 'HTTP 日志 body 上限 (KB)', type: 'number', group: 'httplog', min: 0, desc: '单条请求/响应体捕获上限（默认 64KB，0=不截断）。防止大请求/流式响应撑爆内存与磁盘' },

  // 🗄️ Group: dbreten (历史保留与数据库体积)
  { k: 'history_retention_days', label: '请求历史保留天数', type: 'number', group: 'dbreten', min: 1, desc: 'usage_log 保留天数（默认 90），超出后每小时自动清理。注意：删除只把页放进空闲列表、不会自动缩小数据库文件，需在「数据库」页做增量回收或离线压缩' },
  { k: 'race_log_retention_days', label: '竞速明细保留天数', type: 'number', group: 'dbreten', min: 1, desc: 'race_log 独立保留天数（默认 7），超出自动清理。竞速明细量最大（约 3 万行/天），是数据库体积增长的主要来源' },
];

let curSettings = {};

// renderModelSelect 渲染挂在「强制工具调用」模式选择器右侧的下拉勾选组件。
// 面板列出系统已注册模型；配置中已有的非注册模型追加在列表尾部（带“已配置”标记，向后兼容不丢数据）。
// 模型名经 esc()（utils.js）转义后拼入 HTML，防止配置文件中的特殊字符造成 HTML 注入。
function renderModelSelect(f, sysModels) {
  const raw = curSettings[f.k];
  const selected = Array.isArray(raw) ? raw : [];
  const extra = selected.filter(m => !sysModels.includes(m));
  const all = sysModels.concat(extra);
  const items = all.map(m => `
    <label class="chk"><input type="checkbox" value="${esc(m)}" ${selected.includes(m) ? 'checked' : ''}>${esc(m)}${extra.includes(m) ? '<span class="model-dd-tag">已配置</span>' : ''}</label>`).join('');
  return `<div class="model-dd" id="set_${f.k}_dd">
    <button type="button" class="btn ghost model-dd-btn" id="set_${f.k}_dd_btn">已选 ${selected.length} 个模型 <span class="model-dd-caret">▾</span></button>
    <div class="model-dd-panel hidden" id="set_${f.k}_dd_panel">
      <div class="model-dd-tools">
        <button type="button" class="link" data-act="all">全选</button>
        <button type="button" class="link" data-act="none">清空</button>
        <span class="model-dd-count" id="set_${f.k}_dd_count">已选 ${selected.length}</span>
      </div>
      <div class="model-dd-list">${items || '<div class="desc">系统暂无已注册模型</div>'}</div>
      ${f.desc ? `<div class="desc">${f.desc}</div>` : ''}
    </div>
  </div>`;
}

// collectModelSelect 收集下拉面板中勾选的模型（按 DOM 顺序 = 清单顺序）。
function collectModelSelect(f) {
  const panel = $('#set_' + f.k + '_dd_panel');
  if (!panel) return [];
  const out = [];
  panel.querySelectorAll('input[type="checkbox"]:checked').forEach(cb => {
    const v = cb.value.trim();
    if (v && !out.includes(v)) out.push(v);
  });
  return out;
}

// settingFieldHTML 渲染单个设置字段（设置页与「节点故障策略与记录 → 探测与保护参数」共用）。
// 共用同一实现是有意的：两页渲染同一批配置键，若各写一份，类型/值域/描述迟早漂移。
function settingFieldHTML(f, settings, sysModels) {
    if (f.type === 'model_select') return renderModelSelect(f, sysModels);
    const v = settings[f.k];
    if (f.k === 'forced_tool_call_mode') {
      // 把模型清单下拉内联到模式选择器右侧。
      const modelsField = SETTINGS_FIELDS.find(x => x.k === 'forced_tool_call_models');
      const sel = `<select id="set_${f.k}">${f.opts.map(o => `<option ${o === v ? 'selected' : ''}>${o}</option>`).join('')}</select>`;
      const head = `<div class="trailing-head"><label for="set_${f.k}">${f.label}</label>${renderModelSelect(modelsField, sysModels)}</div>`;
      return `<div class="field trailing-fix"><div class="min-w-0">${head}${f.desc ? `<div class="desc mt-4px">${f.desc}</div>` : ''}</div>${sel}</div>`;
    }
    if (f.type === 'bool') {
      const toggle = `<label class="toggle"><input type="checkbox" id="set_${f.k}" ${v ? 'checked' : ''}><span class="track"></span></label>`;
      return `<div class="field bool"><div class="min-w-0"><label for="set_${f.k}">${f.label}</label>${f.desc ? `<div class="desc mt-4px">${f.desc}</div>` : ''}</div>${toggle}</div>`;
    }
    let input;
    if (f.type === 'select') input = `<select id="set_${f.k}">${f.opts.map(o => `<option ${o === v ? 'selected' : ''}>${o}</option>`).join('')}</select>`;
    else input = `<input type="${f.type}" id="set_${f.k}" value="${v ?? ''}" ${f.max !== undefined ? `max="${f.max}" oninput="if(this.value!=='' && parseInt(this.value)>${f.max}) this.value='${f.max}'"` : ''} ${f.min !== undefined ? `min="${f.min}"` : ''}>`;
    return `<div class="field"><label for="set_${f.k}">${f.label}</label>${input}${f.desc ? `<div class="desc">${f.desc}</div>` : ''}</div>`
}

// collectSettingValue 读取单个字段的当前 UI 值；元素不存在时返回 undefined（调用方跳过）。
// 只处理非 model_select 字段——那类字段的取值由 collectModelSelect 负责。
function collectSettingValue(f) {
  const el = document.getElementById('set_' + f.k);
  if (!el) return undefined;
  if (f.type === 'bool') return el.checked;
  if (f.type === 'number') return parseInt(el.value || '0', 10);
  return el.value;
}

async function loadSettings() {
  const [d, modelsRes] = await Promise.all([
    API.settings.get(),
    API.models.get().catch(() => ({ models: [] })),
  ]);
  curSettings = d.settings || d;
  const sysModels = modelsRes.models || [];

  const fld = (f) => settingFieldHTML(f, curSettings, sysModels);

  // 【核心修改：定义视觉功能分组】
  const groups = {
    pool: { title: '🚀 并发与连接', fields: [] },
    // 故障处理相关参数不在设置页渲染：它们已随「节点故障策略与记录」页一起迁入该页
    // （见 fault: '探测与保护参数' 页签），避免跨页操作。此处仅保留元数据单一来源。
    fault: { title: '🩺 故障处理策略运行参数', fields: [] },
    rt: { title: '🔐 rT Token 抓取策略', fields: [] },
    rtpool: { title: '♻️ rT Token 预取池', fields: [] },
    core: { title: '🛠 核心控制与基础参数', fields: [] },
    security: { title: '🛡 安全增强与模型策略', fields: [] },
    network: { title: '🌐 IPv6 出口策略', fields: [] },
    httplog: { title: '📋 HTTP 请求级日志', fields: [] },
    dbreten: { title: '🗄️ 历史保留与数据库体积', fields: [] }
  };

  SETTINGS_FIELDS.forEach(f => {
    if (groups[f.group]) {
      groups[f.group].fields.push(f);
    }
  });

  let sectionsHtml = '';
  for (const [key, g] of Object.entries(groups)) {
    // fault 分组的字段在「节点故障策略与记录 → 探测与保护参数」页渲染
    if (key === 'fault') continue;
    const numFields = g.fields.filter(f => f.type !== 'bool' && f.type !== 'model_select');
    const boolFields = g.fields.filter(f => f.type === 'bool');

    let extraHtml = '';
    if (key === 'pool') {
      // 兼容提示：这些参数已迁到「节点故障策略与记录」页，原位置不再渲染字段。
      // 不留提示的话，熟悉旧版面的用户会以为配置项被删了（而不是搬走了）。
      extraHtml = `
        <div class="field" style="margin-top:12px; display:flex; justify-content:space-between; align-items:center; gap:12px; background:rgba(255,255,255,0.03); padding:14px; border-radius:10px; border:1px solid var(--stroke);">
          <div>
            <div style="font-weight:600; font-size:14px;">节点故障与探测参数</div>
            <div class="desc" style="margin-top:4px;">探测失败阈值默认值、自动探测开关/周期/并发、全局故障保护，以及按类别的处理方式，都已集中到「节点故障策略与记录」页。</div>
          </div>
          <button type="button" class="btn ghost" style="padding:8px 16px;white-space:nowrap;" onclick="go('faults')">前往该页</button>
        </div>
      `;
    }
    if (key === 'security') {
      extraHtml = `
        <div class="field" style="margin-top:12px; display:flex; justify-content:space-between; align-items:center; background:rgba(255,255,255,0.03); padding:14px; border-radius:10px; border:1px solid var(--stroke);">
          <div>
            <div style="font-weight:600; font-size:14px;">管理后台密码</div>
            <div class="desc" style="margin-top:4px;">定期修改密码有助于保障管理后台及节点会话安全</div>
          </div>
          <button type="button" class="btn ghost" style="padding:8px 16px;" onclick="showChangePasswordModal()">修改密码</button>
        </div>
      `;
    }

    sectionsHtml += `
      <div class="settings-section-title">${g.title}</div>
      ${numFields.length ? `<div class="grid grid-2">${numFields.map(fld).join('')}</div>` : ''}
      ${boolFields.length ? `<div class="grid grid-2" style="margin-top:10px;">${boolFields.map(fld).join('')}</div>` : ''}
      ${extraHtml}
    `;
  }

  $('#settingsForm').innerHTML =
    sectionsHtml +
    '<button class="btn mt-14px" onclick="saveSettings()">保存设置</button>';

  $('#settingsForm').addEventListener('input', () => window.hasUnsavedSettings = true);
  $('#settingsForm').addEventListener('change', () => window.hasUnsavedSettings = true);
  window.hasUnsavedSettings = false;

  if (!window._hasSettingsUnloadListener) {
    window.addEventListener('beforeunload', (e) => {
      if (window.hasUnsavedSettings) {
        e.preventDefault();
        e.returnValue = '';
      }
    });
    window._hasSettingsUnloadListener = true;
  }

  const parallelEl = $('#set_parallel_pool_enabled');
  if (parallelEl) {
    const updateRetryDisabled = () => {
      // 并发池关闭时禁用并发数配置（重试预算已统一按 max_retries 生效）
      const parallelSizeEl = $('#set_parallel_pool_size');
      if (parallelSizeEl) {
        parallelSizeEl.disabled = !parallelEl.checked;
        const sizeContainer = parallelSizeEl.closest('.field');
        if (sizeContainer) {
          sizeContainer.style.opacity = parallelEl.checked ? '1' : '0.5';
          sizeContainer.style.pointerEvents = parallelEl.checked ? '' : 'none';
        }
      }
    };
    updateRetryDisabled();
    parallelEl.addEventListener('change', updateRetryDisabled);
  }

  ['forced_tool_call_models'].forEach(wireModelSelect);
}

// wireModelSelect 绑定单个模型下拉的计数刷新与开合交互。
// 抽成函数供多处复用：下拉交互原先硬编码在 trailing_fix_models 上，新增清单时直接复用。
function wireModelSelect(key) {
  const ddBtn = $('#set_' + key + '_dd_btn');
  const ddPanel = $('#set_' + key + '_dd_panel');
  if (!ddBtn || !ddPanel) return;

  const updateDdCount = () => {
    const n = collectModelSelect({ k: key }).length;
    const countEl = $('#set_' + key + '_dd_count');
    if (countEl) countEl.textContent = '已选 ' + n;
    ddBtn.innerHTML = `已选 ${n} 个模型 <span class="model-dd-caret">▾</span>`;
    window.hasUnsavedSettings = true;
  };

  ddBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    ddPanel.classList.toggle('hidden');
  });
  ddPanel.addEventListener('change', (e) => {
    if (e.target.matches('input[type="checkbox"]')) updateDdCount();
  });
  ddPanel.querySelectorAll('button[data-act]').forEach(b => {
    b.addEventListener('click', () => {
      ddPanel.querySelectorAll('input[type="checkbox"]').forEach(cb => { cb.checked = b.dataset.act === 'all'; });
      updateDdCount();
    });
  });
  if (!window._hasModelDdListener) {
    document.addEventListener('click', (e) => {
      if (!e.target.closest('.model-dd')) {
        document.querySelectorAll('.model-dd-panel:not(.hidden)').forEach(p => p.classList.add('hidden'));
      }
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        document.querySelectorAll('.model-dd-panel:not(.hidden)').forEach(p => p.classList.add('hidden'));
      }
    });
    window._hasModelDdListener = true;
  }
}

async function saveSettings() {
  const out = {};
  for (const f of SETTINGS_FIELDS) {
    // 故障处理相关参数已迁到「节点故障策略与记录」页并由 saveFaultParams 提交；
    // 这里必须跳过，否则在设置页点保存会把另一个页面上（可能未保存）的改动一起写进去。
    if (f.group === 'fault') continue;
    if (f.type === 'model_select') {
      out[f.k] = collectModelSelect(f);
      continue;
    }
    const v = collectSettingValue(f);
    if (v === undefined) continue;
    out[f.k] = v;
  }
  await API.settings.put(out); toast('设置已保存');
  window.hasUnsavedSettings = false;
  await loadSettings();
}

function showChangePasswordModal() {
  $('#oldPwInput').value = '';
  $('#newPwInput').value = '';
  $('#confirmPwInput').value = '';
  $('#changePwErr').textContent = '';
  $('#changePasswordModal').classList.remove('hidden');
}

function closeChangePasswordModal() {
  $('#changePasswordModal').classList.add('hidden');
}

async function submitChangePassword() {
  const oldPw = $('#oldPwInput').value;
  const newPw = $('#newPwInput').value;
  const confirmPw = $('#confirmPwInput').value;
  const errEl = $('#changePwErr');

  if (!oldPw) { errEl.textContent = '请输入原密码'; return; }
  if (newPw.length < 6) { errEl.textContent = '新密码长度至少需要 6 个字符'; return; }
  if (newPw !== confirmPw) { errEl.textContent = '两次输入的新密码不一致'; return; }

  errEl.textContent = '';
  try {
    await API.changePassword(oldPw, newPw);
    closeChangePasswordModal();
    toast('密码修改成功，请使用新密码重新登录！');
    setTimeout(() => { logout(); }, 1500);
  } catch (e) {
    errEl.textContent = e.message || '修改密码失败';
  }
}