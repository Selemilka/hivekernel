// ─── HiveKernel Cron Page ───

let treeNodes = [];  // cached process tree for name resolution

// ─── Load Data ───

async function loadCronEntries() {
    try {
        // Fetch tree and cron in parallel.
        const [treeResp, cronResp] = await Promise.all([
            fetch('/api/tree'),
            fetch('/api/cron'),
        ]);
        const treeData = await treeResp.json();
        const cronData = await cronResp.json();

        if (treeData.ok) {
            treeNodes = treeData.nodes || [];
        }

        const tbody = document.getElementById('cron-body');
        const countEl = document.getElementById('cron-count');

        if (!cronData.ok) {
            tbody.innerHTML = `<tr><td colspan="11" class="cron-empty">Error: ${cronData.error}</td></tr>`;
            return;
        }

        const entries = cronData.entries || [];
        countEl.textContent = entries.length + ' job' + (entries.length !== 1 ? 's' : '');

        if (entries.length === 0) {
            tbody.innerHTML = '<tr><td colspan="11" class="cron-empty">No cron jobs configured. Click "+ Add Cron Job" to create one.</td></tr>';
            return;
        }

        tbody.innerHTML = '';
        entries.forEach(entry => {
            const tr = document.createElement('tr');
            const targetName = getProcessName(entry.target_pid);
            const lastRun = entry.last_run_ms ? timeAgo(entry.last_run_ms) : '-';
            const nextRun = entry.next_run_ms ? timeUntil(entry.next_run_ms) : '-';
            const statusBadge = entry.enabled
                ? '<span class="badge badge-ok">Active</span>'
                : '<span class="badge badge-off">Disabled</span>';
            const desc = entry.execute_description
                ? escapeHtml(entry.execute_description.length > 50
                    ? entry.execute_description.slice(0, 50) + '...'
                    : entry.execute_description)
                : '-';

            const exitCode = entry.last_run_ms ? String(entry.last_exit_code) : '-';
            const exitClass = entry.last_exit_code === 0 ? 'badge-ok' : (entry.last_run_ms ? 'badge-off' : '');
            const durationStr = entry.last_duration_ms ? (entry.last_duration_ms / 1000).toFixed(1) + 's' : '-';

            tr.innerHTML =
                `<td class="cron-cell-name">${escapeHtml(entry.name)}</td>`
                + `<td><code>${escapeHtml(entry.cron_expression)}</code><div class="cron-human">${describeCron(entry.cron_expression)}</div></td>`
                + `<td>${escapeHtml(entry.action)}</td>`
                + `<td>${escapeHtml(targetName)} <span style="color:#666">(PID ${entry.target_pid})</span></td>`
                + `<td title="${escapeHtml(entry.execute_description || '')}">${desc}</td>`
                + `<td>${statusBadge}</td>`
                + `<td>${lastRun}</td>`
                + `<td>${nextRun}</td>`
                + `<td>${exitClass ? `<span class="badge ${exitClass}">${exitCode}</span>` : exitCode}</td>`
                + `<td>${durationStr}</td>`
                + `<td><button class="btn-danger" style="padding:3px 10px;font-size:11px" onclick="removeCron('${entry.id}')">Delete</button></td>`;
            tbody.appendChild(tr);
        });
    } catch (e) {
        document.getElementById('cron-body').innerHTML =
            `<tr><td colspan="11" class="cron-empty">Failed to load: ${e.message}</td></tr>`;
    }
}

// ─── Add Cron ───

function openAddCronModal() {
    document.getElementById('cron-name').value = '';
    document.getElementById('cron-expression').value = '';
    document.getElementById('cron-desc').value = '';
    document.getElementById('cron-explain').textContent = '';

    // Populate target PID dropdown from tree.
    const select = document.getElementById('cron-target');
    select.innerHTML = '';
    treeNodes.forEach(node => {
        if (node.pid === 1) return; // skip kernel
        const opt = document.createElement('option');
        opt.value = node.pid;
        opt.textContent = `${node.name} (PID ${node.pid})`;
        select.appendChild(opt);
    });

    document.getElementById('add-cron-modal').style.display = 'flex';
}

async function doAddCron() {
    const name = document.getElementById('cron-name').value.trim();
    const expression = document.getElementById('cron-expression').value.trim();
    const action = document.getElementById('cron-action').value;
    const targetPid = parseInt(document.getElementById('cron-target').value) || 0;
    const description = document.getElementById('cron-desc').value.trim();

    if (!name) { toast('error', 'Name is required'); return; }
    if (!expression) { toast('error', 'Cron expression is required'); return; }

    try {
        const resp = await fetch('/api/cron', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                name: name,
                cron_expression: expression,
                action: action,
                target_pid: targetPid,
                description: description,
            }),
        });
        const data = await resp.json();
        if (data.ok) {
            toast('success', `Cron job added: ${name}`);
            closeModal('add-cron-modal');
            loadCronEntries();
        } else {
            toast('error', 'Failed: ' + data.error);
        }
    } catch (e) {
        toast('error', 'Error: ' + e.message);
    }
}

// ─── Remove Cron ───

async function removeCron(id) {
    if (!confirm('Delete this cron job?')) return;
    try {
        const resp = await fetch(`/api/cron/${id}`, { method: 'DELETE' });
        const data = await resp.json();
        if (data.ok) {
            toast('success', 'Cron job deleted');
            loadCronEntries();
        } else {
            toast('error', 'Delete failed: ' + data.error);
        }
    } catch (e) {
        toast('error', 'Error: ' + e.message);
    }
}

// ─── Helpers ───

function getProcessName(pid) {
    const node = treeNodes.find(n => n.pid === pid);
    return node ? node.name : 'PID ' + pid;
}

function timeAgo(timestampMs) {
    const diff = Date.now() - timestampMs;
    if (diff < 0) return 'just now';
    if (diff < 60000) return Math.floor(diff / 1000) + 's ago';
    if (diff < 3600000) return Math.floor(diff / 60000) + 'm ago';
    if (diff < 86400000) return Math.floor(diff / 3600000) + 'h ago';
    return Math.floor(diff / 86400000) + 'd ago';
}

function timeUntil(timestampMs) {
    const diff = timestampMs - Date.now();
    if (diff < 0) return 'overdue';
    if (diff < 60000) return 'in ' + Math.floor(diff / 1000) + 's';
    if (diff < 3600000) return 'in ' + Math.floor(diff / 60000) + 'm';
    if (diff < 86400000) return 'in ' + Math.floor(diff / 3600000) + 'h';
    return 'in ' + Math.floor(diff / 86400000) + 'd';
}

function describeCron(expr) {
    const parts = expr.split(/\s+/);
    if (parts.length !== 5) return '';

    const [min, hour, dom, mon, dow] = parts;

    // Common patterns.
    if (min === '*' && hour === '*' && dom === '*' && mon === '*' && dow === '*') {
        return 'Every minute';
    }
    if (min.startsWith('*/')) {
        const n = parseInt(min.slice(2));
        if (hour === '*' && dom === '*' && mon === '*' && dow === '*') {
            return `Every ${n} minutes`;
        }
    }
    if (hour.startsWith('*/')) {
        const n = parseInt(hour.slice(2));
        if (dom === '*' && mon === '*' && dow === '*') {
            return `Every ${n} hours at :${min.padStart(2, '0')}`;
        }
    }
    if (dom === '*' && mon === '*' && dow === '*') {
        if (min !== '*' && hour !== '*') {
            return `Daily at ${hour.padStart(2, '0')}:${min.padStart(2, '0')}`;
        }
    }

    return '';
}

function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

function closeModal(id) {
    document.getElementById(id).style.display = 'none';
}

// Close modal on backdrop click.
document.querySelectorAll('.modal').forEach(modal => {
    modal.addEventListener('click', (e) => {
        if (e.target === modal) modal.style.display = 'none';
    });
});

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

// Live-explain cron expression as user types.
document.getElementById('cron-expression').addEventListener('input', (e) => {
    document.getElementById('cron-explain').textContent = describeCron(e.target.value.trim());
});

// ─── Init ───

loadCronEntries();
