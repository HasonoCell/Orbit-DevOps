// Package pipeline 管理从可信源码事件到既有 Build 与 Release 领域的自动交付编排。
package pipeline

import (
	"context"
	"errors"
)

var ErrSourceUnavailable = errors.New("git source inspector is unavailable")

// SourceRequest 只包含解析 GitHub 仓库和分支所需的配置，不携带凭据。
type SourceRequest struct {
	EndpointKey   string
	RepositoryURL string
	Branch        string
}

// SourceIdentity 是 Git Provider 返回的稳定仓库身份和当前定位信息。
type SourceIdentity struct {
	RepositoryID   int64
	OwnerID        int64
	RepositoryName string
	RepositoryURL  string
	GitRef         string
	HeadCommit     string
}

// GitSourceInspector 隔离 Git Provider 网络协议；数据库只保存解析后的稳定身份。
type GitSourceInspector interface {
	Resolve(context.Context, SourceRequest) (SourceIdentity, error)
}

type unavailableInspector struct{}

func (unavailableInspector) Resolve(context.Context, SourceRequest) (SourceIdentity, error) {
	return SourceIdentity{}, ErrSourceUnavailable
}
