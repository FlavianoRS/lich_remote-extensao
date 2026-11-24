package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// O lich às vezes perde uma resposta que o agente mandou:
//   - o Claude chama reply_to_session com o campo errado ("message" em vez de "answer"); o MCP do lich não
//     valida e entrega "answered" vazio;
//   - Codex e Antigravity leem a tarefa sem avisar o lich a tempo; ele dá "unread" em 30s e a extensão perde
//     o ticket, então a resposta que chega depois fica sem dono.
// Nos dois casos a chamada reply_to_session continua no transcript do agente, e é lida de lá.

// orTranscript completa uma resposta "answered" que chegou vazia.
func orTranscript(status, answer, ticket string) string {
	if status != "answered" || strings.TrimSpace(answer) != "" || ticket == "" {
		return answer
	}
	if s := replyInTranscripts(transcriptRoots(), ticket, time.Now().Add(-3*time.Hour)); s != "" {
		return s
	}
	return answer
}

// lateReply espera, até limit, a resposta de um ticket que o lich deu como "unread".
// ponytail: varre os transcripts a cada 10s; um evento de resposta na API de extensões dispensaria isso.
func lateReply(ticket string, limit time.Duration) string {
	since := time.Now().Add(-10 * time.Minute)
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(10 * time.Second) {
		if s := replyInTranscripts(transcriptRoots(), ticket, since); s != "" {
			return s
		}
	}
	return ""
}

// transcriptRoots são as pastas de transcript JSONL de Claude Code, Codex e Antigravity.
func transcriptRoots() []string {
	home, _ := os.UserHomeDir()
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return []string{
		filepath.Join(claude, "projects"),
		filepath.Join(codex, "sessions"),
		filepath.Join(home, ".gemini", "antigravity-cli", "brain"),
		filepath.Join(home, ".gemini", "antigravity", "brain"),
	}
}

// replyInTranscripts procura, nos .jsonl alterados depois de since, a chamada a reply_to_session com esse
// ticket e devolve o texto dela.
func replyInTranscripts(roots []string, ticket string, since time.Time) string {
	needle := []byte(`"ticket":"` + ticket + `"`)
	found := ""
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if found != "" {
				return fs.SkipAll
			}
			if err != nil {
				return nil // pasta que não existe ou sem acesso: segue
			}
			if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if info, err := d.Info(); err != nil || info.ModTime().Before(since) {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(b, needle) {
				return nil
			}
			for _, line := range bytes.Split(b, []byte("\n")) {
				if !bytes.Contains(line, needle) {
					continue
				}
				var v any
				if json.Unmarshal(line, &v) == nil {
					if s := replyIn(v, ticket); s != "" {
						found = s
						return fs.SkipAll
					}
				}
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	return ""
}

// replyIn acha, em qualquer profundidade, o objeto {"ticket": ticket, ...} e devolve "answer", ou os outros
// textos dele quando o agente usou outro nome de campo.
func replyIn(v any, ticket string) string {
	switch x := v.(type) {
	case map[string]any:
		if x["ticket"] == ticket {
			if s, ok := x["answer"].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				if s, ok := x[k].(string); ok && k != "ticket" && strings.TrimSpace(s) != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n\n")
			}
		}
		for _, c := range x {
			if s := replyIn(c, ticket); s != "" {
				return s
			}
		}
	case []any:
		for _, c := range x {
			if s := replyIn(c, ticket); s != "" {
				return s
			}
		}
	}
	return ""
}
