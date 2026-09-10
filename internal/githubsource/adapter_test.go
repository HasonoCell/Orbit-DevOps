package githubsource

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HasonoCell/OrbitOps/internal/pipeline"
)

func TestAdapterResolvesPublicRepositoryAndBranch(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/example/demo":
			_, _ = fmt.Fprint(response, `{"id":101,"full_name":"example/demo","clone_url":"https://github.com/example/demo.git","private":false,"owner":{"id":202},"ignored":"allowed"}`)
		case "/repos/example/demo/branches/release/one":
			_, _ = fmt.Fprint(response, `{"name":"release/one","commit":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	adapter, err := New(Config{APIBaseURL: server.URL, EndpointKeys: map[string]struct{}{"public": {}}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := adapter.Resolve(context.Background(), pipeline.SourceRequest{EndpointKey: "public", RepositoryURL: "https://github.com/example/demo.git", Branch: "release/one"})
	if err != nil {
		t.Fatal(err)
	}
	if identity.RepositoryID != 101 || identity.OwnerID != 202 || identity.GitRef != "refs/heads/release/one" || identity.HeadCommit == "" {
		t.Fatalf("source identity = %#v", identity)
	}
}

func TestAdapterRejectsPrivateRepository(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(response, `{"id":101,"full_name":"example/demo","clone_url":"https://github.com/example/demo.git","private":true,"owner":{"id":202}}`)
	}))
	defer server.Close()
	adapter, err := New(Config{APIBaseURL: server.URL, EndpointKeys: map[string]struct{}{"public": {}}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Resolve(context.Background(), pipeline.SourceRequest{EndpointKey: "public", RepositoryURL: "https://github.com/example/demo.git", Branch: "main"}); err == nil {
		t.Fatal("private repository was accepted")
	}
}
