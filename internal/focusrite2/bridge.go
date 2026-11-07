package focusrite2

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// Bridge exposes only an expiring QR transfer page, never FC2 credentials or
// monitor controls. Its random capability URL must be scanned in person.
type Bridge struct {
	URLs                   []string
	CertificateFingerprint string
	QR                     <-chan string
	input                  chan string
	server                 *http.Server
	token                  string
	mu                     sync.Mutex
	message                string
	complete               bool
	failed                 bool
	closed                 chan struct{}
	closeOnce              sync.Once
	submitted              bool
}

func StartBridge(ctx context.Context) (*Bridge, error) {
	ln, e := net.Listen("tcp4", "0.0.0.0:0")
	if e != nil {
		return nil, e
	}
	var random [32]byte
	if _, e = rand.Read(random[:]); e != nil {
		ln.Close()
		return nil, e
	}
	input := make(chan string, 1)
	b := &Bridge{closed: make(chan struct{}), input: input, QR: input, token: hex.EncodeToString(random[:]), message: "Waiting for Focusrite Control 2…"}
	port := ln.Addr().(*net.TCPAddr).Port
	addrs, e := net.InterfaceAddrs()
	if e != nil {
		ln.Close()
		return nil, e
	}
	certificateIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, addr := range addrs {
		ip, _, _ := net.ParseCIDR(addr.String())
		if ip.To4() != nil && ip.IsPrivate() && !ip.IsLoopback() {
			certificateIPs = append(certificateIPs, ip)
			b.URLs = append(b.URLs, fmt.Sprintf("https://%s:%d/%s/", ip.String(), port, b.token))
		}
	}
	if len(b.URLs) == 0 {
		ln.Close()
		return nil, errors.New("no local network IPv4 address; connect this computer to Wi-Fi or Ethernet")
	}
	certificate, fingerprint, e := pairingCertificate(certificateIPs)
	if e != nil {
		ln.Close()
		return nil, e
	}
	b.CertificateFingerprint = fingerprint
	b.server = &http.Server{Handler: http.HandlerFunc(b.handle), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	go func() { _ = b.server.ServeTLS(ln, "", "") }()
	go func() {
		select {
		case <-ctx.Done():
		case <-b.closed:
			return
		case <-time.After(5 * time.Minute):
			b.Finish(errors.New("pairing expired; start again in Sweetspot"))
		}
		_ = b.Close()
	}()
	return b, nil
}
func (b *Bridge) Close() error    { b.closeOnce.Do(func() { close(b.closed) }); return b.server.Close() }
func (b *Bridge) Status(s string) { b.mu.Lock(); b.message = s; b.mu.Unlock() }
func (b *Bridge) Finish(e error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.complete = true
	time.AfterFunc(10*time.Second, func() {
		if b.server != nil {
			_ = b.Close()
		}
	})
	b.failed = e != nil
	if e != nil {
		b.message = e.Error()
	} else {
		b.message = "Paired with Focusrite Control 2. You can close this page."
	}
}

func (b *Bridge) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Permissions-Policy", "camera=(self), microphone=()")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' blob:; media-src 'self' blob:; frame-ancestors 'none'; form-action 'self'")
	prefix := "/" + b.token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	// Browser requests from another origin must not submit a pairing payload.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+r.Host {
		http.Error(w, "Origin rejected", http.StatusForbidden)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, prefix) {
	case "":
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, mobilePage)
	case "app.js":
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = io.WriteString(w, mobileScript)
	case "client-event":
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var event struct {
			Event, Message string
			Secure, Camera bool
		}
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&event); e != nil {
			http.Error(w, "invalid client event", 400)
			return
		}
		log.Info("mobile pairing client", "event", event.Event, "message", event.Message, "secure", event.Secure, "camera", event.Camera)
		w.WriteHeader(http.StatusNoContent)
	case "result":
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		b.mu.Lock()
		state := struct {
			Message  string
			Complete bool
		}{b.message, b.complete}
		b.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resultPage.Execute(w, state)
	case "status":
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": b.message, "complete": b.complete, "failed": b.failed})
	case "qr", "scan", "upload", "code":
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		value, e := readQR(r)
		if strings.HasSuffix(r.URL.Path, "/scan") && (errors.Is(e, errNoQR) || (e == nil && !validPayload(value))) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if e != nil {
			code := http.StatusBadRequest
			if errors.Is(e, errWrongQR) {
				code = http.StatusUnprocessableEntity
			}
			http.Error(w, e.Error(), code)
			return
		}
		if _, e = DecodePayload(value); e != nil {
			http.Error(w, e.Error(), http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.submitted || b.complete {
			http.Error(w, "This pairing code was already submitted or the session ended.", http.StatusConflict)
			return
		}
		b.submitted = true
		b.message = "Checking the QR code with Focusrite Control 2…"
		b.input <- value
		if strings.HasSuffix(r.URL.Path, "/upload") || strings.HasSuffix(r.URL.Path, "/code") {
			http.Redirect(w, r, "result", http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

func readQR(r *http.Request) (string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if e := r.ParseForm(); e != nil {
			return "", e
		}
		return r.PostForm.Get("payload"), nil
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		p, e := io.ReadAll(io.LimitReader(r.Body, 8193))
		if e != nil {
			return "", e
		}
		return strings.TrimSpace(string(p)), nil
	}
	if e := r.ParseMultipartForm(10 << 20); e != nil {
		return "", errors.New("could not read QR image (maximum 10 MB)")
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	f, _, e := r.FormFile("image")
	if e != nil {
		return "", errors.New("select a photo containing the FC2 QR code")
	}
	defer f.Close()
	config, _, e := image.DecodeConfig(f)
	if e != nil {
		return "", errors.New("use a JPEG or PNG image")
	}
	if config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > 24_000_000 {
		return "", errors.New("image too large; crop the photo to the QR code")
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return "", e
	}
	img, _, e := image.Decode(f)
	if e != nil {
		return "", errors.New("could not decode QR image")
	}
	return decodeImageQR(img)
}

func decodeImageQR(img image.Image) (string, error) {
	source := gozxing.NewLuminanceSourceFromImage(img)
	sources := []gozxing.LuminanceSource{source}
	// A center crop removes surrounding windows and unrelated QR codes without
	// throwing away any camera pixels. Try the full image before cropped views.
	side := min(source.GetWidth(), source.GetHeight())
	for _, size := range []int{side, side * 3 / 4, side / 2} {
		if size < 64 {
			continue
		}
		cropped, e := source.Crop((source.GetWidth()-size)/2, (source.GetHeight()-size)/2, size, size)
		if e == nil {
			sources = append(sources, cropped)
		}
	}
	foundOther := false
	for _, src := range sources {
		for _, candidate := range []gozxing.LuminanceSource{src, src.Invert()} {
			for _, binarizer := range []gozxing.Binarizer{gozxing.NewHybridBinarizer(candidate), gozxing.NewGlobalHistgramBinarizer(candidate)} {
				bitmap, e := gozxing.NewBinaryBitmap(binarizer)
				if e != nil {
					continue
				}
				code, e := qrcode.NewQRCodeReader().Decode(bitmap, map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_TRY_HARDER: true})
				if e != nil {
					continue
				}
				value := strings.TrimSpace(code.GetText())
				if validPayload(value) {
					return value, nil
				}
				foundOther = true
			}
		}
	}
	if foundOther {
		return "", errWrongQR
	}
	return "", errNoQR
}

var errWrongQR = errors.New("QR code recognized, but it is not the FC2 pairing code; scan the code displayed by Focusrite Control 2")

var errNoQR = errors.New("no FC2 QR code found; keep the complete code in view")

func validPayload(s string) bool { _, e := DecodePayload(s); return e == nil }

//go:embed mobile.html
var mobilePage string

//go:embed mobile.js
var mobileScript string

var resultPage = template.Must(template.New("pairing-result").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">{{if not .Complete}}<meta http-equiv="refresh" content="1">{{end}}<title>Sweetspot pairing</title><style>body{font:18px system-ui;background:#121417;color:#e8eaed;max-width:32rem;margin:auto;padding:24px}a{color:#3ddc84}</style><h1>Sweetspot</h1><p role="status">{{.Message}}</p><p><a href="./">Back to pairing</a></p></html>`))
