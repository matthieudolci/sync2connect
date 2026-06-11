package withings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/matthieudolci/sync2connect/internal/provider"
)

// userID tolerates both encodings Withings uses for userid: a JSON number
// in some responses and a JSON string in others.
type userID string

func (u *userID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*u = userID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("userid is neither string nor number: %s", b)
	}
	*u = userID(n.String())
	return nil
}

// token is the persisted OAuth2 state. Withings refresh tokens are single
// use, so the file is rewritten after every refresh.
type token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	UserID       userID    `json:"user_id"`
}

// tokenResponse is the body of a successful v2/oauth2 requesttoken call.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	UserID       userID `json:"userid"`
}

func (t tokenResponse) toToken() *token {
	return &token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(t.ExpiresIn) * time.Second),
		UserID:       t.UserID,
	}
}

// accessToken returns a valid access token, refreshing when it is expired,
// about to expire, or a refresh is forced by the caller.
func (p *Provider) accessToken(ctx context.Context, force bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tok == nil {
		tok, err := p.loadToken()
		if err != nil {
			return "", err
		}
		p.tok = tok
	}
	if !force && time.Until(p.tok.ExpiresAt) > time.Minute {
		return p.tok.AccessToken, nil
	}
	body, err := p.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {p.tok.RefreshToken},
	})
	if err != nil {
		return "", fmt.Errorf("refreshing withings token (run `sync2connect auth withings` if this persists): %w", err)
	}
	p.tok = body.toToken()
	if err := p.saveToken(p.tok); err != nil {
		return "", err
	}
	return p.tok.AccessToken, nil
}

// requestToken calls v2/oauth2 action=requesttoken with the given grant.
func (p *Provider) requestToken(ctx context.Context, grant url.Values) (*tokenResponse, error) {
	form := url.Values{
		"action":        {"requesttoken"},
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
	}
	for k, vs := range grant {
		form[k] = vs
	}
	var body tokenResponse
	if err := p.call(ctx, "/v2/oauth2", form, false, &body); err != nil {
		return nil, err
	}
	if body.AccessToken == "" {
		return nil, errors.New("withings: token response contained no access token")
	}
	return &body, nil
}

func (p *Provider) loadToken() (*token, error) {
	raw, err := os.ReadFile(p.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("withings is not authenticated yet, run `sync2connect auth withings`: %w", err)
	}
	var tok token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p.tokenPath, err)
	}
	return &tok, nil
}

func (p *Provider) saveToken(tok *token) error {
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.tokenPath, raw, 0o600); err != nil {
		return fmt.Errorf("saving withings token: %w", err)
	}
	return nil
}

// Authenticate runs the OAuth2 authorization-code flow. By default it serves
// the redirect on a local listener and waits for the browser callback; with
// manual auth it asks the user to paste the redirect URL or code instead.
func (p *Provider) Authenticate(ctx context.Context, prompt provider.PromptFunc) error {
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return err
	}
	state := hex.EncodeToString(stateBytes)

	authURL := fmt.Sprintf("%s?%s", p.authorizeURL, url.Values{
		"response_type": {"code"},
		"client_id":     {p.clientID},
		"state":         {state},
		"scope":         {"user.metrics"},
		"redirect_uri":  {p.redirectURI},
	}.Encode())

	fmt.Printf("Open this URL in your browser and authorize the application:\n\n  %s\n\n", authURL)

	var code string
	var err error
	if p.manualAuth {
		code, err = p.codeFromPrompt(prompt, state)
	} else {
		code, err = p.codeFromCallback(ctx, state)
	}
	if err != nil {
		return err
	}

	body, err := p.requestToken(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {p.redirectURI},
	})
	if err != nil {
		return err
	}
	tok := body.toToken()
	if err := p.saveToken(tok); err != nil {
		return err
	}
	p.mu.Lock()
	p.tok = tok
	p.mu.Unlock()
	fmt.Printf("Withings authentication succeeded (user id %s). Token saved to %s\n", tok.UserID, p.tokenPath)
	return nil
}

// codeFromPrompt accepts either the bare authorization code or the full
// redirect URL the browser landed on.
func (p *Provider) codeFromPrompt(prompt provider.PromptFunc, state string) (string, error) {
	input, err := prompt("Paste the full redirect URL (or just the 'code' parameter)", false)
	if err != nil {
		return "", err
	}
	input = strings.TrimSpace(input)
	if u, err := url.Parse(input); err == nil && u.Query().Get("code") != "" {
		if s := u.Query().Get("state"); s != "" && s != state {
			return "", errors.New("withings: state mismatch in redirect URL")
		}
		return u.Query().Get("code"), nil
	}
	if input == "" {
		return "", errors.New("withings: empty authorization code")
	}
	return input, nil
}

// codeFromCallback runs a one-shot HTTP server on the configured listen
// address and waits for the OAuth redirect.
func (p *Provider) codeFromCallback(ctx context.Context, state string) (string, error) {
	redirect, err := url.Parse(p.redirectURI)
	if err != nil {
		return "", fmt.Errorf("invalid redirect_uri %q: %w", p.redirectURI, err)
	}
	callbackPath := redirect.Path
	if callbackPath == "" {
		callbackPath = "/"
	}

	listener, err := net.Listen("tcp", p.listen)
	if err != nil {
		return "", fmt.Errorf("starting callback listener on %s (use `auth withings --manual` on headless machines): %w", p.listen, err)
	}
	defer listener.Close()

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			results <- result{err: errors.New("withings: state mismatch in callback")}
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			results <- result{err: fmt.Errorf("withings: authorization denied: %s", e)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			results <- result{err: errors.New("withings: callback without code")}
			return
		}
		fmt.Fprintln(w, "Authentication complete. You can close this window.")
		results <- result{code: code}
	})

	server := &http.Server{Handler: mux}
	go server.Serve(listener) //nolint:errcheck // shut down via Close below
	defer server.Close()

	fmt.Printf("Waiting for the OAuth callback on %s%s ...\n", p.listen, callbackPath)
	select {
	case r := <-results:
		return r.code, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for withings callback: %w", ctx.Err())
	case <-time.After(5 * time.Minute):
		return "", errors.New("withings: timed out waiting for the OAuth callback")
	}
}
