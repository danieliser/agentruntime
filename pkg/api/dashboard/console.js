const consoleState = {
    sessionId: null,
    sequence: 0,
    ws: null,
    terminal: true,
    eventCount: 0,
	resumeID: null,
	resumable: false,
    runtime: null,
    authJSON: '',
    reconnectTimer: null,
};

const modelProfiles = {
    codex: {
        models: ['gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-5.5'],
        effortsByModel: {
            'gpt-5.6-sol': ['low', 'medium', 'high', 'xhigh', 'max', 'ultra'],
            'gpt-5.6-terra': ['low', 'medium', 'high', 'xhigh', 'max', 'ultra'],
            'gpt-5.6-luna': ['low', 'medium', 'high', 'xhigh', 'max'],
            'gpt-5.5': ['low', 'medium', 'high', 'xhigh'],
        },
        defaultModel: 'gpt-5.6-sol',
        defaultEffort: 'high',
    },
    claude: {
        models: ['claude-opus-5', 'claude-sonnet-5', 'claude-fable-5', 'claude-opus-4-8'],
        effortsByModel: {
            'claude-opus-5': ['low', 'medium', 'high', 'xhigh', 'max'],
            'claude-sonnet-5': ['low', 'medium', 'high', 'xhigh', 'max'],
            'claude-fable-5': ['low', 'medium', 'high', 'xhigh', 'max'],
            'claude-opus-4-8': ['low', 'medium', 'high', 'xhigh', 'max'],
        },
        defaultModel: 'claude-opus-5',
        defaultEffort: 'high',
    },
};

const consoleControlPaths = {
    input: '/input',
    interrupt: '/interrupt',
    cancel: '/cancel',
};

document.addEventListener('DOMContentLoaded', () => {
    state.currentTab = 'console';
    bindConsoleControls();
    updateModelControls();
});

function bindConsoleControls() {
    document.getElementById('console-agent').addEventListener('change', updateModelControls);
    document.getElementById('console-model').addEventListener('change', updateModelSpecificControls);
    document.getElementById('console-restricted').addEventListener('change', updateRestrictedControls);
    document.getElementById('console-auth-file').addEventListener('change', loadConsoleAuthFile);
    document.getElementById('console-form').addEventListener('submit', launchConsoleSession);
    document.getElementById('console-steer-form').addEventListener('submit', steerConsoleSession);
    document.getElementById('console-interrupt').addEventListener('click', () => controlConsoleSession('interrupt'));
    document.getElementById('console-cancel').addEventListener('click', () => controlConsoleSession('cancel'));
}

function updateModelControls() {
    const agent = document.getElementById('console-agent').value;
    const profile = modelProfiles[agent];
    replaceOptions('console-model', profile.models, profile.defaultModel);
    document.querySelectorAll('.claude-field').forEach((field) => {
        field.hidden = agent !== 'claude';
    });
    const restricted = document.getElementById('console-restricted');
    if (agent === 'claude') restricted.checked = false;
    restricted.disabled = agent === 'claude';
    updateRestrictedControls();
    updateModelSpecificControls();
}

function updateModelSpecificControls() {
    const agent = document.getElementById('console-agent').value;
    const profile = modelProfiles[agent];
    const model = document.getElementById('console-model').value;
    const previousEffort = document.getElementById('console-effort').value;
    const efforts = profile.effortsByModel[model];
    const selected = efforts.includes(previousEffort) ? previousEffort : profile.defaultEffort;
    replaceOptions('console-effort', efforts, selected);
    updateFastControl();
}

function replaceOptions(id, values, selected) {
    const select = document.getElementById(id);
    select.replaceChildren(...values.map((value) => {
        const option = document.createElement('option');
        option.value = value;
        option.textContent = value;
        option.selected = value === selected;
        return option;
    }));
}

function updateFastControl() {
    const agent = document.getElementById('console-agent').value;
    const model = document.getElementById('console-model').value;
    const fast = document.getElementById('console-fast');
    const supported = (agent === 'codex' && model === 'gpt-5.5') ||
        (agent === 'claude' && (model === 'claude-opus-5' || model === 'claude-opus-4-8'));
    fast.disabled = !supported;
    if (!supported) fast.checked = false;
}

function updateRestrictedControls() {
    const restricted = document.getElementById('console-restricted').checked;
    document.querySelectorAll('.restricted-field').forEach((field) => {
        field.hidden = !restricted;
    });
}

async function loadConsoleAuthFile(event) {
    consoleState.authJSON = '';
    const file = event.target.files?.[0];
    if (!file) return;
    try {
        const raw = await file.text();
        const parsed = JSON.parse(raw);
        if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') {
            throw new Error('auth.json must contain one JSON object');
        }
        consoleState.authJSON = raw;
        setConsoleNote('Restricted credential loaded in memory for the next launch.');
    } catch (error) {
        event.target.value = '';
        setConsoleNote(error.message, true);
    }
}

async function launchConsoleSession(event) {
    event.preventDefault();
    if (!state.authToken) {
        setConsoleNote('Enter the AgentD dashboard token before launching.', true);
        return;
    }
    const launchButton = document.getElementById('console-launch');
    launchButton.disabled = true;
    setConsoleConnection('Admitting…');
    resetConsoleStream();
    try {
        await admitConsoleRequest(buildConsoleRequest());
    } catch (error) {
        setConsoleConnection('Launch failed', true);
        appendConsoleActivity('error', error.message);
        setConsoleNote(error.message, true);
    } finally {
        launchButton.disabled = false;
    }
}

async function admitConsoleRequest(request, resetBeforeRender = false) {
    const response = await apiFetch('/api/v1/sessions', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(request),
    });
    const envelope = await response.json();
    if (!response.ok || !envelope.data?.session_id) {
        throw new Error(envelope.error?.message || `AgentD returned HTTP ${response.status}`);
    }
    if (resetBeforeRender) resetConsoleStream();
    consoleState.sessionId = envelope.data.session_id;
    consoleState.terminal = terminalStates.has(envelope.data.state);
    renderConsoleSession(envelope.data);
    connectConsoleStream();
    fetchSessions();
}

function buildConsoleRequest() {
    const agent = document.getElementById('console-agent').value;
    const restricted = document.getElementById('console-restricted').checked;
    const schemaText = document.getElementById('console-schema').value.trim();
    const workDir = document.getElementById('console-workdir').value.trim();
    const tracePolicy = document.getElementById('console-trace').value;
    const request = {
        idempotency_key: `dashboard:${crypto.randomUUID()}`,
        name: 'dashboard live console',
        agent,
        runtime: document.getElementById('console-runtime').value,
        model: document.getElementById('console-model').value,
        effort: document.getElementById('console-effort').value,
        fast: document.getElementById('console-fast').checked,
        prompt: document.getElementById('console-prompt').value.trim(),
        timeout: `${document.getElementById('console-timeout').value}s`,
        context: document.getElementById('console-clean').checked ? 'clean' : '',
        auto_discover: !document.getElementById('console-clean').checked,
    };
    if (workDir) request.work_dir = workDir;
    if (tracePolicy !== 'off') request.trace = {plugin: 'opentraces', policy: tracePolicy};
    if (agent === 'claude') {
        request.claude = {max_turns: Number(document.getElementById('console-max-turns').value)};
    } else {
        request.codex = {approval_mode: 'full-auto'};
    }
    if (schemaText) request.structured_output = {json_schema: JSON.parse(schemaText)};
    if (restricted) addRestrictedPolicy(request);
    return request;
}

function addRestrictedPolicy(request) {
    const hosts = document.getElementById('console-hosts').value
        .split(',').map((host) => host.trim()).filter(Boolean);
    request.execution_policy = {
        version: '2.1', workspace: 'ephemeral', workspace_retention: 'terminal_receipt',
        filesystem: 'read_only', network: 'public_https', allowed_tools: ['web_search'],
        egress_allowlist: hosts, egress_diagnostics: true, mcp_servers: [], host_mounts: [],
        approval_policy: 'never',
        resources: {memory_bytes: 2147483648, cpu_cores: 2, pids: 256, open_files: 1024},
    };
    if (!consoleState.authJSON) throw new Error('Choose a Codex auth.json for restricted egress.');
    request.env = {AGENTD_CODEX_AUTH_JSON: consoleState.authJSON};
    request.secret_grants = ['AGENTD_CODEX_AUTH_JSON'];
}

function resetConsoleStream() {
    closeConsoleStream();
    consoleState.sessionId = null;
    consoleState.sequence = 0;
    consoleState.eventCount = 0;
	consoleState.resumeID = null;
	consoleState.resumable = false;
    consoleState.runtime = null;
    consoleState.terminal = false;
    document.getElementById('console-output').textContent = '';
    document.getElementById('console-activity').replaceChildren();
    document.getElementById('console-event-count').textContent = '0 events';
    setConsoleControls(false);
}

function renderConsoleSession(session) {
	consoleState.runtime = session.runtime;
	consoleState.resumable = Boolean(session.resumable);
	consoleState.resumeID = session.provider_session_id || consoleState.resumeID;
    const shortID = session.session_id.slice(0, 8);
    document.getElementById('console-session-title').textContent = `${session.agent} · ${shortID}`;
    document.getElementById('console-session-strip').innerHTML = '';
    for (const text of [session.state, session.runtime, session.model || document.getElementById('console-model').value]) {
        const pill = document.createElement('span');
        pill.className = 'session-pill';
        pill.textContent = text;
        document.getElementById('console-session-strip').appendChild(pill);
    }
    setConsoleControls(!consoleState.terminal);
}

async function resumeConsoleSession(session) {
    closeConsoleStream();
    consoleState.sessionId = session.session_id;
    consoleState.sequence = session.last_sequence || 0;
    consoleState.eventCount = session.last_sequence || 0;
	consoleState.terminal = true;
	consoleState.resumeID = null;
	consoleState.resumable = Boolean(session.resumable);
    document.getElementById('console-agent').value = session.agent;
    updateModelControls();
    document.getElementById('console-runtime').value = session.runtime;
    renderConsoleSession(session);
    setConsoleControls(false, true);
    document.getElementById('console-output').textContent =
        `Continuing ${session.session_id.slice(0, 8)}. Enter a follow-up below to resume its provider conversation.`;
    document.getElementById('console-event-count').textContent = `${consoleState.eventCount} prior events`;
    document.getElementById('console-activity').replaceChildren();
    appendConsoleActivity('system', `Loading provider identity for ${session.session_id}`);
    setConsoleConnection('Loading history…');
    closeDetailPanel();
    switchTab('console');
	if (!session.resumable) {
		setConsoleConnection('Resume unavailable', true);
		appendConsoleActivity('error', 'This session does not have retained provider state.');
		return;
	}
	try {
		consoleState.resumeID = session.provider_session_id || await resolveConsoleResumeID(session.session_id);
        appendConsoleActivity('system', `Provider conversation · ${consoleState.resumeID}`);
        setConsoleConnection('Ready for follow-up');
        setConsoleControls(false);
        document.getElementById('console-steer').focus();
    } catch (error) {
        setConsoleConnection('Resume unavailable', true);
        appendConsoleActivity('error', error.message);
    }
}

async function resolveConsoleResumeID(sessionID) {
    const response = await apiFetch(`/api/v1/sessions/${sessionID}/events?after_sequence=0&limit=20`);
    if (!response.ok) throw new Error(`History replay returned HTTP ${response.status}`);
    const envelope = await response.json();
    for (const event of envelope.data?.events || []) {
        const resumeID = providerResumeIDFromEvent(event);
        if (resumeID) return resumeID;
    }
    throw new Error('This session has no recoverable provider conversation identity.');
}

function providerResumeIDFromEvent(event) {
    const threadID = event.payload?.result?.thread?.id || event.payload?.thread_id;
    if (threadID) return threadID;
    if (!event.raw_base64) return null;
    try {
        const raw = JSON.parse(atob(event.raw_base64));
        return raw.session_id || raw.params?.threadId || raw.result?.thread?.id || null;
    } catch (_) {
        return null;
    }
}

function connectConsoleStream() {
    closeConsoleStream();
    if (!consoleState.sessionId || consoleState.terminal) {
        finalizeConsoleSession();
        return;
    }
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const path = `/api/v1/ws/sessions/${consoleState.sessionId}/events?after_sequence=${consoleState.sequence}`;
    const ws = new WebSocket(`${protocol}//${window.location.host}${path}`, [
        'agentd.v1', `agentd.auth.${state.authToken}`,
    ]);
    ws.onopen = () => setConsoleConnection('Streaming', false, true);
    ws.onmessage = (message) => handleConsoleFrame(JSON.parse(message.data));
    ws.onerror = () => setConsoleConnection('Stream error', true);
    ws.onclose = () => {
        if (!consoleState.terminal && consoleState.sessionId) {
            setConsoleConnection('Reconnecting…');
            consoleState.reconnectTimer = setTimeout(connectConsoleStream, 750);
        }
    };
    consoleState.ws = ws;
}

function closeConsoleStream() {
    clearTimeout(consoleState.reconnectTimer);
    consoleState.reconnectTimer = null;
    if (consoleState.ws) {
        consoleState.ws.onclose = null;
        consoleState.ws.close();
    }
    consoleState.ws = null;
}

function handleConsoleFrame(event) {
    if (event.frame_type === 'stream.ready') {
        appendConsoleActivity('system', `Replay ready through ${event.replay_through}`);
        return;
    }
	if (event.frame_type === 'error') {
		appendConsoleActivity('error', event.error?.message || 'Stream failed');
		return;
	}
	if (event.frame_type === 'session.progress') {
		appendConsoleActivity('system', `${event.stage} · ${event.message}`);
		setConsoleConnection(event.message || event.stage);
		return;
	}
    if (event.sequence) {
        if (event.sequence !== consoleState.sequence + 1) {
            appendConsoleActivity('error', `Sequence gap after ${consoleState.sequence}; reconnecting.`);
            closeConsoleStream();
            connectConsoleStream();
            return;
        }
        consoleState.sequence = event.sequence;
    }
    consoleState.eventCount += 1;
    document.getElementById('console-event-count').textContent = `${consoleState.eventCount} events`;
    const payload = event.payload || {};
    consoleState.resumeID ||= providerResumeIDFromEvent(event);
    if (event.type === 'content.delta') {
        document.getElementById('console-output').textContent += payload.text || '';
        scrollConsoleOutput();
    } else if (event.type === 'output.final') {
        appendConsoleActivity('terminal', 'Structured final output committed');
    } else if (event.type === 'tool.call') {
        appendConsoleToolActivity('call', payload);
    } else if (event.type === 'tool.result') {
        appendConsoleToolActivity('result', payload);
    } else if (event.stream === 'terminal' && event.type.startsWith('session.')) {
        consoleState.terminal = true;
        appendConsoleActivity(event.type === 'session.completed' ? 'terminal' : 'error', event.type);
        closeConsoleStream();
        finalizeConsoleSession();
    } else if (event.type !== 'provider.event') {
        appendConsoleActivity('system', event.type || 'event');
    }
}

async function finalizeConsoleSession() {
    setConsoleControls(false);
    setConsoleConnection('Complete', false, true);
    if (!consoleState.sessionId) return;
    try {
        const sessionResponse = await apiFetch(`/api/v1/sessions/${consoleState.sessionId}`);
        const session = (await sessionResponse.json()).data;
        renderConsoleSession(session);
        const resultResponse = await apiFetch(`/api/v1/sessions/${consoleState.sessionId}/result`);
        if (resultResponse.ok) {
            const result = await resultResponse.json();
            document.getElementById('console-output').textContent = JSON.stringify(result, null, 2);
        }
        const receiptResponse = await apiFetch(`/api/v1/sessions/${consoleState.sessionId}/receipt`);
        if (receiptResponse.ok) {
            const receipt = (await receiptResponse.json()).data;
            appendConsoleActivity('terminal', `Receipt · ${receipt.state} · exit ${receipt.exit_code ?? '-'}`);
        }
        fetchSessions();
        fetchHistory();
    } catch (error) {
        appendConsoleActivity('error', `Finalize failed · ${error.message}`);
    }
}

async function steerConsoleSession(event) {
    event.preventDefault();
    const input = document.getElementById('console-steer');
    const text = input.value.trim();
    if (!text || !consoleState.sessionId) return;
    if (consoleState.terminal) {
        await followUpConsoleSession(text);
        return;
    }
    await postConsoleControl('input', {idempotency_key: `steer:${crypto.randomUUID()}`, kind: 'steer', text});
    appendConsoleActivity('system', `Steer sent · ${text}`);
    input.value = '';
}

async function followUpConsoleSession(text) {
	const previousSessionID = consoleState.sessionId;
	const providerResumeID = consoleState.resumeID;
	const wasResumable = consoleState.resumable;
	if (!wasResumable) {
		setConsoleNote('This session does not have retained provider state.', true);
		return;
    }
    setConsoleControls(false, true);
    setConsoleConnection('Admitting follow-up…');
    try {
        const request = buildConsoleRequest();
        request.idempotency_key = `dashboard-followup:${crypto.randomUUID()}`;
        request.name = 'dashboard console follow-up';
        request.prompt = text;
		request.resume_session = previousSessionID;
        await admitConsoleRequest(request, true);
        document.getElementById('console-steer').value = '';
    } catch (error) {
		consoleState.sessionId = previousSessionID;
		consoleState.resumeID = providerResumeID;
		consoleState.resumable = wasResumable;
        consoleState.terminal = true;
        setConsoleControls(false);
        setConsoleConnection('Follow-up failed', true);
        appendConsoleActivity('error', error.message);
        setConsoleNote(error.message, true);
    }
}

async function controlConsoleSession(action) {
    if (!consoleState.sessionId || consoleState.terminal) return;
    await postConsoleControl(action, {idempotency_key: `${action}:${crypto.randomUUID()}`});
    appendConsoleActivity('system', `${action} requested`);
}

async function postConsoleControl(action, body) {
    const suffix = consoleControlPaths[action];
    if (!suffix) throw new Error(`Unsupported control: ${action}`);
    const response = await apiFetch(`/api/v1/sessions/${consoleState.sessionId}${suffix}`, {
        method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body),
    });
    if (!response.ok) {
        const envelope = await response.json().catch(() => ({}));
        throw new Error(envelope.error?.message || `${action} returned HTTP ${response.status}`);
    }
}

function setConsoleControls(enabled, locked = false) {
	const hasSession = Boolean(consoleState.sessionId);
	const canCompose = hasSession && (enabled || consoleState.resumable);
    const composer = document.getElementById('console-steer');
    const submit = document.getElementById('console-steer-button');
    composer.disabled = !canCompose || locked;
    submit.disabled = !canCompose || locked;
	composer.placeholder = enabled ? 'Steer the active turn…' : consoleState.resumable ?
		'Continue this conversation…' : 'Provider state was not retained';
    submit.textContent = enabled ? 'Send steer' : 'Send follow-up';
    document.getElementById('console-interrupt').disabled = !enabled;
    document.getElementById('console-cancel').disabled = !enabled;
}

function appendConsoleToolActivity(phase, payload) {
    const item = payload.item || {};
    const kind = item.type || payload.name || payload.tool_name || 'provider item';
    const method = payload.native_method || (phase === 'call' ? 'started' : 'completed');
    const status = item.status || payload.status || (phase === 'call' ? 'running' : 'complete');
    const duration = payload.duration_ms == null ? '' : ` · ${payload.duration_ms}ms`;
    const entry = document.createElement('details');
    entry.className = 'activity-entry tool tool-event-details';
    const summary = document.createElement('summary');
    summary.textContent = `${new Date().toLocaleTimeString()} · ${kind} · ${method} · ${status}${duration}`;
    const detail = document.createElement('pre');
    detail.textContent = JSON.stringify(payload, null, 2);
    entry.append(summary, detail);
    document.getElementById('console-activity').appendChild(entry);
}

function appendConsoleActivity(type, message) {
    const entry = document.createElement('div');
    entry.className = `activity-entry ${type}`;
    entry.textContent = `${new Date().toLocaleTimeString()} · ${message}`;
    document.getElementById('console-activity').appendChild(entry);
}

function setConsoleConnection(message, error = false, live = false) {
    const badge = document.getElementById('console-connection');
    badge.textContent = message;
    badge.className = `console-connection${error ? ' error' : live ? ' live' : ''}`;
}

function setConsoleNote(message, error = false) {
    const note = document.getElementById('console-form-note');
    note.textContent = message;
    note.style.color = error ? '#ff938c' : '';
}

function scrollConsoleOutput() {
    const output = document.getElementById('console-output');
    output.scrollTop = output.scrollHeight;
}
