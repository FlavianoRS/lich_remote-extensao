# remoto: controle remoto do lich pelo celular

Extensão do lich (Go) que coloca a máquina na sua rede Tailscale **em espaço de usuário** (`tsnet`, sem admin,
sem driver) e serve um PWA em `https://lich-remoto.<sua-rede>.ts.net`. Funciona de qualquer rede: basta o PC
ligado com o lich aberto e o app Tailscale ativo no celular.

O PWA permite:

- ver as sessões por projeto e o estado de cada uma (aguardando, trabalhando, pronto)
- mandar uma tarefa para uma sessão e ler a resposta
- abrir uma sessão nova (Claude, Codex, opencode ou Antigravity, com worktree opcional)
- fechar uma sessão (via `lich close`, sempre mantendo o worktree)
- responder pelo celular as perguntas (`AskUserQuestion`) e as permissões do Claude Code, com notificação push

## Segurança

- Só aparelhos da **sua** conta Tailscale entram: cada requisição passa por `WhoIs` e precisa ser do mesmo usuário
  dono do nó. Nada fica exposto na internet, e o tráfego é WireGuard ponta a ponta.
- O hook fala com a extensão só por `127.0.0.1`, com um segredo aleatório gravado em
  `%AppData%\lich\extension-data\remoto\hook.json` (lido só pelo usuário).
- Requisições POST exigem o header `X-Remoto` (proteção contra CSRF), e a página usa CSP restritiva. Todo texto
  vindo do agente é desenhado com `textContent`.
- O **modo remoto** começa desligado: o hook devolve na hora e o PC segue normal. Ao desligar, o que estava
  esperando o celular volta para o PC.

## Instalar

1. Compilar (ou usar `dist/remoto` já pronto):
   ```
   go build -trimpath -ldflags="-s -w" -o remoto.exe .
   ```
   Copie `manifest.json` e `remoto.exe` para uma pasta, por exemplo `dist/remoto`.
2. No lich: **Settings › Extensions › Install from… › From a folder** e escolha `dist/remoto`.
3. Abra a tela **Controle remoto** na barra do topo. Na primeira vez ela abre o login do Tailscale; entre com a
   mesma conta do celular.
4. No painel do Tailscale (login.tailscale.com/admin/dns), ligue **MagicDNS** e **HTTPS Certificates**. Sem HTTPS
   o PWA não instala e o push não funciona.
5. No celular: app Tailscale ligado, abra o endereço que a tela mostra, depois "Adicionar à tela inicial" e
   **Ativar notificações** (no iPhone, só depois de instalado na tela inicial, iOS 16.4 ou mais novo).

Login sem navegador: salve uma auth key do Tailscale em `%AppData%\lich\extension-data\remoto\authkey`.

## Ligar as perguntas e as permissões (Claude Code)

Adicione ao `~/.claude/settings.json` (troque o caminho se o seu `%AppData%` for outro):

```json
{
  "hooks": {
    "PreToolUse": [
      { "matcher": "AskUserQuestion",
        "hooks": [{ "type": "command", "command": "\"C:/Users/<você>/AppData/Roaming/lich/extensions/remoto/remoto.exe\" hook", "timeout": 3600 }] }
    ],
    "PermissionRequest": [
      { "matcher": "*",
        "hooks": [{ "type": "command", "command": "\"C:/Users/<você>/AppData/Roaming/lich/extensions/remoto/remoto.exe\" hook", "timeout": 3600 }] }
    ]
  }
}
```

O hook só age em sessões do lich (`LICH_SESSION_ID`) e com o modo remoto ligado. Em qualquer outra situação, ou
se a extensão não estiver rodando, ele sai na hora sem saída e o diálogo normal aparece no PC.

## Limites conhecidos

- **Permissões dos outros agentes** (só permissões; perguntas seguem no PC):
  - Codex: hook `PermissionRequest` em `~/.codex/hooks.json` (mesmo formato do Claude). O Codex pede para confiar no hook na primeira vez.
  - opencode: plugin `opencode/remoto.js` copiado para `~/.config/opencode/plugins/` (1.x, `server()`).
  - Antigravity: plugin em `~/.gemini/config/plugins/remoto/` (hook `PreToolUse` só de `run_command`, com
    `plugins.remoto.enabled` em `~/.gemini/config/config.json`). Sem decisão, o hook devolve `ask` e o PC pergunta.
- **Resposta vazia do Claude:** quando o Claude chama `reply_to_session` com o campo errado (`message`), o lich aceita
  e devolve vazio; a extensão recupera o texto do transcript em `~/.claude/projects`.
- **Resposta "unread" do Codex/Antigravity:** o lich dá a tarefa como não lida em ~60s quando o agente não avisa que
  a leu, e descarta o ticket. A extensão segue esperando até 30 min e pega a resposta no transcript
  (`~/.codex/sessions`, `~/.gemini/antigravity-cli/brain`).
- **Confiança da pasta:** uma sessão Claude aberta pelo celular marca o projeto como confiável no `~/.claude.json`
  antes de abrir (os worktrees em `.worktrees/` herdam). Já o "Hooks need review" do Codex não tem como ser
  respondido daqui: a API de extensões não aperta teclas. Ele só volta quando algum hook muda.
- O lich recusa mandar texto para uma sessão que está aguardando: responda a pendência primeiro.
- A lista atualiza a cada 2s (polling no `sessions.list`).
- O histórico de tarefas fica só na memória (as últimas 20 por sessão) e some quando a extensão reinicia.
