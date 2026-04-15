package ledger

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/agentledgerhq/agentledger/pkg/models"
	"github.com/agentledgerhq/agentledger/pkg/vault"
	"github.com/awnumar/memguard"
	"github.com/google/uuid"
)

// MaxAuthorizeAmount caps a single authorize request at $1,000,000 (in cents).
// Anything above this is rejected before arithmetic, keeping `used + amount`
// safely below math.MaxInt64 and preventing signed-overflow budget bypass.
const MaxAuthorizeAmount int64 = 1_000_000_00

type Ledger struct {
	db           *sql.DB
	key          *memguard.Enclave
	ApprovalMode string        // "auto" or "manual"
	ApprovalTTL  time.Duration // How long a pending transaction waits for human approval
}

func NewLedger(db *sql.DB, masterKey *memguard.Enclave) *Ledger {
	return &Ledger{db: db, key: masterKey, ApprovalMode: "auto", ApprovalTTL: 30 * time.Minute}
}

// decryptCard fetches and decrypts the vaulted card data.
// The caller receives Go strings whose backing arrays can be zeroed via
// CardData.Zeroize(). Because WithCard copies string headers (shared backing
// arrays), zeroing is deferred to the HTTP handler layer — after the response
// has been serialized and flushed to the wire.
func (l *Ledger) decryptCard() (*models.CardData, error) {
	var encrypted string
	err := l.db.QueryRow("SELECT encrypted_payload FROM vault WHERE id = 1").Scan(&encrypted)
	if err == sql.ErrNoRows {
		return nil, errors.New("no card in vault")
	} else if err != nil {
		return nil, err
	}

	plaintext, err := vault.Decrypt(l.key, encrypted)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}
	defer vault.Zeroize(plaintext)

	var card models.CardData
	if err := json.Unmarshal(plaintext, &card); err != nil {
		return nil, fmt.Errorf("invalid card payload format: %w", err)
	}
	return &card, nil
}

// startOfMonth returns midnight UTC on the 1st of the given time's month.
func startOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// checkAndResetPeriod checks if a monthly budget needs a period reset.
// If the current time is in a later month than period_start, it resets
// used_amount to 0 and advances period_start to the start of the current month.
// This is called within a transaction to ensure atomicity.
func checkAndResetPeriod(tx *sql.Tx, agentID string) error {
	var period string
	var periodStart time.Time
	err := tx.QueryRow("SELECT period, period_start FROM budgets WHERE agent_id = ?", agentID).
		Scan(&period, &periodStart)
	if err != nil {
		return err // sql.ErrNoRows is fine to propagate
	}

	if period != "monthly" {
		return nil // cumulative budgets never reset
	}

	now := time.Now().UTC()
	nextPeriod := startOfMonth(periodStart).AddDate(0, 1, 0)

	if now.Before(nextPeriod) {
		return nil // still within current period
	}

	// Reset: new period starts at the beginning of the current month
	newPeriodStart := startOfMonth(now)
	_, err = tx.Exec("UPDATE budgets SET used_amount = 0, period_start = ? WHERE agent_id = ?",
		newPeriodStart, agentID)
	return err
}

func (l *Ledger) GetBudget(agentID string) (*models.Budget, error) {
	tx, err := l.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Auto-reset monthly budgets if needed
	err = checkAndResetPeriod(tx, agentID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	var b models.Budget
	err = tx.QueryRow("SELECT agent_id, limit_amount, used_amount, period, period_start FROM budgets WHERE agent_id = ?", agentID).
		Scan(&b.AgentID, &b.Limit, &b.Used, &b.Period, &b.PeriodStart)
	if err == sql.ErrNoRows {
		return &models.Budget{AgentID: agentID, Limit: 0, Used: 0, Period: "cumulative"}, nil
	}
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &b, nil
}

func (l *Ledger) SetBudget(agentID string, limit int64, period string) (*models.Budget, error) {
	if limit < 0 {
		return nil, fmt.Errorf("budget limit cannot be negative")
	}
	if period == "" {
		period = "cumulative"
	}
	if period != "cumulative" && period != "monthly" {
		return nil, fmt.Errorf("invalid period: %s (must be 'cumulative' or 'monthly')", period)
	}

	periodStart := time.Now().UTC()
	if period == "monthly" {
		periodStart = startOfMonth(periodStart)
	}

	_, err := l.db.Exec(`
		INSERT INTO budgets (agent_id, limit_amount, used_amount, period, period_start)
		VALUES (?, ?, 0, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET limit_amount=excluded.limit_amount, period=excluded.period, period_start=excluded.period_start;
	`, agentID, limit, period, periodStart)
	if err != nil {
		return nil, err
	}

	// Read back the actual state (used_amount may have been preserved on update)
	var b models.Budget
	err = l.db.QueryRow("SELECT agent_id, limit_amount, used_amount, period, period_start FROM budgets WHERE agent_id = ?", agentID).
		Scan(&b.AgentID, &b.Limit, &b.Used, &b.Period, &b.PeriodStart)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (l *Ledger) Authorize(req models.AuthorizeRequest) (*models.AuthorizeResponse, error) {
	tx, err := l.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Serialize metadata once for potential storage
	var metadataJSON *string
	if req.Metadata != nil {
		b, err := json.Marshal(req.Metadata)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize metadata: %w", err)
		}
		s := string(b)
		metadataJSON = &s
	}

	// insertDenied records a denied transaction for the audit trail and commits.
	insertDenied := func(reason string) (*models.AuthorizeResponse, error) {
		txID := uuid.New().String()
		_, err := tx.Exec(`
			INSERT INTO transactions (id, agent_id, amount, merchant, purpose, status, reason, metadata)
			VALUES (?, ?, ?, ?, ?, 'denied', ?, ?)
		`, txID, req.RequestingAgentID, req.Amount, req.Merchant, req.Purpose, reason, metadataJSON)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &models.AuthorizeResponse{Status: "denied", TransactionID: txID, Reason: reason}, nil
	}

	if req.Amount <= 0 {
		return insertDenied("Amount must be strictly positive")
	}
	if req.Amount > MaxAuthorizeAmount {
		return insertDenied("Amount exceeds per-request maximum")
	}

	// Auto-reset monthly budgets if the period has elapsed
	if err := checkAndResetPeriod(tx, req.RequestingAgentID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	// 1. Check budget
	var limit, used int64
	err = tx.QueryRow("SELECT limit_amount, used_amount FROM budgets WHERE agent_id = ?", req.RequestingAgentID).Scan(&limit, &used)
	if err == sql.ErrNoRows {
		return insertDenied("No budget allocated for agent")
	} else if err != nil {
		return nil, err
	}

	// Overflow-safe: limit >= 0 (enforced by SetBudget) and used >= 0
	// (refunds clamp via MAX(..., 0)), so this form cannot wrap.
	if req.Amount > limit-used {
		return insertDenied("Insufficient budget")
	}

	// 2. Reserve funds
	newUsed := used + req.Amount
	_, err = tx.Exec("UPDATE budgets SET used_amount = ? WHERE agent_id = ?", newUsed, req.RequestingAgentID)
	if err != nil {
		return nil, err
	}

	txID := uuid.New().String()

	// 3. Branch on approval mode
	if l.ApprovalMode == "manual" {
		// Manual mode: insert as pending, do NOT release card data yet.
		// Budget is already reserved above; it is refunded if denied or expired.
		expiresAt := time.Now().UTC().Add(l.ApprovalTTL)
		_, err = tx.Exec(`
			INSERT INTO transactions (id, agent_id, amount, merchant, purpose, status, expires_at, metadata)
			VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)
		`, txID, req.RequestingAgentID, req.Amount, req.Merchant, req.Purpose, expiresAt, metadataJSON)
		if err != nil {
			return nil, err
		}

		if err := tx.Commit(); err != nil {
			return nil, err
		}

		return &models.AuthorizeResponse{
			Status:        "pending",
			TransactionID: txID,
			ExpiresAt:     &expiresAt,
		}, nil
	}

	// Auto mode: decrypt card BEFORE committing so that a decrypt failure
	// does not leave the budget consumed with no card data returned.
	card, err := l.decryptCard()
	if err != nil {
		return nil, fmt.Errorf("card decryption failed, budget not consumed: %w", err)
	}

	_, err = tx.Exec(`
		INSERT INTO transactions (id, agent_id, amount, merchant, purpose, status, metadata)
		VALUES (?, ?, ?, ?, ?, 'spent', ?)
	`, txID, req.RequestingAgentID, req.Amount, req.Merchant, req.Purpose, metadataJSON)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	resp := &models.AuthorizeResponse{
		Status:        "spent",
		TransactionID: txID,
	}
	resp.WithCard(card)
	return resp, nil
}

// Approve transitions a pending transaction to spent. Card data is NOT returned
// to the supervisor — the user agent retrieves it via GET /status/{tx_id}.
func (l *Ledger) Approve(txID string) (*models.StatusResponse, error) {
	tx, err := l.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var amount int64
	var status, agentID, merchant, purpose string
	err = tx.QueryRow("SELECT amount, status, agent_id, merchant, purpose FROM transactions WHERE id = ?", txID).
		Scan(&amount, &status, &agentID, &merchant, &purpose)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("transaction not found: %s", txID)
	} else if err != nil {
		return nil, err
	}

	if status != "pending" {
		return nil, fmt.Errorf("cannot approve transaction with status: %s", status)
	}

	// Transition to spent — budget was already reserved at authorize time
	_, err = tx.Exec("UPDATE transactions SET status = 'spent', updated_at = ? WHERE id = ?", time.Now(), txID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &models.StatusResponse{
		TransactionID: txID,
		Status:        "spent",
		Amount:        amount,
		Merchant:      merchant,
		Purpose:       purpose,
		AgentID:       agentID,
	}, nil
}

// Deny transitions a pending transaction to denied and refunds the reserved budget.
// Called by human operator via CLI or API. Also used internally by the expiry sweeper.
func (l *Ledger) Deny(txID string) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var reservedAmount int64
	var status, agentID string
	err = tx.QueryRow("SELECT amount, status, agent_id FROM transactions WHERE id = ?", txID).
		Scan(&reservedAmount, &status, &agentID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("transaction not found: %s", txID)
	} else if err != nil {
		return err
	}

	if status != "pending" {
		return fmt.Errorf("cannot deny transaction with status: %s", status)
	}

	// Transition to denied
	_, err = tx.Exec("UPDATE transactions SET status = 'denied', updated_at = ? WHERE id = ?", time.Now(), txID)
	if err != nil {
		return err
	}

	// Refund reserved budget (clamp to zero as a safety net)
	_, err = tx.Exec("UPDATE budgets SET used_amount = MAX(used_amount - ?, 0) WHERE agent_id = ?", reservedAmount, agentID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// GetStatus returns the current status of a transaction.
// Card data is included only when status == "spent" AND the caller's agent ID
// matches the transaction's agent_id — preventing cross-agent card leaks.
// ExpiresAt is included only when status == "pending".
func (l *Ledger) GetStatus(txID string, callerAgentID string) (*models.StatusResponse, error) {
	var amount int64
	var status, agentID, merchant, purpose string
	var expiresAt sql.NullTime
	err := l.db.QueryRow(
		"SELECT amount, status, agent_id, merchant, purpose, expires_at FROM transactions WHERE id = ?", txID).
		Scan(&amount, &status, &agentID, &merchant, &purpose, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("transaction not found: %s", txID)
	} else if err != nil {
		return nil, err
	}

	resp := &models.StatusResponse{
		TransactionID: txID,
		Status:        status,
		Amount:        amount,
		Merchant:      merchant,
		Purpose:       purpose,
		AgentID:       agentID,
	}
	// Only expose expires_at while the transaction is still pending.
	// Once spent or denied, the expiry window is no longer meaningful.
	if expiresAt.Valid && status == "pending" {
		t := expiresAt.Time
		resp.ExpiresAt = &t
	}

	// Include card data only when the transaction is spent AND the caller
	// identifies as the owning agent. This is self-reported (single user token),
	// not cryptographic — but prevents accidental cross-agent leaks.
	if status == "spent" && callerAgentID == agentID {
		if card, err := l.decryptCard(); err == nil {
			resp.WithCard(card)
		}
	}

	return resp, nil
}

// DrainStalePending expires all pending transactions whose approval window has
// elapsed. Called once at server startup to clean up zombies from a previous run
// (e.g. if the operator switched from manual to auto mode between restarts).
func (l *Ledger) DrainStalePending() error {
	return l.expireStale()
}

// RunExpirySweeper starts a background goroutine that periodically expires pending
// transactions whose approval window has elapsed. Expired transactions are denied
// and their reserved budget is refunded — identical to a human denial.
// The sweeper runs every minute regardless of the configured TTL.
func (l *Ledger) RunExpirySweeper() {
	ticker := time.NewTicker(time.Minute)
	go func() {
		defer ticker.Stop()
		for range ticker.C {
			if err := l.expireStale(); err != nil {
				slog.Error("Expiry sweep failed", "error", err)
			}
		}
	}()
}

// expireStale finds all pending transactions past their expires_at and denies them.
func (l *Ledger) expireStale() error {
	now := time.Now().UTC()
	rows, err := l.db.Query(
		"SELECT id, agent_id, amount FROM transactions WHERE status = 'pending' AND expires_at < ?", now)
	if err != nil {
		return err
	}

	type expiredTx struct {
		id      string
		agentID string
		amount  int64
	}
	var expired []expiredTx
	for rows.Next() {
		var e expiredTx
		if err := rows.Scan(&e.id, &e.agentID, &e.amount); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, e)
	}
	rows.Close()

	for _, e := range expired {
		tx, err := l.db.Begin()
		if err != nil {
			slog.Error("Expiry: failed to begin tx", "tx_id", e.id, "error", err)
			continue
		}
		// Re-check status inside the transaction to prevent double-refund if a
		// concurrent Deny() already transitioned this transaction.
		var currentStatus string
		err = tx.QueryRow("SELECT status FROM transactions WHERE id = ?", e.id).Scan(&currentStatus)
		if err != nil || currentStatus != "pending" {
			tx.Rollback()
			if err != nil {
				slog.Error("Expiry: failed to re-read status", "tx_id", e.id, "error", err)
			}
			continue
		}
		_, err = tx.Exec(
			"UPDATE transactions SET status = 'denied', reason = 'approval window expired', updated_at = ? WHERE id = ?",
			now, e.id)
		if err != nil {
			tx.Rollback()
			slog.Error("Expiry: failed to update transaction", "tx_id", e.id, "error", err)
			continue
		}
		// Clamp refund so used_amount never goes below zero.
		_, err = tx.Exec(
			"UPDATE budgets SET used_amount = MAX(used_amount - ?, 0) WHERE agent_id = ?",
			e.amount, e.agentID)
		if err != nil {
			tx.Rollback()
			slog.Error("Expiry: failed to refund budget", "tx_id", e.id, "error", err)
			continue
		}
		if err := tx.Commit(); err != nil {
			slog.Error("Expiry: failed to commit", "tx_id", e.id, "error", err)
			continue
		}
		slog.Info("Transaction expired", "tx_id", e.id, "agent_id", e.agentID, "amount_cents", e.amount)
	}
	return nil
}

// Credit restores budget for an agent and records an audit entry in the credits table.
// This is a supervisor-only operation (Tier 2+). Amount must be > 0.
func (l *Ledger) Credit(agentID string, amountCents int64, reason string) (*models.CreditResponse, error) {
	if amountCents <= 0 {
		return nil, fmt.Errorf("credit amount must be positive")
	}
	if reason == "" {
		return nil, fmt.Errorf("credit reason is required")
	}

	tx, err := l.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var limit, used int64
	err = tx.QueryRow("SELECT limit_amount, used_amount FROM budgets WHERE agent_id = ?", agentID).
		Scan(&limit, &used)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no budget found for agent: %s", agentID)
	} else if err != nil {
		return nil, err
	}

	newUsed := used - amountCents
	if newUsed < 0 {
		newUsed = 0 // clamp — cannot go below zero
	}

	_, err = tx.Exec("UPDATE budgets SET used_amount = ? WHERE agent_id = ?", newUsed, agentID)
	if err != nil {
		return nil, err
	}

	creditID := uuid.New().String()
	_, err = tx.Exec(
		"INSERT INTO credits (id, agent_id, amount, reason) VALUES (?, ?, ?, ?)",
		creditID, agentID, amountCents, reason,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	slog.Info("Budget credited", "agent_id", agentID, "amount_cents", amountCents, "reason", reason, "credit_id", creditID)
	return &models.CreditResponse{
		AgentID:   agentID,
		Credited:  amountCents,
		NewUsed:   newUsed,
		NewLimit:  limit,
		Available: limit - newUsed,
	}, nil
}

// ListPending returns all transactions currently in pending state, ordered oldest-first.
// Used by the supervisor agent to discover transactions awaiting a decision.
func (l *Ledger) ListPending() ([]models.PendingTransaction, error) {
	rows, err := l.db.Query(
		"SELECT id, agent_id, amount, merchant, purpose, expires_at, created_at FROM transactions WHERE status = 'pending' ORDER BY created_at ASC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txs []models.PendingTransaction
	for rows.Next() {
		var t models.PendingTransaction
		var expiresAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.AgentID, &t.Amount, &t.Merchant, &t.Purpose, &expiresAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			t.ExpiresAt = &expiresAt.Time
		}
		txs = append(txs, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if txs == nil {
		txs = []models.PendingTransaction{}
	}
	return txs, nil
}

// UpdateVault replaces the vaulted card with new encrypted credentials.
// This is a supervisor-only operation (Tier 3). The caller provides plain card data;
// this method encrypts it using the current master key before storage.
func (l *Ledger) UpdateVault(req models.VaultUpdateRequest) error {
	payload := map[string]string{
		"holder_name": req.HolderName,
		"card_number": req.CardNumber,
		"expiry_date": req.ExpiryDate,
		"cvv":         req.CVV,
		"country":     req.Country,
		"zip_code":    req.ZipCode,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal card payload: %w", err)
	}
	encrypted, err := vault.Encrypt(l.key, payloadBytes)
	vault.Zeroize(payloadBytes)
	if err != nil {
		return fmt.Errorf("encryption failed: %w", err)
	}

	_, err = l.db.Exec(`
		INSERT INTO vault (id, encrypted_payload) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload;
	`, encrypted)
	if err != nil {
		return fmt.Errorf("failed to store vault update: %w", err)
	}

	slog.Info("Vault updated via supervisor agent")
	return nil
}

func (l *Ledger) GetTransactions(agentID string) ([]models.Transaction, error) {
	rows, err := l.db.Query(
		"SELECT id, agent_id, amount, merchant, purpose, status, reason, card_id, metadata, expires_at, created_at, updated_at FROM transactions WHERE agent_id = ? ORDER BY created_at DESC",
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txs []models.Transaction
	for rows.Next() {
		var t models.Transaction
		var reason, metadataStr sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.AgentID, &t.Amount, &t.Merchant, &t.Purpose, &t.Status, &reason, &t.CardID, &metadataStr, &expiresAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if reason.Valid {
			t.Reason = reason.String
		}
		if metadataStr.Valid {
			json.Unmarshal([]byte(metadataStr.String), &t.Metadata)
		}
		if expiresAt.Valid {
			t.ExpiresAt = &expiresAt.Time
		}
		txs = append(txs, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if txs == nil {
		txs = []models.Transaction{}
	}
	return txs, nil
}
