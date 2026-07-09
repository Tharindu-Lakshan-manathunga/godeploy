package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"godeploy/internal/config"
	"godeploy/internal/store"
)

type contextKey string

const userContextKey = contextKey("godeploy.user")

func contextUser(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userContextKey).(store.User)
	return u, ok
}

type Manager struct {
	cfg        config.Auth
	st         *store.Store
	secret     []byte
	cookieName string
	ttl        time.Duration
}

func New(cfg config.Auth, st *store.Store) *Manager {
	secret := []byte(os.Getenv("GODEPLOY_SESSION_KEY"))
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}
	return &Manager{
		cfg:        cfg,
		st:         st,
		secret:     secret,
		cookieName: cfg.SessionCookieNameOrDefault(),
		ttl:        cfg.SessionTTL(),
	}
}

func (m *Manager) CookieName() string {
	return m.cookieName
}

func (m *Manager) bootstrapUsers() error {
	for _, item := range m.cfg.BootstrapUsers {
		if item.Username == "" || item.Password == "" {
			continue
		}
		if _, ok := m.st.GetUser(item.Username); ok {
			continue
		}
		salt, err := generateSalt()
		if err != nil {
			return err
		}
		hash := m.hashPassword(item.Password, salt)
		if err := m.st.SaveUser(store.User{Username: item.Username, PasswordHash: hash, Salt: salt, Role: item.Role}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) LoadBootstrapUsers() error {
	if len(m.cfg.BootstrapUsers) == 0 {
		return nil
	}
	return m.bootstrapUsers()
}

func (m *Manager) hashPassword(password, salt string) string {
	seed := []byte(password + salt)
	h := sha256.Sum256(seed)
	for i := 0; i < 100000; i++ {
		h = sha256.Sum256(h[:])
	}
	return hex.EncodeToString(h[:])
}

func (m *Manager) generateSalt() (string, error) {
	return generateSalt()
}

func generateSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (m *Manager) CreateUser(username, password, role string) error {
	if username == "" || password == "" {
		return fmt.Errorf("username and password are required")
	}
	if _, ok := m.st.GetUser(username); ok {
		return fmt.Errorf("user already exists")
	}
	salt, err := generateSalt()
	if err != nil {
		return err
	}
	hash := m.hashPassword(password, salt)
	return m.st.SaveUser(store.User{Username: username, PasswordHash: hash, Salt: salt, Role: role})
}

func (m *Manager) Authenticate(username, password string) (string, error) {
	user, ok := m.st.GetUser(username)
	if !ok {
		return "", fmt.Errorf("invalid credentials")
	}
	if user.PasswordHash != m.hashPassword(password, user.Salt) {
		return "", fmt.Errorf("invalid credentials")
	}
	return m.makeSessionToken(username)
}

func (m *Manager) makeSessionToken(username string) (string, error) {
	expires := time.Now().Add(m.ttl).Unix()
	payload := fmt.Sprintf("%s|%d", username, expires)
	sig := m.sign([]byte(payload))
	token := fmt.Sprintf("%s|%s", payload, hex.EncodeToString(sig))
	return base64.RawURLEncoding.EncodeToString([]byte(token)), nil
}

func (m *Manager) sign(data []byte) []byte {
	h := hmac.New(sha256.New, m.secret)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

func (m *Manager) validateSessionToken(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("invalid token")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid token")
	}
	username := parts[0]
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid token")
	}
	sig, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("invalid token")
	}
	payload := fmt.Sprintf("%s|%d", username, expires)
	if !hmac.Equal(sig, m.sign([]byte(payload))) {
		return "", fmt.Errorf("invalid token")
	}
	if time.Now().Unix() > expires {
		return "", fmt.Errorf("session expired")
	}
	return username, nil
}

func (m *Manager) UserFromRequest(r *http.Request) (store.User, bool) {
	cookie, err := r.Cookie(m.cookieName)
	if err == nil && cookie.Value != "" {
		username, err := m.validateSessionToken(cookie.Value)
		if err == nil {
			if user, ok := m.st.GetUser(username); ok {
				return user, true
			}
		}
	}

	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(authHeader, prefix) {
			token := strings.TrimPrefix(authHeader, prefix)
			username, err := m.validateSessionToken(token)
			if err == nil {
				if user, ok := m.st.GetUser(username); ok {
					return user, true
				}
			}
		}
	}

	if qToken := r.URL.Query().Get("token"); qToken != "" {
		username, err := m.validateSessionToken(qToken)
		if err == nil {
			if user, ok := m.st.GetUser(username); ok {
				return user, true
			}
		}
	}

	return store.User{}, false
}

func (m *Manager) SetSessionCookie(w http.ResponseWriter, token string) {
	cookie := &http.Cookie{
		Name:     m.cookieName,
		Value:    token,
		HttpOnly: true,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.ttl.Seconds()),
	}
	if os.Getenv("GODEPLOY_SESSION_SECURE") == "1" {
		cookie.Secure = true
	}
	http.SetCookie(w, cookie)
}

func (m *Manager) ClearSessionCookie(w http.ResponseWriter) {
	cookie := &http.Cookie{
		Name:     m.cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(w, cookie)
}

func (m *Manager) RequireAdmin(user store.User) bool {
	return user.Role == "admin"
}

func (m *Manager) UserFromContext(ctx context.Context) (store.User, bool) {
	return contextUser(ctx)
}

func (m *Manager) WithUserContext(r *http.Request, user store.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey, user))
}
