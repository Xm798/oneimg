package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

var (
	ErrImportURLInvalid = errors.New("remote image URL is invalid")
	ErrImportSSRF       = errors.New("remote address is not public")
	ErrImportTooLarge   = errors.New("remote image exceeds size limit")
	ErrImportNotImage   = errors.New("remote response is not an image")
	ErrImportTooSlow    = errors.New("remote image download is too slow")
)

type ImportConfig struct {
	// Policy decides which socket addresses may be dialed; nil means PublicAddr.
	Policy      func(netip.Addr) bool
	LookupNetIP func(ctx context.Context, network, host string) ([]netip.Addr, error)
	// IdleTimeout bounds every single read or write, including the TLS handshake and headers.
	IdleTimeout time.Duration
	// The body must deliver at least MinRateBytes per MinRateWindow once Grace has passed.
	MinRateBytes  int64
	MinRateWindow time.Duration
	Grace         time.Duration
	TotalTimeout  time.Duration
}

type RemoteImporter struct {
	cfg       ImportConfig
	dialer    *net.Dialer
	transport *http.Transport
	client    *http.Client
	limiter   *importLimiter
}

func NewRemoteImporter(cfg ImportConfig) *RemoteImporter {
	if cfg.Policy == nil {
		cfg.Policy = PublicAddr
	}
	if cfg.LookupNetIP == nil {
		cfg.LookupNetIP = net.DefaultResolver.LookupNetIP
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 10 * time.Second
	}
	if cfg.MinRateBytes <= 0 {
		cfg.MinRateBytes = 32 << 10
	}
	if cfg.MinRateWindow <= 0 {
		cfg.MinRateWindow = 5 * time.Second
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 5 * time.Second
	}
	if cfg.TotalTimeout <= 0 {
		cfg.TotalTimeout = 60 * time.Second
	}
	importer := &RemoteImporter{
		cfg:     cfg,
		dialer:  &net.Dialer{Timeout: 10 * time.Second, KeepAlive: -1, ControlContext: policyControl(cfg.Policy)},
		limiter: newImportLimiter(),
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	// ResponseHeaderTimeout stays unset: header reads go through idleConn, which already bounds every gap.
	importer.transport = &http.Transport{
		Proxy:                  nil,
		DialContext:            importer.dial,
		Protocols:              protocols,
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
		DisableCompression:     true,
	}
	importer.client = &http.Client{
		Transport: importer.transport,
		Timeout:   cfg.TotalTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			_, err := ValidateRemoteURL(request.URL.String(), cfg.Policy)
			return err
		},
	}
	return importer
}

func (r *RemoteImporter) Close() {
	r.transport.CloseIdleConnections()
}

// Begin takes the import permits for userID; the caller must End the ticket.
func (r *RemoteImporter) Begin(userID int, maxFileSize int64) (*ImportTicket, error) {
	return r.limiter.Begin(userID, maxFileSize)
}

func (r *RemoteImporter) ValidateURL(rawURL string) error {
	_, err := ValidateRemoteURL(rawURL, r.cfg.Policy)
	return err
}

// ValidateRemoteURL performs the checks that need no network I/O. Hostnames are
// checked later, at dial time.
func ValidateRemoteURL(rawURL string, policy func(netip.Addr) bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrImportURLInvalid
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, ErrImportURLInvalid
	}
	if addr, err := netip.ParseAddr(host); err == nil && !policy(addr) {
		return nil, ErrImportSSRF
	}
	return parsed, nil
}

// dial rejects a hostname outright when any of its records is non-public, then
// dials literal addresses so that policyControl sees every connect attempt.
func (r *RemoteImporter) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var targets []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		targets = []netip.Addr{addr}
	} else {
		targets, err = r.cfg.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(targets) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
	}
	for _, target := range targets {
		if !r.cfg.Policy(target) {
			return nil, ErrImportSSRF
		}
	}
	var lastErr error
	for _, target := range targets {
		conn, err := r.dialer.DialContext(ctx, network, net.JoinHostPort(target.Unmap().String(), port))
		if err == nil {
			return &idleConn{Conn: conn, idle: r.cfg.IdleTimeout}, nil
		}
		lastErr = err
		if errors.Is(err, ErrImportSSRF) || ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// rateReader fails a body early once its observed throughput is below the
// floor or cannot finish before the total deadline. It only shortens failures;
// Client.Timeout remains the hard bound.
type rateReader struct {
	body                io.Reader
	cfg                 *ImportConfig
	deadline            time.Time
	contentLength       int64
	started             bool
	start, checkpointAt time.Time
	checkpointBytes     int64
	total               int64
}

func (r *rateReader) Read(p []byte) (int, error) {
	if !r.started {
		r.started = true
		r.start = time.Now()
		r.checkpointAt = r.start
	}
	n, err := r.body.Read(p)
	r.total += int64(n)
	now := time.Now()
	if now.Sub(r.start) < r.cfg.Grace || now.Sub(r.checkpointAt) < r.cfg.MinRateWindow {
		return n, err
	}
	// float64: byte counts times nanoseconds overflow int64 for large files.
	got := float64(r.total - r.checkpointBytes)
	elapsed := float64(now.Sub(r.checkpointAt))
	if got*float64(r.cfg.MinRateWindow) < float64(r.cfg.MinRateBytes)*elapsed {
		return n, ErrImportTooSlow
	}
	if r.contentLength >= 0 && got*float64(r.deadline.Sub(now)) < float64(r.contentLength-r.total)*elapsed {
		return n, ErrImportTooSlow
	}
	r.checkpointAt, r.checkpointBytes = now, r.total
	return n, err
}

// Fetch downloads rawURL, bounded by the ticket's MaxFileSize, into a disk-backed multipart form, the same shape the
// multipart upload path produces. The caller removes the form and ends the
// ticket. No processing slot or storage lock is held here.
func (r *RemoteImporter) Fetch(ctx context.Context, ticket *ImportTicket, rawURL string) (*multipart.Form, *multipart.FileHeader, error) {
	parsed, err := ValidateRemoteURL(rawURL, r.cfg.Policy)
	if err != nil {
		return nil, nil, err
	}
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, nil, errors.Join(ErrImportURLInvalid, err)
	}
	request.Header.Set("Accept", "image/*")
	request.Header.Set("User-Agent", "OneImg/1.0")
	if host := strings.ToLower(request.URL.Hostname()); host == "pximg.net" || strings.HasSuffix(host, ".pximg.net") {
		request.Header.Set("Referer", "https://www.pixiv.net/")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("remote status %d", response.StatusCode)
	}
	if response.ContentLength > ticket.maxFileSize {
		return nil, nil, ErrImportTooLarge
	}
	body := &rateReader{body: response.Body, cfg: &r.cfg, deadline: started.Add(r.cfg.TotalTimeout), contentLength: response.ContentLength}
	head := make([]byte, 512)
	n, err := io.ReadFull(body, head)
	complete := errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
	if err != nil && !complete {
		return nil, nil, err
	}
	head = head[:n]
	if n == 0 {
		return nil, nil, ErrImportNotImage
	}
	contentType := http.DetectContentType(head)
	if !strings.HasPrefix(contentType, "image/") {
		return nil, nil, ErrImportNotImage
	}
	var rest io.Reader = body
	if complete {
		rest = eofReader{}
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	done := make(chan error, 1)
	go func() {
		err := func() error {
			part, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-Disposition": {fmt.Sprintf(`form-data; name="images"; filename=%q`, importFileName(parsed))},
				"Content-Type":        {contentType},
			})
			if err != nil {
				return err
			}
			return copyReserved(part, rest, head, ticket)
		}()
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
		done <- err
	}()
	form, readErr := multipart.NewReader(pr, mw.Boundary()).ReadForm(0)
	if readErr == nil {
		// Consume the closing boundary so the writer can finish.
		_, readErr = io.Copy(io.Discard, pr)
	} else {
		// Unblock a writer that may be waiting on the pipe or on the remote body.
		pr.CloseWithError(readErr)
		response.Body.Close()
	}
	writeErr := <-done
	if writeErr != nil || readErr != nil {
		if form != nil {
			form.RemoveAll()
		}
		if writeErr != nil {
			return nil, nil, writeErr
		}
		return nil, nil, readErr
	}
	files := form.File["images"]
	if len(files) != 1 {
		form.RemoveAll()
		return nil, nil, errors.New("could not construct image upload")
	}
	return form, files[0], nil
}

// IsImportTimeout reports whether err is any timeout class of a remote import:
// the rate watchdog, an idle deadline, Client.Timeout, or a dial / TLS timeout.
func IsImportTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, ErrImportTooSlow) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout())
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

func importFileName(parsed *url.URL) string {
	name := strings.NewReplacer("\r", "", "\n", "", `"`, "").Replace(path.Base(parsed.Path))
	if name == "" || name == "." || name == "/" {
		return "imported-image"
	}
	return name
}

// copyReserved reserves budget before each disk write, so the spool never
// exceeds what the ticket holds.
func copyReserved(dst io.Writer, src io.Reader, head []byte, ticket *ImportTicket) error {
	var written int64
	write := func(chunk []byte) error {
		if written+int64(len(chunk)) > ticket.maxFileSize {
			return ErrImportTooLarge
		}
		if err := ticket.Grow(written + int64(len(chunk))); err != nil {
			return err
		}
		n, err := dst.Write(chunk)
		written += int64(n)
		return err
	}
	if err := write(head); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if writeErr := write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
