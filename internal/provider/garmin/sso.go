package garmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The SSO login mirrors the flow of the Garmin Connect mobile app (the same
// approach used by the garth and withings-sync projects): load the embed
// widget, fetch the sign-in page for a CSRF token, post the credentials
// (plus an MFA code when required) and extract the service ticket from the
// success page.

const (
	ssoUserAgent   = "GCM-iOS-5.7.2.1"
	oauthUserAgent = "com.garmin.android.apps.connectmobile"
)

var (
	csrfRe   = regexp.MustCompile(`name="_csrf"\s+value="([^"]+)"`)
	titleRe  = regexp.MustCompile(`<title>(.+?)</title>`)
	ticketRe = regexp.MustCompile(`embed\?ticket=([^"]+)"`)
)

// ssoLogin performs the credential login and returns the service ticket used
// to obtain OAuth tokens. mfaPrompt is invoked when the account requires a
// multi-factor code.
func (p *Provider) ssoLogin(ctx context.Context, email, password string, mfaPrompt func() (string, error)) (string, error) {
	embedURL := p.ssoBase + "/sso/embed"
	embedParams := url.Values{
		"id":          {"gauth-widget"},
		"embedWidget": {"true"},
		"gauthHost":   {p.ssoBase + "/sso"},
	}
	signinParams := url.Values{
		"id":                              {"gauth-widget"},
		"embedWidget":                     {"true"},
		"gauthHost":                       {embedURL},
		"service":                         {embedURL},
		"source":                          {embedURL},
		"redirectAfterAccountLoginUrl":    {embedURL},
		"redirectAfterAccountCreationUrl": {embedURL},
	}
	signinURL := p.ssoBase + "/sso/signin?" + signinParams.Encode()

	// Prime the session cookies.
	if _, err := p.ssoRequest(ctx, http.MethodGet, embedURL+"?"+embedParams.Encode(), "", nil); err != nil {
		return "", fmt.Errorf("garmin sso: loading embed page: %w", err)
	}

	// Fetch the sign-in form to obtain the CSRF token.
	page, err := p.ssoRequest(ctx, http.MethodGet, signinURL, "", nil)
	if err != nil {
		return "", fmt.Errorf("garmin sso: loading signin page: %w", err)
	}
	csrf := firstMatch(csrfRe, page)
	if csrf == "" {
		return "", errors.New("garmin sso: could not find CSRF token on signin page")
	}

	// Submit the credentials.
	page, err = p.ssoRequest(ctx, http.MethodPost, signinURL, signinURL, url.Values{
		"username": {email},
		"password": {password},
		"embed":    {"true"},
		"_csrf":    {csrf},
	})
	if err != nil {
		return "", fmt.Errorf("garmin sso: submitting credentials: %w", err)
	}

	title := firstMatch(titleRe, page)
	if strings.Contains(title, "MFA") {
		if mfaPrompt == nil {
			return "", errors.New("garmin sso: account requires MFA but no interactive prompt is available; run `sync2connect auth garmin`")
		}
		code, err := mfaPrompt()
		if err != nil {
			return "", err
		}
		mfaCsrf := firstMatch(csrfRe, page)
		if mfaCsrf == "" {
			return "", errors.New("garmin sso: could not find CSRF token on MFA page")
		}
		page, err = p.ssoRequest(ctx, http.MethodPost, p.ssoBase+"/sso/verifyMFA/loginEnterMfaCode?"+signinParams.Encode(), signinURL, url.Values{
			"mfa-code": {strings.TrimSpace(code)},
			"embed":    {"true"},
			"_csrf":    {mfaCsrf},
			"fromPage": {"setupEnterMfaCode"},
		})
		if err != nil {
			return "", fmt.Errorf("garmin sso: submitting MFA code: %w", err)
		}
		title = firstMatch(titleRe, page)
	}

	if !strings.Contains(title, "Success") {
		return "", fmt.Errorf("garmin sso: login failed (page title %q); check email/password", title)
	}
	ticket := firstMatch(ticketRe, page)
	if ticket == "" {
		return "", errors.New("garmin sso: login succeeded but no service ticket found")
	}
	return ticket, nil
}

// ssoRequest issues a request with the session cookie jar, optional referer
// and form body, returning the response body as a string.
func (p *Provider) ssoRequest(ctx context.Context, method, rawURL, referer string, form url.Values) (string, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ssoUserAgent)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	// Garmin answers 200 on bad credentials (with an error page) and 429 on
	// throttling; only genuine server errors arrive as 5xx.
	if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%s %s: http %d", method, rawURL, resp.StatusCode)
	}
	return string(raw), nil
}

func firstMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
