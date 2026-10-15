// Package credentials keeps the Claude OAuth credential that agent
// workers mount alive.
//
// The credential lives in a Kubernetes Secret, created by
// scripts/setup.sh as a point-in-time copy of the host's
// ~/.claude/.credentials.json. Nothing renewed it: agent pods mount the
// Secret read-only and copy it into an emptyDir, so any token the Claude
// CLI refreshed inside a pod died with that pod while the Secret kept
// the original. Once its access token expired, every agent failed in
// well under a second with "OAuth session expired and could not be
// refreshed", produced no output, and — because a run that posts nothing
// leaves no trace the reconciler can see — was respawned on the next
// pass, indefinitely.
//
// Centralising the refresh here fixes the underlying race as well as the
// expiry. Refresh tokens rotate: whichever pod refreshed first consumed
// the stored token and invalidated it for everyone else. With one
// refresher that writes the result back to the Secret, agents become
// pure consumers of a credential that is always valid.
//
// On the Hard Rule in AGENTS.md: this package persists a credential, not
// state. It holds no decision data, caches nothing across calls, and
// answers no question about a ticket — "what should run next" is still
// derived entirely from Taiga, Gitea and the K8s API on every pass.
package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// OAuth client parameters. These are the values Claude Code itself uses
// for its refresh_token grant, read out of the installed CLI rather than
// guessed: a wrong client_id or endpoint fails at runtime with an opaque
// 400 that looks exactly like an expired token.
const (
	// DefaultTokenURL is the OAuth token endpoint.
	DefaultTokenURL = "https://platform.claude.com/v1/oauth/token"

	// DefaultClientID is the Claude Code OAuth client identifier.
	DefaultClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// grantTypeRefresh is the OAuth grant used to exchange a refresh
	// token for a new access token.
	grantTypeRefresh = "refresh_token"
)

// Kubernetes Secret layout.
const (
	// DefaultSecretName is the Secret scripts/setup.sh creates.
	DefaultSecretName = "claude-credentials"

	// DefaultSecretKey is the key inside that Secret holding the
	// credentials JSON. It is mounted into agent pods as
	// .credentials.json.
	DefaultSecretKey = "credentials.json"
)

// Field names inside the credentials JSON document. The file is written
// and read by the Claude CLI, so these names are its format, not ours.
const (
	oauthSectionKey            = "claudeAiOauth"
	accessTokenField           = "accessToken"
	refreshTokenField          = "refreshToken"
	expiresAtField             = "expiresAt"
	refreshTokenExpiresAtField = "refreshTokenExpiresAt"
	scopesField                = "scopes"
)

// Timing defaults.
const (
	// DefaultRefreshBeforeExpiry is how long before expiry a token is
	// renewed. It must comfortably exceed the check interval, or a token
	// can expire between two checks.
	DefaultRefreshBeforeExpiry = 30 * time.Minute

	// DefaultCheckInterval is how often the stored credential is
	// examined.
	DefaultCheckInterval = 5 * time.Minute

	// refreshRequestTimeout bounds a single token request. Matches the
	// timeout the Claude CLI uses for the same call.
	refreshRequestTimeout = 30 * time.Second

	// millisPerSecond converts the OAuth `expires_in` (seconds) to the
	// millisecond epoch the credentials file stores.
	millisPerSecond = 1000
)

// Manager renews the Claude OAuth credential held in a Kubernetes
// Secret.
//
// A Manager is safe to use from a single goroutine; the orchestrator
// runs exactly one refresh loop. Concurrent writers are still handled
// correctly — the Secret is written back with the resourceVersion it was
// read at, so a competing update is rejected rather than clobbered.
type Manager struct {
	clientset     kubernetes.Interface
	namespace     string
	secretName    string
	secretKey     string
	tokenURL      string
	clientID      string
	refreshBefore time.Duration
	httpClient    *http.Client

	// now is the clock, injectable so tests can drive expiry without
	// sleeping.
	now func() time.Time
}

// Config configures a Manager. Zero-valued fields fall back to the
// package defaults.
type Config struct {
	// Namespace is the namespace holding the credential Secret.
	Namespace string

	// SecretName is the Secret's name. Defaults to DefaultSecretName.
	SecretName string

	// SecretKey is the key within the Secret. Defaults to
	// DefaultSecretKey.
	SecretKey string

	// TokenURL overrides the OAuth token endpoint. Defaults to
	// DefaultTokenURL; tests point it at an httptest server.
	TokenURL string

	// ClientID overrides the OAuth client identifier. Defaults to
	// DefaultClientID.
	ClientID string

	// RefreshBeforeExpiry is how long before expiry to renew. Defaults
	// to DefaultRefreshBeforeExpiry.
	RefreshBeforeExpiry time.Duration
}

// NewManager builds a Manager from a Kubernetes client and a Config.
func NewManager(clientset kubernetes.Interface, cfg Config) *Manager {
	m := &Manager{
		clientset:     clientset,
		namespace:     cfg.Namespace,
		secretName:    cfg.SecretName,
		secretKey:     cfg.SecretKey,
		tokenURL:      cfg.TokenURL,
		clientID:      cfg.ClientID,
		refreshBefore: cfg.RefreshBeforeExpiry,
		httpClient:    &http.Client{Timeout: refreshRequestTimeout},
		now:           time.Now,
	}

	if m.secretName == "" {
		m.secretName = DefaultSecretName
	}
	if m.secretKey == "" {
		m.secretKey = DefaultSecretKey
	}
	if m.tokenURL == "" {
		m.tokenURL = DefaultTokenURL
	}
	if m.clientID == "" {
		m.clientID = DefaultClientID
	}
	if m.refreshBefore <= 0 {
		m.refreshBefore = DefaultRefreshBeforeExpiry
	}

	return m
}

// EnsureFresh renews the stored credential when its access token has
// expired or is about to.
//
// It reports whether a refresh was performed. A missing Secret, a Secret
// that holds an API key rather than an OAuth session, and a token with
// time left on it are all ordinary no-ops and return a nil error — the
// orchestrator supports API-key setups, where there is nothing to renew.
func (m *Manager) EnsureFresh(ctx context.Context) (bool, error) {
	secret, doc, oauth, err := m.load(ctx)
	if err != nil {
		return false, err
	}
	if oauth == nil {
		return false, nil
	}

	expiresAt, ok := millisField(oauth, expiresAtField)
	if !ok {
		return false, fmt.Errorf("credential has no usable %q field", expiresAtField)
	}

	deadline := m.now().Add(m.refreshBefore).UnixMilli()
	if expiresAt > deadline {
		return false, nil
	}

	refreshToken, _ := oauth[refreshTokenField].(string)
	if refreshToken == "" {
		return false, fmt.Errorf("credential has no %q — re-run scripts/setup.sh after `claude login`", refreshTokenField)
	}

	// A dead refresh token cannot be exchanged, and retrying it every
	// interval would hammer the endpoint for nothing. Fail with the
	// action the operator has to take instead.
	if rtExpiresAt, ok := millisField(oauth, refreshTokenExpiresAtField); ok && rtExpiresAt <= m.now().UnixMilli() {
		return false, fmt.Errorf(
			"refresh token expired at %s — run `claude login` on the host, then re-run scripts/setup.sh",
			time.UnixMilli(rtExpiresAt).UTC().Format(time.RFC3339))
	}

	scopes := stringSlice(oauth[scopesField])

	resp, err := m.requestToken(ctx, refreshToken, scopes)
	if err != nil {
		return false, err
	}

	if err := applyToken(oauth, resp, m.now()); err != nil {
		return false, err
	}

	if err := m.store(ctx, secret, doc); err != nil {
		return false, err
	}

	newExpiry, _ := millisField(oauth, expiresAtField)
	log.Printf("Claude credential refreshed; access token now valid until %s",
		time.UnixMilli(newExpiry).UTC().Format(time.RFC3339))

	return true, nil
}

// load reads the Secret and decodes the credential document.
//
// The returned oauth map is nil — with a nil error — when there is
// nothing to renew. The whole document is returned alongside it so
// unrecognised fields survive the round trip: the Claude CLI writes more
// into this file than we model, and dropping a field it depends on would
// break the agents this code exists to keep running.
func (m *Manager) load(ctx context.Context) (*corev1.Secret, map[string]any, map[string]any, error) {
	secret, err := m.clientset.CoreV1().Secrets(m.namespace).Get(ctx, m.secretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("reading secret %s/%s: %w", m.namespace, m.secretName, err)
	}

	raw, ok := secret.Data[m.secretKey]
	if !ok || len(raw) == 0 {
		return nil, nil, nil, nil
	}

	// UseNumber keeps every numeric value as its original literal.
	// Decoding into float64 and re-encoding would rewrite the
	// millisecond epochs in this file into exponent notation, which the
	// CLI does not expect.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, nil, fmt.Errorf("parsing %s in secret %s: %w", m.secretKey, m.secretName, err)
	}

	oauth, _ := doc[oauthSectionKey].(map[string]any)
	if oauth == nil {
		// An API-key-only credential file. Nothing to renew.
		return nil, nil, nil, nil
	}

	return secret, doc, oauth, nil
}

// store writes the updated document back to the Secret.
func (m *Manager) store(ctx context.Context, secret *corev1.Secret, doc map[string]any) error {
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encoding refreshed credential: %w", err)
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[m.secretKey] = encoded

	// Update carries the resourceVersion from the Get above, so a
	// competing write is rejected with a conflict instead of silently
	// overwriting a credential someone else just refreshed.
	if _, err := m.clientset.CoreV1().Secrets(m.namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating secret %s/%s: %w", m.namespace, m.secretName, err)
	}

	return nil
}

// tokenResponse is the OAuth token endpoint's reply.
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
}

// requestToken performs the refresh_token grant.
func (m *Manager) requestToken(ctx context.Context, refreshToken string, scopes []string) (*tokenResponse, error) {
	payload := map[string]string{
		"grant_type":    grantTypeRefresh,
		"refresh_token": refreshToken,
		"client_id":     m.clientID,
	}
	if len(scopes) > 0 {
		payload["scope"] = strings.Join(scopes, " ")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request to %s: %w", m.tokenURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body may name the reason (invalid_grant for a consumed or
		// revoked refresh token), but it can also echo credential
		// material, so it is not logged.
		return nil, fmt.Errorf("token refresh rejected with HTTP %d", resp.StatusCode)
	}

	var parsed tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("token response contained no access_token")
	}

	return &parsed, nil
}

// applyToken merges a token response into the credential's OAuth
// section, leaving every field the response does not speak to untouched.
func applyToken(oauth map[string]any, resp *tokenResponse, now time.Time) error {
	if resp.ExpiresIn <= 0 {
		return fmt.Errorf("token response had no positive expires_in")
	}

	oauth[accessTokenField] = resp.AccessToken
	oauth[expiresAtField] = millis(now.UnixMilli() + resp.ExpiresIn*millisPerSecond)

	// Refresh tokens rotate, but not on every exchange. An absent
	// refresh_token means "keep using the one you have" — overwriting it
	// with the empty string would lock the next renewal out.
	if resp.RefreshToken != "" {
		oauth[refreshTokenField] = resp.RefreshToken
	}

	if resp.RefreshTokenExpiresIn > 0 {
		oauth[refreshTokenExpiresAtField] = millis(now.UnixMilli() + resp.RefreshTokenExpiresIn*millisPerSecond)
	}

	if scopes := strings.Fields(resp.Scope); len(scopes) > 0 {
		granted := make([]any, len(scopes))
		for i, s := range scopes {
			granted[i] = s
		}
		oauth[scopesField] = granted
	}

	return nil
}

// millis renders a millisecond epoch as a JSON integer literal.
func millis(v int64) json.Number {
	return json.Number(strconv.FormatInt(v, 10))
}

// millisField reads a millisecond epoch, tolerating both the
// json.Number a decoded document carries and the float64 a
// plain json.Unmarshal would have produced.
func millisField(oauth map[string]any, field string) (int64, bool) {
	switch v := oauth[field].(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case float64:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}

// stringSlice reads a JSON array of strings.
func stringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}

	return out
}
