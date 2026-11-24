// remoto: manda as permissões do opencode para o celular (extensão "remoto" do lich).
// Usa o mesmo canal do hook do Claude Code: POST em 127.0.0.1/hook com o segredo de hook.json.
// Fora do lich, sem a extensão ou com o modo remoto desligado, não faz nada e o diálogo segue no PC.
import { readFileSync } from "node:fs"
import { join } from "node:path"
import { homedir } from "node:os"

const waiting = new Map() // requestID -> AbortController

function conf() {
  const base = process.env.APPDATA || join(homedir(), ".config")
  return JSON.parse(readFileSync(join(base, "lich", "extension-data", "remoto", "hook.json"), "utf8"))
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
    if (r.status !== 200) return
    const d = (await r.json())?.hookSpecificOutput?.decision
    if (!d) return
    const reply = d.behavior === "allow" ? "once" : "reject"
    if (client?.permission?.reply) await client.permission.reply({ requestID: p.id, reply, message: d.message })
    else await replyHTTP(serverUrl, p, reply, d.message)
  } catch {} finally {
    waiting.delete(p.id)
  }
}

// Sem o SDK novo no plugin: a rota nova, e a antiga (/session/:id/permissions/:id) se ela não existir.
async function replyHTTP(serverUrl, p, reply, message) {
  const post = (path, body) => fetch(new URL(path, serverUrl), {
    method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body),
  })
  const r = await post(`/permission/${p.id}/reply`, { reply, message })
  if (!r.ok) await post(`/session/${p.sessionID}/permissions/${p.id}`, { response: reply })
}

async function server(input) {
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
