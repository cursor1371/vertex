var testProgressTimer = null;
function showApp() { const lg = $('#login'), app = $('#app'); if (!lg.classList.contains('hidden')) { lg.classList.add('out'); setTimeout(() => lg.classList.add('hidden'), 420); } app.classList.remove('hidden'); requestAnimationFrame(() => app.classList.add('in')); go('overview', true); }
function showLogin() { const lg = $('#login'), app = $('#app'); app.classList.remove('in'); setTimeout(() => app.classList.add('hidden'), 360); lg.classList.remove('hidden', 'out'); }
async function login() { $('#loginErr').textContent = ''; try { await API.login($('#pw').value); showApp(); } catch (e) { $('#loginErr').textContent = '密码错误或登录失败'; } }
async function logout() { try { await API.logout(); } catch (e) {} if (typeof stopMonitor === 'function') stopMonitor(); showLogin(); }
$('#pw').addEventListener('keydown', (e) => { if (e.key === 'Enter') login(); });

const LOADERS = { overview: loadOverview, settings: loadSettings, keys: loadKeys, models: loadModels, nodes: loadNodes, faults: loadFaultsPage, appearance: loadAppearance, logs: loadLogs, httplog: loadHTTPLog, fingerprint: loadFingerprint, db: loadDB };
let curPage = null;
function go(page, instant) {
  if (curPage === 'settings' && page !== 'settings' && window.hasUnsavedSettings) {
    showConfirm('您对设置进行了修改且未保存，离开将丢失这些更改。确认离开吗？', () => {
      window.hasUnsavedSettings = false;
      go(page, instant);
    });
    return;
  }
  if (curPage === 'faults' && page !== 'faults' && window.hasUnsavedFaultParams) {
    showConfirm('「探测与保护参数」有未保存的修改，离开将丢失这些更改。确认离开吗？', () => {
      window.hasUnsavedFaultParams = false;
      go(page, instant);
    });
    return;
  }
  if (curPage === 'fingerprint' && page !== 'fingerprint' && window.hasUnsavedFingerprint) {
    showConfirm('浏览器指纹页有未保存的修改，离开将丢失这些更改。确认离开吗？', () => {
      window.hasUnsavedFingerprint = false;
      go(page, instant);
    });
    return;
  }
  if (curPage === 'models' && page !== 'models' && window.hasUnsavedModels) {
    showConfirm('模型页有未保存的修改（含表格编辑与差异勾选），离开将丢失这些更改。确认离开吗？', () => {
      window.hasUnsavedModels = false;
      go(page, instant);
    });
    return;
  }

  document.querySelectorAll('nav button').forEach(b => b.classList.toggle('active', b.dataset.page === page));
  const next = $('#page-' + page), cur = curPage && $('#page-' + curPage);
  if (curPage === 'nodes' && testProgressTimer) { clearInterval(testProgressTimer); testProgressTimer = null; }
  if (curPage === 'overview' && page !== 'overview' && typeof stopMonitor === 'function') { stopMonitor(); }
  curPage = page;
  const enter = () => {
    next.classList.add('entering');
    next.classList.remove('hidden');
    requestAnimationFrame(() => requestAnimationFrame(() => {
      next.classList.remove('entering');
      (LOADERS[page] || (() => {}))();
    }));
  };
  if (cur && cur !== next && !instant) {
    cur.classList.add('leaving');
    setTimeout(() => {
      cur.classList.add('hidden');
      cur.classList.remove('leaving');
      enter();
    }, 150);
  } else {
    document.querySelectorAll('main section').forEach(s => { if (s !== next) s.classList.add('hidden'); });
    enter();
  }
}
function toggleMenu() {
  document.querySelector('aside').classList.toggle('open');
  document.getElementById('menuOverlay').classList.toggle('open');
}
function closeMenu() {
  document.querySelector('aside').classList.remove('open');
  document.getElementById('menuOverlay').classList.remove('open');
}
document.querySelectorAll('nav button').forEach(b => b.onclick = () => go(b.dataset.page));
document.querySelectorAll('nav button').forEach(b => b.addEventListener('click', closeMenu));

(async () => { try { const r = await API.checkAuth(); if (r.authenticated) { showApp(); } else { showLogin(); } } catch (e) { showLogin(); } })();
