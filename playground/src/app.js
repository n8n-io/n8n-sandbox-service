import { SandboxClient } from '@n8n/sandbox-client';

const $ = (sel) => document.querySelector(sel);
const baseUrlInput = $('#base-url');
const apiKeyInput = $('#api-key');
const createBtn = $('#create-btn');
const refreshBtn = $('#refresh-btn');
const sandboxList = $('#sandbox-list');
const output = $('#output');
const cmdInput = $('#cmd-input');
const runBtn = $('#run-btn');
const statusEl = $('#status');

let selectedSandboxId = null;
let commandHistory = [];
let historyIndex = -1;
let currentAbortController = null;
let client = null;

// --- SDK client ---

function getClient() {
  const baseUrl = baseUrlInput.value.trim().replace(/\/+$/, '');
  const apiKey = apiKeyInput.value.trim();
  if (!client || client._baseUrl !== baseUrl || client._apiKey !== apiKey) {
    client = new SandboxClient({ baseUrl, apiKey });
    client._baseUrl = baseUrl;
    client._apiKey = apiKey;
  }
  return client;
}

// --- Persistence ---

function loadSettings() {
  const url = localStorage.getItem('sandbox-base-url');
  const key = localStorage.getItem('sandbox-api-key');
  if (url) baseUrlInput.value = url;
  if (key) apiKeyInput.value = key;
}

function saveSettings() {
  localStorage.setItem('sandbox-base-url', baseUrlInput.value.trim());
  localStorage.setItem('sandbox-api-key', apiKeyInput.value.trim());
  client = null;
}

baseUrlInput.addEventListener('change', saveSettings);
apiKeyInput.addEventListener('change', saveSettings);

// --- Output ---

function appendOutput(text, cls) {
  const wasAtBottom = output.scrollTop + output.clientHeight >= output.scrollHeight - 20;
  const span = document.createElement('span');
  span.className = cls;
  span.textContent = text + '\n';
  const empty = output.querySelector('.empty-state');
  if (empty) empty.remove();
  output.appendChild(span);
  if (wasAtBottom) output.scrollTop = output.scrollHeight;
}

// --- Sandbox list ---

async function refreshSandboxes() {
  if (!apiKeyInput.value.trim()) {
    setStatus('Enter an API key first');
    return;
  }
  try {
    setStatus('Loading sandboxes...');
    const c = getClient();
    const baseUrl = baseUrlInput.value.trim().replace(/\/+$/, '');
    const res = await fetch(`${baseUrl}/sandboxes`, {
      headers: { 'X-Api-Key': apiKeyInput.value.trim() },
    });
    if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
    const sandboxes = await res.json();
    renderSandboxes(Array.isArray(sandboxes) ? sandboxes : []);
    setStatus(`${sandboxes.length} sandbox(es)`);
  } catch (e) {
    setStatus(`Error: ${e.message}`);
  }
}

function renderSandboxes(sandboxes) {
  sandboxList.innerHTML = '';
  sandboxes.forEach((sb) => {
    const li = document.createElement('li');
    li.dataset.id = sb.id;
    if (sb.id === selectedSandboxId) li.classList.add('selected');

    const nameSpan = document.createElement('span');
    nameSpan.className = 'sandbox-name';
    nameSpan.textContent = sb.id.slice(0, 12);
    nameSpan.title = sb.id;

    const stateSpan = document.createElement('span');
    const state = (sb.status || '').toLowerCase();
    stateSpan.className = 'sandbox-state';
    if (state === 'running' || state === 'ready') {
      stateSpan.classList.add('state-running');
    } else if (state === 'stopped' || state === 'exited') {
      stateSpan.classList.add('state-stopped');
    } else {
      stateSpan.classList.add('state-other');
    }
    stateSpan.textContent = sb.status || 'unknown';

    const deleteBtn = document.createElement('button');
    deleteBtn.className = 'sandbox-delete';
    deleteBtn.textContent = 'x';
    deleteBtn.title = 'Delete sandbox';
    deleteBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      deleteSandbox(sb.id);
    });

    li.appendChild(nameSpan);
    li.appendChild(stateSpan);
    li.appendChild(deleteBtn);

    li.addEventListener('click', () => selectSandbox(sb));
    sandboxList.appendChild(li);
  });
}

function selectSandbox(sb) {
  selectedSandboxId = sb.id;
  document.querySelectorAll('#sandbox-list li').forEach((li) => {
    li.classList.toggle('selected', li.dataset.id === sb.id);
  });
  cmdInput.disabled = false;
  runBtn.disabled = false;
  cmdInput.focus();
  setStatus(`Selected: ${sb.id.slice(0, 12)}`);
}

// --- Create sandbox ---

async function createSandbox() {
  if (!apiKeyInput.value.trim()) {
    setStatus('Enter an API key first');
    return;
  }

  createBtn.disabled = true;
  try {
    setStatus('Creating sandbox...');
    appendOutput('Creating sandbox...', 'info');
    const t0 = performance.now();
    const sb = await getClient().createSandbox();
    const elapsed = ((performance.now() - t0) / 1000).toFixed(2);
    appendOutput(`Sandbox created: ${sb.id} (${sb.status}) in ${elapsed}s`, 'info');
    setStatus(`Created: ${sb.id.slice(0, 12)} (${elapsed}s)`);
    await refreshSandboxes();
    selectSandbox(sb);
  } catch (e) {
    appendOutput(`Error creating sandbox: ${e.message}`, 'error');
    setStatus(`Create failed: ${e.message}`);
  } finally {
    createBtn.disabled = false;
  }
}

// --- Delete sandbox ---

async function deleteSandbox(id) {
  try {
    setStatus(`Deleting ${id.slice(0, 12)}...`);
    const t0 = performance.now();
    await getClient().deleteSandbox(id);
    const elapsed = ((performance.now() - t0) / 1000).toFixed(2);
    if (selectedSandboxId === id) {
      selectedSandboxId = null;
      cmdInput.disabled = true;
      runBtn.disabled = true;
    }
    appendOutput(`Sandbox deleted: ${id} (${elapsed}s)`, 'info');
    await refreshSandboxes();
  } catch (e) {
    appendOutput(`Error deleting sandbox: ${e.message}`, 'error');
    setStatus(`Delete failed: ${e.message}`);
  }
}

// --- Execute command (streaming NDJSON via native fetch for real-time output) ---

async function execInSandbox(sandboxId, command, timeoutMs = 300000, signal) {
  const baseUrl = baseUrlInput.value.trim().replace(/\/+$/, '');
  const res = await fetch(`${baseUrl}/sandboxes/${sandboxId}/exec`, {
    method: 'POST',
    headers: {
      'X-Api-Key': apiKeyInput.value.trim(),
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ command, timeout_ms: timeoutMs }),
    signal,
  });

  if (!res.ok) {
    const text = await res.text();
    let msg = `${res.status}`;
    try {
      const err = JSON.parse(text);
      if (err.error) msg = err.error;
    } catch { msg = text; }
    throw new Error(msg);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let exitCode = null;

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;

    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split('\n');
    buffer = lines.pop();

    for (const line of lines) {
      if (!line.trim()) continue;
      try {
        const event = JSON.parse(line);
        handleExecEvent(event);
        if (event.type === 'exit') exitCode = event.exit_code;
      } catch {
        appendOutput(line, 'stdout');
      }
    }
  }

  if (buffer.trim()) {
    try {
      const event = JSON.parse(buffer);
      handleExecEvent(event);
      if (event.type === 'exit') exitCode = event.exit_code;
    } catch {
      appendOutput(buffer, 'stdout');
    }
  }

  return exitCode;
}

async function executeCommand() {
  const cmd = cmdInput.value.trim();
  if (!cmd || !selectedSandboxId) return;

  commandHistory.push(cmd);
  historyIndex = commandHistory.length;
  cmdInput.value = '';
  cmdInput.disabled = true;
  runBtn.disabled = true;

  appendOutput(`$ ${cmd}`, 'cmd');

  currentAbortController = new AbortController();

  try {
    await execInSandbox(selectedSandboxId, cmd, 300000, currentAbortController.signal);
  } catch (e) {
    if (e.name !== 'AbortError') {
      appendOutput(`Error: ${e.message}`, 'error');
    }
  } finally {
    currentAbortController = null;
    cmdInput.disabled = false;
    runBtn.disabled = false;
    cmdInput.focus();
  }
}

function handleExecEvent(event) {
  switch (event.type) {
    case 'stdout':
      if (event.data) appendOutput(event.data.replace(/\n$/, ''), 'stdout');
      break;
    case 'stderr':
      if (event.data) appendOutput(event.data.replace(/\n$/, ''), 'stderr');
      break;
    case 'exit':
      if (event.exit_code === 0) {
        appendOutput(`[exit ${event.exit_code} in ${event.execution_time_ms}ms]`, 'exit-ok');
      } else {
        appendOutput(`[exit ${event.exit_code}${event.timed_out ? ' (timed out)' : ''}${event.killed ? ' (killed)' : ''} in ${event.execution_time_ms}ms]`, 'exit-fail');
      }
      break;
    case 'error':
      appendOutput(`Error: ${event.error}`, 'error');
      break;
  }
}

// --- Command history ---

cmdInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    e.preventDefault();
    executeCommand();
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    if (historyIndex > 0) {
      historyIndex--;
      cmdInput.value = commandHistory[historyIndex];
    }
  } else if (e.key === 'ArrowDown') {
    e.preventDefault();
    if (historyIndex < commandHistory.length - 1) {
      historyIndex++;
      cmdInput.value = commandHistory[historyIndex];
    } else {
      historyIndex = commandHistory.length;
      cmdInput.value = '';
    }
  } else if (e.key === 'c' && e.ctrlKey) {
    if (currentAbortController) {
      currentAbortController.abort();
      appendOutput('^C', 'error');
    }
  }
});

// --- File browser ---

const filePathInput = $('#file-path-input');
const fileGoBtn = $('#file-go-btn');
const fileRefreshBtn = $('#file-refresh-btn');
const fileListEl = $('#file-list');
const fileViewer = $('#file-viewer');
const fileViewerName = $('#file-viewer-name');
const fileContentEl = $('#file-content');
const fileEditBtn = $('#file-edit-btn');
const fileCloseBtn = $('#file-close-btn');
const fileEditor = $('#file-editor');
const fileEditorTextarea = $('#file-editor-textarea');
const fileSaveBtn = $('#file-save-btn');
const fileCancelBtn = $('#file-cancel-btn');
const newFilePathInput = $('#new-file-path');
const newFileBtn = $('#new-file-btn');

let currentViewingFile = null;

function normalizePath(p) {
  return p.replace(/\/+/g, '/').replace(/\/$/, '') || '/';
}

async function browseFiles(dir) {
  if (!selectedSandboxId) return;
  dir = normalizePath(dir || '/');
  filePathInput.value = dir;

  try {
    const files = await getClient().listFiles(selectedSandboxId, { path: dir });
    renderFileList(dir, files);
  } catch (e) {
    fileListEl.innerHTML = `<li style="color:#f44747; padding:8px 12px; font-size:12px;">Error: ${e.message}</li>`;
  }
}

function renderFileList(dir, files) {
  fileListEl.innerHTML = '';

  if (dir !== '/') {
    const parentLi = document.createElement('li');
    parentLi.innerHTML = '<span class="file-icon">..</span><span class="file-entry-name">..</span>';
    parentLi.addEventListener('click', () => {
      const parent = dir.split('/').slice(0, -1).join('/') || '/';
      browseFiles(parent);
    });
    fileListEl.appendChild(parentLi);
  }

  if (!files || files.length === 0) {
    const emptyLi = document.createElement('li');
    emptyLi.style.color = '#666';
    emptyLi.textContent = '(empty)';
    fileListEl.appendChild(emptyLi);
    return;
  }

  files.sort((a, b) => {
    if (a.isDir && !b.isDir) return -1;
    if (!a.isDir && b.isDir) return 1;
    return a.name.localeCompare(b.name);
  });

  for (const f of files) {
    const li = document.createElement('li');

    const icon = document.createElement('span');
    icon.className = 'file-icon';
    icon.textContent = f.isDir ? '\u{1F4C1}' : '\u{1F4C4}';

    const name = document.createElement('span');
    name.className = 'file-entry-name';
    name.textContent = f.name;

    li.appendChild(icon);
    li.appendChild(name);

    if (!f.isDir) {
      const size = document.createElement('span');
      size.className = 'file-entry-size';
      size.textContent = formatSize(f.size);
      li.appendChild(size);
    }

    const fullPath = normalizePath(dir + '/' + f.name);

    if (f.isDir) {
      li.addEventListener('click', () => browseFiles(fullPath));
    } else {
      li.addEventListener('click', () => viewFile(fullPath));
    }

    fileListEl.appendChild(li);
  }
}

function formatSize(bytes) {
  if (bytes == null) return '';
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

async function viewFile(filePath) {
  if (!selectedSandboxId) return;
  currentViewingFile = filePath;
  fileViewerName.textContent = filePath;
  fileViewerName.title = filePath;
  fileContentEl.textContent = 'Loading...';
  fileEditor.classList.remove('active');
  fileViewer.classList.add('active');

  try {
    const buf = await getClient().readFile(selectedSandboxId, filePath);
    fileContentEl.textContent = buf.toString('utf-8');
  } catch (e) {
    fileContentEl.textContent = `Error loading file: ${e.message}`;
  }
}

function startEditing() {
  if (!currentViewingFile) return;
  fileEditorTextarea.value = fileContentEl.textContent;
  fileEditor.classList.add('active');
  fileContentEl.style.display = 'none';
  fileEditorTextarea.focus();
}

function cancelEditing() {
  fileEditor.classList.remove('active');
  fileContentEl.style.display = '';
}

async function saveFile() {
  if (!selectedSandboxId || !currentViewingFile) return;

  const content = fileEditorTextarea.value;
  fileSaveBtn.disabled = true;

  try {
    await getClient().writeFile(selectedSandboxId, currentViewingFile, content);
    fileContentEl.textContent = content;
    cancelEditing();
    setStatus(`Saved: ${currentViewingFile}`);
  } catch (e) {
    setStatus(`Save failed: ${e.message}`);
  } finally {
    fileSaveBtn.disabled = false;
  }
}

async function createNewFile() {
  if (!selectedSandboxId) return;
  const filePath = newFilePathInput.value.trim();
  if (!filePath) return;

  const fullPath = filePath.startsWith('/') ? filePath : normalizePath(filePathInput.value + '/' + filePath);

  try {
    await getClient().writeFile(selectedSandboxId, fullPath, '');
    newFilePathInput.value = '';
    setStatus(`Created: ${fullPath}`);
    browseFiles(filePathInput.value);
    viewFile(fullPath);
  } catch (e) {
    setStatus(`Create failed: ${e.message}`);
  }
}

function closeFileViewer() {
  fileViewer.classList.remove('active');
  fileEditor.classList.remove('active');
  fileContentEl.style.display = '';
  currentViewingFile = null;
}

fileGoBtn.addEventListener('click', () => browseFiles(filePathInput.value));
fileRefreshBtn.addEventListener('click', () => browseFiles(filePathInput.value));
filePathInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') browseFiles(filePathInput.value);
});
fileEditBtn.addEventListener('click', startEditing);
fileCloseBtn.addEventListener('click', closeFileViewer);
fileSaveBtn.addEventListener('click', saveFile);
fileCancelBtn.addEventListener('click', cancelEditing);
newFileBtn.addEventListener('click', createNewFile);
newFilePathInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') createNewFile();
});

// Auto-browse files when selecting a sandbox
const _origSelectSandbox = selectSandbox;
selectSandbox = function(sb) {
  _origSelectSandbox(sb);
  browseFiles('/');
};

// --- Status ---

function setStatus(msg) {
  statusEl.textContent = msg;
}

// --- Event listeners ---

createBtn.addEventListener('click', createSandbox);
refreshBtn.addEventListener('click', refreshSandboxes);
runBtn.addEventListener('click', executeCommand);

// --- File panel resize ---

(function () {
  const resizer = document.getElementById('file-panel-resizer');
  const panel = document.getElementById('file-panel');
  const MIN_WIDTH = 200;
  const MAX_WIDTH = 800;

  let startX, startWidth;

  resizer.addEventListener('mousedown', (e) => {
    e.preventDefault();
    startX = e.clientX;
    startWidth = panel.offsetWidth;
    resizer.classList.add('dragging');
    document.addEventListener('mousemove', onMouseMove);
    document.addEventListener('mouseup', onMouseUp);
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
  });

  function onMouseMove(e) {
    const delta = startX - e.clientX;
    const newWidth = Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, startWidth + delta));
    panel.style.width = newWidth + 'px';
  }

  function onMouseUp() {
    resizer.classList.remove('dragging');
    document.removeEventListener('mousemove', onMouseMove);
    document.removeEventListener('mouseup', onMouseUp);
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
  }
})();

// --- Init ---

loadSettings();
