'use strict';
// Tudo que vem do agente é texto: o DOM é montado com textContent, nunca innerHTML.

let state = null;
let prevSessions = {};       // sessão -> assinatura do último snapshot (para saber quem mudou)
let active = null;           // sessão aberta no chat
let dirty = false;
const drafts = {};           // sessão -> texto sendo digitado
const picks = {};            // pendência -> { pergunta -> Set(rótulos) }
const others = {};           // pendência -> { pergunta -> texto livre }
const chats = {};            // sessão -> { total, from, msgs, error }
const $ = (s) => document.querySelector(s);

// Quantas mensagens de cada sessão já foram vistas (para o contador de não lidas). Só conveniência local.
const seen = (() => { try { return JSON.parse(localStorage.getItem('seen') || '{}'); } catch { return {}; } })();
const saveSeen = () => { try { localStorage.setItem('seen', JSON.stringify(seen)); } catch {} };
try { active = localStorage.getItem('active'); } catch {}

function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) el.setAttribute(k, v === true ? '' : v);
  }
  for (const k of kids.flat()) if (k !== null && k !== undefined && k !== false) el.append(k instanceof Node ? k : String(k));
  return el;
}

async function api(path, body) {
  const r = await fetch(path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Remoto': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (r.status === 401) location.assign('/pair'); // pela internet (Funnel): aparelho revogado ou não pareado
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || 'HTTP ' + r.status);
  return j;
}

function toast(msg) {
  const b = $('#banner');
  b.textContent = msg; b.hidden = false;
  clearTimeout(toast.t); toast.t = setTimeout(() => (b.hidden = true), 6000);
}

async function act(btn, fn) {
  btn.disabled = true;
  try { await fn(); } catch (e) { toast(e.message); } finally { btn.disabled = false; }
}

const ago = (t) => {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - t));
  return s < 60 ? s + 's' : s < 3600 ? Math.floor(s / 60) + 'min' : Math.floor(s / 3600) + 'h';
};
const hhmm = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const stateName = { waiting: 'aguardando', busy: 'trabalhando', done: 'pronto' };
const sessionById = (id) => (state?.sessions || []).find((s) => s.id === id);
const pendingOf = (id) => (state?.pending || []).filter((p) => p.session === id);
const title = (s) => s.label + (s.name ? ' · ' + s.name : '');
const avatar = (s, small) => h('div', { class: 'avatar ' + (s.kind || '') + (small ? ' small' : ''), 'aria-hidden': 'true' },
  (s.kind || s.label || '?')[0].toUpperCase(), h('i', { class: s.state || '' }));

// Markdown mínimo da resposta (blocos ```, `código`, **negrito**, títulos), montado como DOM, sem HTML.
function md(text) {
  return text.split(/```[^\n]*\n?/).flatMap((part, i) => (i % 2 ? [h('pre', {}, part.replace(/\n$/, ''))] : inline(part)));
}
function inline(t) {
  t = t.replace(/^#{1,6}\s+(.+)$/gm, '**$1**');
  const out = [], re = /\*\*([^*\n]+)\*\*|`([^`\n]+)`/g;
  let last = 0, m;
  while ((m = re.exec(t))) {
    if (m.index > last) out.push(t.slice(last, m.index));
    out.push(m[1] !== undefined ? h('b', {}, m[1]) : h('code', {}, m[2]));
    last = re.lastIndex;
  }
  out.push(t.slice(last));
  return out;
}

// ---------- pendências (perguntas e permissões), dentro do chat da sessão ----------

function pendingCard(p) {
  const head = h('div', { class: 'row' },
    h('div', { class: 'grow' }, h('div', { class: 'title' }, p.kind === 'question' ? '❓ Pergunta' : '🔐 Permissão'),
      h('div', { class: 'mute' }, 'há ' + ago(p.at))));
  const toPC = h('button', { class: 'ghost', onclick: (e) => act(e.target, () => api('/api/reply', { id: p.id, action: 'pc' })) }, 'Responder no PC');
  if (p.kind === 'permission') return h('div', { class: 'card' }, head, permissionBody(p), permissionActions(p, toPC));
  return h('div', { class: 'card' }, head, ...questionBody(p, toPC));
}

function permissionBody(p) {
  const i = p.input || {};
  const detail = i.command || i.CommandLine || i.file_path || i.filePath || i.TargetFile || i.url || i.pattern || JSON.stringify(i, null, 2);
  return [h('div', { style: 'margin-top:8px' }, h('b', {}, p.tool)), i.description ? h('div', { class: 'mute' }, i.description) : null,
    h('pre', {}, String(detail).slice(0, 4000))];
}

function permissionActions(p, toPC) {
  const reason = h('input', { placeholder: 'Motivo ao negar (opcional)' });
  return [h('div', { style: 'margin-top:8px' }, reason), h('div', { class: 'actions' },
    h('button', { onclick: (e) => act(e.target, () => api('/api/reply', { id: p.id, action: 'allow' })) }, 'Permitir'),
    h('button', { class: 'bad', onclick: (e) => act(e.target, () => api('/api/reply', { id: p.id, action: 'deny', message: reason.value })) }, 'Negar'),
    toPC)];
}

function questionBody(p, toPC) {
  const qs = (p.input && p.input.questions) || [];
  const sel = (picks[p.id] ||= {});
  const txt = (others[p.id] ||= {});
  const answerOf = (q) => (txt[q.question] || '').trim() || [...(sel[q.question] || [])].join(', ');
  const send = h('button', {
    onclick: (e) => act(e.target, () => {
      const answers = {};
      for (const q of qs) answers[q.question] = answerOf(q);
      return api('/api/reply', { id: p.id, action: 'answer', answers });
    }),
  }, 'Enviar resposta');
  const refresh = () => { send.disabled = !qs.every((q) => answerOf(q)); };
  const blocks = qs.map((q) => {
    const set = (sel[q.question] ||= new Set());
    const opts = (q.options || []).map((o) => {
      const b = h('button', { class: 'opt' + (set.has(o.label) ? ' sel' : '') },
        h('div', { class: 'title' }, o.label), o.description ? h('div', { class: 'mute' }, o.description) : null);
      b.addEventListener('click', () => {
        if (q.multiSelect) set.has(o.label) ? set.delete(o.label) : set.add(o.label);
        else { set.clear(); set.add(o.label); txt[q.question] = ''; }
        b.parentElement.querySelectorAll('button.opt').forEach((x, i) => x.classList.toggle('sel', set.has(q.options[i].label)));
        refresh();
      });
      return b;
    });
    const other = h('input', { placeholder: 'Outra resposta…', value: txt[q.question] || '' });
    other.addEventListener('input', () => { txt[q.question] = other.value; refresh(); });
    return h('div', { style: 'margin-top:10px' },
      q.header ? h('span', { class: 'badge' }, q.header) : null,
      h('div', { class: 'title', style: 'margin-top:6px' }, q.question + (q.multiSelect ? ' (várias)' : '')),
      h('div', {}, ...opts), h('div', { style: 'margin-top:6px' }, other));
  });
  refresh();
  return [...blocks, h('div', { class: 'actions' }, send, toPC)];
}

// ---------- histórico (lido do transcript da sessão no PC) ----------

const loading = new Set();
async function loadHistory(sid, older) {
  if (loading.has(sid)) return;
  loading.add(sid);
  try { await fetchHistory(sid, older); } finally { loading.delete(sid); }
}

async function fetchHistory(sid, older) {
  const c = chats[sid];
  const from = older && c ? Math.max(0, c.from - 200) : c ? c.from + c.msgs.length : -1;
  try {
    const r = await api('/api/history?session=' + encodeURIComponent(sid) + '&from=' + from);
    if (!c || r.from !== from || older) chats[sid] = { total: r.total, from: r.from, msgs: r.messages, error: r.error };
    else if (r.messages.length) { c.msgs.push(...r.messages); c.total = r.total; }
    else return; // nada novo: não redesenha
    if (seen[sid] === undefined) { seen[sid] = r.total; saveSeen(); } // primeira vez: o que já existe conta como lido
    if (sid === active && !document.hidden) markSeen(sid);
    render();
  } catch (e) {
    if (!c) chats[sid] = { total: 0, from: 0, msgs: [], error: e.message };
  }
}

function markSeen(sid) {
  const c = chats[sid];
  if (c && seen[sid] !== c.total) { seen[sid] = c.total; saveSeen(); }
}

function unread(sid) {
  const c = chats[sid];
  if (!c || seen[sid] === undefined) return 0;
  return c.msgs.filter((m, i) => c.from + i >= seen[sid] && m.role === 'assistant').length;
}

// ponytail: polling de 2,5 s só da sessão aberta; as outras recarregam quando o snapshot delas muda.
setInterval(() => { if (!document.hidden && active) loadHistory(active); }, 2500);

// ---------- lista de sessões ----------

function preview(s) {
  const p = pendingOf(s.id);
  if (p.length) return { text: p[0].kind === 'question' ? '❓ ' + 'Pergunta aguardando' : '🔐 Permissão: ' + p[0].tool, alert: true };
  const c = chats[s.id];
  const last = c && [...c.msgs].reverse().find((m) => m.role !== 'tool');
  if (s.state === 'busy') return { text: s.tool ? '🔧 ' + s.tool + '…' : 'trabalhando…' };
  if (last) return { text: (last.role === 'user' ? 'Você: ' : '') + last.text.replace(/\s+/g, ' ').slice(0, 140), at: last.at };
  return { text: s.reason || s.detail || stateName[s.state] || s.kind };
}

function sessionItem(s) {
  const pv = preview(s);
  const n = pendingOf(s.id).length;
  const u = s.id === active ? 0 : unread(s.id);
  return h('button', { class: 'item' + (s.id === active ? ' active' : ''), onclick: () => select(s.id) },
    avatar(s),
    h('div', { class: 'grow' },
      h('div', { class: 'top' }, h('span', { class: 'name' }, title(s)), pv.at ? h('span', { class: 'when' }, hhmm(pv.at)) : null),
      h('div', { class: 'bottom' }, h('span', { class: 'preview' + (pv.alert ? ' alert' : '') }, pv.text),
        n ? h('span', { class: 'count alert' }, n) : u ? h('span', { class: 'count' }, u) : null)));
}

function renderList() {
  const byProject = {};
  for (const s of state.sessions || []) (byProject[s.project] ||= []).push(s);
  const order = { waiting: 1, busy: 2, done: 3 };
  const rank = (s) => (pendingOf(s.id).length ? 0 : order[s.state] ?? 4);
  const kids = [];
  const projects = Object.keys(byProject).sort();
  for (const proj of projects) {
    const list = byProject[proj].sort((x, y) => rank(x) - rank(y) || x.label.localeCompare(y.label, undefined, { numeric: true }));
    if (projects.length > 1) kids.push(h('div', { class: 'proj' }, proj));
    kids.push(...list.map(sessionItem));
  }
  if (!kids.length) kids.push(h('div', { class: 'empty' }, 'Nenhuma sessão aberta no lich.'));
  $('#list').replaceChildren(...kids);

  const ts = state.tailscale || {};
  $('#notes').replaceChildren(
    ts.warn ? h('div', { class: 'note' }, '⚠ ' + ts.warn) : '',
    state.remote ? '' : h('div', { class: 'note' }, 'Modo remoto desligado: perguntas e permissões aparecem só no PC.'));
}

// ---------- chat ----------

function renderHead(s) {
  const followBtn = $('#btn-follow'), closeBtn = $('#btn-close');
  followBtn.hidden = closeBtn.hidden = !s;
  if (!s) { $('#head-info').replaceChildren(h('div', { class: 't' }, 'lich remoto')); return; }
  const followed = !!(state.follow || {})[s.id];
  followBtn.textContent = followed ? '🔔' : '🔕';
  followBtn.title = followed ? 'Acompanhando: avisa ao terminar. Toque para parar.' : 'Acompanhar: avisar no celular quando terminar';
  followBtn.setAttribute('aria-label', followBtn.title);
  const sub = [stateName[s.state] || '—', s.state === 'busy' && s.tool ? s.tool : '', s.kind, s.project].filter(Boolean).join(' · ');
  $('#head-info').replaceChildren(avatar(s, true), h('div', { class: 'grow' }, h('div', { class: 't' }, title(s)), h('div', { class: 's' }, sub)));
}

function renderMsgs(s) {
  const box = $('#msgs');
  if (!s) {
    box.replaceChildren(h('div', { class: 'empty' }, (state.sessions || []).length ? 'Escolha uma sessão na lista' : 'Nenhuma sessão aberta no lich.\nUse ＋ para abrir uma.'));
    return;
  }
  const c = chats[s.id];
  const kids = [];
  if (!c) { kids.push(h('div', { class: 'sys' }, 'carregando o histórico…')); loadHistory(s.id); }
  else {
    if (c.from > 0) kids.push(h('button', { class: 'more', onclick: (e) => act(e.target, () => loadHistory(s.id, true)) }, 'Carregar anteriores (' + c.from + ')'));
    if (c.error && !c.msgs.length) kids.push(h('div', { class: 'sys' }, c.error));
    else if (!c.msgs.length) kids.push(h('div', { class: 'sys' }, 'Sem mensagens ainda.'));
    for (const m of c.msgs) {
      if (m.role === 'tool') kids.push(h('div', { class: 'msg tool', onclick: (e) => e.currentTarget.classList.toggle('open') }, '🔧 ' + m.text));
      else kids.push(h('div', { class: 'msg ' + m.role }, m.role === 'assistant' ? md(m.text) : m.text, m.at ? h('time', {}, hhmm(m.at)) : null));
    }
  }
  const waitingPhone = ((state.history || {})[s.id] || []).filter((e) => e.status === 'pending').length;
  if (waitingPhone) kids.push(h('div', { class: 'sys' }, '⏳ aguardando a resposta de ' + waitingPhone + ' tarefa(s) enviada(s) daqui'));
  if (s.state === 'busy') kids.push(h('div', { class: 'sys' }, 'trabalhando' + (s.tool ? ' · ' + s.tool : '') + '…'));
  kids.push(...pendingOf(s.id).map(pendingCard));

  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  const switched = box.dataset.sid !== s.id;
  const top = box.scrollTop, oldH = box.scrollHeight;
  const grewUp = !switched && c && Number(box.dataset.from) > c.from; // "Carregar anteriores": mantém a mesma mensagem na tela
  box.replaceChildren(...kids);
  box.dataset.sid = s.id;
  box.dataset.from = c ? c.from : '';
  box.scrollTop = grewUp ? top + box.scrollHeight - oldH : atBottom || switched ? box.scrollHeight : top;
}

function renderComposer(s) {
  const f = $('#composer'), ta = $('#input');
  f.hidden = !s;
  if (!s) return;
  ta.placeholder = s.state === 'waiting' && !pendingOf(s.id).length ? 'A sessão aguarda uma resposta no PC' : 'Mensagem para ' + s.label;
  $('#btn-send').disabled = !ta.value.trim();
}

function renderElsewhere() {
  const el = $('#elsewhere');
  const p = (state.pending || []).find((x) => x.session !== active);
  el.hidden = !p;
  if (p) { el.textContent = (p.kind === 'question' ? '❓ ' : '🔐 ') + p.label + (p.kind === 'question' ? ' pergunta' : ' pede permissão') + ' · abrir'; el.onclick = () => select(p.session); }
}

function render(force) {
  if (!state) return;
  $('#remote').checked = !!state.remote;
  renderList();
  if (active && !sessionById(active)) setActive(null);
  const s = sessionById(active);
  renderHead(s);
  renderComposer(s);
  renderElsewhere();
  // Não redesenha o chat enquanto um campo de uma pendência está em edição (perderia o foco).
  const a = document.activeElement;
  if (!force && a && $('#msgs').contains(a) && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT')) { dirty = true; return; }
  dirty = false;
  renderMsgs(s);
}

function setActive(sid) {
  active = sid;
  try { sid ? localStorage.setItem('active', sid) : localStorage.removeItem('active'); } catch {}
}

function select(sid) {
  if (active) drafts[active] = $('#input').value;
  setActive(sid);
  const ta = $('#input');
  ta.value = drafts[sid] || '';
  autosize();
  document.body.classList.remove('drawer');
  markSeen(sid);
  loadHistory(sid);
  render(true);
}

document.addEventListener('focusout', () => setTimeout(() => dirty && render(), 0));
document.addEventListener('visibilitychange', () => { if (!document.hidden && active) { markSeen(active); loadHistory(active); } });

// ---------- composer ----------

function autosize() {
  const ta = $('#input');
  ta.style.height = '';
  if (ta.value && ta.offsetParent) ta.style.height = Math.min(ta.scrollHeight + 2, 152) + 'px';
}
$('#input').addEventListener('input', () => { autosize(); $('#btn-send').disabled = !$('#input').value.trim(); if (active) drafts[active] = $('#input').value; });
// Enter envia no teclado físico; no celular o Enter quebra linha e o envio é pelo botão.
$('#input').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey && matchMedia('(pointer: fine)').matches) { e.preventDefault(); $('#composer').requestSubmit(); }
});
$('#composer').addEventListener('submit', (e) => {
  e.preventDefault();
  const ta = $('#input'), sid = active, text = ta.value.trim();
  if (!sid || !text) return;
  act($('#btn-send'), async () => {
    await api('/api/send', { session: sid, prompt: text });
    drafts[sid] = '';
    if (active === sid) { ta.value = ''; autosize(); }
    loadHistory(sid);
  });
});

// ---------- cabeçalho e gaveta ----------

$('#btn-menu').addEventListener('click', () => document.body.classList.add('drawer'));
$('#scrim').addEventListener('click', () => document.body.classList.remove('drawer'));
$('#btn-follow').addEventListener('click', (e) => {
  const followed = !!(state.follow || {})[active];
  act(e.currentTarget, () => api('/api/follow', { session: active, on: !followed }));
});
$('#btn-close').addEventListener('click', (e) => {
  const s = sessionById(active);
  if (!s || !confirm('Fechar ' + s.label + '? O worktree é mantido.')) return;
  act(e.currentTarget, async () => { await api('/api/close', { session: s.id, removeWorktree: false }); setActive(null); render(true); });
});
$('#remote').addEventListener('change', (e) => act(e.target, () => api('/api/remote', { on: e.target.checked })));

$('#btn-new').addEventListener('click', () => {
  const sel = $('#form-new [name=project]');
  sel.replaceChildren(...(state?.projects || []).map((p) => h('option', { value: p.name }, p.name)));
  $('#dlg-new').showModal();
});
$('#form-new').addEventListener('submit', (e) => {
  if (e.submitter?.value !== 'ok') return;
  const body = Object.fromEntries(new FormData(e.target).entries());
  api('/api/open', body).then((o) => { e.target.reset(); select(o.id); }).catch((err) => toast(err.message));
});

function b64ToU8(s) {
  const b = atob((s + '='.repeat((4 - (s.length % 4)) % 4)).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(b, (c) => c.charCodeAt(0));
}
$('#btn-push').addEventListener('click', (e) => act(e.target, async () => {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) throw new Error('Este navegador não tem push. No iPhone, instale o app na tela inicial primeiro.');
  if ((await Notification.requestPermission()) !== 'granted') throw new Error('Permissão de notificação negada.');
  const reg = await navigator.serviceWorker.ready;
  const { key } = await api('/api/vapid');
  const sub = (await reg.pushManager.getSubscription()) || (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToU8(key) }));
  await api('/api/subscribe', sub.toJSON());
  await api('/api/test-push', {});
  e.target.textContent = 'Notificações ativas';
}));

// ---------- conexão ----------

function onState(next) {
  const first = !state;
  state = next;
  const now = {};
  for (const s of state.sessions || []) {
    now[s.id] = [s.state, s.tool, s.detail, s.reason].join('|');
    // Sessão que mudou (ou toda sessão, na primeira carga) recarrega o histórico: alimenta a prévia e as não lidas.
    if (s.id !== active && (first || prevSessions[s.id] !== now[s.id])) loadHistory(s.id);
  }
  prevSessions = now;
  if (first && !sessionById(active)) {
    setActive(null);
    if (matchMedia('(max-width: 759px)').matches && (state.sessions || []).length) document.body.classList.add('drawer');
  }
  if (first && active) { $('#input').value = drafts[active] || ''; loadHistory(active); }
  render();
}

function connect() {
  const es = new EventSource('/api/events');
  es.onopen = () => $('#conn').classList.add('on');
  es.onmessage = (m) => onState(JSON.parse(m.data));
  es.onerror = () => { $('#conn').classList.remove('on'); api('/api/state').catch(() => {}); };
}

if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js');
connect();
