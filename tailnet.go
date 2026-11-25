package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lichdotdev/lich/extension"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

// serveTailnet põe a extensão na rede Tailscale do usuário como o nó "lich-remoto",
// em espaço de usuário (sem driver, sem admin), e serve o PWA em HTTPS na porta 443.
func (a *App) serveTailnet(mux *http.ServeMux) {
	key, _ := os.ReadFile(filepath.Join(a.dir, "authkey")) // opcional: login sem navegador
	s := &tsnet.Server{
		Hostname: "lich-remoto",
		Dir:      filepath.Join(a.dir, "tsnet"),
		AuthKey:  strings.TrimSpace(string(key)),
		Logf:     logger.Discard,
		UserLogf: log.Printf,
	}
	if err := s.Start(); err != nil {
		a.setTS(TSInfo{State: "Erro", Warn: err.Error()})
		return
	}
	lc, err := s.LocalClient()
	if err != nil {
		a.setTS(TSInfo{State: "Erro", Warn: err.Error()})
		return
	}

	var info TSInfo
	opened := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st, err := lc.StatusWithoutPeers(ctx)
		cancel()
		if err == nil {
			info = TSInfo{State: st.BackendState, AuthURL: st.AuthURL}
			if st.AuthURL != "" && st.AuthURL != opened {
				opened = st.AuthURL
				a.conn.Send(extension.OpenExternal.New(extension.OpenExternalData{URL: st.AuthURL}))
			}
			if st.BackendState == "Running" && st.Self != nil {
				info.Addr = strings.TrimSuffix(st.Self.DNSName, ".")
				// Sem HTTPS na tailnet o ListenTLS falha; espera o usuário ligar, sem reiniciar.
				if len(st.CertDomains) == 0 {
					info.Warn = "Ative HTTPS em login.tailscale.com/admin/dns (MagicDNS + HTTPS Certificates). Tentando de novo a cada 5s."
					a.setTS(info)
					time.Sleep(5 * time.Second)
					continue
				}
				a.setTS(info)
				self := st.Self.UserID
				ln, err := s.ListenTLS("tcp", ":443")
				if err != nil {
					log.Print("ListenTLS: ", err)
					a.setTS(TSInfo{State: "Erro", Addr: info.Addr, Warn: err.Error() + " (tentando de novo)"})
					time.Sleep(5 * time.Second)
					continue
				}
				a.mu.Lock()
				a.tsrv = s
				funnel := a.cfg.Funnel
				a.mu.Unlock()
				if funnel {
					go a.startFunnel()
				} else {
					go clearFunnelConfig(s)
				}
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
					if err != nil || who.Node == nil || who.Node.User != self {
						httpErr(w, 403, "dispositivo não autorizado")
						return
					}
					// CSRF: navegador não manda header próprio entre origens sem CORS, que não liberamos.
					if r.Method == http.MethodPost && r.Header.Get("X-Remoto") != "1" {
						httpErr(w, 403, "falta X-Remoto")
						return
					}
					secHeaders(w)
					mux.ServeHTTP(w, r)
				})
				log.Print("PWA em https://" + info.Addr)
				log.Print(http.Serve(ln, h))
				return
			}
		}
		a.setTS(info)
		time.Sleep(2 * time.Second)
	}
}

func (a *App) setTS(t TSInfo) {
	a.mu.Lock()
	a.ts = t
	a.mu.Unlock()
	a.changed()
}

// ---------- tela no lich ----------

func (a *App) render() {
	a.mu.Lock()
	if !a.surface {
		a.mu.Unlock()
		return
	}
	ts, remote, np, ns := a.ts, a.cfg.Remote, len(a.pending), len(a.cfg.Subs)
	sessions := append([]extension.Session(nil), a.sessions...)
	follow := map[string]bool{}
	for k, v := range a.cfg.Follow {
		follow[k] = v
	}
	a.mu.Unlock()

	lbl := func(t string, size float64, w extension.FontWeight) extension.Node {
		return extension.Label.New(extension.LabelProps{Text: t, Size: size, Weight: w})
	}
	btn := func(t, h string, v extension.Variant) extension.Node {
		return extension.Button.New(extension.ButtonProps{Text: t, OnPress: extension.Handler(h), Variant: v})
	}
	kids := []extension.Node{lbl("Controle remoto", 20, extension.FontSemibold)}
	switch {
	case ts.Addr != "":
		kids = append(kids, lbl("Endereço no celular (app Tailscale ligado): https://"+ts.Addr, 14, extension.FontMedium))
	case ts.AuthURL != "":
		kids = append(kids, lbl("Entre na sua conta Tailscale para ligar o acesso pelo celular.", 14, ""), btn("Abrir login do Tailscale", "login", extension.VariantDefault))
	default:
		kids = append(kids, lbl("Tailscale: "+orDash(ts.State), 14, ""))
	}
	if ts.Warn != "" {
		kids = append(kids, lbl("⚠ "+ts.Warn, 13, ""))
	}
	mode := "desligado (perguntas aparecem só no PC)"
	if remote {
		mode = "ligado (perguntas e permissões vão para o celular)"
	}
	kids = append(kids,
		lbl("Modo remoto: "+mode, 14, ""),
		lbl("Pendências aguardando o celular: "+strconv.Itoa(np)+" · aparelhos com notificação: "+strconv.Itoa(ns), 13, ""),
	)
	row := []extension.Node{}
	if remote {
		row = append(row, btn("Desligar modo remoto", "remote-off", extension.VariantOutline))
	} else {
		row = append(row, btn("Ligar modo remoto", "remote-on", extension.VariantDefault))
	}
	if ts.Addr != "" {
		row = append(row, btn("Copiar endereço", "copy", extension.VariantSecondary))
	}
	kids = append(kids, extension.Box.New(extension.BoxProps{Direction: extension.DirectionRow, Gap: 8}, row...))
	kids = append(kids, a.funnelNodes(ts.Addr)...)

	// Acompanhar: o celular avisa quando a sessão termina um turno ou fica aguardando. O histórico aparece no
	// celular para qualquer sessão; marcar aqui é para começar no PC e seguir pelo celular.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Project < sessions[j].Project || sessions[i].Project == sessions[j].Project && sessions[i].Label < sessions[j].Label
	})
	kids = append(kids, extension.Separator.New(extension.NoProps{}),
		lbl("Acompanhar no celular", 16, extension.FontSemibold),
		lbl("Avisa no celular quando a sessão terminar um turno ou precisar de você.", 13, ""))
	for _, se := range sessions {
		kids = append(kids, extension.Box.New(extension.BoxProps{Direction: extension.DirectionRow, Gap: 8, Align: extension.AlignCenter},
			extension.Switch.New(extension.SwitchProps{Checked: follow[se.ID], OnChange: extension.Handler("follow:" + se.ID)}),
			lbl(se.Label+" · "+se.Project+" · "+se.Kind, 13, "")))
	}
	root := extension.Box.New(extension.BoxProps{Direction: extension.DirectionColumn, Gap: 12, Padding: 24}, kids...)
	a.conn.Send(extension.Render.New(extension.RenderData{Surface: "main", Root: root}))
}

func (a *App) onSurfaceEvent(h string) {
	a.mu.Lock()
	ts := a.ts
	a.mu.Unlock()
	if id, ok := strings.CutPrefix(h, "follow:"); ok {
		a.setFollow(id, !a.followed(id))
		return
	}
	if id, ok := strings.CutPrefix(h, "revoke:"); ok {
		a.revoke(id)
		a.render()
		return
	}
	switch h {
	case "funnel":
		a.mu.Lock()
		on := !a.cfg.Funnel
		a.mu.Unlock()
		a.setFunnel(on)
	case "pair":
		if ts.Addr != "" {
			a.newPairing(ts.Addr)
		}
	case "funnel-acl":
		a.conn.Send(extension.OpenExternal.New(extension.OpenExternalData{URL: "https://login.tailscale.com/admin/acls/file"}))
	case "remote-on":
		a.setRemote(true)
	case "remote-off":
		a.setRemote(false)
	case "login":
		if ts.AuthURL != "" {
			a.conn.Send(extension.OpenExternal.New(extension.OpenExternalData{URL: ts.AuthURL}))
		}
	case "copy":
		a.conn.Send(extension.Clipboard.New(extension.ClipboardData{Text: "https://" + ts.Addr}))
		a.conn.Send(extension.ShowToast.New(extension.ToastData{Title: "Endereço copiado"}))
	}
	a.render()
}

func orDash(s string) string {
	if s == "" {
		return "iniciando…"
	}
	return s
}

// ---------- ícone do PWA (gerado, sem arquivo binário no repo) ----------

func serveIcon(w http.ResponseWriter, r *http.Request) {
	size := 192
	if strings.Contains(r.URL.Path, "512") {
		size = 512
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0x0f, 0x76, 0x6e, 0xff}
	fg := color.RGBA{0xff, 0xff, 0xff, 0xff}
	u := size / 16
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, bg)
		}
	}
	fill := func(x0, y0, x1, y1 int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.Set(x, y, fg)
			}
		}
	}
	// ">" em degraus + "_"
	for i := 0; i < 3; i++ {
		fill(4*u+i*u, 5*u+i*u, 5*u+i*u+u/2, 6*u+i*u)
		fill(4*u+i*u, 11*u-i*u-u, 5*u+i*u+u/2, 11*u-i*u)
	}
	fill(4*u+3*u, 7*u+u/2, 4*u+4*u+u/2, 8*u+u/2)
	fill(9*u, 10*u, 12*u, 11*u)
	var buf bytes.Buffer
	png.Encode(&buf, img)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(buf.Bytes())
}
