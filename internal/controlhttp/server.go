// Package controlhttp exposes the narrow loopback dashboard control surface.
package controlhttp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gshepptech/interlock/internal/control"
	"github.com/gshepptech/interlock/internal/dashboard"
	"github.com/gshepptech/interlock/internal/diagnostic"
	"github.com/gshepptech/interlock/internal/handoffrequest"
)

const (
	maxBody     = 8 * 1024
	maxFlashes  = 64
	flashExpiry = time.Minute
)

type Controller interface {
	ClearStaleSession(context.Context, control.Actor, string, string) (control.Result, error)
	ReleaseSessionLeases(context.Context, control.Actor, string, string) (control.Result, error)
	CheckInstallation(context.Context) (control.Result, error)
	VerifyInstallation(context.Context, control.Actor) (control.Result, error)
	RepairInstallation(context.Context, control.Actor, string, bool) (control.Result, error)
	RequestHandoff(context.Context, control.Actor, handoffrequest.Create) (control.Result, error)
	ResolveHandoff(context.Context, control.Actor, string, string) (control.Result, error)
}

type Load func() (dashboard.View, []handoffrequest.Item, error)

type flash struct {
	message string
	expires time.Time
}

type Server struct {
	controller Controller
	load       Load
	authority  string
	csrf       string
	now        func() time.Time
	newToken   func(int) (string, error)

	mu         sync.Mutex
	flashes    map[string]flash
	diagnostic *diagnostic.Report
}

func New(controller Controller, authority string, load Load) (*Server, error) {
	if controller == nil || load == nil {
		return nil, errors.New("control HTTP controller and loader are required")
	}
	if !validLoopbackAuthority(authority) {
		return nil, fmt.Errorf("control HTTP authority %q is not an IP-literal loopback address", authority)
	}
	token, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	return &Server{
		controller: controller, load: load, authority: authority, csrf: token,
		now: time.Now, newToken: randomToken, flashes: map[string]flash{},
	}, nil
}

func (s *Server) CSRFToken() string { return s.csrf }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setBaseHeaders(w)
	nonce, err := randomToken(18)
	if err != nil {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		http.Error(w, "cannot prepare response", http.StatusInternalServerError)
		return
	}
	s.setContentSecurityPolicy(w, nonce)
	if r.Host != s.authority || !validLoopbackAuthority(r.Host) {
		http.Error(w, "invalid dashboard host", http.StatusBadRequest)
		return
	}
	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		s.servePage(w, r, nonce)
	case r.URL.Path == "/api/status" && r.Method == http.MethodGet:
		s.serveStatus(w, nonce)
	case strings.HasPrefix(r.URL.Path, "/api/actions/"):
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveAction(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) servePage(w http.ResponseWriter, r *http.Request, nonce string) {
	view, requests, err := s.load()
	if err != nil {
		http.Error(w, "cannot read coordination state", http.StatusInternalServerError)
		return
	}
	message := ""
	if id := r.URL.Query().Get("flash"); id != "" {
		if r.Header.Get("Sec-Fetch-Mode") == "cors" {
			message = s.peekFlash(id)
		} else {
			message = s.takeFlash(id)
		}
	}
	controlled := dashboard.Controls(view, s.csrf, nonce, message, requests, s.latestDiagnostic())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboard.RenderControlPage(w, controlled)
}

func (s *Server) serveStatus(w http.ResponseWriter, nonce string) {
	view, requests, err := s.load()
	if err != nil {
		http.Error(w, "cannot read coordination state", http.StatusInternalServerError)
		return
	}
	controlled := dashboard.Controls(view, s.csrf, nonce, "", requests, s.latestDiagnostic())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboard.RenderControlStatus(w, controlled)
}

type actionSpec struct {
	required []string
	optional []string
	run      func(context.Context, url.Values) (control.Result, error)
}

func (s *Server) serveAction(w http.ResponseWriter, r *http.Request) {
	if !s.validOrigin(r) {
		http.Error(w, "invalid dashboard origin", http.StatusForbidden)
		return
	}
	spec, ok := s.action(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	values, status, err := parseForm(w, r, spec.required, spec.optional)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if !s.validCSRF(r.Header.Values("X-Interlock-CSRF"), values["csrf"]) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if err := validateActionFields(r.URL.Path, values); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, runErr := spec.run(r.Context(), values)
	if result.Diagnostic != nil {
		s.mu.Lock()
		copy := *result.Diagnostic
		s.diagnostic = &copy
		s.mu.Unlock()
	}
	if runErr != nil || !result.Successful() {
		status := statusFor(runErr)
		if runErr == nil {
			status = http.StatusInternalServerError
		}
		http.Error(w, safeMessage(result, runErr), status)
		return
	}
	message := result.Message
	if result.Warning != "" {
		message += "; warning: " + result.Warning
	}
	location := "/"
	if id := s.putFlash(message); id != "" {
		location += "?flash=" + url.QueryEscape(id)
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (s *Server) action(path string) (actionSpec, bool) {
	csrf := []string{"csrf"}
	switch path {
	case "/api/actions/session-clear":
		return actionSpec{required: append(csrf, "session_id", "confirmation"), run: func(ctx context.Context, v url.Values) (control.Result, error) {
			return s.controller.ClearStaleSession(ctx, control.ActorDashboard, v.Get("session_id"), v.Get("confirmation"))
		}}, true
	case "/api/actions/lease-release":
		return actionSpec{required: append(csrf, "session_id", "confirmation"), run: func(ctx context.Context, v url.Values) (control.Result, error) {
			return s.controller.ReleaseSessionLeases(ctx, control.ActorDashboard, v.Get("session_id"), v.Get("confirmation"))
		}}, true
	case "/api/actions/doctor-check":
		return actionSpec{required: csrf, run: func(ctx context.Context, _ url.Values) (control.Result, error) {
			return s.controller.CheckInstallation(ctx)
		}}, true
	case "/api/actions/doctor-smoke":
		return actionSpec{required: csrf, run: func(ctx context.Context, _ url.Values) (control.Result, error) {
			return s.controller.VerifyInstallation(ctx, control.ActorDashboard)
		}}, true
	case "/api/actions/doctor-repair":
		return actionSpec{required: append(csrf, "confirmation"), optional: []string{"smoke"}, run: func(ctx context.Context, v url.Values) (control.Result, error) {
			return s.controller.RepairInstallation(ctx, control.ActorDashboard, v.Get("confirmation"), v.Get("smoke") == "1")
		}}, true
	case "/api/actions/handoff-request":
		return actionSpec{required: append(csrf, "source_session_id", "target_kind", "target", "note"), run: func(ctx context.Context, v url.Values) (control.Result, error) {
			return s.controller.RequestHandoff(ctx, control.ActorDashboard, handoffrequest.Create{
				SourceSession: v.Get("source_session_id"), TargetKind: handoffrequest.TargetKind(v.Get("target_kind")),
				Target: v.Get("target"), Note: v.Get("note"),
			})
		}}, true
	case "/api/actions/handoff-resolve":
		return actionSpec{required: append(csrf, "request_id", "confirmation"), run: func(ctx context.Context, v url.Values) (control.Result, error) {
			return s.controller.ResolveHandoff(ctx, control.ActorDashboard, v.Get("request_id"), v.Get("confirmation"))
		}}, true
	default:
		return actionSpec{}, false
	}
}

func parseForm(w http.ResponseWriter, r *http.Request, required, optional []string) (url.Values, int, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" || len(params) != 0 {
		return nil, http.StatusUnsupportedMediaType, errors.New("unsupported content type")
	}
	body := http.MaxBytesReader(w, r.Body, maxBody)
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge, errors.New("request body too large")
		}
		return nil, http.StatusBadRequest, errors.New("cannot read request body")
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("malformed form")
	}
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key, list := range values {
		if !allowed[key] || len(list) != 1 {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid form field %q", key)
		}
	}
	for _, key := range required {
		if key == "csrf" && len(values[key]) == 0 {
			continue
		}
		if len(values[key]) != 1 {
			return nil, http.StatusBadRequest, fmt.Errorf("missing form field %q", key)
		}
	}
	if smoke := values.Get("smoke"); smoke != "" && smoke != "1" {
		return nil, http.StatusBadRequest, errors.New("smoke must equal 1")
	}
	return values, 0, nil
}

func validateActionFields(path string, values url.Values) error {
	switch path {
	case "/api/actions/session-clear", "/api/actions/lease-release":
		if !safeIdentifier(values.Get("session_id"), 128) {
			return errors.New("invalid session id")
		}
	case "/api/actions/handoff-request":
		if !safeIdentifier(values.Get("source_session_id"), 128) {
			return errors.New("invalid source session id")
		}
		if kind := values.Get("target_kind"); kind != "agent" && kind != "team" {
			return errors.New("target kind must be agent or team")
		}
		if !safeIdentifier(values.Get("target"), 64) {
			return errors.New("invalid handoff target")
		}
		note := values.Get("note")
		if !utf8.ValidString(note) || len(note) > handoffrequest.MaxNote {
			return errors.New("invalid handoff note")
		}
	case "/api/actions/handoff-resolve":
		if !safeIdentifier(values.Get("request_id"), 128) {
			return errors.New("invalid handoff request id")
		}
	}
	return nil
}

func safeIdentifier(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("-_.:", r) {
			continue
		}
		return false
	}
	return true
}

func (s *Server) validCSRF(headers, form []string) bool {
	if len(headers) > 1 || len(form) > 1 || (len(headers) == 1 && len(form) == 1) {
		return false
	}
	provided := ""
	if len(headers) == 1 {
		provided = headers[0]
	} else if len(form) == 1 {
		provided = form[0]
	}
	return len(provided) == len(s.csrf) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(s.csrf)) == 1
}

func (s *Server) validOrigin(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" || len(r.Header.Values("Origin")) != 1 {
		return false
	}
	origin, err := url.Parse(raw)
	return err == nil && origin.Scheme == "http" && origin.Host == s.authority &&
		origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}

func validLoopbackAuthority(authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) setBaseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (s *Server) setContentSecurityPolicy(w http.ResponseWriter, nonce string) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+
		"'; style-src 'nonce-"+nonce+"'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'; img-src data:")
}

func statusFor(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	switch control.KindOf(err) {
	case control.Invalid:
		return http.StatusBadRequest
	case control.NotFound:
		return http.StatusNotFound
	case control.Conflict:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func safeMessage(result control.Result, err error) string {
	if err != nil && control.KindOf(err) == control.Internal {
		return "control action failed"
	}
	if result.Message != "" {
		return result.Message
	}
	return "control action rejected"
}

func (s *Server) latestDiagnostic() *diagnostic.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diagnostic == nil {
		return nil
	}
	copy := *s.diagnostic
	return &copy
}

func (s *Server) putFlash(message string) string {
	id, err := s.newToken(18)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneFlashesLocked()
	if len(s.flashes) >= maxFlashes {
		var oldestID string
		var oldest time.Time
		for candidate, value := range s.flashes {
			if oldestID == "" || value.expires.Before(oldest) {
				oldestID, oldest = candidate, value.expires
			}
		}
		delete(s.flashes, oldestID)
	}
	s.flashes[id] = flash{message: message, expires: s.now().Add(flashExpiry)}
	return id
}

func (s *Server) takeFlash(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneFlashesLocked()
	value, ok := s.flashes[id]
	if ok {
		delete(s.flashes, id)
		return value.message
	}
	return ""
}

func (s *Server) peekFlash(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneFlashesLocked()
	return s.flashes[id].message
}

func (s *Server) pruneFlashesLocked() {
	now := s.now()
	for id, value := range s.flashes {
		if !value.expires.After(now) {
			delete(s.flashes, id)
		}
	}
}

func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate control token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
