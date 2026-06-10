package garmin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// oauth1Header builds an RFC 5849 Authorization header signed with
// HMAC-SHA1. Query parameters of rawURL and the optional form body
// parameters are included in the signature base string. token and
// tokenSecret may be empty for the initial (consumer-only) request.
func oauth1Header(consumerKey, consumerSecret, token, tokenSecret, method, rawURL string, form url.Values, nonce string, timestamp int64) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	oauthParams := map[string]string{
		"oauth_consumer_key":     consumerKey,
		"oauth_nonce":            nonce,
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(timestamp, 10),
		"oauth_version":          "1.0",
	}
	if token != "" {
		oauthParams["oauth_token"] = token
	}

	// Collect all parameters that participate in the signature.
	type pair struct{ key, value string }
	var pairs []pair
	add := func(k, v string) {
		pairs = append(pairs, pair{percentEncode(k), percentEncode(v)})
	}
	for k, vs := range u.Query() {
		for _, v := range vs {
			add(k, v)
		}
	}
	for k, vs := range form {
		for _, v := range vs {
			add(k, v)
		}
	}
	for k, v := range oauthParams {
		add(k, v)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value < pairs[j].value
	})
	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = p.key + "=" + p.value
	}
	paramString := strings.Join(encoded, "&")

	baseURL := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	baseString := strings.ToUpper(method) + "&" + percentEncode(baseURL) + "&" + percentEncode(paramString)

	signingKey := percentEncode(consumerSecret) + "&" + percentEncode(tokenSecret)
	mac := hmac.New(sha1.New, []byte(signingKey))
	mac.Write([]byte(baseString))
	oauthParams["oauth_signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))

	keys := make([]string, 0, len(oauthParams))
	for k := range oauthParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	headerParts := make([]string, len(keys))
	for i, k := range keys {
		headerParts[i] = fmt.Sprintf("%s=%q", k, percentEncode(oauthParams[k]))
	}
	return "OAuth " + strings.Join(headerParts, ", "), nil
}

// newNonce returns a random hex nonce.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// signRequest is the production wrapper around oauth1Header using a fresh
// nonce and the current time.
func signRequest(consumerKey, consumerSecret, token, tokenSecret, method, rawURL string, form url.Values) (string, error) {
	nonce, err := newNonce()
	if err != nil {
		return "", err
	}
	return oauth1Header(consumerKey, consumerSecret, token, tokenSecret, method, rawURL, form, nonce, time.Now().Unix())
}

// percentEncode implements RFC 3986 encoding as required by OAuth1 (only
// ALPHA, DIGIT, '-', '.', '_' and '~' stay unescaped).
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
