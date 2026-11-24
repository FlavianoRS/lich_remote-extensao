// remoto: extensão do lich que serve um PWA pela rede Tailscale (tsnet, sem admin)
// para acompanhar e comandar as sessões pelo celular.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/lichdotdev/lich/extension"
)

const extID = "remoto"

//go:embed web
var webFS embed.FS

func main() {
	log.SetFlags(0)
	log.SetPrefix("remoto: ")
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(runHook())
	}
	runExtension()
}

// Config é o que sobrevive a um restart, em extension-data/remoto/config.json.
type Config struct {
	Remote    bool                   `json:"remote"`
	VapidPub  string                 `json:"vapidPub"`
	VapidPriv string                 `json:"vapidPriv"`
	Subs      []webpush.Subscription `json:"subs"`
	Follow    map[string]bool        `json:"follow,omitempty"` // sessões acompanhadas: notificação ao terminar
}

type Entry struct {
	ID     string `json:"id"`
	At     int64  `json:"at"`
	Prompt string `json:"prompt"`
	Status string `json:"status"`
	Answer string `json:"answer,omitempty"`
}

type Pending struct {
	ID      string          `json:"id"`
	Session string          `json:"session"`
	Label   string          `json:"label"`
	Kind    string          `json:"kind"` // "question" | "permission"
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input"`
	At      int64           `json:"at"`
	event   string
	agy     bool // Antigravity: saída {"decision"}
	reply   chan Reply
}

type Reply struct {
	ID      string            `json:"id"`
	Action  string            `json:"action"` // answer | allow | deny | pc
	Answers map[string]string `json:"answers"`
	Message string            `json:"message"`
}

type TSInfo struct {
	State   string `json:"state"`
	AuthURL string `json:"authURL,omitempty"`
	Addr    string `json:"addr,omitempty"`
	Warn    string `json:"warn,omitempty"`
}

type App struct {
	conn *extension.Conn
	dir  string

	mu       sync.Mutex
	cfg      Config
	sessions []extension.Session
	projects []extension.Project
	history  map[string][]*Entry
	pending  map[string]*Pending
	ts       TSInfo
	clients  map[chan []byte]struct{}
	lastSnap string
	surface  bool
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func runExtension() {
	a := &App{
		conn:    extension.NewConn(os.Stdin, os.Stdout),
		dir:     os.Getenv(extension.DataDirEnv),
		history: map[string][]*Entry{},
		pending: map[string]*Pending{},
		clients: map[chan []byte]struct{}{},
	}
	if a.dir == "" {
		a.dir = dataDir()
	}
	os.MkdirAll(a.dir, 0o700)
	a.loadConfig()

	mux := a.routes()
	go a.serveLoopback(mux)
	go a.serveTailnet(mux)
	go a.poll()

	for msg := range a.conn.Messages() {
		switch msg.Kind {
		case extension.SurfaceOpened.Name:
			a.mu.Lock()
			a.surface = true
			a.mu.Unlock()
			a.render()
		case extension.SurfaceClosed.Name:
			a.mu.Lock()
			a.surface = false
			a.mu.Unlock()
		case extension.FireEvent.Name:
			if ev, ok := msg.Data.(extension.EventData); ok {
				a.onSurfaceEvent(string(ev.Handler))
			}
		}
	}
}

func dataDir() string {
	base, _ := os.UserConfigDir()
	return filepath.Join(base, "lich", "extension-data", extID)
}

func (a *App) loadConfig() {
	b, err := os.ReadFile(filepath.Join(a.dir, "config.json"))
	if err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	if a.cfg.VapidPriv == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			log.Print("vapid: ", err)
		}
		a.cfg.VapidPriv, a.cfg.VapidPub = priv, pub
		a.saveConfigLocked()
	}
}

func (a *App) saveConfigLocked() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(a.dir, "config.json"), b, 0o600); err != nil {
		log.Print("config: ", err)
	}
}

// ---------- estado ----------

type snapshot struct {
	Remote   bool                `json:"remote"`
	Follow   map[string]bool     `json:"follow"`
	TS       TSInfo              `json:"tailscale"`
	Projects []extension.Project `json:"projects"`
	Sessions []extension.Session `json:"sessions"`
	History  map[string][]*Entry `json:"history"`
	Pending  []*Pending          `json:"pending"`
}

func (a *App) snapshotLocked() []byte {
	p := make([]*Pending, 0, len(a.pending))
	for _, x := range a.pending {
		p = append(p, x)
	}
	sort.Slice(p, func(i, j int) bool { return p[i].At < p[j].At || p[i].At == p[j].At && p[i].ID < p[j].ID })
	b, _ := json.Marshal(snapshot{a.cfg.Remote, a.cfg.Follow, a.ts, a.projects, a.sessions, a.history, p})
	return b
}

// changed envia o estado novo para os navegadores conectados e redesenha a tela no lich.
func (a *App) changed() {
	a.mu.Lock()
	b := a.snapshotLocked()
	same := string(b) == a.lastSnap
	a.lastSnap = string(b)
	for c := range a.clients {
		select {
		case <-c: // descarta o snapshot antigo ainda não lido
		default:
		}
		c <- b
	}
	a.mu.Unlock()
	if !same {
		a.render()
	}
}

func (a *App) labelOf(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.ID == id {
			return s.Label
		}
	}
	return id
}

// ponytail: polling de 2s no sessions.list; trocar por events.subscribe(session.status) se a latência importar.
func (a *App) poll() {
	prev := map[string]string{}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ss, err1 := a.conn.Sessions(ctx)
		ps, err2 := a.conn.Projects(ctx)
		cancel()
		if err1 == nil && err2 == nil {
			a.mu.Lock()
			a.sessions, a.projects = ss, ps
			remote := a.cfg.Remote
			a.mu.Unlock()
			for _, s := range ss {
				followed := a.followed(s.ID)
				if (remote || followed) && s.State == "waiting" && prev[s.ID] != "waiting" {
					body := s.Reason
					if body == "" {
						body = s.Detail
					}
					a.push("⏳ "+s.Label+" aguardando", body, "s-"+s.ID)
				}
				// Sessão acompanhada que terminou o turno: a última resposta do agente vai no push.
				if followed && s.State == "done" && prev[s.ID] == "busy" {
					go a.pushDone(s)
				}
				prev[s.ID] = s.State
			}
			a.changed()
		} else if errors.Is(err1, context.Canceled) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// ---------- ações ----------

func (a *App) addEntry(session string, e *Entry) {
	a.mu.Lock()
	h := append(a.history[session], e)
	if len(h) > 20 {
		h = h[len(h)-20:]
	}
	a.history[session] = h
	a.mu.Unlock()
	a.changed()
}

func (a *App) setEntry(e *Entry, status, answer string) {
	a.mu.Lock()
	e.Status, e.Answer = status, answer
	a.mu.Unlock()
	a.changed()
}

func (a *App) follow(session string, e *Entry, ticket string) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		r, err := a.conn.Answer(ctx, extension.AnswerParams{Ticket: ticket, WaitSeconds: 90})
		cancel()
		if err != nil {
			a.setEntry(e, "erro", err.Error())
			return
		}
		if r.Status == "unread" { // o lich desistiu cedo; a resposta pode chegar pelo transcript
			a.setEntry(e, "pending", "")
			if s := lateReply(ticket, 30*time.Minute); s != "" {
				r.Status, r.Answer = "answered", s
			}
		}
		if r.Status != "pending" {
			r.Answer = orTranscript(r.Status, r.Answer, ticket)
			a.setEntry(e, r.Status, r.Answer)
			a.mu.Lock()
			remote := a.cfg.Remote
			a.mu.Unlock()
			if remote {
				a.push("✅ "+a.labelOf(session)+" respondeu", r.Answer, "a-"+e.ID)
			}
			return
		}
	}
}

func (a *App) send(session, prompt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := a.conn.SendToSession(ctx, extension.SendParams{Session: session, Prompt: prompt, WaitSeconds: 3})
	if err != nil {
		return err
	}
	e := &Entry{ID: newID(), At: time.Now().Unix(), Prompt: prompt, Status: r.Status, Answer: orTranscript(r.Status, r.Answer, r.Ticket)}
	a.addEntry(session, e)
	if r.Status == "pending" {
		go a.follow(session, e, r.Ticket)
	}
	return nil
}

// projectPath é a pasta do projeto pelo nome ou caminho que o PWA mandou.
func (a *App) projectPath(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, pr := range a.projects {
		if pr.Name == name || pr.Path == name {
			return pr.Path
		}
	}
	return ""
}

func (a *App) open(p extension.OpenSessionParams) (extension.OpenedSession, error) {
	// Abrir pelo celular já é a decisão de trabalhar nessa pasta: o diálogo de confiança travaria a sessão
	// até alguém ir ao PC. "" é o agente padrão do projeto, que pode ser qualquer um.
	dir := a.projectPath(p.Project)
	if p.Provider == "" || p.Provider == "claude" {
		if err := trustClaude(dir); err != nil {
			log.Print("confiar na pasta (claude): ", err)
		}
	}
	if (p.Provider == "" || p.Provider == "antigravity") && dir != "" {
		wt := ""
		if p.Worktree != "" { // ponytail: supõe o .worktrees/<branch> do lich; branch com "/" pode divergir
			wt = filepath.Join(dir, ".worktrees", p.Worktree)
		}
		if err := trustAntigravity(dir, wt); err != nil {
			log.Print("confiar na pasta (antigravity): ", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := a.conn.OpenSession(ctx, p)
	if err != nil {
		return o, err
	}
	if p.Prompt != "" {
		e := &Entry{ID: newID(), At: time.Now().Unix(), Prompt: p.Prompt, Status: "pending"}
		if o.TaskError != "" {
			e.Status, e.Answer = "erro", o.TaskError
		} else if o.Task != nil {
			e.Status, e.Answer = o.Task.Status, o.Task.Answer
		}
		a.addEntry(o.ID, e)
		if o.Task != nil && o.Task.Status == "pending" {
			go a.follow(o.ID, e, o.Task.Ticket)
		}
	}
	return o, nil
}

// closeSession usa o CLI do lich: a API de extensões não tem sessions.close.
func (a *App) closeSession(id string, removeWorktree bool) (string, error) {
	bin, err := exec.LookPath("lich")
	if err != nil {
		bin = filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "lich", "lich.exe")
	}
	wt := "keep"
	if removeWorktree {
		wt = "remove"
	}
	out, err := exec.Command(bin, "close", "--json", "--worktree", wt, id).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (a *App) setRemote(on bool) {
	a.mu.Lock()
	a.cfg.Remote = on
	a.saveConfigLocked()
	var release []*Pending
	if !on {
		for _, p := range a.pending {
			release = append(release, p)
		}
	}
	a.mu.Unlock()
	for _, p := range release { // modo remoto desligado: devolve as pendências para o PC
		select {
		case p.reply <- Reply{Action: "pc"}:
		default:
		}
	}
	a.changed()
}

// ---------- notificações push ----------

func (a *App) push(title, body, tag string) {
	if len(body) > 300 {
		body = strings.ToValidUTF8(body[:300], "") + "…"
	}
	payload, _ := json.Marshal(map[string]string{"title": title, "body": body, "tag": tag})
	a.mu.Lock()
	subs := append([]webpush.Subscription(nil), a.cfg.Subs...)
	pub, priv := a.cfg.VapidPub, a.cfg.VapidPriv
	a.mu.Unlock()
	var dead []string
	for i := range subs {
		resp, err := webpush.SendNotification(payload, &subs[i], &webpush.Options{
			Subscriber:      "https://github.com/lichdotdev/lich",
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             600,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			log.Print("push: ", err)
			continue
		}
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			dead = append(dead, subs[i].Endpoint)
		}
		resp.Body.Close()
	}
	if len(dead) > 0 {
		a.mu.Lock()
		kept := a.cfg.Subs[:0]
		for _, s := range a.cfg.Subs {
			gone := false
			for _, d := range dead {
				gone = gone || s.Endpoint == d
			}
			if !gone {
				kept = append(kept, s)
			}
		}
		a.cfg.Subs = kept
		a.saveConfigLocked()
		a.mu.Unlock()
	}
}

// ---------- HTTP ----------

func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /icon-192.png", serveIcon)
	mux.HandleFunc("GET /icon-512.png", serveIcon)

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		b := a.snapshotLocked()
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/events", a.sse)
	mux.HandleFunc("GET /api/vapid", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		writeJSON(w, map[string]string{"key": a.cfg.VapidPub})
	})
	mux.HandleFunc("POST /api/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var s webpush.Subscription
		if !readJSON(w, r, &s) || s.Endpoint == "" {
			return
		}
		a.mu.Lock()
		kept := []webpush.Subscription{s}
		for _, x := range a.cfg.Subs {
			if x.Endpoint != s.Endpoint {
				kept = append(kept, x)
			}
		}
		a.cfg.Subs = kept
		a.saveConfigLocked()
		a.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/test-push", func(w http.ResponseWriter, r *http.Request) {
		a.push("🔔 lich remoto", "Notificações funcionando.", "test")
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session")
		msgs, err := History(sid, a.kindOf(sid))
		if err != nil {
			writeJSON(w, map[string]any{"total": 0, "from": 0, "messages": []Msg{}, "error": err.Error()})
			return
		}
		from, err := strconv.Atoi(r.URL.Query().Get("from"))
		if err != nil || from < 0 || from > len(msgs) {
			from = max(0, len(msgs)-200) // primeira carga: as últimas 200
		}
		writeJSON(w, map[string]any{"total": len(msgs), "from": from, "messages": msgs[from:]})
	})
	mux.HandleFunc("POST /api/follow", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Session string
			On      bool
		}
		if readJSON(w, r, &in) && in.Session != "" {
			a.setFollow(in.Session, in.On)
			writeJSON(w, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("POST /api/remote", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ On bool }
		if readJSON(w, r, &in) {
			a.setRemote(in.On)
			writeJSON(w, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("POST /api/send", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Session, Prompt string }
		if !readJSON(w, r, &in) {
			return
		}
		if in.Session == "" || strings.TrimSpace(in.Prompt) == "" {
			httpErr(w, 400, "sessão e texto são obrigatórios")
			return
		}
		if err := a.send(in.Session, in.Prompt); err != nil {
			httpErr(w, 502, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		var in extension.OpenSessionParams
		if !readJSON(w, r, &in) {
			return
		}
		if in.Project == "" {
			httpErr(w, 400, "projeto é obrigatório")
			return
		}
		o, err := a.open(in)
		if err != nil {
			httpErr(w, 502, err.Error())
			return
		}
		writeJSON(w, o)
	})
	mux.HandleFunc("POST /api/close", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Session        string
			RemoveWorktree bool
		}
		if !readJSON(w, r, &in) || in.Session == "" {
			return
		}
		out, err := a.closeSession(in.Session, in.RemoveWorktree)
		if err != nil {
			httpErr(w, 502, out)
			return
		}
		writeJSON(w, map[string]string{"output": out})
	})
	mux.HandleFunc("POST /api/reply", func(w http.ResponseWriter, r *http.Request) {
		var in Reply
		if !readJSON(w, r, &in) {
			return
		}
		a.mu.Lock()
		p := a.pending[in.ID]
		a.mu.Unlock()
		if p == nil {
			httpErr(w, 404, "pergunta já respondida ou expirada")
			return
		}
		select {
		case p.reply <- in:
			writeJSON(w, map[string]bool{"ok": true})
		default:
			httpErr(w, 409, "já respondida")
		}
	})
	return mux
}

func (a *App) sse(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "sem streaming")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := make(chan []byte, 1)
	a.mu.Lock()
	a.clients[c] = struct{}{}
	c <- a.snapshotLocked()
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.clients, c)
		a.mu.Unlock()
	}()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case b := <-c:
			w.Write([]byte("data: "))
			w.Write(b)
			w.Write([]byte("\n\n"))
		case <-tick.C:
			w.Write([]byte(": ping\n\n"))
		case <-r.Context().Done():
			return
		}
		fl.Flush()
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		httpErr(w, 400, "json inválido: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// serveLoopback atende o hook (e a API, para teste local) em 127.0.0.1, exigindo o segredo
// gravado em hook.json, que só o usuário lê.
func (a *App) serveLoopback(mux *http.ServeMux) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Print("loopback: ", err)
		return
	}
	secret := newID() + newID()
	port := ln.Addr().(*net.TCPAddr).Port
	b, _ := json.Marshal(map[string]any{"port": port, "secret": secret})
	if err := os.WriteFile(filepath.Join(a.dir, "hook.json"), b, 0o600); err != nil {
		log.Print("hook.json: ", err)
	}
	inner := http.NewServeMux()
	inner.HandleFunc("POST /hook", a.hook)
	inner.Handle("/", mux)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Remoto-Secret") != secret {
			httpErr(w, 403, "segredo inválido")
			return
		}
		inner.ServeHTTP(w, r)
	})
	log.Print("loopback em 127.0.0.1:" + strconv.Itoa(port))
	log.Print(http.Serve(ln, h))
}

// ---------- acompanhar ----------

func (a *App) followed(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Follow[id]
}

func (a *App) setFollow(id string, on bool) {
	a.mu.Lock()
	if a.cfg.Follow == nil {
		a.cfg.Follow = map[string]bool{}
	}
	if on {
		a.cfg.Follow[id] = true
	} else {
		delete(a.cfg.Follow, id)
	}
	a.saveConfigLocked()
	a.mu.Unlock()
	a.changed()
}

func (a *App) kindOf(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.ID == id {
			return s.Kind
		}
	}
	return ""
}

// pushDone avisa que uma sessão acompanhada terminou, com o começo da última resposta do agente.
func (a *App) pushDone(s extension.Session) {
	body := ""
	if msgs, err := History(s.ID, s.Kind); err == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "assistant" {
				body = msgs[i].Text
				break
			}
		}
	}
	a.push("✅ "+s.Label+" terminou", body, "d-"+s.ID)
}
