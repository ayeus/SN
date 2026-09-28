// AyeusANN Frontend Application Controller — Production Implementation
document.addEventListener('DOMContentLoaded', () => {
  // Use relative API base or gateway host
  const API_BASE = window.location.origin;

  let currentPlatform = 'mac';
  let activeRegistrationToken = '';
  let activeAPIKey = localStorage.getItem('api_key') || '';
  let currentUser = JSON.parse(localStorage.getItem('user') || 'null');
  let currentOrg = JSON.parse(localStorage.getItem('org') || 'null');
  let authToken = localStorage.getItem('token') || '';

  // ─── Tab Navigation ──────────────────────────────────────────
  const navTabs = document.querySelectorAll('.nav-tab');
  const tabContents = document.querySelectorAll('.tab-content');

  navTabs.forEach(tab => {
    tab.addEventListener('click', () => {
      const target = tab.getAttribute('data-tab');
      navTabs.forEach(t => t.classList.remove('active'));
      tabContents.forEach(c => c.classList.remove('active'));
      tab.classList.add('active');
      const targetEl = document.getElementById(`tab-${target}`);
      if (targetEl) targetEl.classList.add('active');
    });
  });

  // ─── Auth State Management ────────────────────────────────────
  const authModal = document.getElementById('auth-modal');
  const btnLoginModal = document.getElementById('btn-login-modal');
  const modalCloseAuth = document.getElementById('modal-close-auth');
  const btnSubmitLogin = document.getElementById('btn-submit-login');
  const btnSubmitSignup = document.getElementById('btn-submit-signup');
  const authEmailInput = document.getElementById('auth-email');
  const authPasswordInput = document.getElementById('auth-password');

  function updateAuthUI() {
    if (authToken && currentUser) {
      btnLoginModal.textContent = `${currentUser.name || currentUser.email} (Sign Out)`;
      btnLoginModal.onclick = handleLogout;
      fetchHosts();
      fetchModels();
      if (currentOrg && currentOrg.id) {
        fetchBalance(currentOrg.id);
        fetchInvoices(currentOrg.id);
      }
    } else {
      btnLoginModal.textContent = 'Sign In';
      btnLoginModal.onclick = () => authModal.classList.add('active');
      fetchModels();
    }
  }

  modalCloseAuth.addEventListener('click', () => authModal.classList.remove('active'));

  async function handleLogin() {
    const email = authEmailInput.value.trim();
    const password = authPasswordInput.value;
    if (!email || !password) {
      alert('Please provide email and password');
      return;
    }

    try {
      const res = await fetch(`${API_BASE}/v1/auth/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, password })
      });

      const data = await res.json();
      if (!res.ok) {
        alert(`Login failed: ${data.error || res.statusText}`);
        return;
      }

      authToken = data.access_token;
      currentUser = data.user;
      currentOrg = data.organization;

      localStorage.setItem('token', authToken);
      localStorage.setItem('user', JSON.stringify(currentUser));
      localStorage.setItem('org', JSON.stringify(currentOrg));

      authModal.classList.remove('active');
      updateAuthUI();
      issueRegistrationToken();
    } catch (e) {
      alert(`Network error during login: ${e.message}`);
    }
  }

  async function handleSignup() {
    const email = authEmailInput.value.trim();
    const password = authPasswordInput.value;
    if (!email || !password) {
      alert('Please provide email and password (must include upper, lower, digit, symbol)');
      return;
    }

    try {
      const res = await fetch(`${API_BASE}/v1/auth/signup`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          email,
          password,
          name: email.split('@')[0],
          org_name: `${email.split('@')[0]}'s Org`
        })
      });

      const data = await res.json();
      if (!res.ok) {
        alert(`Signup failed: ${data.error || res.statusText}`);
        return;
      }

      authToken = data.access_token;
      currentUser = data.user;
      currentOrg = data.organization;

      localStorage.setItem('token', authToken);
      localStorage.setItem('user', JSON.stringify(currentUser));
      localStorage.setItem('org', JSON.stringify(currentOrg));

      authModal.classList.remove('active');
      updateAuthUI();
      issueRegistrationToken();
    } catch (e) {
      alert(`Network error during signup: ${e.message}`);
    }
  }

  async function handleLogout() {
    if (authToken) {
      try {
        await fetch(`${API_BASE}/v1/auth/logout`, {
          method: 'POST',
          headers: {
            'Authorization': `Bearer ${authToken}`,
            'Content-Type': 'application/json'
          }
        });
      } catch (e) {}
    }

    authToken = '';
    currentUser = null;
    currentOrg = null;
    activeRegistrationToken = '';
    localStorage.removeItem('token');
    localStorage.removeItem('user');
    localStorage.removeItem('org');
    updateAuthUI();
  }

  btnSubmitLogin.addEventListener('click', handleLogin);
  btnSubmitSignup.addEventListener('click', handleSignup);

  // ─── Installer Command Generator ─────────────────────────────
  const cmdText = document.getElementById('installer-cmd-text');
  const cmdPlatformLabel = document.getElementById('cmd-platform-label');
  const tabCmdMac = document.getElementById('tab-cmd-mac');
  const tabCmdWin = document.getElementById('tab-cmd-win');
  const btnGenerateInstaller = document.getElementById('btn-generate-installer');
  const btnCopyInstaller = document.getElementById('btn-copy-installer');

  function updateInstallerCommand() {
    const origin = API_BASE;
    const hostIp = window.location.hostname || '127.0.0.1';
    const coordUrl = `http://${hostIp}:50051`;
    const tokenArg = activeRegistrationToken ? `--token ${activeRegistrationToken}` : '--token <REGISTRATION_TOKEN>';

    if (currentPlatform === 'mac') {
      cmdPlatformLabel.textContent = 'bash (macOS / Linux)';
      cmdText.textContent = `curl -fsSL ${origin}/install.sh | sh -s -- ${tokenArg} --coordinator ${coordUrl}`;
    } else {
      cmdPlatformLabel.textContent = 'powershell (Windows)';
      cmdText.textContent = `$env:SN_REGISTRATION_TOKEN="${activeRegistrationToken || '<REGISTRATION_TOKEN>'}"; $env:SN_COORDINATOR_URL="${coordUrl}"; Invoke-RestMethod ${origin}/install.ps1 | Invoke-Expression`;
    }
  }

  tabCmdMac.addEventListener('click', () => {
    currentPlatform = 'mac';
    tabCmdMac.classList.remove('btn-secondary');
    tabCmdMac.classList.add('btn');
    tabCmdWin.classList.remove('btn');
    tabCmdWin.classList.add('btn-secondary');
    updateInstallerCommand();
  });

  tabCmdWin.addEventListener('click', () => {
    currentPlatform = 'win';
    tabCmdWin.classList.remove('btn-secondary');
    tabCmdWin.classList.add('btn');
    tabCmdMac.classList.remove('btn');
    tabCmdMac.classList.add('btn-secondary');
    updateInstallerCommand();
  });

  btnCopyInstaller.addEventListener('click', () => {
    navigator.clipboard.writeText(cmdText.textContent);
    btnCopyInstaller.textContent = 'Copied!';
    setTimeout(() => { btnCopyInstaller.textContent = 'Copy Command'; }, 2000);
  });

  async function issueRegistrationToken() {
    if (!authToken) {
      authModal.classList.add('active');
      return;
    }

    try {
      const res = await fetch(`${API_BASE}/v1/hosts/register-token`, {
        method: 'POST',
        headers: {
          'Authorization': `Bearer ${authToken}`
        }
      });
      if (res.ok) {
        const data = await res.json();
        activeRegistrationToken = data.registration_token;
        updateInstallerCommand();
      } else {
        const err = await res.json().catch(() => ({}));
        alert(`Failed to issue registration token: ${err.error || res.statusText}`);
      }
    } catch (e) {
      alert(`Network error issuing token: ${e.message}`);
    }
  }

  btnGenerateInstaller.addEventListener('click', async () => {
    await issueRegistrationToken();
    const card = document.getElementById('installer-card');
    if (card) card.scrollIntoView({ behavior: 'smooth' });
  });

  // ─── Fetch Hosts List ─────────────────────────────────────────
  async function fetchHosts() {
    try {
      const headers = {};
      if (authToken) headers['Authorization'] = `Bearer ${authToken}`;

      const res = await fetch(`${API_BASE}/v1/hosts`, { headers });
      if (res.ok) {
        const data = await res.json();
        renderHosts(data.hosts || []);
      }
    } catch (e) {
      console.warn('Failed to fetch hosts:', e);
    }
  }

  function renderHosts(hosts) {
    const tbody = document.getElementById('hosts-table-body');
    const countEl = document.getElementById('host-count');
    if (countEl) countEl.textContent = hosts.length;

    if (!hosts || hosts.length === 0) {
      tbody.innerHTML = `<tr><td colspan="8" style="text-align: center; color: var(--text-muted); padding: 2rem;">No host nodes registered yet. Run the installer script to onboard your GPU node.</td></tr>`;
      return;
    }

    tbody.innerHTML = hosts.map(h => `
      <tr>
        <td><strong>${escapeHtml(h.hostname || h.name)}</strong><br><small style="color: var(--text-muted)">UUID: ${h.id.substring(0, 8)}...</small></td>
        <td><span style="color: var(--accent-cyan); font-weight: bold;">${h.gpus && h.gpus.length ? escapeHtml(h.gpus[0].model) : 'Physical GPU'}</span></td>
        <td>${h.gpus && h.gpus.length ? h.gpus[0].vram_gb : '--'} GB</td>
        <td><span class="status-badge">${(h.tier || 't2').toUpperCase()}</span></td>
        <td>${escapeHtml(h.region || 'IN-SOUTH')}</td>
        <td>Reputation: ${h.reputation}/100</td>
        <td><span class="status-badge"><span class="pulse-dot"></span> ${escapeHtml(h.status)}</span></td>
        <td><code style="color: var(--accent-cyan);">${escapeHtml(h.overlay_ip || 'pending')}</code></td>
      </tr>
    `).join('');
  }

  document.getElementById('btn-refresh-hosts').addEventListener('click', fetchHosts);

  // ─── Fetch Models Catalog ─────────────────────────────────────
  async function fetchModels() {
    try {
      const headers = {};
      if (authToken) headers['Authorization'] = `Bearer ${authToken}`;

      const res = await fetch(`${API_BASE}/v1/models`, { headers });
      if (res.ok) {
        const data = await res.json();
        const models = data.models || [];
        populateModelDropdown(models);
      }
    } catch (e) {
      console.warn('Failed to fetch models:', e);
    }
  }

  function populateModelDropdown(models) {
    const select = document.getElementById('chat-model-select');
    if (!select || models.length === 0) return;

    select.innerHTML = models.map(m => `
      <option value="${escapeHtml(m.name)}">${escapeHtml(m.name)} (${m.params_b}B - ${m.min_vram_gb}GB VRAM)</option>
    `).join('');
  }

  // ─── AI Chat Studio (Real SSE Streaming / OpenAI Wire Format) ─
  const chatMessages = document.getElementById('chat-messages');
  const chatInput = document.getElementById('chat-input');
  const btnSendChat = document.getElementById('btn-send-chat');
  const chatStatLatency = document.getElementById('chat-stat-latency');
  const chatStatTokens = document.getElementById('chat-stat-tokens');
  const chatModelSelect = document.getElementById('chat-model-select');
  const chatApiKeyInput = document.getElementById('chat-api-key-input');
  const btnGenerateApiKey = document.getElementById('btn-generate-api-key');

  if (activeAPIKey) chatApiKeyInput.value = activeAPIKey;

  btnGenerateApiKey.addEventListener('click', async () => {
    if (!authToken) {
      alert('Please sign in first to generate an API key');
      authModal.classList.add('active');
      return;
    }

    try {
      const res = await fetch(`${API_BASE}/v1/api-keys`, {
        method: 'POST',
        headers: {
          'Authorization': `Bearer ${authToken}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({ name: 'Web Studio Key' })
      });

      const data = await res.json();
      if (!res.ok) {
        alert(`Failed to generate API key: ${data.error || res.statusText}`);
        return;
      }

      activeAPIKey = data.secret_key;
      localStorage.setItem('api_key', activeAPIKey);
      chatApiKeyInput.value = activeAPIKey;
      alert(`New API Key generated successfully! Prefix: ${data.prefix}`);
    } catch (e) {
      alert(`Error creating API key: ${e.message}`);
    }
  });

  async function sendChatMessage() {
    const text = chatInput.value.trim();
    if (!text) return;

    const apiKey = chatApiKeyInput.value.trim();
    if (!apiKey) {
      alert('Please provide or generate an API key to execute inference');
      return;
    }

    // Append User Message
    const userMsg = document.createElement('div');
    userMsg.className = 'chat-bubble user';
    userMsg.textContent = text;
    chatMessages.appendChild(userMsg);
    chatInput.value = '';
    chatMessages.scrollTop = chatMessages.scrollHeight;

    // Append Assistant Placeholder
    const assistantMsg = document.createElement('div');
    assistantMsg.className = 'chat-bubble assistant';
    assistantMsg.textContent = 'Routing to GPU cluster...';
    chatMessages.appendChild(assistantMsg);
    chatMessages.scrollTop = chatMessages.scrollHeight;

    const startTime = performance.now();
    let tokenCount = 0;

    try {
      const response = await fetch(`${API_BASE}/v1/chat/completions`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-API-Key': apiKey
        },
        body: JSON.stringify({
          model: chatModelSelect.value,
          messages: [{ role: 'user', content: text }],
          stream: true
        })
      });

      if (!response.ok) {
        const errData = await response.json().catch(() => ({}));
        assistantMsg.textContent = `[Inference Error ${response.status}]: ${errData.error || response.statusText || 'Inference engine unreachable'}`;
        assistantMsg.style.borderColor = 'rgba(255, 23, 68, 0.5)';
        assistantMsg.style.color = '#ff8a80';
        return;
      }

      assistantMsg.textContent = '';
      const reader = response.body.getReader();
      const decoder = new TextDecoder();

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        const chunk = decoder.decode(value);
        const lines = chunk.split('\n');

        for (const line of lines) {
          if (line.startsWith('data: ')) {
            const dataStr = line.replace('data: ', '').trim();
            if (dataStr === '[DONE]') break;
            try {
              const data = JSON.parse(dataStr);
              if (data.choices && data.choices[0].delta && data.choices[0].delta.content) {
                assistantMsg.textContent += data.choices[0].delta.content;
                tokenCount += 1;
                chatMessages.scrollTop = chatMessages.scrollHeight;
              }
            } catch (err) {}
          }
        }
      }

      const elapsed = Math.round(performance.now() - startTime);
      chatStatLatency.textContent = `${elapsed} ms`;
      chatStatTokens.textContent = `${tokenCount} tks`;
    } catch (e) {
      assistantMsg.textContent = `[Network Error]: Unable to reach inference cluster (${e.message}). Check coordinator & router status.`;
      assistantMsg.style.borderColor = 'rgba(255, 23, 68, 0.5)';
      assistantMsg.style.color = '#ff8a80';
    }
  }

  btnSendChat.addEventListener('click', sendChatMessage);
  chatInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') sendChatMessage();
  });

  // ─── Wallet & Billing Data ────────────────────────────────────
  async function fetchBalance(orgId) {
    if (!authToken || !orgId) return;
    try {
      const res = await fetch(`${API_BASE}/v1/balance/${orgId}`, {
        headers: { 'Authorization': `Bearer ${authToken}` }
      });
      if (res.ok) {
        const data = await res.json();
        const balEl = document.getElementById('wallet-balance-val');
        if (balEl && data.balance) {
          balEl.textContent = `${data.balance.currency === 'INR' ? '₹' : '$'} ${data.balance.amount || '0.00'}`;
        }
      }
    } catch (e) {
      console.warn('Failed to fetch balance:', e);
    }
  }

  async function fetchInvoices(orgId) {
    if (!authToken || !orgId) return;
    try {
      const res = await fetch(`${API_BASE}/v1/invoices/${orgId}`, {
        headers: { 'Authorization': `Bearer ${authToken}` }
      });
      if (res.ok) {
        const data = await res.json();
        renderInvoices(data.invoices || []);
      }
    } catch (e) {
      console.warn('Failed to fetch invoices:', e);
    }
  }

  function renderInvoices(invoices) {
    const tbody = document.getElementById('invoices-table-body');
    if (!tbody) return;

    if (!invoices || invoices.length === 0) {
      tbody.innerHTML = `<tr><td colspan="6" style="text-align: center; color: var(--text-muted); padding: 2rem;">No invoices generated yet.</td></tr>`;
      return;
    }

    tbody.innerHTML = invoices.map(inv => `
      <tr>
        <td><strong style="font-family: var(--font-mono);">${escapeHtml(inv.invoice_number)}</strong></td>
        <td>${escapeHtml(inv.billing_period_start || '')} to ${escapeHtml(inv.billing_period_end || '')}</td>
        <td>${inv.currency === 'INR' ? '₹' : '$'} ${inv.subtotal}</td>
        <td>${inv.currency === 'INR' ? '₹' : '$'} ${inv.tax_amount}</td>
        <td><strong>${inv.currency === 'INR' ? '₹' : '$'} ${inv.total}</strong></td>
        <td><span class="status-badge" style="background: rgba(0, 230, 118, 0.1); color: var(--accent-green);">${escapeHtml(inv.status)}</span></td>
      </tr>
    `).join('');
  }

  function escapeHtml(str) {
    if (!str) return '';
    return String(str).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  // Initial load
  updateAuthUI();
  updateInstallerCommand();
});
