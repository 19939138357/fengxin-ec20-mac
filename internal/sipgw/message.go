package sipgw

import (
	"context"
	"fmt"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"strings"
	"time"
)

// A registered identity is bound to the authenticated source endpoint.
func (r *Registrar) authorizedSource(req *sip.Request) bool {
	if req.From() == nil {
		return false
	}
	user := r.GetUserByUsername(req.From().Address.User)
	return user != nil && user.Source == req.Source() && strings.EqualFold(user.Transport, req.Transport()) && time.Now().Before(user.Expires)
}
func (r *Registrar) SetOnMessage(fn func(string, string, string) error) {
	r.mu.Lock()
	r.onMessage = fn
	r.mu.Unlock()
}
func (r *Registrar) handleMessage(req *sip.Request, tx sip.ServerTransaction) {
	if !r.authorizedSource(req) {
		r.respond(tx, req, 403, "Forbidden")
		return
	}
	content := req.GetHeader("Content-Type")
	if content == nil || !strings.HasPrefix(strings.ToLower(content.Value()), "text/plain") {
		r.respond(tx, req, 415, "Use text/plain")
		return
	}
	if len(req.Body()) == 0 || len(req.Body()) > 2048 {
		r.respond(tx, req, 400, "Invalid Message Size")
		return
	}
	user := r.GetUserByUsername(req.From().Address.User)
	r.mu.RLock()
	fn := r.onMessage
	r.mu.RUnlock()
	if fn == nil {
		r.respond(tx, req, 501, "SMS Unavailable")
		return
	}
	if err := fn(user.DeviceID, req.Recipient.User, string(req.Body())); err != nil {
		r.respond(tx, req, 503, "SMS Not Submitted")
		return
	}
	// 200 only means the modem accepted the submission, not recipient delivery.
	r.respond(tx, req, 200, "OK")
}
func (r *Registrar) SendMessage(ctx context.Context, device, from, text string) error {
	user := r.GetUserByDevice(device)
	if user == nil || user.Source == "" || time.Now().After(user.Expires) {
		return fmt.Errorf("SIP client offline")
	}
	var uri sip.Uri
	if err := sip.ParseUri(user.ContactURI, &uri); err != nil {
		return err
	}
	req := sip.NewRequest(sip.MESSAGE, uri)
	host, port, _ := strings.Cut(r.cfg.SIP.Listen, ":")
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s:%s>;tag=%s", from, host, port, sip.GenerateTagN(12))))
	req.AppendHeader(sip.NewHeader("To", "<"+uri.String()+">"))
	req.AppendHeader(sip.NewHeader("Content-Type", "text/plain;charset=utf-8"))
	req.SetBody([]byte(text))
	req.SetTransport(user.Transport)
	req.SetDestination(user.Source)
	tx, err := r.client.TransactionRequest(ctx, req, sipgo.ClientRequestBuild)
	if err != nil {
		return err
	}
	defer tx.Terminate()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res := <-tx.Responses():
			if res != nil && res.StatusCode >= 200 {
				if res.StatusCode >= 300 {
					return fmt.Errorf("SIP MESSAGE rejected: %d", res.StatusCode)
				}
				return nil
			}
		case <-tx.Done():
			return fmt.Errorf("MESSAGE transaction ended")
		}
	}
}
