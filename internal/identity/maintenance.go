package identity

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type FreezeUserCommand struct {
	UserID         uuid.UUID
	MaintenanceRef string
}

// FreezeUser 是离线应急能力，允许暂时打破最后管理员/owner；不删除历史或成员。
func (m *Module) FreezeUser(ctx context.Context, command FreezeUserCommand) error {
	if command.UserID == uuid.Nil || !validLabel(command.MaintenanceRef, 256) {
		return ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := userForSecurityChange(ctx, tx, command.UserID); err != nil {
		return err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	// 最后 owner 被应急冻结时，显式隔离受影响项目；恢复命令重新建立 owner 后才解除隔离。
	frozen, err := tx.ExecContext(ctx, `UPDATE projects p SET identity_state='frozen'
		WHERE p.identity_state='governed'
		AND EXISTS(SELECT 1 FROM project_members pm WHERE pm.project_id=p.id AND pm.user_id=$1 AND pm.role='owner')
		AND NOT EXISTS(SELECT 1 FROM project_members pm JOIN effective_identity_users e ON e.id=pm.user_id
			WHERE pm.project_id=p.id AND pm.role='owner' AND pm.user_id<>$1)`, command.UserID)
	if err != nil {
		return dependencyError(err)
	}
	frozenCount, err := frozen.RowsAffected()
	if err != nil {
		return dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET status='disabled',updated_at=$2 WHERE id=$1`, command.UserID, now); err != nil {
		return dependencyError(err)
	}
	if err := advanceAndRevoke(ctx, tx, command.UserID, now); err != nil {
		return err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: maintenanceActor, ActorKind: audit.ActorKindSystem,
		Action: "identity.freeze_user", TargetType: "user", TargetID: command.UserID,
		Summary: map[string]any{"maintenance_ref": command.MaintenanceRef, "frozen_projects": frozenCount}, CreatedAt: now}); err != nil {
		return dependencyError(err)
	}
	return dependencyErrorOrNil(tx.Commit())
}

type RecoverAdminCommand struct {
	UserID         uuid.UUID
	LoginName      string
	DisplayName    string
	Password       string `json:"-"`
	MaintenanceRef string
}

func (RecoverAdminCommand) String() string   { return "RecoverAdminCommand{redacted}" }
func (RecoverAdminCommand) GoString() string { return "RecoverAdminCommand{redacted}" }

// RecoverAdmin 是永久初始化后的独立恢复路径；可以恢复明确 User 或创建明确的新管理员。
func (m *Module) RecoverAdmin(ctx context.Context, command RecoverAdminCommand) (User, error) {
	loginName, err := normalizeLoginName(command.LoginName)
	if err != nil || !validLabel(command.MaintenanceRef, 256) || command.UserID == uuid.Nil && !validLabel(command.DisplayName, 128) {
		return User{}, ErrInvalidCommand
	}
	hash, err := m.hashPassword(ctx, command.Password)
	if err != nil {
		return User{}, err
	}
	tx, initialized, err := m.beginControlled(ctx, true)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if !initialized.Valid {
		return User{}, ErrInvalidCommand
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return User{}, err
	}
	var user User
	if command.UserID == uuid.Nil {
		user = User{ID: uuid.New(), DisplayName: command.DisplayName, Status: "active", PlatformRole: RolePlatformAdmin, CreatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users
			(id,display_name,status,platform_role,admitted_by,admitted_at,created_at,updated_at)
			VALUES($1,$2,'active','platform_admin',$3,$4,$4,$4)`, user.ID, user.DisplayName, maintenanceActor, now); err != nil {
			return User{}, dependencyError(err)
		}
	} else {
		user, err = userForSecurityChange(ctx, tx, command.UserID)
		if err != nil {
			return User{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET status='active',platform_role='platform_admin',updated_at=$2 WHERE id=$1`, user.ID, now); err != nil {
			return User{}, dependencyError(err)
		}
		user.Status, user.PlatformRole = "active", RolePlatformAdmin
	}
	var existingLogin sql.NullString
	if err := tx.GetContext(ctx, &existingLogin, `SELECT login_name FROM local_credentials WHERE user_id=$1`, user.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return User{}, dependencyError(err)
	}
	if existingLogin.Valid && existingLogin.String != loginName {
		return User{}, ErrInvalidCommand
	}
	result, err := tx.ExecContext(ctx, `UPDATE local_credentials SET password_hash=$2,must_change_password=true,
		updated_at=$3 WHERE user_id=$1`, user.ID, hash, now)
	if err != nil {
		return User{}, dependencyError(err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_credentials
			(user_id,login_name,password_hash,must_change_password,updated_at) VALUES($1,$2,$3,true,$4)`, user.ID, loginName, hash, now); err != nil {
			if isUniqueViolation(err) {
				return User{}, ErrLoginNameConflict
			}
			return User{}, dependencyError(err)
		}
	}
	if err := advanceAndRevoke(ctx, tx, user.ID, now); err != nil {
		return User{}, err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: maintenanceActor, ActorKind: audit.ActorKindSystem,
		Action: "identity.recover_admin", TargetType: "user", TargetID: user.ID,
		Summary: map[string]any{"maintenance_ref": command.MaintenanceRef}, CreatedAt: now}); err != nil {
		return User{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, dependencyError(err)
	}
	return user, nil
}

type RecoverProjectOwnerCommand struct {
	ProjectID      uuid.UUID
	UserID         uuid.UUID
	MaintenanceRef string
}

func (m *Module) RecoverProjectOwner(ctx context.Context, command RecoverProjectOwnerCommand) error {
	if command.ProjectID == uuid.Nil || command.UserID == uuid.Nil || !validLabel(command.MaintenanceRef, 256) {
		return ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var effective bool
	if err := tx.GetContext(ctx, &effective, `SELECT EXISTS(SELECT 1 FROM effective_identity_users WHERE id=$1)`, command.UserID); err != nil {
		return dependencyError(err)
	}
	if !effective {
		return ErrInvalidCommand
	}
	var state string
	if err := tx.GetContext(ctx, &state, `SELECT identity_state FROM projects WHERE id=$1 FOR UPDATE`, command.ProjectID); err != nil {
		return dependencyError(err)
	}
	if state == "legacy" {
		return ErrInvalidCommand
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_members
		(project_id,user_id,role,created_by,created_at,updated_at) VALUES($1,$2,'owner',$3,$4,$4)
		ON CONFLICT(project_id,user_id) DO UPDATE SET role='owner',updated_at=EXCLUDED.updated_at`,
		command.ProjectID, command.UserID, maintenanceActor, now); err != nil {
		return dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET identity_state='governed' WHERE id=$1`, command.ProjectID); err != nil {
		return dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: maintenanceActor, ActorKind: audit.ActorKindSystem,
		Action: "identity.recover_project_owner", TargetType: "project", TargetID: command.ProjectID,
		Summary: map[string]any{"user_id": command.UserID.String(), "maintenance_ref": command.MaintenanceRef}, CreatedAt: now}); err != nil {
		return dependencyError(err)
	}
	return dependencyErrorOrNil(tx.Commit())
}

type LegacyClaimMapping struct {
	ActorID string    `json:"actorId"`
	UserID  uuid.UUID `json:"userId"`
}
type ClaimLegacyMembersCommand struct {
	Mappings       []LegacyClaimMapping
	Execute        bool
	MaintenanceRef string
}
type LegacyClaimReport struct {
	Projects int
	Members  int
}

func (m *Module) ClaimLegacyMembers(ctx context.Context, command ClaimLegacyMembersCommand) (LegacyClaimReport, error) {
	if len(command.Mappings) == 0 || !validLabel(command.MaintenanceRef, 256) {
		return LegacyClaimReport{}, ErrInvalidCommand
	}
	mapping := make(map[string]uuid.UUID, len(command.Mappings))
	for _, item := range command.Mappings {
		actor := strings.TrimSpace(item.ActorID)
		if actor == "" || item.UserID == uuid.Nil {
			return LegacyClaimReport{}, ErrInvalidCommand
		}
		if existing, ok := mapping[actor]; ok && existing != item.UserID {
			return LegacyClaimReport{}, ErrInvalidCommand
		}
		mapping[actor] = item.UserID
	}
	tx, _, err := m.beginControlled(ctx, command.Execute)
	if err != nil {
		return LegacyClaimReport{}, err
	}
	defer func() { _ = tx.Rollback() }()
	actors := make([]string, 0, len(mapping))
	for actor := range mapping {
		actors = append(actors, actor)
	}
	sort.Strings(actors)
	query, args, err := sqlx.In(`SELECT DISTINCT project_id FROM legacy_project_members
		WHERE actor_id IN (?) ORDER BY project_id`, actors)
	if err != nil {
		return LegacyClaimReport{}, err
	}
	projectIDs := make([]uuid.UUID, 0)
	if err := tx.SelectContext(ctx, &projectIDs, tx.Rebind(query), args...); err != nil {
		return LegacyClaimReport{}, dependencyError(err)
	}
	if len(projectIDs) == 0 {
		return LegacyClaimReport{}, ErrLegacyClaimConflict
	}
	lockClause := "FOR SHARE"
	if command.Execute {
		lockClause = "FOR UPDATE"
	}
	query, args, err = sqlx.In(`SELECT id FROM projects WHERE id IN (?) AND identity_state<>'frozen'
		ORDER BY id `+lockClause, projectIDs)
	if err != nil {
		return LegacyClaimReport{}, err
	}
	lockedProjects := make([]uuid.UUID, 0, len(projectIDs))
	if err := tx.SelectContext(ctx, &lockedProjects, tx.Rebind(query), args...); err != nil {
		return LegacyClaimReport{}, dependencyError(err)
	}
	if len(lockedProjects) != len(projectIDs) {
		return LegacyClaimReport{}, ErrLegacyClaimConflict
	}
	type legacyRow struct {
		ProjectID uuid.UUID  `db:"project_id"`
		ActorID   string     `db:"actor_id"`
		Role      string     `db:"role"`
		Claimed   *uuid.UUID `db:"claimed_user_id"`
	}
	query, args, err = sqlx.In(`SELECT project_id,actor_id,role,claimed_user_id FROM legacy_project_members
		WHERE project_id IN (?) ORDER BY project_id,actor_id `+lockClause, lockedProjects)
	if err != nil {
		return LegacyClaimReport{}, err
	}
	rows := make([]legacyRow, 0)
	if err := tx.SelectContext(ctx, &rows, tx.Rebind(query), args...); err != nil {
		return LegacyClaimReport{}, dependencyError(err)
	}
	foundActors := make(map[string]struct{}, len(mapping))
	roles := make(map[uuid.UUID]map[uuid.UUID]string, len(lockedProjects))
	allClaimed := true
	for _, row := range rows {
		userID, mapped := mapping[row.ActorID]
		if !mapped {
			return LegacyClaimReport{}, ErrLegacyClaimConflict
		}
		foundActors[row.ActorID] = struct{}{}
		if row.Claimed != nil && *row.Claimed != userID {
			return LegacyClaimReport{}, ErrLegacyClaimConflict
		}
		allClaimed = allClaimed && row.Claimed != nil
		var valid bool
		if err := tx.GetContext(ctx, &valid, `SELECT EXISTS(SELECT 1 FROM effective_identity_users WHERE id=$1)`, userID); err != nil {
			return LegacyClaimReport{}, dependencyError(err)
		}
		if !valid {
			return LegacyClaimReport{}, ErrInvalidCommand
		}
		if row.Role != "owner" && row.Role != "developer" && row.Role != "viewer" {
			return LegacyClaimReport{}, ErrLegacyClaimConflict
		}
		if roles[row.ProjectID] == nil {
			roles[row.ProjectID] = make(map[uuid.UUID]string)
		}
		if existing, ok := roles[row.ProjectID][userID]; ok && existing != row.Role {
			return LegacyClaimReport{}, ErrLegacyClaimConflict
		}
		roles[row.ProjectID][userID] = row.Role
	}
	if len(foundActors) != len(mapping) {
		return LegacyClaimReport{}, ErrLegacyClaimConflict
	}
	for _, projectID := range lockedProjects {
		hasOwner := false
		for _, role := range roles[projectID] {
			hasOwner = hasOwner || role == "owner"
		}
		if !hasOwner {
			return LegacyClaimReport{}, ErrLastProjectOwner
		}
	}
	report := LegacyClaimReport{Projects: len(lockedProjects), Members: len(rows)}
	if !command.Execute {
		if err := tx.Commit(); err != nil {
			return LegacyClaimReport{}, dependencyError(err)
		}
		return report, nil
	}
	if allClaimed {
		if err := tx.Commit(); err != nil {
			return LegacyClaimReport{}, dependencyError(err)
		}
		return report, nil
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return LegacyClaimReport{}, err
	}
	for _, projectID := range lockedProjects {
		userIDs := make([]uuid.UUID, 0, len(roles[projectID]))
		for userID := range roles[projectID] {
			userIDs = append(userIDs, userID)
		}
		sort.Slice(userIDs, func(i, j int) bool { return userIDs[i].String() < userIDs[j].String() })
		for _, userID := range userIDs {
			role := roles[projectID][userID]
			var existingRole sql.NullString
			err := tx.GetContext(ctx, &existingRole, `SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, userID)
			if err == nil && existingRole.String != role {
				return LegacyClaimReport{}, ErrLegacyClaimConflict
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return LegacyClaimReport{}, dependencyError(err)
			}
			if !existingRole.Valid {
				if _, err := tx.ExecContext(ctx, `INSERT INTO project_members
					(project_id,user_id,role,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`,
					projectID, userID, role, maintenanceActor, now); err != nil {
					return LegacyClaimReport{}, dependencyError(err)
				}
			}
		}
	}
	for _, row := range rows {
		userID := mapping[row.ActorID]
		if _, err := tx.ExecContext(ctx, `UPDATE legacy_project_members SET claimed_user_id=$3,claimed_by=$4,claimed_at=$5
			WHERE project_id=$1 AND actor_id=$2 AND (claimed_user_id IS NULL OR claimed_user_id=$3)`,
			row.ProjectID, row.ActorID, userID, maintenanceActor, now); err != nil {
			return LegacyClaimReport{}, dependencyError(err)
		}
	}
	for _, projectID := range lockedProjects {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET identity_state='governed' WHERE id=$1`, projectID); err != nil {
			return LegacyClaimReport{}, dependencyError(err)
		}
		if err := audit.Append(ctx, tx, audit.Entry{ActorID: maintenanceActor, ActorKind: audit.ActorKindSystem,
			Action: "identity.claim_legacy_members", TargetType: "project", TargetID: projectID,
			Summary: map[string]any{"maintenance_ref": command.MaintenanceRef}, CreatedAt: now}); err != nil {
			return LegacyClaimReport{}, dependencyError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return LegacyClaimReport{}, dependencyError(err)
	}
	return report, nil
}

type ConfigureProviderCommand struct {
	ID                    string
	DisplayName           string
	Issuer                string
	AllowInsecureLoopback bool
	ClientID              string
	ClientSecretRef       string
	Enabled               bool
	MaintenanceRef        string
}

func (m *Module) ConfigureProvider(ctx context.Context, command ConfigureProviderCommand) (OIDCProvider, error) {
	if !validProviderCommand(command) {
		return OIDCProvider{}, ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return OIDCProvider{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current OIDCProvider
	err = tx.GetContext(ctx, &current, `SELECT id,display_name,issuer,allow_insecure_loopback,client_id,client_secret_ref,enabled,created_at
		FROM auth_providers WHERE id=$1 FOR UPDATE`, command.ID)
	existed := err == nil
	wasEnabled := existed && current.Enabled
	now, nowErr := databaseNow(ctx, tx)
	if nowErr != nil {
		return OIDCProvider{}, nowErr
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_providers
			(id,display_name,issuer,allow_insecure_loopback,client_id,client_secret_ref,enabled,created_by,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, command.ID, command.DisplayName, command.Issuer,
			command.AllowInsecureLoopback, command.ClientID, command.ClientSecretRef, command.Enabled, maintenanceActor, now)
		current = OIDCProvider{ID: command.ID, DisplayName: command.DisplayName, Issuer: command.Issuer,
			AllowInsecureLoopback: command.AllowInsecureLoopback, ClientID: command.ClientID,
			SecretRef: command.ClientSecretRef, Enabled: command.Enabled, CreatedAt: now}
	} else if err != nil {
		return OIDCProvider{}, dependencyError(err)
	} else {
		if current.Issuer != command.Issuer || current.AllowInsecureLoopback != command.AllowInsecureLoopback {
			return OIDCProvider{}, ErrInvalidCommand
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_providers SET display_name=$2,client_id=$3,client_secret_ref=$4,
			enabled=$5,updated_at=$6 WHERE id=$1`, command.ID, command.DisplayName, command.ClientID, command.ClientSecretRef, command.Enabled, now)
		current.DisplayName, current.ClientID, current.SecretRef, current.Enabled = command.DisplayName, command.ClientID, command.ClientSecretRef, command.Enabled
	}
	if err != nil {
		if isUniqueViolation(err) {
			return OIDCProvider{}, ErrInvalidCommand
		}
		return OIDCProvider{}, dependencyError(err)
	}
	// 只有把已启用提供方关闭才可能移除有效登录入口；创建禁用政策不改变任何事实。
	if wasEnabled && !command.Enabled {
		var admins, unsafeProjects int
		if err := tx.GetContext(ctx, &admins, `SELECT count(*) FROM effective_identity_users e JOIN users u ON u.id=e.id WHERE u.platform_role='platform_admin'`); err != nil {
			return OIDCProvider{}, dependencyError(err)
		}
		if err := tx.GetContext(ctx, &unsafeProjects, `SELECT count(*) FROM projects p WHERE p.identity_state='governed'
			AND NOT EXISTS(SELECT 1 FROM project_members pm JOIN effective_identity_users e ON e.id=pm.user_id
			 WHERE pm.project_id=p.id AND pm.role='owner')`); err != nil {
			return OIDCProvider{}, dependencyError(err)
		}
		if admins == 0 {
			return OIDCProvider{}, ErrLastAdministrator
		}
		if unsafeProjects > 0 {
			return OIDCProvider{}, ErrLastProjectOwner
		}
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: maintenanceActor, ActorKind: audit.ActorKindSystem,
		Action: "identity.configure_provider", TargetType: "auth_provider",
		TargetID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(command.ID)),
		Summary:  map[string]any{"enabled": command.Enabled, "maintenance_ref": command.MaintenanceRef}, CreatedAt: now}); err != nil {
		return OIDCProvider{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return OIDCProvider{}, dependencyError(err)
	}
	return current, nil
}

func validProviderCommand(command ConfigureProviderCommand) bool {
	issuer, err := url.Parse(command.Issuer)
	if err != nil || issuer.User != nil || issuer.Hostname() == "" || issuer.RawQuery != "" || issuer.Fragment != "" ||
		issuer.Opaque != "" || strings.TrimRight(command.Issuer, "/") != command.Issuer {
		return false
	}
	secure := issuer.Scheme == "https"
	if !secure {
		ip := net.ParseIP(issuer.Hostname())
		loopback := strings.EqualFold(issuer.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
		if issuer.Scheme != "http" || !command.AllowInsecureLoopback || !loopback {
			return false
		}
	} else if command.AllowInsecureLoopback {
		return false
	}
	return len(command.ID) >= 2 && len(command.ID) <= 63 && validLabel(command.DisplayName, 128) &&
		validLabel(command.ClientID, 256) && validLabel(command.ClientSecretRef, 128) && validLabel(command.MaintenanceRef, 256)
}
