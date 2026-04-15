package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentledgerhq/agentledger/pkg/ledger"
	"github.com/agentledgerhq/agentledger/pkg/models"
	"github.com/agentledgerhq/agentledger/pkg/vault"
)

type Server struct {
	Ledger        *ledger.Ledger
	CallbackURL   string // Optional TRANSACTION_CALLBACK_URL for webhook notifications
	WebhookSecret string // Optional HMAC-SHA256 secret for signing webhook payloads
}

func NewServer(l *ledger.Ledger, callbackURL string, webhookSecret string) (*Server, error) {
	// Validate callback URL at construction time — reject non-HTTP(S) schemes.
	if callbackURL != "" {
		if err := validateWebhookURL(callbackURL); err != nil {
			return nil, fmt.Errorf("invalid TRANSACTION_CALLBACK_URL: %w", err)
		}
	}
	return &Server{Ledger: l, CallbackURL: callbackURL, WebhookSecret: webhookSecret}, nil
}

// validateWebhookURL ensures the URL uses http or https and is parseable.
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &url.Error{Op: "validate", URL: raw, Err: url.InvalidHostError("scheme must be http or https")}
	}
	if u.Host == "" {
		return &url.Error{Op: "validate", URL: raw, Err: url.InvalidHostError("host is required")}
	}
	return nil
}

// validatePathID checks that a path parameter is non-empty and within length limits.
// Returns the cleaned value and true, or writes an error and returns false.
func validatePathID(w http.ResponseWriter, id string, label string, maxLen int) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		http.Error(w, label+" required in path", http.StatusBadRequest)
		return "", false
	}
	if len(id) > maxLen {
		http.Error(w, label+" too long (max "+strconv.Itoa(maxLen)+" chars)", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

// respond writes a JSON response with the given status code.
func respond(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// respondAndZeroize marshals v to JSON, writes it to the response, then zeroes
// the serialized buffer. The caller is responsible for zeroing the source struct
// fields (e.g. via ZeroizeCard) after this returns.
func respondAndZeroize(w http.ResponseWriter, status int, v interface{}) {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	buf = append(buf, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(buf)
	vault.Zeroize(buf)
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// Public (no auth, no rate limit)
	mux.HandleFunc("GET /health", LoggingMiddleware(s.handleHealth))

	// ── User-agent routes (AGENTLEDGER_USER_TOKEN) ─────────────────────────────
	// A user agent can only initiate spends and read its own state.
	// It cannot approve, deny, set budgets, or touch the vault.
	mux.HandleFunc("POST /authorize", LoggingMiddleware(RateLimitMiddleware(UserAuthMiddleware(BodyLimitMiddleware(s.handleAuthorize)))))
	mux.HandleFunc("GET /budget/", LoggingMiddleware(RateLimitMiddleware(EitherAuthMiddleware(s.handleGetBudget))))
	mux.HandleFunc("GET /transactions/", LoggingMiddleware(RateLimitMiddleware(EitherAuthMiddleware(s.handleGetTransactions))))
	mux.HandleFunc("GET /status/", LoggingMiddleware(RateLimitMiddleware(UserAuthMiddleware(s.handleGetStatus))))

	// ── Supervisor routes (AGENTLEDGER_SUPERVISOR_TOKEN) ───────────────────────
	// Tier 1 (default): transaction gatekeeping — approve, deny, list pending.
	// A supervisor agent cannot initiate spends; the user token is required for that.
	mux.HandleFunc("GET /pending", LoggingMiddleware(RateLimitMiddleware(SupervisorAuthMiddleware(s.handleListPending))))
	mux.HandleFunc("POST /approve", LoggingMiddleware(RateLimitMiddleware(SupervisorAuthMiddleware(BodyLimitMiddleware(s.handleApprove)))))
	mux.HandleFunc("POST /deny", LoggingMiddleware(RateLimitMiddleware(SupervisorAuthMiddleware(BodyLimitMiddleware(s.handleDeny)))))

	// Tier 2: budget management — set budgets and credit (restore) budget.
	// Requires AGENTLEDGER_SUPERVISOR_TIER >= 2.
	mux.HandleFunc("POST /budget/", LoggingMiddleware(RateLimitMiddleware(SupervisorAuthMiddleware(
		SupervisorTierMiddleware(2, BodyLimitMiddleware(s.handleSetBudget))))))
	mux.HandleFunc("POST /credit", LoggingMiddleware(RateLimitMiddleware(SupervisorAuthMiddleware(
		SupervisorTierMiddleware(2, BodyLimitMiddleware(s.handleCredit))))))

	// Tier 3: vault management — replace the vaulted card.
	// Requires AGENTLEDGER_SUPERVISOR_TIER >= 3. Human must have explicitly acknowledged
	// this capability at init time (confirmed in .env and init log).
	mux.HandleFunc("POST /vault/update", LoggingMiddleware(RateLimitMiddleware(SupervisorAuthMiddleware(
		SupervisorTierMiddleware(3, BodyLimitMiddleware(s.handleVaultUpdate))))))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var req models.AuthorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	if req.RequestingAgentID == "" {
		http.Error(w, "requesting_agent_id is required", http.StatusBadRequest)
		return
	}
	if req.Merchant == "" {
		http.Error(w, "merchant is required", http.StatusBadRequest)
		return
	}
	if req.Purpose == "" {
		http.Error(w, "purpose is required", http.StatusBadRequest)
		return
	}
	if len(req.RequestingAgentID) > 128 || len(req.Merchant) > 256 || len(req.Purpose) > 1024 {
		http.Error(w, "field too long (agent_id max 128, merchant max 256, purpose max 1024)", http.StatusBadRequest)
		return
	}
	if req.Amount <= 0 || req.Amount > ledger.MaxAuthorizeAmount {
		http.Error(w, "amount must be positive and <= per-request maximum", http.StatusBadRequest)
		return
	}

	resp, err := s.Ledger.Authorize(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	httpStatus := http.StatusOK
	if resp.Status == "denied" {
		httpStatus = http.StatusForbidden
	}
	respondAndZeroize(w, httpStatus, resp)
	resp.ZeroizeCard()

	// Fire webhook for every /authorize outcome when TRANSACTION_CALLBACK_URL is configured.
	// event values: "transaction.spent" (auto-mode or approved), "transaction.pending" (manual mode),
	// "transaction.denied" (insufficient budget or invalid request).
	if s.CallbackURL != "" && (resp.Status == "spent" || resp.Status == "pending" || resp.Status == "denied") {
		payload := models.WebhookPayload{
			Event:         "transaction." + resp.Status,
			TransactionID: resp.TransactionID,
			Amount:        req.Amount,
			Merchant:      req.Merchant,
			Purpose:       req.Purpose,
			AgentID:       req.RequestingAgentID,
			ExpiresAt:     resp.ExpiresAt,
			Metadata:      req.Metadata,
			CreatedAt:     time.Now(),
		}
		go s.fireWebhook(payload)
	}
}

func (s *Server) handleGetBudget(w http.ResponseWriter, r *http.Request) {
	pathParam, ok := validatePathID(w, strings.TrimPrefix(r.URL.Path, "/budget/"), "Agent ID", 128)
	if !ok {
		return
	}

	budget, err := s.Ledger.GetBudget(pathParam)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := models.CheckBudgetResponse{
		AgentID:   budget.AgentID,
		Limit:     budget.Limit,
		Used:      budget.Used,
		Available: budget.Limit - budget.Used,
		Period:    budget.Period,
	}
	if budget.Period == "monthly" {
		resp.PeriodStart = budget.PeriodStart.Format(time.RFC3339)
	}
	respond(w, http.StatusOK, resp)
}

// handleSetBudget creates or updates the budget for an agent via API. (Supervisor Tier 2+)
func (s *Server) handleSetBudget(w http.ResponseWriter, r *http.Request) {
	pathParam, ok := validatePathID(w, strings.TrimPrefix(r.URL.Path, "/budget/"), "Agent ID", 128)
	if !ok {
		return
	}

	var req models.SetBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	budget, err := s.Ledger.SetBudget(pathParam, req.Limit, req.Period)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := models.CheckBudgetResponse{
		AgentID:   budget.AgentID,
		Limit:     budget.Limit,
		Used:      budget.Used,
		Available: budget.Limit - budget.Used,
		Period:    budget.Period,
	}
	if budget.Period == "monthly" {
		resp.PeriodStart = budget.PeriodStart.Format(time.RFC3339)
	}
	respond(w, http.StatusCreated, resp)
}

func (s *Server) handleGetTransactions(w http.ResponseWriter, r *http.Request) {
	pathParam, ok := validatePathID(w, strings.TrimPrefix(r.URL.Path, "/transactions/"), "Agent ID", 128)
	if !ok {
		return
	}

	txs, err := s.Ledger.GetTransactions(pathParam)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusOK, txs)
}

// handleListPending returns all currently pending transactions. (Supervisor Tier 1+)
func (s *Server) handleListPending(w http.ResponseWriter, r *http.Request) {
	txs, err := s.Ledger.ListPending()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusOK, txs)
}

// handleApprove transitions a pending transaction to spent and returns card data. (Supervisor Tier 1+)
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req models.ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	resp, err := s.Ledger.Approve(req.TransactionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respond(w, http.StatusOK, resp)
}

// handleDeny transitions a pending transaction to denied and refunds the budget. (Supervisor Tier 1+)
func (s *Server) handleDeny(w http.ResponseWriter, r *http.Request) {
	var req models.DenyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	if err := s.Ledger.Deny(req.TransactionID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respond(w, http.StatusOK, models.TransactionResponse{Status: "denied"})
}

// handleGetStatus returns the current status of a transaction. (User-agent readable)
// Card data is included only when status == "spent" and the caller identifies as the owning agent.
func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	pathParam, ok := validatePathID(w, strings.TrimPrefix(r.URL.Path, "/status/"), "Transaction ID", 128)
	if !ok {
		return
	}

	// The agent self-reports its identity via query param. Card data is only
	// returned if this matches the transaction's agent_id.
	callerAgentID := r.URL.Query().Get("agent_id")

	resp, err := s.Ledger.GetStatus(pathParam, callerAgentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	respondAndZeroize(w, http.StatusOK, resp)
	resp.ZeroizeCard()
}

// handleCredit restores budget for an agent. (Supervisor Tier 2+)
func (s *Server) handleCredit(w http.ResponseWriter, r *http.Request) {
	var req models.CreditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	if req.AgentID == "" {
		http.Error(w, "agent_id is required", http.StatusBadRequest)
		return
	}
	if len(req.AgentID) > 128 {
		http.Error(w, "agent_id too long (max 128 chars)", http.StatusBadRequest)
		return
	}
	if req.Amount <= 0 {
		http.Error(w, "amount must be positive", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	if len(req.Reason) > 1024 {
		http.Error(w, "reason too long (max 1024 chars)", http.StatusBadRequest)
		return
	}

	resp, err := s.Ledger.Credit(req.AgentID, req.Amount, req.Reason)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respond(w, http.StatusOK, resp)
}

// handleVaultUpdate replaces the vaulted card credentials. (Supervisor Tier 3 only)
func (s *Server) handleVaultUpdate(w http.ResponseWriter, r *http.Request) {
	var req models.VaultUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	if req.HolderName == "" || req.CardNumber == "" || req.ExpiryDate == "" || req.CVV == "" || req.Country == "" || req.ZipCode == "" {
		http.Error(w, "holder_name, card_number, expiry_date, cvv, country, and zip_code are required", http.StatusBadRequest)
		return
	}

	if err := s.Ledger.UpdateVault(req); err != nil {
		req.ZeroizeFields()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.ZeroizeFields()
	respond(w, http.StatusOK, map[string]string{"status": "vault updated"})
}

// fireWebhook sends an async POST to TRANSACTION_CALLBACK_URL with the given payload.
// If AGENTLEDGER_WEBHOOK_SECRET is configured, the request includes an X-Agentledger-Signature
// header containing an HMAC-SHA256 hex digest of the body, allowing the receiver to
// verify authenticity.
func (s *Server) fireWebhook(payload models.WebhookPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("Failed to marshal webhook payload", "error", err)
		return
	}

	req, err := http.NewRequest("POST", s.CallbackURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("Webhook: failed to create request", "url", s.CallbackURL, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	// Sign the payload if a webhook secret is configured
	if s.WebhookSecret != "" {
		mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
		mac.Write(body)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Agentledger-Signature", "sha256="+sig)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("Webhook callback failed", "url", s.CallbackURL, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Info("Webhook callback sent", "url", s.CallbackURL, "status", resp.StatusCode, "transaction_id", payload.TransactionID)
	} else {
		slog.Warn("Webhook callback returned non-2xx", "url", s.CallbackURL, "status", resp.StatusCode, "transaction_id", payload.TransactionID)
	}
}
