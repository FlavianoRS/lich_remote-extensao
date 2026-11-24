package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplyInTranscripts(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(body), 0o600)
	}
	// Claude com o campo errado, Codex (rollout) e Antigravity (transcript_full), cada um no seu formato.
	write("claude/p/s.jsonl", `{"type":"user","message":{"content":"ticket aaa"}}`+"\n"+
		`{"message":{"content":[{"type":"tool_use","name":"mcp__lich__reply_to_session","input":{"ticket":"aaa","message":"do claude"}}]}}`+"\n")
	write("codex/2026/11/24/r.jsonl", `{"item":{"type":"McpToolCall","tool":"reply_to_session","arguments":{"ticket":"bbb","answer":"do codex"}}}`+"\n")
	write("agy/x/.system_generated/logs/transcript_full.jsonl", `{"tool_calls":[{"args":{"Arguments":{"answer":"do agy","ticket":"ccc"},"ToolName":"reply_to_session"}}]}`+"\n")
	roots := []string{filepath.Join(dir, "claude"), filepath.Join(dir, "codex"), filepath.Join(dir, "agy"), filepath.Join(dir, "nao-existe")}
	since := time.Now().Add(-time.Hour)
	for ticket, want := range map[string]string{"aaa": "do claude", "bbb": "do codex", "ccc": "do agy", "zzz": ""} {
		if got := replyInTranscripts(roots, ticket, since); got != want {
			t.Errorf("%s: got %q want %q", ticket, got, want)
		}
	}
}

func TestTrustClaude(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	cfg := filepath.Join(dir, ".claude.json")
	os.WriteFile(cfg, []byte(`{"numStartups":3,"projects":{"C:/x":{"allowedTools":["a"]}}}`), 0o600)
	if err := trustClaude(`C:\x`); err != nil {
		t.Fatal(err)
	}
	if err := trustClaude(`C:\y\z`); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	var got struct {
		NumStartups int
		Projects    map[string]map[string]any
	}
	json.Unmarshal(b, &got)
	if got.NumStartups != 3 || got.Projects["C:/x"]["hasTrustDialogAccepted"] != true || got.Projects["C:/x"]["allowedTools"] == nil || got.Projects["C:/y/z"]["hasTrustDialogAccepted"] != true {
		t.Fatalf("%s", b)
	}
}

func TestTrustAntigravity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	os.WriteFile(cfg, []byte(`{"model":"x","trustedWorkspaces":["C:\\a"]}`), 0o600)
	if err := trustAntigravity(`c:\A`, `C:\b`, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	var got struct {
		Model             string
		TrustedWorkspaces []string
	}
	json.Unmarshal(b, &got)
	if got.Model != "x" || len(got.TrustedWorkspaces) != 2 || got.TrustedWorkspaces[1] != `C:\b` {
		t.Fatalf("%s", b)
	}
}
