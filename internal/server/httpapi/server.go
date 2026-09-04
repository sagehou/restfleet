package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

const (
	sessionCookieName = "restfleet_session"
	csrfCookieName    = "restfleet_csrf"
	maxJSONBody       = 1 << 20
)

type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

type Options struct {
	SecureCookies bool
	StaticDir     string
	Logger        *slog.Logger
	Build         BuildInfo
}

type API struct {
	control           *control.ControlPlane
	secureCookies     bool
	staticDir         string
	logger            *slog.Logger
	build             BuildInfo
	metrics           *Metrics
	loginLimiter      *rateLimiter
	bootstrapLimiter  *rateLimiter
	enrollmentLimiter *rateLimiter
	readLimiter       *rateLimiter
	mutationLimiter   *rateLimiter
}

func New(controlPlane *control.ControlPlane, options Options) *API {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return &API{
		control:           controlPlane,
		secureCookies:     options.SecureCookies,
		staticDir:         options.StaticDir,
		logger:            logger,
		build:             options.Build,
		metrics:           NewMetrics(),
		loginLimiter:      newRateLimiter(5, time.Minute),
		bootstrapLimiter:  newRateLimiter(5, time.Minute),
		enrollmentLimiter: newRateLimiter(10, time.Minute),
		readLimiter:       newRateLimiter(300, time.Minute),
		mutationLimiter:   newRateLimiter(60, time.Minute),
	}
}

func (a *API) NewRootHandler() http.Handler {
	mux := http.NewServeMux()
	HandlerWithOptions(a, StdHTTPServerOptions{
		BaseRouter:       mux,
		ErrorHandlerFunc: a.generatedError,
	})
	if a.staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(a.staticDir)))
	} else {
		mux.Handle("/", http.NotFoundHandler())
	}
	return a.middleware(mux)
}

func (a *API) MetricsHandler() http.Handler {
	handler := a.metrics.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if counts, err := a.control.AgentHealthCounts(r.Context()); err == nil {
			a.metrics.setAgentHealth(counts.Online, counts.Degraded, counts.Offline)
		}
		handler.ServeHTTP(w, r)
	})
}

func (a *API) ObserveAgentHeartbeat(result string) {
	a.metrics.observeAgentHeartbeat(result)
}

func (a *API) HealthLive(w http.ResponseWriter, _ *http.Request) {
	a.json(w, http.StatusOK, Health{Status: Ok})
}

func (a *API) HealthReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := a.control.Ready(ctx); err != nil {
		a.problem(w, r, http.StatusServiceUnavailable, "NOT_READY", "Service unavailable", "Required control-plane dependencies are not ready.", nil)
		return
	}
	checks := map[string]string{"database": "ok", "schema": "ok", "audit": "ok"}
	a.json(w, http.StatusOK, Health{Status: Ok, Checks: &checks})
}

func (a *API) BootstrapStatus(w http.ResponseWriter, r *http.Request) {
	required, err := a.control.BootstrapRequired(r.Context())
	if err != nil {
		a.internalProblem(w, r)
		return
	}
	a.json(w, http.StatusOK, BootstrapStatus{BootstrapRequired: required})
}

func (a *API) Bootstrap(w http.ResponseWriter, r *http.Request, params BootstrapParams) {
	meta := requestMeta(r)
	ipKey := "ip:" + hex.EncodeToString(meta.SourceIPHash)
	if !a.bootstrapLimiter.allow(ipKey) {
		if err := a.control.RecordDenied(r.Context(), "AUTH_BOOTSTRAP", "BOOTSTRAP", "RATE_LIMITED", meta); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return
	}
	var request BootstrapRequest
	if err := decodeJSON(w, r, &request); err != nil {
		if err := a.control.RecordDenied(r.Context(), "AUTH_BOOTSTRAP", "BOOTSTRAP", "INVALID_REQUEST", meta); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid.", nil)
		return
	}
	authenticated, credentials, err := a.control.Bootstrap(
		r.Context(),
		params.XRestFleetBootstrapToken,
		request.Username,
		request.DisplayName,
		request.Password,
		meta,
	)
	if err != nil {
		var validation *control.ValidationError
		switch {
		case errors.As(err, &validation):
			fieldErrors := []FieldError{{Field: validation.Field, Code: validation.Code}}
			a.problem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation failed", "One or more fields are invalid.", &fieldErrors)
		case errors.Is(err, control.ErrInvalidBootstrapToken):
			a.problem(w, r, http.StatusForbidden, "BOOTSTRAP_DENIED", "Bootstrap denied", "Bootstrap is not available for this request.", nil)
		case errors.Is(err, domain.ErrBootstrapClosed):
			a.problem(w, r, http.StatusConflict, "BOOTSTRAP_CLOSED", "Bootstrap closed", "The first administrator has already been created.", nil)
		default:
			a.internalProblem(w, r)
		}
		return
	}
	a.setSessionCookies(w, authenticated, credentials)
	a.json(w, http.StatusCreated, sessionResponse(authenticated))
}

func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	meta := requestMeta(r)
	var request LoginRequest
	if err := decodeJSON(w, r, &request); err != nil {
		if err := a.control.RecordDenied(r.Context(), "AUTH_LOGIN", "SESSION", "INVALID_REQUEST", meta); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid.", nil)
		return
	}
	ipKey := "ip:" + hex.EncodeToString(meta.SourceIPHash)
	accountKey := "account:" + hex.EncodeToString(security.HashSecret(strings.ToLower(strings.TrimSpace(request.Username))))
	if !a.loginLimiter.allow(ipKey, accountKey) {
		if err := a.control.RecordDenied(r.Context(), "AUTH_LOGIN", "SESSION", "RATE_LIMITED", meta); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return
	}
	authenticated, credentials, err := a.control.Login(r.Context(), request.Username, request.Password, meta)
	if err != nil {
		if errors.Is(err, control.ErrInvalidCredentials) {
			a.problem(w, r, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Authentication failed", "The supplied credentials are invalid.", nil)
		} else {
			a.internalProblem(w, r)
		}
		return
	}
	a.setSessionCookies(w, authenticated, credentials)
	a.json(w, http.StatusOK, sessionResponse(authenticated))
}

func (a *API) Session(w http.ResponseWriter, r *http.Request) {
	authenticated, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.readLimiter.allow("session:" + authenticated.Session.ID.String()) {
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return
	}
	a.json(w, http.StatusOK, sessionResponse(authenticated))
}

func (a *API) Logout(w http.ResponseWriter, r *http.Request, params LogoutParams) {
	authenticated, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.mutationLimiter.allow("session:" + authenticated.Session.ID.String()) {
		if err := a.control.RecordDenied(r.Context(), "AUTH_LOGOUT", "SESSION", "RATE_LIMITED", requestMeta(r)); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return
	}
	if err := a.control.Logout(r.Context(), authenticated, params.XCSRFToken, requestMeta(r)); err != nil {
		if errors.Is(err, control.ErrCSRF) {
			a.problem(w, r, http.StatusForbidden, "CSRF_INVALID", "Request denied", "The CSRF token is missing or invalid.", nil)
		} else {
			a.internalProblem(w, r)
		}
		return
	}
	a.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) DashboardSummary(w http.ResponseWriter, r *http.Request) {
	authenticated, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.readLimiter.allow("session:" + authenticated.Session.ID.String()) {
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return
	}
	hosts, err := a.control.Hosts(r.Context())
	if err != nil {
		a.internalProblem(w, r)
		return
	}
	health, err := a.control.AgentHealthCounts(r.Context())
	if err != nil {
		a.internalProblem(w, r)
		return
	}
	a.json(w, http.StatusOK, DashboardSummary{
		CollectedAt: time.Now().UTC(), Hosts: int64(len(hosts)),
		AgentsOnline: int64(health.Online), AgentsDegraded: int64(health.Degraded),
		AgentsOffline: int64(health.Offline),
		Plans:         0, Repositories: 0, Operations: 0,
	})
}

func (a *API) Version(w http.ResponseWriter, r *http.Request) {
	authenticated, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.readLimiter.allow("session:" + authenticated.Session.ID.String()) {
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return
	}
	a.json(w, http.StatusOK, Version{
		Version:       a.build.Version,
		Commit:        a.build.Commit,
		BuiltAt:       a.build.Date,
		SchemaVersion: 4,
	})
}

func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (domain.AuthenticatedSession, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	token := ""
	if err == nil {
		token = cookie.Value
	}
	authenticated, err := a.control.Authenticate(r.Context(), token, requestMeta(r))
	if err != nil {
		if errors.Is(err, control.ErrUnauthorized) {
			a.clearSessionCookies(w)
			a.problem(w, r, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "Authentication required", "Sign in to continue.", nil)
		} else {
			a.internalProblem(w, r)
		}
		return domain.AuthenticatedSession{}, false
	}
	return authenticated, true
}

func (a *API) setSessionCookies(
	w http.ResponseWriter,
	authenticated domain.AuthenticatedSession,
	credentials control.SessionCredentials,
) {
	maxAge := int(authenticated.Session.AbsoluteExpiresAt.Sub(authenticated.Session.CreatedAt).Seconds())
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    credentials.SessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  authenticated.Session.AbsoluteExpiresAt,
		MaxAge:   maxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    credentials.CSRFToken,
		Path:     "/",
		HttpOnly: false,
		Secure:   a.secureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  authenticated.Session.AbsoluteExpiresAt,
		MaxAge:   maxAge,
	})
}

func (a *API) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: name == sessionCookieName,
			Secure:   a.secureCookies,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

func sessionResponse(authenticated domain.AuthenticatedSession) Session {
	return Session{
		User: User{
			Id:          authenticated.User.ID,
			Username:    authenticated.User.Username,
			DisplayName: authenticated.User.DisplayName,
			Role:        UserRole(authenticated.User.Role),
		},
		IdleExpiresAt:     authenticated.Session.IdleExpiresAt,
		AbsoluteExpiresAt: authenticated.Session.AbsoluteExpiresAt,
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

type requestIDKey struct{}

func requestID(r *http.Request) uuid.UUID {
	value, _ := r.Context().Value(requestIDKey{}).(uuid.UUID)
	return value
}

func requestMeta(r *http.Request) control.RequestMeta {
	host := r.RemoteAddr
	if parsed, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsed
	}
	return control.RequestMeta{
		RequestID:        requestID(r),
		SourceIPHash:     security.HashSecret(host),
		UserAgentSummary: security.Redact(r.UserAgent()),
	}
}

func (a *API) problem(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	code string,
	title string,
	detail string,
	fieldErrors *[]FieldError,
) {
	w.Header().Set("Content-Type", "application/problem+json")
	a.json(w, status, Problem{
		Type:      "https://restfleet.dev/problems/" + strings.ToLower(strings.ReplaceAll(code, "_", "-")),
		Title:     title,
		Status:    status,
		Detail:    detail,
		Instance:  r.URL.Path,
		Code:      code,
		RequestId: requestID(r),
		Errors:    fieldErrors,
	})
}

func (a *API) generatedError(w http.ResponseWriter, r *http.Request, _ error) {
	switch r.URL.Path {
	case "/api/v1/bootstrap":
		if err := a.control.RecordDenied(r.Context(), "AUTH_BOOTSTRAP", "BOOTSTRAP", "TOKEN_MISSING", requestMeta(r)); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusForbidden, "BOOTSTRAP_DENIED", "Bootstrap denied", "Bootstrap is not available for this request.", nil)
	case "/api/v1/auth/logout":
		if err := a.control.RecordDenied(r.Context(), "AUTHORIZATION", "SESSION", "CSRF_MISSING", requestMeta(r)); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusForbidden, "CSRF_INVALID", "Request denied", "The CSRF token is missing or invalid.", nil)
	default:
		if r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/") &&
			r.Header.Get("X-CSRF-Token") == "" && r.URL.Path != "/api/v1/agent-enrollment" {
			if err := a.control.RecordDenied(r.Context(), "AUTHORIZATION", "SESSION", "CSRF_MISSING", requestMeta(r)); err != nil {
				a.internalProblem(w, r)
				return
			}
			a.problem(w, r, http.StatusForbidden, "CSRF_INVALID", "Request denied", "The CSRF token is missing or invalid.", nil)
			return
		}
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request parameters are invalid.", nil)
	}
}

func (a *API) internalProblem(w http.ResponseWriter, r *http.Request) {
	a.problem(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable", "The request could not be completed.", nil)
}

func (a *API) json(w http.ResponseWriter, status int, value any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(value)
}

func (a *API) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id, err := uuid.NewV7()
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Request-ID", id.String())
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		recorder := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		route := routeLabel(r.URL.Path)
		a.metrics.observe(route, r.Method, recorder.status, time.Since(started))
		a.logger.Info("http request",
			"component", "server",
			"event", "http_request",
			"request_id", id.String(),
			"route", route,
			"method", r.Method,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func routeLabel(path string) string {
	switch path {
	case "/health/live", "/health/ready",
		"/api/v1/bootstrap/status", "/api/v1/bootstrap",
		"/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/auth/session",
		"/api/v1/dashboard/summary", "/api/v1/version",
		"/api/v1/hosts", "/api/v1/agent-enrollment":
		return path
	}
	if strings.HasPrefix(path, "/api/v1/hosts/") {
		switch {
		case strings.HasSuffix(path, "/enrollment-tokens"):
			return "/api/v1/hosts/{host_id}/enrollment-tokens"
		case strings.HasSuffix(path, "/inventory"):
			return "/api/v1/hosts/{host_id}/inventory"
		case strings.HasSuffix(path, "/agents"):
			return "/api/v1/hosts/{host_id}/agents"
		case strings.HasSuffix(path, "/disable"):
			return "/api/v1/hosts/{host_id}/disable"
		case strings.HasSuffix(path, "/enable"):
			return "/api/v1/hosts/{host_id}/enable"
		default:
			return "/api/v1/hosts/{host_id}"
		}
	}
	if strings.HasPrefix(path, "/api/v1/enrollment-tokens/") {
		return "/api/v1/enrollment-tokens/{token_id}"
	}
	if strings.HasPrefix(path, "/api/v1/agents/") {
		if strings.HasSuffix(path, "/revoke") {
			return "/api/v1/agents/{agent_id}/revoke"
		}
		return "/api/v1/agents/{agent_id}"
	}
	if strings.HasPrefix(path, "/api/") {
		return "api_unknown"
	}
	return "static"
}
