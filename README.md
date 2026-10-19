# Köprü

iPhone ile Windows PC arasında Wi-Fi üzerinden iki yönlü fotoğraf, video ve dosya aktarımı.
PC'de tek bir `.exe` çalışır; iPhone tarafı Safari'de açılan bir sayfadır, uygulama kurulmaz.

## Nasıl çalışır

- PC'de yerel ağda bir HTTP sunucusu açılır (varsayılan port 8765).
- Panel (`/pc`) yalnızca bilgisayarın kendisinden açılabilir; QR kodu, gelen dosyaları ve
  gönderilecekleri gösterir.
- iPhone QR koddaki adresi açar. Adresteki erişim anahtarı (`?t=`) olmadan hiçbir sayfa açılmaz;
  hatalı denemeler IP başına sınırlandırılır.
- Yüklemeler belleğe alınmadan doğrudan diske akar (çok büyük videolar sorun olmaz), bitince
  `.kopru-part` uzantılı geçici dosya asıl adına taşınır. Aynı adlı dosyalar `ad (1).jpg` olur.
- İndirmeler Range destekler (Safari'de video oynatma ve kaldığı yerden devam), çoklu indirme
  sıkıştırmasız ZIP olarak akar.
- JPEG/PNG/GIF için küçük önizlemeler üretilir; iPhone fotoğraflarındaki EXIF döndürme uygulanır.

## Klasörler

- `%USERPROFILE%\Köprü\iPhone'dan Gelenler`
- `%USERPROFILE%\Köprü\iPhone'a Gönderilecekler`

## Derleme

Go 1.22+ gerekir.

```
go build -ldflags "-s -w" -o Kopru.exe .
```

Linux/macOS'tan Windows için:

```
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o Kopru.exe .
```

İkon `rsrc_windows_amd64.syso` dosyasından gelir. Yeniden üretmek için:

```
go install github.com/akavel/rsrc@latest
rsrc -ico winres/icon.ico -manifest winres/kopru.manifest -arch amd64 -o rsrc_windows_amd64.syso
```

## Dosyalar

| Dosya | İçerik |
|---|---|
| `main.go` | Başlangıç, ayarlar, ikinci kopya kontrolü, konsol çıktısı |
| `server.go` | HTTP uçları, erişim kontrolü, yükleme/indirme/ZIP |
| `files.go` | Dosya adı temizleme, listeleme, benzersiz ad ayırma |
| `thumb.go` | Önizleme üretimi ve EXIF döndürme |
| `netinfo.go` | iPhone'un ulaşabileceği yerel IP'nin seçimi |
| `platform_windows.go` | Konsol ayarları, tarayıcı/Gezgin açma |
| `web/` | PC paneli, iPhone sayfası, ortak stil, ikon |
