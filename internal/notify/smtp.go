package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

func sendSMTP(ctx context.Context, cfg Settings, event Event) deliveryResult {
	return sendSMTPWithTLS(ctx, cfg, event, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
}

func sendSMTPWithTLS(ctx context.Context, cfg Settings, event Event, tlsConfig *tls.Config) deliveryResult {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return smtpFailure("连接 SMTP 服务器", err, false)
	}
	defer conn.Close()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	if cfg.Security == "tls" {
		tlsConn := tls.Client(conn, tlsConfig)
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			return smtpFailure("验证 SMTP TLS 连接", err, false)
		}
		conn = tlsConn
	} else if cfg.Security != "starttls" {
		return deliveryResult{Status: "failed", Detail: "SMTP 加密方式无效"}
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return smtpFailure("读取 SMTP 欢迎信息", err, false)
	}
	defer client.Close()
	if cfg.Security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return deliveryResult{Status: "failed", Detail: "服务器未提供 STARTTLS，已停止发送以保护邮箱授权码"}
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return smtpFailure("验证 STARTTLS 连接", err, false)
		}
	}
	if state, ok := client.TLSConnectionState(); !ok || !state.HandshakeComplete {
		return deliveryResult{Status: "failed", Detail: "SMTP 连接未完成 TLS 验证"}
	}
	var auth smtp.Auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	if ok, mechanisms := client.Extension("AUTH"); ok {
		available := " " + strings.ToUpper(mechanisms) + " "
		if !strings.Contains(available, " PLAIN ") && strings.Contains(available, " LOGIN ") {
			auth = &loginAuth{username: cfg.Username, password: cfg.Password}
		}
	}
	if err = client.Auth(auth); err != nil {
		return smtpFailure("SMTP 身份验证", err, false)
	}
	if err = client.Mail(cfg.From); err != nil {
		return smtpFailure("提交发件邮箱", err, false)
	}
	if err = client.Rcpt(cfg.To); err != nil {
		return smtpFailure("提交收件邮箱", err, false)
	}
	writer, err := client.Data()
	if err != nil {
		return smtpFailure("开始投递邮件", err, false)
	}
	if _, err = writer.Write(composeMessage(cfg, event)); err != nil {
		return smtpFailure("上传邮件正文", err, true)
	}
	if err = writer.Close(); err != nil {
		return smtpFailure("确认邮件投递", err, true)
	}
	// DATA's final 250 is the acceptance boundary. QUIT failures cannot revoke acceptance.
	_ = client.Quit()
	return deliveryResult{Status: "sent", Detail: "SMTP 服务器已接受邮件；最终入箱以收件邮箱为准"}
}

type loginAuth struct {
	username, password string
	step               int
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("LOGIN requires TLS")
	}
	a.step = 0
	return "LOGIN", nil, nil
}
func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	default:
		return nil, errors.New("unexpected LOGIN challenge")
	}
}

func smtpFailure(stage string, err error, dataStarted bool) deliveryResult {
	var protocol *textproto.Error
	if errors.As(err, &protocol) {
		return deliveryResult{Status: "failed", Detail: fmt.Sprintf("%s失败（SMTP %d）", stage, protocol.Code), Retry: protocol.Code >= 400 && protocol.Code < 500}
	}
	if dataStarted {
		return deliveryResult{Status: "uncertain", Detail: stage + "时连接中断，邮件可能已被接受；为避免重复通知，不自动重发"}
	}
	var network net.Error
	retry := errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
	return deliveryResult{Status: "failed", Detail: stage + "失败，请检查服务器地址、网络和 TLS 证书", Retry: retry}
}

func composeMessage(cfg Settings, e Event) []byte {
	var result bytes.Buffer
	digest := sha256.Sum256([]byte(e.ID))
	fmt.Fprintf(&result, "From: <%s>\r\nTo: <%s>\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@vohive.local>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", cfg.From, cfg.To, mime.QEncoding.Encode("UTF-8", e.Title), e.OccurredAt.Format(time.RFC1123Z), hex.EncodeToString(digest[:]))
	w := quotedprintable.NewWriter(&result)
	body := e.Body
	if e.Number != "" {
		body = "号码：" + e.Number + "\n" + body
	}
	body = "时间：" + e.OccurredAt.Local().Format("2006-01-02 15:04:05 -0700") + "\n" + body
	_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
	_ = w.Close()
	return result.Bytes()
}
