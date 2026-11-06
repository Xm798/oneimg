package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func loopbackPolicy(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr == netip.MustParseAddr("127.0.0.1") || addr == netip.IPv6Loopback() || PublicAddr(addr)
}

func newTestImporter(t *testing.T, mutate func(*ImportConfig)) *RemoteImporter {
	t.Helper()
	cfg := ImportConfig{Policy: loopbackPolicy}
	if mutate != nil {
		mutate(&cfg)
	}
	importer := NewRemoteImporter(cfg)
	t.Cleanup(importer.Close)
	return importer
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func fetchFrom(t *testing.T, importer *RemoteImporter, rawURL string, maxFileSize int64) (*multipart.Form, *multipart.FileHeader, error) {
	t.Helper()
	ticket, err := importer.Begin(1, maxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ticket.End)
	form, header, err := importer.Fetch(context.Background(), ticket, rawURL)
	if form != nil {
		t.Cleanup(func() { form.RemoveAll() })
	}
	return form, header, err
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "multipart-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestRemoteImportShortImageIsDiskBacked(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	payload := testPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(payload)
	}))
	defer server.Close()

	form, header, err := fetchFrom(t, newTestImporter(t, nil), server.URL+"/dir/photo%22x.png", 10*mib)
	if err != nil {
		t.Fatal(err)
	}
	if header.Size != int64(len(payload)) || header.Filename != `photox.png` || header.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("header = %q size=%d type=%q", header.Filename, header.Size, header.Header.Get("Content-Type"))
	}
	file, err := header.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.(*os.File); !ok {
		t.Fatalf("spooled file type = %T, want *os.File", file)
	}
	got, _ := io.ReadAll(file)
	file.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("spooled bytes differ from the remote body")
	}
	if len(spoolFiles(t, temp)) != 1 {
		t.Fatal("expected one spool file under TMPDIR")
	}
	if err := form.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if len(spoolFiles(t, temp)) != 0 {
		t.Fatal("RemoveAll left the spool file behind")
	}
}

func pngHead(t *testing.T) []byte {
	t.Helper()
	head := append([]byte(nil), testPNG(t)...)
	return append(head, make([]byte, 512)...)[:512]
}

func fakeLookup(records map[string][]netip.Addr) func(context.Context, string, string) ([]netip.Addr, error) {
	return func(_ context.Context, _ string, host string) ([]netip.Addr, error) {
		if addrs, ok := records[host]; ok {
			return addrs, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
}

func countingServer(handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	return server, &hits
}

func TestRemoteImportRejectsHostnameResolvingPrivate(t *testing.T) {
	payload := testPNG(t)
	server, hits := countingServer(func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) })
	defer server.Close()
	importer := newTestImporter(t, func(cfg *ImportConfig) {
		cfg.LookupNetIP = fakeLookup(map[string][]netip.Addr{"internal.test": {netip.MustParseAddr("10.0.0.1")}})
	})
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	if _, _, err := fetchFrom(t, importer, "http://internal.test:"+port+"/a.png", 10*mib); !errors.Is(err, ErrImportSSRF) {
		t.Fatalf("error = %v, want ErrImportSSRF", err)
	}
	if hits.Load() != 0 {
		t.Fatal("private hostname was contacted")
	}
}

func TestRemoteImportRedirectToPrivateHostnameRejected(t *testing.T) {
	var target string
	server, hits := countingServer(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	})
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	target = "http://internal.test:" + port + "/b.png"
	importer := newTestImporter(t, func(cfg *ImportConfig) {
		cfg.LookupNetIP = fakeLookup(map[string][]netip.Addr{
			"origin.test":   {netip.MustParseAddr("127.0.0.1")},
			"internal.test": {netip.MustParseAddr("10.0.0.1")},
		})
	})
	if _, _, err := fetchFrom(t, importer, "http://origin.test:"+port+"/a.png", 10*mib); !errors.Is(err, ErrImportSSRF) {
		t.Fatalf("error = %v, want ErrImportSSRF", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want only the origin request", hits.Load())
	}
}

func TestRemoteImportControlRejectsWhenPrecheckBypassed(t *testing.T) {
	payload := testPNG(t)
	server, hits := countingServer(func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) })
	defer server.Close()
	var calls atomic.Int64
	importer := newTestImporter(t, func(cfg *ImportConfig) {
		cfg.LookupNetIP = fakeLookup(map[string][]netip.Addr{"origin.test": {netip.MustParseAddr("127.0.0.1")}})
		// Approves only the dial pre-check, as a rebinding resolver would.
		cfg.Policy = func(addr netip.Addr) bool { return calls.Add(1) == 1 && loopbackPolicy(addr) }
	})
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	if _, _, err := fetchFrom(t, importer, "http://origin.test:"+port+"/a.png", 10*mib); !errors.Is(err, ErrImportSSRF) {
		t.Fatalf("error = %v, want ErrImportSSRF", err)
	}
	if hits.Load() != 0 {
		t.Fatal("socket connected although the dial-time policy denied it")
	}
}

func TestRemoteImportIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)
	importer := newTestImporter(t, func(cfg *ImportConfig) { cfg.IdleTimeout = 100 * time.Millisecond })
	started := time.Now()
	_, _, err := fetchFrom(t, importer, server.URL+"/a.png", 10*mib)
	if !errors.Is(err, os.ErrDeadlineExceeded) || !IsImportTimeout(err) {
		t.Fatalf("error = %v, want an idle deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("idle timeout took %v", elapsed)
	}
}

func TestRemoteImportHeaderStall(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	importer := newTestImporter(t, func(cfg *ImportConfig) { cfg.IdleTimeout = 100 * time.Millisecond })
	_, _, err = fetchFrom(t, importer, "http://"+listener.Addr().String()+"/a.png", 10*mib)
	if !IsImportTimeout(err) {
		t.Fatalf("error = %v, want a timeout", err)
	}
}

func dripServer(t *testing.T, contentLength int, interval time.Duration, chunk int) *httptest.Server {
	t.Helper()
	head := pngHead(t)
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentLength > 0 {
			w.Header().Set("Content-Length", strconv.Itoa(contentLength))
		}
		w.Write(head)
		w.(http.Flusher).Flush()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := w.Write(make([]byte, chunk)); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(func() { close(stop); server.Close() })
	return server
}

func TestRemoteImportTotalTimeout(t *testing.T) {
	server := dripServer(t, 0, 10*time.Millisecond, 64)
	importer := newTestImporter(t, func(cfg *ImportConfig) {
		cfg.TotalTimeout = 300 * time.Millisecond
		cfg.Grace = time.Minute
	})
	started := time.Now()
	_, _, err := fetchFrom(t, importer, server.URL+"/a.png", 10*mib)
	if !IsImportTimeout(err) {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("total timeout took %v", elapsed)
	}
}

func TestRemoteImportMinimumRate(t *testing.T) {
	server := dripServer(t, 0, 20*time.Millisecond, 1)
	importer := newTestImporter(t, func(cfg *ImportConfig) {
		cfg.Grace = 100 * time.Millisecond
		cfg.MinRateWindow = 100 * time.Millisecond
		cfg.MinRateBytes = 1 << 10
	})
	if _, _, err := fetchFrom(t, importer, server.URL+"/a.png", 10*mib); !errors.Is(err, ErrImportTooSlow) {
		t.Fatalf("error = %v, want ErrImportTooSlow", err)
	}
}

func TestRemoteImportInfeasibleDeadline(t *testing.T) {
	// About 50 KiB/s against a 10 MiB body and a 2s total.
	server := dripServer(t, 10<<20, 10*time.Millisecond, 512)
	importer := newTestImporter(t, func(cfg *ImportConfig) {
		cfg.TotalTimeout = 2 * time.Second
		cfg.Grace = 100 * time.Millisecond
		cfg.MinRateWindow = 100 * time.Millisecond
		cfg.MinRateBytes = 1
	})
	started := time.Now()
	if _, _, err := fetchFrom(t, importer, server.URL+"/a.png", 20*mib); !errors.Is(err, ErrImportTooSlow) {
		t.Fatalf("error = %v, want ErrImportTooSlow", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("infeasible download failed after %v", elapsed)
	}
}

func TestRemoteImportRejectsOversizeBodies(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%v", chunked), func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			body := append(pngHead(t), make([]byte, 2*mib)...)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if chunked {
					w.Write(body[:512])
					w.(http.Flusher).Flush()
					w.Write(body[512:])
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.Write(body)
			}))
			defer server.Close()
			if _, _, err := fetchFrom(t, newTestImporter(t, nil), server.URL+"/a.png", mib); !errors.Is(err, ErrImportTooLarge) {
				t.Fatalf("error = %v, want ErrImportTooLarge", err)
			}
			if files := spoolFiles(t, temp); len(files) != 0 {
				t.Fatalf("spool files left behind: %v", files)
			}
		})
	}
}

func TestRemoteImportRejectsNonImageEarly(t *testing.T) {
	const total = 64 << 20
	written := make(chan int64, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		chunk := bytes.Repeat([]byte("<html>"), 1<<10)
		var sent int64
		for sent < total {
			n, err := w.Write(chunk)
			sent += int64(n)
			if err != nil {
				break
			}
		}
		written <- sent
	}))
	defer server.Close()
	if _, _, err := fetchFrom(t, newTestImporter(t, nil), server.URL+"/a.png", 128*mib); !errors.Is(err, ErrImportNotImage) {
		t.Fatalf("error = %v, want ErrImportNotImage", err)
	}
	select {
	case sent := <-written:
		if sent >= total {
			t.Fatal("server delivered the whole body; the download was not cut short")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server write was not interrupted")
	}
}

func TestRemoteImportSharedTransportNoLeak(t *testing.T) {
	payload := testPNG(t)
	var closed atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	importer := newTestImporter(t, nil)
	transport := importer.transport
	baseline := runtime.NumGoroutine()
	for range 20 {
		ticket, err := importer.Begin(1, 10*mib)
		if err != nil {
			t.Fatal(err)
		}
		form, _, err := importer.Fetch(context.Background(), ticket, server.URL+"/a.png")
		if err != nil {
			t.Fatal(err)
		}
		form.RemoveAll()
		ticket.End()
		if importer.transport != transport || importer.client.Transport != transport {
			t.Fatal("transport was replaced")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() < 20 || runtime.NumGoroutine() > baseline+2 {
		if time.Now().After(deadline) {
			t.Fatalf("closed=%d goroutines=%d baseline=%d", closed.Load(), runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRemoteImportHTTP1Only(t *testing.T) {
	protocols := make(chan string, 1)
	payload := testPNG(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocols <- r.Proto
		w.Write(payload)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	importer := newTestImporter(t, nil)
	importer.transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if _, _, err := fetchFrom(t, importer, server.URL+"/a.png", 10*mib); err != nil {
		t.Fatal(err)
	}
	if proto := <-protocols; proto != "HTTP/1.1" {
		t.Fatalf("protocol = %s, want HTTP/1.1", proto)
	}
}

func TestRemoteImportStopsWhenUserByteCapIsHeld(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	body := append(pngHead(t), make([]byte, 2*mib)...)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	defer server.Close()
	importer := newTestImporter(t, nil)
	// maxFileSize 64 MiB gives a per-user cap of 64 MiB; the first ticket holds all of it.
	holder, err := importer.Begin(1, 64*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.End()
	if err := holder.Grow(64 * mib); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetchFrom(t, importer, server.URL+"/a.png", 64*mib); !errors.Is(err, ErrImportBusy) {
		t.Fatalf("error = %v, want ErrImportBusy", err)
	}
	if files := spoolFiles(t, temp); len(files) != 0 {
		t.Fatalf("spool files left behind: %v", files)
	}
}

func TestRemoteImportDownloadHoldsNoStorageLock(t *testing.T) {
	storageTopology.Lock()
	defer storageTopology.Unlock()
	unlock := lockBucket(1, true)
	defer unlock()
	payload := testPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) }))
	defer server.Close()
	importer := newTestImporter(t, nil)
	ticket, err := importer.Begin(1, 10*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer ticket.End()
	done := make(chan error, 1)
	go func() {
		form, _, err := importer.Fetch(context.Background(), ticket, server.URL+"/a.png")
		if form != nil {
			form.RemoveAll()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download blocked on a storage lock")
	}
}
