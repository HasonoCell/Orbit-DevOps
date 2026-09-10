// Package githubsource 通过 GitHub REST API 解析公开仓库身份和权威 Branch Head。
package githubsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/pipeline"
)

type Config struct {
	APIBaseURL   string
	Token        string
	EndpointKeys map[string]struct{}
	Timeout      time.Duration
	HTTPClient   *http.Client
}

type Adapter struct {
	baseURL      *url.URL
	token        string
	endpointKeys map[string]struct{}
	client       *http.Client
}

func New(config Config) (*Adapter, error) {
	if config.APIBaseURL == "" {
		config.APIBaseURL = "https://api.github.com"
	}
	parsed, err := url.Parse(config.APIBaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid GitHub API base URL")
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	return &Adapter{baseURL: parsed, token: strings.TrimSpace(config.Token), endpointKeys: config.EndpointKeys, client: client}, nil
}

// Resolve 只接受 github.com 的无凭据 HTTPS Clone URL，并确认仓库公开且 Branch 存在。
func (a *Adapter) Resolve(ctx context.Context, request pipeline.SourceRequest) (pipeline.SourceIdentity, error) {
	if _, ok := a.endpointKeys[request.EndpointKey]; !ok {
		return pipeline.SourceIdentity{}, errors.New("github webhook endpoint is not configured")
	}
	owner, repository, err := parseRepositoryURL(request.RepositoryURL)
	if err != nil {
		return pipeline.SourceIdentity{}, err
	}
	record, err := a.repository(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository))
	if err != nil {
		return pipeline.SourceIdentity{}, err
	}
	return a.withHead(ctx, record, request.Branch)
}

// Head 先按稳定 Repository ID 解析当前名称，再校验 Owner 和 Branch Head，支持同 Owner 改名。
func (a *Adapter) Head(ctx context.Context, request pipeline.HeadRequest) (pipeline.SourceIdentity, error) {
	if request.RepositoryID <= 0 || request.OwnerID <= 0 || !strings.HasPrefix(request.GitRef, "refs/heads/") {
		return pipeline.SourceIdentity{}, errors.New("invalid GitHub head request")
	}
	record, err := a.repository(ctx, "/repositories/"+strconv.FormatInt(request.RepositoryID, 10))
	if err != nil {
		return pipeline.SourceIdentity{}, err
	}
	if record.Owner.ID != request.OwnerID {
		return pipeline.SourceIdentity{}, pipeline.ErrSourceOwnerChanged
	}
	return a.withHead(ctx, record, strings.TrimPrefix(request.GitRef, "refs/heads/"))
}

type repositoryResponse struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	Private  bool   `json:"private"`
	Owner    struct {
		ID int64 `json:"id"`
	} `json:"owner"`
}

type branchResponse struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

func (a *Adapter) repository(ctx context.Context, endpoint string) (repositoryResponse, error) {
	var result repositoryResponse
	if err := a.get(ctx, endpoint, &result); err != nil {
		return result, err
	}
	if result.ID <= 0 || result.Owner.ID <= 0 || result.FullName == "" || result.CloneURL == "" || result.Private {
		return result, errors.New("GitHub repository is not a supported public repository")
	}
	return result, nil
}

func (a *Adapter) withHead(ctx context.Context, repository repositoryResponse, branch string) (pipeline.SourceIdentity, error) {
	parts := strings.Split(repository.FullName, "/")
	if len(parts) != 2 {
		return pipeline.SourceIdentity{}, errors.New("GitHub repository full name is invalid")
	}
	var result branchResponse
	if err := a.get(ctx, "/repos/"+url.PathEscape(parts[0])+"/"+url.PathEscape(parts[1])+"/branches/"+url.PathEscape(branch), &result); err != nil {
		return pipeline.SourceIdentity{}, err
	}
	if result.Name != branch || result.Commit.SHA == "" {
		return pipeline.SourceIdentity{}, errors.New("GitHub branch response is invalid")
	}
	return pipeline.SourceIdentity{RepositoryID: repository.ID, OwnerID: repository.Owner.ID, RepositoryName: repository.FullName, RepositoryURL: repository.CloneURL, GitRef: "refs/heads/" + branch, HeadCommit: strings.ToLower(result.Commit.SHA)}, nil
}

func (a *Adapter) get(ctx context.Context, endpoint string, destination any) error {
	requestURL := strings.TrimRight(a.baseURL.String(), "/") + "/" + strings.TrimLeft(path.Clean(endpoint), "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("build GitHub request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if a.token != "" {
		request.Header.Set("Authorization", "Bearer "+a.token)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", pipeline.ErrSourceUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode == http.StatusNotFound {
			return pipeline.ErrSourceNotFound
		}
		return fmt.Errorf("%w: status %d", pipeline.ErrSourceUnavailable, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1024*1024))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: decode response: %v", pipeline.ErrSourceUnavailable, err)
	}
	return nil
}

func parseRepositoryURL(value string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("repository URL must be a credential-free github.com HTTPS URL")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("repository URL must identify one GitHub repository")
	}
	return parts[0], parts[1], nil
}
