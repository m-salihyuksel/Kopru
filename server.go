package main

import (
	"archive/zip"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web
var webFS embed.FS

var contentTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".heic": "image/heic", ".heif": "image/heif", ".avif": "image/avif",
	".mov": "video/quicktime", ".mp4": "video/mp4", ".m4v": "video/x-m4v", ".3gp": "video/3gpp",
	".webm": "video/webm", ".pdf": "application/pdf", ".txt": "text/plain; charset=utf-8",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/wav", ".zip": "application/zip",
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"app": appID, "version": appVersion})
	})
	mux.HandleFunc("GET /icon.png", a.static("web/icon.png", "image/png"))
	mux.HandleFunc("GET /favicon.ico", a.static("web/icon.png", "image/png"))
	mux.HandleFunc("GET /app.css", a.static("web/app.css", "text/css; charset=utf-8"))

	mux.HandleFunc("GET /{$}", a.auth(a.page("web/phone.html")))
	mux.HandleFunc("GET /pc", a.auth(a.localOnly(a.page("web/pc.html"))))

	mux.HandleFunc("GET /api/state", a.auth(a.localOnly(a.handleState)))
	mux.HandleFunc("GET /api/files", a.auth(a.handleFiles))
	mux.HandleFunc("POST /api/upload", a.auth(a.handleUpload))
	mux.HandleFunc("POST /api/delete", a.auth(a.localOnly(a.handleDelete)))
	mux.HandleFunc("POST /api/reveal", a.auth(a.localOnly(a.handleReveal)))
	mux.HandleFunc("GET /qr.png", a.auth(a.localOnly(a.handleQR)))

	mux.HandleFunc("GET /f/{dir}/{name}", a.auth(a.handleFile))
	mux.HandleFunc("GET /t/{dir}/{name}", a.auth(a.handleThumb))
	mux.HandleFunc("GET /zip/{dir}", a.auth(a.handleZip))

	return mux
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) isLocal(r *http.Request) bool { return a.isLocalIP(remoteIP(r)) }

// auth: QR koddaki ?t= anahtarı ya da daha önce verilen çerez olmadan hiçbir şey açılmaz.
func (a *App) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r)
		if a.blocked(ip) {
			http.Error(w, "Çok fazla hatalı deneme. 10 dakika sonra tekrar deneyin.", http.StatusTooManyRequests)
			return
		}
		ok := false
		if t := r.URL.Query().Get("t"); t != "" {
			ok = subtle.ConstantTimeCompare([]byte(t), []byte(a.token)) == 1
			if ok {
				http.SetCookie(w, &http.Cookie{
					Name: "kopru", Value: a.token, Path: "/",
					MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
				})
			}
		} else if c, err := r.Cookie("kopru"); err == nil {
			ok = subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.token)) == 1
		}
		if !ok {
			a.fail(ip)
			a.denied(w, r)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("X-Kopru") != "1" {
			http.Error(w, "Geçersiz istek", http.StatusForbidden)
			return
		}
		if !a.isLocalIP(ip) {
			a.touchClient(ip, r.UserAgent())
		}
		h(w, r)
	}
}

func (a *App) localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isLocal(r) {
			http.Error(w, "Bu sayfa yalnızca PC'den açılabilir.", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (a *App) blocked(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[ip]
	return f != nil && time.Now().Before(f.until)
}

func (a *App) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[ip]
	if f == nil || time.Since(f.first) > 10*time.Minute {
		f = &failure{first: time.Now()}
		a.failures[ip] = f
	}
	f.count++
	if f.count >= 30 {
		f.until = time.Now().Add(10 * time.Minute)
	}
}

func (a *App) touchClient(ip, ua string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.clients[ip]
	if c == nil {
		c = &Client{IP: ip}
		a.clients[ip] = c
		a.logf("Bağlandı: %s (%s)", deviceFromUA(ua), ip)
	}
	c.Device = deviceFromUA(ua)
	c.LastSeen = time.Now()
}

func (a *App) denied(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.Method != http.MethodGet {
		http.Error(w, "Yetkisiz. PC ekranındaki QR kodu yeniden okutun.", http.StatusUnauthorized)
		return
	}
	b, _ := webFS.ReadFile("web/denied.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write(b)
}

func (a *App) static(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(b)
	}
}

func (a *App) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s := strings.ReplaceAll(string(b), "{{HOST}}", html.EscapeString(a.hostname))
		s = strings.ReplaceAll(s, "{{VERSION}}", appVersion)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		io.WriteString(w, s)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	ups := make([]Upload, 0, len(a.uploads))
	for _, u := range a.uploads {
		c := *u
		c.Done = u.counter.n.Load()
		c.counter = nil
		ups = append(ups, c)
	}
	type clientOut struct {
		IP     string `json:"ip"`
		Device string `json:"device"`
		Ago    int64  `json:"ago"`
	}
	cls := []clientOut{}
	for _, c := range a.clients {
		if time.Since(c.LastSeen) < 10*time.Minute {
			cls = append(cls, clientOut{c.IP, c.Device, since(c.LastSeen)})
		}
	}
	a.mu.Unlock()
	sort.Slice(ups, func(i, j int) bool { return ups[i].ID < ups[j].ID })
	sort.Slice(cls, func(i, j int) bool { return cls[i].Ago < cls[j].Ago })

	writeJSON(w, map[string]any{
		"host":    a.hostname,
		"port":    a.port,
		"addrs":   lanAddrs(a.port, a.token),
		"in":      listDir(a.inDir),
		"out":     listDir(a.outDir),
		"inDir":   a.inDir,
		"outDir":  a.outDir,
		"uploads": ups,
		"clients": cls,
		"sending": a.sending.Load(),
	})
}

func (a *App) handleFiles(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("dir")
	if key == "" {
		key = "out"
	}
	dir, ok := a.dirFor(key)
	if !ok {
		jsonError(w, http.StatusBadRequest, "Geçersiz klasör")
		return
	}
	writeJSON(w, map[string]any{"host": a.hostname, "files": listDir(dir)})
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dest := q.Get("dest")
	if dest == "" {
		dest = "in"
	}
	if dest == "out" && !a.isLocal(r) {
		jsonError(w, http.StatusForbidden, "Bu klasöre yalnızca PC'den dosya eklenebilir.")
		return
	}
	dir, ok := a.dirFor(dest)
	if !ok {
		jsonError(w, http.StatusBadRequest, "Geçersiz klasör")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonError(w, http.StatusInternalServerError, "Klasör oluşturulamadı: "+err.Error())
		return
	}
	name := sanitizeName(q.Get("name"))
	final, part := a.reserve(dir, name)
	defer a.release(final)

	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Dosya oluşturulamadı: "+err.Error())
		return
	}
	cnt := &counter{w: f}
	who := "PC"
	if !a.isLocal(r) {
		who = deviceFromUA(r.UserAgent())
	}
	a.mu.Lock()
	a.nextID++
	up := &Upload{ID: a.nextID, Name: filepath.Base(final), Dest: dest, Total: r.ContentLength, Client: who, Started: time.Now(), counter: cnt}
	a.uploads[up.ID] = up
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.uploads, up.ID)
		a.mu.Unlock()
	}()

	buf := make([]byte, 1<<20)
	n, err := io.CopyBuffer(cnt, r.Body, buf)
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && r.ContentLength >= 0 && n != r.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		os.Remove(part)
		msg := "Aktarım yarıda kaldı."
		if isDiskFull(err) {
			msg = "PC'de yeterli boş alan yok."
		}
		a.logf("! Yarıda kaldı: %s (%v)", up.Name, err)
		jsonError(w, http.StatusBadRequest, msg)
		return
	}
	if ms, perr := strconv.ParseInt(q.Get("mtime"), 10, 64); perr == nil && ms > 0 {
		t := time.UnixMilli(ms)
		if t.Before(time.Now().Add(24*time.Hour)) && t.Year() > 1990 {
			os.Chtimes(part, time.Now(), t)
		}
	}
	// Bu arada Gezgin'den aynı adla dosya konduysa ezmeyelim.
	if exists(final) {
		a.release(final)
		final, _ = a.reserve(dir, name)
		defer a.release(final)
	}
	// Virüs tarayıcısı yeni dosyayı kısa süre kilitleyebilir; birkaç kez dene.
	err = os.Rename(part, final)
	for i := 0; err != nil && i < 20; i++ {
		time.Sleep(150 * time.Millisecond)
		err = os.Rename(part, final)
	}
	if err != nil {
		os.Remove(part)
		jsonError(w, http.StatusInternalServerError, "Dosya kaydedilemedi: "+err.Error())
		return
	}
	if dest == "in" {
		a.logf("← Alındı: %s (%s)", filepath.Base(final), humanSize(n))
	} else {
		a.logf("+ Gönderilecekler'e eklendi: %s (%s)", filepath.Base(final), humanSize(n))
	}
	writeJSON(w, map[string]any{"name": filepath.Base(final), "size": n})
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.resolve(r.URL.Query().Get("dir"), r.URL.Query().Get("name"))
	if !ok {
		jsonError(w, http.StatusBadRequest, "Geçersiz dosya")
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		jsonError(w, http.StatusInternalServerError, "Silinemedi: "+err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleReveal(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("dir")
	dir, ok := a.dirFor(key)
	if !ok {
		jsonError(w, http.StatusBadRequest, "Geçersiz klasör")
		return
	}
	file := ""
	if n := r.URL.Query().Get("name"); n != "" {
		p, ok := a.resolve(key, n)
		if !ok {
			jsonError(w, http.StatusBadRequest, "Geçersiz dosya")
			return
		}
		file = p
	}
	if err := revealInExplorer(dir, file); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleQR(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	if !strings.HasPrefix(u, "http://") || len(u) > 200 {
		http.Error(w, "Geçersiz adres", http.StatusBadRequest)
		return
	}
	q, err := qrcode.New(u, qrcode.Medium)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q.DisableBorder = true
	png, err := q.PNG(-8)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func contentDisposition(kind, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s", kind, ascii, url.PathEscape(name))
}

func (a *App) handleFile(w http.ResponseWriter, r *http.Request) {
	dirKey, name := r.PathValue("dir"), r.PathValue("name")
	p, ok := a.resolve(dirKey, name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if ct, ok := contentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		w.Header().Set("Content-Type", ct)
	}
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", contentDisposition("attachment", name))
		rg := r.Header.Get("Range")
		if !a.isLocal(r) {
			if rg == "" || strings.HasPrefix(rg, "bytes=0-") {
				a.logf("→ Gönderiliyor: %s (%s)", name, humanSize(st.Size()))
			}
			a.sending.Add(1)
			defer a.sending.Add(-1)
		}
	} else {
		w.Header().Set("Content-Disposition", contentDisposition("inline", name))
	}
	w.Header().Set("Cache-Control", "private, max-age=0")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (a *App) handleThumb(w http.ResponseWriter, r *http.Request) {
	dirKey, name := r.PathValue("dir"), r.PathValue("name")
	p, ok := a.resolve(dirKey, name)
	if !ok || !thumbExt[strings.ToLower(filepath.Ext(name))] {
		http.NotFound(w, r)
		return
	}
	st, err := os.Stat(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	key := fmt.Sprintf("%s|%d|%d", p, st.Size(), st.ModTime().UnixNano())
	b, ok := a.thumbs.get(key)
	if !ok {
		a.thumbs.sem <- struct{}{}
		b, ok = a.thumbs.get(key)
		if !ok {
			b, err = makeThumb(p)
			if err == nil {
				a.thumbs.put(key, b)
			}
		}
		<-a.thumbs.sem
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(b)
}

// handleZip, seçilen (ya da tüm) dosyaları sıkıştırmadan tek bir ZIP olarak akıtır.
func (a *App) handleZip(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("dir")
	dir, ok := a.dirFor(key)
	if !ok {
		http.NotFound(w, r)
		return
	}
	want := map[string]bool{}
	for _, n := range r.URL.Query()["n"] {
		want[n] = true
	}
	files := listDir(dir)
	sel := files[:0:0]
	for _, f := range files {
		if len(want) == 0 || want[f.Name] {
			sel = append(sel, f)
		}
	}
	if len(sel) == 0 {
		http.Error(w, "İndirilecek dosya yok.", http.StatusNotFound)
		return
	}
	zipName := "Kopru-" + time.Now().Format("2006-01-02_15-04") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", zipName))
	w.Header().Set("Cache-Control", "no-store")
	if !a.isLocal(r) {
		a.logf("→ ZIP gönderiliyor: %d dosya", len(sel))
		a.sending.Add(1)
		defer a.sending.Add(-1)
	}
	zw := zip.NewWriter(w)
	buf := make([]byte, 1<<20)
	for _, f := range sel {
		p := filepath.Join(dir, f.Name)
		src, err := os.Open(p)
		if err != nil {
			continue
		}
		hdr := &zip.FileHeader{Name: f.Name, Method: zip.Store, Modified: time.UnixMilli(f.MTime)}
		dst, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.CopyBuffer(dst, src, buf)
		}
		src.Close()
		if err != nil {
			return // bağlantı koptu
		}
	}
	zw.Close()
}
