package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentledgerhq/agentledger/pkg/api"
	"github.com/agentledgerhq/agentledger/pkg/db"
	"github.com/agentledgerhq/agentledger/pkg/ledger"
	"github.com/agentledgerhq/agentledger/pkg/vault"
	"github.com/awnumar/memguard"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var rootCmd = &cobra.Command{
	Use:   "agentledger",
	Short: "AgentLedger Lite - The ledger and credential vault for AI agents",
}

// dbPath returns the database file path from AGENTLEDGER_DB_PATH or the default.
func dbPath() string {
	if p := os.Getenv("AGENTLEDGER_DB_PATH"); p != "" {
		return p
	}
	return "agentledger.db"
}

// generateToken creates a random hex token with the given prefix.
func generateToken(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	token := prefix + hex.EncodeToString(b)
	vault.Zeroize(b)
	return token, nil
}

var supervisorTierFlag int

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize the local SQLite database and environment",
	Run: func(cmd *cobra.Command, args []string) {
		// Validate tier flag
		if supervisorTierFlag < 1 || supervisorTierFlag > 3 {
			fmt.Println("Error: --supervisor-tier must be 1, 2, or 3.")
			os.Exit(1)
		}

		// Tier 3 requires explicit human acknowledgement
		if supervisorTierFlag == 3 {
			fmt.Println()
			fmt.Println("⚠  WARNING: Supervisor Tier 3 enables full delegation, including vault management.")
			fmt.Println("   A supervisor agent with this configuration can replace the vaulted card.")
			fmt.Println("   This is intentional and supported — you are choosing full agent delegation.")
			fmt.Println("   AgentLedger's security model at Tier 3 is entirely the security of your")
			fmt.Println("   supervisor agent and its token.")
			fmt.Println()
			fmt.Print("   Type CONFIRM to proceed: ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			if strings.TrimSpace(scanner.Text()) != "CONFIRM" {
				fmt.Println("Aborted. Re-run without --supervisor-tier 3 or type CONFIRM to accept.")
				os.Exit(1)
			}
			fmt.Println()
		}

		// Generate master key
		key, err := vault.GenerateKey()
		if err != nil {
			fmt.Printf("Failed to generate master key: %v\n", err)
			os.Exit(1)
		}
		keyHex := hex.EncodeToString(key)
		vault.Zeroize(key)

		userToken, err := generateToken("alu_")
		if err != nil {
			fmt.Printf("Failed to generate user token: %v\n", err)
			os.Exit(1)
		}
		supervisorToken, err := generateToken("als_")
		if err != nil {
			fmt.Printf("Failed to generate supervisor token: %v\n", err)
			os.Exit(1)
		}

		webhookSecret, err := generateToken("whsec_")
		if err != nil {
			fmt.Printf("Failed to generate webhook secret: %v\n", err)
			os.Exit(1)
		}

		envContent := fmt.Sprintf(
			"AGENTLEDGER_MASTER_KEY=%s\n"+
				"AGENTLEDGER_USER_TOKEN=%s\n"+
				"AGENTLEDGER_SUPERVISOR_TOKEN=%s\n"+
				"AGENTLEDGER_SUPERVISOR_TIER=%d\n"+
				"APPROVAL_MODE=auto\n"+
				"AGENTLEDGER_APPROVAL_TTL=30m\n"+
				"# AGENTLEDGER_HOST=127.0.0.1\n"+
				"# AGENTLEDGER_PORT=9119\n"+
				"# TRANSACTION_CALLBACK_URL=\n"+
				"AGENTLEDGER_WEBHOOK_SECRET=%s\n",
			keyHex, userToken, supervisorToken, supervisorTierFlag, webhookSecret,
		)
		if err := os.WriteFile(".env", []byte(envContent), 0600); err != nil {
			fmt.Printf("Failed to write .env file: %v\n", err)
			os.Exit(1)
		}

		os.Setenv("AGENTLEDGER_MASTER_KEY", keyHex)
		os.Setenv("AGENTLEDGER_USER_TOKEN", userToken)
		os.Setenv("AGENTLEDGER_SUPERVISOR_TOKEN", supervisorToken)
		database, err := db.InitDB(dbPath())
		if err != nil {
			fmt.Printf("Failed to initialize database: %v\n", err)
			os.Exit(1)
		}
		defer database.Close()

		// Permanent audit log entry for tier 3 consent
		if supervisorTierFlag == 3 {
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
			slog.Info("Supervisor Tier 3 initialized with explicit human consent",
				"supervisor_tier", 3, "vault_access", true, "acknowledged", true)
		}

		fmt.Println("Initialization complete.")
		fmt.Printf("Supervisor tier: %d\n", supervisorTierFlag)
		fmt.Println("Created .env with:")
		fmt.Println("  AGENTLEDGER_USER_TOKEN       — for user agents (/authorize, /budget GET, /status, /transactions)")
		fmt.Printf("  AGENTLEDGER_SUPERVISOR_TOKEN — for supervisor agents (approve, deny, list_pending")
		if supervisorTierFlag >= 2 {
			fmt.Printf(", set_budget, credit")
		}
		if supervisorTierFlag >= 3 {
			fmt.Printf(", vault_update")
		}
		fmt.Println(")")
		fmt.Println("Created agentledger.db")
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the local HTTP API server",
	Run: func(cmd *cobra.Command, args []string) {
		godotenv.Load()
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

		masterKey, err := vault.LoadMasterKey()
		if err != nil {
			fmt.Printf("Error loading master key: %v\n", err)
			os.Exit(1)
		}

		// Validate tokens
		if os.Getenv("AGENTLEDGER_USER_TOKEN") == "" {
			fmt.Println("Error: AGENTLEDGER_USER_TOKEN not set. Did you run 'agentledger init'?")
			os.Exit(1)
		}
		if os.Getenv("AGENTLEDGER_SUPERVISOR_TOKEN") == "" {
			fmt.Println("Error: AGENTLEDGER_SUPERVISOR_TOKEN not set. Did you run 'agentledger init'?")
			os.Exit(1)
		}

		database, err := db.InitDB(dbPath())
		if err != nil {
			fmt.Printf("Failed to open database: %v\n", err)
			os.Exit(1)
		}
		defer database.Close()

		l := ledger.NewLedger(database, masterKey)

		approvalMode := os.Getenv("APPROVAL_MODE")
		if approvalMode == "" {
			approvalMode = "auto"
		}
		if approvalMode != "auto" && approvalMode != "manual" {
			fmt.Printf("Invalid APPROVAL_MODE: %s (must be 'auto' or 'manual')\n", approvalMode)
			os.Exit(1)
		}
		l.ApprovalMode = approvalMode

		approvalTTL := 30 * time.Minute
		if ttlStr := os.Getenv("AGENTLEDGER_APPROVAL_TTL"); ttlStr != "" {
			if d, err := time.ParseDuration(ttlStr); err == nil {
				approvalTTL = d
			} else {
				fmt.Printf("Invalid AGENTLEDGER_APPROVAL_TTL %q, using default 30m: %v\n", ttlStr, err)
			}
		}
		l.ApprovalTTL = approvalTTL

		callbackURL := os.Getenv("TRANSACTION_CALLBACK_URL")
		webhookSecret := os.Getenv("AGENTLEDGER_WEBHOOK_SECRET")

		supervisorTier := 1
		if tierStr := os.Getenv("AGENTLEDGER_SUPERVISOR_TIER"); tierStr != "" {
			if t, err := strconv.Atoi(tierStr); err == nil {
				supervisorTier = t
			} else {
				fmt.Printf("Invalid AGENTLEDGER_SUPERVISOR_TIER: %v\n", err)
				os.Exit(1)
			}
		}

		// Drain any stale pending transactions from a previous run, regardless
		// of the current approval mode. This prevents zombie reservations when
		// switching from manual to auto mode between restarts.
		if err := l.DrainStalePending(); err != nil {
			slog.Warn("Failed to drain stale pending transactions on startup", "error", err)
		}

		if approvalMode == "manual" {
			l.RunExpirySweeper()
		}

		server, err := api.NewServer(l, callbackURL, webhookSecret)
		if err != nil {
			fmt.Printf("Error configuring server: %v\n", err)
			os.Exit(1)
		}

		mux := http.NewServeMux()
		server.RegisterRoutes(mux)

		host := "127.0.0.1"
		if h := os.Getenv("AGENTLEDGER_HOST"); h != "" {
			host = h
		}
		port := "9119"
		if p := os.Getenv("AGENTLEDGER_PORT"); p != "" {
			port = p
		}
		slog.Info("Starting AgentLedger server",
			"host", host,
			"port", port,
			"approval_mode", approvalMode,
			"approval_ttl", approvalTTL.String(),
			"supervisor_tier", supervisorTier,
			"callback_url_configured", callbackURL != "",
			"webhook_hmac_configured", webhookSecret != "",
		)

		httpServer := &http.Server{
			Addr:    host + ":" + port,
			Handler: mux,
		}

		// Graceful shutdown: listen for SIGINT/SIGTERM, drain in-flight
		// requests, then close the database cleanly.
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Server failed", "error", err)
				os.Exit(1)
			}
		}()

		<-quit
		slog.Info("Shutting down server (10s grace period)...")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			slog.Error("Server forced to shutdown", "error", err)
		}
		slog.Info("Server stopped")
	},
}

var vaultCmd = &cobra.Command{
	Use:   "vault",
	Short: "Manage the local credential vault",
}

var testCardFlag bool

// promptSecret prompts for sensitive input with hidden echo, then zeroizes the raw bytes.
func promptSecret(label string) (string, error) {
	fmt.Printf("Enter %s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	vault.Zeroize(b)
	return s, nil
}

var vaultAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add credit card details to the local vault",
	Run: func(cmd *cobra.Command, args []string) {
		godotenv.Load()
		masterKey, err := vault.LoadMasterKey()
		if err != nil {
			fmt.Printf("Error loading master key. Did you run 'agentledger init'? %v\n", err)
			os.Exit(1)
		}

		database, err := db.InitDB(dbPath())
		if err != nil {
			fmt.Printf("Failed to open database: %v\n", err)
			os.Exit(1)
		}
		defer database.Close()

		payload := make(map[string]string)

		if testCardFlag {
			payload["holder_name"] = "Test Cardholder"
			payload["card_number"] = "4242424242424242"
			payload["expiry_date"] = "12/29"
			payload["cvv"] = "123"
			payload["country"] = "US"
			payload["zip_code"] = "10001"
			fmt.Println("Using test card details.")
		} else {
			fields := []struct{ key, label string }{
				{"holder_name", "Cardholder Name"},
				{"card_number", "Card Number"},
				{"expiry_date", "Expiry Date (MM/YY)"},
				{"cvv", "CVV"},
				{"country", "Country Code (e.g. US, GB, DE)"},
				{"zip_code", "ZIP / Postal Code"},
			}
			for _, f := range fields {
				val, err := promptSecret(f.label)
				if err != nil {
					fmt.Printf("Error reading %s: %v\n", f.label, err)
					os.Exit(1)
				}
				payload[f.key] = val
			}
		}

		payloadBytes, _ := json.Marshal(payload)
		encrypted, err := vault.Encrypt(masterKey, payloadBytes)
		vault.Zeroize(payloadBytes)
		if err != nil {
			fmt.Printf("Encryption failed: %v\n", err)
			os.Exit(1)
		}

		_, err = database.Exec(`
			INSERT INTO vault (id, encrypted_payload) VALUES (1, ?)
			ON CONFLICT(id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload;
		`, encrypted)
		if err != nil {
			fmt.Printf("Failed to store encrypted payload: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("Successfully encrypted and vaulted credentials.")
	},
}

var budgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Manage local budgets",
}

var budgetPeriodFlag string

var budgetSetCmd = &cobra.Command{
	Use:   "set [agent_id] [amount_in_cents]",
	Short: "Set the budget for an agent (optionally with --period monthly)",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		agentID := args[0]
		amountStr := args[1]
		amountCent, err := strconv.ParseInt(amountStr, 10, 64)
		if err != nil {
			fmt.Println("Amount must be a valid integer representing cents.")
			os.Exit(1)
		}
		if amountCent < 0 {
			fmt.Println("Budget amount cannot be negative.")
			os.Exit(1)
		}

		godotenv.Load()
		database, err := db.InitDB(dbPath())
		if err != nil {
			fmt.Printf("Failed to open database: %v\n", err)
			os.Exit(1)
		}
		defer database.Close()

		l := ledger.NewLedger(database, nil)
		period := budgetPeriodFlag
		if period == "" {
			period = "cumulative"
		}
		if _, err := l.SetBudget(agentID, amountCent, period); err != nil {
			fmt.Printf("Failed to set budget: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Budget for agent '%s' set to %d cents (period: %s).\n", agentID, amountCent, period)
	},
}

// txAction sends a POST to the running server using the supervisor token.
func txAction(action, txID string) {
	godotenv.Load()
	supervisorToken := os.Getenv("AGENTLEDGER_SUPERVISOR_TOKEN")
	if supervisorToken == "" {
		fmt.Println("Error: AGENTLEDGER_SUPERVISOR_TOKEN not set. Did you run 'agentledger init'?")
		os.Exit(1)
	}

	payload, _ := json.Marshal(map[string]string{"transaction_id": txID})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:9119/"+action, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+supervisorToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Failed to connect to server. Is 'agentledger serve' running? %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Printf("Transaction %s %sed.\n", txID, action)
	} else {
		fmt.Printf("Action failed (HTTP %d): %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		os.Exit(1)
	}
}

var approveCmd = &cobra.Command{
	Use:   "approve [transaction_id]",
	Short: "Approve a pending transaction (requires running server, uses supervisor token)",
	Args:  cobra.ExactArgs(1),
	Run:   func(cmd *cobra.Command, args []string) { txAction("approve", args[0]) },
}

var denyCmd = &cobra.Command{
	Use:   "deny [transaction_id]",
	Short: "Deny a pending transaction (requires running server, uses supervisor token)",
	Args:  cobra.ExactArgs(1),
	Run:   func(cmd *cobra.Command, args []string) { txAction("deny", args[0]) },
}

func main() {
	memguard.CatchInterrupt()
	defer memguard.Purge()

	initCmd.Flags().IntVar(&supervisorTierFlag, "supervisor-tier", 1,
		"Supervisor delegation tier (1=approve/deny, 2=+budget/credit, 3=+vault). Tier 3 requires interactive confirmation.")
	vaultAddCmd.Flags().BoolVar(&testCardFlag, "test-card", false, "Use a fake card for E2E testing (bypasses terminal prompt)")
	budgetSetCmd.Flags().StringVar(&budgetPeriodFlag, "period", "cumulative", "Budget period: 'cumulative' (no reset) or 'monthly' (auto-resets each month)")

	vaultCmd.AddCommand(vaultAddCmd)
	budgetCmd.AddCommand(budgetSetCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(vaultCmd)
	rootCmd.AddCommand(budgetCmd)
	rootCmd.AddCommand(approveCmd)
	rootCmd.AddCommand(denyCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		memguard.SafeExit(1)
	}
}
