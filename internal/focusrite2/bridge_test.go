package focusrite2

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/skip2/go-qrcode"
)

func testBridge() *Bridge {
	input := make(chan string, 1)
	return &Bridge{token: "test-capability", input: input, QR: input}
}
func request(b *Bridge, method, path, kind string, body *bytes.Buffer) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		reader = body
	}
	r := httptest.NewRequest(method, "https://192.168.1.2:1234"+path, reader)
	if kind != "" {
		r.Header.Set("Content-Type", kind)
	}
	w := httptest.NewRecorder()
	b.handle(w, r)
	return w
}
func TestBridgeUploadAndConfirmation(t *testing.T) {
	b := testBridge()
	code, e := qrcode.New("deadbeef12345678", qrcode.Medium)
	if e != nil {
		t.Fatal(e)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("image", "fc2.png")
	if e = png.Encode(part, code.Image(256)); e != nil {
		t.Fatal(e)
	}
	_ = form.Close()
	w := request(b, "POST", "/test-capability/qr", form.FormDataContentType(), &body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := <-b.QR; got != "deadbeef12345678" {
		t.Fatal(got)
	}
	w = request(b, "GET", "/test-capability/status", "", nil)
	var state struct{ Complete bool }
	_ = json.Unmarshal(w.Body.Bytes(), &state)
	if state.Complete {
		t.Fatal("upload must not imply FC2 approval")
	}
	b.Finish(nil)
	w = request(b, "GET", "/test-capability/status", "", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &state)
	if !state.Complete {
		t.Fatal("missing confirmation")
	}
	w = request(b, "POST", "/test-capability/qr", "text/plain", bytes.NewBufferString("aabb"))
	if w.Code != http.StatusConflict {
		t.Fatal(w.Code)
	}
}
func TestBridgeRejectsOtherOriginAndInvalidData(t *testing.T) {
	b := testBridge()
	for _, path := range []string{"/", "/wrong/qr", "/test-capability/unknown"} {
		if w := request(b, "GET", path, "", nil); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "https://192.168.1.2/test-capability/qr", strings.NewReader("deadbeef"))
	r.Header.Set("Origin", "https://elsewhere.example")
	w := httptest.NewRecorder()
	b.handle(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, value := range []string{"", "not FC2", strings.Repeat("a", 8193), "abc"} {
		if w := request(b, "POST", "/test-capability/qr", "text/plain", bytes.NewBufferString(value)); w.Code != 400 {
			t.Fatalf("invalid payload accepted: %d", w.Code)
		}
	}
	select {
	case <-b.QR:
		t.Fatal("invalid request delivered data")
	default:
	}
}
func TestBridgeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b, e := StartBridge(ctx)
	if e != nil {
		cancel()
		t.Fatal(e)
	}
	cancel()
	_ = b.Close()
}

func TestBridgeHTTPSCertificateAndSameOriginSubmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, e := StartBridge(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer bridge.Close()
	certificate := bridge.server.TLSConfig.Certificates[0].Leaf
	if certificate == nil || bridge.CertificateFingerprint == "" {
		t.Fatal("missing certificate or fingerprint")
	}
	if e = certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature); e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	address, e := url.Parse(bridge.URLs[0])
	if e != nil {
		t.Fatal(e)
	}
	if address.Scheme != "https" {
		t.Fatal(address)
	}
	for _, endpoint := range bridge.URLs {
		u, e := url.Parse(endpoint)
		if e != nil {
			t.Fatal(e)
		}
		if e = certificate.VerifyHostname(u.Hostname()); e != nil {
			t.Fatal(e)
		}
	}
	response, e := client.Get(address.String() + "status")
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	// An HTTPS page submits an https Origin. Accept that exact origin while
	// rejecting the equivalent plaintext origin and unrelated origins.
	for _, scheme := range []string{"http", "https"} {
		request, e := http.NewRequest(http.MethodPost, address.String()+"qr", strings.NewReader("deadbeef"))
		if e != nil {
			t.Fatal(e)
		}
		request.Header.Set("Content-Type", "text/plain")
		request.Header.Set("Origin", scheme+"://"+address.Host)
		response, e = client.Do(request)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		expected := http.StatusForbidden
		if scheme == "https" {
			expected = http.StatusAccepted
		}
		if response.StatusCode != expected {
			t.Fatalf("%s origin: got %d want %d", scheme, response.StatusCode, expected)
		}
	}
	if value := <-bridge.QR; value != "deadbeef" {
		t.Fatal(value)
	}
	untrustedTransport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}}
	defer untrustedTransport.CloseIdleConnections()
	untrusted := &http.Client{Transport: untrustedTransport, Timeout: 3 * time.Second}
	if response, e = untrusted.Get(address.String() + "status"); e == nil {
		response.Body.Close()
		t.Fatal("self-signed certificate unexpectedly trusted")
	}
}

func TestLiveScanWaitsForReadableFC2Code(t *testing.T) {
	bridge := testBridge()
	for _, payload := range []string{"https://sweetspot.example/pair", "deadbeef12345678"} {
		code, e := qrcode.New(payload, qrcode.Medium)
		if e != nil {
			t.Fatal(e)
		}
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, _ := form.CreateFormFile("image", "frame.png")
		if e = png.Encode(part, code.Image(256)); e != nil {
			t.Fatal(e)
		}
		_ = form.Close()
		result := request(bridge, "POST", "/test-capability/scan", form.FormDataContentType(), &body)
		expected := http.StatusUnprocessableEntity
		if payload == "deadbeef12345678" {
			expected = http.StatusAccepted
		}
		if result.Code != expected {
			t.Fatalf("scan returned %d: %s", result.Code, result.Body.String())
		}
	}
	if payload := <-bridge.QR; payload != "deadbeef12345678" {
		t.Fatal(payload)
	}
}

func TestPairingPageHasNativeFormsAndExternalScript(t *testing.T) {
	b := testBridge()
	page := request(b, "GET", "/test-capability/", "", nil)
	for _, markup := range []string{`action="upload" method="post" enctype="multipart/form-data"`, `action="code" method="post"`, `name="payload"`, `src="app.js" defer`} {
		if !strings.Contains(page.Body.String(), markup) {
			t.Fatalf("missing non-JavaScript form or script: %s", markup)
		}
	}
	script := request(b, "GET", "/test-capability/app.js", "", nil)
	if script.Code != 200 || !strings.Contains(script.Header().Get("Content-Type"), "application/javascript") || !strings.Contains(script.Body.String(), "camera.onclick") {
		t.Fatal("scanner script unavailable")
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("same-origin script blocked")
	}
	result := request(b, "POST", "/test-capability/code", "application/x-www-form-urlencoded", bytes.NewBufferString("payload=deadbeef"))
	if result.Code != http.StatusSeeOther || result.Header().Get("Location") != "/test-capability/result" {
		t.Fatalf("native submission failed: %d %s", result.Code, result.Body.String())
	}
	if got := <-b.QR; got != "deadbeef" {
		t.Fatal(got)
	}
	status := request(b, "GET", "/test-capability/result", "", nil)
	if !strings.Contains(status.Body.String(), `http-equiv="refresh"`) {
		t.Fatal("native status page must refresh while FC2 verifies")
	}
	b.Finish(nil)
	status = request(b, "GET", "/test-capability/result", "", nil)
	if strings.Contains(status.Body.String(), `http-equiv="refresh"`) || !strings.Contains(status.Body.String(), "Paired with Focusrite") {
		t.Fatal("native status page did not show success")
	}
}

func TestDensePairingQRInCameraFrame(t *testing.T) {
	payload := strings.Repeat("a1b2c3d4e5f6", 32)
	for _, inverted := range []bool{false, true} {
		code, e := qrcode.New(payload, qrcode.Medium)
		if e != nil {
			t.Fatal(e)
		}
		if inverted {
			code.ForegroundColor = color.White
			code.BackgroundColor = color.Black
		}
		frame := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
		draw.Draw(frame, frame.Bounds(), image.NewUniform(color.RGBA{R: 45, G: 48, B: 54, A: 255}), image.Point{}, draw.Src)
		qr := code.Image(640)
		rect := image.Rect(640, 220, 1280, 860)
		draw.Draw(frame, rect, qr, image.Point{}, draw.Src)
		var encoded bytes.Buffer
		if e = jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 95}); e != nil {
			t.Fatal(e)
		}
		captured, e := jpeg.Decode(&encoded)
		if e != nil {
			t.Fatal(e)
		}
		got, e := decodeImageQR(captured)
		if e != nil || got != payload {
			t.Fatalf("camera QR inverted=%v: err=%v payload matches=%v", inverted, e, got == payload)
		}
	}
}

// A desktop screenshot of FC2's window decodes, and the code is handed to
// the pairing session once, as with the mobile page.
func TestScreenshotSubmitOnce(t *testing.T) {
	payload := strings.Repeat("0f1e2d3c4b5a6978", 16)
	code, e := qrcode.New(payload, qrcode.Medium)
	if e != nil {
		t.Fatal(e)
	}
	shot := image.NewRGBA(image.Rect(0, 0, 2560, 1600))
	draw.Draw(shot, shot.Bounds(), image.NewUniform(color.RGBA{R: 30, G: 30, B: 32, A: 255}), image.Point{}, draw.Src)
	draw.Draw(shot, image.Rect(1500, 300, 2140, 940), code.Image(640), image.Point{}, draw.Src) // off-centre, as in a window
	var file bytes.Buffer
	if e = png.Encode(&file, shot); e != nil {
		t.Fatal(e)
	}
	got, e := DecodeQRImage(bytes.NewReader(file.Bytes()))
	if e != nil || got != payload {
		t.Fatalf("screenshot: err=%v payload matches=%v", e, got == payload)
	}

	b := testBridge()
	if e = b.Submit("not hex"); e == nil {
		t.Fatal("an invalid code must be rejected")
	}
	if e = b.Submit(got); e != nil {
		t.Fatal(e)
	}
	if v := <-b.QR; v != payload {
		t.Fatal("submitted code not handed to pairing")
	}
	if e = b.Submit(got); !errors.Is(e, ErrAlreadySubmitted) {
		t.Fatalf("second submit: %v", e)
	}

	if _, e = DecodeQRImage(bytes.NewReader([]byte("plain text"))); e == nil {
		t.Error("a non-image file must be rejected")
	}
}
