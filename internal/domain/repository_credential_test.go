package domain

import (
	"fmt"
	"strings"
	"testing"
)

func TestGatewayOriginAndCredentialRedaction(t *testing.T) {
	for _, origin := range []string{"https://gateway.example", "https://localhost:8443", "https://[::1]:8443"} {
		if !ValidGatewayOrigin(origin) {
			t.Fatal("valid HTTPS origin rejected")
		}
	}
	for _, origin := range []string{"http://gateway.example", "https://u:p@gateway.example", "https://gateway.example/", "https://gateway.example?", "https://gateway.example#", "https://gateway.example:0", "https://gateway.example:65536", "https://gateway.example:0443", "https://gateway.example:", "https://gateway.example/../x", "https://gateway.example\\x", "https://gateway.example\n"} {
		if ValidGatewayOrigin(origin) {
			t.Fatal("unsafe origin accepted")
		}
	}
	c := RepositoryCredential{GatewayPassword: []byte("gateway-secret-canary"), ResticPassword: []byte("restic-secret-canary")}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", c, c, c), "canary") {
		t.Fatal("formatted credential leaked")
	}
}
