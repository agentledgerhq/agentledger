package models

import (
	"time"
	"unsafe"
)

// CardData holds the full set of payment credentials stored in the vault.
type CardData struct {
	HolderName string `json:"holder_name"`
	CardNumber string `json:"card_number"`
	ExpiryDate string `json:"expiry_date"` // MM/YY
	CVV        string `json:"cvv"`
	Country    string `json:"country"`
	ZipCode    string `json:"zip_code"`
}

// zeroString overwrites the backing array of a Go string with zeroes, then
// resets the header. This is technically undefined behaviour (mutating an
// immutable string) but is the standard pattern used in Go security libraries
// and works on all current implementations.
func zeroString(s *string) {
	if len(*s) > 0 {
		b := unsafe.Slice(unsafe.StringData(*s), len(*s))
		for i := range b {
			b[i] = 0
		}
	}
	*s = ""
}

// Zeroize securely wipes all credential fields in the CardData struct.
func (c *CardData) Zeroize() {
	if c == nil {
		return
	}
	zeroString(&c.HolderName)
	zeroString(&c.CardNumber)
	zeroString(&c.ExpiryDate)
	zeroString(&c.CVV)
	zeroString(&c.Country)
	zeroString(&c.ZipCode)
}

type AuthorizeRequest struct {
	Amount            int64                  `json:"amount"`
	Merchant          string                 `json:"merchant"`
	Purpose           string                 `json:"purpose"`
	RequestingAgentID string                 `json:"requesting_agent_id"`
	Metadata          map[string]interface{} `json:"metadata,omitempty"`
}

// AuthorizeResponse is returned by POST /authorize.
// status values:
//   - "spent"   — auto mode: budget consumed, card details included in this response.
//   - "pending" — manual mode: awaiting human approval, no card data. ExpiresAt is set.
//   - "denied"  — budget check failed or amount invalid. No card data.
type AuthorizeResponse struct {
	Status        string     `json:"status"`
	TransactionID string     `json:"transaction_id,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"` // Set for pending transactions
	HolderName    string     `json:"holder_name,omitempty"`
	CardNumber    string     `json:"card_number,omitempty"`
	ExpiryDate    string     `json:"expiry_date,omitempty"`
	CVV           string     `json:"cvv,omitempty"`
	Country       string     `json:"country,omitempty"`
	ZipCode       string     `json:"zip_code,omitempty"`
}

// WithCard populates card fields on an AuthorizeResponse from a CardData.
func (r *AuthorizeResponse) WithCard(c *CardData) {
	r.HolderName = c.HolderName
	r.CardNumber = c.CardNumber
	r.ExpiryDate = c.ExpiryDate
	r.CVV = c.CVV
	r.Country = c.Country
	r.ZipCode = c.ZipCode
}

// ZeroizeCard securely wipes card credential fields on an AuthorizeResponse.
func (r *AuthorizeResponse) ZeroizeCard() {
	zeroString(&r.HolderName)
	zeroString(&r.CardNumber)
	zeroString(&r.ExpiryDate)
	zeroString(&r.CVV)
	zeroString(&r.Country)
	zeroString(&r.ZipCode)
}

type TransactionResponse struct {
	Status string `json:"status"`
}

type CheckBudgetResponse struct {
	AgentID     string `json:"agent_id"`
	Limit       int64  `json:"limit"`
	Used        int64  `json:"used"`
	Available   int64  `json:"available"`
	Period      string `json:"period"`                 // "cumulative" or "monthly"
	PeriodStart string `json:"period_start,omitempty"` // ISO 8601 timestamp, present for monthly budgets
}

type Budget struct {
	AgentID     string    `json:"agent_id"`
	Limit       int64     `json:"limit_amount"`
	Used        int64     `json:"used_amount"`
	Period      string    `json:"period"` // "cumulative" or "monthly"
	PeriodStart time.Time `json:"period_start"`
}

// Transaction represents a ledger entry as returned by GET /transactions/{agent}.
// Status values: "pending", "spent", "denied".
type Transaction struct {
	ID        string                 `json:"id"`
	AgentID   string                 `json:"agent_id"`
	Amount    int64                  `json:"amount"`
	Merchant  string                 `json:"merchant"`
	Purpose   string                 `json:"purpose"`
	Status    string                 `json:"status"`
	Reason    string                 `json:"reason,omitempty"`
	CardID    *string                `json:"card_id,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// SetBudgetRequest is the payload for POST /budget/{agent}.
type SetBudgetRequest struct {
	Limit  int64  `json:"limit"`            // Budget limit in cents
	Period string `json:"period,omitempty"` // "cumulative" (default) or "monthly"
}

// ApproveRequest is the payload for POST /approve.
type ApproveRequest struct {
	TransactionID string `json:"transaction_id"`
}

// DenyRequest is the payload for POST /deny.
type DenyRequest struct {
	TransactionID string `json:"transaction_id"`
}

// StatusResponse is returned by GET /status/{tx_id}.
// Card data is included only when status == "spent" (budget consumed, card released).
// ExpiresAt is set only when status == "pending".
type StatusResponse struct {
	TransactionID string     `json:"transaction_id"`
	Status        string     `json:"status"`
	Amount        int64      `json:"amount"`
	Merchant      string     `json:"merchant"`
	Purpose       string     `json:"purpose"`
	AgentID       string     `json:"agent_id"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	HolderName    string     `json:"holder_name,omitempty"`
	CardNumber    string     `json:"card_number,omitempty"`
	ExpiryDate    string     `json:"expiry_date,omitempty"`
	CVV           string     `json:"cvv,omitempty"`
	Country       string     `json:"country,omitempty"`
	ZipCode       string     `json:"zip_code,omitempty"`
}

// WithCard populates card fields on a StatusResponse from a CardData.
func (r *StatusResponse) WithCard(c *CardData) {
	r.HolderName = c.HolderName
	r.CardNumber = c.CardNumber
	r.ExpiryDate = c.ExpiryDate
	r.CVV = c.CVV
	r.Country = c.Country
	r.ZipCode = c.ZipCode
}

// ZeroizeCard securely wipes card credential fields on a StatusResponse.
func (r *StatusResponse) ZeroizeCard() {
	zeroString(&r.HolderName)
	zeroString(&r.CardNumber)
	zeroString(&r.ExpiryDate)
	zeroString(&r.CVV)
	zeroString(&r.Country)
	zeroString(&r.ZipCode)
}

// CreditRequest is the payload for POST /credit.
type CreditRequest struct {
	AgentID string `json:"agent_id"`
	Amount  int64  `json:"amount"` // Cents to restore to the budget
	Reason  string `json:"reason"` // Required: human-readable reason for the credit
}

// CreditResponse is returned by POST /credit.
type CreditResponse struct {
	AgentID   string `json:"agent_id"`
	Credited  int64  `json:"credited"`
	NewUsed   int64  `json:"new_used"`
	NewLimit  int64  `json:"new_limit"`
	Available int64  `json:"available"`
}

// VaultUpdateRequest is the payload for POST /vault/update (Supervisor Tier 3).
type VaultUpdateRequest struct {
	HolderName string `json:"holder_name"`
	CardNumber string `json:"card_number"`
	ExpiryDate string `json:"expiry_date"`
	CVV        string `json:"cvv"`
	Country    string `json:"country"`
	ZipCode    string `json:"zip_code"`
}

// ZeroizeFields securely wipes all credential fields on a VaultUpdateRequest.
func (r *VaultUpdateRequest) ZeroizeFields() {
	zeroString(&r.HolderName)
	zeroString(&r.CardNumber)
	zeroString(&r.ExpiryDate)
	zeroString(&r.CVV)
	zeroString(&r.Country)
	zeroString(&r.ZipCode)
}

// PendingTransaction is a summary of a pending transaction returned by GET /pending.
type PendingTransaction struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agent_id"`
	Amount    int64      `json:"amount"`
	Merchant  string     `json:"merchant"`
	Purpose   string     `json:"purpose"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// WebhookPayload is POSTed to TRANSACTION_CALLBACK_URL on every /authorize outcome.
// Event values: "transaction.spent", "transaction.pending", "transaction.denied".
type WebhookPayload struct {
	Event         string                 `json:"event"` // "transaction.spent" | "transaction.pending" | "transaction.denied"
	TransactionID string                 `json:"transaction_id"`
	Amount        int64                  `json:"amount"`
	Merchant      string                 `json:"merchant"`
	Purpose       string                 `json:"purpose"`
	AgentID       string                 `json:"agent_id"`
	ExpiresAt     *time.Time             `json:"expires_at,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
}
