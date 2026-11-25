'use strict';
// Tudo que vem do agente é texto: o DOM é montado com textContent, nunca innerHTML.
// Visual: proposta da sessão Antigravity 62 (mockup v0.5.2), ligada aos dados reais da extensão.

let state = null;
let prevSessions = {};       // sessão -> assinatura do último snapshot (para saber quem mudou)
let active = null;           // sessão aberta no chat
let dirty = false;
let online = false;
const drafts = {};           // sessão -> texto sendo digitado
const picks = {};            // pendência -> { pergunta -> Set(rótulos) }
const others = {};           // pendência -> { pergunta -> texto livre }
const resolved = {};         // pendência -> mensagem mostrada até o snapshot tirar o cartão
const chats = {};            // sessão -> { total, from, msgs, error }
const openTools = new Set(); // linhas de ferramenta abertas (pelo texto), sobrevivem ao redesenho
const $ = (s) => document.querySelector(s);

// Quantas mensagens de cada sessão já foram vistas (contador de não lidas). Só conveniência local.
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

// Ícones: só caminhos SVG fixos deste arquivo (nada vindo do agente).
const SVG_NS = 'http://www.w3.org/2000/svg';
function svg(shapes, strokeWidth = '2') {
  const el = document.createElementNS(SVG_NS, 'svg');
  for (const [k, v] of Object.entries({ viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': strokeWidth,
    'stroke-linecap': 'round', 'stroke-linejoin': 'round', class: 'icon-svg', 'aria-hidden': 'true' })) el.setAttribute(k, v);
  for (const [tag, attrs] of shapes) {
    const s = document.createElementNS(SVG_NS, tag);
    for (const [k, v] of Object.entries(attrs)) s.setAttribute(k, v);
    el.append(s);
  }
  return el;
}
const ICON = {
  claude: [['path', { d: 'M12 2l2.4 6.9L21.3 12l-6.9 3.1L12 22l-2.4-6.9L2.7 12l6.9-3.1L12 2z' }]],
  codex: [['polyline', { points: '4 17 10 11 4 5' }], ['line', { x1: 12, y1: 19, x2: 20, y2: 19 }]],
  opencode: [['polyline', { points: '16 18 22 12 16 6' }], ['polyline', { points: '8 6 2 12 8 18' }]],
  antigravity: [['polygon', { points: '12 2 22 8.5 22 15.5 12 22 2 15.5 2 8.5 12 2' }], ['line', { x1: 12, y1: 22, x2: 12, y2: 15.5 }], ['polyline', { points: '22 8.5 12 15.5 2 8.5' }]],
  warn: [['path', { d: 'M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01' }]],
  off: [['circle', { cx: 12, cy: 12, r: 10 }], ['line', { x1: 4.93, y1: 4.93, x2: 19.07, y2: 19.07 }]],
  lock: [['rect', { x: 3, y: 11, width: 18, height: 11, rx: 2, ry: 2 }], ['path', { d: 'M7 11V7a5 5 0 0 1 10 0v4' }]],
  ask: [['circle', { cx: 12, cy: 12, r: 10 }], ['path', { d: 'M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3M12 17h.01' }]],
  chat: [['path', { d: 'M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z' }]],
  info: [['circle', { cx: 12, cy: 12, r: 10 }], ['line', { x1: 12, y1: 8, x2: 12, y2: 12 }], ['line', { x1: 12, y1: 16, x2: 12.01, y2: 16 }]],
  up: [['polyline', { points: '7 11 12 6 17 11' }], ['polyline', { points: '7 18 12 13 17 18' }]],
  down: [['polyline', { points: '6 9 12 15 18 9' }]],
};
const agentIcon = (kind) => svg(ICON[kind] || ICON.antigravity);

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
  const b = $('#system-banner');
  b.textContent = msg; b.hidden = false;
  clearTimeout(toast.t); toast.t = setTimeout(() => (b.hidden = true), 6000);
}

async function act(btn, fn) {
  btn.disabled = true;
  try { await fn(); } catch (e) { toast(e.message); } finally { btn.disabled = false; }
}

const ago = (t) => {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - t));
  return s < 60 ? s + 's atrás' : s < 3600 ? Math.floor(s / 60) + ' min atrás' : Math.floor(s / 3600) + 'h atrás';
};
const hhmm = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const dayOf = (ms) => {
  const d = new Date(ms), t = new Date();
  const y = new Date(t); y.setDate(t.getDate() - 1);
  if (d.toDateString() === t.toDateString()) return 'Hoje';
  if (d.toDateString() === y.toDateString()) return 'Ontem';
  return d.toLocaleDateString([], { day: '2-digit', month: 'short' });
};
const stateName = { waiting: 'Aguardando você', busy: 'Trabalhando', done: 'Pronto' };
const sessionById = (id) => (state?.sessions || []).find((s) => s.id === id);
const pendingOf = (id) => (state?.pending || []).filter((p) => p.session === id);
const avatar = (s, cls) => h('div', { class: cls + ' ' + (s.kind || ''), 'aria-hidden': 'true' }, agentIcon(s.kind));

// ---------- markdown mínimo (blocos ```, `código`, **negrito**, títulos), sempre como DOM ----------

function md(text) {
  return text.split('```').flatMap((part, i) => {
    if (i % 2 === 0) return inline(part);
    const nl = part.indexOf('\n');
    const lang = nl > -1 ? part.slice(0, nl).trim() : '';
    const code = (nl > -1 ? part.slice(nl + 1) : part).replace(/\n$/, '');
    const copy = h('button', { class: 'btn-copy-code', type: 'button' }, 'Copiar');
    copy.addEventListener('click', () => {
      navigator.clipboard?.writeText(code).then(() => { copy.textContent = '✓ Copiado'; setTimeout(() => (copy.textContent = 'Copiar'), 2000); });
    });
    return [h('div', { class: 'code-block' }, h('div', { class: 'code-block-header' }, h('span', {}, lang || 'código'), copy), h('pre', {}, code))];
  });
}
function inline(t) {
  t = t.replace(/^#{1,6}\s+(.+)$/gm, '**$1**');
  const out = [], re = /\*\*([^*\n]+)\*\*|`([^`\n]+)`/g;
  let last = 0, m;
  while ((m = re.exec(t))) {
    if (m.index > last) out.push(t.slice(last, m.index));
    out.push(m[1] !== undefined ? h('strong', {}, m[1]) : h('code', {}, m[2]));
    last = re.lastIndex;
  }
  out.push(t.slice(last));
  return out;
}

// ---------- cartões de pergunta e permissão, dentro do chat da sessão ----------

function reply(card, body, done) {
  return (e) => act(e.currentTarget, async () => {
    await api('/api/reply', body);
    resolved[body.id] = done;
    card.replaceChildren(resolvedNode(done));
  });
}
const resolvedNode = (r) => h('div', { class: 'card-resolved ' + r.cls }, r.text);

function cardHead(icon, title, p) {
  return h('div', { class: 'card-head' },
    h('div', { class: 'card-title-group' }, h('div', { class: 'card-icon', 'aria-hidden': 'true' }, icon), h('div', { class: 'card-title' }, title)),
    h('div', { class: 'card-time' }, ago(p.at)));
}

function permissionCard(p) {
  const card = h('div', { class: 'action-card permission-card' });
  if (resolved[p.id]) { card.append(resolvedNode(resolved[p.id])); return card; }
  const i = p.input || {};
  const detail = i.command || i.CommandLine || i.file_path || i.filePath || i.TargetFile || i.url || i.pattern || JSON.stringify(i, null, 2);
  const reason = h('input', { class: 'card-input-reason', placeholder: 'Motivo ao negar (opcional)' });
  card.append(
    cardHead('🔐', 'Permissão: ' + p.tool, p),
    i.description ? h('div', { class: 'card-desc' }, i.description) : null,
    h('div', { class: 'card-code-preview' }, String(detail).slice(0, 4000)),
    reason,
    h('div', { class: 'card-actions' },
      h('button', { class: 'btn-action-primary', onclick: reply(card, { id: p.id, action: 'allow' }, { cls: 'success', text: '✓ Permitido pelo celular.' }) }, '✓ Permitir'),
      h('button', { class: 'btn-action-danger', onclick: (e) => reply(card, { id: p.id, action: 'deny', message: reason.value }, { cls: 'rejected', text: '✕ Negado' + (reason.value ? ': ' + reason.value : '.') })(e) }, '✕ Negar'),
      h('button', { class: 'btn-action-pc', onclick: reply(card, { id: p.id, action: 'pc' }, { cls: 'success', text: '🖥️ Fica para responder no PC.' }) }, '🖥️ Responder no PC')));
  return card;
}

function questionCard(p) {
  const card = h('div', { class: 'action-card question-card' });
  if (resolved[p.id]) { card.append(resolvedNode(resolved[p.id])); return card; }
  const qs = (p.input && p.input.questions) || [];
  const sel = (picks[p.id] ||= {});
  const txt = (others[p.id] ||= {});
  const answerOf = (q) => (txt[q.question] || '').trim() || [...(sel[q.question] || [])].join(', ');
  const send = h('button', { class: 'btn-action-primary' }, 'Enviar resposta');
  send.addEventListener('click', (e) => {
    const answers = {};
    for (const q of qs) answers[q.question] = answerOf(q);
    reply(card, { id: p.id, action: 'answer', answers }, { cls: 'success', text: '✓ Resposta enviada: ' + Object.values(answers).join(' · ') })(e);
  });
  const refresh = () => { send.disabled = !qs.every((q) => answerOf(q)); };
  const blocks = qs.map((q) => {
    const set = (sel[q.question] ||= new Set());
    const list = h('div', { class: 'options-list', role: q.multiSelect ? 'group' : 'radiogroup' });
    const sync = () => list.querySelectorAll('.option-btn').forEach((b, i) => {
      const on = set.has(q.options[i].label);
      b.classList.toggle('selected', on);
      b.setAttribute('aria-checked', on);
    });
    for (const o of q.options || []) {
      list.append(h('button', {
        type: 'button', class: 'option-btn' + (q.multiSelect ? ' multi' : ''), role: q.multiSelect ? 'checkbox' : 'radio',
        onclick: () => {
          if (q.multiSelect) set.has(o.label) ? set.delete(o.label) : set.add(o.label);
          else { set.clear(); set.add(o.label); txt[q.question] = ''; other.value = ''; }
          sync(); refresh();
        },
      }, h('div', { class: 'option-indicator', 'aria-hidden': 'true' }, '✓'),
      h('div', { class: 'option-text-group' }, h('span', { class: 'option-label' }, o.label), o.description ? h('span', { class: 'option-sub' }, o.description) : null)));
    }
    const other = h('input', { class: 'card-input-reason', placeholder: 'Outra resposta…', value: txt[q.question] || '' });
    other.addEventListener('input', () => { txt[q.question] = other.value; if (other.value.trim() && !q.multiSelect) { set.clear(); sync(); } refresh(); });
    sync();
    return [h('div', { class: 'card-desc card-question' }, q.question + (q.multiSelect ? ' (pode marcar várias)' : '')), list, other];
  });
  refresh();
  card.append(cardHead('❓', qs[0]?.header || 'Pergunta', p), ...blocks.flat(),
    h('div', { class: 'card-actions' }, send,
      h('button', { class: 'btn-action-pc', onclick: reply(card, { id: p.id, action: 'pc' }, { cls: 'success', text: '🖥️ Fica para responder no PC.' }) }, '🖥️ Responder no PC')));
  return card;
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

// ---------- gaveta: sessões ----------

function preview(s) {
  const p = pendingOf(s.id);
  if (p.length) return { text: p[0].kind === 'question' ? '❓ ' + (p[0].input?.questions?.[0]?.question || 'Pergunta') : '🔐 Permissão: ' + p[0].tool, cls: 'alert-preview' };
  if (s.state === 'busy') return { text: s.tool ? '⚡ ' + s.tool + '…' : 'Trabalhando…', cls: 'busy-preview' };
  const c = chats[s.id];
  const last = c && [...c.msgs].reverse().find((m) => m.role !== 'tool');
  if (last) return { text: (last.role === 'user' ? 'Você: ' : '') + last.text.replace(/\s+/g, ' ').slice(0, 140), at: last.at };
  return { text: s.reason || s.detail || stateName[s.state] || s.kind };
}

// Sessão num worktree: mostra o nome da pasta (a branch, no lich) num chip.
const worktree = (s) => (s.path && s.projectPath && s.path !== s.projectPath ? s.path.split(/[\\/]/).pop() : '');

function sessionItem(s) {
  const pv = preview(s);
  const n = pendingOf(s.id).length;
  const u = s.id === active ? 0 : unread(s.id);
  return h('button', { class: 'session-item' + (s.id === active ? ' active' : '') + (n ? ' has-pending' : ''), onclick: () => select(s.id) },
    h('div', { class: 'avatar-wrap' }, avatar(s, 'avatar'), h('div', { class: 'avatar-badge ' + (s.state || '') })),
    h('div', { class: 'session-info' },
      h('div', { class: 'session-row-top' },
        h('span', { class: 'session-name' }, s.label),
        worktree(s) ? h('span', { class: 'session-worktree', title: s.path }, worktree(s)) : null,
        pv.at ? h('span', { class: 'session-time' }, hhmm(pv.at)) : null),
      h('div', { class: 'session-row-bot' },
        h('span', { class: 'session-preview ' + (pv.cls || '') }, pv.text),
        n ? h('span', { class: 'badge-count pending', title: 'Aguardando você' }, n) : u ? h('span', { class: 'badge-count', title: 'Não lidas' }, u) : null)));
}

function renderList() {
  const list = $('#session-list');
  const sessions = state.sessions || [];
  if (!sessions.length) {
    list.replaceChildren(h('div', { class: 'empty-state' }, h('div', { class: 'empty-icon-wrap' }, svg(ICON.info)),
      h('div', { class: 'empty-title' }, 'Nenhuma sessão aberta'), h('div', { class: 'empty-text' }, 'Abra uma sessão com qualquer agente pelo botão "Nova".')));
  } else {
    const byProject = {};
    for (const s of sessions) (byProject[s.project] ||= []).push(s);
    const order = { waiting: 1, busy: 2, done: 3 };
    const rank = (s) => (pendingOf(s.id).length ? 0 : order[s.state] ?? 4);
    list.replaceChildren(...Object.keys(byProject).sort().map((proj) => {
      const items = byProject[proj].sort((x, y) => rank(x) - rank(y) || x.label.localeCompare(y.label, undefined, { numeric: true }));
      return h('div', { class: 'project-group' },
        h('div', { class: 'project-header' }, h('span', {}, proj), h('span', { class: 'project-count' }, items.length)),
        ...items.map(sessionItem));
    }));
  }
  const ts = state.tailscale || {};
  $('#notes').replaceChildren(
    ts.warn ? h('div', { class: 'drawer-banner warn' }, svg(ICON.warn), h('span', {}, ts.warn)) : '',
    state.remote ? '' : h('div', { class: 'drawer-banner danger' }, svg(ICON.off), h('span', {}, 'Modo remoto desligado: perguntas e permissões aparecem só no PC.')));
}

// ---------- chat ----------

function renderHead(s) {
  const followBtn = $('#btn-follow'), closeBtn = $('#btn-close');
  $('#menu-badge').hidden = !(state.pending || []).some((p) => p.session !== active);
  followBtn.hidden = closeBtn.hidden = !s;
  if (!s) { $('#chat-head-info').replaceChildren(h('div', { class: 'chat-head-title' }, 'lich remoto')); return; }
  const followed = !!(state.follow || {})[s.id];
  followBtn.classList.toggle('active', followed);
  followBtn.title = followed ? 'Acompanhando: avisa no celular ao terminar. Toque para parar.' : 'Acompanhar: avisar no celular quando terminar';
  followBtn.setAttribute('aria-label', followBtn.title);
  followBtn.setAttribute('aria-pressed', followed);
  const st = s.state === 'busy' && s.tool ? 'Executando: ' + s.tool : stateName[s.state] || '—';
  $('#chat-head-info').replaceChildren(avatar(s, 'chat-head-avatar'),
    h('div', { class: 'chat-head-info' },
      h('div', { class: 'chat-head-title-row' }, h('span', { class: 'chat-head-title' }, s.label)),
      h('div', { class: 'chat-head-status-row' }, h('span', { class: 'chat-status-pill ' + (s.state || '') }, st), h('span', {}, '·'), h('span', {}, s.project))));
}

function toolCard(m) {
  const i = m.text.indexOf(': ');
  const split = i > 0 && i < 60;
  const name = split ? m.text.slice(0, i) : m.text;
  const rest = split ? m.text.slice(i + 2) : '';
  const card = h('div', { class: 'tool-card' + (openTools.has(m.text) ? ' open' : '') });
  card.append(
    h('button', { type: 'button', class: 'tool-header', disabled: !rest, 'aria-expanded': openTools.has(m.text), onclick: (e) => {
      const on = card.classList.toggle('open');
      on ? openTools.add(m.text) : openTools.delete(m.text);
      e.currentTarget.setAttribute('aria-expanded', on);
    } },
      h('div', { class: 'tool-icon-label' }, h('span', { class: 'tool-badge' }, name), h('span', { class: 'tool-command' }, rest)),
      rest ? h('div', { class: 'tool-meta' }, h('span', { class: 'tool-chevron' }, svg(ICON.down, '2.5'))) : null),
    rest ? h('div', { class: 'tool-content' }, rest) : '');
  return card;
}

function renderMsgs(s) {
  const box = $('#messages-container');
  if (!s) {
    box.dataset.sid = '';
    box.replaceChildren(h('div', { class: 'empty-state' }, h('div', { class: 'empty-icon-wrap' }, svg(ICON.chat)),
      h('div', { class: 'empty-title' }, (state.sessions || []).length ? 'Escolha uma sessão' : 'Nenhuma sessão aberta'),
      h('div', { class: 'empty-text' }, (state.sessions || []).length ? 'Abra uma conversa na lista de sessões para acompanhar ou mandar tarefas.' : 'Use "Nova" na lista de sessões para abrir uma.')));
    return;
  }
  const c = chats[s.id];
  const kids = [];
  if (!c) { kids.push(h('div', { class: 'date-divider' }, 'carregando o histórico…')); loadHistory(s.id); }
  else {
    if (c.from > 0) kids.push(h('div', { class: 'load-more-wrap' }, h('button', { class: 'btn-load-more', onclick: (e) => act(e.currentTarget, () => loadHistory(s.id, true)) }, svg(ICON.up), 'Carregar anteriores (' + c.from + ')')));
    if (c.error && !c.msgs.length) kids.push(h('div', { class: 'date-divider' }, c.error));
    else if (!c.msgs.length) kids.push(h('div', { class: 'date-divider' }, 'Sem mensagens ainda'));
    let day = '';
    for (const m of c.msgs) {
      if (m.at && dayOf(m.at) !== day) { day = dayOf(m.at); kids.push(h('div', { class: 'date-divider' }, day)); }
      if (m.role === 'tool') kids.push(toolCard(m));
      else if (m.role === 'error') kids.push(h('div', { class: 'msg-bubble error' }, m.text));
      else kids.push(h('div', { class: 'msg-bubble ' + m.role }, m.role === 'assistant' ? md(m.text) : m.text, m.at ? h('div', { class: 'msg-meta' }, hhmm(m.at)) : null));
    }
  }
  const waitingPhone = ((state.history || {})[s.id] || []).filter((e) => e.status === 'pending').length;
  if (waitingPhone) kids.push(h('div', { class: 'date-divider' }, '⏳ aguardando a resposta de ' + waitingPhone + ' tarefa(s) enviada(s) daqui'));
  if (s.state === 'busy') kids.push(h('div', { class: 'typing-indicator-card' },
    h('div', { class: 'wave-dots', 'aria-hidden': 'true' }, h('span', { class: 'wave-dot' }), h('span', { class: 'wave-dot' }), h('span', { class: 'wave-dot' })),
    h('span', {}, s.label + (s.tool ? ' está usando ' + s.tool : ' está trabalhando') + '…')));
  for (const p of pendingOf(s.id)) kids.push(p.kind === 'permission' ? permissionCard(p) : questionCard(p));

  // Preserva a posição: rola para o fim só se já estava no fim (ou trocou de sessão); "Carregar anteriores"
  // mantém a mesma mensagem na tela.
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  const switched = box.dataset.sid !== s.id;
  const top = box.scrollTop, oldH = box.scrollHeight;
  const grewUp = !switched && c && Number(box.dataset.from) > c.from;
  box.replaceChildren(...kids);
  box.dataset.sid = s.id;
  box.dataset.from = c ? c.from : '';
  box.scrollTop = grewUp ? top + box.scrollHeight - oldH : atBottom || switched ? box.scrollHeight : top;
}

function renderComposer(s) {
  const f = $('#composer-form'), ta = $('#chat-input');
  f.hidden = !s;
  if (!s) return;
  ta.placeholder = s.state === 'waiting' && !pendingOf(s.id).length ? 'A sessão aguarda uma resposta no PC' : 'Mensagem para ' + s.label;
  $('#btn-send').disabled = !ta.value.trim();
}

function renderElsewhere() {
  const el = $('#banner-elsewhere');
  const p = (state.pending || []).find((x) => x.session !== active);
  el.hidden = !p;
  if (!p) return;
  el.onclick = () => select(p.session);
  el.onkeydown = (e) => { if (e.key === 'Enter' || e.key === ' ') select(p.session); };
  el.replaceChildren(
    h('div', { class: 'banner-text-wrap' }, svg(p.kind === 'permission' ? ICON.lock : ICON.ask),
      h('span', {}, p.label + (p.kind === 'permission' ? ' pede permissão' : ' pergunta'))),
    h('span', { class: 'banner-action-text' }, 'Abrir →'));
}

function renderConn() {
  $('#conn-dot').className = 'conn-dot ' + (online ? 'online' : 'offline');
  $('#conn-text').textContent = online ? 'Conectado ao PC' : 'Sem conexão com o PC';
}

function render(force) {
  renderConn();
  if (!state) return;
  $('#switch-remote').checked = !!state.remote;
  renderList();
  if (active && !sessionById(active)) setActive(null);
  const s = sessionById(active);
  renderHead(s);
  renderComposer(s);
  renderElsewhere();
  // Não redesenha o chat enquanto um campo de um cartão está em edição (perderia o foco).
  const a = document.activeElement;
  if (!force && a && $('#messages-container').contains(a) && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT')) { dirty = true; return; }
  dirty = false;
  renderMsgs(s);
}

function setActive(sid) {
  active = sid;
  try { sid ? localStorage.setItem('active', sid) : localStorage.removeItem('active'); } catch {}
}

function select(sid) {
  if (active) drafts[active] = $('#chat-input').value;
  setActive(sid);
  $('#chat-input').value = drafts[sid] || '';
  autosize();
  document.body.classList.remove('drawer-open');
  markSeen(sid);
  loadHistory(sid);
  render(true);
}

document.addEventListener('focusout', () => setTimeout(() => dirty && render(), 0));
document.addEventListener('visibilitychange', () => { if (!document.hidden && active) { markSeen(active); loadHistory(active); } });

// ---------- composer ----------

function autosize() {
  const ta = $('#chat-input');
  ta.style.height = '';
  if (ta.value && ta.offsetParent) ta.style.height = Math.min(ta.scrollHeight + 2, 140) + 'px';
}
$('#chat-input').addEventListener('input', () => {
  autosize();
  $('#btn-send').disabled = !$('#chat-input').value.trim();
  if (active) drafts[active] = $('#chat-input').value;
});
// Enter envia no teclado físico; no celular o Enter quebra linha e o envio é pelo botão.
$('#chat-input').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey && matchMedia('(pointer: fine)').matches) { e.preventDefault(); $('#composer-form').requestSubmit(); }
});
$('#composer-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const ta = $('#chat-input'), sid = active, text = ta.value.trim();
  if (!sid || !text) return;
  act($('#btn-send'), async () => {
    await api('/api/send', { session: sid, prompt: text });
    drafts[sid] = '';
    if (active === sid) { ta.value = ''; autosize(); }
    loadHistory(sid);
  });
});

// ---------- cabeçalho, gaveta e diálogos ----------

$('#btn-menu').addEventListener('click', () => document.body.classList.add('drawer-open'));
$('#scrim').addEventListener('click', () => document.body.classList.remove('drawer-open'));
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') document.body.classList.remove('drawer-open'); });
$('#btn-follow').addEventListener('click', (e) => {
  const followed = !!(state.follow || {})[active];
  act(e.currentTarget, () => api('/api/follow', { session: active, on: !followed }));
});
$('#btn-close').addEventListener('click', (e) => {
  const s = sessionById(active);
  if (!s || !confirm('Fechar ' + s.label + '? O worktree é mantido.')) return;
  act(e.currentTarget, async () => { await api('/api/close', { session: s.id, removeWorktree: false }); setActive(null); render(true); });
});
$('#switch-remote').addEventListener('change', (e) => act(e.target, () => api('/api/remote', { on: e.target.checked })));

$('#btn-new').addEventListener('click', () => {
  $('#new-project').replaceChildren(...(state?.projects || []).map((p) => h('option', { value: p.name }, p.name)));
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
$('#btn-push').addEventListener('click', (e) => act(e.currentTarget, async () => {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) throw new Error('Este navegador não tem push. No iPhone, instale o app na tela inicial primeiro.');
  if ((await Notification.requestPermission()) !== 'granted') throw new Error('Permissão de notificação negada.');
  const reg = await navigator.serviceWorker.ready;
  const { key } = await api('/api/vapid');
  const sub = (await reg.pushManager.getSubscription()) || (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToU8(key) }));
  await api('/api/subscribe', sub.toJSON());
  await api('/api/test-push', {});
  $('#push-btn-text').textContent = 'Notificações ativas';
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
  for (const id of Object.keys(resolved)) if (!(state.pending || []).some((p) => p.id === id)) delete resolved[id];
  if (first && !sessionById(active)) {
    setActive(null);
    if (matchMedia('(max-width: 759px)').matches && (state.sessions || []).length) document.body.classList.add('drawer-open');
  }
  if (first && active) { $('#chat-input').value = drafts[active] || ''; loadHistory(active); }
  render();
}

function connect() {
  const es = new EventSource('/api/events');
  es.onopen = () => { online = true; renderConn(); };
  es.onmessage = (m) => onState(JSON.parse(m.data));
  es.onerror = () => { online = false; renderConn(); api('/api/state').catch(() => {}); };
}

if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js');
connect();
