package uploadserver

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

var rk = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))

func appPkg(t *testing.T, version string) []byte {
	var buf bytes.Buffer
	if err := mbu.Build(&buf, mbu.Manifest{Type: mbu.TypeApp, Version: version, App: &mbu.AppInfo{LocalAPI: 2}},
		[]mbu.Source{{Path: "index.html", Data: []byte("x")}}, []ed25519.PrivateKey{rk}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func server(t *testing.T) (*Server, *int64) {
	l := store.Layout{Root: t.TempDir()}
	ring := trust.NewRing(nil, []ed25519.PublicKey{rk.Public().(ed25519.PublicKey)})
	im := importer.New(importer.Deps{Layout: l, DeviceID: "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b", AgentVersion: "0.2.0",
		Ring: func() (*trust.Ring, error) { return ring, nil }})
	var got int64
	return New(Deps{Layout: l, DeviceID: "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b", Importer: im,
		ApplyClientTime: func(unix int64, b importer.Batch) ClockReport {
			if b.Verified {
				got = unix
			}
			return ClockReport{}
		}}), &got
}

func upload(t *testing.T, s *Server, host string, files map[string][]byte) (*httptest.ResponseRecorder, ImportResponse) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, data := range files {
		w, _ := mw.CreateFormFile("package", name)
		w.Write(data)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/import", &body)
	req.Host = host
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-MB-Client-Time", "1790000000")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var r ImportResponse
	json.Unmarshal(w.Body.Bytes(), &r)
	return w, r
}

func TestImport(t *testing.T) {
	s, clock := server(t)
	w, r := upload(t, s, ServiceIP, map[string][]byte{"../../etc/app.mbu": appPkg(t, "2.0.0")})
	if w.Code != 200 || len(r.Results) != 1 || r.Results[0].Action != importer.ActionInstalled {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r.Results[0].File != "app.mbu" {
		t.Errorf("upload name not sanitised: %q", r.Results[0].File)
	}
	if *clock != 1790000000 {
		t.Errorf("client time not passed on after a verified import")
	}
}

func TestGarbageIsRefusedAndClockIgnored(t *testing.T) {
	s, clock := server(t)
	_, r := upload(t, s, ServiceIP, map[string][]byte{"x.mbu": []byte("not a zip")})
	if len(r.Results) != 1 || r.Results[0].Action != importer.ActionRejected {
		t.Fatalf("%+v", r)
	}
	if *clock != 0 {
		t.Error("clock applied without a verified package")
	}
}

func TestHostGuard(t *testing.T) {
	s, _ := server(t)
	w, _ := upload(t, s, "evil.example", map[string][]byte{"a.mbu": appPkg(t, "2.0.0")})
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding host accepted: %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = ServiceIP
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("MusallahBoard update")) {
		t.Fatalf("upload page: %d", rec.Code)
	}
}
