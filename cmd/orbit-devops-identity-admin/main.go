// orbit-devops-identity-admin 提供显式、离线且可审计的身份恢复与迁移入口。
// 它不启动 HTTP 服务，不加载 Kubernetes、Redis、Registry 或 OIDC Client Secret。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "identity maintenance failed:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("command is required: initialize-admin, freeze-user, recover-admin, recover-project-owner, claim-legacy-members, configure-provider")
	}
	databaseURL := strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("ORBIT_DEVOPS_DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	database, err := sqlx.ConnectContext(ctx, "pgx", databaseURL)
	if err != nil {
		return errors.New("database unavailable")
	}
	defer database.Close()
	module, err := identity.New(database, projectauth.NewOwnershipGuard())
	if err != nil {
		return err
	}
	switch arguments[0] {
	case "initialize-admin":
		return initializeAdmin(ctx, module, arguments[1:], stdout, stderr)
	case "freeze-user":
		return freezeUser(ctx, module, arguments[1:], stdout)
	case "recover-admin":
		return recoverAdmin(ctx, module, arguments[1:], stdout, stderr)
	case "recover-project-owner":
		return recoverProjectOwner(ctx, module, arguments[1:], stdout)
	case "claim-legacy-members":
		return claimLegacyMembers(ctx, module, arguments[1:], stdout)
	case "configure-provider":
		return configureProvider(ctx, module, arguments[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func initializeAdmin(ctx context.Context, module *identity.Module, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("initialize-admin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	loginName := flags.String("login-name", "", "initial administrator login name")
	displayName := flags.String("display-name", "", "initial administrator display name")
	reference := flags.String("maintenance-ref", "", "operator change or incident reference")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("initialize-admin does not accept positional arguments")
	}
	password, err := readConfirmedPassword(stderr)
	if err != nil {
		return err
	}
	defer clear(password)
	user, err := module.InitializeAdmin(ctx, identity.InitializeAdminCommand{LoginName: *loginName,
		DisplayName: *displayName, Password: string(password), MaintenanceRef: *reference})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "initialized user %s\n", user.ID)
	return err
}

func freezeUser(ctx context.Context, module *identity.Module, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("freeze-user", flag.ContinueOnError)
	userValue := flags.String("user-id", "", "exact user UUID")
	reference := flags.String("maintenance-ref", "", "operator change or incident reference")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	userID, err := parseRequiredUUID(*userValue)
	if err != nil {
		return err
	}
	if err := module.FreezeUser(ctx, identity.FreezeUserCommand{UserID: userID, MaintenanceRef: *reference}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "froze user %s\n", userID)
	return err
}

func recoverAdmin(ctx context.Context, module *identity.Module, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("recover-admin", flag.ContinueOnError)
	userValue := flags.String("user-id", "", "existing user UUID; omit to create a recovery user")
	loginName := flags.String("login-name", "", "local login name")
	displayName := flags.String("display-name", "", "display name for a newly created recovery user")
	reference := flags.String("maintenance-ref", "", "operator change or incident reference")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	var userID uuid.UUID
	var err error
	if strings.TrimSpace(*userValue) != "" {
		userID, err = uuid.Parse(*userValue)
		if err != nil {
			return errors.New("user-id must be an exact UUID")
		}
	}
	password, err := readConfirmedPassword(stderr)
	if err != nil {
		return err
	}
	defer clear(password)
	user, err := module.RecoverAdmin(ctx, identity.RecoverAdminCommand{UserID: userID, LoginName: *loginName,
		DisplayName: *displayName, Password: string(password), MaintenanceRef: *reference})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "recovered administrator %s\n", user.ID)
	return err
}

func recoverProjectOwner(ctx context.Context, module *identity.Module, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("recover-project-owner", flag.ContinueOnError)
	projectValue := flags.String("project-id", "", "exact project UUID")
	userValue := flags.String("user-id", "", "exact effective user UUID")
	reference := flags.String("maintenance-ref", "", "operator change or incident reference")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	projectID, err := parseRequiredUUID(*projectValue)
	if err != nil {
		return fmt.Errorf("project-id: %w", err)
	}
	userID, err := parseRequiredUUID(*userValue)
	if err != nil {
		return fmt.Errorf("user-id: %w", err)
	}
	if err := module.RecoverProjectOwner(ctx, identity.RecoverProjectOwnerCommand{ProjectID: projectID,
		UserID: userID, MaintenanceRef: *reference}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "recovered owner %s for project %s\n", userID, projectID)
	return err
}

func claimLegacyMembers(ctx context.Context, module *identity.Module, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("claim-legacy-members", flag.ContinueOnError)
	mappingFile := flags.String("mapping-file", "", "JSON file containing actorId/userId mappings")
	execute := flags.Bool("execute", false, "commit the validated claims; omission performs dry-run")
	reference := flags.String("maintenance-ref", "", "operator change or incident reference")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*mappingFile) == "" {
		return errors.New("mapping-file is required")
	}
	payload, err := os.ReadFile(*mappingFile)
	if err != nil {
		return errors.New("mapping-file cannot be read")
	}
	var mappings []identity.LegacyClaimMapping
	if err := json.Unmarshal(payload, &mappings); err != nil {
		return errors.New("mapping-file must be a JSON array of actorId/userId objects")
	}
	report, err := module.ClaimLegacyMembers(ctx, identity.ClaimLegacyMembersCommand{Mappings: mappings,
		Execute: *execute, MaintenanceRef: *reference})
	if err != nil {
		return err
	}
	mode := "dry-run"
	if *execute {
		mode = "executed"
	}
	_, err = fmt.Fprintf(stdout, "%s: %d projects, %d legacy memberships\n", mode, report.Projects, report.Members)
	return err
}

func configureProvider(ctx context.Context, module *identity.Module, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("configure-provider", flag.ContinueOnError)
	id := flags.String("id", "", "stable provider ID")
	displayName := flags.String("display-name", "", "provider display name")
	issuer := flags.String("issuer", "", "stable HTTPS issuer URL")
	allowLoopback := flags.Bool("allow-insecure-loopback", false, "allow an HTTP issuer only on this host's loopback interface")
	clientID := flags.String("client-id", "", "OIDC client ID")
	secretRef := flags.String("client-secret-ref", "", "deployment secret reference")
	enabled := flags.Bool("enabled", false, "enable this provider")
	reference := flags.String("maintenance-ref", "", "operator change or incident reference")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	provider, err := module.ConfigureProvider(ctx, identity.ConfigureProviderCommand{ID: *id, DisplayName: *displayName,
		Issuer: *issuer, AllowInsecureLoopback: *allowLoopback, ClientID: *clientID,
		ClientSecretRef: *secretRef, Enabled: *enabled, MaintenanceRef: *reference})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "configured provider %s enabled=%t\n", provider.ID, provider.Enabled)
	return err
}

func readConfirmedPassword(stderr io.Writer) ([]byte, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errors.New("password input requires a terminal")
	}
	_, _ = fmt.Fprint(stderr, "Password: ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(stderr)
	if err != nil {
		return nil, errors.New("password could not be read")
	}
	_, _ = fmt.Fprint(stderr, "Confirm password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(stderr)
	if err != nil {
		clear(first)
		return nil, errors.New("password confirmation could not be read")
	}
	defer clear(second)
	if string(first) != string(second) {
		clear(first)
		return nil, errors.New("passwords do not match")
	}
	return first, nil
}

func parseRequiredUUID(value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, errors.New("must be an exact non-zero UUID")
	}
	return parsed, nil
}
