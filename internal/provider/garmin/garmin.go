// Package garmin implements a Destination provider for Garmin Connect.
//
// Garmin's official Health API only exposes read access to partners, so this
// provider uses the same mechanism as the Garmin Connect mobile app (and the
// garth / withings-sync projects): an SSO credential login exchanged for an
// OAuth1 token (valid for about a year), which in turn is exchanged for
// short-lived OAuth2 bearer tokens used to upload FIT weight files.
package garmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/matthieudolci/sync2connect/internal/fit"
	"github.com/matthieudolci/sync2connect/internal/model"
	"github.com/matthieudolci/sync2connect/internal/provider"
)

const (
	defaultAPIBase = "https://connectapi.garmin.com"
	defaultSSOBase = "https://sso.garmin.com"
	// defaultConsumerURL serves the OAuth consumer key/secret of the Garmin
	// Connect mobile app, maintained by the garth project.
	defaultConsumerURL = "https://thegarth.s3.amazonaws.com/oauth_consumer.json"
)

func init() {
	provider.Register("garmin", New)
}

// Provider is the Garmin Connect destination provider.
type Provider struct {
	email    string
	password string

	apiBase     string
	ssoBase     string
	consumerURL string
	tokenPath   string
	hc          *http.Client

	mu       sync.Mutex
	tok      *tokens
	consumer *consumerCreds
}

// tokens is the persisted authentication state.
type tokens struct {
	OAuth1Token       string    `json:"oauth1_token"`
	OAuth1Secret      string    `json:"oauth1_secret"`
	MFAToken          string    `json:"mfa_token,omitempty"`
	OAuth2AccessToken string    `json:"oauth2_access_token"`
	OAuth2ExpiresAt   time.Time `json:"oauth2_expires_at"`
}

type consumerCreds struct {
	ConsumerKey    string `json:"consumer_key"`
	ConsumerSecret string `json:"consumer_secret"`
}

// New builds the provider. Settings: email and password (only needed for
// `auth garmin`; sync runs on stored tokens), and optional api_url, sso_url
// and consumer_url overrides.
func New(cfg provider.Config) (provider.Provider, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	return &Provider{
		email:       cfg.Settings.Get("email", ""),
		password:    cfg.Settings.Get("password", ""),
		apiBase:     strings.TrimRight(cfg.Settings.Get("api_url", defaultAPIBase), "/"),
		ssoBase:     strings.TrimRight(cfg.Settings.Get("sso_url", defaultSSOBase), "/"),
		consumerURL: cfg.Settings.Get("consumer_url", defaultConsumerURL),
		tokenPath:   filepath.Join(cfg.StateDir, "garmin_tokens.json"),
		hc:          &http.Client{Timeout: 60 * time.Second, Jar: jar},
	}, nil
}

func (p *Provider) Name() string { return "garmin" }

// Authenticate logs in with email/password (prompting when not configured),
// obtains OAuth1 + OAuth2 tokens and persists them. The OAuth1 token stays
// valid for about a year, so this is a rare operation.
func (p *Provider) Authenticate(ctx context.Context, prompt provider.PromptFunc) error {
	email, password := p.email, p.password
	var err error
	if email == "" {
		if email, err = prompt("Garmin Connect email", false); err != nil {
			return err
		}
	}
	if password == "" {
		if password, err = prompt("Garmin Connect password", true); err != nil {
			return err
		}
	}

	mfaPrompt := func() (string, error) { return prompt("Garmin MFA code", false) }
	tok, err := p.login(ctx, strings.TrimSpace(email), password, mfaPrompt)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.tok = tok
	p.mu.Unlock()
	fmt.Printf("Garmin authentication succeeded. Tokens saved to %s\n", p.tokenPath)
	return nil
}

// login performs the full SSO + OAuth1 + OAuth2 flow and persists the
// resulting tokens. It does not touch p.tok or p.mu, so it is safe to call
// while holding the provider lock.
func (p *Provider) login(ctx context.Context, email, password string, mfaPrompt func() (string, error)) (*tokens, error) {
	ticket, err := p.ssoLogin(ctx, email, password, mfaPrompt)
	if err != nil {
		return nil, err
	}
	consumer, err := p.consumerCredentials(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := p.fetchOAuth1Token(ctx, consumer, ticket)
	if err != nil {
		return nil, err
	}
	if err := p.exchangeOAuth2(ctx, consumer, tok); err != nil {
		return nil, err
	}
	if err := p.saveTokens(tok); err != nil {
		return nil, err
	}
	return tok, nil
}

// PushBody encodes the measurements as a FIT weight file and uploads it.
func (p *Provider) PushBody(ctx context.Context, measurements []model.BodyMeasurement) error {
	if len(measurements) == 0 {
		return nil
	}
	accessToken, err := p.accessToken(ctx)
	if err != nil {
		return err
	}

	data := fit.EncodeWeight(measurements, time.Now().UTC())
	filename := fmt.Sprintf("sync2connect-%d.fit", time.Now().Unix())

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/upload-service/upload", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", oauthUserAgent)

	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("garmin: uploading fit file: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil // every measurement already exists in Garmin Connect
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return checkImportResult(raw)
	default:
		return fmt.Errorf("garmin: upload failed with http %d: %s", resp.StatusCode, truncate(raw, 500))
	}
}

// importResult mirrors the relevant part of the upload-service response.
type importResult struct {
	DetailedImportResult struct {
		Failures []struct {
			Messages []struct {
				Code    int    `json:"code"`
				Content string `json:"content"`
			} `json:"messages"`
		} `json:"failures"`
	} `json:"detailedImportResult"`
}

// checkImportResult surfaces upload failures, ignoring duplicate-file
// rejections which simply mean the data was already synced.
func checkImportResult(raw []byte) error {
	var res importResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil // tolerate non-JSON success bodies (e.g. async 202)
	}
	for _, failure := range res.DetailedImportResult.Failures {
		for _, msg := range failure.Messages {
			if strings.Contains(strings.ToLower(msg.Content), "duplicate") {
				continue
			}
			return fmt.Errorf("garmin: upload rejected: %s (code %d)", msg.Content, msg.Code)
		}
	}
	return nil
}

// accessToken returns a valid OAuth2 bearer token, re-exchanging the stored
// OAuth1 token when the current one is expired or missing. When no usable
// tokens exist and credentials are configured, it logs in automatically
// (headless; accounts with MFA still need `sync2connect auth garmin` once).
func (p *Provider) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tok == nil {
		tok, err := p.loadTokens()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			if tok, err = p.autoLogin(ctx, err); err != nil {
				return "", err
			}
		}
		p.tok = tok
	}
	if p.tok.OAuth2AccessToken != "" && time.Until(p.tok.OAuth2ExpiresAt) > time.Minute {
		return p.tok.OAuth2AccessToken, nil
	}
	if p.tok.OAuth1Token == "" {
		tok, err := p.autoLogin(ctx, errors.New("garmin is not authenticated yet, run `sync2connect auth garmin`"))
		if err != nil {
			return "", err
		}
		p.tok = tok
		return p.tok.OAuth2AccessToken, nil
	}
	consumer, err := p.consumerCredentials(ctx)
	if err != nil {
		return "", err
	}
	if err := p.exchangeOAuth2(ctx, consumer, p.tok); err != nil {
		// The OAuth1 token has likely expired (they last about a year);
		// fall back to a fresh login when credentials are available.
		tok, lerr := p.autoLogin(ctx, fmt.Errorf("refreshing garmin token (run `sync2connect auth garmin` if this persists): %w", err))
		if lerr != nil {
			return "", lerr
		}
		p.tok = tok
		return p.tok.OAuth2AccessToken, nil
	}
	if err := p.saveTokens(p.tok); err != nil {
		return "", err
	}
	return p.tok.OAuth2AccessToken, nil
}

// autoLogin attempts a headless login with the configured credentials,
// returning cause unchanged when none are configured. Without an
// interactive prompt, accounts requiring MFA fail with a clear error.
func (p *Provider) autoLogin(ctx context.Context, cause error) (*tokens, error) {
	if p.email == "" || p.password == "" {
		return nil, cause
	}
	tok, err := p.login(ctx, strings.TrimSpace(p.email), p.password, nil)
	if err != nil {
		return nil, fmt.Errorf("garmin automatic login failed: %w", err)
	}
	return tok, nil
}

// fetchOAuth1Token trades the SSO ticket for a long-lived OAuth1 token.
func (p *Provider) fetchOAuth1Token(ctx context.Context, consumer *consumerCreds, ticket string) (*tokens, error) {
	endpoint := fmt.Sprintf("%s/oauth-service/oauth/preauthorized?ticket=%s&login-url=%s&accepts-mfa-tokens=true",
		p.apiBase, url.QueryEscape(ticket), url.QueryEscape(p.ssoBase+"/sso/embed"))
	auth, err := signRequest(consumer.ConsumerKey, consumer.ConsumerSecret, "", "", http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("User-Agent", oauthUserAgent)

	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("garmin: requesting oauth1 token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("garmin: oauth1 token request failed with http %d: %s", resp.StatusCode, truncate(raw, 500))
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, fmt.Errorf("garmin: parsing oauth1 response: %w", err)
	}
	tok := &tokens{
		OAuth1Token:  values.Get("oauth_token"),
		OAuth1Secret: values.Get("oauth_token_secret"),
		MFAToken:     values.Get("mfa_token"),
	}
	if tok.OAuth1Token == "" || tok.OAuth1Secret == "" {
		return nil, errors.New("garmin: oauth1 response missing token")
	}
	return tok, nil
}

// exchangeOAuth2 trades the OAuth1 token for an OAuth2 bearer token,
// updating tok in place.
func (p *Provider) exchangeOAuth2(ctx context.Context, consumer *consumerCreds, tok *tokens) error {
	endpoint := p.apiBase + "/oauth-service/oauth/exchange/user/2.0"
	form := url.Values{}
	if tok.MFAToken != "" {
		form.Set("mfa_token", tok.MFAToken)
	}
	auth, err := signRequest(consumer.ConsumerKey, consumer.ConsumerSecret, tok.OAuth1Token, tok.OAuth1Secret, http.MethodPost, endpoint, form)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("User-Agent", oauthUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("garmin: exchanging for oauth2 token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("garmin: oauth2 exchange failed with http %d: %s", resp.StatusCode, truncate(raw, 500))
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("garmin: parsing oauth2 response: %w", err)
	}
	if body.AccessToken == "" {
		return errors.New("garmin: oauth2 exchange returned no access token")
	}
	tok.OAuth2AccessToken = body.AccessToken
	tok.OAuth2ExpiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return nil
}

// consumerCredentials returns the OAuth consumer key/secret, fetching and
// caching them on first use.
func (p *Provider) consumerCredentials(ctx context.Context) (*consumerCreds, error) {
	if p.consumer != nil {
		return p.consumer, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.consumerURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("garmin: fetching oauth consumer credentials: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("garmin: fetching oauth consumer credentials: http %d", resp.StatusCode)
	}
	var creds consumerCreds
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&creds); err != nil {
		return nil, fmt.Errorf("garmin: parsing oauth consumer credentials: %w", err)
	}
	if creds.ConsumerKey == "" || creds.ConsumerSecret == "" {
		return nil, errors.New("garmin: oauth consumer credentials are incomplete")
	}
	p.consumer = &creds
	return p.consumer, nil
}

func (p *Provider) loadTokens() (*tokens, error) {
	raw, err := os.ReadFile(p.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("garmin is not authenticated yet, run `sync2connect auth garmin`: %w", err)
	}
	var tok tokens
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p.tokenPath, err)
	}
	return &tok, nil
}

func (p *Provider) saveTokens(tok *tokens) error {
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.tokenPath, raw, 0o600); err != nil {
		return fmt.Errorf("saving garmin tokens: %w", err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
