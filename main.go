package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	appName     = "Köprü"
	appID       = "kopru"
	appVersion  = "1.0.0"
	defaultPort = 8765
	inFolder    = "iPhone'dan Gelenler"
	outFolder   = "iPhone'a Gönderilecekler"
)

type Config struct {
	Token string `json:"token"`
	Dir   string `json:"dir,omitempty"`
}

type App struct {
	token    string
	port     int
	baseDir  string
	inDir    string
	outDir   string
	hostname string

	mu       sync.Mutex
	reserved map[string]bool
	uploads  map[int64]*Upload
	nextID   int64
	clients  map[string]*Client
	failures map[string]*failure

	localMu      sync.Mutex
	localIPs     map[string]bool
	localIPsTime time.Time

	thumbs  *thumbCache
	logCh   chan string
	sending atomic.Int32 // telefona o an giden indirme sayısı
}

type Upload struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Dest    string    `json:"dest"`
	Total   int64     `json:"total"`
	Done    int64     `json:"done"`
	Client  string    `json:"client"`
	Started time.Time `json:"-"`
	counter *counter
}

type Client struct {
	IP       string    `json:"ip"`
	Device   string    `json:"device"`
	LastSeen time.Time `json:"-"`
}

type failure struct {
	count int
	first time.Time
	until time.Time
}

func main() {
	portFlag := flag.Int("port", 0, "Sunucu portu (varsayılan 8765)")
	dirFlag := flag.String("dir", "", "Aktarım klasörü (varsayılan: kullanıcı klasörü\\Köprü)")
	noBrowser := flag.Bool("no-browser", false, "Başlangıçta tarayıcıyı açma")
	flag.Parse()

	platformInit(appName + " – iPhone ↔ PC aktarım")

	cfgPath := configPath()
	cfg := loadConfig(cfgPath)
	if cfg.Token == "" {
		cfg.Token = newToken(8)
		saveConfig(cfgPath, cfg)
	}

	base := *dirFlag
	if base == "" {
		base = cfg.Dir
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, "Köprü")
	}
	base, _ = filepath.Abs(base)

	app := &App{
		token:    cfg.Token,
		baseDir:  base,
		inDir:    filepath.Join(base, inFolder),
		outDir:   filepath.Join(base, outFolder),
		reserved: map[string]bool{},
		uploads:  map[int64]*Upload{},
		clients:  map[string]*Client{},
		failures: map[string]*failure{},
		thumbs:   newThumbCache(64 << 20),
		logCh:    make(chan string, 512),
	}
	app.hostname, _ = os.Hostname()
	if app.hostname == "" {
		app.hostname = "Bu PC"
	}

	for _, d := range []string{app.inDir, app.outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fatal("Klasör oluşturulamadı: %s\n%v", d, err)
		}
		cleanupParts(d)
	}

	port := *portFlag
	if port == 0 {
		port = defaultPort
	}

	// Zaten açık bir Köprü varsa onun panelini aç ve çık.
	if *portFlag == 0 && isRunning(port) {
		fmt.Println("Köprü zaten çalışıyor; panel tarayıcıda açılıyor.")
		openBrowser(fmt.Sprintf("http://127.0.0.1:%d/pc?t=%s", port, app.token))
		time.Sleep(2 * time.Second)
		return
	}

	ln, err := listenFrom(port, *portFlag != 0)
	if err != nil {
		fatal("Sunucu başlatılamadı: %v", err)
	}
	app.port = ln.Addr().(*net.TCPAddr).Port

	go app.logLoop()
	app.printBanner()

	srv := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if !*noBrowser {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser(fmt.Sprintf("http://127.0.0.1:%d/pc?t=%s", app.port, app.token))
		}()
	}

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("Sunucu durdu: %v", err)
	}
}

func listenFrom(port int, exact bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err == nil || exact {
		return ln, err
	}
	for p := port + 1; p <= port+15; p++ {
		if ln, err = net.Listen("tcp", ":"+strconv.Itoa(p)); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", ":0")
}

func isRunning(port int) bool {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		App string `json:"app"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	return v.App == appID
}

func (a *App) printBanner() {
	line := strings.Repeat("─", 58)
	fmt.Println(line)
	fmt.Printf("  %s %s  –  iPhone ↔ PC aktarım\n", appName, appVersion)
	fmt.Println(line)
	fmt.Println()
	fmt.Println("  Panel tarayıcıda açıldı. Açılmadıysa şu adrese gidin:")
	fmt.Printf("    http://127.0.0.1:%d/pc?t=%s\n\n", a.port, a.token)
	addrs := lanAddrs(a.port, a.token)
	if len(addrs) == 0 {
		fmt.Println("  ! Ağ bağlantısı bulunamadı. PC ile iPhone aynı Wi-Fi'da olmalı.")
	} else {
		fmt.Println("  iPhone'da Safari'ye yazılacak adres:")
		for i, ad := range addrs {
			mark := " "
			if i == 0 {
				mark = "•"
			}
			fmt.Printf("   %s %s   (%s)\n", mark, ad.URL, ad.Iface)
		}
	}
	fmt.Println()
	fmt.Println("  Gelenler:      " + a.inDir)
	fmt.Println("  Gönderilecek:  " + a.outDir)
	fmt.Println()
	fmt.Println("  Windows güvenlik duvarı soru sorarsa \"Erişime izin ver\" deyin.")
	fmt.Println("  Kapatmak için bu pencereyi kapatın.")
	fmt.Println(line)
}

// logf, istek işleyicilerini asla bekletmeyen kayıt yazar.
func (a *App) logf(format string, args ...any) {
	msg := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
	select {
	case a.logCh <- msg:
	default:
	}
}

func (a *App) logLoop() {
	for m := range a.logCh {
		fmt.Println(m)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\nHata: "+format+"\n", args...)
	waitBeforeExit()
	os.Exit(1)
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "Kopru", "config.json")
}

func loadConfig(p string) Config {
	var c Config
	b, err := os.ReadFile(p)
	if err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(p string, c Config) {
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(p, b, 0o600)
}

// Karıştırılması kolay karakterler (0/o, 1/l/i) çıkarıldı.
const tokenAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func newToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = tokenAlphabet[int(b[i])%len(tokenAlphabet)]
	}
	return string(b)
}
