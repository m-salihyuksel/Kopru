package main

import (
	"errors"
	"html"
	"io"
	"net/http"
	"strings"
)

// errStopped, PC'den "Bağlantıyı kes" denince süren aktarımları durdurmak için kullanılır.
var errStopped = errors.New("bağlantı bilgisayardan kesildi")

// curToken, o anki erişim anahtarını verir. PC'den "Bağlantıyı kes" denince değişir.
func (a *App) curToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

func (a *App) isRevoked(t string) bool {
	if t == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.revoked[t]
}

// rotate, erişim anahtarını yeniler: eski anahtarla bağlı tüm telefonlar düşer,
// telefonla süren aktarımlar durur ve yeni bir QR kod oluşur.
func (a *App) rotate() (string, int) {
	tok := newToken(8)
	a.mu.Lock()
	old := a.token
	a.token = tok
	a.revoked[old] = true
	n := len(a.clients)
	a.clients = map[string]*Client{}
	a.cfg.Token = tok
	a.cfg.Revoked = append(a.cfg.Revoked, old)
	if len(a.cfg.Revoked) > 10 {
		a.cfg.Revoked = a.cfg.Revoked[len(a.cfg.Revoked)-10:]
	}
	cfg := a.cfg
	cfg.Revoked = append([]string(nil), a.cfg.Revoked...)
	a.mu.Unlock()
	a.gen.Add(1)
	saveConfig(a.cfgPath, cfg)
	return tok, n
}

// notify, PC panelinde kısa bir bildirim olarak gösterilecek olayı kaydeder.
func (a *App) notify(text string) {
	a.mu.Lock()
	a.noticeID++
	a.noticeText = text
	a.mu.Unlock()
}

func setAuthCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: "kopru", Value: tok, Path: "/",
		MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func clearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: "kopru", Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// handleDisconnect: telefon kendi bağlantısını keser. Anahtar değişmez;
// QR kodu yeniden okutunca tekrar bağlanır.
func (a *App) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	clearAuthCookie(w)
	ip := remoteIP(r)
	if !a.isLocalIP(ip) {
		dev := deviceFromUA(r.UserAgent())
		a.mu.Lock()
		delete(a.clients, ip)
		a.mu.Unlock()
		a.logf("× %s bağlantıyı kesti (%s)", dev, ip)
		a.notify(dev + " bağlantıyı kesti")
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleDisconnectAll: PC'den bağlı telefonların hepsini düşürür ve yeni QR kod üretir.
func (a *App) handleDisconnectAll(w http.ResponseWriter, r *http.Request) {
	tok, n := a.rotate()
	setAuthCookie(w, tok)
	if n > 0 {
		a.logf("× Bağlantı kesildi (%d cihaz). Yeni QR kod hazır.", n)
	} else {
		a.logf("× Bağlantı anahtarı yenilendi. Yeni QR kod hazır.")
	}
	writeJSON(w, map[string]any{"token": tok, "kicked": n})
}

// stopFunc, telefona/telefondan süren bir aktarımın PC'den kesilip kesilmediğini söyler.
// PC'nin kendi istekleri hiç durdurulmaz.
func (a *App) stopFunc(r *http.Request) func() bool {
	if a.isLocal(r) {
		return nil
	}
	gen := a.gen.Load()
	return func() bool { return a.gen.Load() != gen }
}

type stopReader struct {
	r    io.ReadSeeker
	stop func() bool
}

func (s *stopReader) Read(p []byte) (int, error) {
	if s.stop() {
		return 0, errStopped
	}
	return s.r.Read(p)
}

func (s *stopReader) Seek(off int64, whence int) (int64, error) { return s.r.Seek(off, whence) }

var infoPages = map[string][2]string{
	"invalid": {"Kodu yeniden okutun",
		"Bu bağlantının süresi dolmuş ya da eksik. Bilgisayardaki Köprü penceresinde görünen QR kodu iPhone kamerasıyla okutun."},
	"local": {"Bu panel adresi eski",
		"Kopru.exe'yi yeniden çalıştırın; panel doğru adresle açılır. Köprü zaten açıksa yalnızca paneli yeniden açar."},
	"revoked": {"Bağlantı bilgisayardan kesildi",
		"Yeniden bağlanmak için bilgisayardaki Köprü penceresinde görünen yeni QR kodu iPhone kamerasıyla okutun."},
	"bye": {"Bağlantı kesildi",
		"Bu iPhone artık bilgisayara bağlı değil. Yeniden bağlanmak için bilgisayardaki Köprü penceresinde görünen QR kodu kamerayla okutun."},
}

func (a *App) infoPage(w http.ResponseWriter, kind string, code int) {
	p, ok := infoPages[kind]
	if !ok {
		p = infoPages["invalid"]
	}
	b, _ := webFS.ReadFile("web/denied.html")
	s := strings.NewReplacer(
		"{{TITLE}}", html.EscapeString(p[0]),
		"{{TEXT}}", html.EscapeString(p[1]),
		"{{VERSION}}", appVersion,
	).Replace(string(b))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(code)
	io.WriteString(w, s)
}
