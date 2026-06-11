package withings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matthieudolci/sync2connect/internal/provider"
)

// fakeWithings simulates the wbsapi endpoints.
type fakeWithings struct {
	t         *testing.T
	refreshes int
	// pages of measuregrps returned for body measurement queries
	pages []string
	calls int
}

func (f *fakeWithings) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/oauth2", func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("action") != "requesttoken" {
			f.t.Errorf("oauth2 action = %q", r.PostFormValue("action"))
		}
		switch r.PostFormValue("grant_type") {
		case "refresh_token":
			f.refreshes++
			if got := r.PostFormValue("refresh_token"); got != "refresh-old" {
				f.t.Errorf("refresh_token = %q", got)
			}
			// userid arrives as a string here and as a number below: the
			// live API uses both encodings.
			fmt.Fprint(w, `{"status":0,"body":{"access_token":"access-new","refresh_token":"refresh-new","expires_in":10800,"userid":"42"}}`)
		case "authorization_code":
			if got := r.PostFormValue("code"); got != "the-code" {
				f.t.Errorf("code = %q", got)
			}
			fmt.Fprint(w, `{"status":0,"body":{"access_token":"access-1","refresh_token":"refresh-1","expires_in":10800,"userid":42}}`)
		default:
			fmt.Fprint(w, `{"status":503,"error":"bad grant"}`)
		}
	})
	mux.HandleFunc("/measure", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			f.t.Errorf("measure authorization = %q", got)
		}
		if r.PostFormValue("meastype") == "4" { // height query
			fmt.Fprint(w, `{"status":0,"body":{"measuregrps":[
				{"grpid":9,"date":1500000000,"category":1,"measures":[{"value":175,"type":4,"unit":-2}]}
			]}}`)
			return
		}
		if r.PostFormValue("action") != "getmeas" {
			f.t.Errorf("measure action = %q", r.PostFormValue("action"))
		}
		page := f.calls
		f.calls++
		if page >= len(f.pages) {
			fmt.Fprint(w, `{"status":0,"body":{"measuregrps":[]}}`)
			return
		}
		fmt.Fprint(w, f.pages[page])
	})
	return mux
}

func newTestProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	p, err := New(provider.Config{
		Settings: provider.Settings{
			"client_id":     "cid",
			"client_secret": "csecret",
			"api_url":       server.URL,
		},
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Provider)
}

func writeToken(t *testing.T, p *Provider, expiresAt time.Time) {
	t.Helper()
	raw, _ := json.Marshal(token{
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		ExpiresAt:    expiresAt,
	})
	if err := os.WriteFile(p.tokenPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFetchBody(t *testing.T) {
	fake := &fakeWithings{t: t, pages: []string{
		// page 1: weight + composition, more data available
		`{"status":0,"body":{"more":1,"offset":1,"measuregrps":[
			{"grpid":1,"date":1700000000,"category":1,"measures":[
				{"value":80500,"type":1,"unit":-3},
				{"value":225,"type":6,"unit":-1},
				{"value":60040,"type":76,"unit":-3},
				{"value":44275,"type":77,"unit":-3},
				{"value":3200,"type":88,"unit":-3}
			]}
		]}}`,
		// page 2: weight only, plus a goal (category 2) that must be skipped
		`{"status":0,"body":{"more":0,"measuregrps":[
			{"grpid":2,"date":1700086400,"category":1,"measures":[{"value":81000,"type":1,"unit":-3}]},
			{"grpid":3,"date":1700086401,"category":2,"measures":[{"value":75000,"type":1,"unit":-3}]}
		]}}`,
	}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server)
	writeToken(t, p, time.Now().Add(time.Hour))

	ms, err := p.FetchBody(context.Background(), time.Unix(1690000000, 0))
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d measurements, want 2", len(ms))
	}

	first := ms[0]
	if !first.Timestamp.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("first timestamp = %v", first.Timestamp)
	}
	if first.WeightKg != 80.5 {
		t.Fatalf("weight = %v, want 80.5", first.WeightKg)
	}
	if first.BodyFatPercent == nil || *first.BodyFatPercent != 22.5 {
		t.Fatalf("fat = %v, want 22.5", first.BodyFatPercent)
	}
	if first.MuscleMassKg == nil || *first.MuscleMassKg != 60.04 {
		t.Fatalf("muscle = %v, want 60.04", first.MuscleMassKg)
	}
	if first.BoneMassKg == nil || *first.BoneMassKg != 3.2 {
		t.Fatalf("bone = %v, want 3.2", first.BoneMassKg)
	}
	// hydration: 44.275 kg of 80.5 kg = 55.0%
	if first.HydrationPercent == nil || *first.HydrationPercent < 54.9 || *first.HydrationPercent > 55.1 {
		t.Fatalf("hydration = %v, want ~55", first.HydrationPercent)
	}
	// BMI: 80.5 / 1.75^2 = 26.28...
	if first.BMI == nil || *first.BMI < 26.2 || *first.BMI > 26.4 {
		t.Fatalf("bmi = %v, want ~26.3", first.BMI)
	}

	second := ms[1]
	if second.WeightKg != 81 || second.BodyFatPercent != nil {
		t.Fatalf("second measurement = %+v", second)
	}
}

func TestFetchBodyRefreshesExpiredToken(t *testing.T) {
	fake := &fakeWithings{t: t, pages: []string{
		`{"status":0,"body":{"measuregrps":[]}}`,
	}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p := newTestProvider(t, server)
	writeToken(t, p, time.Now().Add(-time.Hour)) // expired

	if _, err := p.FetchBody(context.Background(), time.Unix(0, 0)); err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if fake.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", fake.refreshes)
	}
	// The rotated refresh token must be persisted (Withings refresh tokens
	// are single-use).
	raw, err := os.ReadFile(p.tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var tok token
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "refresh-new" || tok.AccessToken != "access-new" {
		t.Fatalf("token file not rotated: %+v", tok)
	}
}

func TestFetchBodyWithoutAuth(t *testing.T) {
	server := httptest.NewServer((&fakeWithings{t: t}).handler())
	defer server.Close()

	p := newTestProvider(t, server)
	_, err := p.FetchBody(context.Background(), time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "auth withings") {
		t.Fatalf("expected not-authenticated error, got %v", err)
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(provider.Config{Settings: provider.Settings{}, StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("expected credentials error, got %v", err)
	}
}

func TestCodeFromPrompt(t *testing.T) {
	p := &Provider{}
	prompt := func(answer string) provider.PromptFunc {
		return func(label string, secret bool) (string, error) { return answer, nil }
	}

	code, err := p.codeFromPrompt(prompt("http://localhost:8484/callback?code=abc&state=st1"), "st1")
	if err != nil || code != "abc" {
		t.Fatalf("full URL: code=%q err=%v", code, err)
	}
	if _, err := p.codeFromPrompt(prompt("http://localhost:8484/callback?code=abc&state=WRONG"), "st1"); err == nil {
		t.Fatal("state mismatch not detected")
	}
	code, err = p.codeFromPrompt(prompt("rawcode123"), "st1")
	if err != nil || code != "rawcode123" {
		t.Fatalf("bare code: code=%q err=%v", code, err)
	}
}

// TestFetchBodyAutoAuth verifies that with auto_auth enabled, a missing
// token triggers the authorization flow inline and sync proceeds once the
// browser callback arrives.
func TestFetchBodyAutoAuth(t *testing.T) {
	fake := &fakeWithings{t: t, pages: []string{
		`{"status":0,"body":{"measuregrps":[]}}`,
	}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	p, err := New(provider.Config{
		Settings: provider.Settings{
			"client_id":     "cid",
			"client_secret": "csecret",
			"api_url":       server.URL,
			"auto_auth":     "true",
			"listen":        "127.0.0.1:0",
		},
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wp := p.(*Provider)

	// Simulate the user's browser: once the listener is up, follow the
	// redirect with the code and the state from the auth URL.
	wp.authStarted = func(authURL, addr string) {
		go func() {
			u, err := url.Parse(authURL)
			if err != nil {
				t.Error(err)
				return
			}
			state := u.Query().Get("state")
			resp, err := http.Get(fmt.Sprintf("http://%s/callback?code=the-code&state=%s", addr, state))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}

	if _, err := wp.FetchBody(context.Background(), time.Unix(0, 0)); err != nil {
		t.Fatalf("FetchBody with auto_auth: %v", err)
	}
	if _, err := os.Stat(wp.tokenPath); err != nil {
		t.Fatalf("token file not created: %v", err)
	}
}

// TestFetchBodyAutoAuthDisabled keeps the original behavior: a missing
// token is an error pointing at the auth command.
func TestFetchBodyAutoAuthDisabled(t *testing.T) {
	server := httptest.NewServer((&fakeWithings{t: t}).handler())
	defer server.Close()

	p := newTestProvider(t, server) // auto_auth not set
	_, err := p.FetchBody(context.Background(), time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "auth withings") {
		t.Fatalf("expected not-authenticated error, got %v", err)
	}
}
