package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

type Addr struct {
	IP    string `json:"ip"`
	Iface string `json:"iface"`
	URL   string `json:"url"`
	score int
}

// lanAddrs, iPhone'un ulaşabileceği yerel ağ adreslerini en olasıdan başlayarak sıralar.
func lanAddrs(port int, token string) []Addr {
	primary := primaryIP()
	ifaces, _ := net.Interfaces()
	var out []Addr
	seen := map[string]bool{}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			s := 0
			name := strings.ToLower(ifc.Name)
			switch {
			case containsAny(name, "wi-fi", "wifi", "wlan", "wireless", "kablosuz"):
				s += 3
			case containsAny(name, "ethernet", "eth", "en0", "en1"):
				s += 2
			}
			if containsAny(name, "vethernet", "virtual", "vmware", "vbox", "virtualbox", "hyper-v", "wsl", "docker", "tailscale", "zerotier", "bluetooth", "loopback", "vpn", "tap", "tun") {
				s -= 6
			}
			if ip.IsPrivate() {
				s += 2
			}
			if ip[0] == 172 && ip[1] == 20 && ip[2] == 10 { // iPhone kişisel erişim noktası
				s += 4
			}
			if ip[0] == 100 && ip[1]&0xC0 == 64 { // CGNAT / Tailscale
				s -= 4
			}
			if primary != "" && ip.String() == primary {
				s += 5
			}
			out = append(out, Addr{
				IP:    ip.String(),
				Iface: ifc.Name,
				URL:   fmt.Sprintf("http://%s:%d/?t=%s", ip, port, token),
				score: s,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// primaryIP, varsayılan rotanın kullandığı yerel IP'yi bulur (paket gönderilmez).
func primaryIP() string {
	c, err := net.DialTimeout("udp4", "8.8.8.8:53", 300*time.Millisecond)
	if err != nil {
		return ""
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	return ""
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// isLocalIP, isteğin bu bilgisayarın kendisinden gelip gelmediğini söyler.
func (a *App) isLocalIP(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	if p.IsLoopback() {
		return true
	}
	a.localMu.Lock()
	defer a.localMu.Unlock()
	if time.Since(a.localIPsTime) > 15*time.Second || a.localIPs == nil {
		m := map[string]bool{}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, ad := range addrs {
				if ipn, ok := ad.(*net.IPNet); ok {
					m[ipn.IP.String()] = true
				}
			}
		}
		a.localIPs = m
		a.localIPsTime = time.Now()
	}
	return a.localIPs[p.String()]
}
