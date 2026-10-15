package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// fixedNow is the clock every test runs against, so expiry maths is
// deterministic.
var fixedNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// credentialJSON builds a credentials document whose access token
// expires the given duration from fixedNow.
func credentialJSON(t *testing.T, accessExpiry, refreshExpiry time.Duration) []byte {
	t.Helper()

	doc := fmt.Sprintf(`{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-old",
    "refreshToken": "sk-ant-ort01-old",
    "expiresAt": %d,
    "refreshTokenExpiresAt": %d,
    "scopes": ["user:inference", "user:profile"],
    "subscriptionType": "team",
    "rateLimitTier": "default_claude_max_5x"
  }
}`, fixedNow.Add(accessExpiry).UnixMilli(), fixedNow.Add(refreshExpiry).UnixMilli())

	return []byte(doc)
}

// tokenServer stands in for the OAuth endpoint. It records the request
// body it was handed and replies with the given status and payload.
func tokenServer(t *testing.T, status int, payload string, seen *map[string]any) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			body, _ := io.ReadAll(r.Body)
			captured := map[string]any{}
			_ = json.Unmarshal(body, &captured)
			*seen = captured
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// newTestManager wires a Manager against a fake clientset holding the
// given secret data, and a token endpoint.
func newTestManager(t *testing.T, secretData []byte, tokenURL string) (*Manager, *fake.Clientset) {
	t.Helper()

	objects := []runtime.Object{}
	if secretData != nil {
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: "agents"},
			Data:       map[string][]byte{DefaultSecretKey: secretData},
		})
	}

	clientset := fake.NewSimpleClientset(objects...)
	m := NewManager(clientset, Config{Namespace: "agents", TokenURL: tokenURL})
	m.now = func() time.Time { return fixedNow }

	return m, clientset
}

// storedOAuth reads the OAuth section back out of the fake Secret.
func storedOAuth(t *testing.T, clientset *fake.Clientset) map[string]any {
	t.Helper()

	secret, err := clientset.CoreV1().Secrets("agents").Get(context.Background(), DefaultSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading back secret: %v", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(secret.Data[DefaultSecretKey])))
	dec.UseNumber()

	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("parsing stored credential: %v", err)
	}

	oauth, _ := doc[oauthSectionKey].(map[string]any)
	if oauth == nil {
		t.Fatalf("stored credential has no %q section", oauthSectionKey)
	}

	return oauth
}

const validTokenResponse = `{
  "access_token": "sk-ant-oat01-new",
  "refresh_token": "sk-ant-ort01-new",
  "expires_in": 28800,
  "refresh_token_expires_in": 2592000,
  "scope": "user:inference user:profile"
}`

func TestEnsureFreshDecides(t *testing.T) {
	tests := []struct {
		name          string
		secret        []byte
		wantRefreshed bool
		wantErr       string
	}{
		{
			name:          "expired token is renewed",
			secret:        credentialJSON(t, -time.Hour, 30*24*time.Hour),
			wantRefreshed: true,
		},
		{
			name:          "token inside the refresh window is renewed",
			secret:        credentialJSON(t, 10*time.Minute, 30*24*time.Hour),
			wantRefreshed: true,
		},
		{
			name:          "token with time left is left alone",
			secret:        credentialJSON(t, 6*time.Hour, 30*24*time.Hour),
			wantRefreshed: false,
		},
		{
			name:          "token exactly at the window boundary is renewed",
			secret:        credentialJSON(t, DefaultRefreshBeforeExpiry, 30*24*time.Hour),
			wantRefreshed: true,
		},
		{
			name:          "missing secret is a no-op",
			secret:        nil,
			wantRefreshed: false,
		},
		{
			name:          "api-key credential without an oauth section is a no-op",
			secret:        []byte(`{"apiKey": "sk-ant-api03-xxx"}`),
			wantRefreshed: false,
		},
		{
			name:    "expired refresh token names the operator action",
			secret:  credentialJSON(t, -time.Hour, -time.Minute),
			wantErr: "claude login",
		},
		{
			name:    "credential without a refresh token errors",
			secret:  []byte(`{"claudeAiOauth": {"accessToken": "x", "expiresAt": 1}}`),
			wantErr: "refreshToken",
		},
		{
			name:    "credential without an expiry errors",
			secret:  []byte(`{"claudeAiOauth": {"accessToken": "x", "refreshToken": "y"}}`),
			wantErr: "expiresAt",
		},
		{
			name:    "unparseable credential errors",
			secret:  []byte(`not json`),
			wantErr: "parsing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tokenServer(t, http.StatusOK, validTokenResponse, nil)
			m, _ := newTestManager(t, tt.secret, srv.URL)

			refreshed, err := m.EnsureFresh(context.Background())

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if refreshed != tt.wantRefreshed {
				t.Errorf("refreshed = %v, want %v", refreshed, tt.wantRefreshed)
			}
		})
	}
}

func TestEnsureFreshWritesRenewedCredential(t *testing.T) {
	var request map[string]any
	srv := tokenServer(t, http.StatusOK, validTokenResponse, &request)
	m, clientset := newTestManager(t, credentialJSON(t, -time.Hour, 30*24*time.Hour), srv.URL)

	if _, err := m.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("request uses the refresh_token grant", func(t *testing.T) {
		if got := request["grant_type"]; got != grantTypeRefresh {
			t.Errorf("grant_type = %v, want %v", got, grantTypeRefresh)
		}
		if got := request["refresh_token"]; got != "sk-ant-ort01-old" {
			t.Errorf("refresh_token = %v, want the stored token", got)
		}
		if got := request["client_id"]; got != DefaultClientID {
			t.Errorf("client_id = %v, want %v", got, DefaultClientID)
		}
		if got := request["scope"]; got != "user:inference user:profile" {
			t.Errorf("scope = %v, want the stored scopes", got)
		}
	})

	oauth := storedOAuth(t, clientset)

	t.Run("tokens are replaced", func(t *testing.T) {
		if got := oauth[accessTokenField]; got != "sk-ant-oat01-new" {
			t.Errorf("accessToken = %v, want the new token", got)
		}
		if got := oauth[refreshTokenField]; got != "sk-ant-ort01-new" {
			t.Errorf("refreshToken = %v, want the rotated token", got)
		}
	})

	t.Run("expiry is recomputed from expires_in", func(t *testing.T) {
		want := fixedNow.Add(28800 * time.Second).UnixMilli()
		got, ok := millisField(oauth, expiresAtField)
		if !ok {
			t.Fatalf("expiresAt missing or malformed")
		}
		if got != want {
			t.Errorf("expiresAt = %d, want %d", got, want)
		}
	})

	t.Run("unmodelled fields survive the round trip", func(t *testing.T) {
		if got := oauth["subscriptionType"]; got != "team" {
			t.Errorf("subscriptionType = %v, want it preserved", got)
		}
		if got := oauth["rateLimitTier"]; got != "default_claude_max_5x" {
			t.Errorf("rateLimitTier = %v, want it preserved", got)
		}
	})

	t.Run("epochs stay integer literals", func(t *testing.T) {
		secret, err := clientset.CoreV1().Secrets("agents").Get(context.Background(), DefaultSecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading back secret: %v", err)
		}
		// A float64 round trip would render these in exponent notation,
		// which the Claude CLI does not expect.
		if strings.Contains(string(secret.Data[DefaultSecretKey]), "e+") {
			t.Errorf("stored credential contains exponent notation: %s", secret.Data[DefaultSecretKey])
		}
	})
}

func TestEnsureFreshTokenEndpointFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		payload string
		wantErr string
	}{
		{
			name:    "rejected grant",
			status:  http.StatusBadRequest,
			payload: `{"error": "invalid_grant"}`,
			wantErr: "HTTP 400",
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			payload: `{}`,
			wantErr: "HTTP 500",
		},
		{
			name:    "response without an access token",
			status:  http.StatusOK,
			payload: `{"expires_in": 3600}`,
			wantErr: "no access_token",
		},
		{
			name:    "response without an expiry",
			status:  http.StatusOK,
			payload: `{"access_token": "sk-ant-oat01-new"}`,
			wantErr: "expires_in",
		},
		{
			name:    "unparseable response",
			status:  http.StatusOK,
			payload: `not json`,
			wantErr: "parsing token response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tokenServer(t, tt.status, tt.payload, nil)
			m, clientset := newTestManager(t, credentialJSON(t, -time.Hour, 30*24*time.Hour), srv.URL)

			_, err := m.EnsureFresh(context.Background())
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected an error containing %q, got %q", tt.wantErr, err.Error())
			}

			// A failed refresh must leave the stored credential intact
			// rather than half-written.
			if got := storedOAuth(t, clientset)[accessTokenField]; got != "sk-ant-oat01-old" {
				t.Errorf("accessToken = %v, want the original to be untouched", got)
			}
		})
	}
}

func TestApplyTokenRotation(t *testing.T) {
	tests := []struct {
		name             string
		resp             tokenResponse
		wantRefreshToken string
		wantScopes       []any
	}{
		{
			name: "rotated refresh token replaces the old one",
			resp: tokenResponse{
				AccessToken: "new", RefreshToken: "rotated", ExpiresIn: 3600,
			},
			wantRefreshToken: "rotated",
		},
		{
			name: "absent refresh token keeps the old one",
			resp: tokenResponse{
				AccessToken: "new", ExpiresIn: 3600,
			},
			wantRefreshToken: "original",
		},
		{
			name: "granted scopes replace the stored ones",
			resp: tokenResponse{
				AccessToken: "new", ExpiresIn: 3600, Scope: "user:inference",
			},
			wantRefreshToken: "original",
			wantScopes:       []any{"user:inference"},
		},
		{
			name: "empty scope leaves the stored scopes alone",
			resp: tokenResponse{
				AccessToken: "new", ExpiresIn: 3600,
			},
			wantRefreshToken: "original",
			wantScopes:       []any{"user:profile"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oauth := map[string]any{
				refreshTokenField: "original",
				scopesField:       []any{"user:profile"},
			}

			if err := applyToken(oauth, &tt.resp, fixedNow); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := oauth[refreshTokenField]; got != tt.wantRefreshToken {
				t.Errorf("refreshToken = %v, want %v", got, tt.wantRefreshToken)
			}

			if tt.wantScopes != nil {
				got := stringSlice(oauth[scopesField])
				want := stringSlice(tt.wantScopes)
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("scopes = %v, want %v", got, want)
				}
			}
		})
	}
}
