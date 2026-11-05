package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSMTP struct {
	address   string
	tlsConfig *tls.Config
	authCalls atomic.Int32
	data      chan string
	done      chan struct{}
}

func newFakeSMTP(t *testing.T, security, mode string) *fakeSMTP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	parsed, _ := x509.ParseCertificate(der)
	pool.AddCert(parsed)
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{address: listener.Addr().String(), tlsConfig: &tls.Config{ServerName: "localhost", RootCAs: pool, MinVersion: tls.VersionTLS12}, data: make(chan string, 1), done: make(chan struct{})}
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			t.Error("fake SMTP did not stop")
		}
	})
	go func() {
		defer close(s.done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if security == "tls" {
			conn = tls.Server(conn, serverTLS)
		}
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		reply := func(text string) { _, _ = writer.WriteString(text + "\r\n"); _ = writer.Flush() }
		reply("220 localhost ESMTP")
		loginStep := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if loginStep > 0 {
				decoded, err := base64.StdEncoding.DecodeString(line)
				if err != nil {
					t.Error("invalid LOGIN response")
					return
				}
				if loginStep == 1 {
					if string(decoded) != "user" {
						t.Error("wrong LOGIN username")
					}
					loginStep = 2
					reply("334 UGFzc3dvcmQ6")
				} else {
					if string(decoded) != "secret" {
						t.Error("wrong LOGIN password")
					}
					loginStep = 0
					reply("235 authenticated")
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				reply("250-localhost")
				if security == "starttls" && mode != "no-starttls" {
					reply("250-STARTTLS")
				}
				if mode == "login" {
					reply("250 AUTH LOGIN")
				} else {
					reply("250 AUTH PLAIN")
				}
			case line == "STARTTLS":
				reply("220 Ready for TLS")
				conn = tls.Server(conn, serverTLS)
				if err := conn.(*tls.Conn).Handshake(); err != nil {
					return
				}
				reader = bufio.NewReader(conn)
				writer = bufio.NewWriter(conn)
			case strings.HasPrefix(line, "AUTH"):
				s.authCalls.Add(1)
				if _, ok := conn.(*tls.Conn); !ok {
					t.Error("credentials sent without TLS")
				}
				if mode == "login" {
					if line != "AUTH LOGIN" {
						t.Error("LOGIN-only server got wrong mechanism")
					}
					loginStep = 1
					reply("334 VXNlcm5hbWU6")
				} else {
					reply("235 authenticated")
				}
			case strings.HasPrefix(line, "MAIL FROM"):
				reply("250 sender accepted")
			case strings.HasPrefix(line, "RCPT TO"):
				if mode == "rcpt-reject" {
					reply("550 no such mailbox")
				} else {
					reply("250 recipient accepted")
				}
			case line == "DATA":
				reply("354 Send content")
				data, err := io.ReadAll(textproto.NewReader(reader).DotReader())
				if err != nil {
					return
				}
				s.data <- string(data)
				if mode == "wait-final" {
					_, _ = reader.ReadString('\n')
					return
				}
				if mode == "disconnect" {
					return
				}
				if mode == "data-defer" {
					reply("451 temporary failure")
				} else {
					reply("250 queued")
				}
			case line == "QUIT":
				reply("221 goodbye")
				return
			default:
				reply("500 unsupported")
			}
		}
	}()
	return s
}

func TestVerifiedSMTPDeliveryBoundaries(t *testing.T) {
	cases := []struct {
		security, mode, status string
		retry                  bool
	}{
		{"tls", "accept", "sent", false}, {"starttls", "accept", "sent", false}, {"tls", "login", "sent", false},
		{"tls", "disconnect", "uncertain", false}, {"tls", "rcpt-reject", "failed", false}, {"tls", "data-defer", "failed", true}, {"starttls", "no-starttls", "failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.security+"/"+tc.mode, func(t *testing.T) {
			server := newFakeSMTP(t, tc.security, tc.mode)
			host, portText, _ := net.SplitHostPort(server.address)
			port, _ := strconv.Atoi(portText)
			cfg := Settings{Host: host, Port: port, Security: tc.security, Username: "user", Password: "secret", From: "sender@example.com", To: "owner@example.com"}
			result := sendSMTPWithTLS(context.Background(), cfg, Event{ID: "fixed-id", Title: "收到短信", Body: "中文正文", OccurredAt: time.Now()}, server.tlsConfig)
			if result.Status != tc.status || result.Retry != tc.retry {
				t.Fatalf("result %+v", result)
			}
			if tc.mode == "no-starttls" && server.authCalls.Load() != 0 {
				t.Fatal("auth preceded TLS")
			}
			if tc.status == "sent" {
				select {
				case data := <-server.data:
					if !strings.Contains(data, "Message-ID:") {
						t.Fatal("missing stable message ID")
					}
				case <-time.After(time.Second):
					t.Fatal("no message")
				}
			}
		})
	}
}

func TestUntrustedTLSNeverAuthenticates(t *testing.T) {
	s := newFakeSMTP(t, "tls", "accept")
	host, portText, _ := net.SplitHostPort(s.address)
	port, _ := strconv.Atoi(portText)
	result := sendSMTP(context.Background(), Settings{Host: host, Port: port, Security: "tls", Username: "user", Password: "secret"}, Event{})
	if result.Status != "failed" || s.authCalls.Load() != 0 {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestMessageMIMEAndStableID(t *testing.T) {
	c := Settings{From: "sender@example.com", To: "owner@example.com"}
	e := Event{ID: "same-id", Number: "12345", Title: "收到新的中文短信", Body: "第一行\n第二行", OccurredAt: time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)}
	first := composeMessage(c, e)
	second := composeMessage(c, e)
	if !bytes.Equal(first, second) {
		t.Fatal("message changed across attempts")
	}
	m, err := mail.ReadMessage(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subject != e.Title {
		t.Fatal("Chinese subject not encoded")
	}
	body, err := io.ReadAll(quotedprintable.NewReader(m.Body))
	if err != nil || !strings.Contains(string(body), "第二行") || !strings.Contains(string(body), "12345") {
		t.Fatal("Chinese body lost")
	}
}

func TestDisableDuringDATAIsUncertainAndNotRetried(t *testing.T) {
	server := newFakeSMTP(t, "tls", "wait-final")
	host, portText, _ := net.SplitHostPort(server.address)
	port, _ := strconv.Atoi(portText)
	s := configuredService(t)
	if _, err := s.Configure(Settings{Enabled: true, Host: host, Port: port, Security: "tls", Username: "user", Password: "secret", From: "sender@example.com", To: "owner@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(Event{ID: "during-data", Type: "incoming_sms", Title: "短信", Body: "中文"}); err != nil {
		t.Fatal(err)
	}
	s.send = func(ctx context.Context, c Settings, e Event) deliveryResult {
		return sendSMTPWithTLS(ctx, c, e, server.tlsConfig)
	}
	done := make(chan error, 1)
	go func() { _, err := s.process(context.Background()); done <- err }()
	select {
	case <-server.data:
	case <-time.After(2 * time.Second):
		t.Fatal("SMTP did not receive body")
	}
	if _, err := s.Configure(Settings{Enabled: false, Host: host, Port: port, Security: "tls", Username: "user", From: "sender@example.com", To: "owner@example.com"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("DATA delivery not cancelled")
	}
	if e := s.Events(1)[0]; e.Status != "uncertain" || !e.NextAttempt.IsZero() {
		t.Fatalf("ambiguous DATA not preserved: %+v", e)
	}
}
