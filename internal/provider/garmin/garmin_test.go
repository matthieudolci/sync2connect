package garmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthieudolci/sync2connect/internal/fit"
	"github.com/matthieudolci/sync2connect/internal/model"
	"github.com/matthieudolci/sync2connect/internal/provider"
)

// fakeGarmin simulates the SSO, OAuth and upload endpoints.
type fakeGarmin struct {
	t           *testing.T
	requireMFA  bool
	uploads     [][]byte
	uploadCode  int
	exchanges   int
	failuresMsg string
	// rejectExchangeToken makes the OAuth2 exchange return 401 for this
	// OAuth1 token, simulating an expired token.
	rejectExchangeToken string
}

func (f *fakeGarmin) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/consumer", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"consumer_key":    "test-key",
			"consumer_secret": "test-secret",
		})
	})
	mux.HandleFunc("/sso/embed", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html></html>")
	})
	mux.HandleFunc("/sso/signin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `<html><title>Sign In</title><input name="_csrf" value="csrf-123"></html>`)
			return
		}
		if got := r.PostFormValue("_csrf"); got != "csrf-123" {
			f.t.Errorf("signin POST csrf = %q", got)
		}
		if r.PostFormValue("username") != "user@example.com" || r.PostFormValue("password") != "hunter2" {
			fmt.Fprint(w, `<html><title>Sign In Failure</title></html>`)
			return
		}
		if f.requireMFA {
			fmt.Fprint(w, `<html><title>MFA Required</title><input name="_csrf" value="csrf-mfa"></html>`)
			return
		}
		fmt.Fprint(w, `<html><title>Success</title><a href="https://x/embed?ticket=ST-ticket-1"</a></html>`)
	})
	mux.HandleFunc("/sso/verifyMFA/loginEnterMfaCode", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostFormValue("mfa-code"); got != "424242" {
			f.t.Errorf("mfa code = %q", got)
		}
		if got := r.PostFormValue("_csrf"); got != "csrf-mfa" {
			f.t.Errorf("mfa csrf = %q", got)
		}
		fmt.Fprint(w, `<html><title>Success</title><a href="https://x/embed?ticket=ST-ticket-1"</a></html>`)
	})
	mux.HandleFunc("/oauth-service/oauth/preauthorized", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "OAuth ") || !strings.Contains(auth, `oauth_consumer_key="test-key"`) {
			f.t.Errorf("preauthorized missing oauth1 signature: %q", auth)
		}
		if got := r.URL.Query().Get("ticket"); got != "ST-ticket-1" {
			f.t.Errorf("preauthorized ticket = %q", got)
		}
		fmt.Fprint(w, "oauth_token=ot-1&oauth_token_secret=os-1")
	})
	mux.HandleFunc("/oauth-service/oauth/exchange/user/2.0", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if f.rejectExchangeToken != "" && strings.Contains(auth, fmt.Sprintf("oauth_token=%q", f.rejectExchangeToken)) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"token expired"}`)
			return
		}
		f.exchanges++
		if !strings.Contains(auth, `oauth_token="ot-1"`) {
			f.t.Errorf("exchange missing oauth1 token: %q", auth)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("bearer-%d", f.exchanges),
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/upload-service/upload", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer bearer-") {
			f.t.Errorf("upload authorization = %q", got)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			f.t.Errorf("upload without file part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		buf := make([]byte, 1<<20)
		n, _ := file.Read(buf)
		f.uploads = append(f.uploads, buf[:n])
		if f.uploadCode != 0 {
			w.WriteHeader(f.uploadCode)
		}
		msg := `{"detailedImportResult":{"uploadId":1,"failures":[]}}`
		if f.failuresMsg != "" {
			msg = fmt.Sprintf(`{"detailedImportResult":{"failures":[{"messages":[{"code":202,"content":%q}]}]}}`, f.failuresMsg)
		}
		fmt.Fprint(w, msg)
	})
	return mux
}

func newTestProvider(t *testing.T, server *httptest.Server, email, password string) *Provider {
	t.Helper()
	p, err := New(provider.Config{
		Settings: provider.Settings{
			"email":        email,
			"password":     password,
			"api_url":      server.URL,
			"sso_url":      server.URL,
			"consumer_url": server.URL + "/consumer",
		},
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Provider)
}

func noPrompt(t *testing.T) provider.PromptFunc {
	return func(label string, secret bool) (string, error) {
		t.Fatalf("unexpected prompt: %s", label)
		return "", nil
	}
}

func TestAuthenticateAndPush(t *testing.T) {
	fake := &fakeGarmin{t: t}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "user@example.com", "hunter2")
	if err := p.Authenticate(context.Background(), noPrompt(t)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// Tokens must be persisted.
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(p.tokenPath), "garmin_tokens.json"))
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	var tok tokens
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatal(err)
	}
	if tok.OAuth1Token != "ot-1" || tok.OAuth2AccessToken == "" {
		t.Fatalf("persisted tokens incomplete: %+v", tok)
	}

	err = p.PushBody(context.Background(), []model.BodyMeasurement{
		{Timestamp: time.Now().Add(-time.Hour), WeightKg: 80.5},
	})
	if err != nil {
		t.Fatalf("PushBody: %v", err)
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("got %d uploads, want 1", len(fake.uploads))
	}
	// The uploaded payload must be a valid FIT file.
	up := fake.uploads[0]
	if len(up) < 14 || string(up[8:12]) != ".FIT" {
		t.Fatalf("upload is not a FIT file: % x", up[:12])
	}
	wantCRC := uint16(up[len(up)-2]) | uint16(up[len(up)-1])<<8
	if got := fit.Checksum(up[:len(up)-2]); got != wantCRC {
		t.Fatalf("uploaded FIT CRC mismatch: %#x != %#x", got, wantCRC)
	}
}

func TestAuthenticateWithMFA(t *testing.T) {
	fake := &fakeGarmin{t: t, requireMFA: true}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "user@example.com", "hunter2")
	prompt := func(label string, secret bool) (string, error) {
		if !strings.Contains(label, "MFA") {
			t.Fatalf("unexpected prompt %q", label)
		}
		return "424242", nil
	}
	if err := p.Authenticate(context.Background(), prompt); err != nil {
		t.Fatalf("Authenticate with MFA: %v", err)
	}
}

func TestAuthenticateBadCredentials(t *testing.T) {
	fake := &fakeGarmin{t: t}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "user@example.com", "wrong")
	err := p.Authenticate(context.Background(), noPrompt(t))
	if err == nil || !strings.Contains(err.Error(), "login failed") {
		t.Fatalf("expected login failure, got %v", err)
	}
}

// TestPushReexchangesExpiredToken verifies a stored OAuth1 token is enough:
// the provider re-exchanges for a fresh OAuth2 token without re-login.
func TestPushReexchangesExpiredToken(t *testing.T) {
	fake := &fakeGarmin{t: t}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "", "")
	stored := tokens{
		OAuth1Token:       "ot-1",
		OAuth1Secret:      "os-1",
		OAuth2AccessToken: "bearer-stale",
		OAuth2ExpiresAt:   time.Now().Add(-time.Hour),
	}
	raw, _ := json.Marshal(stored)
	if err := os.WriteFile(p.tokenPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	err := p.PushBody(context.Background(), []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 79}})
	if err != nil {
		t.Fatalf("PushBody: %v", err)
	}
	if fake.exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", fake.exchanges)
	}
}

func TestPushDuplicateIsSuccess(t *testing.T) {
	for name, fake := range map[string]*fakeGarmin{
		"http409":          {uploadCode: http.StatusConflict},
		"duplicateFailure": {failuresMsg: "Duplicate file detected"},
	} {
		t.Run(name, func(t *testing.T) {
			fake.t = t
			server := httptest.NewServer(fake.handler())
			defer server.Close()

			p := newTestProvider(t, server, "user@example.com", "hunter2")
			if err := p.Authenticate(context.Background(), noPrompt(t)); err != nil {
				t.Fatal(err)
			}
			err := p.PushBody(context.Background(), []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 79}})
			if err != nil {
				t.Fatalf("duplicate upload should not error, got %v", err)
			}
		})
	}
}

func TestPushRejectedUpload(t *testing.T) {
	fake := &fakeGarmin{t: t, failuresMsg: "File is corrupt"}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "user@example.com", "hunter2")
	if err := p.Authenticate(context.Background(), noPrompt(t)); err != nil {
		t.Fatal(err)
	}
	err := p.PushBody(context.Background(), []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 79}})
	if err == nil || !strings.Contains(err.Error(), "File is corrupt") {
		t.Fatalf("expected rejection error, got %v", err)
	}
}

func TestPushWithoutAuth(t *testing.T) {
	server := httptest.NewServer((&fakeGarmin{t: t}).handler())
	defer server.Close()

	p := newTestProvider(t, server, "", "")
	err := p.PushBody(context.Background(), []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 79}})
	if err == nil || !strings.Contains(err.Error(), "auth garmin") {
		t.Fatalf("expected not-authenticated error, got %v", err)
	}
}

// TestPushAutoLogin verifies that with credentials configured, a missing
// token file triggers a headless login during PushBody.
func TestPushAutoLogin(t *testing.T) {
	fake := &fakeGarmin{t: t}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "user@example.com", "hunter2")
	err := p.PushBody(context.Background(), []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 80}})
	if err != nil {
		t.Fatalf("PushBody with auto-login: %v", err)
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(fake.uploads))
	}
	if _, err := os.Stat(p.tokenPath); err != nil {
		t.Fatalf("token file not created: %v", err)
	}
}

// TestPushAutoLoginMFA: MFA accounts cannot log in headless and must get a
// clear error pointing at the interactive auth command.
func TestPushAutoLoginMFA(t *testing.T) {
	fake := &fakeGarmin{t: t, requireMFA: true}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "user@example.com", "hunter2")
	err := p.PushBody(context.Background(), []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 80}})
	if err == nil || !strings.Contains(err.Error(), "MFA") {
		t.Fatalf("expected MFA error, got %v", err)
	}
}

// TestPushAutoReloginOnDeadOAuth1 verifies that when the stored OAuth1
// token is rejected (expired after ~a year), a fresh login happens
// automatically when credentials are configured.
func TestPushAutoReloginOnDeadOAuth1(t *testing.T) {
	fake := &fakeGarmin{t: t, rejectExchangeToken: "ot-dead"}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server, "user@example.com", "hunter2")
	stored := tokens{OAuth1Token: "ot-dead", OAuth1Secret: "os-dead"}
	raw, _ := json.Marshal(stored)
	if err := os.WriteFile(p.tokenPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	err := p.PushBody(context.Background(), []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 80}})
	if err != nil {
		t.Fatalf("PushBody with expired oauth1: %v", err)
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(fake.uploads))
	}
}
