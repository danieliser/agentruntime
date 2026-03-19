#!/usr/bin/env bash
# End-to-end test for named persistent chat sessions.
# Requires agentd running on localhost:8090.
set -euo pipefail

BASE="http://localhost:8090"
CHAT="e2e-test-$$"
PASS=0
FAIL=0
SKIP=0

green() { printf "\033[32m✓ %s\033[0m\n" "$1"; PASS=$((PASS+1)); }
red()   { printf "\033[31m✗ %s\033[0m\n" "$1"; FAIL=$((FAIL+1)); }
yellow(){ printf "\033[33m⊘ %s\033[0m\n" "$1"; SKIP=$((SKIP+1)); }
info()  { printf "\033[36m→ %s\033[0m\n" "$1"; }

cleanup() {
    info "Cleaning up chat: $CHAT"
    curl -sS -X DELETE "$BASE/chats/$CHAT" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# -------------------------------------------------------------------
info "Checking agentd health..."
HEALTH=$(curl -sS "$BASE/health" 2>/dev/null) || { red "agentd not responding"; exit 1; }
echo "$HEALTH" | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['status']=='ok'" 2>/dev/null \
    && green "Step 0: agentd healthy" \
    || { red "Step 0: agentd unhealthy"; exit 1; }

# -------------------------------------------------------------------
info "Step 1: Create named chat"
RESP=$(curl -sS "$BASE/chats" \
    -H 'content-type: application/json' \
    -d "{\"name\":\"$CHAT\",\"config\":{\"agent\":\"claude\",\"idle_timeout\":\"1m\"}}")
STATE=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('state',''))" 2>/dev/null)
[ "$STATE" = "created" ] && green "Step 1: Chat created (state=created)" || red "Step 1: Expected state=created, got $STATE"

# -------------------------------------------------------------------
info "Step 2: Send first message"
RESP=$(curl -sS "$BASE/chats/$CHAT/messages" \
    -H 'content-type: application/json' \
    -d '{"message":"Say exactly: e2e test passed. Nothing else."}')
SPAWNED=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('spawned',False))" 2>/dev/null)
SID1=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('session_id',''))" 2>/dev/null)
[ "$SPAWNED" = "True" ] && green "Step 2: Message sent, session spawned ($SID1)" || red "Step 2: Expected spawned=true, got $SPAWNED"

# -------------------------------------------------------------------
info "Step 3: Check state = running"
RESP=$(curl -sS "$BASE/chats/$CHAT")
STATE=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('state',''))" 2>/dev/null)
CHAIN=$(echo "$RESP" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('session_chain',[])))" 2>/dev/null)
[ "$STATE" = "running" ] && green "Step 3: State=running, chain=$CHAIN" || red "Step 3: Expected running, got $STATE"

# -------------------------------------------------------------------
info "Step 4: Wait for idle transition (polling up to 60s)..."
for i in $(seq 1 30); do
    STATE=$(curl -sS "$BASE/chats/$CHAT" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('state',''))" 2>/dev/null)
    [ "$STATE" = "idle" ] && break
    sleep 2
done
[ "$STATE" = "idle" ] && green "Step 4: Transitioned to idle" || red "Step 4: Expected idle, got $STATE after 60s"

# -------------------------------------------------------------------
info "Step 5: Check message history"
RESP=$(curl -sS "$BASE/chats/$CHAT/messages?limit=50")
AGENT_MSGS=$(echo "$RESP" | python3 -c "
import sys,json
data = json.load(sys.stdin)
msgs = [m for m in data.get('messages',[]) if m['type']=='agent_message' and not m.get('data',{}).get('delta')]
for m in msgs:
    print(m.get('data',{}).get('text',''))
" 2>/dev/null)
if echo "$AGENT_MSGS" | grep -qi "e2e test passed"; then
    green "Step 5: Message history contains 'e2e test passed'"
else
    red "Step 5: Expected 'e2e test passed' in history, got: $AGENT_MSGS"
fi

# Check no system/init events leaked
SYSTEM_COUNT=$(echo "$RESP" | python3 -c "import sys,json; print(len([m for m in json.load(sys.stdin).get('messages',[]) if m['type']=='system']))" 2>/dev/null)
[ "$SYSTEM_COUNT" = "0" ] && green "Step 5b: No system events in history" || red "Step 5b: Found $SYSTEM_COUNT system events in history"

# -------------------------------------------------------------------
info "Step 6: Send second message (respawn + resume test)"
RESP=$(curl -sS "$BASE/chats/$CHAT/messages" \
    -H 'content-type: application/json' \
    -d '{"message":"What was the last thing you said? Reply with just that text."}')
SPAWNED=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('spawned',False))" 2>/dev/null)
SID2=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('session_id',''))" 2>/dev/null)
[ "$SPAWNED" = "True" ] && green "Step 6: Respawned with new session ($SID2)" || red "Step 6: Expected spawned=true"
[ "$SID1" != "$SID2" ] && green "Step 6b: Different session ID from first" || red "Step 6b: Same session ID — not a new session"

# -------------------------------------------------------------------
info "Step 7: Check session chain grew"
for i in $(seq 1 15); do
    CHAIN=$(curl -sS "$BASE/chats/$CHAT" 2>/dev/null | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('session_chain',[])))" 2>/dev/null)
    [ "$CHAIN" = "2" ] && break
    sleep 2
done
[ "$CHAIN" = "2" ] && green "Step 7: Session chain has 2 entries" || red "Step 7: Expected chain=2, got $CHAIN"

# Wait for idle again before continuing
info "Waiting for idle..."
for i in $(seq 1 30); do
    STATE=$(curl -sS "$BASE/chats/$CHAT" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('state',''))" 2>/dev/null)
    [ "$STATE" = "idle" ] && break
    sleep 2
done

# -------------------------------------------------------------------
info "Step 8: List all chats"
RESP=$(curl -sS "$BASE/chats")
FOUND=$(echo "$RESP" | python3 -c "import sys,json; print(any(c['name']=='$CHAT' for c in json.load(sys.stdin)))" 2>/dev/null)
[ "$FOUND" = "True" ] && green "Step 8: Chat found in list" || red "Step 8: Chat not in list"

# -------------------------------------------------------------------
info "Step 9: Concurrent message test"
# Send first to start a session
curl -sS "$BASE/chats/$CHAT/messages" -H 'content-type: application/json' -d '{"message":"Count to 100 slowly."}' >/dev/null 2>&1
sleep 1
# Now send concurrent — should get 429 or queue
RESP=$(curl -sS "$BASE/chats/$CHAT/messages" -H 'content-type: application/json' -d '{"message":"interrupt"}')
# Check if it was rejected (429) or queued
STATUS=$(echo "$RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('error','accepted'))" 2>/dev/null)
if echo "$STATUS" | grep -qi "busy\|queue\|429"; then
    green "Step 9: Concurrent message rejected/queued ($STATUS)"
else
    yellow "Step 9: Concurrent message accepted (no rejection). Status: $STATUS"
fi

# Wait for idle
info "Waiting for idle..."
for i in $(seq 1 30); do
    STATE=$(curl -sS "$BASE/chats/$CHAT" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('state',''))" 2>/dev/null)
    [ "$STATE" = "idle" ] && break
    sleep 2
done

# -------------------------------------------------------------------
info "Step 10: Config mutation (idle only)"
RESP=$(curl -sS -X PATCH "$BASE/chats/$CHAT/config" \
    -H 'content-type: application/json' \
    -d '{"model":"claude-sonnet-4-6"}')
if echo "$RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'error' not in d" 2>/dev/null; then
    green "Step 10: Config patched while idle"
else
    red "Step 10: Config patch failed: $RESP"
fi

# Verify model was set
MODEL=$(curl -sS "$BASE/chats/$CHAT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('config',{}).get('model',''))" 2>/dev/null)
[ "$MODEL" = "claude-sonnet-4-6" ] && green "Step 10b: Model updated to claude-sonnet-4-6" || red "Step 10b: Expected claude-sonnet-4-6, got $MODEL"

# -------------------------------------------------------------------
info "Step 11: Config mutation blocked when running"
curl -sS "$BASE/chats/$CHAT/messages" -H 'content-type: application/json' -d '{"message":"Wait 5 seconds then say done."}' >/dev/null 2>&1
sleep 1
RESP=$(curl -sS -X PATCH "$BASE/chats/$CHAT/config" \
    -H 'content-type: application/json' \
    -d '{"model":"claude-opus-4-6"}')
if echo "$RESP" | grep -q "idle"; then
    green "Step 11: Config patch correctly blocked while running"
else
    red "Step 11: Config patch should be blocked while running: $RESP"
fi

# Wait for idle
info "Waiting for idle..."
for i in $(seq 1 30); do
    STATE=$(curl -sS "$BASE/chats/$CHAT" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('state',''))" 2>/dev/null)
    [ "$STATE" = "idle" ] && break
    sleep 2
done

# -------------------------------------------------------------------
info "Step 12: Delete chat"
RESP=$(curl -sS -X DELETE "$BASE/chats/$CHAT")
DELETED=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('deleted',False))" 2>/dev/null)
[ "$DELETED" = "True" ] && green "Step 12: Chat deleted with JSON response" || red "Step 12: Expected {deleted:true}, got: $RESP"

# Verify gone
RESP=$(curl -sS "$BASE/chats")
FOUND=$(echo "$RESP" | python3 -c "import sys,json; print(any(c['name']=='$CHAT' for c in json.load(sys.stdin)))" 2>/dev/null)
[ "$FOUND" = "False" ] && green "Step 12b: Chat gone from list" || red "Step 12b: Chat still in list"

# -------------------------------------------------------------------
echo ""
echo "======================================"
echo "  Results: $PASS passed, $FAIL failed, $SKIP skipped"
echo "======================================"
[ "$FAIL" -eq 0 ] && exit 0 || exit 1
