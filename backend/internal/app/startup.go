package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	backendMigrations "cpa-helper/backend/migrations"

	"github.com/pressly/goose/v3"
)

var (
	ErrDatabaseNotInitialized = errors.New("database is not initialized")
	ErrDatabaseNeedsMigration = errors.New("database needs migration")
	ErrDatabaseTooNew         = errors.New("database migration version is newer than this application")
	ErrAppSettingsMissing     = errors.New("app settings row is missing")
)

type NewOptions struct {
	Migrate         bool
	RequireReady    bool
	StartBackground bool
}

type RuntimePaths struct {
	RepoRoot string
	DataDir  string
	DBDir    string
	DBPath   string
}

type MigrationReport struct {
	DBPath          string
	PreviousVersion int64
	CurrentVersion  int64
	TargetVersion   int64
}

type StartupCheck struct {
	DBPath         string
	CurrentVersion int64
	TargetVersion  int64
}

func Migrate(ctx context.Context) (MigrationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := resolveRuntimePaths()
	if err != nil {
		return MigrationReport{}, err
	}
	db, err := openRuntimeDB(paths, false)
	if err != nil {
		return MigrationReport{}, err
	}
	defer db.Close()

	before, err := currentMigrationVersion(ctx, db)
	if err != nil && !errors.Is(err, ErrDatabaseNotInitialized) {
		return MigrationReport{DBPath: paths.DBPath, TargetVersion: backendMigrations.LatestVersion}, err
	}
	app := &App{db: db}
	if err := app.runMigrations(ctx); err != nil {
		return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, TargetVersion: backendMigrations.LatestVersion}, err
	}
	after, err := currentMigrationVersion(ctx, db)
	if err != nil {
		return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, TargetVersion: backendMigrations.LatestVersion}, err
	}
	return MigrationReport{
		DBPath:          paths.DBPath,
		PreviousVersion: before,
		CurrentVersion:  after,
		TargetVersion:   backendMigrations.LatestVersion,
	}, nil
}

// allowedRollbackTargets whitelists the goose versions `migrate down-to` may roll back
// to. A downgrade is destructive — its Down migrations drop the columns/tables (and the
// data in them) added since the target — so only explicitly reviewed rollback targets
// are permitted, never an arbitrary version.
var allowedRollbackTargets = map[int64]bool{
	202609040002: true, // pre-reset-credit-consume baseline (see docs/migrations-rollback.md)
}

func sortedRollbackTargets() []int64 {
	targets := make([]int64, 0, len(allowedRollbackTargets))
	for v := range allowedRollbackTargets {
		targets = append(targets, v)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
	return targets
}

// pendingRedeemCount returns how many in-flight (pending) rows the redeem ledger holds,
// or 0 when the table does not exist yet (a partially-migrated DB below 202609060003).
func pendingRedeemCount(ctx context.Context, db *sql.DB) (int, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='codex_keeper_reset_redeems'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_keeper_reset_redeems WHERE status = ?`, keeperRedeemStatusPending).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MigrateDownTo rolls the schema DOWN to target, which MUST be an allowlisted rollback
// target. It refuses to act when target is not whitelisted or is not strictly below the
// current version. DESTRUCTIVE: the Down migrations drop everything added since target
// (for the reset-credit-consume release that includes subscription_active_until and its
// data). By default it also REFUSES when the redeem ledger holds any pending (unresolved,
// unknown-outcome) redeem, because dropping the ledger loses that redeem's unique
// idempotency key — a later re-upgrade + reset would mint a NEW key and could double
// consume. Pass allowPending=true only after reconciling those redeems and backing up.
//
// This is an OFFLINE operation: stop the CPA-Helper service first. The pending-redeem
// check is a pre-flight guard, NOT atomic protection — a still-running service could
// create a pending redeem between the check and the drop. Quiescence is the operator's
// responsibility per docs/migrations-rollback.md.
func MigrateDownTo(ctx context.Context, target int64, allowPending bool) (MigrationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !allowedRollbackTargets[target] {
		return MigrationReport{}, fmt.Errorf("refusing to roll back to unlisted version %d; allowed targets: %v", target, sortedRollbackTargets())
	}
	paths, err := resolveRuntimePaths()
	if err != nil {
		return MigrationReport{}, err
	}
	db, err := openRuntimeDB(paths, false)
	if err != nil {
		return MigrationReport{}, err
	}
	defer db.Close()

	before, err := currentMigrationVersion(ctx, db)
	if err != nil {
		return MigrationReport{DBPath: paths.DBPath, TargetVersion: target}, err
	}
	if before <= target {
		return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, CurrentVersion: before, TargetVersion: target},
			fmt.Errorf("current version %d is not newer than target %d; nothing to roll back", before, target)
	}
	if !allowPending {
		pending, perr := pendingRedeemCount(ctx, db)
		if perr != nil {
			return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, TargetVersion: target}, perr
		}
		if pending > 0 {
			return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, TargetVersion: target},
				fmt.Errorf("refusing to roll back: %d pending redeem(s) in codex_keeper_reset_redeems whose idempotency keys would be lost; reconcile them, back up, then re-run with --allow-pending", pending)
		}
	}
	goose.SetBaseFS(backendMigrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, TargetVersion: target}, err
	}
	if err := goose.DownToContext(ctx, db, ".", target); err != nil {
		return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, TargetVersion: target}, err
	}
	after, err := currentMigrationVersion(ctx, db)
	if err != nil {
		return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, TargetVersion: target}, err
	}
	return MigrationReport{DBPath: paths.DBPath, PreviousVersion: before, CurrentVersion: after, TargetVersion: target}, nil
}

func CheckStartup(ctx context.Context) (StartupCheck, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := resolveRuntimePaths()
	if err != nil {
		return StartupCheck{}, err
	}
	return checkStartupPaths(ctx, paths)
}

func (a *App) Readiness(ctx context.Context) (StartupCheck, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return checkDatabaseReady(ctx, a.db, filepath.Join(a.dataDir, "db", "cpa_helper.sqlite3"))
}

func resolveRuntimePaths() (RuntimePaths, error) {
	repoRoot, err := detectRepoRoot()
	if err != nil {
		return RuntimePaths{}, err
	}
	dataDir := os.Getenv("CPA_HELPER_DATA_DIR")
	if strings.TrimSpace(dataDir) == "" {
		dataDir = filepath.Join(repoRoot, "data")
	}
	dbDir := filepath.Join(dataDir, "db")
	return RuntimePaths{
		RepoRoot: repoRoot,
		DataDir:  dataDir,
		DBDir:    dbDir,
		DBPath:   filepath.Join(dbDir, "cpa_helper.sqlite3"),
	}, nil
}

func openRuntimeDB(paths RuntimePaths, readOnly bool) (*sql.DB, error) {
	if readOnly {
		if _, err := os.Stat(paths.DBPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: SQLite database file is missing; run `cpa-helper migrate`", ErrDatabaseNotInitialized)
			}
			return nil, err
		}
	} else if err := os.MkdirAll(paths.DBDir, 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", sqliteDSN(paths.DBPath, readOnly))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func sqliteDSN(dbPath string, readOnly bool) string {
	if readOnly {
		return "file:" + filepath.ToSlash(dbPath) + "?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	}
	return dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
}

func checkStartupPaths(ctx context.Context, paths RuntimePaths) (StartupCheck, error) {
	db, err := openRuntimeDB(paths, true)
	if err != nil {
		return StartupCheck{DBPath: paths.DBPath, TargetVersion: backendMigrations.LatestVersion}, err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return StartupCheck{DBPath: paths.DBPath, TargetVersion: backendMigrations.LatestVersion}, err
	}
	return checkDatabaseReady(ctx, db, paths.DBPath)
}

func checkDatabaseReady(ctx context.Context, db *sql.DB, dbPath string) (StartupCheck, error) {
	current, err := currentMigrationVersion(ctx, db)
	report := StartupCheck{
		DBPath:         dbPath,
		CurrentVersion: current,
		TargetVersion:  backendMigrations.LatestVersion,
	}
	if err != nil {
		return report, err
	}
	if current < backendMigrations.LatestVersion {
		return report, fmt.Errorf("%w: current version %d, target version %d; run `cpa-helper migrate`", ErrDatabaseNeedsMigration, current, backendMigrations.LatestVersion)
	}
	if current > backendMigrations.LatestVersion {
		return report, fmt.Errorf("%w: current version %d, target version %d", ErrDatabaseTooNew, current, backendMigrations.LatestVersion)
	}
	if err := requireSchemaShape(ctx, db); err != nil {
		return report, err
	}
	if err := requireAppSettingsRow(ctx, db); err != nil {
		return report, err
	}
	return report, nil
}

func currentMigrationVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		if strings.Contains(err.Error(), "no such table: goose_db_version") {
			return 0, fmt.Errorf("%w: goose_db_version table is missing", ErrDatabaseNotInitialized)
		}
		return 0, err
	}
	if !version.Valid || version.Int64 <= 0 {
		return 0, ErrDatabaseNotInitialized
	}
	return version.Int64, nil
}

func requireSchemaShape(ctx context.Context, db *sql.DB) error {
	required := []struct {
		table  string
		column string
	}{
		{"app_settings", "session_secret"},
		{"users", "username"},
		{"usage_records", "dedupe_key"},
		{"usage_records", "ttft_ms"},
		{"model_prices", "request_usd"},
		{"user_quota_charges", "lifetime_deducted_usd"},
		{"user_card_shop_favorites", "shop_key"},
		{"user_card_shop_tags", "tag"},
	}
	for _, item := range required {
		ok, err := columnExists(ctx, db, item.table, item.column)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: missing schema column %s.%s; run `cpa-helper migrate`", ErrDatabaseNeedsMigration, item.table, item.column)
		}
	}
	return nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func requireAppSettingsRow(ctx context.Context, db *sql.DB) error {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_settings WHERE id = 1`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: app_settings id=1 is missing; run `cpa-helper migrate`", ErrAppSettingsMissing)
	}
	return nil
}
