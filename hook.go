package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// agyAsk é a saída "sem decisão" do Antigravity, que exige JSON: "ask" segue o fluxo normal do PC.
const agyAsk = `{"decision":"ask"}`

// runHook é o "remoto.exe hook" chamado pelo Claude Code (PreToolUse/AskUserQuestion e
// PermissionRequest), pelo Codex (PermissionRequest), pelo Antigravity (PreToolUse, camelCase) e pelo
// plugin do opencode. Qualquer falha sai com 0 e sem decisão: o fluxo normal do PC segue.
func runHook() int {
	payload, _ := io.ReadAll(os.Stdin)
	// Início de conversa / prompt (SessionStart, UserPromptSubmit, PreInvocation): só anota onde está o
	// transcript da sessão, para o histórico no celular. Nada a decidir; o Antigravity exige um JSON.
	if recordTranscript(os.Getenv("LICH_SESSION_ID"), payload) {
		if bytes.Contains(payload, []byte(`"hookEventName"`)) {
			os.Stdout.WriteString("{}")
		}
		return 0
	}
	decided := postHook(payload)
	if !decided && bytes.Contains(payload, []byte(`"hookEventName"`)) {
		os.Stdout.WriteString(agyAsk)
	}
	logHook(payload, decided)
	return 0
}

// logHook anota cada chamada em hook.log (evento, ferramenta, sessão, se decidiu) para descobrir qual
// agente não chega até a extensão. ponytail: corta o arquivo em 256 KiB, sem rotação.
func logHook(payload []byte, decided bool) {
	var hp hookPayload
	json.Unmarshal(payload, &hp)
	ev, tool := hp.Event, hp.ToolName
	if ev == "" {
		ev, tool = hp.AgyEvent, hp.ToolCall.Name
	}
	path := filepath.Join(dataDir(), "hook.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 256<<10 {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(time.Now().Format("2006-01-02 15:04:05") + " ev=" + ev + " tool=" + tool +
		" session=" + os.Getenv("LICH_SESSION_ID") + " decided=" + strconv.FormatBool(decided) +
		" bytes=" + strconv.Itoa(len(payload)) + "\n")
}

// postHook repassa o payload para a extensão e escreve a decisão; false = nenhuma decisão escrita.
func postHook(payload []byte) bool {
	sid := os.Getenv("LICH_SESSION_ID")
	if sid == "" || len(payload) == 0 {
		return false // fora do lich: não interfere
	}
	b, err := os.ReadFile(filepath.Join(dataDir(), "hook.json"))
	if err != nil {
		return false // extensão não está rodando
	}
	var hc struct {
		Port   int
		Secret string
	}
	if json.Unmarshal(b, &hc) != nil || hc.Port == 0 {
		return false
	}
	body, _ := json.Marshal(map[string]any{"session": sid, "payload": json.RawMessage(payload)})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(hc.Port)+"/hook", bytes.NewReader(body))
	req.Header.Set("X-Remoto-Secret", hc.Secret)
	// Sem timeout próprio: quem limita a espera é o "timeout" do hook no settings.json.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	io.Copy(os.Stdout, resp.Body)
	return true
}

type hookPayload struct {
	Event     string          `json:"hook_event_name"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	// Antigravity: camelCase e a chamada em toolCall.
	AgyEvent string `json:"hookEventName"`
	ToolCall struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

// hook recebe a pergunta do Claude Code, segura a requisição até o celular responder
// e devolve a saída do hook. 204 = sem decisão (o diálogo aparece no PC).
func (a *App) hook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Session string          `json:"session"`
		Payload json.RawMessage `json:"payload"`
	}
	var hp hookPayload
	if !readJSON(w, r, &in) || json.Unmarshal(in.Payload, &hp) != nil {
		return
	}
	agy := hp.Event == "" && hp.AgyEvent == "PreToolUse"
	if agy {
		hp.Event, hp.ToolName, hp.ToolInput = "PermissionRequest", hp.ToolCall.Name, hp.ToolCall.Args
	}
	a.mu.Lock()
	remote := a.cfg.Remote
	a.mu.Unlock()
	kind := ""
	switch {
	case hp.Event == "PreToolUse" && hp.ToolName == "AskUserQuestion":
		kind = "question"
	case hp.Event == "PermissionRequest":
		kind = "permission"
	}
	if !remote || kind == "" {
		w.WriteHeader(204)
		return
	}

	p := &Pending{
		ID: newID(), Session: in.Session, Label: a.labelOf(in.Session), Kind: kind,
		Tool: hp.ToolName, Input: hp.ToolInput, At: time.Now().Unix(),
		event: hp.Event, agy: agy, reply: make(chan Reply, 1),
	}
	a.mu.Lock()
	a.pending[p.ID] = p
	a.mu.Unlock()
	a.changed()
	defer func() {
		a.mu.Lock()
		delete(a.pending, p.ID)
		a.mu.Unlock()
		a.changed()
	}()
	if kind == "question" {
		a.push("❓ "+p.Label+" pergunta", firstQuestion(hp.ToolInput), "p-"+p.ID)
	} else {
		a.push("🔐 "+p.Label+" pede permissão", hp.ToolName, "p-"+p.ID)
	}

	select {
	case rep := <-p.reply:
		out := hookOutput(p, rep)
		if out == nil {
			w.WriteHeader(204)
			return
		}
		writeJSON(w, out)
	case <-r.Context().Done(): // hook encerrado (timeout ou respondido no PC)
	}
}

func firstQuestion(input json.RawMessage) string {
	var q struct {
		Questions []struct{ Question string } `json:"questions"`
	}
	json.Unmarshal(input, &q)
	if len(q.Questions) == 0 {
		return ""
	}
	return q.Questions[0].Question
}

// hookOutput monta a saída documentada do hook; nil = sem decisão.
func hookOutput(p *Pending, rep Reply) any {
	if p.agy { // Antigravity: {"decision": allow|deny|ask, "reason"}
		switch rep.Action {
		case "allow":
			return map[string]any{"decision": "allow"}
		case "deny":
			return map[string]any{"decision": "deny", "reason": denyMessage(rep)}
		}
		return nil
	}
	switch {
	case p.Kind == "question" && rep.Action == "answer" && len(rep.Answers) > 0:
		var input map[string]any
		if json.Unmarshal(p.Input, &input) != nil {
			return nil
		}
		input["answers"] = rep.Answers // pergunta -> rótulo escolhido (multi: separados por vírgula)
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":      "PreToolUse",
			"permissionDecision": "allow",
			"updatedInput":       input,
		}}
	case p.Kind == "permission" && rep.Action == "allow":
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      map[string]any{"behavior": "allow"},
		}}
	case p.Kind == "permission" && rep.Action == "deny":
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      map[string]any{"behavior": "deny", "message": denyMessage(rep)},
		}}
	}
	return nil
}

func denyMessage(rep Reply) string {
	if rep.Message != "" {
		return rep.Message
	}
	return "Negado pelo usuário no celular."
}
