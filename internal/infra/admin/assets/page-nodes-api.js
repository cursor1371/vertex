// page-nodes-api.js — 节点/外观 后台 API 通信（数据操作）
async function addAndFetchSub() {
  const u = $('#subUrl').value.trim();
  if (!u) return toast('请填订阅 URL');
  toast('正在拉取...');
  try {
    const res = await API.subscriptions.fetch(u);
    $('#subUrl').value = '';
    await loadNodes();
    toast('拉取成功，导入了 ' + (res.count || 0) + ' 个节点');
  } catch (e) {
    toast('拉取失败: ' + e.message, 'err');
  }
}
async function testAllNodes() {
  if (testProgressTimer) return toast('已有批量测试正在进行中');
  const d = await API.nodes.list();
  const nodes = d.nodes || [];
  if (!nodes.length) return toast('无可测试节点');

  const enabled = nodes.filter(function (n) { return !n.disabled; });
  if (!enabled.length) return toast('没有已启用的节点可测试');

  toast('后台全量测速任务已提交启动...');
  await API.nodes.testAll();
  startTestProgressPolling();
}
async function terminateTestAll() {
  try {
    await API.nodes.testTerminate();
    if (testProgressTimer) {
      clearInterval(testProgressTimer);
      testProgressTimer = null;
    }
    currentTestPaused = false;
    const progressEl = document.getElementById('testProgress');
    if (progressEl) progressEl.style.display = 'none';
    loadNodes();
    toast('正在终止批量测速...');
  } catch (e) {
    toast(e.message || '操作失败');
  }
}
async function dedupNodes() { await API.nodes.dedup(); loadNodes(); toast('去重完成'); }
async function deleteDisabledNodes() { await API.nodes.deleteDisabled(); loadNodes(); toast('清理完成'); }
async function sortNodesByLatencyToggle() {
  const desc = window.nodeSortDesc;
  await API.nodes.sort(desc);
  window.nodeSortDesc = !desc;
  localStorage.setItem('vproxy_node_sort_desc', window.nodeSortDesc ? '1' : '0');
  updateSortBtnLabel();
  await loadNodes();
  toast(desc ? '已按延迟降序重排节点' : '已按延迟升序重排节点');
}
async function exportNodes() {
  try {
    const d = await API.nodes.list();
    const nodes = d.nodes || [];
    if (nodes.length === 0) {
      toast('没有可导出的节点');
      return;
    }
    const text = nodes.map(n => n.raw_uri).join('\n');
    const blob = new Blob([text], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'nodes.txt';
    a.click();
    URL.revokeObjectURL(url);
    toast('已导出 ' + nodes.length + ' 个节点');
  } catch (e) {
    toast('导出失败: ' + e.message, 'err');
  }
}
async function testSingleNode(uri) {
  toast('正在测试节点...');
  try {
    const result = await API.nodes.test(uri, { auto_disable: true });
    const msg = result.ok
      ? '测试通过 ' + Math.round(result.elapsed_ms) + 'ms'
      : '测试失败 ' + (result.error || '');
    toast(msg);
    await loadNodes();
  } catch (e) {
    toast('测试出错: ' + e.message, 'err');
  }
}
// applyLocalNodeUpdate / refreshNodesView 支撑单行操作的就地更新：
// 此前启用/禁用/防删/锁定每次都调 loadNodes()——5 个并发请求 + 整页 DOM 重建，
// 滚动位置与焦点丢失。改为本地修改内存节点字段后重渲染（删除/去重/导入等
// 改变节点集合的操作仍走全量 loadNodes()）。
function refreshNodesView() {
  renderNodesTable(window.lastNodesList || [], window.lastHealthMap || {});
}
function applyLocalNodeUpdate(uri, mutate) {
  (window.lastNodesList || []).forEach(function (n) {
    if (n.raw_uri === uri) mutate(n);
  });
  refreshNodesView();
}

// setLocalDisabledByHealth 就地改健康映射里的禁用来源标记：启用/禁用后若不改，
// 视图会因仍读到旧标记把节点分进错误的筛选档/徽章（禁用来源存在 health 里，
// 不在节点对象上，见 disabledByHealthOf）。需在 applyLocalNodeUpdate 之前调用。
function setLocalDisabledByHealth(uri, flag) {
  if (!window.lastHealthMap) window.lastHealthMap = {};
  if (!window.lastHealthMap[uri]) window.lastHealthMap[uri] = {};
  window.lastHealthMap[uri].disabled_by_health = !!flag;
}

async function enableNode(uri) {
  try {
    await API.nodes.enable(uri);
    // 人工启用清禁用来源标记（健康映射里），徽章/筛选档与节点对象同步更新。
    setLocalDisabledByHealth(uri, false);
    applyLocalNodeUpdate(uri, function (n) { n.disabled = false; });
    toast('已启用该节点', 'ok');
  } catch (e) { toast('启用失败: ' + e.message, 'err'); }
}
// disableNode 单行禁用：此前启用态节点没有行内禁用按钮，只能勾选后走批量。
// 复用 batch-disable 接口的单 URI 形态；防删锁节点会被后端跳过，故渲染层对
// 防删节点不渲染该按钮（需先解除防删）。
async function disableNode(uri) {
  showConfirm('确定禁用该节点吗？禁用后立即退出调度（可随时手动启用恢复）。', async () => {
    try {
      await API.nodes.batchDisable([uri]);
      // 人工禁用清禁用来源标记：与后端 BatchUpdateNodesDisabled 清 DisabledByHealth 一致，
      // 人工禁用不会被测速成功自动放开。
      setLocalDisabledByHealth(uri, false);
      applyLocalNodeUpdate(uri, function (n) { n.disabled = true; });
      toast('已禁用该节点', 'ok');
    } catch (e) { toast('禁用失败: ' + e.message, 'err'); }
  });
}
// useNode/unuseNode/delNode 此前无 try/catch：失败时 Promise rejection 未处理，
// loadNodes 与成功 toast 都不执行，页面完全静默无反应。
async function useNode(uri) {
  try {
    await API.useNode(uri);
    // 本地同步 active_node_uri（badge 判定依据），loadSettings 异步刷新设置页展示。
    if (typeof curSettings !== 'undefined') curSettings.active_node_uri = uri;
    if (typeof loadSettings === 'function') loadSettings();
    refreshNodesView();
    toast('已锁定使用该节点，并关闭并发池', 'ok');
  } catch (e) { toast('操作失败: ' + e.message, 'err'); }
}
async function unuseNode(uri) {
  try {
    await API.useNode('');
    if (typeof curSettings !== 'undefined') curSettings.active_node_uri = '';
    if (typeof loadSettings === 'function') loadSettings();
    refreshNodesView();
    toast('已取消锁定，并恢复并发池', 'ok');
  } catch (e) { toast('操作失败: ' + e.message, 'err'); }
}
async function delNode(uri) {
  if (!confirm('删除该节点？')) return;
  try {
    await API.nodes.delete(uri);
    await loadNodes();
    toast('已删除', 'ok');
  } catch (e) { toast('删除失败: ' + e.message, 'err'); }
}
// lockNode 为单个节点添加/解除防删锁：上锁后不会被删除、去重移除或自动/批量禁用。
async function lockNode(uri, locked) {
  try {
    await API.nodes.lock([uri], locked);
    applyLocalNodeUpdate(uri, function (n) { n.locked = locked; });
    toast(locked ? '已添加防删锁' : '已解除防删锁');
  } catch (e) { toast('操作失败: ' + e.message, 'err'); }
}
// lockHealthyNodes 一键防删：只给"确实成功过"的节点上锁
// （未禁用 + 测试通过且无连续失败 + 请求级成功次数 > 0）。
async function lockHealthyNodes() {
  try {
    const res = await API.nodes.lockHealthy();
    await loadNodes();
    toast('一键防删：已锁定 ' + (res.count || 0) + ' 个成功过的节点');
  } catch (e) { toast('操作失败: ' + e.message, 'err'); }
}
// unlockAllNodes 一键解除全部出口节点的防删锁（需确认，解除后这些节点重新可被删除/禁用）。
async function unlockAllNodes() {
  showConfirm('确定解除全部节点的防删锁吗？解除后这些节点将重新可被删除、禁用。', async () => {
    try {
      const res = await API.nodes.unlockAll();
      await loadNodes();
      toast('已解除 ' + (res.count || 0) + ' 个节点的防删锁');
    } catch (e) { toast('操作失败: ' + e.message, 'err'); }
  });
}
async function lockProxyNode(uri, locked) {
  try {
    await API.proxyNodes.lock([uri], locked);
    await loadNodes();
    toast(locked ? '已添加防删锁' : '已解除防删锁');
  } catch (e) { toast('操作失败: ' + e.message, 'err'); }
}
// unlockAllProxyNodes 一键解除全部前置节点的防删锁（需确认，解除后重新可被删除/禁用）。
async function unlockAllProxyNodes() {
  showConfirm('确定解除全部前置节点的防删锁吗？解除后这些节点将不再受删除/禁用保护。', async () => {
    try {
      const res = await API.proxyNodes.unlockAll();
      await loadNodes();
      toast('已解除 ' + (res.count || 0) + ' 个前置节点的防删锁');
    } catch (e) { toast('操作失败: ' + e.message, 'err'); }
  });
}
async function batchEnableSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  showConfirm('确定批量启用选中的 ' + uris.length + ' 个节点吗？', async () => {
    toast('批量启用中...');
    try {
      await API.nodes.batchEnable(uris);
      window.selectedNodeURIs.clear();
      await loadNodes();
      toast('已成功启用 ' + uris.length + ' 个节点');
    } catch (e) { toast('操作失败: ' + e.message, 'err'); }
  });
}
async function batchDisableSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  // 二次确认：批量禁用立即影响线上调度（此前零确认，风险门槛低于删除却更高）。
  // 防删锁节点会被后端跳过，提示里说明数量避免"为什么少禁了几个"的困惑。
  const lockedSkipped = (window.lastNodesList || []).filter(function (n) {
    return n.locked && window.selectedNodeURIs.has(n.raw_uri);
  }).length;
  const skipHint = lockedSkipped > 0 ? '\n其中 ' + lockedSkipped + ' 个已加防删锁，将被跳过。' : '';
  showConfirm('确定批量禁用选中的 ' + uris.length + ' 个节点吗？禁用后立即退出调度，可随时手动启用。' + skipHint, async () => {
    toast('批量禁用中...');
    try {
      await API.nodes.batchDisable(uris);
      window.selectedNodeURIs.clear();
      await loadNodes();
      toast('已成功禁用 ' + uris.length + ' 个节点');
    } catch (e) { toast('操作失败: ' + e.message, 'err'); }
  });
}
async function batchDeleteSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  if (!confirm('确定要批量删除选中 ' + uris.length + ' 个节点吗？')) return;
  toast('批量删除中...');
  try {
    await API.nodes.batchDelete(uris);
    window.selectedNodeURIs.clear();
    await loadNodes();
    toast('已成功删除 ' + uris.length + ' 个节点');
  } catch (e) { toast('操作失败: ' + e.message, 'err'); }
}
function importFileNodes(replace) {
  const fileInput = document.getElementById('nodeImportFile');
  if (!fileInput.files.length) return toast('请先选择一个节点配置文件');
  const file = fileInput.files[0];
  const reader = new FileReader();
  toast('正在读取配置文件并解析...');
  reader.onload = async function (e) {
    const text = e.target.result;
    try {
      const res = await API.nodes.import(text, replace);
      await loadNodes();
      fileInput.value = '';
      toast(replace ? '替换成功，导入了 ' + res.count + ' 个节点' : '导入成功，追加了 ' + res.count + ' 个节点');
    } catch (err) {
      toast('文件导入解析失败: ' + err.message, 'err');
    }
  };
  reader.readAsText(file);
}
// ─── 前置代理池 ───
async function batchImportSelectedToProxyPool() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要导入前置代理池的节点');
  showConfirm('确定把选中的 ' + uris.length + ' 个节点导入前置代理池吗？', async () => {
    toast('批量导入前置代理池中...');
    try {
      const res = await API.proxyNodes.importBatch(uris);
      window.selectedNodeURIs.clear();
      await loadNodes();
      toast('已导入前置代理 ' + (res.imported || 0) + ' 个，跳过 ' + (res.skipped || 0) + ' 个');
    } catch (e) {
      toast('批量导入失败: ' + e.message);
    }
  });
}
async function importProxyNode() {
  const uri = $('#globalProxy').value.trim();
  if (!uri) return toast('请先输入代理 URI');
  try {
    await API.proxyNodes.import(uri);
    $('#globalProxy').value = '';
    await loadNodes();
    toast('已导入前置代理并加入轮询池');
  } catch (e) {
    toast('导入失败: ' + e.message);
  }
}
async function testProxyNode(uri) {
  toast('正在测试前置代理...');
  try {
    const res = await API.proxyNodes.test(uri);
    const msg = res.ok
      ? '测试通过 ' + Math.round(res.elapsed_ms) + 'ms'
      : '测试失败 ' + (res.error || '');
    toast(msg);
    await loadNodes();
  } catch (e) {
    toast('测试出错: ' + e.message, 'err');
  }
}
async function testAllProxyNodes() {
  const nodes = window.lastProxyNodes || [];
  if (!nodes.length) return toast('前置代理池为空，无可测试节点');
  toast('开始批量测试 ' + nodes.length + ' 个前置代理...');
  let passCount = 0;
  let doneCount = 0;
  const batchSize = 5;
  for (let i = 0; i < nodes.length; i += batchSize) {
    const chunk = nodes.slice(i, i + batchSize);
    await Promise.all(chunk.map(async (n) => {
      try {
        const res = await API.proxyNodes.test(n.raw_uri);
        if (res && res.ok) passCount++;
      } catch (e) {}
      doneCount++;
    }));
    toast('前置代理测试中: ' + doneCount + '/' + nodes.length + '...');
  }
  await loadNodes();
  toast('前置代理测试完成: ' + passCount + '/' + nodes.length + ' 通过');
}
async function toggleProxyNode(uri, disabled) {
  try {
    await API.proxyNodes.toggle([uri], disabled);
    await loadNodes();
    toast(disabled ? '已禁用该前置代理' : '已启用该前置代理');
  } catch (e) {
    toast('操作失败: ' + e.message, 'err');
  }
}
async function batchToggleProxyNodes(uris, disabled) {
  if (!uris || !uris.length) return toast('请先选择要操作的前置节点');
  try {
    await API.proxyNodes.toggle(uris, disabled);
    await loadNodes();
    toast(disabled ? '已批量禁用 ' + uris.length + ' 个前置节点' : '已批量启用 ' + uris.length + ' 个前置节点');
  } catch (e) {
    toast('批量操作失败: ' + e.message);
  }
}
async function deleteProxyNode(uri) {
  if (!confirm('确定删除该前置代理节点？')) return;
  try {
    await API.proxyNodes.batchDelete([uri]);
    await loadNodes();
    toast('已删除');
  } catch (e) {
    toast('删除失败: ' + e.message);
  }
}
async function batchDeleteProxyNodes(uris) {
  if (!uris || !uris.length) return toast('请选择要删除的前置节点');
  if (!confirm('确定删除所选 ' + uris.length + ' 个前置代理节点？')) return;
  try {
    await API.proxyNodes.batchDelete(uris);
    await loadNodes();
    toast('已删除 ' + uris.length + ' 个前置节点');
  } catch (e) {
    toast('批量删除失败: ' + e.message);
  }
}
async function deleteDisabledProxyNodes() {
  if (!confirm('确定清空所有已禁用的前置代理节点？')) return;
  try {
    const res = await API.proxyNodes.deleteDisabled();
    await loadNodes();
    toast('已清空 ' + (res.deleted_count || 0) + ' 个禁用节点');
  } catch (e) {
    toast('清理失败: ' + e.message);
  }
}
async function dedupProxyNodes() {
  try {
    const res = await API.proxyNodes.dedup();
    await loadNodes();
    toast('去重完成，移除 ' + (res.removed_count || 0) + ' 个');
  } catch (e) {
    toast('去重失败: ' + e.message);
  }
}
