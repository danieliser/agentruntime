// Dashboard app state
const state = {
    sessions: [],
    history: [],
    health: null,
    currentSessionId: null,
    currentTab: 'active',
    ws: null,
    refreshInterval: null,
    historyRefreshInterval: null,
    eventLogs: {}, // keyed by sessionId
    eventSequences: {}, // durable sequence pointer keyed by sessionId
	 detailTrigger: null,
	 authToken: sessionStorage.getItem('agentd.dashboard.token') || '',
};

const terminalStates = new Set([
    'completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate'
]);

// Initialize app
document.addEventListener('DOMContentLoaded', () => {
    setupEventListeners();
    fetchHealth();
	 initializeAuthenticatedDashboard();

    // Auto-refresh active sessions every 3 seconds
    state.refreshInterval = setInterval(() => {
        fetchHealth();
        if (state.currentTab === 'active') {
			if (state.authToken) fetchSessions();
        }
    }, 3000);

    // Auto-refresh history every 10 seconds
    state.historyRefreshInterval = setInterval(() => {
        if (state.currentTab === 'history') {
			if (state.authToken) fetchHistory();
        }
    }, 10000);
});

async function initializeAuthenticatedDashboard() {
	if (!state.authToken) await requestDashboardToken();
	let authRejected = false;
	try {
		const response = await apiFetch('/api/v1/capabilities');
		if (!response.ok) {
			authRejected = response.status === 401;
			throw new Error(`HTTP ${response.status}`);
		}
		sessionStorage.setItem('agentd.dashboard.token', state.authToken);
		await Promise.all([fetchSessions(), fetchHistory()]);
	} catch (error) {
		sessionStorage.removeItem('agentd.dashboard.token');
		state.authToken = '';
		const authError = document.getElementById('dashboard-auth-error');
		authError.textContent = authRejected
			? 'That token was not accepted.'
			: 'AgentD is not reachable. Your token was not rejected.';
		authError.classList.remove('hidden');
		await requestDashboardToken();
		return initializeAuthenticatedDashboard();
	}
}

function requestDashboardToken() {
	const gate = document.getElementById('dashboard-auth');
	const form = document.getElementById('dashboard-auth-form');
	const input = document.getElementById('dashboard-auth-token');
	gate.classList.remove('hidden');
	input.focus();
	return new Promise((resolve) => {
		form.onsubmit = (event) => {
			event.preventDefault();
			state.authToken = input.value.trim();
			if (!state.authToken) return;
			document.getElementById('dashboard-auth-error').classList.add('hidden');
			gate.classList.add('hidden');
			input.value = '';
			resolve();
		};
	});
}

function apiFetch(path, options = {}) {
	const headers = new Headers(options.headers || {});
	headers.set('Authorization', `Bearer ${state.authToken}`);
	return fetch(path, {...options, headers});
}

// Setup event listeners
function setupEventListeners() {
    const closeBtn = document.getElementById('close-detail');
    closeBtn.addEventListener('click', closeDetailPanel);
	document.addEventListener('keydown', (event) => {
		if (event.key === 'Escape' && state.currentSessionId) closeDetailPanel();
	});

	for (const tableID of ['sessions-table', 'history-table']) {
		const table = document.getElementById(tableID);
		table.addEventListener('click', openSessionFromRow);
		table.addEventListener('keydown', openSessionFromRow);
	}

    // Tab switching
    document.querySelectorAll('.tab-btn').forEach(btn => {
        btn.addEventListener('click', (e) => {
            const tabName = e.target.dataset.tab;
            switchTab(tabName);
        });
    });
}

function openSessionFromRow(event) {
	if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ') return;
	if (event.target.closest('.session-row-actions')) return;
	const row = event.target.closest('tbody tr[data-session-id]');
	if (!row) return;
	if (event.type === 'keydown') event.preventDefault();
	showDetailPanel(row.dataset.sessionId, row);
}

// Switch between tabs
function switchTab(tabName) {
    state.currentTab = tabName;

    // Update active tab button
    document.querySelectorAll('.tab-btn').forEach(btn => {
        if (btn.dataset.tab === tabName) {
            btn.classList.add('active');
        } else {
            btn.classList.remove('active');
        }
    });

    // Show/hide tab content
    document.querySelectorAll('.tab-content').forEach(content => {
        content.classList.remove('active');
    });
    document.getElementById(`tab-${tabName}`).classList.add('active');

    // Fetch data for the current tab
    if (tabName === 'active') {
        fetchSessions();
    } else if (tabName === 'history') {
        fetchHistory();
    }
}

// Fetch health status
async function fetchHealth() {
    try {
        const res = await fetch('/health');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        state.health = await res.json();
        updateHealthBadge();
    } catch (err) {
        console.error('Health fetch error:', err);
        updateHealthBadge(err);
    }
}

// Update health badge
function updateHealthBadge(err) {
    const badge = document.getElementById('status-badge');
    const runtimeList = document.getElementById('runtime-list');

    if (err) {
        badge.textContent = 'Offline';
        badge.className = 'status-badge error';
        runtimeList.textContent = '';
        return;
    }

    if (!state.health) return;

    badge.textContent = 'Online';
    badge.className = 'status-badge ok';

    const runtimes = state.health.runtimes || [];
    const defaultRuntime = state.health.default_runtime || '';
    const runtimeText = runtimes.length > 0
        ? `${runtimes.join(', ')} (default: ${defaultRuntime})`
        : 'No runtimes';

    runtimeList.textContent = runtimeText;
}

// Fetch sessions list
async function fetchSessions() {
    try {
		const res = await apiFetch('/api/v1/sessions');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const envelope = await res.json();
        const all = envelope.data || [];
        // Active tab only shows non-terminal sessions
        state.sessions = all.filter(s => !terminalStates.has(s.state));
        updateSessionsTable();
    } catch (err) {
        console.error('Sessions fetch error:', err);
    }
}

// Update sessions table
function updateSessionsTable() {
    const tbody = document.getElementById('sessions-body');

    if (!state.sessions || state.sessions.length === 0) {
        tbody.innerHTML = '<tr><td colspan="7" class="empty-message">No sessions</td></tr>';
        return;
    }

    tbody.innerHTML = state.sessions.map(session => {
        const statusClass = getStatusClass(session.state);
        const sessionIdShort = session.session_id.substring(0, 8);

        return `
            <tr data-session-id="${escapeAttr(session.session_id)}" tabindex="0">
                <td class="session-primary-cell">
                    <span class="session-id">${escapeHtml(sessionIdShort)}…</span>
                    <div class="session-row-actions">
                        <button class="btn btn-view" data-action="info" data-session-id="${escapeAttr(session.session_id)}">View</button>
                        <button class="btn btn-delete" data-action="delete" data-session-id="${escapeAttr(session.session_id)}">Cancel</button>
                    </div>
                </td>
                <td>${escapeHtml(session.agent)}</td>
                <td>${escapeHtml(session.runtime)}</td>
                <td>
                    <span class="session-status ${statusClass}">
                        ${escapeHtml(session.state)}
                    </span>
                </td>
                <td>-</td>
                <td>-</td>
                <td>-</td>
            </tr>
        `;
    }).join('');

    // Add event listeners to action buttons
    tbody.querySelectorAll('[data-action]').forEach(btn => {
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            const action = btn.dataset.action;
            const sessionId = btn.dataset.sessionId;
            if (action === 'info') {
                showDetailPanel(sessionId, btn);
            } else if (action === 'delete') {
                deleteSession(sessionId);
            }
        });
    });
}

// Fetch session history
async function fetchHistory() {
    try {
		const res = await apiFetch('/api/v1/sessions');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const envelope = await res.json();
        state.history = (envelope.data || [])
            .filter(s => terminalStates.has(s.state))
            .sort((left, right) => new Date(right.updated_at) - new Date(left.updated_at))
            .slice(0, 50);
        updateHistoryTable();
    } catch (err) {
        console.error('History fetch error:', err);
    }
}

// Update history table
function updateHistoryTable() {
    const tbody = document.getElementById('history-body');

    if (!state.history || state.history.length === 0) {
        tbody.innerHTML = '<tr><td colspan="9" class="empty-message">No history</td></tr>';
        return;
    }

    tbody.innerHTML = state.history.map(entry => {
        const statusClass = getStatusClass(entry.state);
        const sessionIdShort = entry.session_id.substring(0, 8);
        const duration = entry.created_at && entry.updated_at
            ? formatDuration(new Date(entry.updated_at) - new Date(entry.created_at))
            : '-';
        const date = entry.updated_at
            ? new Date(entry.updated_at).toLocaleString()
            : '-';

        return `
            <tr data-session-id="${escapeAttr(entry.session_id)}" tabindex="0">
                <td class="session-primary-cell">
                    <span class="session-id">${escapeHtml(sessionIdShort)}…</span>
                    <div class="session-row-actions">
                        <button class="btn btn-view" data-action="info" data-session-id="${escapeAttr(entry.session_id)}">View</button>
						<button class="btn btn-view" data-action="resume" data-session-id="${escapeAttr(entry.session_id)}" ${entry.resumable ? '' : 'disabled title="Provider state was not retained"'}>Continue</button>
                    </div>
                </td>
                <td>${escapeHtml(entry.agent || '-')}</td>
                <td>
                    <span class="session-status ${statusClass}">
                        ${escapeHtml(entry.state || '-')}
                    </span>
                </td>
                <td>${escapeHtml(duration)}</td>
                <td>-</td>
                <td>-</td>
                <td>-</td>
                <td>-</td>
                <td><span class="date-small">${escapeHtml(date)}</span></td>
            </tr>
        `;
    }).join('');

    // Add event listeners to action buttons
    tbody.querySelectorAll('[data-action]').forEach(btn => {
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            const action = btn.dataset.action;
            const sessionId = btn.dataset.sessionId;
            if (action === 'info') {
                showDetailPanel(sessionId, btn);
            } else if (action === 'resume') {
                const session = state.history.find(entry => entry.session_id === sessionId);
                if (session) resumeConsoleSession(session);
            }
        });
    });
}

// Get status CSS class
function getStatusClass(status) {
    switch (status) {
        case 'running':
            return 'status-running';
        case 'completed':
            return 'status-completed';
        case 'failed':
        case 'cancelled':
        case 'timed_out':
        case 'crashed':
        case 'indeterminate':
            return 'status-failed';
        case 'pending':
        default:
            return 'status-pending';
    }
}

// Show detail panel
async function showDetailPanel(sessionId, trigger = document.activeElement) {
    state.currentSessionId = sessionId;
	state.detailTrigger = trigger;

    // Disconnect old WS if any
    if (state.ws) {
        state.ws.close();
        state.ws = null;
    }

	renderDetailLoading(sessionId);
	setDetailPanelOpen(true);

	// Fetch session info
    try {
		const res = await apiFetch(`/api/v1/sessions/${sessionId}`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const envelope = await res.json();
        const info = envelope.data;
		if (state.currentSessionId !== sessionId) return;
        renderDetailPanel(info);
		const transcript = await fetchConversationTranscript(sessionId);
		if (state.currentSessionId !== sessionId) return;
		renderConversationTranscript(document.getElementById('event-log'), transcript.messages);
		state.eventSequences[sessionId] = transcript.lastSequence;
		if (!terminalStates.has(info.state)) connectWebSocket(sessionId);
    } catch (err) {
        console.error('Session info fetch error:', err);
		if (state.currentSessionId === sessionId) {
			renderConversationTranscript(document.getElementById('event-log'), [], 'Conversation history could not be loaded.');
		}
    }
}

function renderDetailLoading(sessionId) {
	const content = document.getElementById('detail-content');
	content.innerHTML = '';
	content.appendChild(createDetailField('Session ID', sessionId));
	renderConversationTranscript(document.getElementById('event-log'), [], 'Loading conversation...');
}

function setDetailPanelOpen(open) {
	const panel = document.getElementById('detail-panel');
	panel.classList.toggle('is-open', open);
	panel.setAttribute('aria-hidden', String(!open));
	panel.inert = !open;
}

// Create detail field element
function createDetailField(label, value) {
    const div = document.createElement('div');
    div.className = 'detail-field';

    const labelSpan = document.createElement('span');
    labelSpan.className = 'detail-field-label';
    labelSpan.textContent = label;

    const valueSpan = document.createElement('span');
    valueSpan.className = 'detail-field-value';
    valueSpan.textContent = value || '-';

    div.appendChild(labelSpan);
    div.appendChild(valueSpan);

    return div;
}

// Render detail panel
function renderDetailPanel(info) {
    const content = document.getElementById('detail-content');

    // Clear previous content
    content.innerHTML = '';

    // Calculate uptime if still running
    let uptime = '-';
    if (!terminalStates.has(info.state)) {
        const createdAt = new Date(info.created_at);
        const now = new Date();
        uptime = formatDuration(now - createdAt);
    }

    // Create fields using DOM methods
    const fields = [
        ['Session ID', info.session_id],
        ['Agent', info.agent],
        ['Runtime', info.runtime],
        ['Status', info.state],
        ['Created', new Date(info.created_at).toLocaleString()],
        ['Uptime', uptime || '-'],
        ['Generation', String(info.generation)],
        ['Last Sequence', String(info.last_sequence)],
        ['Updated', new Date(info.updated_at).toLocaleString()],
    ];

    if (info.exit_code !== null && info.exit_code !== undefined) {
        fields.push(['Exit Code', String(info.exit_code)]);
    }

    if (info.session_dir) {
        fields.push(['Session Dir', info.session_dir]);
    }

    if (info.volume_name) {
        fields.push(['Volume', info.volume_name]);
    }

    fields.forEach(([label, value]) => {
        content.appendChild(createDetailField(label, value));
    });

    // Clear event log
    const eventLog = document.getElementById('event-log');
    eventLog.innerHTML = '';
    const systemEntry = document.createElement('div');
    systemEntry.className = 'event-log-entry system';
    systemEntry.textContent = 'Loading conversation...';
    eventLog.appendChild(systemEntry);

    state.eventLogs[state.currentSessionId] = [];

	setDetailPanelOpen(true);
}

// Connect to WebSocket
function connectWebSocket(sessionId) {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const afterSequence = state.eventSequences[sessionId] || 0;
	const ws = new WebSocket(
		`${protocol}//${window.location.host}/api/v1/ws/sessions/${sessionId}/events?after_sequence=${afterSequence}`,
		['agentd.v1', `agentd.auth.${state.authToken}`],
	);

    ws.onopen = () => {};

    ws.onmessage = (event) => {
        try {
            const data = JSON.parse(event.data);
            handleSessionEvent(data);
        } catch (err) {
            console.error('Failed to parse WS message:', err);
        }
    };

    ws.onerror = (err) => {
        console.error('WebSocket error:', err);
    };

    ws.onclose = () => {};

    state.ws = ws;
}

// Handle session events
function handleSessionEvent(event) {
	if (event.frame_type === 'stream.ready') return;
    if (event.frame_type === 'error') {
		console.error('Session stream error:', event.error?.message || 'Event stream failed');
        return;
    }
    if (event.sequence) {
        const previous = state.eventSequences[event.session_id] || 0;
        if (event.sequence !== previous + 1) {
			console.error(`Sequence gap: expected ${previous + 1}, received ${event.sequence}`);
            state.ws?.close();
            return;
        }
        state.eventSequences[event.session_id] = event.sequence;
    }
	const eventLog = document.getElementById('event-log');
	const userText = userTextFromConversationEvent(event);
	if (userText) {
		appendConversationMessageTo(eventLog, 'user', userText);
		return;
	}
	if (event.type === 'content.delta') {
		appendConversationDeltaTo(eventLog, event.payload?.text || '');
		return;
	}
	const assistantText = completedAssistantTextFromConversationEvent(event);
	const lastBody = eventLog.lastElementChild?.dataset.role === 'assistant' ?
		eventLog.lastElementChild.querySelector('.conversation-text') : null;
	if (assistantText && !lastBody?.textContent) appendConversationMessageTo(eventLog, 'assistant', assistantText);
}

// Close detail panel
function closeDetailPanel() {
    if (state.ws) {
        state.ws.close();
        state.ws = null;
    }
    state.currentSessionId = null;
	setDetailPanelOpen(false);
	if (state.detailTrigger?.isConnected) state.detailTrigger.focus({preventScroll: true});
	state.detailTrigger = null;
}

// Delete session
async function deleteSession(sessionId) {
    if (!confirm(`Cancel session ${sessionId.substring(0, 8)}…?`)) {
        return;
    }

    try {
		const res = await apiFetch(`/api/v1/sessions/${sessionId}/cancel`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ idempotency_key: `dashboard-cancel:${sessionId}` }),
        });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);

        // Close detail panel if it was showing this session
        if (state.currentSessionId === sessionId) {
            closeDetailPanel();
        }

        // Refresh list
        fetchSessions();
    } catch (err) {
        console.error('Cancel session error:', err);
        alert('Failed to cancel session');
    }
}

// Format duration (milliseconds)
function formatDuration(ms) {
    const seconds = Math.floor(ms / 1000);
    const minutes = Math.floor(seconds / 60);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (days > 0) {
        return `${days}d ${hours % 24}h`;
    } else if (hours > 0) {
        return `${hours}h ${minutes % 60}m`;
    } else if (minutes > 0) {
        return `${minutes}m ${seconds % 60}s`;
    } else {
        return `${seconds}s`;
    }
}

// Escape HTML
function escapeHtml(text) {
    if (!text) return '';
    const map = {
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;',
        "'": '&#039;'
    };
    return text.replace(/[&<>"']/g, m => map[m]);
}

// Escape HTML attribute
function escapeAttr(text) {
    if (!text) return '';
    return text.replace(/"/g, '&quot;').replace(/'/g, '&#039;');
}
