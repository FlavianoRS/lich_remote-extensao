package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// trustClaude marca dir como confiável no ~/.claude.json, como o "Yes, I trust this folder" faria.
// Abrir uma sessão pelo celular já é a decisão de trabalhar nessa pasta, e o diálogo de confiança travaria a
// sessão até alguém ir ao PC. Os worktrees do lich ficam dentro do projeto e herdam a confiança dele.
// O home nunca é gravado (o Claude não aceita confiança persistente ali).
//
// ponytail: lê e grava o arquivo inteiro sem a trava do Claude; uma sessão do Claude gravando no mesmo
// instante pode desfazer a marca, e o diálogo volta a aparecer uma vez.
func trustClaude(dir string) error {
	home, _ := os.UserHomeDir()
	if dir == "" || strings.EqualFold(filepath.Clean(dir), filepath.Clean(home)) {
		return nil
	}
	cfg := filepath.Join(home, ".claude.json")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		cfg = filepath.Join(d, ".claude.json")
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return err
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw, ok := top["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return err
		}
	}
	key := filepath.ToSlash(filepath.Clean(dir))
	p := projects[key]
	if p == nil {
		p = map[string]json.RawMessage{}
	}
	if string(p["hasTrustDialogAccepted"]) == "true" {
		return nil
	}
	p["hasTrustDialogAccepted"] = json.RawMessage("true")
	projects[key] = p
	if top["projects"], err = json.Marshal(projects); err != nil {
		return err
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfg + ".remoto.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cfg)
}

// trustAntigravity põe dirs em trustedWorkspaces do Antigravity CLI. Ali a confiança é por pasta exata (não
// herda da pasta-pai), então o worktree entra com o caminho que o lich vai criar.
func trustAntigravity(dirs ...string) error {
	home, _ := os.UserHomeDir()
	cfg := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	top := map[string]json.RawMessage{}
	if b, err := os.ReadFile(cfg); err == nil {
		if err := json.Unmarshal(b, &top); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	var list []string
	if raw, ok := top["trustedWorkspaces"]; ok {
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
	}
	added := false
	for _, d := range dirs {
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		known := false
		for _, x := range list {
			known = known || strings.EqualFold(filepath.Clean(x), d)
		}
		if !known {
			list, added = append(list, d), true
		}
	}
	if !added {
		return nil
	}
	var err error
	if top["trustedWorkspaces"], err = json.Marshal(list); err != nil {
		return err
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	tmp := cfg + ".remoto.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cfg)
}
