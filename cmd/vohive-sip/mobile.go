package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iniwex5/vohive/internal/notify"
	"github.com/iniwex5/vohive/internal/winmodem"
	qrcode "github.com/skip2/go-qrcode"
)

const mobileMaxBodyBytes = 4096

var mobileNumberPattern = regexp.MustCompile(`^\+?[0-9]{3,20}$`)

type mobileModem interface {
	Snapshot() winmodem.Status
	SMSInbox() []winmodem.IncomingSMS
	SMSReceiverStatus() winmodem.SMSReceiveStatus
	SendSMS(string, string) error
}

type mobileRegistrar interface{ CountRegistered() int }
type mobileEvents interface{ Events(int) []notify.Event }

type mobilePairing struct {
	Version   int              `json:"version"`
	APIURL    string           `json:"api_url"`
	Token     string           `json:"token"`
	TLSSHA256 string           `json:"tls_sha256"`
	SIP       mobileSIPPairing `json:"sip"`
}
type mobileSIPPairing struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Transport string `json:"transport"`
}

type mobileService struct {
	listen    string
	token     string
	tlsConfig *tls.Config
	pairing   mobilePairing
	server    *http.Server
	ready     atomic.Bool
}

// newMobileService validates all mobile settings before the modem is opened.
// An empty mobile_listen retains the existing loopback-only deployment.
func newMobileService(cfg config, baseDir string) (*mobileService, error) {
	if cfg.MobileListen == "" {
		return nil, nil
	}
	listenHost, portText, err := net.SplitHostPort(cfg.MobileListen)
	if err != nil || net.ParseIP(listenHost) == nil {
		return nil, errors.New("mobile_listen must contain a bind IP and port, for example 0.0.0.0:7577")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("mobile_listen port must be between 1 and 65535")
	}
	if len(cfg.MobileToken) < 32 || len(cfg.MobileToken) > 512 || strings.ContainsAny(cfg.MobileToken, " \t\r\n") {
		return nil, errors.New("mobile_token must be 32 to 512 non-whitespace characters")
	}
	host := strings.TrimSpace(cfg.MobileAdvertiseHost)
	if host == "" || strings.ContainsAny(host, "/\\?#@ \t\r\n") || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return nil, errors.New("mobile_advertise_host must be a certificate-covered host or IP, without a URL scheme or port")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
		return nil, errors.New("mobile_advertise_host must identify a reachable unicast address")
	}
	if cfg.MobileCertFile == "" || cfg.MobileKeyFile == "" {
		return nil, errors.New("mobile_cert_file and mobile_key_file are required")
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(baseDir, path)
	}
	cert, err := tls.LoadX509KeyPair(resolve(cfg.MobileCertFile), resolve(cfg.MobileKeyFile))
	if err != nil {
		return nil, fmt.Errorf("load mobile TLS certificate/key: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("mobile TLS certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, errors.New("mobile TLS leaf certificate is invalid")
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("mobile TLS certificate is not currently valid")
	}
	if err = leaf.VerifyHostname(host); err != nil {
		return nil, errors.New("mobile TLS certificate SAN does not match mobile_advertise_host")
	}
	if len(leaf.ExtKeyUsage) > 0 {
		serverAllowed := false
		for _, usage := range leaf.ExtKeyUsage {
			if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
				serverAllowed = true
			}
		}
		if !serverAllowed {
			return nil, errors.New("mobile TLS certificate does not permit server authentication")
		}
	}
	sipHost := net.ParseIP(cfg.SIPAdvertiseIP)
	if sipHost == nil || sipHost.IsLoopback() || sipHost.IsUnspecified() || sipHost.IsMulticast() {
		return nil, errors.New("mobile pairing requires a reachable LAN/VPN sip_advertise_ip")
	}
	sipListenHost, sipPortText, err := net.SplitHostPort(cfg.SIPListen)
	if err != nil {
		return nil, errors.New("mobile pairing requires a valid sip_listen IP and port")
	}
	sipListenIP := net.ParseIP(sipListenHost)
	if sipListenIP == nil || sipListenIP.IsLoopback() {
		return nil, errors.New("mobile pairing requires SIP to listen on its LAN/VPN address or wildcard")
	}
	sipPort, err := strconv.Atoi(sipPortText)
	if err != nil || sipPort < 1 || sipPort > 65535 {
		return nil, errors.New("mobile pairing requires a valid SIP port")
	}
	digest := sha256.Sum256(cert.Certificate[0])
	return &mobileService{listen: cfg.MobileListen, token: cfg.MobileToken,
		tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		pairing:   mobilePairing{Version: 1, APIURL: "https://" + net.JoinHostPort(host, portText), Token: cfg.MobileToken, TLSSHA256: hex.EncodeToString(digest[:]), SIP: mobileSIPPairing{Host: cfg.SIPAdvertiseIP, Port: sipPort, Username: cfg.SIPUsername, Password: cfg.SIPPassword, Transport: "udp"}},
	}, nil
}

func (s *mobileService) Ready() bool { return s.ready.Load() }

func newMobileHTTPServer(address string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{Addr: address, Handler: handler, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}

// Start binds synchronously, so a port conflict cannot be reported as a usable mobile API.
func (s *mobileService) Start(ctx context.Context, modem mobileModem, registrar mobileRegistrar, events mobileEvents, failed func(error)) error {
	listener, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("bind Android HTTPS listener: %w", err)
	}
	s.server = newMobileHTTPServer(s.listen, newMobileHandler(s.token, modem, registrar, events), s.tlsConfig)
	s.ready.Store(true)
	go func() {
		err := s.server.ServeTLS(listener, "", "")
		s.ready.Store(false)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && failed != nil {
			failed(err)
		}
	}()
	go func() {
		<-ctx.Done()
		s.ready.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			_ = s.server.Close()
		}
	}()
	return nil
}

func (s *mobileService) Close() {
	s.ready.Store(false)
	if s.server != nil {
		_ = s.server.Close()
	}
}

func mobileJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func mobileError(w http.ResponseWriter, status int, message string) {
	mobileJSON(w, status, map[string]string{"error": message})
}

func mobileMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	mobileError(w, http.StatusMethodNotAllowed, "不支持该请求方法")
	return false
}

type mobileCall struct {
	ID         string    `json:"id"`
	Number     string    `json:"number"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	OccurredAt time.Time `json:"occurred_at"`
	Status     string    `json:"status"`
}

func newMobileHandler(token string, modem mobileModem, registrar mobileRegistrar, events mobileEvents) http.Handler {
	expected := sha256.Sum256([]byte(token))
	smsBusy := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		authorization := r.Header.Get("Authorization")
		provided := strings.TrimPrefix(authorization, "Bearer ")
		actual := sha256.Sum256([]byte(provided))
		if !strings.HasPrefix(authorization, "Bearer ") || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="VoHive mobile"`)
			mobileError(w, http.StatusUnauthorized, "需要有效的手机配对凭据")
			return
		}
		switch r.URL.Path {
		case "/v1/status":
			if !mobileMethod(w, r, http.MethodGet) {
				return
			}
			mobileJSON(w, http.StatusOK, map[string]any{"version": version, "modem": modem.Snapshot(), "sip_registered": registrar.CountRegistered()})
		case "/v1/inbox":
			if !mobileMethod(w, r, http.MethodGet) {
				return
			}
			messages := modem.SMSInbox()
			if messages == nil {
				messages = []winmodem.IncomingSMS{}
			}
			mobileJSON(w, http.StatusOK, map[string]any{"messages": messages, "receiver": modem.SMSReceiverStatus()})
		case "/v1/calls":
			if !mobileMethod(w, r, http.MethodGet) {
				return
			}
			calls := []mobileCall{}
			for _, e := range events.Events(1000) {
				if e.Type == "incoming_call" {
					calls = append(calls, mobileCall{ID: e.ID, Number: e.Number, Title: e.Title, Body: e.Body, OccurredAt: e.OccurredAt, Status: e.Status})
				}
			}
			mobileJSON(w, http.StatusOK, calls)
		case "/v1/sms":
			if !mobileMethod(w, r, http.MethodPost) {
				return
			}
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				mobileError(w, http.StatusUnsupportedMediaType, "短信请求必须使用 application/json")
				return
			}
			if r.ContentLength > mobileMaxBodyBytes {
				mobileError(w, http.StatusRequestEntityTooLarge, "短信请求过大")
				return
			}
			var body struct {
				Number string `json:"number"`
				Text   string `json:"text"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, mobileMaxBodyBytes))
			decoder.DisallowUnknownFields()
			if err = decoder.Decode(&body); err != nil {
				mobileDecodeError(w, err)
				return
			}
			if err = decoder.Decode(new(any)); err != io.EOF {
				if err == nil {
					err = errors.New("multiple JSON values")
				}
				mobileDecodeError(w, err)
				return
			}
			body.Number = strings.TrimSpace(body.Number)
			if !mobileNumberPattern.MatchString(body.Number) || len(body.Text) == 0 {
				mobileError(w, http.StatusUnprocessableEntity, "收件号码或短信内容无效")
				return
			}
			if len(body.Text) > 2048 {
				mobileError(w, http.StatusRequestEntityTooLarge, "短信内容过长；当前仅支持单条短信")
				return
			}
			if !modem.Snapshot().Registered {
				mobileError(w, http.StatusConflict, "模组尚未注册蜂窝网络，短信未提交")
				return
			}
			select {
			case smsBusy <- struct{}{}:
				defer func() { <-smsBusy }()
			default:
				mobileError(w, http.StatusConflict, "已有短信正在提交，请等待其结果；不要重复发送")
				return
			}
			if r.Context().Err() != nil {
				mobileError(w, http.StatusRequestTimeout, "请求已取消，短信未提交")
				return
			}
			// Exactly one call; a timeout or uncertain modem response is never retried here.
			if err = modem.SendSMS(body.Number, body.Text); err != nil {
				status := http.StatusConflict
				if strings.Contains(err.Error(), "single SMS") || strings.Contains(err.Error(), "invalid number") {
					status = http.StatusUnprocessableEntity
				}
				if strings.Contains(strings.ToLower(err.Error()), "timeout") {
					status = http.StatusGatewayTimeout
				}
				mobileError(w, status, err.Error())
				return
			}
			mobileJSON(w, http.StatusOK, map[string]string{"state": "submitted", "note": "模组已接受短信，尚未确认收件人送达；请勿自动重发"})
		default:
			mobileError(w, http.StatusNotFound, "API 路径不存在")
		}
	})
}

func mobileDecodeError(w http.ResponseWriter, err error) {
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		mobileError(w, http.StatusRequestEntityTooLarge, "短信请求过大")
		return
	}
	mobileError(w, http.StatusBadRequest, "JSON 格式无效")
}

// The caller invokes this only after the existing loopback web BasicAuth check.
// Credentials and QR bytes are generated on explicit requests and are never logged.
func handleMobilePairing(w http.ResponseWriter, r *http.Request, s *mobileService) bool {
	if r.URL.Path != "/api/mobile-pairing" && r.URL.Path != "/api/mobile-pairing/qr" {
		return false
	}
	if !mobileMethod(w, r, http.MethodGet) {
		return true
	}
	if s == nil || !s.Ready() {
		mobileError(w, http.StatusServiceUnavailable, "手机 HTTPS 接口未启用或尚未就绪")
		return true
	}
	if r.URL.Path == "/api/mobile-pairing" {
		mobileJSON(w, http.StatusOK, s.pairing)
		return true
	}
	payload, err := json.Marshal(s.pairing)
	if err != nil {
		mobileError(w, http.StatusInternalServerError, "无法生成配对信息")
		return true
	}
	png, err := qrcode.Encode(string(payload), qrcode.Medium, 384)
	if err != nil {
		mobileError(w, http.StatusInternalServerError, "无法生成配对二维码")
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
	return true
}
