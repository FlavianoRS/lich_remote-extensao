package main

import (
	"encoding/json"
	"testing"
)

func TestHookOutput(t *testing.T) {
	q := &Pending{Kind: "question", Input: json.RawMessage(`{"questions":[{"question":"Qual?","options":[{"label":"A"}]}]}`)}
	out, _ := json.Marshal(hookOutput(q, Reply{Action: "answer", Answers: map[string]string{"Qual?": "A"}}))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"answers":{"Qual?":"A"},"questions":[{"options":[{"label":"A"}],"question":"Qual?"}]}}}`
	if string(out) != want {
		t.Fatalf("pergunta:\n got %s\nwant %s", out, want)
	}

	p := &Pending{Kind: "permission"}
	out, _ = json.Marshal(hookOutput(p, Reply{Action: "deny"}))
	if string(out) != `{"hookSpecificOutput":{"decision":{"behavior":"deny","message":"Negado pelo usuário no celular."},"hookEventName":"PermissionRequest"}}` {
		t.Fatalf("negar: %s", out)
	}
	if hookOutput(p, Reply{Action: "pc"}) != nil || hookOutput(q, Reply{Action: "answer"}) != nil {
		t.Fatal("pc / resposta vazia devem deixar a decisão para o PC")
	}
}

func TestHookOutputAntigravity(t *testing.T) {
	p := &Pending{Kind: "permission", agy: true}
	out, _ := json.Marshal(hookOutput(p, Reply{Action: "allow"}))
	if string(out) != `{"decision":"allow"}` {
		t.Fatalf("permitir: %s", out)
	}
	out, _ = json.Marshal(hookOutput(p, Reply{Action: "deny", Message: "não"}))
	if string(out) != `{"decision":"deny","reason":"não"}` {
		t.Fatalf("negar: %s", out)
	}
	if hookOutput(p, Reply{Action: "pc"}) != nil {
		t.Fatal("pc deve deixar a decisão para o PC")
	}
}

func TestAgyPayload(t *testing.T) {
	var hp hookPayload
	json.Unmarshal([]byte(`{"hookEventName":"PreToolUse","toolCall":{"name":"run_command","args":{"CommandLine":"npm test"}}}`), &hp)
	if hp.Event != "" || hp.AgyEvent != "PreToolUse" || hp.ToolCall.Name != "run_command" || string(hp.ToolCall.Args) != `{"CommandLine":"npm test"}` {
		t.Fatalf("%+v", hp)
	}
}
