// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

// Pairing a device. berth pair prints a link to GET /pair?code=..., which
// only shows a button: link previews in chat apps fetch a GET, and must not
// spend the code. The button's same-origin POST trades the code, through
// berthd, for a device token kept in an HttpOnly cookie. Reading the
// harbor needs no pairing; the actions (the switchboard) do.

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// cookieName is the device cookie. A year, like oh-my-pocket's sign-in.
const (
	cookieName = "berth_device"
	cookieAge  = 365 * 24 * 60 * 60
)

var pairPage = template.Must(template.New("pair").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark"><title>Pair this device - berth</title><link rel="stylesheet" href="/harbor.css"></head>
<body class="pair-page"><main class="pair">
<h1>{{.Title}}</h1><p>{{.Body}}</p>
{{- if .Code}}
<form method="post" action="/pair">
<input type="hidden" name="code" value="{{.Code}}">
<label>Name this device <input name="name" value="phone" maxlength="40" autocomplete="off"></label>
<button type="submit">Pair this device</button>
</form>
{{- end}}
<p><a href="/">Open the harbor</a></p>
</main></body></html>
`))

type pairView struct{ Title, Body, Code string }

func (h *Harbor) pairShow(w http.ResponseWriter, r *http.Request) {
	v := pairView{Title: "Pair this device",
		Body: "Pairing lets this browser switch and stop engines from the harbor. The code works once.",
		Code: r.URL.Query().Get("code")}
	if v.Code == "" {
		v = pairView{Title: "No pairing code", Body: "Run berth pair on the machine and open the link it prints."}
	}
	h.pairWrite(w, http.StatusOK, v)
}

func (h *Harbor) pairWrite(w http.ResponseWriter, status int, v pairView) {
	// The guard's CSP forbids every form; this page posts its one form here.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pairPage.Execute(w, v)
}

// crossSite is a form posted from another site, as the browser reports it.
func crossSite(r *http.Request) bool {
	s := r.Header.Get("Sec-Fetch-Site")
	return s != "" && s != "same-origin" && s != "none"
}

func (h *Harbor) pairRedeem(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		h.pairWrite(w, http.StatusForbidden, pairView{Title: "Not paired", Body: "Pair from the harbor's own page."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		h.pairWrite(w, http.StatusBadRequest, pairView{Title: "Not paired", Body: "The form did not arrive whole; open the link again."})
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if len(name) > 40 {
		name = name[:40]
	}
	m, err := h.ask(proto.Msg{Op: proto.OpPairRedeem, Code: strings.ToUpper(strings.TrimSpace(r.PostForm.Get("code"))), DeviceName: name})
	switch {
	case err != nil:
		h.pairWrite(w, http.StatusBadGateway, pairView{Title: "Not paired", Body: "berthd is not answering: " + err.Error()})
		return
	case m.Op == proto.OpRefused:
		h.pairWrite(w, http.StatusForbidden, pairView{Title: "Not paired", Body: m.Reason + ". Run berth pair for a new link."})
		return
	}
	c := &http.Cookie{Name: cookieName, Value: m.Token, Path: "/", MaxAge: cookieAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure(r)}
	http.SetCookie(w, c)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// secure is whether the browser reached the harbor over https: directly, or
// through a TLS-terminating proxy on this machine (tailscale serve) that
// says so. The cookie is then never sent over plain http.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}

// paired is whether the request carries a paired device's token, in the
// cookie or as a bearer token for scripts. berthd decides.
func (h *Harbor) paired(r *http.Request) bool {
	tok := ""
	if c, err := r.Cookie(cookieName); err == nil {
		tok = c.Value
	} else if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		tok = strings.TrimSpace(b)
	}
	if tok == "" {
		return false
	}
	m, err := h.ask(proto.Msg{Op: proto.OpPairCheck, Token: tok})
	return err == nil && m.Paired
}

// me tells the page whether this browser may act, so it shows the actions
// only to a paired device.
func (h *Harbor) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Paired bool `json:"paired"`
	}{h.paired(r)})
}

// ask sends one message to berthd and reads its one reply.
func (h *Harbor) ask(req proto.Msg) (proto.Msg, error) {
	conn, raw, err := server.Dial(h.Socket)
	if err != nil {
		return proto.Msg{}, err
	}
	defer raw.Close()
	if err := conn.Send(req); err != nil {
		return proto.Msg{}, err
	}
	return conn.Recv()
}
