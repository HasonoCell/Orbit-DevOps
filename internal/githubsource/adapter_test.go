package githubsource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
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

func TestAdapterHeadFollowsRenameButRejectsOwnerTransfer(t *testing.T) {
	t.Parallel()
	t.Run("same owner rename", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/repositories/101":
				_, _ = fmt.Fprint(response, `{"id":101,"full_name":"example/renamed","clone_url":"https://github.com/example/renamed.git","private":false,"owner":{"id":202}}`)
			case "/repos/example/renamed/branches/main":
				_, _ = fmt.Fprint(response, `{"name":"main","commit":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`)
			default:
				http.NotFound(response, request)
			}
		}))
		defer server.Close()
		adapter, err := New(Config{APIBaseURL: server.URL, HTTPClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		identity, err := adapter.Head(context.Background(), pipeline.HeadRequest{RepositoryID: 101, OwnerID: 202, GitRef: "refs/heads/main"})
		if err != nil {
			t.Fatal(err)
		}
		if identity.RepositoryName != "example/renamed" || identity.RepositoryURL != "https://github.com/example/renamed.git" {
			t.Fatalf("renamed identity = %#v", identity)
		}
	})

	t.Run("owner transfer", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(response, `{"id":101,"full_name":"other/demo","clone_url":"https://github.com/other/demo.git","private":false,"owner":{"id":303}}`)
		}))
		defer server.Close()
		adapter, err := New(Config{APIBaseURL: server.URL, HTTPClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.Head(context.Background(), pipeline.HeadRequest{RepositoryID: 101, OwnerID: 202, GitRef: "refs/heads/main"})
		if !errors.Is(err, pipeline.ErrSourceOwnerChanged) {
			t.Fatalf("owner transfer error = %v", err)
		}
	})
}

func TestAdapterMapsProviderFailuresAndSendsOptionalToken(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "not found", status: http.StatusNotFound, want: pipeline.ErrSourceNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests, want: pipeline.ErrSourceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer read-token" || request.Header.Get("Accept") != "application/vnd.github+json" {
					t.Errorf("GitHub request headers = %#v", request.Header)
				}
				response.WriteHeader(test.status)
			}))
			defer server.Close()
			adapter, err := New(Config{APIBaseURL: server.URL, Token: "read-token", EndpointKeys: map[string]struct{}{"public": {}}, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Resolve(context.Background(), pipeline.SourceRequest{EndpointKey: "public", RepositoryURL: "https://github.com/example/demo.git", Branch: "main"})
			if !errors.Is(err, test.want) {
				t.Fatalf("provider error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestAdapterRequiresConfiguredEndpointAndCredentialFreeURL(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{EndpointKeys: map[string]struct{}{"public": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Resolve(context.Background(), pipeline.SourceRequest{EndpointKey: "missing", RepositoryURL: "https://github.com/example/demo.git", Branch: "main"}); err == nil {
		t.Fatal("unconfigured endpoint was accepted")
	}
	if _, err := adapter.Resolve(context.Background(), pipeline.SourceRequest{EndpointKey: "public", RepositoryURL: "https://token@github.com/example/demo.git", Branch: "main"}); err == nil {
		t.Fatal("credential-bearing repository URL was accepted")
	}
}
