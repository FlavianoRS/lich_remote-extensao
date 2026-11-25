package main

// Funnel: o mesmo PWA pela internet, para o celular sem o app Tailscale. Desligado por padrão.
// Pela internet só passa aparelho pareado: o pareamento usa um código de uso único que vale 5 minutos,
// mostrado (com QR) na tela da extensão no lich; o aparelho ganha um token num cookie HttpOnly e a
// extensão guarda só o hash dele. Revogar apaga o hash. A porta dos hooks (loopback) nunca vai para fora.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lichdotdev/lich/extension"
	"rsc.io/qr"
	"tailscale.com/tsnet"
)

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hash     string `json:"hash"` // sha256 do token; o token só existe no cookie do aparelho
	Created  int64  `json:"created"`
	LastSeen int64  `json:"lastSeen"`
}

type funnelState struct {
	ln    net.Listener
	err   string
	code  string
	exp   time.Time
	qr    []byte
	fails map[string][]time.Time // ip -> falhas de pareamento recentes
}

const (
	cookieName  = "remoto_dev"
	codeTTL     = 5 * time.Minute
	failWindow  = 10 * time.Minute
	failsPerIP  = 5
	failsGlobal = 30
)

// startFunnel abre o listener público (só Funnel; a tailnet continua no ListenTLS de sempre).
func (a *App) startFunnel() {
	a.mu.Lock()
	s, on, busy := a.tsrv, a.cfg.Funnel, a.fun.ln != nil
	a.mu.Unlock()
	if s == nil || !on || busy {
		return
	}
	ln, err := s.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
	a.mu.Lock()
	if err != nil {
		a.fun.err = err.Error()
	} else if !a.cfg.Funnel { // desligaram enquanto abria
		ln.Close()
		ln = nil
	} else {
		a.fun.ln, a.fun.err = ln, ""
	}
	a.mu.Unlock()
	a.render()
	if err != nil || ln == nil {
		log.Print("funnel: ", err)
		return
	}
	a.funnelLog("-", "funnel ligado")
	srv := &http.Server{Handler: a.funnelHandler(a.mux), ReadHeaderTimeout: 10 * time.Second}
	log.Print("funnel: ", srv.Serve(ln))
}

// stopFunnel fecha o listener; o tsnet tira a liberação do Funnel da porta ao fechar.
func (a *App) stopFunnel() {
	a.mu.Lock()
	ln := a.fun.ln
	a.fun.ln, a.fun.code, a.fun.qr = nil, "", nil
	a.mu.Unlock()
	if ln != nil {
		ln.Close()
		a.funnelLog("-", "funnel desligado")
	}
}

// clearFunnelConfig tira uma liberação de Funnel que tenha sobrado de uma execução que caiu.
func clearFunnelConfig(s *tsnet.Server) {
	lc, err := s.LocalClient()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sc, err := lc.GetServeConfig(ctx)
	if err != nil || sc == nil || len(sc.AllowFunnel) == 0 {
		return
	}
	sc.AllowFunnel = nil
	lc.SetServeConfig(ctx, sc)
}

func (a *App) setFunnel(on bool) {
	a.mu.Lock()
	a.cfg.Funnel = on
	a.fun.err = ""
	a.saveConfigLocked()
	a.mu.Unlock()
	if on {
		go a.startFunnel()
	} else {
		a.stopFunnel()
	}
}

// newPairing gera um código novo (invalida o anterior) e o QR com o link de pareamento.
func (a *App) newPairing(addr string) {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // sem 0/O, 1/I/L
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	code := string(b)
	var png []byte
	if c, err := qr.Encode("https://"+addr+"/pair?c="+code, qr.M); err == nil {
		png = c.PNG()
	}
	a.mu.Lock()
	a.fun.code, a.fun.exp, a.fun.qr = code, time.Now().Add(codeTTL), png
	a.mu.Unlock()
}

func (a *App) revoke(id string) {
	a.mu.Lock()
	for i, d := range a.cfg.Devices {
		if d.ID == id {
			a.cfg.Devices = append(a.cfg.Devices[:i], a.cfg.Devices[i+1:]...)
			a.saveConfigLocked()
			a.mu.Unlock()
			a.funnelLog("-", "revogado "+d.Name)
			return
		}
	}
	a.mu.Unlock()
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// deviceFor devolve o aparelho pareado do cookie, ou nil.
func (a *App) deviceFor(r *http.Request) *Device {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != 64 {
		return nil
	}
	h := []byte(hashToken(c.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.cfg.Devices {
		d := &a.cfg.Devices[i]
		if subtle.ConstantTimeCompare(h, []byte(d.Hash)) == 1 {
			if now := time.Now().Unix(); now-d.LastSeen > 300 {
				d.LastSeen = now
				a.saveConfigLocked()
			}
			cp := *d
			return &cp
		}
	}
	return nil
}

// tooManyFails aplica o limite de tentativas: por IP e no total (contra varredura distribuída).
func (a *App) tooManyFails(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := time.Now().Add(-failWindow)
	total := 0
	for k, ts := range a.fun.fails {
		n := 0
		for _, t := range ts {
			if t.After(cut) {
				ts[n] = t
				n++
			}
		}
		if n == 0 {
			delete(a.fun.fails, k)
			continue
		}
		a.fun.fails[k] = ts[:n]
		total += n
	}
	return len(a.fun.fails[ip]) >= failsPerIP || total >= failsGlobal
}

func (a *App) addFail(ip string) {
	a.mu.Lock()
	if a.fun.fails == nil {
		a.fun.fails = map[string][]time.Time{}
	}
	a.fun.fails[ip] = append(a.fun.fails[ip], time.Now())
	a.mu.Unlock()
}

// pair troca um código válido por um aparelho novo; "" = código errado, vencido ou já usado.
func (a *App) pair(code, name string) (token, devName string) {
	code = strings.ToUpper(strings.TrimSpace(code))
	a.mu.Lock()
	defer a.mu.Unlock()
	ok := a.fun.code != "" && time.Now().Before(a.fun.exp) &&
		subtle.ConstantTimeCompare([]byte(code), []byte(a.fun.code)) == 1
	if !ok {
		return "", ""
	}
	a.fun.code, a.fun.qr = "", nil // uso único
	b := make([]byte, 32)
	rand.Read(b)
	token = hex.EncodeToString(b)
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name))
	if name == "" {
		name = "Celular"
	}
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40])
	}
	now := time.Now().Unix()
	a.cfg.Devices = append(a.cfg.Devices, Device{ID: newID(), Name: name, Hash: hashToken(token), Created: now, LastSeen: now})
	a.saveConfigLocked()
	return token, name
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) funnelHandler(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		secHeaders(w)
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		if r.URL.Path == "/pair" {
			a.pairPage(w, r, ip)
			return
		}
		// Manifesto e ícones não têm dado nenhum, e o navegador os busca sem cookie ao instalar o app.
		public := r.URL.Path == "/manifest.webmanifest" || strings.HasPrefix(r.URL.Path, "/icon-")
		if !public && a.deviceFor(r) == nil {
			if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Redirect(w, r, "/pair", http.StatusSeeOther)
				return
			}
			httpErr(w, 401, "aparelho não pareado")
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("X-Remoto") != "1" {
			httpErr(w, 403, "falta X-Remoto")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

var pairTmpl = template.Must(template.New("pair").Parse(`<!doctype html>
<html lang="pt-BR"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="theme-color" content="#0f766e"><title>Parear · lich remoto</title>
<style>
:root{--bg:#f6f7f8;--card:#fff;--fg:#1b1f23;--mute:#6a737d;--line:#e1e4e8;--accent:#0f766e;--bad:#b91c1c;color-scheme:light dark}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--card:#161b22;--fg:#e6edf3;--mute:#8b949e;--line:#30363d;--accent:#2dd4bf;--bad:#f87171}}
*{box-sizing:border-box}body{margin:0;min-height:100dvh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:16px/1.45 system-ui,sans-serif;padding:16px}
form{width:min(400px,100%);background:var(--card);border:1px solid var(--line);border-radius:16px;padding:20px;display:grid;gap:14px}
h1{margin:0;font-size:20px}p{margin:0;color:var(--mute);font-size:14px}label{display:grid;gap:6px;font-size:14px}
input{font:inherit;padding:12px;border-radius:10px;border:1px solid var(--line);background:var(--bg);color:var(--fg)}
input[name=c]{font:600 22px/1 ui-monospace,monospace;letter-spacing:.2em;text-transform:uppercase;text-align:center}
button{font:inherit;font-weight:600;border:0;border-radius:10px;padding:12px;min-height:48px;background:var(--accent);color:#fff}
.err{color:var(--bad);font-size:14px}
</style></head><body>
<form method="post" action="/pair">
<h1>Parear este aparelho</h1>
<p>No PC, abra o lich › Controle remoto › Parear celular e digite o código mostrado lá. Ele vale 5 minutos e serve uma vez.</p>
{{if .Err}}<div class="err" role="alert">{{.Err}}</div>{{end}}
<label>Código<input name="c" value="{{.Code}}" required autocomplete="one-time-code" autocapitalize="characters" maxlength="8" inputmode="text"></label>
<label>Nome deste aparelho<input name="n" value="Celular" maxlength="40"></label>
<button>Parear</button>
</form></body></html>`))

func (a *App) pairPage(w http.ResponseWriter, r *http.Request, ip string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct{ Code, Err string }{Code: r.URL.Query().Get("c")}
	if r.Method == http.MethodPost {
		// CSRF: formulário só vale vindo da própria página.
		if o := r.Header.Get("Origin"); o != "" && o != "https://"+r.Host {
			httpErr(w, 403, "origem inválida")
			return
		}
		if a.tooManyFails(ip) {
			a.funnelLog(ip, "pareamento bloqueado (muitas tentativas)")
			w.WriteHeader(429)
			data.Err = "Muitas tentativas. Espere alguns minutos e gere um código novo no PC."
			pairTmpl.Execute(w, data)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		code, name := r.PostFormValue("c"), r.PostFormValue("n")
		token, name := a.pair(code, name)
		if token == "" {
			a.addFail(ip)
			a.funnelLog(ip, "pareamento recusado")
			w.WriteHeader(401)
			data.Code, data.Err = "", "Código errado, vencido ou já usado. Gere outro no PC."
			pairTmpl.Execute(w, data)
			return
		}
		a.funnelLog(ip, "pareado: "+name)
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", MaxAge: 400 * 24 * 3600,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		a.render()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		httpErr(w, 405, "método")
		return
	}
	pairTmpl.Execute(w, data)
}

// funnelLog registra pareamentos, recusas e liga/desliga em funnel.log. ponytail: corta em 256 KiB, sem rotação.
func (a *App) funnelLog(ip, msg string) {
	path := filepath.Join(a.dir, "funnel.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 256<<10 {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(time.Now().Format("2006-01-02 15:04:05") + " " + ip + " " + msg + "\n")
}

func secHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
}

// ---------- tela no lich ----------

func label(t string, size float64, w extension.FontWeight) extension.Node {
	return extension.Label.New(extension.LabelProps{Text: t, Size: size, Weight: w})
}

func button(t, h string, v extension.Variant) extension.Node {
	return extension.Button.New(extension.ButtonProps{Text: t, OnPress: extension.Handler(h), Variant: v})
}

func (a *App) funnelNodes(addr string) []extension.Node {
	a.mu.Lock()
	on, devices := a.cfg.Funnel, append([]Device(nil), a.cfg.Devices...)
	open, ferr, code, exp, png := a.fun.ln != nil, a.fun.err, a.fun.code, a.fun.exp, a.fun.qr
	a.mu.Unlock()

	status := "Desligado: o app só abre pelo Tailscale."
	switch {
	case on && open:
		status = "Ligado: https://" + addr + " abre de qualquer rede, só para aparelhos pareados."
	case on && ferr != "":
		status = "⚠ Não ligou: " + ferr
	case on:
		status = "Ligando…"
	}
	kids := []extension.Node{
		extension.Separator.New(extension.NoProps{}),
		label("Acesso sem Tailscale (Funnel)", 16, extension.FontSemibold),
		label("Abre o app pela internet, sem o Tailscale no celular. Só aparelhos pareados entram; quem tem um aparelho pareado controla os agentes deste PC.", 13, ""),
		extension.Box.New(extension.BoxProps{Direction: extension.DirectionRow, Gap: 8, Align: extension.AlignCenter},
			extension.Switch.New(extension.SwitchProps{Checked: on, OnChange: extension.Handler("funnel")}),
			label(status, 13, "")),
	}
	if on && strings.Contains(ferr, "node attribute") {
		kids = append(kids,
			label(`Libere o Funnel na sua tailnet: no painel do Tailscale, em Access controls, inclua em "nodeAttrs" {"target": ["autogroup:member"], "attr": ["funnel"]}, salve e ligue a chave de novo.`, 13, ""),
			button("Abrir Access controls do Tailscale", "funnel-acl", extension.VariantSecondary))
	}
	if open {
		kids = append(kids, button("Parear celular", "pair", extension.VariantDefault))
		if code != "" && time.Now().Before(exp) {
			if png != nil {
				kids = append(kids, extension.Image.New(extension.ImageProps{Data: png, Width: 220, Height: 220}))
			}
			kids = append(kids,
				extension.Label.New(extension.LabelProps{Text: code[:4] + "-" + code[4:], Size: 26, Weight: extension.FontSemibold, Mono: true}),
				label("Leia o QR no celular, ou abra https://"+addr+"/pair e digite o código (sem o hífen). Vale até "+exp.Format("15:04")+" e serve uma vez.", 13, ""))
		}
	}
	if len(devices) > 0 {
		kids = append(kids, label(fmt.Sprintf("Aparelhos pareados (%d)", len(devices)), 14, extension.FontMedium))
		for _, d := range devices {
			kids = append(kids, extension.Box.New(extension.BoxProps{Direction: extension.DirectionRow, Gap: 8, Align: extension.AlignCenter},
				label(d.Name+" · pareado em "+time.Unix(d.Created, 0).Format("02/01")+" · último acesso "+time.Unix(d.LastSeen, 0).Format("02/01 15:04"), 13, ""),
				button("Revogar", "revoke:"+d.ID, extension.VariantOutline)))
		}
	}
	return kids
}
