// 数据库页：体积体检、一键清理并回收、历史清理、空间回收与完整性检查。

// fmtBytes 把字节数格式化为易读文本（与后端 humanBytes 口径一致）。
function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(2) + ' ' + units[i];
}

// fmtTS 把 unix 秒格式化为本地时间；0/空返回占位符。
function fmtTS(ts) {
  const n = Number(ts) || 0;
  if (!n) return '—';
  const d = new Date(n * 1000);
  const p = x => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

// autoVacuumName 把 PRAGMA auto_vacuum 的取值翻成可读名。
function autoVacuumName(v) {
  return { 0: 'NONE（删除后不回收）', 1: 'FULL', 2: 'INCREMENTAL（可增量回收）' }[Number(v)] || String(v);
}

// reclaimOutcomeText 把后端回收结论翻成用户能读懂的一句话。
// 关键：区分"没有空闲页可回收""当前模式不支持增量回收""回收未生效"，
// 不能把 0 字节一律说成"已释放空间"。
function reclaimOutcomeText(outcome) {
  return {
    reclaimed: '已回收空闲页',
    no_free_pages: '没有空闲页可回收',
    mode_not_incremental: '未回收：当前 auto_vacuum 不是 INCREMENTAL',
    migrated: '已初始化增量回收并压缩文件',
    no_effect: '未回收：增量回收未生效',
    skipped: '未执行回收',
  }[outcome] || String(outcome || '');
}

// 一键操作各阶段的展示名。
const DB_STAGE_LABEL = {
  stats_before: '回收前快照',
  prune: '清理超期记录',
  reclaim: '空间回收',
  checkpoint: 'WAL checkpoint',
  verify: '完整性校验',
  stats_after: '回收后快照',
};

async function loadDB() {
  let d;
  try {
    d = await API.db.stats();
  } catch (e) {
    toast('数据库体检失败：' + (e && e.message ? e.message : e), 'error');
    return;
  }

  $('#dbSpaceRows').innerHTML =
    '<tr><td>数据库文件</td><td>' + fmtBytes(d.file_bytes) + '</td></tr>' +
    '<tr><td>WAL / SHM</td><td>' + fmtBytes(d.wal_bytes) + ' / ' + fmtBytes(d.shm_bytes) + '</td></tr>' +
    '<tr><td>页大小 / 总页数</td><td>' + fmtBytes(d.page_size) + ' / ' + d.page_count + '</td></tr>' +
    '<tr><td>空闲页</td><td>' + d.freelist_pages + '（' + fmtBytes(d.freelist_bytes) + '）</td></tr>' +
    '<tr><td>可回收比例</td><td>' + ((d.reclaim_ratio || 0) * 100).toFixed(1) + '%</td></tr>' +
    '<tr><td>auto_vacuum</td><td>' + esc(autoVacuumName(d.auto_vacuum)) + '</td></tr>' +
    '<tr><td>journal_mode</td><td>' + esc(d.journal_mode) + '</td></tr>' +
    '<tr><td>磁盘可用 / 总容量</td><td>' + fmtBytes(d.disk_free_bytes) + ' / ' + fmtBytes(d.disk_total_bytes) + '</td></tr>';

  $('#dbTableRows').innerHTML = (d.tables || []).map(t =>
    '<tr><td>' + esc(t.name) + '</td><td>' + t.rows + '</td><td>' + fmtTS(t.min_ts) + '</td><td>' + fmtTS(t.max_ts) + '</td></tr>'
  ).join('');

  // 增量回收仅在 auto_vacuum=INCREMENTAL 时有效，据此给出可执行的提示。
  const hint = Number(d.auto_vacuum) === 2
    ? '当前已开启 auto_vacuum=INCREMENTAL，增量回收可把空闲页归还文件系统；回收后需 checkpoint 才会真正截断主库文件。'
    : '当前 auto_vacuum=' + esc(autoVacuumName(d.auto_vacuum)) + '，增量回收不会缩小文件；'
      + '请点「初始化增量回收」做一次性迁移（此后日常清理即可自动回收），或在停服后运行离线压缩。';
  $('#dbReclaimHint').textContent = hint;
}

// setMaintainProgress 更新一键操作的阶段提示。
function setMaintainProgress(text) {
  $('#dbMaintainProgress').textContent = text || '';
}

// renderMaintainResult 渲染一键操作的完整结果（成功与失败都渲染，不只弹 toast）。
function renderMaintainResult(res, httpFailed) {
  if (!res) return;
  setMaintainProgress('');
  const failed = !res.ok || httpFailed;
  const before = res.stats_before || {};
  const after = res.stats_after || {};
  const del = res.deleted || {};
  const delKeys = Object.keys(del);

  const rows = [];
  rows.push(['结论', esc(res.message || '') + (failed && res.failed_stage ? '（失败于阶段：' + esc(DB_STAGE_LABEL[res.failed_stage] || res.failed_stage) + '）' : '')]);
  rows.push(['删除行数', delKeys.length ? (delKeys.map(k => esc(k) + ' ' + del[k] + ' 行').join('，') + '（合计 ' + (res.deleted_total || 0) + '）') : '0']);
  rows.push(['空间回收', esc(reclaimOutcomeText(res.reclaim_outcome))]);
  rows.push(['实际回收（主库）', fmtBytes(res.reclaimed_bytes || 0) + '，空闲页减少 ' + fmtBytes(res.freelist_freed_bytes || 0)]);
  rows.push(['主库文件', fmtBytes(before.file_bytes) + ' → ' + fmtBytes(after.file_bytes)]);
  rows.push(['WAL / SHM', fmtBytes(before.wal_bytes) + ' → ' + fmtBytes(after.wal_bytes)
    + '（WAL 单独计量，不计入主库回收量）'
    + (res.checkpoint && res.checkpoint.busy ? '，checkpoint 受阻：有连接在读' : '')]);
  rows.push(['auto_vacuum', esc(autoVacuumName(res.auto_vacuum_before)) + ' → ' + esc(autoVacuumName(res.auto_vacuum_after))]);
  rows.push(['完整性校验', esc(res.quick_check || '—')]);

  const stageRows = (res.stages || []).map(s =>
    '<tr><td>' + (s.ok ? '✅' : '❌') + '</td><td>' + esc(DB_STAGE_LABEL[s.name] || s.name) + '</td><td>' + esc(s.detail || '') + '</td></tr>'
  ).join('');

  $('#dbMaintainResult').innerHTML =
    '<div class="table-wrap" style="margin-top:10px;"><table class="stats-table"><tbody>' +
    rows.map(r => '<tr><td style="width:170px;">' + r[0] + '</td><td>' + r[1] + '</td></tr>').join('') +
    '</tbody></table></div>' +
    (stageRows ? '<div class="table-wrap" style="margin-top:10px;"><table class="stats-table"><thead><tr><th></th><th>阶段</th><th>结果</th></tr></thead><tbody>' + stageRows + '</tbody></table></div>' : '');
}

// dbMaintain 一键「清理并回收空间」。
async function dbMaintain() {
  const days = parseInt($('#dbMaintainDays').value, 10);
  if (isNaN(days) || days < 0 || days > 3650) {
    toast('保留天数需在 0~3650 之间（0 = 不清理历史）', 'error');
    return;
  }
  const init = $('#dbMaintainInit').checked;
  const cutoff = days > 0 ? new Date(Date.now() - days * 86400000) : null;

  let msg = days > 0
    ? '将删除 race_log / usage_log / http_log 三张日志表中 ' + fmtTS(Math.floor(cutoff.getTime() / 1000)) + '（' + days + ' 天）之前的记录，此操作不可恢复；保留期内记录与节点/密钥等非日志数据不受影响。随后执行空间回收、WAL checkpoint 与完整性校验。'
    : '保留天数填 0：不删除任何记录，仅执行空间回收、WAL checkpoint 与完整性校验。';
  if (init) {
    msg += ' 已勾选一次性初始化：若当前未启用增量回收，将重建数据库文件（需约 1.1× 库大小的临时磁盘空间，期间独占数据库连接）。';
  }
  msg += ' 确认继续？';

  showConfirm(msg, async () => {
    const btn = $('#dbMaintainBtn');
    if (btn.disabled) return; // 防重复提交
    btn.disabled = true;
    $('#dbMaintainResult').innerHTML = '';
    setMaintainProgress('执行中：清理 → 回收 → checkpoint → 校验 …');
    try {
      const r = await API.db.maintain(days, init);
      const res = (r.body && r.body.result) ? r.body.result : r.body;
      renderMaintainResult(res, !r.ok);
      if (r.ok && res && res.ok) {
        toast('一键清理并回收完成');
      } else {
        const m = (r.body && r.body.error && r.body.error.message) || (res && res.message) || ('HTTP ' + r.status);
        toast('操作未全部完成：' + m, 'error');
      }
      loadDB(); // 刷新统计区
    } catch (e) {
      setMaintainProgress('');
      toast('请求失败：' + (e && e.message ? e.message : e), 'error');
    } finally {
      btn.disabled = false;
    }
  });
}

async function dbVacuum(mode) {
  const msgs = {
    incremental: '增量回收会归还未使用的空闲页：在线执行、不需要额外磁盘；回收后需 checkpoint 才真正截断主库文件。确认继续？',
    init_incremental: '一次性初始化会把数据库切换到 auto_vacuum=INCREMENTAL 并重建文件（同时压掉历史空洞）。需要约 1.1× 库大小的临时磁盘空间，期间独占数据库连接、可能耗时较久。此后日常清理即可自动回收空间。确认继续？',
    full: '完整 VACUUM 会重建数据库文件，期间独占数据库连接、可能耗时较久，且需要与库等量的临时磁盘空间。确认继续？',
  };
  showConfirm(msgs[mode] || '确认继续？', async () => {
    try {
      const r = await API.db.vacuum(mode);
      if (mode === 'incremental') {
        // 区分"回收了""没空闲页""模式不支持"——不能把 0 字节说成已释放空间。
        if (r.reclaim_outcome === 'reclaimed') {
          toast('增量回收完成，空闲页减少 ' + fmtBytes(r.freed_bytes || 0));
        } else if (r.reclaim_outcome === 'no_free_pages') {
          toast('没有空闲页可回收');
        } else {
          toast(r.message || reclaimOutcomeText(r.reclaim_outcome), 'error');
        }
      } else if (mode === 'init_incremental') {
        toast(r.changed
          ? ('已开启增量回收：主库 ' + fmtBytes(r.file_bytes_before) + ' → ' + fmtBytes(r.file_bytes_after))
          : '当前已是增量回收模式，无需初始化');
      } else {
        toast('完整 VACUUM 完成');
      }
      loadDB();
    } catch (e) {
      toast('回收失败：' + (e && e.message ? e.message : e), 'error');
    }
  });
}

async function dbCheck() {
  $('#dbCheckResult').textContent = '检查中…';
  try {
    const r = await API.db.check();
    $('#dbCheckResult').textContent = r.ok ? '完整性检查通过（ok）' : ('发现问题：' + r.result);
    toast(r.ok ? '完整性检查通过' : '完整性检查发现问题', r.ok ? undefined : 'error');
  } catch (e) {
    $('#dbCheckResult').textContent = '';
    toast('完整性检查失败：' + (e && e.message ? e.message : e), 'error');
  }
}

async function dbPrune() {
  const days = parseInt($('#dbPruneDays').value, 10);
  if (!days || days < 1) { toast('请输入有效的天数（≥1）', 'error'); return; }
  showConfirm('将删除三张日志表中 ' + days + ' 天之前的记录，此操作不可恢复。确认继续？', async () => {
    try {
      const r = await API.db.prune(days);
      const del = r.deleted || {};
      const parts = Object.keys(del).map(k => k + ' ' + del[k] + ' 行');
      toast('已清理：' + (parts.length ? parts.join('，') : '无匹配记录'));
      loadDB();
    } catch (e) {
      toast('清理失败：' + (e && e.message ? e.message : e), 'error');
    }
  });
}

async function dbClear() {
  const table = $('#dbClearTable').value;
  const label = $('#dbClearTable').selectedOptions[0].textContent;
  showConfirm('将清空 ' + label + '，数据不可恢复。确认继续？', async () => {
    try {
      const r = await API.db.clear(table);
      toast('已清空 ' + table + '，删除 ' + r.deleted + ' 行');
      loadDB();
    } catch (e) {
      toast('清空失败：' + (e && e.message ? e.message : e), 'error');
    }
  });
}
