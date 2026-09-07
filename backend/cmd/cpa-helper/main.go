package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	backendApp "cpa-helper/backend/internal/app"
	"cpa-helper/backend/internal/httpserver"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		log.Fatalf("%v", err)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	command := "start"
	if len(args) > 0 {
		command = strings.TrimSpace(args[0])
	}
	switch command {
	case "", "start":
		if _, err := backendAddr(); err != nil {
			return err
		}
		report, err := backendApp.Migrate(ctx)
		if err != nil {
			return fmt.Errorf("migrate before start: %w", err)
		}
		log.Printf("migration check completed: db_version=%d target_version=%d", report.CurrentVersion, report.TargetVersion)
		return serve(ctx)
	case "serve":
		return serve(ctx)
	case "migrate":
		// `migrate down-to <version>` rolls the schema DOWN to an allowlisted rollback
		// target (destructive; see docs/migrations-rollback.md). Bare `migrate` runs Up.
		// A destructive subcommand rejects unknown subcommands/flags rather than silently
		// falling back to Up or ignoring a typo.
		if len(args) >= 2 {
			sub := strings.TrimSpace(args[1])
			if sub != "down-to" {
				printUsage(stdout)
				return fmt.Errorf("unknown migrate subcommand %q", sub)
			}
			if len(args) < 3 {
				printUsage(stdout)
				return fmt.Errorf("migrate down-to requires a target version")
			}
			target, err := strconv.ParseInt(strings.TrimSpace(args[2]), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid target version %q: %w", args[2], err)
			}
			allowPending := false
			for _, extra := range args[3:] {
				switch strings.TrimSpace(extra) {
				case "--allow-pending":
					allowPending = true
				default:
					printUsage(stdout)
					return fmt.Errorf("unknown flag %q for migrate down-to", extra)
				}
			}
			report, err := backendApp.MigrateDownTo(ctx, target, allowPending)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "rollback completed: db=%s previous_version=%d current_version=%d target_version=%d\n", report.DBPath, report.PreviousVersion, report.CurrentVersion, report.TargetVersion)
			return nil
		}
		report, err := backendApp.Migrate(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "migration completed: db=%s previous_version=%d current_version=%d target_version=%d\n", report.DBPath, report.PreviousVersion, report.CurrentVersion, report.TargetVersion)
		return nil
	case "doctor":
		report, err := backendApp.CheckStartup(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "ready: db=%s current_version=%d target_version=%d\n", report.DBPath, report.CurrentVersion, report.TargetVersion)
		return nil
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stdout)
		return fmt.Errorf("unknown command %q", command)
	}
}

func serve(ctx context.Context) error {
	addr, err := backendAddr()
	if err != nil {
		return err
	}
	report, err := backendApp.CheckStartup(ctx)
	if err != nil {
		return fmt.Errorf("startup check failed: %w", err)
	}
	log.Printf("startup check passed: db_version=%d target_version=%d", report.CurrentVersion, report.TargetVersion)

	app, err := backendApp.NewWithOptions(ctx, backendApp.NewOptions{
		RequireReady:    true,
		StartBackground: true,
	})
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer app.Close()

	if err := httpserver.Run(ctx, httpserver.Config{
		Addr:    addr,
		Handler: app.Routes(),
	}); err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return nil
}

func backendAddr() (string, error) {
	addr := strings.TrimSpace(os.Getenv("CPA_HELPER_ADDR"))
	if addr == "" {
		addr = ":18317"
	}
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid CPA_HELPER_ADDR %q: use host:port or :port", addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid CPA_HELPER_ADDR %q: port must be between 1 and 65535", addr)
	}
	return addr, nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  cpa-helper            Run migrations, then start the service
  cpa-helper start      Run migrations, then start the service
  cpa-helper migrate    Run database migrations (Up) and exit
  cpa-helper migrate down-to <version>
                        Roll the schema DOWN to an allowlisted version (destructive)
  cpa-helper serve      Start only after read-only startup checks pass
  cpa-helper doctor     Run read-only startup checks and exit
`)
}
