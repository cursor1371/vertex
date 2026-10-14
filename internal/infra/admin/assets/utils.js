function $(s) { return document.querySelector(s); }
// toast(msg, type)：type 可选 'ok' | 'err'，默认 info。此前签名只有 1 个参数，
// 但 7 处调用传了 ', true' 想表达错误样式——被静默忽略，错误与成功提示外观相同。
// err 停留更久（4s）且带红色描边（.toast.err，见 components.css）。
function toast(msg, type) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.remove('toast-ok', 'toast-err');
  if (type === 'ok') t.classList.add('toast-ok');
  if (type === 'err') t.classList.add('toast-err');
  t.classList.add('show');
  clearTimeout(t._hideTimer);
  t._hideTimer = setTimeout(() => t.classList.remove('show'), type === 'err' ? 4000 : 1900);
}
function esc(s) { return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }

// copyText 把文本复制到剪贴板并显示反馈（点击元素复制，如 request_id 列）。
function copyText(text, el) {
  const done = () => {
    if (el) {
      const old = el.textContent;
      el.textContent = '✓ 已复制';
      setTimeout(() => { el.textContent = old; }, 1200);
    }
    toast('已复制');
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done).catch(() => fallbackCopy(text, done));
  } else {
    fallbackCopy(text, done);
  }
}

function fallbackCopy(text, done) {
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
    done();
  } catch (e) {
    toast('复制失败');
  }
}
const _tmap = { vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', ss: 'Shadowsocks', shadowsocks: 'Shadowsocks', hysteria2: 'Hysteria2', hy2: 'Hysteria2', tuic: 'TUIC' };
const DEFAULT_BG = "url('background.jpg')";

function showConfirm(msg, onOk, onCancel) {
  const m = $('#confirmModal');
  if (!m) return;
  $('#confirmModalText').textContent = msg;
  m.classList.remove('hidden');
  
  const okBtn = $('#confirmOkBtn');
  const cancelBtn = $('#confirmCancelBtn');
  
  const cleanup = () => {
    m.classList.add('hidden');
    okBtn.onclick = null;
    cancelBtn.onclick = null;
  };
  
  okBtn.onclick = () => { cleanup(); if(onOk) onOk(); };
  cancelBtn.onclick = () => { cleanup(); if(onCancel) onCancel(); };
}
