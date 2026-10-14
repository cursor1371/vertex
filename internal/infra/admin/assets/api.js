const API = {
  async raw(path, opts) {
    const r = await fetch(path, Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts));
    if (r.status === 401 && path !== '/api/admin/login' && path !== '/api/admin/password') {
      showLogin(); throw new Error('未登录');
    }
    const ct = r.headers.get('content-type') || '';
    const body = ct.includes('json') ? await r.json() : await r.text();
    if (!r.ok) throw new Error((body && body.error && body.error.message) || body || ('HTTP ' + r.status));
    return body;
  },
  // rawBody 返回原始响应而不抛错：供需要读取失败详情（如分阶段结果）的调用使用。
  // API.raw 只保留 error.message，会丢掉随失败一起返回的 result。
  async rawBody(path, opts) {
    const r = await fetch(path, Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts));
    if (r.status === 401 && path !== '/api/admin/login' && path !== '/api/admin/password') {
      showLogin(); throw new Error('未登录');
    }
    const ct = r.headers.get('content-type') || '';
    const body = ct.includes('json') ? await r.json() : await r.text();
    return { ok: r.ok, status: r.status, body };
  },
  checkAuth() { return this.raw('/api/admin/check-auth'); },
  login(password) { return this.raw('/api/admin/login', { method: 'POST', body: JSON.stringify({ password }) }); },
  logout() { return this.raw('/api/admin/logout', { method: 'POST' }); },
  changePassword(oldPw, newPw) { return this.raw('/api/admin/password', { method: 'POST', body: JSON.stringify({ old_password: oldPw, new_password: newPw }) }); },
  settings: {
    get() { return API.raw('/api/admin/settings'); },
    put(v) { return API.raw('/api/admin/settings', { method: 'PUT', body: JSON.stringify({ settings: v }) }); },
  },
  fingerprint: {
    get() { return API.raw('/api/admin/fingerprint'); },
  },
  stats: {
    get() { return API.raw('/api/admin/stats'); },
    history(qs) { return API.raw('/api/admin/stats/history' + (qs ? '?' + qs : '')); },
    reset() { return API.raw('/api/admin/stats/reset', { method: 'POST' }); },
    active() { return API.raw('/api/admin/stats/active'); },
    rtTokens(limit) { return API.raw('/api/admin/stats/rt-token?limit=' + (limit || 30)); },
    // rtProbe 只取 rT 预取池暂停状态（不拉事件表）：补货暂停期间的低频探测用。
    rtProbe() { return API.raw('/api/admin/stats/rt-token?probe=1'); },
    stopRequest(reqId) { return API.raw('/api/admin/requests/stop', { method: 'POST', body: JSON.stringify({ req_id: reqId }) }); },
    records(qs) { return API.raw('/api/admin/stats/records' + (qs ? '?' + qs : '')); },
    nodes(days) { return API.raw('/api/admin/stats/nodes?days=' + (days || 0)); },
    racing(days, bucket) { return API.raw('/api/admin/stats/racing?days=' + (days || 1) + '&bucket=' + (bucket || 'hour')); },
    racingReset() { return API.raw('/api/admin/stats/racing/reset', { method: 'POST' }); },
  },
  db: {
    stats() { return API.raw('/api/admin/db/stats'); },
    prune(days) { return API.raw('/api/admin/db/prune', { method: 'POST', body: JSON.stringify({ days }) }); },
    clear(table) { return API.raw('/api/admin/db/clear', { method: 'POST', body: JSON.stringify({ table }) }); },
    vacuum(mode) { return API.raw('/api/admin/db/vacuum', { method: 'POST', body: JSON.stringify({ mode }) }); },
    check() { return API.raw('/api/admin/db/check', { method: 'POST' }); },
    // maintain 用 rawBody：阶段失败会返回 500 且响应体仍带完整分阶段结果，
    // 需要把失败详情展示给用户，而不是只弹一句错误。
    maintain(days, initIncremental) {
      return API.rawBody('/api/admin/db/maintain', {
        method: 'POST',
        body: JSON.stringify({ days, init_incremental: !!initIncremental }),
      });
    },
  },
  httpLog: {
    list(qs) { return API.raw('/api/admin/http-log' + (qs ? '?' + qs : '')); },
    detail(id) { return API.raw('/api/admin/http-log/detail?id=' + id); },
    reset() { return API.raw('/api/admin/http-log/reset', { method: 'POST' }); },
    exportUrl(qs) { return '/api/admin/http-log/export' + (qs ? '?' + qs : ''); },
    importData(items) {
      return API.raw('/api/admin/http-log/import', {
        method: 'POST',
        body: JSON.stringify({ items }),
      });
    },
  },
  logs: {
    reset() { return API.raw('/api/admin/log/reset', { method: 'POST' }); },
  },
  keys: {
    list() { return API.raw('/api/admin/keys'); },
    add(n, k, desc) { return API.raw('/api/admin/keys', { method: 'POST', body: JSON.stringify({ name: n, key: k, description: desc }) }); },
    del(n) { return API.raw('/api/admin/keys/' + encodeURIComponent(n), { method: 'DELETE' }); },
  },
  models: {
    get() { return API.raw('/api/admin/models'); },
    // put 提交完整快照（六字段）：models/alias_map/family_map 完整替换；
    // disabled_models/custom_models/model_options 显式给出即整字段替换（缺省=保持现值）。
    put(payload) { return API.raw('/api/admin/models', { method: 'PUT', body: JSON.stringify(payload) }); },
    // fetchUpstream 用 rawBody：失败（ok:false）时响应体仍带 attempts/错误摘要等诊断字段，
    // 需要把完整 body 交给页面展示，而不是只抛一句错误。
    async fetchUpstream(body) {
      const r = await API.rawBody('/api/admin/models/fetch-upstream', { method: 'POST', body: JSON.stringify(body || {}) });
      return r.body || {};
    },
  },
  nodes: {
    list() { return API.raw('/api/admin/nodes'); },
    stats(days) { return API.raw('/api/admin/nodes/stats?days=' + (days || 0)); },
    delete(uri) { return API.raw('/api/admin/nodes', { method: 'DELETE', body: JSON.stringify({ raw_uri: uri }) }); },
    test(uri, opts) { return API.raw('/api/admin/nodes/test', { method: 'POST', body: JSON.stringify(Object.assign({ raw_uri: uri, auto_disable: true, timeout_seconds: 25 }, opts || {})) }); },
    enable(uri) { return API.raw('/api/admin/nodes/enable', { method: 'POST', body: JSON.stringify({ raw_uri: uri }) }); },
    testAll() { return API.raw('/api/admin/nodes/test-all', { method: 'POST' }); },
    testProgress() { return API.raw('/api/admin/nodes/test-progress', { method: 'GET' }); },
    testPause() { return API.raw('/api/admin/nodes/test-pause', { method: 'POST' }); },
    testResume() { return API.raw('/api/admin/nodes/test-resume', { method: 'POST' }); },
    testTerminate() { return API.raw('/api/admin/nodes/test-terminate', { method: 'POST' }); },
    dedup() { return API.raw('/api/admin/nodes/deduplicate', { method: 'POST' }); },
    deleteDisabled() { return API.raw('/api/admin/nodes/disabled', { method: 'DELETE' }); },
    import(text, replace) { return API.raw('/api/admin/nodes/import', { method: 'POST', body: JSON.stringify({ text, replace }) }); },
    batchEnable(uris) { return API.raw('/api/admin/nodes/batch-enable', { method: 'POST', body: JSON.stringify({ uris }) }); },
    batchDisable(uris) { return API.raw('/api/admin/nodes/batch-disable', { method: 'POST', body: JSON.stringify({ uris }) }); },
    batchDelete(uris) { return API.raw('/api/admin/nodes/batch-delete', { method: 'POST', body: JSON.stringify({ uris }) }); },
    sort(desc) { return API.raw('/api/admin/nodes/sort', { method: 'POST', body: JSON.stringify({ desc: !!desc }) }); },
    lock(uris, locked) { return API.raw('/api/admin/nodes/lock', { method: 'POST', body: JSON.stringify({ uris, locked: !!locked }) }); },
    lockHealthy() { return API.raw('/api/admin/nodes/lock-healthy', { method: 'POST' }); },
    unlockAll() { return API.raw('/api/admin/nodes/unlock-all', { method: 'POST' }); },
  },
  faultPolicies: {
    list() { return API.raw('/api/admin/fault-policies'); },
    save(policies) { return API.raw('/api/admin/fault-policies', { method: 'PUT', body: JSON.stringify({ policies }) }); },
    reset() { return API.raw('/api/admin/fault-policies', { method: 'DELETE' }); },
    resetClass(cls) { return API.raw('/api/admin/fault-policies?class=' + encodeURIComponent(cls), { method: 'DELETE' }); },
  },
  nodeFaults: {
    list(limit) { return API.raw('/api/admin/nodes/faults' + (limit ? '?limit=' + limit : '')); },
    clear() { return API.raw('/api/admin/nodes/faults', { method: 'DELETE' }); },
  },
  subscriptions: {
    fetch(url) { return API.raw('/api/admin/subscriptions/fetch', { method: 'POST', body: JSON.stringify({ url }) }); },
  },
  proxyNodes: {
    list() { return API.raw('/api/admin/proxy-nodes', { method: 'GET' }); },
    import(uri) { return API.raw('/api/admin/proxy-nodes/import', { method: 'POST', body: JSON.stringify({ raw_uri: uri }) }); },
    importBatch(uris) { return API.raw('/api/admin/proxy-nodes/import-batch', { method: 'POST', body: JSON.stringify({ uris }) }); },
    toggle(uris, disabled) { return API.raw('/api/admin/proxy-nodes/toggle', { method: 'POST', body: JSON.stringify({ uris, disabled: !!disabled }) }); },
    batchDelete(uris) { return API.raw('/api/admin/proxy-nodes/batch-delete', { method: 'POST', body: JSON.stringify({ uris }) }); },
    deleteDisabled() { return API.raw('/api/admin/proxy-nodes/delete-disabled', { method: 'POST' }); },
    dedup() { return API.raw('/api/admin/proxy-nodes/dedup', { method: 'POST' }); },
    test(uri, opts) { return API.raw('/api/admin/proxy-nodes/test', { method: 'POST', body: JSON.stringify(Object.assign({ raw_uri: uri, timeout_seconds: 25 }, opts || {})) }); },
    lock(uris, locked) { return API.raw('/api/admin/proxy-nodes/lock', { method: 'POST', body: JSON.stringify({ uris, locked: !!locked }) }); },
    unlockAll() { return API.raw('/api/admin/proxy-nodes/unlock-all', { method: 'POST' }); },
  },
  useNode(uri) { return this.raw('/api/admin/use-node', { method: 'POST', body: JSON.stringify({ raw_uri: uri }) }); },
};