package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const partSuffix = ".kopru-part"

type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Kind  string `json:"kind"`
	Thumb bool   `json:"thumb"`
}

var winReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

var imageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".heif": true, ".bmp": true, ".tif": true, ".tiff": true, ".dng": true, ".avif": true}
var videoExt = map[string]bool{".mov": true, ".mp4": true, ".m4v": true, ".avi": true, ".mkv": true, ".3gp": true, ".webm": true, ".hevc": true, ".wmv": true}
var thumbExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true}

func kindOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case imageExt[ext]:
		return "image"
	case videoExt[ext]:
		return "video"
	}
	return "file"
}

// sanitizeName, telefondan gelen adı Windows'ta güvenle kullanılabilir hale getirir.
func sanitizeName(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "_")
	}
	var b strings.Builder
	for _, r := range s {
		if r < 32 || r == 127 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	s = strings.TrimSpace(b.String())
	s = strings.TrimRight(s, ". ")
	s = strings.TrimLeft(s, ". ")
	if s == "" {
		s = "dosya"
	}
	ext := filepath.Ext(s)
	base := strings.TrimSuffix(s, ext)
	if winReserved[strings.ToUpper(base)] {
		s = "_" + s
		base = "_" + base
	}
	if len(s) > 180 {
		if len(ext) > 20 {
			ext = ""
		}
		for len(base)+len(ext) > 180 {
			_, size := utf8.DecodeLastRuneInString(base)
			base = base[:len(base)-size]
		}
		s = strings.TrimRight(base, ". ") + ext
	}
	if strings.HasSuffix(strings.ToLower(s), partSuffix) {
		s += "_"
	}
	return s
}

// validName, URL'den gelen adın klasör dışına çıkamayacağını doğrular.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00") || strings.Contains(name, ":") {
		return false
	}
	if strings.HasSuffix(strings.ToLower(name), partSuffix) {
		return false
	}
	return filepath.Base(name) == name
}

func (a *App) dirFor(key string) (string, bool) {
	switch key {
	case "in":
		return a.inDir, true
	case "out":
		return a.outDir, true
	}
	return "", false
}

func (a *App) resolve(dirKey, name string) (string, bool) {
	dir, ok := a.dirFor(dirKey)
	if !ok || !validName(name) {
		return "", false
	}
	p := filepath.Join(dir, name)
	if filepath.Dir(p) != filepath.Clean(dir) {
		return "", false
	}
	return p, true
}

func listDir(dir string) []FileInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []FileInfo{}
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		ln := strings.ToLower(n)
		if e.IsDir() || strings.HasPrefix(n, ".") || ln == "desktop.ini" || ln == "thumbs.db" || strings.HasSuffix(ln, partSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, FileInfo{
			Name:  n,
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
			Kind:  kindOf(n),
			Thumb: thumbExt[strings.ToLower(filepath.Ext(n))],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MTime != out[j].MTime {
			return out[i].MTime > out[j].MTime
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// reserve, aynı adlı dosyalar çakışmasın diye benzersiz bir hedef ad ayırır: "image (1).jpg" gibi.
func (a *App) reserve(dir, name string) (final, part string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		p := filepath.Join(dir, cand)
		key := strings.ToLower(p) // Windows büyük/küçük harf ayırmaz
		if a.reserved[key] || exists(p) || exists(p+partSuffix) {
			continue
		}
		a.reserved[key] = true
		return p, p + partSuffix
	}
}

func (a *App) release(final string) {
	a.mu.Lock()
	delete(a.reserved, strings.ToLower(final))
	a.mu.Unlock()
}

func cleanupParts(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e.Name()), partSuffix) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// counter, yükleme ilerlemesini sayan bir io.Writer sarmalayıcısıdır.
type counter struct {
	w interface{ Write([]byte) (int, error) }
	n atomic.Int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	s := fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
	return strings.Replace(s, ".", ",", 1)
}

func deviceFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "iPad / Mac"
	}
	return "Cihaz"
}

func since(t time.Time) int64 { return int64(time.Since(t) / time.Second) }
