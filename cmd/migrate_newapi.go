package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/migration/newapi"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

var migrateNewAPIFlags struct {
	sourceType        string
	sourceDSN         string
	sourceLogType     string
	sourceLogDSN      string
	targetType        string
	targetDSN         string
	apply             bool
	includeLogs       bool
	includeAPIKeys    bool
	quotaPerUnit      float64
	conflictStrategy  string
	passwordMode      string
	apiKeyPrefix      string
	preserveAdminRole bool
	batchSize         int
	jsonOutput        bool
	debugSQL          bool
}

var migrateNewAPICmd = &cobra.Command{
	Use:   "migrate-newapi",
	Short: "Migrate active New API users into Octopus",
	Long: strings.TrimSpace(`
Migrate users from a New API database into Octopus.

The active-user filter is intentionally strict for migration cleanup:
only users with at least one New API consume log (logs.type=2) are imported.
Zero-usage registration spam is skipped. The current migration policy imports
users, remaining balance, and usage-summary notes only. Historical detailed logs
and old New API token secrets are intentionally not copied, so Octopus stays fast
and users create fresh Octopus API keys after migration.
`),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMigrateNewAPI(cmd.Context())
	},
}

func init() {
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.sourceType, "source-type", "sqlite", "New API database type: sqlite, mysql, postgres")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.sourceDSN, "source-dsn", "", "New API main database path or DSN")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.sourceLogType, "source-log-type", "", "New API log database type; defaults to --source-type")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.sourceLogDSN, "source-log-dsn", "", "New API log database path or DSN; defaults to --source-dsn")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.targetType, "target-type", "sqlite", "Octopus target database type: sqlite, mysql, postgres")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.targetDSN, "target-dsn", "data/octopus.db", "Octopus target database path or DSN")
	migrateNewAPICmd.Flags().BoolVar(&migrateNewAPIFlags.apply, "apply", false, "actually write to the Octopus target database; without this flag the command is a dry-run")
	migrateNewAPICmd.Flags().BoolVar(&migrateNewAPIFlags.includeLogs, "include-logs", false, "deprecated and ignored: detailed New API logs are not imported in summary-only mode")
	migrateNewAPICmd.Flags().BoolVar(&migrateNewAPIFlags.includeAPIKeys, "include-api-keys", false, "deprecated and ignored: New API tokens are not imported in summary-only mode")
	migrateNewAPICmd.Flags().Float64Var(&migrateNewAPIFlags.quotaPerUnit, "quota-per-unit", 0, "New API quota unit; 0 reads options.QuotaPerUnit or falls back to 500000")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.conflictStrategy, "conflict", "skip", "username conflict handling: skip, merge, rename")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.passwordMode, "password-mode", "preserve", "password handling: preserve bcrypt hash, random reset hash, or disabled")
	migrateNewAPICmd.Flags().StringVar(&migrateNewAPIFlags.apiKeyPrefix, "api-key-prefix", "sk-octopus-newapi-", "deprecated and ignored in summary-only mode")
	migrateNewAPICmd.Flags().BoolVar(&migrateNewAPIFlags.preserveAdminRole, "preserve-admin-role", false, "preserve New API admin/root roles; off by default so migrated accounts become normal users")
	migrateNewAPICmd.Flags().IntVar(&migrateNewAPIFlags.batchSize, "batch-size", 500, "database batch size")
	migrateNewAPICmd.Flags().BoolVar(&migrateNewAPIFlags.jsonOutput, "json", false, "print machine-readable JSON summary")
	migrateNewAPICmd.Flags().BoolVar(&migrateNewAPIFlags.debugSQL, "debug-sql", false, "print SQL debug logs")
	rootCmd.AddCommand(migrateNewAPICmd)
}

func runMigrateNewAPI(ctx context.Context) error {
	if strings.TrimSpace(migrateNewAPIFlags.sourceDSN) == "" {
		return fmt.Errorf("--source-dsn is required")
	}
	sourceDB, closeSource, err := newapi.OpenDatabase(migrateNewAPIFlags.sourceType, migrateNewAPIFlags.sourceDSN, migrateNewAPIFlags.debugSQL)
	if err != nil {
		return fmt.Errorf("open New API source database: %w", err)
	}
	defer closeSource()

	sourceLogDB := sourceDB
	var closeSourceLog func() error
	if strings.TrimSpace(migrateNewAPIFlags.sourceLogDSN) != "" && migrateNewAPIFlags.sourceLogDSN != migrateNewAPIFlags.sourceDSN {
		sourceLogType := migrateNewAPIFlags.sourceLogType
		if strings.TrimSpace(sourceLogType) == "" {
			sourceLogType = migrateNewAPIFlags.sourceType
		}
		sourceLogDB, closeSourceLog, err = newapi.OpenDatabase(sourceLogType, migrateNewAPIFlags.sourceLogDSN, migrateNewAPIFlags.debugSQL)
		if err != nil {
			return fmt.Errorf("open New API log database: %w", err)
		}
		defer closeSourceLog()
	}

	targetDB, closeTarget, err := openMigrateTarget(ctx)
	if err != nil {
		return err
	}
	if closeTarget != nil {
		defer closeTarget()
	}

	summary, err := newapi.Run(ctx, newapi.Config{
		SourceDB:          sourceDB,
		SourceLogDB:       sourceLogDB,
		TargetDB:          targetDB,
		Apply:             migrateNewAPIFlags.apply,
		IncludeLogs:       migrateNewAPIFlags.includeLogs,
		IncludeAPIKeys:    migrateNewAPIFlags.includeAPIKeys,
		QuotaPerUnit:      migrateNewAPIFlags.quotaPerUnit,
		BatchSize:         migrateNewAPIFlags.batchSize,
		ConflictStrategy:  migrateNewAPIFlags.conflictStrategy,
		PasswordMode:      migrateNewAPIFlags.passwordMode,
		APIKeyPrefix:      migrateNewAPIFlags.apiKeyPrefix,
		PreserveAdminRole: migrateNewAPIFlags.preserveAdminRole,
	})
	if err != nil {
		return err
	}

	if migrateNewAPIFlags.jsonOutput {
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	}
	printMigrateNewAPISummary(summary)
	return nil
}

func openMigrateTarget(ctx context.Context) (*gorm.DB, func() error, error) {
	if migrateNewAPIFlags.apply {
		if err := db.InitDB(migrateNewAPIFlags.targetType, migrateNewAPIFlags.targetDSN, migrateNewAPIFlags.debugSQL); err != nil {
			return nil, nil, fmt.Errorf("init Octopus target database: %w", err)
		}
		if err := op.UserInit(); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("init Octopus users: %w", err)
		}
		return db.GetDB(), db.Close, nil
	}

	if strings.EqualFold(migrateNewAPIFlags.targetType, "sqlite") || strings.EqualFold(migrateNewAPIFlags.targetType, "sqlite3") {
		targetPath := strings.TrimSpace(migrateNewAPIFlags.targetDSN)
		if targetPath != "" && !strings.HasPrefix(targetPath, "file:") && !strings.Contains(targetPath, "?") {
			if _, err := os.Stat(targetPath); err != nil {
				if os.IsNotExist(err) {
					return nil, nil, nil
				}
				return nil, nil, fmt.Errorf("stat Octopus target database: %w", err)
			}
		}
	}
	targetDB, closeTarget, err := newapi.OpenDatabase(migrateNewAPIFlags.targetType, migrateNewAPIFlags.targetDSN, migrateNewAPIFlags.debugSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("open Octopus target database for dry-run: %w", err)
	}
	_ = ctx
	return targetDB, closeTarget, nil
}

func printMigrateNewAPISummary(summary *newapi.Summary) {
	mode := "DRY-RUN"
	if !summary.DryRun {
		mode = "APPLIED"
	}
	fmt.Printf("New API -> Octopus migration %s\n", mode)
	fmt.Printf("quota_per_unit: %.0f (%s)\n", summary.QuotaPerUnit, summary.SourceReference)
	fmt.Printf("users: source=%d active=%d skipped_inactive=%d created=%d merged=%d renamed=%d conflict_skipped=%d invalid_skipped=%d\n",
		summary.SourceUsers,
		summary.ActiveUsers,
		summary.InactiveUsersSkipped,
		summary.UsersCreated,
		summary.UsersMerged,
		summary.UsersRenamed,
		summary.UsersSkippedConflict,
		summary.UsersSkippedInvalid,
	)
	fmt.Printf("api_keys: considered=%d created=%d skipped=%d prefix=%s\n",
		summary.APIKeysConsidered,
		summary.APIKeysCreated,
		summary.APIKeysSkipped,
		summary.ImportedAPIKeyPrefix,
	)
	fmt.Printf("logs: considered=%d created=%d skipped=%d stats_updated=%t\n",
		summary.LogsConsidered,
		summary.LogsCreated,
		summary.LogsSkipped,
		summary.StatsUpdated,
	)
	fmt.Printf("quota: imported_balance=%.6f imported_used_quota=%.6f imported_log_cost=%.6f\n",
		summary.ImportedBalance,
		summary.ImportedUsedQuota,
		summary.ImportedLogCost,
	)
	if len(summary.Warnings) > 0 {
		fmt.Println("warnings:")
		for _, warning := range summary.Warnings {
			fmt.Println("- " + warning)
		}
	}
}
