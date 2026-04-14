package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const csrfCookieName = "burkebot_csrf"

type basicAuthConfig struct {
	Username string
	Password string
}

func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func secureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if s.auth == nil {
		return true
	}

	username, password, ok := r.BasicAuth()
	if ok && secureCompare(username, s.auth.Username) && secureCompare(password, s.auth.Password) {
		return true
	}

	w.Header().Set("WWW-Authenticate", `Basic realm="burkebot", charset="UTF-8"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
	return false
}

func (s *Server) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	if len(s.csrfKey) == 0 {
		return "", nil
	}

	if cookie, err := r.Cookie(csrfCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		return cookie.Value, nil
	}

	secret, err := randomToken(32)
	if err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	return secret, nil
}

func (s *Server) signCSRF(secret, action string) string {
	if len(s.csrfKey) == 0 {
		return ""
	}

	mac := hmac.New(sha256.New, s.csrfKey)
	io.WriteString(mac, secret)
	io.WriteString(mac, "\n")
	io.WriteString(mac, action)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request, action string) string {
	secret, err := s.ensureCSRFCookie(w, r)
	if err != nil {
		s.logger.Error("failed to prepare csrf token", "error", err, "action", action)
		return ""
	}
	return s.signCSRF(secret, action)
}

func (s *Server) verifyCSRF(w http.ResponseWriter, r *http.Request, action string) bool {
	if len(s.csrfKey) == 0 {
		return true
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return false
	}

	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}

	expected := s.signCSRF(cookie.Value, action)
	actual := strings.TrimSpace(r.FormValue("csrf_token"))
	if actual == "" || !secureCompare(actual, expected) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}

	return true
}
