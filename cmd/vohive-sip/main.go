package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vohive/internal/cscall"
	"github.com/iniwex5/vohive/internal/desktopcall"
	"github.com/iniwex5/vohive/internal/notify"
	"github.com/iniwex5/vohive/internal/sipgw"
	"github.com/iniwex5/vohive/internal/winmodem"
	"github.com/iniwex5/vohive/pkg/logger"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const version = "1.0.0-fengxin"

//go:embed index.html
var indexHTML []byte

type config struct {
	Desktop             bool   `json:"desktop"`
	ATPort              string `json:"at_port"`
	AudioDevice         string `json:"audio_device"`
	SIPListen           string `json:"sip_listen"`
	SIPAdvertiseIP      string `json:"sip_advertise_ip"`
	SIPUsername         string `json:"sip_username"`
	SIPPassword         string `json:"sip_password"`
	WebListen           string `json:"web_listen"`
	WebPassword         string `json:"web_password"`
	MobileListen        string `json:"mobile_listen,omitempty"`
	MobileToken         string `json:"mobile_token,omitempty"`
	MobileCertFile      string `json:"mobile_cert_file,omitempty"`
	MobileKeyFile       string `json:"mobile_key_file,omitempty"`
	MobileAdvertiseHost string `json:"mobile_advertise_host,omitempty"`
}

func main() {
	configFile := flag.String("config", "config.json", "Configuration file")
	flag.Parse()
	raw, err := os.ReadFile(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	var cfg config
	if err = json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.ATPort == "" || cfg.AudioDevice == "" || len(cfg.SIPPassword) < 12 || len(cfg.WebPassword) < 8 {
		log.Fatal("AT port, audio endpoint, SIP password >=12 characters and web password >=8 characters are required")
	}
	wh, _, err := net.SplitHostPort(cfg.WebListen)
	if err != nil || net.ParseIP(wh) == nil || !net.ParseIP(wh).IsLoopback() {
		log.Fatal("web_listen must use a loopback IP")
	}
	if net.ParseIP(cfg.SIPAdvertiseIP) == nil {
		log.Fatal("sip_advertise_ip must be an IP address")
	}
	if cfg.Desktop {
		cfg.MobileListen = ""
	}
	mobile, err := newMobileService(cfg, filepath.Dir(*configFile))
	if err != nil {
		log.Fatalf("Android HTTPS API configuration: %v", err)
	}
	logger.Setup(logger.LogConfig{Debug: false, Filename: "logs/gateway.log", MaxAge: 7})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dataDir := filepath.Join(filepath.Dir(*configFile), "data")
	if err := preparePrivateDataDir(dataDir); err != nil {
		log.Fatalf("Protect local messages and notification storage: %v", err)
	}
	openNotifications := notify.New
	if cfg.Desktop {
		openNotifications = notify.NewDesktop
	}
	notifications, err := openNotifications(filepath.Join(dataDir, "notifications"))
	if err != nil {
		log.Fatalf("Open notification storage: %v", err)
	}
	var notificationError atomic.Value
	notificationError.Store("")
	go func() {
		if err := notifications.Run(ctx); err != nil {
			notificationError.Store("邮件队列已停止：无法保存投递状态，请检查磁盘后重启网关")
			log.Printf("Notification queue stopped due to persistence failure: %v", err)
		}
	}()
	modem, err := winmodem.Open(cfg.ATPort)
	if err != nil {
		if !cfg.Desktop {
			log.Fatalf("Open modem: %v", err)
		}
		log.Printf("EC20 disconnected; keeping desktop services available: %v", err)
		modem = winmodem.NewDisconnected(cfg.ATPort, err)
	}
	defer modem.Close()
	if modem.Snapshot().Connected {
		if err = modem.Init(); err != nil {
			if !cfg.Desktop {
				log.Fatal(err)
			}
			modem.Close()
			log.Printf("EC20 initialization stopped: %v", err)
		}
		go modem.Monitor(ctx)
	}
	if err = modem.StartSMSReceiver(ctx, filepath.Join(dataDir, "sms"), func(s winmodem.IncomingSMS) error {
		_, err := notifications.Enqueue(notify.Event{ID: "sms-" + s.ID, Type: "incoming_sms", Number: s.Number, Title: "蜂信 收到短信", Body: fmt.Sprintf("发件号码：%s\n收到时间：%s\n\n%s", s.Number, s.ReceivedAt.Local().Format("2006-01-02 15:04:05"), s.Text), OccurredAt: s.ReceivedAt})
		return err
	}); err != nil {
		log.Printf("SMS receiver initialization: %v", err)
	}
	sipCfg := sipgw.DefaultConfig()
	sipCfg.Enabled = true
	sipCfg.SIP.Listen = cfg.SIPListen
	sipCfg.SIP.ExternalIP = cfg.SIPAdvertiseIP
	sipCfg.SIP.Realm = "vohive.local"
	sipCfg.Users = []sipgw.UserConfig{{Username: cfg.SIPUsername, Password: cfg.SIPPassword, DisplayName: "EC20", DeviceID: "ec20"}}
	if err = sipCfg.Validate(); err != nil {
		log.Fatal(err)
	}
	registrar, err := sipgw.NewRegistrar(sipCfg)
	if err != nil {
		log.Fatal(err)
	}
	var desktop *desktopcall.Manager
	if cfg.Desktop {
		desktop = desktopcall.New(modem, cfg.AudioDevice, dataDir)
		defer desktop.Stop()
	} else {
		manager := cscall.NewManagerWithController("ec20", cfg.AudioDevice, cscall.NewATController(modem), registrar)
		manager.SetCallObserver(func(e cscall.IncomingCallEvent) error {
			number := e.Number
			if number == "" {
				number = "未提供来电号码"
			}
			title := "蜂信 来电提醒"
			body := fmt.Sprintf("来电号码：%s\n来电时间：%s", number, e.ReceivedAt.Local().Format("2006-01-02 15:04:05"))
			if e.Type == "ended" {
				title = "蜂信 来电结果"
				outcome := map[string]string{"answered": "已接听", "missed": "未接听", "rejected": "已拒接", "failed": "呼叫处理失败"}[e.Outcome]
				if outcome == "" {
					outcome = e.Outcome
				}
				body += fmt.Sprintf("\n处理结果：%s\n结束时间：%s\n接听时长：%d 秒", outcome, e.EndedAt.Local().Format("2006-01-02 15:04:05"), e.DurationSeconds)
			}
			_, err := notifications.Enqueue(notify.Event{ID: "call-" + e.ID + "-" + e.Type, Type: "incoming_call", Number: e.Number, Title: title, Body: body, OccurredAt: e.ReceivedAt})
			return err
		})
		defer func() {
			manager.Stop()
			flushCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			if err := manager.WaitCallObservers(flushCtx); err != nil {
				log.Printf("Call event persistence: %v", err)
			}
			registrar.Stop()
		}()
		registrar.SetOnInvite(manager.HandleOutboundInvite)
		registrar.SetOnBye(func(device string, req *sip.Request, tx sip.ServerTransaction) {
			if req.CallID() != nil {
				manager.HandleClientBye(req.CallID().Value())
			}
		})
		registrar.SetOnCancel(func(device string, req *sip.Request, tx sip.ServerTransaction) {
			tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
			if req.CallID() != nil {
				manager.HandleClientCancel(req.CallID().Value())
			}
		})
		registrar.SetOnPrack(func(_ string, req *sip.Request, tx sip.ServerTransaction) {
			tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		})
		registrar.SetOnMessage(func(_ string, number, text string) error { return modem.SendSMS(number, text) })
		if err = registrar.Start(ctx); err != nil {
			log.Fatal(err)
		}

	}
	if mobile != nil {
		if err = mobile.Start(ctx, modem, registrar, notifications, func(err error) {
			log.Printf("Android HTTPS API stopped: %v", err)
			cancel()
		}); err != nil {
			log.Fatalf("Android HTTPS API startup: %v", err)
		}
		defer mobile.Close()
	}
	network := &networkService{modem: modem}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		user, pw, ok := r.BasicAuth()
		if !ok || user != "admin" || subtle.ConstantTimeCompare([]byte(pw), []byte(cfg.WebPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="蜂信"`)
			http.Error(w, "Authentication required", 401)
			return
		}
		if r.Method != "GET" {
			origin := r.Header.Get("Origin")
			if origin != "http://"+r.Host {
				http.Error(w, "Same-origin request required", 403)
				return
			}
		}
		if desktop != nil && strings.HasPrefix(r.URL.Path, "/api/desktop/") {
			desktop.ServeHTTP(w, r)
			return
		}
		if handleNotifications(w, r, notifications) {
			return
		}
		if handleMobilePairing(w, r, mobile) {
			return
		}
		switch r.URL.Path {
		case "/api/network":
			network.ServeHTTP(w, r)
		case "/":
			if r.Method != "GET" {
				http.Error(w, "Method", 405)
				return
			}
			w.Header().Set("Content-Type", "text/html;charset=utf-8")
			w.Write(indexHTML)
		case "/api/status":
			if r.Method != "GET" {
				http.Error(w, "Method", 405)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"version": version, "desktop": cfg.Desktop, "modem": modem.Snapshot(), "sip_listen": cfg.SIPListen, "sip_username": cfg.SIPUsername, "sip_registered": registrar.CountRegistered(), "audio_device": cfg.AudioDevice, "notification_error": notificationError.Load(), "mobile_enabled": mobile != nil, "mobile_ready": mobile != nil && mobile.Ready(), "transport": "Cellular AT + USB Audio ↔ SIP/RTP PCMU"})
		case "/api/sip-settings":
			if r.Method != "POST" {
				http.Error(w, "Method", 405)
				return
			}
			_, port, _ := net.SplitHostPort(cfg.SIPListen)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"host": cfg.SIPAdvertiseIP, "port": port, "username": cfg.SIPUsername, "password": cfg.SIPPassword, "realm": "vohive.local", "transport": "UDP", "codec": "PCMU / G.711 μ-law (8000 Hz)"})
		case "/api/sms":
			if r.Method != "POST" {
				http.Error(w, "Method", 405)
				return
			}
			var body struct {
				Number string `json:"number"`
				Text   string `json:"text"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				http.Error(w, "Invalid JSON", 400)
				return
			}
			if err := modem.SendSMS(body.Number, body.Text); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"state":"submitted","note":"Modem accepted; recipient delivery not yet verified"}`)
		case "/api/inbox":
			if r.Method != "GET" {
				http.Error(w, "Method", 405)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"messages": modem.SMSInbox(), "receiver": modem.SMSReceiverStatus()})
		case "/api/shutdown":
			if r.Method != "POST" {
				http.Error(w, "Method", 405)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			cancel()
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{Addr: cfg.WebListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 70 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		server.Shutdown(shutdownCtx)
	}()
	log.Printf("蜂信 Mac %s: web http://%s; SIP %s; AT %s", version, cfg.WebListen, cfg.SIPListen, cfg.ATPort)
	if err = server.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "Server closed") {
		log.Fatal(err)
	}
}
