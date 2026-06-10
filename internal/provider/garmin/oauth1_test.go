package garmin

import (
	"net/url"
	"strings"
	"testing"
)

// TestOAuth1HeaderTwitterVector checks the signature against the worked
// example from the Twitter API documentation ("Creating a signature"), a
// widely used OAuth1 HMAC-SHA1 test vector.
func TestOAuth1HeaderTwitterVector(t *testing.T) {
	header, err := oauth1Header(
		"xvz1evFS4wEEPTGEFPHBog",
		"kAcSOqF21Fu85e7zjz7ZN2U4ZRhfV3WpwPAoE3Z7kBw",
		"370773112-GmHxMAgYyLbNEtIKZeRNFsMKPR9EyMZeS9weJAEb",
		"LswwdoUaIvS8ltyTt5jkRh4J50vUPVVHtR2YPi5kE",
		"POST",
		"https://api.twitter.com/1.1/statuses/update.json?include_entities=true",
		url.Values{"status": {"Hello Ladies + Gentlemen, a signed OAuth request!"}},
		"kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg",
		1318622958,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Expected signature, percent-encoded as it appears in the header.
	wantSig := `oauth_signature="hCtSmYh%2BiHYCEqBWrE7C7hYmtUk%3D"`
	if !strings.Contains(header, wantSig) {
		t.Fatalf("header missing expected signature %s:\n%s", wantSig, header)
	}
	if !strings.HasPrefix(header, "OAuth ") {
		t.Fatalf("header does not start with OAuth: %s", header)
	}
	for _, part := range []string{
		`oauth_consumer_key="xvz1evFS4wEEPTGEFPHBog"`,
		`oauth_signature_method="HMAC-SHA1"`,
		`oauth_timestamp="1318622958"`,
		`oauth_version="1.0"`,
	} {
		if !strings.Contains(header, part) {
			t.Fatalf("header missing %s:\n%s", part, header)
		}
	}
}

// TestOAuth1HeaderNoToken covers the consumer-only request used for the
// initial preauthorized call.
func TestOAuth1HeaderNoToken(t *testing.T) {
	header, err := oauth1Header("key", "secret", "", "", "GET",
		"https://example.com/oauth?a=1", nil, "nonce", 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(header, "oauth_token") {
		t.Fatalf("consumer-only header must not contain oauth_token: %s", header)
	}
}

func TestPercentEncode(t *testing.T) {
	cases := map[string]string{
		"Ladies + Gentlemen": "Ladies%20%2B%20Gentlemen",
		"An encoded string!": "An%20encoded%20string%21",
		"Dogs, Cats & Mice":  "Dogs%2C%20Cats%20%26%20Mice",
		"☃":                  "%E2%98%83",
		"safe-._~ABCxyz019":  "safe-._~ABCxyz019",
	}
	for in, want := range cases {
		if got := percentEncode(in); got != want {
			t.Errorf("percentEncode(%q) = %q, want %q", in, got, want)
		}
	}
}
