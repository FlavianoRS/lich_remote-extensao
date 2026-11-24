'use strict';
// Tudo que vem do agente é texto: o DOM é montado com textContent, nunca innerHTML.

let state = null;
let dirty = false;
const openSessions = new Set();
const drafts = {};          // sessão -> texto sendo digitado
const picks = {};           // pendência -> { pergunta -> Set(rótulos) }
const others = {};          // pendência -> { pergunta -> texto livre }
const $ = (s) => document.querySelector(s);

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
const stateName = { waiting: 'aguardando', busy: 'trabalhando', done: 'pronto', '': '—' };

// ---------- pendências (perguntas e permissões) ----------

function pendingCard(p) {
  const head = h('div', { class: 'row' },
    h('div', { class: 'grow' }, h('div', { class: 'title' }, p.kind === 'question' ? '❓ Pergunta' : '🔐 Permissão'),
      h('div', { class: 'mute' }, p.label + ' · há ' + ago(p.at))));
  const toPC = h('button', { class: 'ghost', onclick: (e) => act(e.target, () => api('/api/reply', { id: p.id, action: 'pc' })) }, 'Responder no PC');
  if (p.kind === 'permission') return h('div', { class: 'card pending' }, head, permissionBody(p), permissionActions(p, toPC));
  return h('div', { class: 'card pending' }, head, ...questionBody(p, toPC));
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
      q.header ? h('span', { class: 'badge none' }, q.header) : null,
      h('div', { class: 'title', style: 'margin-top:6px' }, q.question + (q.multiSelect ? ' (várias)' : '')),
      h('div', {}, ...opts), h('div', { style: 'margin-top:6px' }, other));
  });
  refresh();
  return [...blocks, h('div', { class: 'actions' }, send, toPC)];
}

// ---------- sessões ----------

function sessionCard(s) {
  const isOpen = openSessions.has(s.id);
  const st = s.state || '';
  const line = s.reason || s.detail || s.tool || '';
  const head = h('div', { class: 'row', style: 'cursor:pointer', onclick: () => { isOpen ? openSessions.delete(s.id) : openSessions.add(s.id); render(true); } },
    h('div', { class: 'grow' },
      h('div', { class: 'title ellipsis' }, s.label + (s.name ? ' · ' + s.name : '')),
      h('div', { class: 'mute ellipsis' }, s.kind + (line ? ' · ' + line : ''))),
    h('span', { class: 'badge ' + (st || 'none') }, stateName[st] ?? st));
  if (!isOpen) return h('div', { class: 'card' }, head);

  const ta = h('textarea', { rows: 3, placeholder: st === 'waiting' ? 'A sessão está aguardando uma resposta no PC' : 'Tarefa para ' + s.label });
  ta.value = drafts[s.id] || '';
  ta.addEventListener('input', () => (drafts[s.id] = ta.value));
  const sendBtn = h('button', {
    onclick: (e) => act(e.target, async () => {
      await api('/api/send', { session: s.id, prompt: ta.value });
      drafts[s.id] = ''; ta.value = '';
    }),
  }, 'Enviar');
  const closeBtn = h('button', {
    class: 'ghost',
    onclick: (e) => {
      if (!confirm('Fechar ' + s.label + '? O worktree é mantido.')) return;
      act(e.target, async () => { await api('/api/close', { session: s.id, removeWorktree: false }); openSessions.delete(s.id); });
    },
  }, 'Fechar sessão');
  const hist = ((state.history || {})[s.id] || []).slice().reverse().map((e) =>
    h('div', { class: 'entry' },
      h('div', { class: 'mute' }, 'você · há ' + ago(e.at) + ' · ' + (e.status === 'pending' ? 'aguardando resposta…' : e.status)),
      h('div', {}, e.prompt), e.answer ? h('pre', {}, e.answer) : null));
  return h('div', { class: 'card' }, head, h('div', { style: 'margin-top:10px' }, ta), h('div', { class: 'actions' }, sendBtn, closeBtn), ...hist);
}

// ---------- tela ----------

function render(force) {
  if (!state) return;
  const a = document.activeElement;
  if (!force && a && $('#app').contains(a) && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT')) { dirty = true; return; }
  dirty = false;
  $('#remote').checked = !!state.remote;
  const ts = state.tailscale || {};
  const app = $('#app');
  const kids = [];
  if (ts.warn) kids.push(h('div', { class: 'card mute' }, '⚠ ' + ts.warn));
  if (!state.remote) kids.push(h('div', { class: 'card mute' }, 'Modo remoto desligado: perguntas e permissões aparecem só no PC. Ligue no topo para recebê-las aqui.'));
  if (state.pending && state.pending.length) {
    kids.push(h('h2', {}, 'Aguardando você'), ...state.pending.map(pendingCard));
  }
  const byProject = {};
  for (const s of state.sessions || []) (byProject[s.project] ||= []).push(s);
  const order = { waiting: 0, busy: 1, done: 2 };
  for (const [proj, list] of Object.entries(byProject).sort()) {
    list.sort((x, y) => (order[x.state] ?? 3) - (order[y.state] ?? 3) || x.label.localeCompare(y.label));
    kids.push(h('h2', {}, proj), ...list.map(sessionCard));
  }
  if (!(state.sessions || []).length) kids.push(h('div', { class: 'empty' }, 'Nenhuma sessão aberta no lich.'));
  app.replaceChildren(...kids);
}

document.addEventListener('focusout', () => setTimeout(() => dirty && render(), 0));

$('#remote').addEventListener('change', (e) => act(e.target, () => api('/api/remote', { on: e.target.checked })));

$('#btn-new').addEventListener('click', () => {
  const sel = $('#form-new [name=project]');
  sel.replaceChildren(...(state?.projects || []).map((p) => h('option', { value: p.name }, p.name)));
  $('#dlg-new').showModal();
});
$('#form-new').addEventListener('submit', (e) => {
  if (e.submitter?.value !== 'ok') return;
  const f = new FormData(e.target);
  const body = Object.fromEntries(f.entries());
  api('/api/open', body).then((o) => { openSessions.add(o.id); toast('Sessão ' + o.label + ' aberta'); e.target.reset(); }).catch((err) => toast(err.message));
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

function connect() {
  const es = new EventSource('/api/events');
  es.onopen = () => $('#conn').classList.add('on');
  es.onmessage = (m) => { state = JSON.parse(m.data); render(); };
  es.onerror = () => $('#conn').classList.remove('on');
}

if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js');
connect();
