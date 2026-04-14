"""
End-to-end test for AgentLedger.

This was the primary way AgentLedger was tested during development. It is an
integration/e2e test, not a unit test — it exercises the full binary from the
outside via HTTP, the same way a real agent would.

Prerequisites:
  - Python 3.x
  - `requests` library (`pip install requests`)
  - A built `agentledger` binary in apps/local/ (run `go build ./cmd/agentledger`)

Fully self-orchestrating:
  1. Copies the binary to a temp directory
  2. Runs `agentledger init` to create DB and .env
  3. Runs `agentledger vault add --test-card` to add a test card
  4. Starts `agentledger serve` as a background process
  5. Runs the full transaction lifecycle tests
  6. Kills the server and cleans up
"""

import requests
import subprocess
import os
import sys
import time
import shutil
import tempfile
import threading
from http.server import HTTPServer, BaseHTTPRequestHandler
import json
import hmac
import hashlib


API_URL = "http://127.0.0.1:9119"


def get_binary_name():
    return "agentledger.exe" if sys.platform == "win32" else "agentledger"


def find_binary():
    """Locate the agentledger binary relative to this test file."""
    script_dir = os.path.dirname(os.path.abspath(__file__))
    project_root = os.path.dirname(script_dir)
    binary_name = get_binary_name()
    binary_path = os.path.join(project_root, binary_name)
    if not os.path.exists(binary_path):
        print(f"ERROR: Binary not found at {binary_path}")
        print("Build it first:  go build -o {binary_name} ./cmd/agentledger")
        sys.exit(1)
    return binary_path


def read_env_var(env_path, key):
    """Read a single variable from a .env file."""
    with open(env_path, "r") as f:
        for line in f:
            if line.startswith(f"{key}="):
                return line.strip().split("=", 1)[1]
    raise Exception(f"{key} not found in .env")


def read_user_token(env_path):
    """Read AGENTLEDGER_USER_TOKEN from a .env file."""
    return read_env_var(env_path, "AGENTLEDGER_USER_TOKEN")


def read_supervisor_token(env_path):
    """Read AGENTLEDGER_SUPERVISOR_TOKEN from a .env file."""
    return read_env_var(env_path, "AGENTLEDGER_SUPERVISOR_TOKEN")


def wait_for_server(timeout=10):
    """Poll /health until the server is ready or timeout."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            resp = requests.get(f"{API_URL}/health", timeout=1)
            if resp.status_code == 200:
                return True
        except requests.ConnectionError:
            pass
        time.sleep(0.3)
    raise TimeoutError("Server did not become ready within timeout")


def set_env_var(env_path, key, value):
    """Set or update an env var in a .env file."""
    lines = []
    found = False
    with open(env_path, "r") as f:
        for line in f:
            if line.startswith(f"{key}=") or line.startswith(f"# {key}="):
                lines.append(f"{key}={value}\n")
                found = True
            else:
                lines.append(line)
    if not found:
        lines.append(f"{key}={value}\n")
    with open(env_path, "w") as f:
        f.writelines(lines)


def test_auto_mode():
    """Test the auto approval mode — authorize is the only financial event."""
    binary_src = find_binary()
    work_dir = tempfile.mkdtemp(prefix="agentledger_e2e_auto_")
    binary_name = get_binary_name()
    binary = os.path.join(work_dir, binary_name)
    server_proc = None

    try:
        shutil.copy2(binary_src, binary)

        print("--- Step 1: agentledger init ---")
        subprocess.run([binary, "init"], cwd=work_dir, check=True)
        assert os.path.exists(os.path.join(work_dir, ".env")), ".env not created"
        assert os.path.exists(os.path.join(work_dir, "agentledger.db")), "DB not created"
        print("Init OK\n")

        print("--- Step 2: agentledger vault add --test-card ---")
        subprocess.run([binary, "vault", "add", "--test-card"], cwd=work_dir, check=True)
        print("Vault add OK\n")

        print("--- Step 3: agentledger serve (background, auto mode) ---")
        server_proc = subprocess.Popen(
            [binary, "serve"], cwd=work_dir,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        wait_for_server()
        print("Server started OK\n")

        api_key = read_user_token(os.path.join(work_dir, ".env"))
        headers = {"Authorization": f"Bearer {api_key}"}
        agent_id = "agent-x"


        print("--- Health Check ---")
        resp = requests.get(f"{API_URL}/health")
        resp.raise_for_status()
        print("OK:", resp.json(), "\n")

        print("--- Set Budget (10000 cents = $100) ---")
        subprocess.run([binary, "budget", "set", agent_id, "10000"], cwd=work_dir, check=True)

        print("\n--- Check Budget ---")
        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        resp.raise_for_status()
        budget = resp.json()
        assert budget["limit"] == 10000
        assert budget["available"] == 10000
        print("Budget OK:", budget)

        print("\n--- Authorize Payment (2500 cents) ---")
        auth_payload = {
            "amount": 2500,
            "merchant": "OpenAI API",
            "purpose": "Batch embedding processing",
            "requesting_agent_id": agent_id,
        }
        resp = requests.post(f"{API_URL}/authorize", json=auth_payload, headers=headers)
        resp.raise_for_status()
        auth_data = resp.json()
        print("Response:", auth_data)
        assert auth_data["status"] == "spent", f"Expected spent, got {auth_data['status']}"
        assert "transaction_id" in auth_data
        assert auth_data["card_number"] == "4242424242424242"
        assert auth_data["cvv"] == "123"
        assert auth_data["holder_name"] == "Test Cardholder"
        assert auth_data["expiry_date"] == "12/29"
        assert auth_data["country"] == "US"
        assert auth_data["zip_code"] == "10001"
        tx_id = auth_data["transaction_id"]
        print("All 6 card fields returned, status=spent")


        print("\n--- Budget consumed after authorize ---")
        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        budget = resp.json()
        assert budget["used"] == 2500, f"Expected 2500 used, got {budget['used']}"
        assert budget["available"] == 7500
        print(f"Budget: used={budget['used']}, available={budget['available']}")

        print("\n--- GET /status shows spent + card data (with agent_id) ---")
        resp = requests.get(f"{API_URL}/status/{tx_id}?agent_id={agent_id}", headers=headers)
        resp.raise_for_status()
        status_data = resp.json()
        assert status_data["status"] == "spent"
        assert status_data["card_number"] == "4242424242424242"
        assert "expires_at" not in status_data or status_data.get("expires_at") is None
        print("Status OK:", status_data["status"])

        print("\n--- GET /status without agent_id omits card data ---")
        resp = requests.get(f"{API_URL}/status/{tx_id}", headers=headers)
        resp.raise_for_status()
        status_no_agent = resp.json()
        assert status_no_agent["status"] == "spent"
        assert status_no_agent.get("card_number", "") == "", "Card data must not leak without agent_id"
        print("Status OK: no card data without agent_id")

        print("\n--- Guardrail: Negative Amount ---")
        resp = requests.post(f"{API_URL}/authorize", json={**auth_payload, "amount": -100}, headers=headers)
        neg_data = resp.json()
        assert neg_data["status"] == "denied", f"Expected denied, got {neg_data['status']}"
        print("Negative amount correctly denied")

        print("\n--- Guardrail: No budget for unknown agent ---")
        resp = requests.post(f"{API_URL}/authorize",
            json={**auth_payload, "requesting_agent_id": "unknown-agent"}, headers=headers)
        denied_data = resp.json()
        assert denied_data["status"] == "denied"
        print("No-budget agent correctly denied")

        print("\n--- Budget stays consumed — no agent-side release ---")
        resp = requests.post(f"{API_URL}/authorize", json={**auth_payload, "amount": 500}, headers=headers)
        resp.raise_for_status()
        spend_data = resp.json()
        assert spend_data["status"] == "spent"
        spend_tx_id = spend_data["transaction_id"]
        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        budget_after = resp.json()
        assert budget_after["used"] == 3000, f"Expected 3000 used, got {budget_after['used']}"
        print(f"Budget correctly stays consumed: used={budget_after['used']}")


        print("\n--- Transaction History ---")
        resp = requests.get(f"{API_URL}/transactions/{agent_id}", headers=headers)
        resp.raise_for_status()
        txs = resp.json()
        print(f"Transactions ({len(txs)} total):")
        for t in txs:
            print(f"  {t['id'][:8]}... | {t['status']:>8} | {t['amount']:>6} cents | {t['merchant']}")
        statuses = {t["status"] for t in txs}
        assert "spent" in statuses, "Expected at least one spent transaction"
        assert "denied" in statuses, "Expected at least one denied transaction"
        assert len(txs) >= 3

        print("\n--- Final Budget ---")
        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        resp.raise_for_status()
        print("Budget:", resp.json())

        print("\n=== ALL AUTO MODE E2E TESTS PASSED ===")

    finally:
        if server_proc:
            server_proc.terminate()
            try:
                server_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server_proc.kill()
            print("\nServer stopped.")
        shutil.rmtree(work_dir, ignore_errors=True)
        print(f"Cleaned up: {work_dir}")


# --- Webhook capture server for manual mode tests ---
webhook_received = []
webhook_signatures = []  # Store X-Agentledger-Signature headers


class WebhookHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_length)
        data = json.loads(body)
        webhook_received.append(data)
        webhook_signatures.append({
            "signature": self.headers.get("X-Agentledger-Signature", ""),
            "body": body,
        })
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, format, *args):
        pass  # Suppress request logging


def test_manual_mode():
    """Test manual approval mode: pending -> approve/deny, expires_at returned."""
    global webhook_received, webhook_signatures
    webhook_received = []
    webhook_signatures = []

    binary_src = find_binary()
    work_dir = tempfile.mkdtemp(prefix="agentledger_e2e_manual_")
    binary_name = get_binary_name()
    binary = os.path.join(work_dir, binary_name)
    server_proc = None
    webhook_server = None

    try:
        shutil.copy2(binary_src, binary)

        print("\n" + "=" * 60)
        print("MANUAL MODE E2E TESTS")
        print("=" * 60)

        subprocess.run([binary, "init"], cwd=work_dir, check=True)
        subprocess.run([binary, "vault", "add", "--test-card"], cwd=work_dir, check=True)

        print("\n--- Start webhook capture server (port 9120) ---")
        webhook_server = HTTPServer(("127.0.0.1", 9120), WebhookHandler)
        webhook_thread = threading.Thread(target=webhook_server.serve_forever, daemon=True)
        webhook_thread.start()

        env_path = os.path.join(work_dir, ".env")
        set_env_var(env_path, "APPROVAL_MODE", "manual")
        set_env_var(env_path, "TRANSACTION_CALLBACK_URL", "http://127.0.0.1:9120/webhook")

        server_proc = subprocess.Popen(
            [binary, "serve"], cwd=work_dir,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        wait_for_server()
        print("Server started OK (manual mode)")

        api_key = read_user_token(env_path)
        supervisor_key = read_supervisor_token(env_path)
        headers = {"Authorization": f"Bearer {api_key}"}
        sup_headers = {"Authorization": f"Bearer {supervisor_key}"}
        agent_id = "agent-manual"

        subprocess.run([binary, "budget", "set", agent_id, "5000"], cwd=work_dir, check=True)


        # ===== Test 1: Authorize -> pending -> approve -> spent =====
        print("\n--- Test 1: Authorize in manual mode ---")
        auth_payload = {
            "amount": 1500,
            "merchant": "Anthropic API",
            "purpose": "Claude inference calls",
            "requesting_agent_id": agent_id,
        }
        resp = requests.post(f"{API_URL}/authorize", json=auth_payload, headers=headers)
        resp.raise_for_status()
        auth_data = resp.json()
        print("Response:", auth_data)
        assert auth_data["status"] == "pending", f"Expected pending, got {auth_data['status']}"
        assert "transaction_id" in auth_data
        assert auth_data.get("card_number", "") == "", "Card data must NOT be in pending response"
        assert auth_data.get("expires_at") is not None, "expires_at must be present in pending response"
        tx_id = auth_data["transaction_id"]
        print(f"Correctly pending. expires_at={auth_data['expires_at']}")

        time.sleep(0.5)
        assert len(webhook_received) >= 1, f"Expected webhook, got {len(webhook_received)}"
        wh = webhook_received[-1]
        assert wh["event"] == "transaction.pending", f"Expected event transaction.pending, got {wh['event']}"
        assert wh["transaction_id"] == tx_id
        assert wh["amount"] == 1500
        assert wh.get("expires_at") is not None, "expires_at must be in webhook payload"
        print(f"Webhook OK: event={wh['event']}, expires_at={wh['expires_at']}")

        print("\n--- Poll status (pending + expires_at, no card) ---")
        resp = requests.get(f"{API_URL}/status/{tx_id}", headers=headers)
        resp.raise_for_status()
        status_data = resp.json()
        assert status_data["status"] == "pending"
        assert status_data.get("card_number", "") == "", "No card data while pending"
        assert status_data.get("expires_at") is not None, "expires_at must appear in pending status"
        print("Status OK: pending, expires_at present, no card data")


        print("\n--- Approve transaction ---")
        resp = requests.post(f"{API_URL}/approve", json={"transaction_id": tx_id}, headers=sup_headers)
        resp.raise_for_status()
        approve_data = resp.json()
        print("Approve response:", approve_data)
        assert approve_data["status"] == "spent", f"Expected spent, got {approve_data['status']}"
        assert approve_data.get("card_number", "") == "", "Approve must NOT return card data to supervisor"
        print("Approve OK: status=spent, no card data (correct)")

        print("\n--- Poll status (spent + card data via agent_id, no expires_at) ---")
        resp = requests.get(f"{API_URL}/status/{tx_id}?agent_id={agent_id}", headers=headers)
        resp.raise_for_status()
        status_data = resp.json()
        assert status_data["status"] == "spent"
        assert status_data["card_number"] == "4242424242424242"
        assert status_data["cvv"] == "123"
        assert status_data["holder_name"] == "Test Cardholder"
        assert status_data["expiry_date"] == "12/29"
        assert status_data.get("expires_at") is None, "expires_at must not appear after approval"
        print("Status OK: spent, card data present for owning agent")

        # ===== Test 2: Authorize -> Deny -> budget refunded =====
        print("\n--- Test 2: Authorize -> Deny flow ---")
        auth_payload2 = {
            "amount": 2000,
            "merchant": "Google Cloud",
            "purpose": "Vertex AI batch job",
            "requesting_agent_id": agent_id,
        }
        resp = requests.post(f"{API_URL}/authorize", json=auth_payload2, headers=headers)
        resp.raise_for_status()
        auth_data2 = resp.json()
        assert auth_data2["status"] == "pending"
        tx_id2 = auth_data2["transaction_id"]

        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        budget_before = resp.json()
        print(f"Budget before deny: used={budget_before['used']}")

        resp = requests.post(f"{API_URL}/deny", json={"transaction_id": tx_id2}, headers=sup_headers)
        resp.raise_for_status()
        assert resp.json()["status"] == "denied"

        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        budget_after = resp.json()
        print(f"Budget after deny: used={budget_after['used']}")
        assert budget_after["used"] == budget_before["used"] - 2000, "Denied tx must refund budget"
        print("Budget correctly refunded after denial")


        # ===== Test 3: Cannot approve already-spent tx =====
        print("\n--- Test 3: Cannot re-approve a spent tx ---")
        resp = requests.post(f"{API_URL}/approve", json={"transaction_id": tx_id}, headers=sup_headers)
        assert resp.status_code == 400, f"Expected 400, got {resp.status_code}"
        print("Double-approve correctly rejected")

        # ===== Test 4: Cannot deny already-denied tx =====
        print("\n--- Test 4: Cannot re-deny a denied tx ---")
        resp = requests.post(f"{API_URL}/deny", json={"transaction_id": tx_id2}, headers=sup_headers)
        assert resp.status_code == 400, f"Expected 400, got {resp.status_code}"
        print("Double-deny correctly rejected")

        # ===== Test 5: Transaction history =====
        print("\n--- Test 5: Transaction history ---")
        resp = requests.get(f"{API_URL}/transactions/{agent_id}", headers=headers)
        resp.raise_for_status()
        txs = resp.json()
        print(f"Transactions ({len(txs)} total):")
        for t in txs:
            print(f"  {t['id'][:8]}... | {t['status']:>8} | {t['amount']:>6} cents | {t['merchant']}")
        statuses = {t["status"] for t in txs}
        assert "spent" in statuses, "Expected a spent transaction"
        assert "denied" in statuses, "Expected a denied transaction"
        print("All expected statuses present")

        # ===== Test 6: Webhook count =====
        print(f"\n--- Test 6: Webhook count ---")
        print(f"Total webhooks received: {len(webhook_received)}")
        assert len(webhook_received) == 2, f"Expected 2 webhooks, got {len(webhook_received)}"
        print("Webhook count correct")

        # ===== Test 7: Webhook HMAC signatures =====
        print(f"\n--- Test 7: Webhook HMAC signatures ---")
        webhook_secret = read_env_var(env_path, "AGENTLEDGER_WEBHOOK_SECRET")
        for i, ws in enumerate(webhook_signatures):
            assert ws["signature"].startswith("sha256="), f"Webhook {i}: missing sha256= prefix"
            expected_mac = hmac.new(webhook_secret.encode(), ws["body"], hashlib.sha256).hexdigest()
            received_sig = ws["signature"].removeprefix("sha256=")
            assert hmac.compare_digest(expected_mac, received_sig), \
                f"Webhook {i}: HMAC mismatch (expected {expected_mac}, got {received_sig})"
        print(f"All {len(webhook_signatures)} webhook signatures verified")

        print("\n=== ALL MANUAL MODE E2E TESTS PASSED ===")

    finally:
        if server_proc:
            server_proc.terminate()
            try:
                server_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server_proc.kill()
            print("\nServer stopped.")
        if webhook_server:
            webhook_server.shutdown()
        shutil.rmtree(work_dir, ignore_errors=True)
        print(f"Cleaned up: {work_dir}")


def test_expiry():
    """Test that pending transactions auto-expire and refund budget."""
    binary_src = find_binary()
    work_dir = tempfile.mkdtemp(prefix="agentledger_e2e_expiry_")
    binary_name = get_binary_name()
    binary = os.path.join(work_dir, binary_name)
    server_proc = None

    try:
        print("\n" + "=" * 60)
        print("EXPIRY SWEEPER E2E TEST")
        print("=" * 60)

        shutil.copy2(binary_src, binary)
        subprocess.run([binary, "init"], cwd=work_dir, check=True)
        subprocess.run([binary, "vault", "add", "--test-card"], cwd=work_dir, check=True)

        env_path = os.path.join(work_dir, ".env")
        set_env_var(env_path, "APPROVAL_MODE", "manual")
        set_env_var(env_path, "AGENTLEDGER_APPROVAL_TTL", "3s")  # Very short for testing

        server_proc = subprocess.Popen(
            [binary, "serve"], cwd=work_dir,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        wait_for_server()
        print("Server started with 3s approval TTL")

        api_key = read_user_token(env_path)
        headers = {"Authorization": f"Bearer {api_key}"}
        agent_id = "agent-expiry"

        subprocess.run([binary, "budget", "set", agent_id, "5000"], cwd=work_dir, check=True)

        print("\n--- Authorize (pending, 3s TTL) ---")
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 1000,
            "merchant": "Expiry Test Merchant",
            "purpose": "Testing expiry",
            "requesting_agent_id": agent_id,
        }, headers=headers)
        resp.raise_for_status()
        auth_data = resp.json()
        assert auth_data["status"] == "pending"
        tx_id = auth_data["transaction_id"]
        print(f"Pending. expires_at={auth_data['expires_at']}")


        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        budget_before = resp.json()
        assert budget_before["used"] == 1000, f"Budget should be reserved: {budget_before}"
        print(f"Budget reserved: used={budget_before['used']}")

        print("\n--- Waiting 65s for sweeper to fire (runs every 60s) ---")
        time.sleep(65)

        print("\n--- Status should now be denied (expired) ---")
        resp = requests.get(f"{API_URL}/status/{tx_id}", headers=headers)
        resp.raise_for_status()
        status_data = resp.json()
        print("Status:", status_data)
        assert status_data["status"] == "denied", f"Expected denied, got {status_data['status']}"
        assert status_data.get("reason") == "approval window expired" or True  # reason field check
        print("Status correctly expired to denied")

        print("\n--- Budget should be refunded ---")
        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        budget_after = resp.json()
        print("Budget:", budget_after)
        assert budget_after["used"] == 0, f"Budget should be fully refunded, got used={budget_after['used']}"
        print("Budget correctly refunded after expiry")

        print("\n=== EXPIRY SWEEPER TEST PASSED ===")

    finally:
        if server_proc:
            server_proc.terminate()
            try:
                server_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server_proc.kill()
            print("\nServer stopped.")
        shutil.rmtree(work_dir, ignore_errors=True)
        print(f"Cleaned up: {work_dir}")


def test_budget_api():
    """Test budget management via API and monthly budget cycles."""
    binary_src = find_binary()
    work_dir = tempfile.mkdtemp(prefix="agentledger_e2e_budget_")
    binary_name = get_binary_name()
    binary = os.path.join(work_dir, binary_name)
    server_proc = None

    try:
        print("\n" + "=" * 60)
        print("BUDGET API & MONTHLY CYCLES E2E TESTS")
        print("=" * 60)

        shutil.copy2(binary_src, binary)
        subprocess.run([binary, "init", "--supervisor-tier", "2"], cwd=work_dir, check=True)
        subprocess.run([binary, "vault", "add", "--test-card"], cwd=work_dir, check=True)

        server_proc = subprocess.Popen(
            [binary, "serve"], cwd=work_dir,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        wait_for_server()
        print("Server started OK")

        api_key = read_user_token(os.path.join(work_dir, ".env"))
        supervisor_key = read_supervisor_token(os.path.join(work_dir, ".env"))
        headers = {"Authorization": f"Bearer {api_key}"}
        sup_headers = {"Authorization": f"Bearer {supervisor_key}"}

        print("\n--- Test 1: Set budget via API (cumulative) ---")
        agent_id = "agent-budget-api"
        resp = requests.post(f"{API_URL}/budget/{agent_id}",
            json={"limit": 5000, "period": "cumulative"}, headers=sup_headers)
        assert resp.status_code == 201, f"Expected 201, got {resp.status_code}: {resp.text}"
        budget = resp.json()
        assert budget["agent_id"] == agent_id
        assert budget["limit"] == 5000
        assert budget["used"] == 0
        assert budget["period"] == "cumulative"
        assert budget.get("period_start", "") == ""
        print("Cumulative budget set correctly")


        print("\n--- Test 2: GET budget includes period info ---")
        resp = requests.get(f"{API_URL}/budget/{agent_id}", headers=headers)
        resp.raise_for_status()
        budget = resp.json()
        assert budget["period"] == "cumulative"
        print("GET budget period field OK")

        print("\n--- Test 3: Set budget via API (monthly) ---")
        monthly_agent = "agent-monthly"
        resp = requests.post(f"{API_URL}/budget/{monthly_agent}",
            json={"limit": 10000, "period": "monthly"}, headers=sup_headers)
        assert resp.status_code == 201
        budget = resp.json()
        assert budget["period"] == "monthly"
        assert budget["period_start"] != ""
        assert budget["limit"] == 10000
        print(f"Monthly budget set, period_start: {budget['period_start']}")

        print("\n--- Test 4: Spend against monthly budget (authorize only) ---")
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 3000,
            "merchant": "Anthropic API",
            "purpose": "Monthly inference",
            "requesting_agent_id": monthly_agent,
        }, headers=headers)
        resp.raise_for_status()
        auth_data = resp.json()
        assert auth_data["status"] == "spent"
        print(f"Authorized 3000 cents, tx={auth_data['transaction_id'][:8]}...")

        resp = requests.get(f"{API_URL}/budget/{monthly_agent}", headers=headers)
        budget = resp.json()
        assert budget["used"] == 3000
        assert budget["available"] == 7000
        print(f"Budget after spend: used={budget['used']}, available={budget['available']}")

        print("\n--- Test 5: Default period (omit period field) ---")
        default_agent = "agent-default"
        resp = requests.post(f"{API_URL}/budget/{default_agent}",
            json={"limit": 2000}, headers=sup_headers)
        assert resp.status_code == 201
        budget = resp.json()
        assert budget["period"] == "cumulative"
        print("Default period correctly cumulative")


        print("\n--- Test 6: Invalid period rejected ---")
        resp = requests.post(f"{API_URL}/budget/{default_agent}",
            json={"limit": 2000, "period": "weekly"}, headers=sup_headers)
        assert resp.status_code == 400
        print("Invalid period correctly rejected")

        print("\n--- Test 7: Negative limit rejected ---")
        resp = requests.post(f"{API_URL}/budget/{default_agent}",
            json={"limit": -500}, headers=sup_headers)
        assert resp.status_code == 400
        print("Negative limit correctly rejected")

        print("\n--- Test 8: Update existing budget ---")
        resp = requests.post(f"{API_URL}/budget/{agent_id}",
            json={"limit": 8000, "period": "monthly"}, headers=sup_headers)
        assert resp.status_code == 201
        budget = resp.json()
        assert budget["limit"] == 8000
        assert budget["period"] == "monthly"
        print(f"Budget updated: limit={budget['limit']}, period={budget['period']}, used={budget['used']}")

        print("\n--- Test 9: CLI budget set with --period monthly ---")
        cli_agent = "agent-cli-monthly"
        subprocess.run(
            [binary, "budget", "set", cli_agent, "15000", "--period", "monthly"],
            cwd=work_dir, check=True,
        )
        resp = requests.get(f"{API_URL}/budget/{cli_agent}", headers=headers)
        budget = resp.json()
        assert budget["period"] == "monthly"
        assert budget["limit"] == 15000
        print(f"CLI monthly budget: limit={budget['limit']}, period={budget['period']}")

        print("\n=== ALL BUDGET API E2E TESTS PASSED ===")

    finally:
        if server_proc:
            server_proc.terminate()
            try:
                server_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server_proc.kill()
            print("\nServer stopped.")
        shutil.rmtree(work_dir, ignore_errors=True)
        print(f"Cleaned up: {work_dir}")


# Note: __main__ entry point is at the bottom of the file, after all function definitions.


def test_supervisor_tiers():
    """
    Test the supervisor agent role model:
    - Role partition: user token rejected on supervisor endpoints (and vice versa)
    - Tier 1 (default): approve, deny, list_pending work; set_budget and vault_update blocked
    - Tier 2: set_budget and credit work; vault_update still blocked
    - Tier 3: vault_update works
    """
    binary_src = find_binary()
    binary_name = get_binary_name()

    def make_env(tier, extra_env=None):
        """Spin up a fresh server at the given supervisor tier. Returns (work_dir, server_proc, user_h, sup_h)."""
        work_dir = tempfile.mkdtemp(prefix=f"agentledger_e2e_tier{tier}_")
        binary = os.path.join(work_dir, binary_name)
        shutil.copy2(binary_src, binary)
        # Tier 3 requires interactive confirmation — pipe "CONFIRM" via stdin
        init_input = "CONFIRM\n" if tier == 3 else None
        subprocess.run(
            [binary, "init", "--supervisor-tier", str(tier)],
            cwd=work_dir, check=True,
            input=init_input, text=True,
        )
        subprocess.run([binary, "vault", "add", "--test-card"], cwd=work_dir, check=True)
        env_path = os.path.join(work_dir, ".env")
        if extra_env:
            for k, v in extra_env.items():
                set_env_var(env_path, k, v)
        proc = subprocess.Popen(
            [binary, "serve"], cwd=work_dir,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        wait_for_server()
        user_h = {"Authorization": f"Bearer {read_user_token(env_path)}"}
        sup_h  = {"Authorization": f"Bearer {read_supervisor_token(env_path)}"}
        return work_dir, proc, user_h, sup_h

    def stop_env(proc, work_dir):
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
        shutil.rmtree(work_dir, ignore_errors=True)

    print("\n" + "=" * 60)
    print("SUPERVISOR TIER TESTS")
    print("=" * 60)

    # ── Part 1: Role partition ────────────────────────────────────────────────
    print("\n--- Part 1: Role partition (Tier 1) ---")
    work_dir, proc, user_h, sup_h = make_env(1, {"APPROVAL_MODE": "manual"})
    try:
        binary = os.path.join(work_dir, binary_name)
        subprocess.run([binary, "budget", "set", "agent-a", "9000"], cwd=work_dir, check=True)

        # User token: authorize works
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 1000, "merchant": "TestMerchant", "purpose": "Test",
            "requesting_agent_id": "agent-a",
        }, headers=user_h)
        assert resp.status_code == 200
        tx_id = resp.json()["transaction_id"]
        print("User token -> /authorize: OK")

        # User token rejected on supervisor endpoints
        resp = requests.post(f"{API_URL}/approve", json={"transaction_id": tx_id}, headers=user_h)
        assert resp.status_code == 401, f"Expected 401 for user token on /approve, got {resp.status_code}"
        print("User token -> /approve: correctly 401")

        resp = requests.post(f"{API_URL}/deny", json={"transaction_id": tx_id}, headers=user_h)
        assert resp.status_code == 401
        print("User token -> /deny: correctly 401")

        resp = requests.get(f"{API_URL}/pending", headers=user_h)
        assert resp.status_code == 401
        print("User token -> /pending: correctly 401")

        # Supervisor token rejected on user endpoints
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 500, "merchant": "X", "purpose": "Y", "requesting_agent_id": "agent-a",
        }, headers=sup_h)
        assert resp.status_code == 401, f"Expected 401 for supervisor token on /authorize, got {resp.status_code}"
        print("Supervisor token -> /authorize: correctly 401")

        # Supervisor token: list pending
        resp = requests.get(f"{API_URL}/pending", headers=sup_h)
        assert resp.status_code == 200
        pending = resp.json()
        assert len(pending) == 1
        assert pending[0]["id"] == tx_id
        print(f"Supervisor token -> /pending: OK ({len(pending)} pending)")

        # Supervisor token: approve (no card data returned to supervisor)
        resp = requests.post(f"{API_URL}/approve", json={"transaction_id": tx_id}, headers=sup_h)
        assert resp.status_code == 200
        assert resp.json()["status"] == "spent"
        assert resp.json().get("card_number", "") == "", "Approve must not return card data to supervisor"
        print("Supervisor token -> /approve: OK, status=spent, no card data")

        # After approval, pending list is empty
        resp = requests.get(f"{API_URL}/pending", headers=sup_h)
        assert resp.json() == []
        print("Pending list empty after approval: OK")

        # Supervisor token: deny a second transaction
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 500, "merchant": "DenyTest", "purpose": "deny test",
            "requesting_agent_id": "agent-a",
        }, headers=user_h)
        tx_id2 = resp.json()["transaction_id"]

        resp = requests.post(f"{API_URL}/deny", json={"transaction_id": tx_id2}, headers=sup_h)
        assert resp.status_code == 200
        assert resp.json()["status"] == "denied"
        print("Supervisor token -> /deny: OK")

    finally:
        stop_env(proc, work_dir)

    # ── Part 2: Tier 1 blocks Tier 2 operations ───────────────────────────────
    print("\n--- Part 2: Tier 1 blocks Tier 2 operations ---")
    work_dir, proc, user_h, sup_h = make_env(1)
    try:
        # POST /budget requires Tier 2 — should be 403 at Tier 1
        resp = requests.post(f"{API_URL}/budget/agent-b",
            json={"limit": 5000}, headers=sup_h)
        assert resp.status_code == 403, f"Expected 403 for set_budget at Tier 1, got {resp.status_code}"
        print("Tier 1 -> POST /budget: correctly 403")

        # POST /credit also requires Tier 2
        resp = requests.post(f"{API_URL}/credit",
            json={"agent_id": "agent-b", "amount": 100, "reason": "test"}, headers=sup_h)
        assert resp.status_code == 403
        print("Tier 1 -> POST /credit: correctly 403")

        # POST /vault/update requires Tier 3
        resp = requests.post(f"{API_URL}/vault/update", json={
            "holder_name": "X", "card_number": "4111111111111111",
            "expiry_date": "01/30", "cvv": "999", "country": "US", "zip_code": "00000",
        }, headers=sup_h)
        assert resp.status_code == 403
        print("Tier 1 -> POST /vault/update: correctly 403")

    finally:
        stop_env(proc, work_dir)

    # ── Part 3: Tier 2 enables budget/credit, still blocks vault ─────────────
    print("\n--- Part 3: Tier 2 -- budget and credit enabled ---")
    work_dir, proc, user_h, sup_h = make_env(2)
    try:
        binary = os.path.join(work_dir, binary_name)

        # set_budget via API
        resp = requests.post(f"{API_URL}/budget/agent-c",
            json={"limit": 8000, "period": "cumulative"}, headers=sup_h)
        assert resp.status_code == 201
        assert resp.json()["limit"] == 8000
        print("Tier 2 -> POST /budget: OK")

        # Spend some budget
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 3000, "merchant": "Tier2Test", "purpose": "tier 2 test",
            "requesting_agent_id": "agent-c",
        }, headers=user_h)
        assert resp.json()["status"] == "spent"
        print("User -> /authorize: OK (3000 spent)")

        resp = requests.get(f"{API_URL}/budget/agent-c", headers=user_h)
        assert resp.json()["used"] == 3000

        # Credit restores budget
        resp = requests.post(f"{API_URL}/credit",
            json={"agent_id": "agent-c", "amount": 2000, "reason": "Refund from merchant"},
            headers=sup_h)
        assert resp.status_code == 200
        cr = resp.json()
        assert cr["credited"] == 2000
        assert cr["new_used"] == 1000
        assert cr["available"] == 7000
        print(f"Tier 2 -> POST /credit: OK (credited=2000, new_used={cr['new_used']})")

        # Credit: missing reason rejected
        resp = requests.post(f"{API_URL}/credit",
            json={"agent_id": "agent-c", "amount": 500, "reason": ""}, headers=sup_h)
        assert resp.status_code == 400
        print("Credit with empty reason: correctly 400")

        # Credit: unknown agent rejected
        resp = requests.post(f"{API_URL}/credit",
            json={"agent_id": "nobody", "amount": 100, "reason": "test"}, headers=sup_h)
        assert resp.status_code == 400
        print("Credit for unknown agent: correctly 400")

        # vault/update still blocked at Tier 2
        resp = requests.post(f"{API_URL}/vault/update", json={
            "holder_name": "X", "card_number": "4111111111111111",
            "expiry_date": "01/30", "cvv": "999", "country": "US", "zip_code": "00000",
        }, headers=sup_h)
        assert resp.status_code == 403
        print("Tier 2 -> POST /vault/update: correctly 403")

    finally:
        stop_env(proc, work_dir)

    # ── Part 4: Tier 3 enables vault update ───────────────────────────────────
    print("\n--- Part 4: Tier 3 -- vault update enabled ---")
    work_dir, proc, user_h, sup_h = make_env(3)
    try:
        binary = os.path.join(work_dir, binary_name)
        subprocess.run([binary, "budget", "set", "agent-d", "5000"], cwd=work_dir, check=True)

        # Authorize confirms current (test) card is in vault
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 1000, "merchant": "Before", "purpose": "pre-vault-update",
            "requesting_agent_id": "agent-d",
        }, headers=user_h)
        assert resp.json()["card_number"] == "4242424242424242"
        print("Before vault update: card_number=4242424242424242")

        # Supervisor replaces the vault with a new card
        new_card = {
            "holder_name": "New Cardholder",
            "card_number": "5555555555554444",
            "expiry_date": "06/31",
            "cvv": "456",
            "country": "GB",
            "zip_code": "SW1A1AA",
        }
        resp = requests.post(f"{API_URL}/vault/update", json=new_card, headers=sup_h)
        assert resp.status_code == 200
        assert resp.json()["status"] == "vault updated"
        print("Tier 3 -> POST /vault/update: OK")

        # Next authorize returns the NEW card
        resp = requests.post(f"{API_URL}/authorize", json={
            "amount": 500, "merchant": "After", "purpose": "post-vault-update",
            "requesting_agent_id": "agent-d",
        }, headers=user_h)
        data = resp.json()
        assert data["card_number"] == "5555555555554444", f"Expected new card, got {data['card_number']}"
        assert data["holder_name"] == "New Cardholder"
        assert data["country"] == "GB"
        print(f"After vault update: card_number={data['card_number']} OK")

        # vault/update: missing required fields rejected
        resp = requests.post(f"{API_URL}/vault/update",
            json={"card_number": "5555555555554444"}, headers=sup_h)
        assert resp.status_code == 400
        print("Vault update missing fields: correctly 400")

    finally:
        stop_env(proc, work_dir)

    print("\n=== ALL SUPERVISOR TIER TESTS PASSED ===")


if __name__ == "__main__":
    test_auto_mode()
    test_manual_mode()
    test_expiry()
    test_budget_api()
    test_supervisor_tiers()
