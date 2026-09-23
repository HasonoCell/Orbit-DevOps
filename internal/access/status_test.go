package access

import "testing"

func TestDNSMatchingRequiresEveryAnswerToMatchGateway(t *testing.T) {
	if !dnsMatchesGateway([]string{"127.0.0.1", "127.0.0.2"}, []string{"127.0.0.2", "127.0.0.1"}) {
		t.Fatal("all gateway addresses should verify")
	}
	if dnsMatchesGateway([]string{"127.0.0.1", "192.0.2.4"}, []string{"127.0.0.1"}) {
		t.Fatal("unexpected DNS answer must be mismatch")
	}
	if dnsMatchesGateway(nil, []string{"127.0.0.1"}) {
		t.Fatal("empty answer must not verify")
	}
}
