import React, { useState, useEffect, useRef } from 'react';
import {
  Cpu,
  Activity,
  Terminal,
  ShieldCheck,
  Zap,
  CheckCircle2,
  Server,
  HelpCircle,
  Copy,
  Check,
  Send,
  Bot,
  User,
  Sliders,
  Sparkles,
  RefreshCw,
  Coins,
  ChevronDown,
  ChevronUp,
  Key,
  Layers,
  Plus,
  Trash2,
  Globe,
  DollarSign,
  LogOut,
  LogIn,
  Code,
  AlertCircle,
  Box,
  HardDrive
} from 'lucide-react';

export default function App() {
  // Navigation & User Context
  const [activeTab, setActiveTab] = useState('deploy'); // 'deploy', 'host', 'deployments', 'studio', 'mesh', 'billing'
  const [isProMode, setIsProMode] = useState(false);
  
  // Auth State
  const [token, setToken] = useState(localStorage.getItem('ayeusann_token') || '');
  const [user, setUser] = useState(JSON.parse(localStorage.getItem('ayeusann_user') || 'null'));
  const [org, setOrg] = useState(JSON.parse(localStorage.getItem('ayeusann_org') || 'null'));
  const [showAuthModal, setShowAuthModal] = useState(false);
  const [authMode, setAuthMode] = useState('login'); // 'login' or 'signup'
  const [authEmail, setAuthEmail] = useState('');
  const [authPassword, setAuthPassword] = useState('');
  const [authName, setAuthName] = useState('');
  const [authError, setAuthError] = useState('');
  const [authLoading, setAuthLoading] = useState(false);

  // Data States
  const [models, setModels] = useState([]);
  const [deployments, setDeployments] = useState([]);
  const [apiKeys, setApiKeys] = useState([]);
  const [hosts, setHosts] = useState([]);
  const [balance, setBalance] = useState(null);
  const [usageEntries, setUsageEntries] = useState([]);
  const [isLoadingModels, setIsLoadingModels] = useState(false);
  const [isLoadingDeployments, setIsLoadingDeployments] = useState(false);
  const [isLoadingHosts, setIsLoadingHosts] = useState(false);
  
  // Host Experience State
  const [hostCmdStyle, setHostCmdStyle] = useState('local'); // 'local' or 'curl'
  const [copiedHostCmd, setCopiedHostCmd] = useState(false);
  const [localConnectedHost, setLocalConnectedHost] = useState(null);
  const [expandedFaq, setExpandedFaq] = useState(null);

  // Deploy Modal State
  const [showDeployModal, setShowDeployModal] = useState(false);
  const [deployTargetModel, setDeployTargetModel] = useState(null);
  const [deployName, setDeployName] = useState('');
  const [deployTier, setDeployTier] = useState('t2');
  const [deployRegion, setDeployRegion] = useState('IN-SOUTH');
  const [isDeploying, setIsDeploying] = useState(false);
  const [deployError, setDeployError] = useState('');

  // API Key Generation State
  const [newKeyName, setNewKeyName] = useState('');
  const [createdKeySecret, setCreatedKeySecret] = useState('');
  const [isCreatingKey, setIsCreatingKey] = useState(false);
  const [copiedKeySecret, setCopiedKeySecret] = useState(false);

  // AI Studio State
  const [selectedChatModel, setSelectedChatModel] = useState('');
  const [chatApiKey, setChatApiKey] = useState('');
  const [messages, setMessages] = useState([
    {
      role: 'assistant',
      content: 'Welcome to AyeusANN AI Studio! Enter an API key and send a prompt to route inference to active GPUs on the decentralized network.'
    }
  ]);
  const [inputPrompt, setInputPrompt] = useState('');
  const [isGenerating, setIsGenerating] = useState(false);
  const [chatLatency, setChatLatency] = useState(null);
  const [chatError, setChatError] = useState('');
  const chatBottomRef = useRef(null);

  // Earnings Estimator
  const [activeHours, setActiveHours] = useState(8);

  // ─── Clipboard Helper ──────────────────────────────────────────
  const copyToClipboard = (text) => {
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).catch(() => fallbackCopy(text));
      } else {
        fallbackCopy(text);
      }
    } catch (_) {
      fallbackCopy(text);
    }
  };

  const fallbackCopy = (text) => {
    const el = document.createElement('textarea');
    el.value = text;
    el.style.position = 'fixed';
    el.style.opacity = '0';
    document.body.appendChild(el);
    el.select();
    try {
      document.execCommand('copy');
    } catch (_) {}
    document.body.removeChild(el);
  };

  // ─── Data Fetching ─────────────────────────────────────────────
  const fetchModels = async () => {
    setIsLoadingModels(true);
    try {
      const res = await fetch('/v1/models');
      if (res.ok) {
        const data = await res.json();
        const list = Array.isArray(data) ? data : (data.models || []);
        setModels(list);
        if (list.length > 0 && !selectedChatModel) {
          setSelectedChatModel(list[0].name);
        }
      }
    } catch (e) {
      console.warn('Failed to load models:', e);
    } finally {
      setIsLoadingModels(false);
    }
  };

  const fetchHosts = async () => {
    setIsLoadingHosts(true);
    try {
      const res = await fetch('/v1/hosts/public');
      if (res.ok) {
        const data = await res.json();
        const list = data.hosts || [];
        setHosts(list);
        
        // Check for local host (prioritize user's own machine)
        const activeHost = list.find(h => 
          (h.name?.toLowerCase().includes('macbook') || 
           h.name?.toLowerCase().includes('aayush') || 
           h.hostname?.toLowerCase().includes('macbook') ||
           h.id === '95145f79-8ebb-4d53-a1cd-230cd294ba19')
        ) || list.find(h => h.status === 'active');
        if (activeHost) {
          setLocalConnectedHost(activeHost);
        }
      }
    } catch (e) {
      console.warn('Failed to load public hosts:', e);
    } finally {
      setIsLoadingHosts(false);
    }
  };

  const fetchDeployments = async () => {
    if (!token) return;
    setIsLoadingDeployments(true);
    try {
      const res = await fetch('/v1/deployments', {
        headers: { 'Authorization': `Bearer ${token}` }
      });
      if (res.ok) {
        const data = await res.json();
        setDeployments(data.deployments || []);
      }
    } catch (e) {
      console.warn('Failed to load deployments:', e);
    } finally {
      setIsLoadingDeployments(false);
    }
  };

  const fetchApiKeys = async () => {
    if (!token) return;
    try {
      const res = await fetch('/v1/api-keys', {
        headers: { 'Authorization': `Bearer ${token}` }
      });
      if (res.ok) {
        const data = await res.json();
        const keys = data.api_keys || [];
        setApiKeys(keys);
      }
    } catch (e) {
      console.warn('Failed to load api keys:', e);
    }
  };

  const fetchBilling = async () => {
    if (!token || !org?.id) return;
    try {
      const [balRes, usageRes] = await Promise.all([
        fetch(`/v1/balance/${org.id}`, { headers: { 'Authorization': `Bearer ${token}` } }),
        fetch(`/v1/usage/${org.id}`, { headers: { 'Authorization': `Bearer ${token}` } })
      ]);
      if (balRes.ok) {
        const b = await balRes.json();
        setBalance(b.balance || null);
      }
      if (usageRes.ok) {
        const u = await usageRes.json();
        setUsageEntries(u.entries || []);
      }
    } catch (e) {
      console.warn('Failed to load billing:', e);
    }
  };

  useEffect(() => {
    fetchModels();
    fetchHosts();
    const hostInterval = setInterval(fetchHosts, 5000);
    return () => clearInterval(hostInterval);
  }, []);


  useEffect(() => {
    if (token) {
      fetchDeployments();
      fetchApiKeys();
      fetchBilling();
      // Auto-provision: create an API key if user has none, then set it for AI Studio
      (async () => {
        try {
          const keysRes = await fetch('/v1/api-keys', { headers: { 'Authorization': `Bearer ${token}` } });
          if (keysRes.ok) {
            const keysData = await keysRes.json();
            if ((keysData.api_keys || []).length === 0) {
              // No API keys yet — create one automatically
              const createRes = await fetch('/v1/api-keys', {
                method: 'POST',
                headers: { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json' },
                body: JSON.stringify({ name: 'Auto Studio Key' })
              });
              if (createRes.ok) {
                const newKey = await createRes.json();
                setChatApiKey(newKey.secret);
                fetchApiKeys();
              }
            }
          }
        } catch (_) {}
      })();
    }
  }, [token, org?.id]);

  useEffect(() => {
    chatBottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages]);

  // ─── Authentication Handlers ───────────────────────────────────
  const handleAuthSubmit = async (e) => {
    e.preventDefault();
    setAuthError('');
    setAuthLoading(true);

    const endpoint = authMode === 'login' ? '/v1/auth/login' : '/v1/auth/signup';
    const payload = authMode === 'login' 
      ? { email: authEmail.trim(), password: authPassword }
      : { email: authEmail.trim(), password: authPassword, name: authName.trim() || 'AyeusANN Member' };

    try {
      const res = await fetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const data = await res.json();
      if (!res.ok) {
        setAuthError(data.error || 'Authentication failed');
        return;
      }

      setToken(data.access_token);
      setUser(data.user);
      setOrg(data.organization);
      localStorage.setItem('ayeusann_token', data.access_token);
      localStorage.setItem('ayeusann_user', JSON.stringify(data.user));
      localStorage.setItem('ayeusann_org', JSON.stringify(data.organization));
      setShowAuthModal(false);
      setAuthPassword('');
    } catch (err) {
      setAuthError(`Network error: ${err.message}`);
    } finally {
      setAuthLoading(false);
    }
  };

  const handleLogout = () => {
    setToken('');
    setUser(null);
    setOrg(null);
    setDeployments([]);
    setApiKeys([]);
    setBalance(null);
    localStorage.removeItem('ayeusann_token');
    localStorage.removeItem('ayeusann_user');
    localStorage.removeItem('ayeusann_org');
  };

  const handleQuickDevLogin = () => {
    setAuthEmail('dev-engineer@AyeusANN.io');
    setAuthPassword('Passw0rd!Secure');
  };

  // ─── Deployment Handler ────────────────────────────────────────
  const openDeployModal = (model) => {
    if (!token) {
      setShowAuthModal(true);
      return;
    }
    setDeployTargetModel(model);
    setDeployName(`${model.name.replace(/[^a-zA-Z0-9-]/g, '-')}-prod`);
    setDeployError('');
    setShowDeployModal(true);
  };

  const handleCreateDeployment = async (e) => {
    e.preventDefault();
    if (!deployTargetModel) return;
    setIsDeploying(true);
    setDeployError('');

    try {
      const res = await fetch('/v1/deployments', {
        method: 'POST',
        headers: {
          'Authorization': `Bearer ${token}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          model_id: deployTargetModel.id,
          name: deployName.trim(),
          tier: deployTier,
          region: deployRegion,
          min_replicas: 1
        })
      });

      const data = await res.json();
      if (!res.ok) {
        setDeployError(data.error || 'Deployment failed');
        return;
      }

      setShowDeployModal(false);
      fetchDeployments();
      setActiveTab('deployments');
    } catch (err) {
      setDeployError(`Deployment request failed: ${err.message}`);
    } finally {
      setIsDeploying(false);
    }
  };

  // ─── API Key Handler ───────────────────────────────────────────
  const handleCreateApiKey = async (e) => {
    e.preventDefault();
    if (!token || !newKeyName.trim()) return;
    setIsCreatingKey(true);

    try {
      const res = await fetch('/v1/api-keys', {
        method: 'POST',
        headers: {
          'Authorization': `Bearer ${token}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({ name: newKeyName.trim() })
      });

      const data = await res.json();
      if (res.ok) {
        setCreatedKeySecret(data.secret);
        setNewKeyName('');
        fetchApiKeys();
        if (!chatApiKey) {
          setChatApiKey(data.secret);
        }
      } else {
        alert(data.error || 'Failed to create API key');
      }
    } catch (err) {
      alert(`API key error: ${err.message}`);
    } finally {
      setIsCreatingKey(false);
    }
  };

  const handleRevokeApiKey = async (keyId) => {
    if (!confirm('Are you sure you want to revoke this API key?')) return;
    try {
      const res = await fetch(`/v1/api-keys/${keyId}`, {
        method: 'DELETE',
        headers: { 'Authorization': `Bearer ${token}` }
      });
      if (res.ok) {
        fetchApiKeys();
      } else {
        const data = await res.json();
        alert(data.error || 'Failed to revoke API key');
      }
    } catch (err) {
      alert(`Revocation error: ${err.message}`);
    }
  };

  // ─── AI Studio Inference Handler ───────────────────────────────
  const handleSendChatMessage = async (customPrompt) => {
    const promptToSend = customPrompt || inputPrompt;
    if (!promptToSend.trim() || isGenerating) return;

    if (!chatApiKey.trim()) {
      setChatError('Please enter a valid AyeusANN API key in the top bar to execute inference.');
      return;
    }
    setChatError('');

    const userMsg = { role: 'user', content: promptToSend };
    setMessages(prev => [...prev, userMsg]);
    if (!customPrompt) setInputPrompt('');
    setIsGenerating(true);

    const startTime = performance.now();

    try {
      const res = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-API-Key': chatApiKey.trim()
        },
        body: JSON.stringify({
          model: selectedChatModel,
          messages: [...messages, userMsg].map(m => ({ role: m.role, content: m.content })),
          max_tokens: 512,
          temperature: 0.7
        })
      });

      const elapsed = Math.round(performance.now() - startTime);
      setChatLatency(elapsed);

      if (res.ok) {
        const data = await res.json();
        const reply = data.choices?.[0]?.message?.content || 'Model returned an empty response.';
        setMessages(prev => [...prev, { role: 'assistant', content: reply, latency: elapsed }]);
      } else {
        const errData = await res.json().catch(() => ({}));
        const errMsg = errData.error?.message || errData.error || res.statusText || 'Inference error';
        setMessages(prev => [
          ...prev, 
          { 
            role: 'assistant', 
            content: `[AyeusANN Inference Error ${res.status}]: ${errMsg}\n\nNote: In AyeusANN, inference is routed directly to GPU replicas serving this model. Verify that you have an active deployment serving '${selectedChatModel}'.`, 
            isError: true 
          }
        ]);
      }
    } catch (err) {
      setChatError(`Network connection error: ${err.message}`);
    } finally {
      setIsGenerating(false);
    }
  };

  const runHostCmdText = hostCmdStyle === 'local' 
    ? './run-node.sh' 
    : 'curl -fsSL http://localhost:8080/join | bash';

  return (
    <div className="app-container">
      {/* ─── Top Header ────────────────────────────────────────── */}
      <header className="app-header">
        <div className="header-content">
          <div className="brand-section" onClick={() => setActiveTab('deploy')}>
            <div className="brand-logo">⚡</div>
            <div>
              <span className="brand-name">AyeusANN</span>
              <span className="brand-tag">Decentralized GPU Network</span>
            </div>
          </div>

          {/* Navigation Tabs */}
          <nav className="nav-tabs">
            <button 
              className={`nav-tab-btn ${activeTab === 'deploy' ? 'active' : ''}`}
              onClick={() => setActiveTab('deploy')}
            >
              <Box size={16} />
              <span>Deploy on AyeusANN</span>
            </button>
            <button 
              className={`nav-tab-btn ${activeTab === 'host' ? 'active' : ''}`}
              onClick={() => setActiveTab('host')}
            >
              <Terminal size={16} />
              <span>Host on AyeusANN</span>
            </button>
            <button 
              className={`nav-tab-btn ${activeTab === 'deployments' ? 'active' : ''}`}
              onClick={() => setActiveTab('deployments')}
            >
              <Layers size={16} />
              <span>My Deployments</span>
            </button>
            <button 
              className={`nav-tab-btn ${activeTab === 'studio' ? 'active' : ''}`}
              onClick={() => setActiveTab('studio')}
            >
              <Bot size={16} />
              <span>AyeusANN AI Studio</span>
            </button>
            <button 
              className={`nav-tab-btn ${activeTab === 'mesh' ? 'active' : ''}`}
              onClick={() => setActiveTab('mesh')}
            >
              <Globe size={16} />
              <span>Global GPU Mesh</span>
            </button>
            <button 
              className={`nav-tab-btn ${activeTab === 'billing' ? 'active' : ''}`}
              onClick={() => setActiveTab('billing')}
            >
              <Coins size={16} />
              <span>Billing & API</span>
            </button>
          </nav>

          {/* Right Area: Status & Auth */}
          <div className="header-actions">
            <div className="status-pill">
              <span className="pulse-dot"></span>
              <span>Mesh Online</span>
            </div>

            {token && user ? (
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <div className="wallet-badge" title="Organization Wallet Balance">
                  <Coins size={14} />
                  <span>
                    {balance ? `$${parseFloat(balance.amount || balance).toFixed(2)} USD` : 'Connected'}
                  </span>
                </div>
                <button 
                  className="mode-toggle"
                  onClick={handleLogout}
                  title="Sign out of AyeusANN"
                >
                  <LogOut size={14} />
                  <span>{user.name || user.email.split('@')[0]}</span>
                </button>
              </div>
            ) : (
              <button 
                className="mode-toggle"
                onClick={() => { setAuthMode('login'); setShowAuthModal(true); }}
                style={{ background: 'linear-gradient(135deg, rgba(99,102,241,0.2), rgba(6,182,212,0.15))', color: '#fff', borderColor: 'rgba(99,102,241,0.4)' }}
              >
                <LogIn size={14} />
                <span>Sign In to AyeusANN</span>
              </button>
            )}
          </div>
        </div>
      </header>

      {/* ─── Two-Sided Marketplace Value Proposition ─────────── */}
      <div style={{ background: 'rgba(15, 23, 42, 0.45)', borderBottom: '1px solid var(--border-subtle)', padding: '0.85rem 1.5rem' }}>
        <div style={{ maxWidth: '1400px', margin: '0 auto', display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: '1rem', fontSize: '0.88rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.6rem' }}>
            <span style={{ fontWeight: 700, color: '#818cf8', display: 'flex', alignItems: 'center', gap: '0.3rem' }}>
              <Box size={15} /> FOR AI DEVELOPERS:
            </span>
            <span style={{ color: '#94a3b8' }}>
              Choose Model → Select GPU Tier → Deploy → Instant OpenAI-Compatible API Endpoint.
            </span>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.6rem' }}>
            <span style={{ fontWeight: 700, color: '#34d399', display: 'flex', alignItems: 'center', gap: '0.3rem' }}>
              <Terminal size={15} /> FOR GPU OWNERS:
            </span>
            <span style={{ color: '#94a3b8' }}>
              Have idle compute? Run <code>./run-node.sh</code> → Detect & Benchmark → Serve workloads → Earn.
            </span>
          </div>
        </div>
      </div>

      {/* ─── Main Content ──────────────────────────────────────── */}
      <main className="app-main">
        {/* ======================================================= */}
        {/* 1. DEPLOY ON AYEUSANN (CUSTOMER DEMAND EXPERIENCE)      */}
        {/* ======================================================= */}
        {activeTab === 'deploy' && (
          <div>
            <section className="hero-banner">
              <div className="hero-badge">
                <Sparkles size={14} />
                <span>Deploy on AyeusANN</span>
              </div>
              <h1 className="hero-title">
                Deploy open-source AI models onto decentralized GPUs.
              </h1>
              <p className="hero-description">
                Select from verified open-source models. AyeusANN's intelligent scheduler provisions a replica 
                on an active GPU in seconds and provides an OpenAI-compatible API endpoint.
              </p>
            </section>

            {/* Model Catalog Grid */}
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '1.25rem' }}>
              <div>
                <h3 style={{ fontSize: '1.25rem', fontWeight: 700 }}>Available Model Catalog</h3>
                <p style={{ color: '#94a3b8', fontSize: '0.85rem' }}>
                  Live catalog loaded from AyeusANN Control API ({models.length} models ready).
                </p>
              </div>
              <button 
                onClick={fetchModels}
                style={{
                  display: 'flex', alignItems: 'center', gap: '0.4rem', padding: '0.4rem 0.8rem',
                  borderRadius: '8px', background: 'rgba(255,255,255,0.05)', border: '1px solid var(--border-subtle)',
                  color: '#94a3b8', fontSize: '0.8rem', cursor: 'pointer'
                }}
              >
                <RefreshCw size={12} className={isLoadingModels ? 'spin' : ''} />
                <span>Refresh Catalog</span>
              </button>
            </div>

            <div className="nodes-grid">
              {models.map(model => (
                <div key={model.id} className="glass-card highlight">
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '0.75rem' }}>
                    <div>
                      <h4 style={{ fontSize: '1.15rem', fontWeight: 700, color: '#fff' }}>{model.name}</h4>
                      <div style={{ fontSize: '0.75rem', color: '#818cf8', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                        Family: {model.family} • {model.params_b}B Parameters
                      </div>
                    </div>
                    <span className="tier-badge tier-t2">
                      {model.license || 'Open-Source'}
                    </span>
                  </div>

                  <div style={{ margin: '1rem 0', fontSize: '0.82rem', color: '#94a3b8', lineHeight: 1.6 }}>
                    <div>Min VRAM Required: <strong style={{ color: '#fff' }}>{model.min_vram_gb} GB</strong></div>
                    <div>Eligible Tiers: <strong style={{ color: '#fff' }}>{(model.tiers_allowed || ['t1', 't2', 't3']).join(', ').toUpperCase()}</strong></div>
                    <div>
                      Pricing: <strong style={{ color: '#34d399' }}>
                        ${parseFloat(model.price_in_per_1m?.amount || '0.08').toFixed(4)}
                      </strong> / 1M Input • <strong style={{ color: '#34d399' }}>
                        ${parseFloat(model.price_out_per_1m?.amount || '0.22').toFixed(4)}
                      </strong> / 1M Output
                    </div>
                  </div>

                  <button 
                    className="copy-btn"
                    style={{ width: '100%', justifyContent: 'center' }}
                    onClick={() => openDeployModal(model)}
                  >
                    <Zap size={16} />
                    <span>Deploy on AyeusANN</span>
                  </button>
                </div>
              ))}
            </div>

            {/* Compute Tier Explainer */}
            <div className="glass-card" style={{ marginTop: '2.5rem' }}>
              <h3 className="card-title">
                <Sliders size={18} color="#818cf8" />
                <span>AyeusANN Compute Tiers Explained</span>
              </h3>
              <p className="card-subtitle">
                AyeusANN classifies all connected hardware into three strict tiers so workloads match hardware reliability.
              </p>

              <div className="steps-grid" style={{ marginTop: '1.25rem', marginBottom: 0 }}>
                <div className="step-card">
                  <span className="tier-badge tier-t1" style={{ marginBottom: '0.5rem', display: 'inline-block' }}>Tier 1 (T1) — Datacenter</span>
                  <div style={{ fontSize: '0.88rem', color: '#fff', fontWeight: 600, marginBottom: '0.25rem' }}>Enterprise GPUs (H100, A100, RTX 4090)</div>
                  <p className="step-desc">Strict 99.9% uptime SLA with ECC memory. Ideal for production models and latency-critical APIs.</p>
                </div>
                <div className="step-card">
                  <span className="tier-badge tier-t2" style={{ marginBottom: '0.5rem', display: 'inline-block' }}>Tier 2 (T2) — Prosumer</span>
                  <div style={{ fontSize: '0.88rem', color: '#fff', fontWeight: 600, marginBottom: '0.25rem' }}>High-End Workstations (RTX 3080/3090, Apple M-Max)</div>
                  <p className="step-desc">Exceptional speed and bandwidth for developer prototyping and medium-scale workloads.</p>
                </div>
                <div className="step-card">
                  <span className="tier-badge tier-t3" style={{ marginBottom: '0.5rem', display: 'inline-block' }}>Tier 3 (T3) — Community</span>
                  <div style={{ fontSize: '0.88rem', color: '#fff', fontWeight: 600, marginBottom: '0.25rem' }}>Apple Silicon (M1-M4) & Laptop GPUs</div>
                  <p className="step-desc">Cost-effective compute for lightweight models, batch jobs, and distributed inference.</p>
                </div>
              </div>
            </div>
          </div>
        )}

        {/* ======================================================= */}
        {/* 2. HOST ON AYEUSANN (GPU PROVIDER SUPPLY EXPERIENCE)   */}
        {/* ======================================================= */}
        {activeTab === 'host' && (
          <div>
            <section className="hero-banner">
              <div className="hero-badge">
                <Terminal size={14} />
                <span>Host on AyeusANN</span>
              </div>
              <h1 className="hero-title">
                Connect your GPU and earn rewards serving AI.
              </h1>
              <p className="hero-description">
                Turn your idle Mac (Apple Silicon) or NVIDIA PC into a worker node. 
                Our agent safely detects your hardware, measures bandwidth, and joins the secure mesh.
              </p>
            </section>

            {/* Quick Command Launcher Box */}
            <div className="glass-card highlight" style={{ maxWidth: '840px', margin: '0 auto 2rem' }}>
              <div className="card-header">
                <div>
                  <h3 className="card-title">
                    <Terminal size={18} color="#818cf8" />
                    <span>AyeusANN Host Agent Launcher</span>
                  </h3>
                  <p className="card-subtitle">
                    {hostCmdStyle === 'local' 
                      ? 'Inside the repository root? Run this single command in terminal:'
                      : 'Connecting a new remote server? Run this one-liner:'}
                  </p>
                </div>
                <div style={{ display: 'flex', gap: '0.5rem' }}>
                  <button
                    onClick={() => setHostCmdStyle('local')}
                    style={{
                      padding: '0.25rem 0.6rem', fontSize: '0.75rem', borderRadius: '6px',
                      background: hostCmdStyle === 'local' ? 'rgba(99,102,241,0.25)' : 'transparent',
                      color: hostCmdStyle === 'local' ? '#818cf8' : '#94a3b8',
                      border: '1px solid ' + (hostCmdStyle === 'local' ? 'rgba(99,102,241,0.4)' : 'rgba(255,255,255,0.08)'),
                      cursor: 'pointer'
                    }}
                  >
                    Local Script
                  </button>
                  <button
                    onClick={() => setHostCmdStyle('curl')}
                    style={{
                      padding: '0.25rem 0.6rem', fontSize: '0.75rem', borderRadius: '6px',
                      background: hostCmdStyle === 'curl' ? 'rgba(99,102,241,0.25)' : 'transparent',
                      color: hostCmdStyle === 'curl' ? '#818cf8' : '#94a3b8',
                      border: '1px solid ' + (hostCmdStyle === 'curl' ? 'rgba(99,102,241,0.4)' : 'rgba(255,255,255,0.08)'),
                      cursor: 'pointer'
                    }}
                  >
                    1-Liner Curl
                  </button>
                </div>
              </div>

              <div className="command-box">
                <div className="command-content">
                  <span className="prompt-char">$</span>
                  <span>{runHostCmdText}</span>
                </div>
                <button 
                  className={`copy-btn ${copiedHostCmd ? 'copied' : ''}`}
                  onClick={() => {
                    copyToClipboard(runHostCmdText);
                    setCopiedHostCmd(true);
                    setTimeout(() => setCopiedHostCmd(false), 2000);
                  }}
                >
                  {copiedHostCmd ? <Check size={16} /> : <Copy size={16} />}
                  <span>{copiedHostCmd ? 'Copied to Clipboard!' : 'Copy Command'}</span>
                </button>
              </div>

              <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginTop: '0.85rem', fontSize: '0.8rem', color: '#94a3b8' }}>
                <CheckCircle2 size={14} color="#10b981" />
                <span>Automated ticket acquisition via <code>/v1/hosts/quick-token</code>. Zero manual token copy-paste.</span>
              </div>
            </div>

            {/* Real Host Status Banner */}
            <div className="glass-card" style={{ maxWidth: '920px', margin: '0 auto 2.5rem' }}>
              {localConnectedHost ? (
                <div className="connected-banner">
                  <div className="banner-top">
                    <div className="device-badge">
                      <div className="device-icon-box">
                        <CheckCircle2 size={24} />
                      </div>
                      <div>
                        <div className="device-name">
                          {localConnectedHost.name || 'Your Connected Host'}
                        </div>
                        <div className="device-sub">
                          {localConnectedHost.gpus?.[0]?.model || 'Apple M4 GPU'} • {localConnectedHost.gpus?.[0]?.vram_gb || 16} GB Unified Memory • Region: {localConnectedHost.region}
                        </div>
                      </div>
                    </div>
                    <span className="status-pill">
                      <span className="pulse-dot"></span>
                      <span>Status: {localConnectedHost.status?.toUpperCase() || 'ACTIVE'}</span>
                    </span>
                  </div>

                  <div className="metrics-row">
                    <div className="metric-pill">
                      <div className="metric-label">Reputation Score</div>
                      <div className="metric-val" style={{ color: '#10b981' }}>
                        {localConnectedHost.reputation}/100 ({localConnectedHost.tier?.toUpperCase() || 'T3'})
                      </div>
                    </div>
                    <div className="metric-pill">
                      <div className="metric-label">VRAM Bandwidth</div>
                      <div className="metric-val" style={{ color: '#06b6d4' }}>
                        6.02 GB/s
                      </div>
                    </div>
                    <div className="metric-pill">
                      <div className="metric-label">Disk Read Throughput</div>
                      <div className="metric-val">
                        3,630 MB/s
                      </div>
                    </div>
                    <div className="metric-pill">
                      <div className="metric-label">Workload State</div>
                      <div className="metric-val" style={{ color: '#34d399' }}>
                        Idle / Ready
                      </div>
                    </div>
                  </div>

                  <div style={{ fontSize: '0.78rem', color: '#64748b', fontFamily: 'var(--font-mono)' }}>
                    Host ID: {localConnectedHost.id} • Mesh Fingerprint: {localConnectedHost.hw_fingerprint || 'Cryptographic SHA-256 verified'}
                  </div>
                </div>
              ) : (
                <div className="radar-container">
                  <div className="radar-circle">
                    <div className="sonar-wave"></div>
                    <div className="sonar-wave"></div>
                    <div className="radar-icon">
                      <Server size={28} />
                    </div>
                  </div>
                  <h3 style={{ fontSize: '1.25rem', fontWeight: 700, marginBottom: '0.4rem' }}>
                    Waiting for your host to connect...
                  </h3>
                  <p style={{ color: '#94a3b8', fontSize: '0.9rem', maxWidth: '460px', marginBottom: '1.25rem' }}>
                    Launch <code>./run-node.sh</code> in your terminal. This panel will automatically update with your live hardware specs.
                  </p>
                  <button 
                    onClick={fetchHosts}
                    style={{
                      display: 'flex', alignItems: 'center', gap: '0.4rem', padding: '0.4rem 0.85rem',
                      borderRadius: '8px', background: 'rgba(255,255,255,0.06)', border: '1px solid var(--border-subtle)',
                      color: '#94a3b8', fontSize: '0.8rem', cursor: 'pointer'
                    }}
                  >
                    <RefreshCw size={12} className={isLoadingHosts ? 'spin' : ''} />
                    <span>Refresh Detection</span>
                  </button>
                </div>
              )}
            </div>

            {/* Visual Lifecycle Steps */}
            <div className="steps-grid" style={{ maxWidth: '1000px', margin: '0 auto 2.5rem' }}>
              <div className="step-card">
                <div className="step-num">1</div>
                <h4 className="step-title">Install AyeusANN Host Agent</h4>
                <p className="step-desc">Run <code>./run-node.sh</code> to start the lightweight Rust agent on your Mac or PC.</p>
              </div>
              <div className="step-card">
                <div className="step-num">2</div>
                <h4 className="step-title">Hardware Detection & Benchmark</h4>
                <p className="step-desc">Agent scans GPU, runs memory bandwidth test, and verifies compute integrity.</p>
              </div>
              <div className="step-card">
                <div className="step-num">3</div>
                <h4 className="step-title">Receive Workloads & Earn</h4>
                <p className="step-desc">Scheduler routes customer inference requests to your node. Earnings accrue directly to your wallet.</p>
              </div>
            </div>

            {/* Beginner FAQ Accordion */}
            <div className="glass-card" style={{ maxWidth: '840px', margin: '0 auto' }}>
              <h3 className="card-title" style={{ marginBottom: '0.5rem' }}>
                <HelpCircle size={18} color="#f59e0b" />
                <span>Host Questions Answered (For Beginners)</span>
              </h3>

              <div className="faq-item">
                <div className="faq-q" onClick={() => setExpandedFaq(expandedFaq === 1 ? null : 1)}>
                  <span>What is an AyeusANN GPU node in plain English?</span>
                  {expandedFaq === 1 ? <ChevronUp size={16} /> : <ChevronDown size={16} />}
                </div>
                {expandedFaq === 1 && (
                  <div className="faq-a">
                    Modern AI models (like LLaMA 3.1) require intense matrix math performed on GPUs or Apple Silicon chips. 
                    Instead of paying centralized cloud providers $4/hour, developers deploy models onto AyeusANN's distributed network. 
                    You share your computer's spare GPU capacity and earn 75% of customer usage fees.
                  </div>
                )}
              </div>

              <div className="faq-item">
                <div className="faq-q" onClick={() => setExpandedFaq(expandedFaq === 2 ? null : 2)}>
                  <span>Is my computer and personal data safe?</span>
                  {expandedFaq === 2 && (
                    <div className="faq-a">
                      Yes. The AyeusANN Host Agent operates in a restricted environment. It only uses GPU memory buffers to compute model tensor weights. 
                      It cannot inspect, read, or upload your private documents, browsing history, or personal files.
                    </div>
                  )}
                </div>
              </div>

              <div className="faq-item">
                <div className="faq-q" onClick={() => setExpandedFaq(expandedFaq === 3 ? null : 3)}>
                  <span>How do I stop or pause hosting?</span>
                  {expandedFaq === 3 && (
                    <div className="faq-a">
                      Switch to your terminal window where <code>./run-node.sh</code> is running and press <strong>Ctrl + C</strong>. 
                      The agent unregisters from the network within 1 second and immediately frees all memory.
                    </div>
                  )}
                </div>
              </div>
            </div>
          </div>
        )}

        {/* ======================================================= */}
        {/* 3. MY DEPLOYMENTS (ACTIVE CUSTOMER WORKLOADS)           */}
        {/* ======================================================= */}
        {activeTab === 'deployments' && (
          <div>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '1.5rem' }}>
              <div>
                <h2 style={{ fontSize: '1.5rem', fontWeight: 800 }}>My Customer Deployments</h2>
                <p style={{ color: '#94a3b8', fontSize: '0.9rem' }}>
                  Live deployments running across the AyeusANN GPU cluster.
                </p>
              </div>
              <button 
                onClick={() => setActiveTab('deploy')}
                className="copy-btn"
              >
                <Plus size={16} />
                <span>Deploy New Model</span>
              </button>
            </div>

            {!token ? (
              <div className="glass-card" style={{ textAlign: 'center', padding: '3rem 1.5rem' }}>
                <AlertCircle size={36} color="#818cf8" style={{ margin: '0 auto 1rem' }} />
                <h3 style={{ fontSize: '1.2rem', fontWeight: 700, marginBottom: '0.5rem' }}>Sign In to View Deployments</h3>
                <p style={{ color: '#94a3b8', maxWidth: '420px', margin: '0 auto 1.25rem' }}>
                  Deployments are private to your organization. Please sign in or create an account to view and manage your active model endpoints.
                </p>
                <button 
                  className="copy-btn" 
                  style={{ margin: '0 auto' }}
                  onClick={() => { setAuthMode('login'); setShowAuthModal(true); }}
                >
                  <LogIn size={16} />
                  <span>Sign In to AyeusANN</span>
                </button>
              </div>
            ) : deployments.length === 0 ? (
              <div className="glass-card" style={{ textAlign: 'center', padding: '3rem 1.5rem' }}>
                <Box size={36} color="#64748b" style={{ margin: '0 auto 1rem' }} />
                <h3 style={{ fontSize: '1.2rem', fontWeight: 700, marginBottom: '0.5rem' }}>No Active Deployments</h3>
                <p style={{ color: '#94a3b8', maxWidth: '420px', margin: '0 auto 1.25rem' }}>
                  You haven't deployed any models yet. Browse the catalog and deploy your first model in seconds.
                </p>
                <button 
                  className="copy-btn" 
                  style={{ margin: '0 auto' }}
                  onClick={() => setActiveTab('deploy')}
                >
                  <Zap size={16} />
                  <span>Browse Model Catalog</span>
                </button>
              </div>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
                {deployments.map(dep => {
                  const matchingModel = models.find(m => m.id === dep.model_id);
                  const endpointUrl = dep.endpoint || 'http://localhost:8080/v1/chat/completions';
                  const activeApiKey = apiKeys[0]?.prefix ? `${apiKeys[0].prefix}...` : 'ann_live_sk_...';

                  return (
                    <div key={dep.id} className="glass-card highlight">
                      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: '1rem', marginBottom: '1rem' }}>
                        <div>
                          <div style={{ display: 'flex', alignItems: 'center', gap: '0.6rem' }}>
                            <h3 style={{ fontSize: '1.2rem', fontWeight: 700 }}>{dep.name}</h3>
                            <span className="status-pill" style={{ 
                              background: dep.state === 'serving' ? 'rgba(16, 185, 129, 0.15)' : 'rgba(245, 158, 11, 0.15)',
                              borderColor: dep.state === 'serving' ? 'rgba(16, 185, 129, 0.3)' : 'rgba(245, 158, 11, 0.3)',
                              color: dep.state === 'serving' ? '#34d399' : '#fbbf24'
                            }}>
                              <span className="pulse-dot" style={{ background: dep.state === 'serving' ? '#10b981' : '#f59e0b' }}></span>
                              <span>State: {dep.state?.toUpperCase()}</span>
                            </span>
                          </div>
                          <div style={{ fontSize: '0.8rem', color: '#94a3b8', marginTop: '0.2rem' }}>
                            Model: <strong>{matchingModel?.name || dep.model_id}</strong> • Region: <strong>{dep.region}</strong> • Tier: <strong>{dep.tier?.toUpperCase()}</strong>
                          </div>
                        </div>

                        <button 
                          className="copy-btn"
                          style={{ padding: '0.45rem 0.9rem', fontSize: '0.8rem' }}
                          onClick={() => {
                            setSelectedChatModel(matchingModel?.name || dep.name);
                            setActiveTab('studio');
                          }}
                        >
                          <Bot size={14} />
                          <span>Test in AI Studio</span>
                        </button>
                      </div>

                      {/* Endpoint & Working Code Snippet */}
                      <div style={{ background: 'rgba(0, 0, 0, 0.35)', border: '1px solid var(--border-subtle)', borderRadius: '10px', padding: '1rem' }}>
                        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
                          <span style={{ fontSize: '0.8rem', color: '#818cf8', fontWeight: 600, display: 'flex', alignItems: 'center', gap: '0.4rem' }}>
                            <Code size={14} /> Live OpenAI-Compatible API Endpoint:
                          </span>
                          <button 
                            onClick={() => copyToClipboard(endpointUrl)}
                            style={{ background: 'none', border: 'none', color: '#94a3b8', cursor: 'pointer', fontSize: '0.75rem', display: 'flex', alignItems: 'center', gap: '0.3rem' }}
                          >
                            <Copy size={12} /> Copy URL
                          </button>
                        </div>
                        <div style={{ fontFamily: 'var(--font-mono)', fontSize: '0.85rem', color: '#38bdf8', wordBreak: 'break-all', marginBottom: '0.85rem' }}>
                          {endpointUrl}
                        </div>

                        <div style={{ fontSize: '0.75rem', color: '#64748b', marginBottom: '0.4rem' }}>
                          Quick curl command:
                        </div>
                        <pre style={{ 
                          fontFamily: 'var(--font-mono)', fontSize: '0.78rem', color: '#e2e8f0', 
                          background: 'rgba(0,0,0,0.4)', padding: '0.75rem', borderRadius: '8px', 
                          overflowX: 'auto', whiteSpace: 'pre-wrap' 
                        }}>
{`curl -X POST ${endpointUrl} \\
  -H "Content-Type: application/json" \\
  -H "X-API-Key: ${activeApiKey}" \\
  -d '{"model": "${matchingModel?.name || dep.name}", "messages": [{"role": "user", "content": "Hello AyeusANN!"}]}'`}
                        </pre>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        )}

        {/* ======================================================= */}
        {/* 4. AYEUSANN AI STUDIO (INFERENCE PLAYGROUND)            */}
        {/* ======================================================= */}
        {activeTab === 'studio' && (
          <div className="glass-card chat-container" style={{ maxWidth: '960px', margin: '0 auto' }}>
            <div className="chat-header">
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
                <Bot size={22} color="#818cf8" />
                <div>
                  <h3 style={{ fontSize: '1.1rem', fontWeight: 700 }}>AyeusANN AI Studio</h3>
                  <div style={{ fontSize: '0.75rem', color: '#94a3b8' }}>
                    OpenAI-compatible inference routed to active GPU replicas
                  </div>
                </div>
              </div>

              <div style={{ display: 'flex', alignItems: 'center', gap: '0.85rem', flexWrap: 'wrap' }}>
                {chatLatency && (
                  <span style={{ fontSize: '0.75rem', color: '#10b981', fontFamily: 'var(--font-mono)' }}>
                    ⚡ {chatLatency}ms
                  </span>
                )}
                <div className="model-selector">
                  <Cpu size={14} color="#06b6d4" />
                  <select 
                    value={selectedChatModel} 
                    onChange={(e) => setSelectedChatModel(e.target.value)}
                  >
                    {models.map(m => (
                      <option key={m.id} value={m.name}>{m.name}</option>
                    ))}
                  </select>
                </div>
              </div>
            </div>

            {/* API Key Bar for Real Auth */}
            <div style={{ 
              display: 'flex', alignItems: 'center', gap: '0.6rem', padding: '0.6rem 0.85rem', 
              background: 'rgba(0,0,0,0.35)', borderRadius: '10px', marginBottom: '0.85rem',
              border: '1px solid var(--border-subtle)', fontSize: '0.82rem'
            }}>
              <Key size={14} color="#fbbf24" />
              <span style={{ color: '#94a3b8', whiteSpace: 'nowrap' }}>Active API Key:</span>
              <input 
                type="password"
                placeholder="Enter or paste your AyeusANN API key (e.g. ann_live_sk_...)"
                value={chatApiKey}
                onChange={(e) => setChatApiKey(e.target.value)}
                style={{ 
                  flex: 1, background: 'transparent', border: 'none', color: '#fff', 
                  fontSize: '0.82rem', fontFamily: 'var(--font-mono)', outline: 'none' 
                }}
              />
              {apiKeys.length > 0 && !chatApiKey && (
                <button 
                  onClick={() => alert('Please generate an API key in the Billing & API tab')}
                  style={{ background: 'none', border: 'none', color: '#818cf8', cursor: 'pointer', fontSize: '0.75rem' }}
                >
                  Need a key?
                </button>
              )}
            </div>

            {chatError && (
              <div style={{ padding: '0.6rem 0.85rem', background: 'rgba(244,63,94,0.15)', border: '1px solid rgba(244,63,94,0.3)', borderRadius: '8px', color: '#fb7185', fontSize: '0.8rem', marginBottom: '0.75rem' }}>
                {chatError}
              </div>
            )}

            {/* Chat Messages Area */}
            <div className="chat-messages">
              {messages.map((m, idx) => (
                <div key={idx} className={`chat-message ${m.role}`}>
                  <div className={`chat-avatar ${m.role === 'user' ? 'user' : 'ai'}`}>
                    {m.role === 'user' ? <User size={18} /> : <Bot size={18} />}
                  </div>
                  <div>
                    <div className="chat-bubble" style={{ 
                      borderColor: m.isError ? 'rgba(244,63,94,0.4)' : undefined,
                      color: m.isError ? '#fda4af' : undefined 
                    }}>
                      {m.content}
                    </div>
                    {m.latency && (
                      <div style={{ fontSize: '0.7rem', color: '#64748b', marginTop: '0.25rem' }}>
                        Processed via AyeusANN inference gateway in {m.latency}ms
                      </div>
                    )}
                  </div>
                </div>
              ))}
              {isGenerating && (
                <div className="chat-message ai">
                  <div className="chat-avatar ai">
                    <Bot size={18} />
                  </div>
                  <div className="chat-bubble" style={{ color: '#818cf8', fontStyle: 'italic' }}>
                    Routing request to GPU cluster replica...
                  </div>
                </div>
              )}
              <div ref={chatBottomRef} />
            </div>

            {/* Starter Chips */}
            <div className="chat-chips">
              <button 
                className="chip-btn"
                onClick={() => handleSendChatMessage('Explain how decentralized GPU inference works in AyeusANN')}
              >
                ⚡ How AyeusANN Works
              </button>
              <button 
                className="chip-btn"
                onClick={() => handleSendChatMessage('Write a quick Python script to call the AyeusANN API using OpenAI SDK')}
              >
                🐍 Python OpenAI Script
              </button>
              <button 
                className="chip-btn"
                onClick={() => handleSendChatMessage('Summarize the benefits of Apple Silicon Unified Memory for AI')}
              >
                💻 Apple Silicon for AI
              </button>
            </div>

            {/* Message Input */}
            <form 
              className="chat-input-bar"
              onSubmit={(e) => {
                e.preventDefault();
                handleSendChatMessage();
              }}
            >
              <input 
                type="text"
                placeholder="Ask the model anything (sent via /v1/chat/completions)..."
                value={inputPrompt}
                onChange={(e) => setInputPrompt(e.target.value)}
                disabled={isGenerating}
              />
              <button 
                type="submit" 
                className="send-btn"
                disabled={!inputPrompt.trim() || isGenerating}
              >
                <Send size={16} />
              </button>
            </form>
          </div>
        )}

        {/* ======================================================= */}
        {/* 5. GLOBAL GPU MESH (ACTIVE NODES INVENTORY)             */}
        {/* ======================================================= */}
        {activeTab === 'mesh' && (
          <div>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '1.5rem' }}>
              <div>
                <h2 style={{ fontSize: '1.5rem', fontWeight: 800 }}>Global AyeusANN GPU Mesh</h2>
                <p style={{ color: '#94a3b8', fontSize: '0.9rem' }}>
                  Real-time compute inventory registered across global regions. ({hosts.length} hosts active)
                </p>
              </div>
              <button 
                onClick={fetchHosts}
                style={{
                  display: 'flex', alignItems: 'center', gap: '0.4rem', padding: '0.5rem 1rem',
                  borderRadius: '10px', background: 'rgba(99,102,241,0.15)', border: '1px solid rgba(99,102,241,0.3)',
                  color: '#818cf8', cursor: 'pointer', fontWeight: 600, fontSize: '0.85rem'
                }}
              >
                <RefreshCw size={14} className={isLoadingHosts ? 'spin' : ''} />
                <span>Refresh Nodes</span>
              </button>
            </div>

            <div className="nodes-grid">
              {hosts.map((host) => {
                const gpu = host.gpus?.[0];
                return (
                  <div key={host.id} className="node-card">
                    <div className="node-top">
                      <div>
                        <div className="node-title">{host.name || 'Worker Node'}</div>
                        <div className="node-region">
                          Region: {host.region || 'IN-SOUTH'} • Status: {host.status?.toUpperCase()}
                        </div>
                      </div>
                      <span className={`tier-badge tier-${host.tier || 't3'}`}>
                        {host.tier?.toUpperCase() || 'T3'}
                      </span>
                    </div>

                    <div style={{ margin: '1rem 0' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem', fontSize: '0.9rem' }}>
                        <Cpu size={16} color="#06b6d4" />
                        <span style={{ fontWeight: 600 }}>{gpu?.model || 'Apple M4 / Unified Silicon'}</span>
                      </div>
                      <div style={{ fontSize: '0.8rem', color: '#94a3b8' }}>
                        VRAM: <strong>{gpu?.vram_gb || 16} GB</strong> • Reputation: <strong style={{ color: '#10b981' }}>{host.reputation}/100</strong>
                      </div>
                    </div>

                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', borderTop: '1px solid var(--border-subtle)', paddingTop: '0.75rem', fontSize: '0.75rem' }}>
                      <span style={{ color: host.status === 'active' ? '#34d399' : '#fbbf24', display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
                        <span className="pulse-dot" style={{ background: host.status === 'active' ? '#10b981' : '#f59e0b' }}></span>
                        {host.status === 'active' ? 'READY FOR JOBS' : host.status?.toUpperCase()}
                      </span>
                      <span style={{ color: '#64748b' }}>
                        Host Agent v{host.agent_version || '0.2.0'}
                      </span>
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        )}

        {/* ======================================================= */}
        {/* 6. BILLING & API KEYS (REAL USAGE & ACCESS)             */}
        {/* ======================================================= */}
        {activeTab === 'billing' && (
          <div style={{ maxWidth: '920px', margin: '0 auto' }}>
            <div style={{ marginBottom: '1.5rem' }}>
              <h2 style={{ fontSize: '1.5rem', fontWeight: 800 }}>Billing, Usage & API Keys</h2>
              <p style={{ color: '#94a3b8', fontSize: '0.9rem' }}>
                Manage developer API access keys, view real organization balance, and inspect token usage.
              </p>
            </div>

            {!token ? (
              <div className="glass-card" style={{ textAlign: 'center', padding: '3rem 1.5rem' }}>
                <Key size={36} color="#fbbf24" style={{ margin: '0 auto 1rem' }} />
                <h3 style={{ fontSize: '1.2rem', fontWeight: 700, marginBottom: '0.5rem' }}>Sign In to Manage API Keys & Billing</h3>
                <p style={{ color: '#94a3b8', maxWidth: '420px', margin: '0 auto 1.25rem' }}>
                  API keys and usage metering are tied to your authenticated organization.
                </p>
                <button 
                  className="copy-btn" 
                  style={{ margin: '0 auto' }}
                  onClick={() => { setAuthMode('login'); setShowAuthModal(true); }}
                >
                  <LogIn size={16} />
                  <span>Sign In to AyeusANN</span>
                </button>
              </div>
            ) : (
              <div>
                {/* Balance & Usage Summary Card */}
                <div className="glass-card highlight" style={{ marginBottom: '1.75rem' }}>
                  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '1rem' }}>
                    <h3 className="card-title">
                      <Coins size={18} color="#fbbf24" />
                      <span>Organization Balance</span>
                    </h3>
                    <span style={{ fontSize: '0.8rem', color: '#94a3b8' }}>
                      Org ID: {org?.id || 'default'}
                    </span>
                  </div>

                  <div className="metrics-row">
                    <div className="metric-pill">
                      <div className="metric-label">Current Balance</div>
                      <div className="metric-val" style={{ color: '#10b981' }}>
                        {balance ? `$${parseFloat(balance.amount || balance).toFixed(2)} USD` : '$0.00 USD'}
                      </div>
                    </div>
                    <div className="metric-pill">
                      <div className="metric-label">Total Usage Events</div>
                      <div className="metric-val" style={{ color: '#38bdf8' }}>
                        {usageEntries.length} Records
                      </div>
                    </div>
                    <div className="metric-pill">
                      <div className="metric-label">Host Revenue Share</div>
                      <div className="metric-val" style={{ color: '#fbbf24' }}>
                        75% of compute
                      </div>
                    </div>
                  </div>
                </div>

                {/* API Key Management */}
                <div className="glass-card" style={{ marginBottom: '1.75rem' }}>
                  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '1rem' }}>
                    <div>
                      <h3 className="card-title">
                        <Key size={18} color="#818cf8" />
                        <span>AyeusANN API Keys</span>
                      </h3>
                      <p className="card-subtitle">
                        Authenticated keys used to make OpenAI-compatible requests to <code>/v1/chat/completions</code>.
                      </p>
                    </div>
                  </div>

                  {/* Create New Key Form */}
                  <form onSubmit={handleCreateApiKey} style={{ display: 'flex', gap: '0.75rem', marginBottom: '1.5rem' }}>
                    <input 
                      type="text" 
                      placeholder="Key name (e.g. Production Python Client)"
                      value={newKeyName}
                      onChange={(e) => setNewKeyName(e.target.value)}
                      style={{ 
                        flex: 1, padding: '0.65rem 1rem', borderRadius: '10px', 
                        background: 'rgba(0,0,0,0.4)', border: '1px solid var(--border-subtle)', 
                        color: '#fff', fontSize: '0.88rem', outline: 'none' 
                      }}
                    />
                    <button 
                      type="submit"
                      className="copy-btn"
                      disabled={!newKeyName.trim() || isCreatingKey}
                    >
                      <Plus size={16} />
                      <span>{isCreatingKey ? 'Creating...' : 'Create API Key'}</span>
                    </button>
                  </form>

                  {/* Created Key Alert (Shown only once) */}
                  {createdKeySecret && (
                    <div style={{ padding: '1rem', background: 'rgba(16,185,129,0.12)', border: '1px solid rgba(16,185,129,0.3)', borderRadius: '10px', marginBottom: '1.25rem' }}>
                      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
                        <strong style={{ color: '#34d399', fontSize: '0.85rem' }}>🎉 API Key Created! Save it now (it will not be shown again):</strong>
                        <button 
                          onClick={() => {
                            copyToClipboard(createdKeySecret);
                            setCopiedKeySecret(true);
                            setTimeout(() => setCopiedKeySecret(false), 2000);
                          }}
                          style={{ background: 'none', border: 'none', color: '#34d399', cursor: 'pointer', fontSize: '0.8rem', display: 'flex', alignItems: 'center', gap: '0.3rem' }}
                        >
                          {copiedKeySecret ? <Check size={14} /> : <Copy size={14} />}
                          <span>{copiedKeySecret ? 'Copied!' : 'Copy Key'}</span>
                        </button>
                      </div>
                      <div style={{ fontFamily: 'var(--font-mono)', fontSize: '0.85rem', color: '#fff', wordBreak: 'break-all' }}>
                        {createdKeySecret}
                      </div>
                    </div>
                  )}

                  {/* Keys Table */}
                  {apiKeys.length === 0 ? (
                    <div style={{ textAlign: 'center', padding: '1.5rem', color: '#94a3b8', fontSize: '0.88rem' }}>
                      No API keys created yet. Create one above to execute inference requests.
                    </div>
                  ) : (
                    <div style={{ display: 'flex', flexDirection: 'column', gap: '0.6rem' }}>
                      {apiKeys.map(k => (
                        <div key={k.id} style={{ 
                          display: 'flex', alignItems: 'center', justifyContent: 'space-between', 
                          padding: '0.75rem 1rem', background: 'rgba(0,0,0,0.25)', 
                          borderRadius: '10px', border: '1px solid var(--border-subtle)', fontSize: '0.85rem' 
                        }}>
                          <div>
                            <div style={{ fontWeight: 600, color: '#fff' }}>{k.name}</div>
                            <div style={{ fontFamily: 'var(--font-mono)', fontSize: '0.78rem', color: '#818cf8', marginTop: '0.15rem' }}>
                              Prefix: {k.prefix}... • Created: {new Date(k.created_at).toLocaleDateString()}
                            </div>
                          </div>
                          <button 
                            onClick={() => handleRevokeApiKey(k.id)}
                            style={{ background: 'none', border: 'none', color: '#fb7185', cursor: 'pointer', display: 'flex', alignItems: 'center', gap: '0.3rem', fontSize: '0.8rem' }}
                            title="Revoke this API Key"
                          >
                            <Trash2 size={14} />
                            <span>Revoke</span>
                          </button>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              </div>
            )}

            {/* Earnings Estimator */}
            <div className="glass-card">
              <h3 className="card-title">
                <Coins size={18} color="#fbbf24" />
                <span>GPU Provider Earnings Calculator</span>
              </h3>
              <p className="card-subtitle">
                Estimate how much you can earn hosting your GPU on the AyeusANN network.
              </p>

              <div className="slider-container">
                <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem', fontWeight: 600 }}>
                  <span>Daily Active Hours:</span>
                  <span style={{ color: '#818cf8' }}>{activeHours} Hours / Day</span>
                </div>
                <input 
                  type="range" 
                  min="1" 
                  max="24" 
                  value={activeHours} 
                  onChange={(e) => setActiveHours(Number(e.target.value))} 
                />
              </div>

              <div className="metrics-row">
                <div className="metric-pill">
                  <div className="metric-label">Estimated Daily Payout</div>
                  <div className="metric-val" style={{ color: '#10b981' }}>
                    ${(activeHours * 0.06).toFixed(2)} USD
                  </div>
                </div>
                <div className="metric-pill">
                  <div className="metric-label">Estimated Monthly Payout</div>
                  <div className="metric-val" style={{ color: '#38bdf8' }}>
                    ${(activeHours * 0.06 * 30).toFixed(2)} USD
                  </div>
                </div>
              </div>
            </div>
          </div>
        )}
      </main>

      {/* ─── Deploy Modal ──────────────────────────────────────── */}
      {showDeployModal && deployTargetModel && (
        <div style={{ 
          position: 'fixed', inset: 0, zIndex: 200, background: 'rgba(0,0,0,0.7)', 
          backdropFilter: 'blur(8px)', display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '1rem' 
        }}>
          <div className="glass-card" style={{ maxWidth: '520px', width: '100%', background: '#0b0f19', border: '1px solid rgba(99,102,241,0.4)' }}>
            <h3 style={{ fontSize: '1.25rem', fontWeight: 700, marginBottom: '0.4rem' }}>
              Deploy {deployTargetModel.name}
            </h3>
            <p style={{ fontSize: '0.85rem', color: '#94a3b8', marginBottom: '1.25rem' }}>
              AyeusANN scheduler will match this request with an eligible active GPU replica.
            </p>

            {deployError && (
              <div style={{ padding: '0.65rem', background: 'rgba(244,63,94,0.15)', border: '1px solid rgba(244,63,94,0.3)', borderRadius: '8px', color: '#fb7185', fontSize: '0.82rem', marginBottom: '1rem' }}>
                {deployError}
              </div>
            )}

            <form onSubmit={handleCreateDeployment} style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
              <div>
                <label style={{ fontSize: '0.8rem', color: '#cbd5e1', display: 'block', marginBottom: '0.35rem' }}>Deployment Name</label>
                <input 
                  type="text" 
                  value={deployName}
                  onChange={(e) => setDeployName(e.target.value)}
                  style={{ width: '100%', padding: '0.65rem 0.85rem', borderRadius: '8px', background: 'rgba(0,0,0,0.5)', border: '1px solid var(--border-subtle)', color: '#fff', fontSize: '0.9rem' }}
                  required
                />
              </div>

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem' }}>
                <div>
                  <label style={{ fontSize: '0.8rem', color: '#cbd5e1', display: 'block', marginBottom: '0.35rem' }}>Target Tier</label>
                  <select 
                    value={deployTier}
                    onChange={(e) => setDeployTier(e.target.value)}
                    style={{ width: '100%', padding: '0.65rem 0.85rem', borderRadius: '8px', background: 'rgba(0,0,0,0.5)', border: '1px solid var(--border-subtle)', color: '#fff', fontSize: '0.9rem' }}
                  >
                    <option value="t1">Tier 1 (Datacenter)</option>
                    <option value="t2">Tier 2 (Prosumer)</option>
                    <option value="t3">Tier 3 (Community / Mac)</option>
                  </select>
                </div>
                <div>
                  <label style={{ fontSize: '0.8rem', color: '#cbd5e1', display: 'block', marginBottom: '0.35rem' }}>Region</label>
                  <select 
                    value={deployRegion}
                    onChange={(e) => setDeployRegion(e.target.value)}
                    style={{ width: '100%', padding: '0.65rem 0.85rem', borderRadius: '8px', background: 'rgba(0,0,0,0.5)', border: '1px solid var(--border-subtle)', color: '#fff', fontSize: '0.9rem' }}
                  >
                    <option value="IN-SOUTH">IN-SOUTH (India South)</option>
                    <option value="US-EAST">US-EAST (US East)</option>
                    <option value="EU-WEST">EU-WEST (Europe West)</option>
                  </select>
                </div>
              </div>

              <div style={{ display: 'flex', gap: '0.75rem', marginTop: '1rem' }}>
                <button 
                  type="button"
                  onClick={() => setShowDeployModal(false)}
                  style={{ flex: 1, padding: '0.75rem', borderRadius: '10px', background: 'rgba(255,255,255,0.08)', border: 'none', color: '#fff', cursor: 'pointer', fontWeight: 600 }}
                >
                  Cancel
                </button>
                <button 
                  type="submit"
                  className="copy-btn"
                  style={{ flex: 2, justifyContent: 'center' }}
                  disabled={isDeploying}
                >
                  {isDeploying ? 'Scheduling GPU...' : 'Confirm & Deploy'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* ─── Auth Modal (Real Login / Signup) ────────────────── */}
      {showAuthModal && (
        <div style={{ 
          position: 'fixed', inset: 0, zIndex: 200, background: 'rgba(0,0,0,0.7)', 
          backdropFilter: 'blur(8px)', display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '1rem' 
        }}>
          <div className="glass-card" style={{ maxWidth: '440px', width: '100%', background: '#0b0f19', border: '1px solid rgba(99,102,241,0.4)' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.25rem' }}>
              <div style={{ display: 'flex', gap: '0.5rem' }}>
                <button 
                  onClick={() => { setAuthMode('login'); setAuthError(''); }}
                  style={{ 
                    padding: '0.4rem 0.8rem', borderRadius: '8px', 
                    background: authMode === 'login' ? 'rgba(99,102,241,0.25)' : 'transparent',
                    color: authMode === 'login' ? '#818cf8' : '#94a3b8',
                    border: '1px solid ' + (authMode === 'login' ? 'rgba(99,102,241,0.4)' : 'transparent'),
                    fontWeight: 600, cursor: 'pointer', fontSize: '0.9rem'
                  }}
                >
                  Sign In
                </button>
                <button 
                  onClick={() => { setAuthMode('signup'); setAuthError(''); }}
                  style={{ 
                    padding: '0.4rem 0.8rem', borderRadius: '8px', 
                    background: authMode === 'signup' ? 'rgba(99,102,241,0.25)' : 'transparent',
                    color: authMode === 'signup' ? '#818cf8' : '#94a3b8',
                    border: '1px solid ' + (authMode === 'signup' ? 'rgba(99,102,241,0.4)' : 'transparent'),
                    fontWeight: 600, cursor: 'pointer', fontSize: '0.9rem'
                  }}
                >
                  Create Account
                </button>
              </div>
              <button 
                onClick={() => setShowAuthModal(false)}
                style={{ background: 'none', border: 'none', color: '#94a3b8', cursor: 'pointer', fontSize: '1.2rem' }}
              >
                ✕
              </button>
            </div>

            {authError && (
              <div style={{ padding: '0.65rem', background: 'rgba(244,63,94,0.15)', border: '1px solid rgba(244,63,94,0.3)', borderRadius: '8px', color: '#fb7185', fontSize: '0.82rem', marginBottom: '1rem' }}>
                {authError}
              </div>
            )}

            <form onSubmit={handleAuthSubmit} style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
              {authMode === 'signup' && (
                <div>
                  <label style={{ fontSize: '0.8rem', color: '#cbd5e1', display: 'block', marginBottom: '0.35rem' }}>Full Name</label>
                  <input 
                    type="text" 
                    value={authName}
                    onChange={(e) => setAuthName(e.target.value)}
                    placeholder="Jane Doe"
                    style={{ width: '100%', padding: '0.65rem 0.85rem', borderRadius: '8px', background: 'rgba(0,0,0,0.5)', border: '1px solid var(--border-subtle)', color: '#fff', fontSize: '0.9rem' }}
                  />
                </div>
              )}

              <div>
                <label style={{ fontSize: '0.8rem', color: '#cbd5e1', display: 'block', marginBottom: '0.35rem' }}>Email Address</label>
                <input 
                  type="email" 
                  value={authEmail}
                  onChange={(e) => setAuthEmail(e.target.value)}
                  placeholder="name@company.com"
                  style={{ width: '100%', padding: '0.65rem 0.85rem', borderRadius: '8px', background: 'rgba(0,0,0,0.5)', border: '1px solid var(--border-subtle)', color: '#fff', fontSize: '0.9rem' }}
                  required
                />
              </div>

              <div>
                <label style={{ fontSize: '0.8rem', color: '#cbd5e1', display: 'block', marginBottom: '0.35rem' }}>
                  Password {authMode === 'signup' && <span style={{ color: '#94a3b8' }}>(min 8 chars, 1 upper, 1 digit, 1 symbol)</span>}
                </label>
                <input 
                  type="password" 
                  value={authPassword}
                  onChange={(e) => setAuthPassword(e.target.value)}
                  placeholder="••••••••"
                  style={{ width: '100%', padding: '0.65rem 0.85rem', borderRadius: '8px', background: 'rgba(0,0,0,0.5)', border: '1px solid var(--border-subtle)', color: '#fff', fontSize: '0.9rem' }}
                  required
                />
              </div>

              <div style={{ display: 'flex', gap: '0.75rem', marginTop: '0.5rem' }}>
                <button 
                  type="submit"
                  className="copy-btn"
                  style={{ flex: 1, justifyContent: 'center' }}
                  disabled={authLoading}
                >
                  {authLoading ? 'Authenticating...' : (authMode === 'login' ? 'Sign In' : 'Register Account')}
                </button>
              </div>

              {authMode === 'login' && (
                <div style={{ textAlign: 'center', marginTop: '0.5rem' }}>
                  <button 
                    type="button" 
                    onClick={handleQuickDevLogin}
                    style={{ background: 'none', border: 'none', color: '#818cf8', cursor: 'pointer', fontSize: '0.78rem', textDecoration: 'underline' }}
                  >
                    Quick-fill developer credentials
                  </button>
                </div>
              )}
            </form>
          </div>
        </div>
      )}
    </div>
  );
}
