package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFunnelPairing(t *testing.T) {
	a := &App{dir: t.TempDir()}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	h := a.funnelHandler(inner)
	do := func(method, path, body, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://lich-remoto.x.ts.net"+path, strings.NewReader(body))
		r.RemoteAddr = "203.0.113.9:5555"
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// Sem aparelho: página vai para o pareamento, API recusa.
	if w := do("GET", "/", "", ""); w.Code != 303 || w.Header().Get("Location") != "/pair" {
		t.Fatalf("GET / sem cookie: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := do("GET", "/api/state", "", ""); w.Code != 401 {
		t.Fatalf("API sem cookie: %d", w.Code)
	}
	if w := do("GET", "/api/state", "", strings.Repeat("a", 64)); w.Code != 401 {
		t.Fatalf("cookie inventado: %d", w.Code)
	}
	// Sem código gerado nada pareia.
	if w := do("POST", "/pair", "c=ABCDEFGH&n=x", ""); w.Code != 401 {
		t.Fatalf("sem código: %d", w.Code)
	}

	a.newPairing("lich-remoto.x.ts.net")
	code := a.fun.code
	if len(code) != 8 || a.fun.qr == nil {
		t.Fatalf("código %q qr %d", code, len(a.fun.qr))
	}
	w := do("POST", "/pair", url.Values{"c": {strings.ToLower(code)}, "n": {"Meu\ncelular"}}.Encode(), "")
	if w.Code != 303 {
		t.Fatalf("pareamento: %d %s", w.Code, w.Body)
	}
	var token string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.HttpOnly && c.Secure && c.SameSite == http.SameSiteStrictMode {
			token = c.Value
		}
	}
	if token == "" || len(a.cfg.Devices) != 1 || a.cfg.Devices[0].Name != "Meucelular" || a.cfg.Devices[0].Hash == token {
		t.Fatalf("aparelho: %+v token %q", a.cfg.Devices, token)
	}
	// Código é de uso único.
	if w := do("POST", "/pair", "c="+code, ""); w.Code != 401 {
		t.Fatalf("código reutilizado: %d", w.Code)
	}
	// Com o cookie passa; POST sem X-Remoto não.
	if w := do("GET", "/api/state", "", token); w.Code != 200 {
		t.Fatalf("com cookie: %d", w.Code)
	}
	if w := do("POST", "/api/send", "", token); w.Code != 403 {
		t.Fatalf("POST sem X-Remoto: %d", w.Code)
	}
	// Revogado volta a ser recusado.
	a.revoke(a.cfg.Devices[0].ID)
	if w := do("GET", "/api/state", "", token); w.Code != 401 {
		t.Fatalf("revogado: %d", w.Code)
	}
}

func TestFunnelRateLimit(t *testing.T) {
	a := &App{dir: t.TempDir()}
	h := a.funnelHandler(http.NotFoundHandler())
	try := func(ip string) int {
		r := httptest.NewRequest("POST", "https://h/pair", strings.NewReader("c=ZZZZZZZZ"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = ip + ":1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < failsPerIP; i++ {
		try("198.51.100.1")
	}
	if c := try("198.51.100.1"); c != 429 {
		t.Fatalf("IP depois de %d falhas: %d", failsPerIP, c)
	}
	// O código certo também é bloqueado nesse IP (não dá para seguir chutando).
	a.newPairing("h")
	if c := try("198.51.100.1"); c != 429 {
		t.Fatalf("bloqueio não vale: %d", c)
	}
	if c := try("198.51.100.2"); c != 401 {
		t.Fatalf("outro IP: %d", c)
	}
}
