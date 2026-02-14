// ─── HiveKernel Chat ───

// Chat history (persisted in localStorage).
let chatHistory = [];
const HISTORY_KEY = 'hivekernel-chat-history';

// ─── Init ───

function init() {
    loadHistory();
    renderHistory();
    setupInput();
    loadSidebar();
    checkConnection();

    // Refresh sidebar every 15s.
    setInterval(loadSidebar, 15000);
}

// ─── Connection check ───

async function checkConnection() {
    try {
        const resp = await fetch('/api/tree');
        const data = await resp.json();
        if (data.ok) {
            setStatus(true);
        } else {
            setStatus(false, data.error);
        }
    } catch (e) {
        setStatus(false);
    }
}

function setStatus(connected, detail) {
    const el = document.getElementById('status-indicator');
    if (connected) {
        el.textContent = 'Connected';
        el.className = 'status connected';
    } else {
        el.textContent = detail ? 'Error' : 'Disconnected';
        el.className = 'status disconnected';
    }
}

// ─── History ───

function loadHistory() {
    try {
        const saved = localStorage.getItem(HISTORY_KEY);
        if (saved) {
            chatHistory = JSON.parse(saved);
        }
    } catch (e) {
        chatHistory = [];
    }
}

function saveHistory() {
    // Keep last 100 messages.
    if (chatHistory.length > 100) {
        chatHistory = chatHistory.slice(-100);
    }
    localStorage.setItem(HISTORY_KEY, JSON.stringify(chatHistory));
}

function renderHistory() {
    const container = document.getElementById('chat-messages');
    // Keep the system welcome bubble.
    const welcome = container.firstElementChild;
    container.innerHTML = '';
    if (chatHistory.length === 0 && welcome) {
        container.appendChild(welcome);
    }

    for (const msg of chatHistory) {
        appendBubble(msg.role, msg.content, msg.time, false);
    }
    scrollToBottom();
}

// ─── Chat UI ───

function appendBubble(role, content, time, scroll = true) {
    const container = document.getElementById('chat-messages');
    const div = document.createElement('div');
    div.className = `chat-bubble ${role}`;

    const text = document.createElement('div');
    text.textContent = content;
    div.appendChild(text);

    if (time) {
        const ts = document.createElement('div');
        ts.className = 'bubble-time';
        ts.textContent = new Date(time).toLocaleTimeString();
        div.appendChild(ts);
    }

    container.appendChild(div);
    if (scroll) scrollToBottom();
}

function scrollToBottom() {
    const container = document.getElementById('chat-messages');
    container.scrollTop = container.scrollHeight;
}

function setTyping(show) {
    document.getElementById('chat-typing').style.display = show ? 'block' : 'none';
}

// ─── Input ───

function setupInput() {
    const input = document.getElementById('chat-input');

    // Auto-resize textarea.
    input.addEventListener('input', () => {
        input.style.height = 'auto';
        input.style.height = Math.min(input.scrollHeight, 120) + 'px';
    });

    // Enter to send, Shift+Enter for newline.
    input.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            sendMessage();
        }
    });
}

// ─── Send Message ───

async function sendMessage() {
    const input = document.getElementById('chat-input');
    const message = input.value.trim();
    if (!message) return;

    // Clear input.
    input.value = '';
    input.style.height = 'auto';

    // Add user message.
    const now = Date.now();
    chatHistory.push({ role: 'user', content: message, time: now });
    appendBubble('user', message, now);
    saveHistory();

    // Send to API.
    setTyping(true);
    document.getElementById('chat-send').disabled = true;

    try {
        // Build LLM-compatible history (last 10 messages).
        const llmHistory = chatHistory.slice(-11, -1).map(m => ({
            role: m.role === 'assistant' ? 'assistant' : 'user',
            content: m.content,
        }));

        const resp = await fetch('/api/chat', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ message, history: llmHistory }),
        });
        const data = await resp.json();

        if (data.ok) {
            const respTime = Date.now();
            chatHistory.push({ role: 'assistant', content: data.response, time: respTime });
            appendBubble('assistant', data.response, respTime);
            saveHistory();
            // Refresh sidebar (cron might have been added).
            loadSidebar();
        } else {
            appendBubble('system', 'Error: ' + (data.error || 'Unknown error'), Date.now());
        }
    } catch (e) {
        appendBubble('system', 'Connection error: ' + e.message, Date.now());
    }

    setTyping(false);
    document.getElementById('chat-send').disabled = false;
    input.focus();
}

// ─── Sidebar ───

async function loadSidebar() {
    await loadAgents();
    await loadCron();
}

async function loadAgents() {
    const container = document.getElementById('sidebar-agents');
    try {
        const resp = await fetch('/api/tree');
        const data = await resp.json();
        if (!data.ok) {
            container.innerHTML = '<span class="empty">Not connected</span>';
            return;
        }

        const daemons = data.nodes.filter(n => n.role === 'daemon' || n.role === 'kernel');
        if (daemons.length === 0) {
            container.innerHTML = '<span class="empty">No agents</span>';
            return;
        }

        container.innerHTML = '';
        const roleColors = {
            kernel: '#e94560', daemon: '#ff9f43', agent: '#54a0ff',
            architect: '#a55eea', lead: '#5f27cd', worker: '#10ac84', task: '#576574',
        };
        const stateColors = {
            idle: '#576574', running: '#10ac84', blocked: '#ee5a24',
            sleeping: '#f9ca24', dead: '#636e72', zombie: '#d63031',
        };

        for (const n of daemons) {
            const div = document.createElement('div');
            div.className = 'agent-item';
            div.innerHTML =
                `<span class="agent-dot" style="background:${stateColors[n.state] || '#576574'}"></span>` +
                `<span class="agent-name">${escapeHtml(n.name)}</span>` +
                `<span class="agent-pid">PID ${n.pid}</span>`;
            container.appendChild(div);
        }

        setStatus(true);
    } catch (e) {
        container.innerHTML = '<span class="empty">Error loading</span>';
        setStatus(false);
    }
}

async function loadCron() {
    const container = document.getElementById('sidebar-cron');
    try {
        const resp = await fetch('/api/cron');
        const data = await resp.json();
        if (!data.ok || data.entries.length === 0) {
            container.innerHTML = '<span class="empty">No scheduled tasks</span>';
            return;
        }

        container.innerHTML = '';
        for (const e of data.entries) {
            const div = document.createElement('div');
            div.className = 'cron-item';
            div.innerHTML =
                `<div>` +
                `<span class="cron-name">${escapeHtml(e.name)}</span><br>` +
                `<span class="cron-expr">${escapeHtml(e.cron_expression)}</span>` +
                `</div>` +
                `<span class="cron-del" title="Remove" onclick="removeCron('${e.id}')">&times;</span>`;
            container.appendChild(div);
        }
    } catch (e) {
        // Silently ignore.
    }
}

async function removeCron(cronId) {
    try {
        await fetch(`/api/cron/${cronId}`, { method: 'DELETE' });
        loadCron();
    } catch (e) {
        toast('error', 'Failed to remove cron entry');
    }
}

// ─── Utilities ───

function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

function toast(type, message) {
    const container = document.getElementById('toast-container');
    const el = document.createElement('div');
    el.className = `toast ${type}`;
    el.textContent = message;
    container.appendChild(el);
    setTimeout(() => {
        el.style.opacity = '0';
        el.style.transition = 'opacity 0.3s';
        setTimeout(() => el.remove(), 300);
    }, 3000);
}

// ─── Start ───

init();
