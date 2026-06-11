// Package withings implements a Source provider backed by the official
// Withings public health data API (https://developer.withings.com). It
// authenticates with OAuth2 and reads body-composition measurements via the
// measure/getmeas endpoint.
package withings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthieudolci/sync2connect/internal/model"
	"github.com/matthieudolci/sync2connect/internal/provider"
)

const (
	defaultAPIBase      = "https://wbsapi.withings.net"
	defaultAuthorizeURL = "https://account.withings.com/oauth2_user/authorize2"
	defaultRedirectURI  = "http://localhost:8484/callback"
	defaultListen       = "localhost:8484"
)

// Withings measurement types (https://developer.withings.com/api-reference).
const (
	measWeight     = 1
	measHeight     = 4
	measFatRatio   = 6
	measMuscleMass = 76
	measHydration  = 77 // hydration mass in kg
	measBoneMass   = 88
)

func init() {
	provider.Register("withings", New)
}

// Provider is the Withings source provider.
type Provider struct {
	clientID     string
	clientSecret string
	redirectURI  string
	listen       string
	manualAuth   bool
	autoAuth     bool

	// authStarted is a test hook invoked when the callback listener is up.
	authStarted func(authURL, addr string)

	apiBase      string
	authorizeURL string
	tokenPath    string
	hc           *http.Client

	mu  sync.Mutex
	tok *token
}

// New builds the provider from configuration. Required settings: client_id
// and client_secret from your Withings developer application.
func New(cfg provider.Config) (provider.Provider, error) {
	clientID := cfg.Settings.Get("client_id", "")
	clientSecret := cfg.Settings.Get("client_secret", "")
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("withings: client_id and client_secret are required (create an app at https://developer.withings.com)")
	}
	autoAuth, _ := strconv.ParseBool(cfg.Settings.Get("auto_auth", "false"))
	return &Provider{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  cfg.Settings.Get("redirect_uri", defaultRedirectURI),
		listen:       cfg.Settings.Get("listen", defaultListen),
		manualAuth:   cfg.ManualAuth,
		autoAuth:     autoAuth,
		apiBase:      strings.TrimRight(cfg.Settings.Get("api_url", defaultAPIBase), "/"),
		authorizeURL: cfg.Settings.Get("authorize_url", defaultAuthorizeURL),
		tokenPath:    filepath.Join(cfg.StateDir, "withings_token.json"),
		hc:           &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (p *Provider) Name() string { return "withings" }

// apiResponse is the envelope Withings wraps around every response.
type apiResponse struct {
	Status int             `json:"status"`
	Error  string          `json:"error"`
	Body   json.RawMessage `json:"body"`
}

// Withings status codes that indicate an expired or invalid access token.
func isAuthStatus(status int) bool { return status == 401 }

// call POSTs a form to the API and decodes the enveloped response body into
// out. With auth it attaches a bearer token, refreshing it when expired.
func (p *Provider) call(ctx context.Context, path string, form url.Values, auth bool, out any) error {
	attempt := func(forceRefresh bool) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+path, strings.NewReader(form.Encode()))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if auth {
			tok, err := p.accessToken(ctx, forceRefresh)
			if err != nil {
				return 0, err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := p.hc.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		if err != nil {
			return 0, err
		}
		var envelope apiResponse
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return 0, fmt.Errorf("withings: decoding %s response (http %d): %w", path, resp.StatusCode, err)
		}
		if envelope.Status != 0 {
			return envelope.Status, fmt.Errorf("withings: %s returned status %d: %s", path, envelope.Status, envelope.Error)
		}
		if out != nil {
			if err := json.Unmarshal(envelope.Body, out); err != nil {
				return 0, fmt.Errorf("withings: decoding %s body: %w", path, err)
			}
		}
		return 0, nil
	}

	status, err := attempt(false)
	if err != nil && auth && isAuthStatus(status) {
		// The token may have been revoked server-side; refresh and retry once.
		_, err = attempt(true)
	}
	return err
}

// getmeasBody mirrors the measure/getmeas response body.
type getmeasBody struct {
	MeasureGroups []measureGroup `json:"measuregrps"`
	More          int            `json:"more"`
	Offset        int            `json:"offset"`
}

type measureGroup struct {
	GrpID    int64     `json:"grpid"`
	Date     int64     `json:"date"`
	Category int       `json:"category"`
	Measures []measure `json:"measures"`
}

type measure struct {
	Value int64 `json:"value"`
	Type  int   `json:"type"`
	Unit  int   `json:"unit"`
}

// realValue applies the Withings decimal scaling: value * 10^unit.
func (m measure) realValue() float64 {
	return float64(m.Value) * math.Pow10(m.Unit)
}

// FetchBody returns body measurements created or updated since the given
// time, oldest first.
func (p *Provider) FetchBody(ctx context.Context, since time.Time) ([]model.BodyMeasurement, error) {
	groups, err := p.fetchGroups(ctx, since)
	if err != nil {
		return nil, err
	}
	heightM, err := p.fetchHeight(ctx)
	if err != nil {
		return nil, err
	}

	var out []model.BodyMeasurement
	for _, g := range groups {
		if g.Category != 1 { // 1 = real measurements, 2 = user objectives
			continue
		}
		bm := model.BodyMeasurement{Timestamp: time.Unix(g.Date, 0).UTC()}
		var hydrationKg *float64
		for _, m := range g.Measures {
			v := m.realValue()
			switch m.Type {
			case measWeight:
				bm.WeightKg = v
			case measFatRatio:
				bm.BodyFatPercent = model.Float(v)
			case measMuscleMass:
				bm.MuscleMassKg = model.Float(v)
			case measHydration:
				hydrationKg = model.Float(v)
			case measBoneMass:
				bm.BoneMassKg = model.Float(v)
			}
		}
		if bm.WeightKg <= 0 {
			continue // groups without weight (e.g. standalone heart rate) cannot be synced
		}
		if hydrationKg != nil {
			bm.HydrationPercent = model.Float(*hydrationKg / bm.WeightKg * 100)
		}
		if heightM > 0 {
			bm.BMI = model.Float(bm.WeightKg / (heightM * heightM))
		}
		out = append(out, bm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

// fetchGroups pages through measure/getmeas results.
func (p *Provider) fetchGroups(ctx context.Context, since time.Time) ([]measureGroup, error) {
	meastypes := fmt.Sprintf("%d,%d,%d,%d,%d", measWeight, measFatRatio, measMuscleMass, measHydration, measBoneMass)
	var groups []measureGroup
	offset := 0
	for {
		form := url.Values{
			"action":     {"getmeas"},
			"meastypes":  {meastypes},
			"category":   {"1"},
			"lastupdate": {strconv.FormatInt(since.Unix(), 10)},
		}
		if offset > 0 {
			form.Set("offset", strconv.Itoa(offset))
		}
		var body getmeasBody
		if err := p.call(ctx, "/measure", form, true, &body); err != nil {
			return nil, err
		}
		groups = append(groups, body.MeasureGroups...)
		if body.More == 0 {
			return groups, nil
		}
		offset = body.Offset
	}
}

// fetchHeight returns the user's most recent height in meters, or 0 when
// none is recorded. Height is needed to derive BMI.
func (p *Provider) fetchHeight(ctx context.Context) (float64, error) {
	form := url.Values{
		"action":    {"getmeas"},
		"meastype":  {strconv.Itoa(measHeight)},
		"category":  {"1"},
		"startdate": {"0"},
	}
	var body getmeasBody
	if err := p.call(ctx, "/measure", form, true, &body); err != nil {
		return 0, err
	}
	height := 0.0
	latest := int64(0)
	for _, g := range body.MeasureGroups {
		for _, m := range g.Measures {
			if m.Type == measHeight && g.Date >= latest {
				latest = g.Date
				height = m.realValue()
			}
		}
	}
	return height, nil
}
