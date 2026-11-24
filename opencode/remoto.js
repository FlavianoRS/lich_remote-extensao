// remoto: manda as permissões do opencode para o celular (extensão "remoto" do lich).
// Usa o mesmo canal do hook do Claude Code: POST em 127.0.0.1/hook com o segredo de hook.json.
// Fora do lich, sem a extensão ou com o modo remoto desligado, não faz nada e o diálogo segue no PC.
import { appendFileSync, readFileSync } from "node:fs"
import { join } from "node:path"
import { homedir } from "node:os"

const waiting = new Map() // requestID -> AbortController

const dataDir = () => join(process.env.APPDATA || join(homedir(), ".config"), "lich", "extension-data", "remoto")

function conf() {
  return JSON.parse(readFileSync(join(dataDir(), "hook.json"), "utf8"))
}

// Mesma linha do hook.log que o remoto.exe escreve, para saber se o plugin carregou e viu o pedido.
function logLine(ev, tool, decided) {
  try {
    const t = new Date().toLocaleString("sv-SE").slice(0, 19)
    appendFileSync(join(dataDir(), "hook.log"),
      `${t} ev=opencode:${ev} tool=${tool || ""} session=${process.env.LICH_SESSION_ID || ""} decided=${decided}
`)
  } catch {}
}

async function ask(client, serverUrl, p) {
  const session = process.env.LICH_SESSION_ID
  if (!session) return
  let c
  try { c = conf() } catch { return }
  const ctl = new AbortController()
  waiting.set(p.id, ctl)
  try {
    const payload = {
      hook_event_name: "PermissionRequest",
      tool_name: p.permission,
      tool_input: { ...(p.metadata || {}), command: (p.patterns || []).join("\n") },
    }
    const r = await fetch(`http://127.0.0.1:${c.port}/hook`, {
      method: "POST",
      headers: { "content-type": "application/json", "X-Remoto-Secret": c.secret },
      body: JSON.stringify({ session, payload }),
      signal: ctl.signal,
    })
    if (r.status !== 200) return logLine("permission", p.permission, false)
    const d = (await r.json())?.hookSpecificOutput?.decision
    logLine("permission", p.permission, !!d)
    if (!d) return
    const reply = d.behavior === "allow" ? "once" : "reject"
    const err = await replyTo(client, serverUrl, p, reply, d.message)
    if (err) logLine("reply-falhou " + String(err).slice(0, 200).replace(/\s+/g, " "), p.permission, false)
  } catch (e) {
    if (e?.name !== "AbortError") logLine("erro " + String(e).slice(0, 200).replace(/\s+/g, " "), p.permission, false)
  } finally {
    waiting.delete(p.id)
  }
}

// Devolve a decisão ao opencode; "" = deu certo, senão o motivo da última tentativa.
// O plugin do 1.x recebe o SDK v1 (postSessionIdPermissionsPermissionId, que devolve {error} em vez de lançar);
// o 2.x tem client.permission.reply. O HTTP direto é o último recurso: no TUI o servidor costuma ser interno.
async function replyTo(client, serverUrl, p, reply, message) {
  const tries = [
    client?.postSessionIdPermissionsPermissionId &&
      (() => client.postSessionIdPermissionsPermissionId({ path: { id: p.sessionID, permissionID: p.id }, body: { response: reply } })),
    client?.permission?.reply && (() => client.permission.reply({ requestID: p.id, reply, message })),
    serverUrl && (() => fetch(new URL(`/session/${p.sessionID}/permissions/${p.id}`, serverUrl), {
      method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ response: reply }),
    })),
  ].filter(Boolean)
  let last = "nenhuma API de resposta no plugin"
  for (const t of tries) {
    try {
      const r = await t()
      if (r?.error) last = JSON.stringify(r.error)
      else if (r instanceof Response && !r.ok) last = "HTTP " + r.status
      else return ""
    } catch (e) {
      last = String(e)
    }
  }
  return last
}

async function server(input) {
  logLine("plugin-loaded", "", false)
  return {
    // Não aguarda: o opencode espera os hooks, e a pergunta pode levar minutos.
    event: async ({ event }) => {
      const p = event?.properties
      if (event?.type === "permission.asked" && p?.id) ask(input?.client, input?.serverUrl, p)
      if (event?.type === "permission.replied") waiting.get(p?.requestID)?.abort() // respondida no PC
    },
  }
}

export default { id: "remoto", server }
