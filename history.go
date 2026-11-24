package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Histórico das sessões, lido do transcript de cada agente, para o celular ver também o que foi escrito no PC.
//
// Onde fica a conversa de cada sessão do lich:
//   - transcripts.json (gravado pelo próprio "remoto.exe hook" no início de cada conversa): Codex e Antigravity,
//     cujos hooks do lich não gravam o id da conversa no lich.db nesta máquina;
//   - lich.db, sessions.provider_session_id: Claude Code e opencode.

// Msg é uma linha do chat: o que a pessoa escreveu, o que o agente respondeu ou uma ferramenta que ele usou.
type Msg struct {
	Role string `json:"role"` // user | assistant | tool | error
	Text string `json:"text"`
	At   int64  `json:"at,omitempty"` // unix ms
}

const msgLimit = 20000 // um texto maior que isso é cortado: o celular não precisa do arquivo inteiro

// ---------- ligação sessão do lich -> transcript ----------

type transcriptRef struct {
	Path     string `json:"path,omitempty"`
	Provider string `json:"provider,omitempty"` // id da conversa no agente
	Kind     string `json:"kind,omitempty"`
	At       int64  `json:"at"`
}

var mapMu sync.Mutex

func transcriptsFile() string { return filepath.Join(dataDir(), "transcripts.json") }

func readTranscriptMap() map[string]transcriptRef {
	m := map[string]transcriptRef{}
	if b, err := os.ReadFile(transcriptsFile()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// recordTranscript guarda, se o payload for de início de conversa ou de prompt, onde está o transcript da
// sessão do lich. Devolve false quando o payload não é desses eventos.
// ponytail: lê e grava o arquivo inteiro a cada evento, sem trava entre processos; eventos raros (um por prompt).
func recordTranscript(sid string, payload []byte) bool {
	var p struct {
		Event          string `json:"hook_event_name"`
		AgyEvent       string `json:"hookEventName"`
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		ConversationID string `json:"conversationId"`
		AgyTranscript  string `json:"transcriptPath"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return false
	}
	ref := transcriptRef{At: time.Now().Unix()}
	switch {
	case p.Event == "SessionStart" || p.Event == "UserPromptSubmit":
		ref.Path, ref.Provider = p.TranscriptPath, p.SessionID
	case p.AgyEvent == "PreInvocation":
		ref.Path, ref.Provider, ref.Kind = p.AgyTranscript, p.ConversationID, "antigravity"
	default:
		return false
	}
	if sid == "" || (ref.Path == "" && ref.Provider == "") {
		return true
	}
	mapMu.Lock()
	defer mapMu.Unlock()
	m := readTranscriptMap()
	if old, ok := m[sid]; ok && old.Path == ref.Path && old.Provider == ref.Provider {
		return true
	}
	m[sid] = ref
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := transcriptsFile() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, transcriptsFile())
	}
	return true
}

// lichProvider é o kind e o id da conversa que o lich guarda para a sessão.
func lichProvider(sid string) (kind, provider string) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dataDir(), "..", "..", "lich.db"))+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return "", ""
	}
	defer db.Close()
	db.QueryRow("select kind, provider_session_id from sessions where id = ?", sid).Scan(&kind, &provider)
	return kind, provider
}

// source diz de onde ler a conversa: um arquivo JSONL (e o formato) ou uma sessão do opencode.
type source struct {
	kind string // claude | codex | antigravity | opencode
	path string // JSONL; vazio no opencode
	id   string // conversa no agente
}

func findSource(sid, kind string) (source, error) {
	home, _ := os.UserHomeDir()
	ref, ok := readTranscriptMap()[sid]
	lk, provider := lichProvider(sid)
	if kind == "" {
		kind = lk
	}
	if ok && ref.Kind != "" {
		kind = ref.Kind
	}
	if ok && ref.Provider != "" {
		provider = ref.Provider
	}
	if ok && ref.Path != "" {
		if _, err := os.Stat(ref.Path); err == nil && kind != "antigravity" {
			return source{kind: kind, path: ref.Path, id: provider}, nil
		}
	}
	if provider == "" {
		return source{}, fmt.Errorf("o lich ainda não sabe qual é a conversa desta sessão; ela aparece depois do próximo prompt")
	}
	first := func(pattern string) string {
		m, _ := filepath.Glob(pattern)
		if len(m) == 0 {
			return ""
		}
		return m[len(m)-1]
	}
	var p string
	switch kind {
	case "claude":
		p = first(filepath.Join(claudeProjectsDir(), "*", provider+".jsonl"))
	case "codex":
		codex := os.Getenv("CODEX_HOME")
		if codex == "" {
			codex = filepath.Join(home, ".codex")
		}
		p = first(filepath.Join(codex, "sessions", "*", "*", "*", "rollout-*-"+provider+".jsonl"))
	case "antigravity":
		for _, app := range []string{"antigravity-cli", "antigravity"} {
			if p = filepath.Join(home, ".gemini", app, "brain", provider, ".system_generated", "logs", "transcript.jsonl"); fileExists(p) {
				break
			}
			p = ""
		}
	case "opencode":
		return source{kind: kind, id: provider}, nil
	}
	if p == "" {
		return source{}, fmt.Errorf("transcript de %s %s não encontrado", kind, provider)
	}
	return source{kind: kind, path: p, id: provider}, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func claudeProjectsDir() string {
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".claude")
	}
	return filepath.Join(root, "projects")
}

// ---------- leitura, com cache por versão do arquivo ----------

type cached struct {
	key  string
	msgs []Msg
}

var (
	histMu    sync.Mutex
	histCache = map[string]cached{}
)

// History devolve a conversa da sessão, relendo só quando o transcript mudou.
func History(sid, kind string) ([]Msg, error) {
	src, err := findSource(sid, kind)
	if err != nil {
		return nil, err
	}
	key := src.kind + "|" + src.id
	if src.path != "" {
		st, err := os.Stat(src.path)
		if err != nil {
			return nil, err
		}
		key += fmt.Sprintf("|%s|%d|%d", src.path, st.Size(), st.ModTime().UnixNano())
	} else {
		key += "|" + opencodeVersion(src.id)
	}
	histMu.Lock()
	c, ok := histCache[sid]
	histMu.Unlock()
	if ok && c.key == key {
		return c.msgs, nil
	}
	var msgs []Msg
	switch src.kind {
	case "claude":
		msgs, err = parseJSONL(src.path, claudeLine)
	case "codex":
		msgs, err = parseJSONL(src.path, codexLine)
	case "antigravity":
		msgs, err = parseJSONL(src.path, agyLine)
	case "opencode":
		msgs, err = opencodeHistory(src.id)
	default:
		err = fmt.Errorf("agente %q sem leitor de histórico", src.kind)
	}
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		if msgs[i].Role == "user" {
			msgs[i].Text = relayTask(msgs[i].Text)
		}
	}
	histMu.Lock()
	histCache[sid] = cached{key, msgs}
	histMu.Unlock()
	return msgs, nil
}

func parseJSONL(path string, line func([]byte) []Msg) ([]Msg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Msg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		for _, m := range line(sc.Bytes()) {
			m.Text = clip(strings.TrimSpace(m.Text))
			if m.Text != "" {
				out = append(out, m)
			}
		}
	}
	return out, sc.Err()
}

func clip(s string) string {
	if len(s) > msgLimit {
		return strings.ToValidUTF8(s[:msgLimit], "") + "\n…"
	}
	return s
}

func ms(ts string) int64 {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// toolLine resume uma chamada de ferramenta: nome e o argumento que diz o que ela faz.
func toolLine(name string, args map[string]any) string {
	for _, k := range []string{"command", "CommandLine", "cmd", "file_path", "filePath", "TargetFile", "AbsolutePath", "path", "pattern", "url", "query", "description", "Query"} {
		if v, ok := args[k]; ok {
			s := fmt.Sprint(v)
			if a, ok := v.([]any); ok && len(a) > 0 {
				s = fmt.Sprint(a[len(a)-1])
			}
			if s != "" {
				if len(s) > 300 {
					s = s[:300] + "…"
				}
				return name + ": " + s
			}
		}
	}
	return name
}

// claudeLine: {"type":"user"|"assistant","message":{"content": texto | blocos}}. Fica de fora o que não foi a
// pessoa nem o agente escrevendo: isMeta, subagentes, resultados de ferramenta e avisos do sistema.
func claudeLine(b []byte) []Msg {
	var r struct {
		Type        string `json:"type"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &r) != nil || r.IsMeta || r.IsSidechain || (r.Type != "user" && r.Type != "assistant") {
		return nil
	}
	at := ms(r.Timestamp)
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		if r.Type == "user" && !strings.HasPrefix(strings.TrimSpace(s), "<") {
			return []Msg{{Role: "user", Text: s, At: at}}
		}
		return nil
	}
	var blocks []struct {
		Type  string         `json:"type"`
		Text  string         `json:"text"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	}
	json.Unmarshal(r.Message.Content, &blocks)
	var out []Msg
	for _, bl := range blocks {
		switch {
		case bl.Type == "text" && r.Type == "user" && !strings.HasPrefix(strings.TrimSpace(bl.Text), "<"):
			out = append(out, Msg{Role: "user", Text: bl.Text, At: at})
		case bl.Type == "text" && r.Type == "assistant":
			out = append(out, Msg{Role: "assistant", Text: bl.Text, At: at})
		case bl.Type == "tool_use":
			out = append(out, Msg{Role: "tool", Text: toolLine(bl.Name, bl.Input), At: at})
		}
	}
	return out
}

// codexLine: os itens concluídos do turno (event_msg/item_completed), que já separam o que a pessoa escreveu
// das instruções que o Codex injeta.
func codexLine(b []byte) []Msg {
	var r struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type string `json:"type"`
			Item struct {
				Type    string                  `json:"type"`
				Content []struct{ Text string } `json:"content"`
				Command json.RawMessage         `json:"command"`
				Server  string                  `json:"server"`
				Tool    string                  `json:"tool"`
			} `json:"item"`
		} `json:"payload"`
	}
	if !bytes.Contains(b, []byte(`"item_completed"`)) || json.Unmarshal(b, &r) != nil || r.Payload.Type != "item_completed" {
		return nil
	}
	it, at := r.Payload.Item, ms(r.Timestamp)
	text := func() string {
		var parts []string
		for _, c := range it.Content {
			parts = append(parts, c.Text)
		}
		return strings.Join(parts, "\n")
	}
	switch it.Type {
	case "UserMessage":
		return []Msg{{Role: "user", Text: text(), At: at}}
	case "AgentMessage":
		return []Msg{{Role: "assistant", Text: text(), At: at}}
	case "CommandExecution":
		var cmd []string
		if json.Unmarshal(it.Command, &cmd) != nil || len(cmd) == 0 {
			var s string
			json.Unmarshal(it.Command, &s)
			cmd = []string{s}
		}
		return []Msg{{Role: "tool", Text: toolLine("Shell", map[string]any{"command": cmd[len(cmd)-1]}), At: at}}
	case "McpToolCall":
		return []Msg{{Role: "tool", Text: it.Server + "/" + it.Tool, At: at}}
	}
	return nil
}

// agyLine: passos do Antigravity. O pedido da pessoa vem entre <USER_REQUEST>, com metadados em volta.
func agyLine(b []byte) []Msg {
	var r struct {
		Type      string `json:"type"`
		Content   string `json:"content"`
		Error     string `json:"error"`
		CreatedAt string `json:"created_at"`
		ToolCalls []struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	at := ms(r.CreatedAt)
	switch r.Type {
	case "USER_INPUT":
		s := r.Content
		if i := strings.Index(s, "<USER_REQUEST>"); i >= 0 {
			s = s[i+len("<USER_REQUEST>"):]
			if j := strings.Index(s, "</USER_REQUEST>"); j >= 0 {
				s = s[:j]
			}
		}
		return []Msg{{Role: "user", Text: s, At: at}}
	case "PLANNER_RESPONSE":
		out := []Msg{{Role: "assistant", Text: r.Content, At: at}}
		for _, t := range r.ToolCalls {
			out = append(out, Msg{Role: "tool", Text: toolLine(t.Name, t.Args), At: at})
		}
		return out
	case "ERROR_MESSAGE":
		return []Msg{{Role: "error", Text: r.Error, At: at}}
	}
	return nil
}

// ---------- opencode (SQLite) ----------

func opencodeDB() (*sql.DB, error) {
	home, _ := os.UserHomeDir()
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "share")
	}
	return sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(base, "opencode", "opencode.db"))+"?mode=ro&_pragma=busy_timeout(2000)")
}

func opencodeVersion(id string) string {
	db, err := opencodeDB()
	if err != nil {
		return ""
	}
	defer db.Close()
	var n, t sql.NullInt64
	db.QueryRow("select count(*), max(time_updated) from part where session_id = ?", id).Scan(&n, &t)
	return fmt.Sprintf("%d|%d", n.Int64, t.Int64)
}

// opencodeHistory lê as partes de texto e de ferramenta das mensagens (opencode 1.x: tabelas message e part).
func opencodeHistory(id string) ([]Msg, error) {
	db, err := opencodeDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`select m.data, p.data, p.time_created from part p join message m on m.id = p.message_id
		where p.session_id = ? order by m.time_created, p.time_created, p.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Msg
	for rows.Next() {
		var md, pd string
		var at int64
		if rows.Scan(&md, &pd, &at) != nil {
			continue
		}
		var m struct{ Role string }
		var p struct {
			Type      string
			Text      string
			Synthetic bool
			Tool      string
			State     struct {
				Input map[string]any
			}
		}
		json.Unmarshal([]byte(md), &m)
		json.Unmarshal([]byte(pd), &p)
		switch {
		case p.Type == "text" && !p.Synthetic && strings.TrimSpace(p.Text) != "":
			role := "assistant"
			if m.Role == "user" {
				role = "user"
			}
			out = append(out, Msg{Role: role, Text: clip(strings.TrimSpace(p.Text)), At: at})
		case p.Type == "tool":
			out = append(out, Msg{Role: "tool", Text: toolLine(p.Tool, p.State.Input), At: at})
		}
	}
	return out, rows.Err()
}

// relayTask tira o envelope que o lich põe numa tarefa repassada (origem, ticket, instrução de resposta) e
// deixa só o pedido, marcado como vindo de outra sessão ou do celular.
func relayTask(s string) string {
	i := strings.Index(s, "[lich] ")
	if i < 0 || i > 80 {
		return s
	}
	body := s[i:]
	j := strings.Index(body, "\n\n")
	if j < 0 {
		return s
	}
	body = body[j+2:]
	for _, end := range []string{"\n\nQuando tiver uma resposta", "\n\nWhen you have an answer", "\n\nCuando tengas una respuesta"} {
		if k := strings.Index(body, end); k >= 0 {
			body = body[:k]
			break
		}
	}
	return "📱 " + strings.TrimSpace(body)
}
