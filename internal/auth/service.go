// Package auth provides the bundled application's optional native and bearer authentication.
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"golang.org/x/time/rate"
)

const (
	sessionCookieName    = "webdesktop_session"
	maximumSecretBytes   = 4096
	minimumPasswordBytes = 8
	minimumBearerBytes   = 32
	maximumLoginClients  = 1024
	loginClientRetention = 15 * time.Minute
)

// Config contains resolved authentication settings.
type Config struct {
	LoginEnabled      bool
	PasswordFile      string
	BearerEnabled     bool
	BearerTokenFile   string
	SessionTTL        time.Duration
	SecureCookie      bool
	TrustedProxyCIDRs []string
}

// Service validates configured credentials and owns browser sessions.
type Service struct {
	loginEnabled   bool
	bearerEnabled  bool
	password       [sha256.Size]byte
	bearerToken    [sha256.Size]byte
	sessionTTL     time.Duration
	secureCookie   bool
	logger         *zap.Logger
	trustedProxies []netip.Prefix

	loginLimitersMu sync.Mutex
	loginLimiters   map[string]*loginClient
	sessionsMu      sync.Mutex
	sessions        map[[sha256.Size]byte]browserSession
}

type loginClient struct {
	limiter     *rate.Limiter
	lastAttempt time.Time
}

type browserSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	expiresAt time.Time
}

type sessionResponse struct {
	Version       int  `json:"version"`
	Required      bool `json:"required"`
	Authenticated bool `json:"authenticated"`
	LoginEnabled  bool `json:"login_enabled"`
	BearerEnabled bool `json:"bearer_enabled"`
}

type loginRequest struct {
	Credential string `json:"credential"`
}

type errorResponse struct {
	Error protocolError `json:"error"`
}

type protocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// New loads configured credentials and constructs an authentication service.
func New(cfg Config, logger *zap.Logger) (*Service, error) {
	if len(cfg.TrustedProxyCIDRs) > 32 {
		return nil, errors.New("trusted login proxy CIDRs must contain at most 32 entries")
	}
	service := &Service{
		loginEnabled:  cfg.LoginEnabled,
		bearerEnabled: cfg.BearerEnabled,
		sessionTTL:    cfg.SessionTTL,
		secureCookie:  cfg.SecureCookie,
		logger:        logger,
		loginLimiters: make(map[string]*loginClient),
		sessions:      make(map[[sha256.Size]byte]browserSession),
	}
	for _, value := range cfg.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("parse trusted login proxy CIDR %q: %w", value, err)
		}
		if prefix.Bits() == 0 {
			return nil, fmt.Errorf("trusted login proxy CIDR must not contain every address: %q", value)
		}
		service.trustedProxies = append(service.trustedProxies, prefix.Masked())
	}

	var err error
	if cfg.LoginEnabled {
		service.password, err = loadSecret(cfg.PasswordFile, "native login password", minimumPasswordBytes)
		if err != nil {
			return nil, err
		}
	}
	if cfg.BearerEnabled {
		service.bearerToken, err = loadSecret(cfg.BearerTokenFile, "bearer token", minimumBearerBytes)
		if err != nil {
			return nil, err
		}
	}

	return service, nil
}

// Mount registers the public browser session endpoints.
func (s *Service) Mount(router *gin.Engine) {
	router.GET("/api/auth/session", s.getSession)
	router.POST("/api/auth/login", s.login)
	router.DELETE("/api/auth/session", s.logout)
}

// Middleware requires either a valid browser session or bearer token when auth is enabled.
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authorized, sessionContext := s.authorized(c.Request)
		if authorized {
			if sessionContext != nil {
				requestContext, cancel := context.WithCancel(c.Request.Context())
				stop := context.AfterFunc(sessionContext, cancel)
				c.Request = c.Request.WithContext(requestContext)
				c.Next()
				stop()
				cancel()
				return
			}
			c.Next()
			return
		}

		c.Header("Cache-Control", "no-store")
		if s.bearerEnabled {
			c.Header("WWW-Authenticate", `Bearer realm="webdesktop"`)
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, errorResponse{
			Error: protocolError{Code: "authentication_required", Message: "authentication is required"},
		})
	}
}

func (s *Service) getSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, s.status(c.Request))
}

func (s *Service) login(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !s.loginEnabled && !s.bearerEnabled {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, errorResponse{
			Error: protocolError{Code: "unsupported_media_type", Message: "login requests must use application/json"},
		})
		return
	}
	if origin := c.GetHeader("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host != c.Request.Host {
			c.AbortWithStatusJSON(http.StatusForbidden, errorResponse{
				Error: protocolError{Code: "origin_not_allowed", Message: "login request origin is not allowed"},
			})
			return
		}
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maximumSecretBytes+128)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request loginRequest
	if err := decoder.Decode(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{
			Error: protocolError{Code: "invalid_request", Message: "login request is invalid"},
		})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{
			Error: protocolError{Code: "invalid_request", Message: "login request must contain one JSON object"},
		})
		return
	}
	if request.Credential == "" || len(request.Credential) > maximumSecretBytes {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{
			Error: protocolError{Code: "invalid_request", Message: "credential is required"},
		})
		return
	}
	clientAddress := s.loginClientAddress(c.Request)
	if !s.allowLogin(clientAddress) {
		s.logger.Warn("authentication rate limit exceeded", zap.String("client_bucket", clientAddress))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, errorResponse{
			Error: protocolError{Code: "rate_limited", Message: "too many login attempts"},
		})
		return
	}

	method := s.matchLoginCredential(request.Credential)
	if method == "" {
		s.logger.Warn("authentication rejected")
		c.AbortWithStatusJSON(http.StatusUnauthorized, errorResponse{
			Error: protocolError{Code: "invalid_credentials", Message: "credential is invalid"},
		})
		return
	}

	value, expiresAt, err := s.newSession()
	if err != nil {
		s.logger.Error("create browser session", zap.Error(err))
		c.AbortWithStatusJSON(http.StatusInternalServerError, errorResponse{
			Error: protocolError{Code: "internal_error", Message: "browser session could not be created"},
		})
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(s.sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	s.logger.Info("browser authenticated", zap.String("method", method))
	status := s.status(c.Request)
	status.Authenticated = true
	c.JSON(http.StatusOK, status)
}

func (s *Service) logout(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var cancel context.CancelFunc
	if cookie, err := c.Request.Cookie(sessionCookieName); err == nil {
		digest := sha256.Sum256([]byte(cookie.Value))
		s.sessionsMu.Lock()
		if session, exists := s.sessions[digest]; exists {
			cancel = session.cancel
		}
		delete(s.sessions, digest)
		s.sessionsMu.Unlock()
	}
	if cancel != nil {
		cancel()
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Path:     "/",
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	c.Status(http.StatusNoContent)
}

func (s *Service) status(request *http.Request) sessionResponse {
	authorized, _ := s.authorized(request)
	return sessionResponse{
		Version:       1,
		Required:      s.loginEnabled || s.bearerEnabled,
		Authenticated: authorized,
		LoginEnabled:  s.loginEnabled,
		BearerEnabled: s.bearerEnabled,
	}
}

func (s *Service) authorized(request *http.Request) (bool, context.Context) {
	if !s.loginEnabled && !s.bearerEnabled {
		return true, nil
	}
	if s.bearerEnabled {
		scheme, credential, found := strings.Cut(request.Header.Get("Authorization"), " ")
		if found && strings.EqualFold(scheme, "Bearer") && credential != "" && !strings.Contains(credential, " ") {
			digest := sha256.Sum256([]byte(credential))
			if subtle.ConstantTimeCompare(digest[:], s.bearerToken[:]) == 1 {
				return true, nil
			}
		}
	}
	cookie, err := request.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false, nil
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	now := time.Now()
	var cancel context.CancelFunc
	s.sessionsMu.Lock()
	session, exists := s.sessions[digest]
	if exists && !now.Before(session.expiresAt) {
		delete(s.sessions, digest)
		cancel = session.cancel
		exists = false
	}
	s.sessionsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !exists {
		return false, nil
	}
	return true, session.ctx
}

func (s *Service) matchLoginCredential(credential string) string {
	digest := sha256.Sum256([]byte(credential))
	if s.loginEnabled && subtle.ConstantTimeCompare(digest[:], s.password[:]) == 1 {
		return "password"
	}
	if s.bearerEnabled && subtle.ConstantTimeCompare(digest[:], s.bearerToken[:]) == 1 {
		return "bearer"
	}
	return ""
}

func (s *Service) newSession() (string, time.Time, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", time.Time{}, fmt.Errorf("read random session token: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(value))
	expiresAt := time.Now().Add(s.sessionTTL)
	sessionContext, cancel := context.WithDeadline(context.Background(), expiresAt)
	now := time.Now()
	var expired []context.CancelFunc

	s.sessionsMu.Lock()
	for existing, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, existing)
			expired = append(expired, session.cancel)
		}
	}
	if existing, exists := s.sessions[digest]; exists {
		expired = append(expired, existing.cancel)
	}
	s.sessions[digest] = browserSession{ctx: sessionContext, cancel: cancel, expiresAt: expiresAt}
	s.sessionsMu.Unlock()
	for _, cancel := range expired {
		cancel()
	}
	return value, expiresAt, nil
}

func (s *Service) allowLogin(clientAddress string) bool {
	now := time.Now()
	s.loginLimitersMu.Lock()
	defer s.loginLimitersMu.Unlock()

	for address, client := range s.loginLimiters {
		if now.Sub(client.lastAttempt) >= loginClientRetention {
			delete(s.loginLimiters, address)
		}
	}
	client, exists := s.loginLimiters[clientAddress]
	if !exists {
		if len(s.loginLimiters) >= maximumLoginClients {
			oldestAddress := ""
			oldestAttempt := now
			for address, candidate := range s.loginLimiters {
				if !candidate.lastAttempt.After(oldestAttempt) {
					oldestAddress = address
					oldestAttempt = candidate.lastAttempt
				}
			}
			delete(s.loginLimiters, oldestAddress)
		}
		client = &loginClient{limiter: rate.NewLimiter(rate.Every(3*time.Second), 5)}
		s.loginLimiters[clientAddress] = client
	}
	client.lastAttempt = now
	return client.limiter.AllowN(now, 1)
}

func (s *Service) loginClientAddress(request *http.Request) string {
	remote, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	clientAddress := remote.Addr().Unmap()
	trusted := false
	for _, prefix := range s.trustedProxies {
		if prefix.Contains(clientAddress) {
			trusted = true
			break
		}
	}
	if trusted {
		forwarded := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
		for index := len(forwarded) - 1; index >= 0; index-- {
			address, err := netip.ParseAddr(strings.TrimSpace(forwarded[index]))
			if err != nil {
				break
			}
			clientAddress = address.Unmap()
			trusted = false
			for _, prefix := range s.trustedProxies {
				if prefix.Contains(clientAddress) {
					trusted = true
					break
				}
			}
			if !trusted {
				break
			}
		}
	}
	if clientAddress.Is6() {
		return netip.PrefixFrom(clientAddress, 64).Masked().String()
	}
	return clientAddress.String()
}

func loadSecret(path, name string, minimumBytes int) ([sha256.Size]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("open %s file: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("inspect %s file: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return [sha256.Size]byte{}, fmt.Errorf("%s file is not a regular file", name)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return [sha256.Size]byte{}, fmt.Errorf("%s file permissions are %04o, group and other access must be disabled", name, info.Mode().Perm())
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumSecretBytes+3))
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("read %s file: %w", name, err)
	}
	if len(contents) > maximumSecretBytes+2 {
		return [sha256.Size]byte{}, fmt.Errorf("%s file exceeds %d bytes", name, maximumSecretBytes+2)
	}
	contents = bytes.TrimSuffix(contents, []byte{'\n'})
	contents = bytes.TrimSuffix(contents, []byte{'\r'})
	if !utf8.Valid(contents) || bytes.Contains(contents, []byte{'\n'}) || bytes.Contains(contents, []byte{'\r'}) {
		return [sha256.Size]byte{}, fmt.Errorf("%s must be one UTF-8 line", name)
	}
	if len(contents) < minimumBytes || len(contents) > maximumSecretBytes {
		return [sha256.Size]byte{}, fmt.Errorf("%s must contain between %d and %d bytes", name, minimumBytes, maximumSecretBytes)
	}
	return sha256.Sum256(contents), nil
}
