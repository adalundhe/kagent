// Scratch evidence helper for the kagent-dev/kagent#3395 review. Not part of any PR.
// Admits crash recovery with database.Client.BeginCrashedSessionRecovery, the
// call RevertSession makes, without executing it inline.
// Usage: zz-rv3395-admit <dsn> <session-id> <actor-uid> <atespace> <snapshot-uri> <fidelity>
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/pkg/postgres"
)

func main() {
	if len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: zz-rv3395-admit <dsn> <session-id> <actor-uid> <atespace> <snapshot-uri> <fidelity>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Connect as the controller does: assume its read/write role and use its schema.
	config, err := postgres.PoolConfig(os.Args[1], "kagent_readwrite")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	config.ConnConfig.RuntimeParams["search_path"] = "kagent"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()
	store := database.NewClient(pool, "public")
	sessionID := os.Args[2]
	generation, err := store.SessionLifecycleGeneration(ctx, sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generation:", err)
		os.Exit(1)
	}
	operation, err := store.BeginCrashedSessionRecovery(ctx, sessionID, generation, os.Args[3], database.SessionTaskSnapshot{
		Atespace: os.Args[4], URI: os.Args[5], Fidelity: os.Args[6],
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "admit:", err)
		os.Exit(1)
	}
	fmt.Printf("admitted operation %s: state=%s operation=%s executor=%s\n", operation.ID, operation.Instance.State, operation.Instance.Operation, operation.ExecutorID)
}
