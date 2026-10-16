package hubweb

import (
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"net/http"
	"testing"
)

func TestAnimeSourceRoutesValidateAndDenyAIAndViewers(t *testing.T) {
	r := nativeRig(t, func(c *Config, _ *jobs.Config) {
		c.Token = "operator-token-value"
		c.Agents = []AgentCredential{{Name: "agent", Token: "agent-token-value"}}
	})
	for _, path := range []string{"/api/v1/anime/sources/search?provider=bad&q=title", "/api/v1/anime/sources/search?provider=mikan&q=title&page=bad", "/api/v1/anime/sources/mikan/0"} {
		code, _ := nativeRequest(t, r, "GET", path, "", "operator-token-value", "")
		if code != 400 {
			t.Fatal(path, code)
		}
		code, _ = nativeRequest(t, r, "GET", path, "", "agent-token-value", "")
		if code != 403 {
			t.Fatal("AI source query allowed", code)
		}
	}
	for _, path := range []string{"/api/v1/anime/sources/search", "/api/v1/anime/sources/mikan/123"} {
		req, _ := http.NewRequest("GET", path, nil)
		if roleRouteAllowed(domain.Principal{Kind: domain.PrincipalUser, Role: domain.RoleViewer}, req) {
			t.Fatal("viewer discovery allowed")
		}
	}
}
