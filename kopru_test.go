package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	phoneIP = "10.77.0.2:51000" // bu bilgisayarın arayüzlerinde olmayan, "telefon" adresi
	pcIP    = "127.0.0.1:52000"
	iphone  = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1"
)

func newTestApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{Token: "eskianahtar"}
	a := newApp(cfg, filepath.Join(dir, "cfg", "config.json"), filepath.Join(dir, "data"))
	a.port = 8765
	for _, d := range []string{a.inDir, a.outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for range a.logCh {
		}
	}()
	return a, a.routes()
}

type req struct {
	method, url, from, cookie string
	body                      io.Reader
	post                      bool
}

func do(h http.Handler, r req) *httptest.ResponseRecorder {
	if r.method == "" {
		r.method = http.MethodGet
	}
	hr := httptest.NewRequest(r.method, r.url, r.body)
	hr.RemoteAddr = r.from
	if r.from == phoneIP {
		hr.Header.Set("User-Agent", iphone)
	}
	if r.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: "kopru", Value: r.cookie})
	}
	if r.method == http.MethodPost {
		hr.Header.Set("X-Kopru", "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	return rec
}

func cookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kopru" {
			return c
		}
	}
	return nil
}

func TestPCDisconnectRevokesPhone(t *testing.T) {
	a, h := newTestApp(t)

	// Telefon QR koddaki adresi açar ve çerez alır.
	rec := do(h, req{url: "/?t=eskianahtar", from: phoneIP})
	if rec.Code != 200 || cookieOf(rec) == nil {
		t.Fatalf("telefon bağlanamadı: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Bağlantıyı kes") || !strings.Contains(rec.Body.String(), "<b>MSY</b>") {
		t.Fatal("telefon sayfasında düğme ya da imza yok")
	}
	if n := len(a.clients); n != 1 {
		t.Fatalf("bağlı cihaz sayısı %d, beklenen 1", n)
	}

	// PC panelinden "Bağlantıyı kes".
	rec = do(h, req{method: "POST", url: "/api/disconnect-all", from: pcIP, cookie: "eskianahtar"})
	if rec.Code != 200 {
		t.Fatalf("disconnect-all: %d %s", rec.Code, rec.Body)
	}
	var d struct {
		Token  string `json:"token"`
		Kicked int    `json:"kicked"`
	}
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Token == "" || d.Token == "eskianahtar" || d.Kicked != 1 {
		t.Fatalf("beklenmeyen yanıt: %+v", d)
	}
	if c := cookieOf(rec); c == nil || c.Value != d.Token {
		t.Fatal("PC paneline yeni çerez verilmedi")
	}

	// Telefonun eski çerezi artık geçmez; API "revoked" der, sayfa açıklama gösterir.
	rec = do(h, req{url: "/api/files?dir=out", from: phoneIP, cookie: "eskianahtar"})
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"code":"revoked"`) {
		t.Fatalf("eski çerez reddedilmedi: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, req{url: "/", from: phoneIP, cookie: "eskianahtar"})
	body := rec.Body.String()
	if rec.Code != 401 || !strings.Contains(body, "Bağlantı bilgisayardan kesildi") || !strings.Contains(body, "<b>MSY</b>") {
		t.Fatalf("kesildi sayfası yanlış: %d", rec.Code)
	}
	if c := cookieOf(rec); c == nil || c.MaxAge >= 0 {
		t.Fatal("telefondaki eski çerez silinmedi")
	}
	// Eski anahtarlı denemeler kaba kuvvet sayacına eklenmez.
	if f := a.failures["10.77.0.2"]; f != nil {
		t.Fatalf("eski anahtar hatalı deneme sayıldı: %d", f.count)
	}

	// PC sayfası eski adresle yenilense de yeni çerezle açılır.
	rec = do(h, req{url: "/pc?t=eskianahtar", from: pcIP, cookie: d.Token})
	if rec.Code != 200 {
		t.Fatalf("PC paneli yenilenince açılmadı: %d", rec.Code)
	}
	// Çerez yoksa eski panel adresi yönlendirme metni gösterir.
	rec = do(h, req{url: "/pc?t=eskianahtar", from: pcIP})
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "Bu panel adresi eski") {
		t.Fatalf("eski panel adresi metni yok: %d", rec.Code)
	}

	// Yeni QR kodla telefon yeniden bağlanır.
	rec = do(h, req{url: "/?t=" + d.Token, from: phoneIP})
	if rec.Code != 200 {
		t.Fatalf("yeni anahtarla bağlanılamadı: %d", rec.Code)
	}

	// Yeni anahtar ve iptal listesi kalıcı.
	cfg := loadConfig(a.cfgPath)
	if cfg.Token != d.Token || len(cfg.Revoked) != 1 || cfg.Revoked[0] != "eskianahtar" {
		t.Fatalf("ayar dosyası güncellenmedi: %+v", cfg)
	}
	b := newApp(cfg, a.cfgPath, a.baseDir)
	if !b.isRevoked("eskianahtar") {
		t.Fatal("yeniden başlatınca eski anahtar iptal listesinde değil")
	}
}

func TestPhoneDisconnectsItself(t *testing.T) {
	a, h := newTestApp(t)
	do(h, req{url: "/?t=eskianahtar", from: phoneIP})

	rec := do(h, req{method: "POST", url: "/api/disconnect", from: phoneIP, cookie: "eskianahtar"})
	if rec.Code != 200 {
		t.Fatalf("disconnect: %d", rec.Code)
	}
	if c := cookieOf(rec); c == nil || c.MaxAge >= 0 {
		t.Fatal("telefon çerezi silinmedi")
	}
	if len(a.clients) != 0 {
		t.Fatal("telefon bağlı listesinden çıkmadı")
	}

	// PC paneli bildirimi görür.
	rec = do(h, req{url: "/api/state", from: pcIP, cookie: "eskianahtar"})
	var s struct {
		Notice struct {
			ID   int64  `json:"id"`
			Text string `json:"text"`
		} `json:"notice"`
		Clients []any `json:"clients"`
	}
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Notice.ID != 1 || s.Notice.Text != "iPhone bağlantıyı kesti" || len(s.Clients) != 0 {
		t.Fatalf("PC bildirimi yanlış: %+v", s)
	}

	// Anahtar değişmez: QR kod tekrar okutulunca bağlanır.
	if rec := do(h, req{url: "/?t=eskianahtar", from: phoneIP}); rec.Code != 200 {
		t.Fatalf("yeniden bağlanılamadı: %d", rec.Code)
	}

	rec = do(h, req{url: "/bye", from: phoneIP})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Bağlantı kesildi") {
		t.Fatalf("/bye sayfası yanlış: %d", rec.Code)
	}
}

func TestPCDisconnectStopsPhoneUpload(t *testing.T) {
	a, h := newTestApp(t)
	pr, pw := io.Pipe()
	const total = 8 << 20
	hr := httptest.NewRequest("POST", "/api/upload?name=video.mov", pr)
	hr.RemoteAddr = phoneIP
	hr.ContentLength = total
	hr.Header.Set("X-Kopru", "1")
	hr.Header.Set("User-Agent", iphone)
	hr.AddCookie(&http.Cookie{Name: "kopru", Value: "eskianahtar"})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, hr); close(done) }()

	chunk := bytes.Repeat([]byte{1}, 256<<10)
	for i := 0; i < 4; i++ { // 1 MB gönder
		pw.Write(chunk)
	}
	if !waitFor(func() bool { return countUploads(a) == 1 }) {
		t.Fatal("yükleme başlamadı")
	}
	a.rotate()
	go func() { // telefon göndermeye devam etse de sunucu bırakmalı
		for i := 0; i < 28; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		pw.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("yükleme durmadı")
	}
	pr.CloseWithError(io.ErrClosedPipe)
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "revoked") {
		t.Fatalf("beklenen 410, gelen %d %s", rec.Code, rec.Body)
	}
	if entries, _ := os.ReadDir(a.inDir); len(entries) != 0 {
		t.Fatalf("yarım dosya kaldı: %v", entries)
	}
}

func TestPCDisconnectStopsPhoneDownload(t *testing.T) {
	a, h := newTestApp(t)
	big := bytes.Repeat([]byte("k"), 16<<20)
	os.WriteFile(filepath.Join(a.outDir, "film.mp4"), big, 0o644)

	hr := httptest.NewRequest("GET", "/f/out/film.mp4?dl=1", nil)
	hr.RemoteAddr = phoneIP
	hr.AddCookie(&http.Cookie{Name: "kopru", Value: "eskianahtar"})
	w := &kickWriter{ResponseRecorder: httptest.NewRecorder(), kick: func() { a.rotate() }}
	h.ServeHTTP(w, hr)
	if got := w.Body.Len(); got >= len(big) {
		t.Fatalf("indirme durmadı: %d / %d bayt", got, len(big))
	}

	// PC'nin kendi indirmesi etkilenmez.
	w2 := &kickWriter{ResponseRecorder: httptest.NewRecorder(), kick: func() { a.rotate() }}
	hr2 := httptest.NewRequest("GET", "/f/out/film.mp4?dl=1", nil)
	hr2.RemoteAddr = pcIP
	hr2.AddCookie(&http.Cookie{Name: "kopru", Value: a.curToken()})
	h.ServeHTTP(w2, hr2)
	if w2.Body.Len() != len(big) {
		t.Fatalf("PC indirmesi yarıda kaldı: %d", w2.Body.Len())
	}
}

func TestPhoneCannotUsePCOnlyEndpoints(t *testing.T) {
	_, h := newTestApp(t)
	for _, u := range []string{"/api/disconnect-all", "/api/delete?dir=in&name=x", "/api/reveal?dir=in"} {
		if rec := do(h, req{method: "POST", url: u, from: phoneIP, cookie: "eskianahtar"}); rec.Code != 403 {
			t.Errorf("%s telefondan çağrılabildi: %d", u, rec.Code)
		}
	}
	if rec := do(h, req{url: "/pc", from: phoneIP, cookie: "eskianahtar"}); rec.Code != 403 {
		t.Errorf("PC paneli telefondan açıldı: %d", rec.Code)
	}
}

// kickWriter, ilk veri parçası yazıldıktan sonra PC'den "Bağlantıyı kes" denmiş gibi davranır.
type kickWriter struct {
	*httptest.ResponseRecorder
	kick   func()
	kicked bool
}

func (k *kickWriter) Write(p []byte) (int, error) {
	n, err := k.ResponseRecorder.Write(p)
	if !k.kicked {
		k.kicked = true
		k.kick()
	}
	return n, err
}

func countUploads(a *App) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.uploads)
}

func waitFor(f func() bool) bool {
	for i := 0; i < 200; i++ {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
