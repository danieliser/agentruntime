function textFromConversationContent(content) {
	if (typeof content === 'string') return content;
	if (!Array.isArray(content)) return '';
	return content.map((part) => {
		if (typeof part === 'string') return part;
		if (!part || typeof part !== 'object') return '';
		return typeof part.text === 'string' ? part.text : '';
	}).join('');
}

function userTextFromConversationEvent(event) {
	const payload = event.payload || {};
	const item = payload.item || {};
	if (item.type === 'userMessage') return textFromConversationContent(item.content);
	const message = payload.message || {};
	if (message.role === 'user') return textFromConversationContent(message.content);
	return '';
}

function completedAssistantTextFromConversationEvent(event) {
	const payload = event.payload || {};
	if (event.type === 'content.completed' && typeof payload.text === 'string') return payload.text;
	const item = payload.item || {};
	if (event.type === 'tool.result' && item.type === 'agentMessage' && typeof item.text === 'string') {
		return item.text;
	}
	return '';
}

function conversationMessagesFromEvents(events) {
	const messages = [];
	let assistantText = '';
	const flushAssistant = () => {
		if (!assistantText) return;
		messages.push({role: 'assistant', text: assistantText});
		assistantText = '';
	};
	for (const event of events) {
		const userText = userTextFromConversationEvent(event);
		if (userText) {
			flushAssistant();
			const previous = messages[messages.length - 1];
			if (!previous || previous.role !== 'user' || previous.text !== userText) {
				messages.push({role: 'user', text: userText});
			}
			continue;
		}
		if (event.type === 'content.delta' && typeof event.payload?.text === 'string') {
			assistantText += event.payload.text;
			continue;
		}
		const completedText = completedAssistantTextFromConversationEvent(event);
		if (completedText && !assistantText) assistantText = completedText;
		if (event.type === 'turn.completed' || event.type === 'turn.failed' || event.stream === 'terminal') {
			flushAssistant();
		}
	}
	flushAssistant();
	return messages;
}

async function fetchSessionConversationEvents(sessionID, eventBudget) {
	const events = [];
	let cursor = 0;
	for (;;) {
		const response = await apiFetch(`/api/v1/sessions/${sessionID}/events?after_sequence=${cursor}&limit=1000`);
		if (!response.ok) throw new Error(`History replay returned HTTP ${response.status}`);
		const envelope = await response.json();
		const page = envelope.data || {};
		const pageEvents = page.events || [];
		if (events.length + pageEvents.length > eventBudget) {
			throw new Error('Conversation history exceeds the dashboard replay limit.');
		}
		events.push(...pageEvents);
		if (!page.has_more) return {events, lastSequence: page.last_sequence || cursor};
		const nextCursor = pageEvents[pageEvents.length - 1]?.sequence || cursor;
		if (nextCursor <= cursor) throw new Error('Conversation history pagination did not advance.');
		cursor = nextCursor;
	}
}

async function fetchConversationTranscript(sessionID) {
	const lineage = [];
	const visited = new Set();
	let currentID = sessionID;
	for (let depth = 0; currentID && depth < 64; depth += 1) {
		if (visited.has(currentID)) throw new Error('Conversation history contains a continuation cycle.');
		visited.add(currentID);
		const response = await apiFetch(`/api/v1/sessions/${currentID}`);
		if (!response.ok) throw new Error(`Session history returned HTTP ${response.status}`);
		const info = (await response.json()).data;
		lineage.unshift(info);
		currentID = info.resume_source_session_id || '';
	}
	if (currentID) throw new Error('Conversation history exceeds 64 continuation sessions.');

	const messages = [];
	let eventCount = 0;
	let lastSequence = 0;
	const maxEvents = 100000;
	for (const info of lineage) {
		const history = await fetchSessionConversationEvents(info.session_id, maxEvents - eventCount);
		eventCount += history.events.length;
		messages.push(...conversationMessagesFromEvents(history.events));
		if (info.session_id === sessionID) lastSequence = history.lastSequence;
	}
	return {messages, eventCount, lastSequence};
}

function appendConversationMessageTo(target, role, text) {
	if (target.children.length === 1 && target.firstElementChild?.classList.contains('conversation-empty')) {
		target.replaceChildren();
	}
	const message = document.createElement('section');
	message.className = `conversation-message ${role}`;
	message.dataset.role = role;
	const label = document.createElement('strong');
	label.className = 'conversation-role';
	label.textContent = role === 'user' ? 'You' : 'Agent';
	const body = document.createElement('div');
	body.className = 'conversation-text';
	body.textContent = text;
	message.append(label, body);
	target.appendChild(message);
	return body;
}

function appendConversationMessage(role, text) {
	return appendConversationMessageTo(document.getElementById('console-output'), role, text);
}

function appendConversationDeltaTo(target, text) {
	if (!text) return;
	const lastMessage = target.lastElementChild;
	const body = lastMessage?.dataset.role === 'assistant' ? lastMessage.querySelector('.conversation-text') :
		appendConversationMessageTo(target, 'assistant', '');
	body.textContent += text;
	target.scrollTop = target.scrollHeight;
}

function renderConversationTranscript(target, messages, emptyText = 'No conversation messages were retained.') {
	target.replaceChildren();
	if (messages.length === 0) {
		const empty = document.createElement('div');
		empty.className = 'conversation-empty';
		empty.textContent = emptyText;
		target.appendChild(empty);
		return;
	}
	for (const message of messages) appendConversationMessageTo(target, message.role, message.text);
	target.scrollTop = target.scrollHeight;
}
