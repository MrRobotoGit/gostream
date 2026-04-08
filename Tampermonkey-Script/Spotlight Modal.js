// ==UserScript==
// @name         Spotlight Modal
// @namespace    http://tampermonkey.net/
// @version      0.0.1
// @description  Spotlight-style modal for unified request search in Plex
// @author       JJJ
// @match        *://localhost:32302/web/*
// @match        *://127.0.0.1:32302/web/*
// @icon         https://www.google.com/s2/favicons?sz=64&domain=undefined.localhost
// @grant        none
// ==/UserScript==

(function() {
    'use strict';

    const API_BASE = 'http://localhost:8095';
    const TMDB_IMG_PLACEHOLDER = 'data:image/gif;base64,R0lGODlhAQABAAAAACw=';

    let highlightedIndex = -1;
    let searchResults = [];
    let selected = null;
    let confirmCandidates = [];
    let requestId = null;
    let searchTimer = null;
    let requestProcessTimer = null;
    let requestStatusPollTimer = null;

    let currentMode = 'both';
    let lastPreferences = null;

    function setRequestProcess(state, text) {
        const badges = Array.from(document.querySelectorAll('#gostream-request-process'));
        if (!badges.length) return;

        if (requestProcessTimer) {
            clearTimeout(requestProcessTimer);
            requestProcessTimer = null;
        }

        const palette = {
            idle: {
                background: 'rgba(15,23,42,0.7)',
                border: '1px solid rgba(148,163,184,0.35)',
                color: 'rgba(148,163,184,1)',
            },
            running: {
                background: 'rgba(59,130,246,0.16)',
                border: '1px solid rgba(59,130,246,0.4)',
                color: 'rgba(147,197,253,1)',
            },
            success: {
                background: 'rgba(34,197,94,0.18)',
                border: '1px solid rgba(34,197,94,0.45)',
                color: 'rgba(134,239,172,1)',
            },
            error: {
                background: 'rgba(239,68,68,0.18)',
                border: '1px solid rgba(248,113,113,0.45)',
                color: 'rgba(252,165,165,1)',
            }
        };
        const style = palette[state] || palette.idle;

        const displayText = text || (state === 'idle' ? 'Ready' : '');
        badges.forEach(b => {
            b.textContent = displayText;
            b.style.background = style.background;
            b.style.border = style.border;
            b.style.color = style.color;
        });

        if (state === 'success' || state === 'error') {
            requestProcessTimer = setTimeout(() => {
                setRequestProcess('idle', 'Ready');
            }, 6000);
        }
    }

    function stopRequestStatusPolling() {
        if (requestStatusPollTimer) {
            clearTimeout(requestStatusPollTimer);
            requestStatusPollTimer = null;
        }
    }

    function setIdleIfNoActiveRequest() {
        if (requestId) return;
        if (!requestStatusPollTimer) setRequestProcess('idle', 'Ready');
    }

    function el(tag, attrs = {}, children = []) {
        const node = document.createElement(tag);
        Object.entries(attrs).forEach(([k, v]) => {
            if (k === 'style') Object.assign(node.style, v);
            else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
            else if (v !== null && v !== undefined) node.setAttribute(k, String(v));
        });
        children.forEach(c => {
            node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
        });
        return node;
    }

    async function apiGet(path) {
        const res = await fetch(`${API_BASE}${path}`, { credentials: 'omit' });
        if (!res.ok) {
            const txt = await res.text().catch(() => '');
            throw new Error(`GET ${path} failed (${res.status}) ${txt}`);
        }
        return await res.json();
    }

    async function apiPost(path, body) {
        const res = await fetch(`${API_BASE}${path}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body || {}),
            credentials: 'omit'
        });
        const data = await res.json().catch(() => ({}));
        return { ok: res.ok, status: res.status, data };
    }

    async function apiPut(path, body) {
        const res = await fetch(`${API_BASE}${path}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body || {}),
            credentials: 'omit'
        });
        const data = await res.json().catch(() => ({}));
        return { ok: res.ok, status: res.status, data };
    }

    function findPlexSearchAnchor() {
        const input = document.querySelector('#quickSearchInput');
        if (!input) return null;
        const combobox = input.closest('[role="combobox"]');
        return combobox || input.parentElement;
    }

    function openSpotlight() {
        const overlay = document.getElementById('gostream-spotlight-overlay');
        if (!overlay) return;
        overlay.style.display = 'flex';
        setIdleIfNoActiveRequest();
        const input = document.getElementById('gostream-spotlight-input');
        if (input) {
            input.focus();
        }
        highlightedIndex = -1;
        searchResults = [];
        selected = null;
        confirmCandidates = [];
        if (requestId && !requestStatusPollTimer) {
            pollRequestStatus(requestId);
        }
        showPanel('search');
        loadPreferencesIntoModal();
    }

    function closeSpotlight() {
        const overlay = document.getElementById('gostream-spotlight-overlay');
        if (!overlay) return;
        overlay.style.display = 'none';
        const input = document.getElementById('gostream-spotlight-input');
        if (input) input.value = '';
        const resultsBox = document.getElementById('gostream-spotlight-results');
        if (resultsBox) {
            resultsBox.innerHTML = '';
            resultsBox.style.display = 'none';
        }
        const optionsBox = document.getElementById('gostream-spotlight-options');
        if (optionsBox) optionsBox.style.display = 'none';
        const confirmBox = document.getElementById('gostream-spotlight-confirm');
        if (confirmBox) confirmBox.style.display = 'none';
        const progressBox = document.getElementById('gostream-spotlight-progress');
        if (progressBox) progressBox.style.display = 'none';
        highlightedIndex = -1;
        searchResults = [];
        selected = null;
        confirmCandidates = [];
        setIdleIfNoActiveRequest();
    }

    function uniqStrings(xs) {
        const out = [];
        const seen = new Set();
        (xs || []).forEach(v => {
            const s = String(v || '').trim();
            if (!s) return;
            if (seen.has(s)) return;
            seen.add(s);
            out.push(s);
        });
        return out;
    }

    function parseCommaList(raw) {
        if (!raw) return [];
        return uniqStrings(String(raw).split(',').map(x => x.trim()).filter(Boolean));
    }

    function getModeFromUI() {
        const r = document.querySelector('input[name="gostream-mode"]:checked');
        const v = r ? String(r.value || '').trim() : 'both';
        if (v === 'movie' || v === 'tv' || v === 'both') return v;
        return 'both';
    }

    function setModeInUI(mode) {
        const m = (mode === 'movie' || mode === 'tv' || mode === 'both') ? mode : 'both';
        const r = document.querySelector(`input[name="gostream-mode"][value="${m}"]`);
        if (r) r.checked = true;
        currentMode = m;
    }

    function allowedByMode(itemType) {
        const t = String(itemType || '').toLowerCase();
        if (currentMode === 'movie') return t === 'movie';
        if (currentMode === 'tv') return t === 'tv';
        return t === 'movie' || t === 'tv';
    }

    function getDefaultPairFromUI(inputId1, inputId2) {
        const a = String((document.getElementById(inputId1) || {}).value || '').trim();
        const b = String((document.getElementById(inputId2) || {}).value || '').trim();
        if (!a && b) return [b, ''];
        return [a, b];
    }

    function applyDefaultsFirst(list, default1, default2) {
        const xs = uniqStrings(list || []);
        const d1 = String(default1 || '').trim();
        const d2 = String(default2 || '').trim();
        const defaults = uniqStrings([d1, d2]);
        return [...defaults, ...xs.filter(x => !defaults.includes(x))];
    }

    function getPreferencesFromUI() {
        const requireMultiAudio = !!(document.getElementById('gostream-req-multiaudio') || {}).checked;
        const requireMultiSubs = !!(document.getElementById('gostream-req-multisubs') || {}).checked;

        const audioFromChecks = Array.from(document.querySelectorAll('input.gostream-audio-lang'))
            .filter(x => x.checked)
            .map(x => String(x.value));
        const subsFromChecks = Array.from(document.querySelectorAll('input.gostream-sub-lang'))
            .filter(x => x.checked)
            .map(x => String(x.value));

        const audioOther = parseCommaList((document.getElementById('gostream-audio-other') || {}).value);
        const subsOther = parseCommaList((document.getElementById('gostream-subs-other') || {}).value);

        const [defaultAudio1, defaultAudio2] = getDefaultPairFromUI('gostream-default-audio-1', 'gostream-default-audio-2');
        const [defaultSubs1, defaultSubs2] = getDefaultPairFromUI('gostream-default-subs-1', 'gostream-default-subs-2');

        const audioLangs = uniqStrings([...audioFromChecks, ...audioOther]);
        const subLangs = uniqStrings([...subsFromChecks, ...subsOther]);

        return {
            audio_languages: applyDefaultsFirst(audioLangs, defaultAudio1, defaultAudio2),
            subtitle_languages: applyDefaultsFirst(subLangs, defaultSubs1, defaultSubs2),
            require_multi_audio: requireMultiAudio,
            require_multi_subs: requireMultiSubs,
        };
    }

    function fillPreferencesUI(prefs) {
        const p = prefs && typeof prefs === 'object' ? prefs : {};
        lastPreferences = p;

        const multiAudio = document.getElementById('gostream-req-multiaudio');
        const multiSubs = document.getElementById('gostream-req-multisubs');
        if (multiAudio) multiAudio.checked = !!p.require_multi_audio;
        if (multiSubs) multiSubs.checked = !!p.require_multi_subs;

        const audioLangs = uniqStrings(p.audio_languages || []);
        const subLangs = uniqStrings(p.subtitle_languages || []);

        const defaultAudio1 = audioLangs[0] || '';
        const defaultAudio2 = audioLangs[1] || '';
        const defaultSubs1 = subLangs[0] || '';
        const defaultSubs2 = subLangs[1] || '';

        const audioDefault1Input = document.getElementById('gostream-default-audio-1');
        const audioDefault2Input = document.getElementById('gostream-default-audio-2');
        const subsDefault1Input = document.getElementById('gostream-default-subs-1');
        const subsDefault2Input = document.getElementById('gostream-default-subs-2');
        if (audioDefault1Input) audioDefault1Input.value = defaultAudio1;
        if (audioDefault2Input) audioDefault2Input.value = defaultAudio2;
        if (subsDefault1Input) subsDefault1Input.value = defaultSubs1;
        if (subsDefault2Input) subsDefault2Input.value = defaultSubs2;

        const knownAudio = new Set(Array.from(document.querySelectorAll('input.gostream-audio-lang')).map(cb => cb.value));
        const knownSubs = new Set(Array.from(document.querySelectorAll('input.gostream-sub-lang')).map(cb => cb.value));

        document.querySelectorAll('input.gostream-audio-lang').forEach(cb => { cb.checked = audioLangs.includes(cb.value); });
        document.querySelectorAll('input.gostream-sub-lang').forEach(cb => { cb.checked = subLangs.includes(cb.value); });

        const audioOther = audioLangs.filter(x => !knownAudio.has(x) && x !== defaultAudio1 && x !== defaultAudio2).join(', ');
        const subsOther = subLangs.filter(x => !knownSubs.has(x) && x !== defaultSubs1 && x !== defaultSubs2).join(', ');
        const audioOtherInput = document.getElementById('gostream-audio-other');
        const subsOtherInput = document.getElementById('gostream-subs-other');
        if (audioOtherInput) audioOtherInput.value = audioOther;
        if (subsOtherInput) subsOtherInput.value = subsOther;

        const saveDefaults = document.getElementById('gostream-save-defaults');
        if (saveDefaults) saveDefaults.checked = false;
    }

    async function loadPreferencesIntoModal() {
        try {
            const prefs = await apiGet('/api/preferences');
            fillPreferencesUI(prefs);
        } catch (e) {
            fillPreferencesUI(lastPreferences || {});
        }
    }

    function showPanel(which) {
        const resultsBox = document.getElementById('gostream-spotlight-results');
        const optionsBox = document.getElementById('gostream-spotlight-options');
        const confirmBox = document.getElementById('gostream-spotlight-confirm');
        const progressBox = document.getElementById('gostream-spotlight-progress');

        if (resultsBox && which !== 'search') resultsBox.style.display = 'none';
        if (optionsBox) optionsBox.style.display = which === 'options' ? 'block' : 'none';
        if (confirmBox) confirmBox.style.display = which === 'confirm' ? 'flex' : 'none';
        if (progressBox) progressBox.style.display = which === 'progress' ? 'flex' : 'none';
    }

    function renderResults(items) {
        searchResults = items || [];
        highlightedIndex = -1;
        const resultsBox = document.getElementById('gostream-spotlight-results');
        if (!resultsBox) return;

        if (!items || items.length === 0) {
            resultsBox.innerHTML = '<div style="padding: 12px; color: rgba(148,163,184,1); font-size: 13px;">No results</div>';
            resultsBox.style.display = 'block';
            return;
        }

        resultsBox.innerHTML = items.map((item, idx) => {
            const safeTitle = (item.title || '').replace(/</g, '&lt;').replace(/>/g, '&gt;');
            const badge = item.type === 'movie'
                ? '<span style="font-size: 10px; padding: 2px 8px; border-radius: 999px; border: 1px solid rgba(59,130,246,0.3); background: rgba(59,130,246,0.12); color: rgba(147,197,253,1);">MOVIE</span>'
                : '<span style="font-size: 10px; padding: 2px 8px; border-radius: 999px; border: 1px solid rgba(168,85,247,0.3); background: rgba(168,85,247,0.12); color: rgba(216,180,254,1);">TV</span>';
            const posterHtml = item.poster
                ? `<img src="${item.poster}" style="width: 28px; height: 40px; object-fit: cover; border-radius: 6px; background: rgba(30,41,59,1);" />`
                : `<div style="width: 28px; height: 40px; border-radius: 6px; background: rgba(30,41,59,1); display: flex; align-items: center; justify-content: center; font-size: 12px;">--</div>`;
            return `
                <button
                    type="button"
                    class="gostream-result-item"
                    data-index="${idx}"
                    style="all: unset; width: 100%; display: flex; gap: 10px; align-items: center; padding: 10px 12px; cursor: pointer; color: white; box-sizing: border-box; transition: background 0.15s;">
                    ${posterHtml}
                    <div style="flex: 1; min-width: 0; display: flex; flex-direction: column;">
                        <div style="display: flex; align-items: center; gap: 8px;">
                            <div style="font-size: 13px; font-weight: 600; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;">${safeTitle}</div>
                            ${badge}
                        </div>
                        <div style="font-size: 11px; color: rgba(148,163,184,1);">${item.year || '--'} · TMDB ${item.tmdb_id}</div>
                    </div>
                </button>
            `;
        }).join('');

        resultsBox.querySelectorAll('.gostream-result-item').forEach((btn, idx) => {
            btn.addEventListener('mouseenter', () => { btn.style.background = 'rgba(255,255,255,0.05)'; });
            btn.addEventListener('mouseleave', () => {
                btn.style.background = idx === highlightedIndex ? 'rgba(59,130,246,0.2)' : 'transparent';
            });
            btn.addEventListener('click', () => selectResult(searchResults[idx]));
        });

        resultsBox.style.display = 'block';
    }

    function renderHighlightedResults() {
        const items = document.querySelectorAll('.gostream-result-item');
        items.forEach((item, idx) => {
            if (idx === highlightedIndex) {
                item.style.background = 'rgba(59,130,246,0.2)';
                item.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
            } else {
                item.style.background = 'transparent';
            }
        });
    }

    async function selectResult(item) {
        selected = item;
        setIdleIfNoActiveRequest();
        showPanel('options');
        const title = document.getElementById('gostream-selected-title');
        const meta = document.getElementById('gostream-selected-meta');
        const poster = document.getElementById('gostream-selected-poster');
        if (title) title.textContent = item.title || '--';
        if (meta) meta.textContent = `${String(item.type || '').toUpperCase()} · ${item.year || '--'} · TMDB ${item.tmdb_id}`;
        if (poster) poster.src = item.poster || TMDB_IMG_PLACEHOLDER;

        const collectionComplete = document.getElementById('gostream-req-collection-complete');
        if (collectionComplete) collectionComplete.checked = false;
    }

    function normalizeCandidateList(seedFallback, expandRes) {
        const out = [];
        const seen = new Set();
        function add(x) {
            if (!x) return;
            const type = String(x.type || '').toLowerCase();
            const tmdbId = Number(x.tmdb_id);
            if (!(type === 'movie' || type === 'tv')) return;
            if (!Number.isFinite(tmdbId) || tmdbId <= 0) return;
            const k = `${type}:${tmdbId}`;
            if (seen.has(k)) return;
            seen.add(k);
            out.push({
                type,
                tmdb_id: tmdbId,
                title: x.title || '--',
                year: x.year || '',
                poster: x.poster || '',
                reason: x.reason || ''
            });
        }
        if (expandRes && typeof expandRes === 'object') {
            add(expandRes.seed);
            (expandRes.candidates || []).forEach(add);
        }
        add(seedFallback);
        return out;
    }

    function renderConfirmCandidates(items) {
        const box = document.getElementById('gostream-confirm-list');
        if (!box) return;
        const list = items || [];
        box.innerHTML = list.map((it, idx) => {
            const safeTitle = (it.title || '').replace(/</g, '&lt;').replace(/>/g, '&gt;');
            const badge = it.type === 'movie'
                ? '<span style="font-size: 10px; padding: 2px 8px; border-radius: 999px; border: 1px solid rgba(59,130,246,0.3); background: rgba(59,130,246,0.12); color: rgba(147,197,253,1);">MOVIE</span>'
                : '<span style="font-size: 10px; padding: 2px 8px; border-radius: 999px; border: 1px solid rgba(168,85,247,0.3); background: rgba(168,85,247,0.12); color: rgba(216,180,254,1);">TV</span>';
            const posterHtml = it.poster
                ? `<img src="${it.poster}" style="width: 28px; height: 40px; object-fit: cover; border-radius: 6px; background: rgba(30,41,59,1);" />`
                : `<div style="width: 28px; height: 40px; border-radius: 6px; background: rgba(30,41,59,1); display: flex; align-items: center; justify-content: center; font-size: 12px;">--</div>`;
            const reasonHtml = it.reason
                ? `<span style="font-size: 10px; padding: 2px 8px; border-radius: 999px; border: 1px solid rgba(148,163,184,0.25); background: rgba(15,23,42,0.55); color: rgba(203,213,225,1);">${String(it.reason).replace(/</g, '&lt;').replace(/>/g, '&gt;')}</span>`
                : '';
            const allowed = allowedByMode(it.type);
            const disabledStyle = allowed ? '' : 'opacity: 0.55;';
            const disabledNote = allowed ? '' : '<div style="font-size: 11px; color: rgba(148,163,184,1);">Disabled by mode</div>';
            return `
                <label style="display:flex; gap: 10px; align-items: center; padding: 10px 12px; border-bottom: 1px solid rgba(255,255,255,0.06); ${disabledStyle}">
                    <input class="gostream-confirm-cb" data-idx="${idx}" type="checkbox" ${allowed ? 'checked' : ''} ${allowed ? '' : 'disabled'} />
                    ${posterHtml}
                    <div style="flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 4px;">
                        <div style="display:flex; align-items:center; gap: 8px; flex-wrap: wrap;">
                            <div style="font-size: 13px; font-weight: 600; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 420px;">${safeTitle}</div>
                            ${badge}
                            ${reasonHtml}
                        </div>
                        <div style="font-size: 11px; color: rgba(148,163,184,1);">${it.year || '--'} · TMDB ${it.tmdb_id}</div>
                        ${disabledNote}
                    </div>
                </label>
            `;
        }).join('');

        const updateCount = () => {
            const cbs = Array.from(document.querySelectorAll('input.gostream-confirm-cb'));
            const selectedCount = cbs.filter(cb => cb.checked && !cb.disabled).length;
            const out = document.getElementById('gostream-confirm-count');
            if (out) out.textContent = `${selectedCount} selected`;
        };

        box.querySelectorAll('input.gostream-confirm-cb').forEach(cb => {
            cb.addEventListener('change', updateCount);
        });
        updateCount();
    }

    function selectAllConfirm(state) {
        Array.from(document.querySelectorAll('input.gostream-confirm-cb')).forEach(cb => {
            if (cb.disabled) return;
            cb.checked = !!state;
        });
        const out = document.getElementById('gostream-confirm-count');
        if (out) {
            const cbs = Array.from(document.querySelectorAll('input.gostream-confirm-cb'));
            const selectedCount = cbs.filter(cb => cb.checked && !cb.disabled).length;
            out.textContent = `${selectedCount} selected`;
        }
    }

    function setProgressText(text) {
        const t = document.getElementById('gostream-progress-text');
        if (t) t.textContent = text || '';
    }

    function setProgressJson(obj) {
        const pre = document.getElementById('gostream-progress-json');
        if (!pre) return;
        try {
            pre.textContent = JSON.stringify(obj || {}, null, 2);
        } catch (e) {
            pre.textContent = String(obj || '');
        }
    }

    function inferDone(st) {
        const s = st && typeof st === 'object' ? st : {};
        const state = String(s.state || s.status || '').toLowerCase();
        if (['completed', 'complete', 'done', 'success', 'failed', 'error', 'stopped', 'cancelled', 'canceled'].includes(state)) return true;
        if (s.done === true) return true;
        if (s.running === false && state) return true;
        return false;
    }

    function inferLabel(st) {
        const s = st && typeof st === 'object' ? st : {};
        const state = String(s.state || s.status || '').trim();
        const phase = String(s.phase || s.step || '').trim();
        const msg = String(s.message || s.detail || '').trim();
        const parts = [state, phase, msg].filter(Boolean);
        return parts.join(' · ');
    }

    async function pollRequestStatus(reqId) {
        stopRequestStatusPolling();
        if (!reqId) return;
        let consecutiveErrors = 0;

        const tick = async () => {
            try {
                const st = await apiGet(`/api/request/status?request_id=${encodeURIComponent(String(reqId))}`);
                consecutiveErrors = 0;

                const label = inferLabel(st) || 'Working...';
                setProgressText(label);
                setProgressJson(st);

                if (inferDone(st)) {
                    stopRequestStatusPolling();
                    requestId = null;
                    const state = String(st.state || st.status || '').toLowerCase();
                    if (['completed', 'complete', 'done', 'success'].includes(state)) {
                        setRequestProcess('success', 'Completed');
                    } else {
                        setRequestProcess('error', state ? state : 'Finished');
                    }
                    return;
                }

                setRequestProcess('running', 'Downloading...');
                requestStatusPollTimer = setTimeout(tick, 1500);
            } catch (e) {
                consecutiveErrors += 1;
                if (consecutiveErrors >= 5) {
                    stopRequestStatusPolling();
                    setRequestProcess('error', 'Status error');
                    setProgressText('Status error');
                    return;
                }
                requestStatusPollTimer = setTimeout(tick, 2000);
            }
        };

        setRequestProcess('running', 'Downloading...');
        requestStatusPollTimer = setTimeout(tick, 600);
    }

    async function goToConfirmStep() {
        if (!selected) return;
        const status = document.getElementById('gostream-request-status');
        if (status) status.textContent = '';

        const wantCollectionComplete = !!(document.getElementById('gostream-req-collection-complete') || {}).checked;
        const seed = {
            type: String(selected.type || '').toLowerCase(),
            tmdb_id: Number(selected.tmdb_id),
            title: selected.title || '--',
            year: selected.year || '',
            poster: selected.poster || ''
        };

        try {
            setRequestProcess('running', wantCollectionComplete ? 'Expanding...' : 'Preparing...');
            let expanded = null;
            if (wantCollectionComplete) {
                const r = await apiPost('/api/request/expand', {
                    type: seed.type,
                    tmdb_id: seed.tmdb_id,
                });
                if (!r.ok) throw new Error(r.data && r.data.message ? r.data.message : `Expand failed (${r.status})`);
                expanded = r.data;
            }

            confirmCandidates = normalizeCandidateList(seed, expanded);
            renderConfirmCandidates(confirmCandidates);
            showPanel('confirm');
            setIdleIfNoActiveRequest();
        } catch (e) {
            setRequestProcess('error', 'Expand error');
            if (status) status.textContent = `Error: ${e.message}`;
        }
    }

    async function submitConfirmedRequest() {
        const status = document.getElementById('gostream-request-status');
        if (status) status.textContent = '';

        const saveDefaults = !!(document.getElementById('gostream-save-defaults') || {}).checked;
        if (saveDefaults) {
            try {
                setRequestProcess('running', 'Saving defaults...');
                const prefs = getPreferencesFromUI();
                const r = await apiPut('/api/preferences', prefs);
                if (!r.ok) throw new Error(r.data && r.data.message ? r.data.message : `Save defaults failed (${r.status})`);
            } catch (e) {
                setRequestProcess('error', 'Prefs error');
                if (status) status.textContent = `Preferences error: ${e.message}`;
                return;
            }
        }

        const cbs = Array.from(document.querySelectorAll('input.gostream-confirm-cb'));
        const picked = cbs
            .filter(cb => cb.checked && !cb.disabled)
            .map(cb => confirmCandidates[Number(cb.getAttribute('data-idx'))])
            .filter(Boolean)
            .filter(it => allowedByMode(it.type));

        const movieIds = Array.from(new Set(picked.filter(x => x.type === 'movie').map(x => Number(x.tmdb_id)).filter(n => Number.isFinite(n))));
        const tvIds = Array.from(new Set(picked.filter(x => x.type === 'tv').map(x => Number(x.tmdb_id)).filter(n => Number.isFinite(n))));

        const payload = {
            movie_tmdb_ids: currentMode === 'tv' ? [] : movieIds,
            tv_tmdb_ids: currentMode === 'movie' ? [] : tvIds,
        };

        if (payload.movie_tmdb_ids.length === 0 && payload.tv_tmdb_ids.length === 0) {
            setRequestProcess('error', 'Nothing selected');
            if (status) status.textContent = 'Select at least one candidate.';
            return;
        }

        try {
            setRequestProcess('running', 'Submitting...');
            const r = await apiPost('/api/request/submit', payload);
            if (!r.ok) throw new Error(r.data && r.data.message ? r.data.message : `Submit failed (${r.status})`);
            requestId = r.data && r.data.request_id ? r.data.request_id : null;
            if (!requestId) throw new Error('Missing request_id');

            const rid = document.getElementById('gostream-progress-request-id');
            if (rid) rid.textContent = String(requestId);
            setProgressText('Queued');
            setProgressJson({ request_id: requestId });
            showPanel('progress');
            await pollRequestStatus(requestId);
        } catch (e) {
            setRequestProcess('error', 'Submit error');
            if (status) status.textContent = `Submit error: ${e.message}`;
        }
    }

    async function doSearch() {
        const input = document.getElementById('gostream-spotlight-input');
        if (!input) return;
        const q = input.value.trim();
        const resultsBox = document.getElementById('gostream-spotlight-results');
        if (searchTimer) clearTimeout(searchTimer);
        selected = null;
        confirmCandidates = [];
        showPanel('search');

        if (q.length < 2) {
            if (resultsBox) {
                resultsBox.innerHTML = '';
                resultsBox.style.display = 'none';
            }
            setIdleIfNoActiveRequest();
            return;
        }
        setRequestProcess('running', 'Searching...');
        renderResults([]);
        searchTimer = setTimeout(async () => {
            try {
                currentMode = getModeFromUI();
                const data = await apiGet(`/api/request/search?q=${encodeURIComponent(q)}&types=${encodeURIComponent(currentMode)}&limit=10`);

                const results = Array.isArray(data && data.results) ? data.results.slice() : [];
                const qNorm = String(q || '').toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();
                const scoreTitle = (title) => {
                    const tNorm = String(title || '').toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();
                    if (!qNorm || !tNorm) return 0;
                    if (tNorm === qNorm) return 100;
                    if (tNorm.startsWith(qNorm + ' ') || tNorm.startsWith(qNorm)) return 80;
                    if (tNorm.includes(' ' + qNorm + ' ')) return 60;
                    if (tNorm.includes(qNorm)) return 40;
                    return 0;
                };

                const ranked = results
                    .map((r, idx) => ({ r, idx, s: scoreTitle(r && r.title) }))
                    .sort((a, b) => (b.s - a.s) || (a.idx - b.idx))
                    .map(x => x.r)
                    .slice(0, 8);

                renderResults(ranked);
                if (!data.results || data.results.length === 0) {
                    setIdleIfNoActiveRequest();
                } else {
                    setRequestProcess('running', 'Searching...');
                }
            } catch (e) {
                if (resultsBox) {
                    resultsBox.innerHTML = '<div style="padding: 12px; color: rgba(248,113,113,1); font-size: 13px;">Search error</div>';
                    resultsBox.style.display = 'block';
                }
                setRequestProcess('error', 'Search error');
            }
        }, 300);
    }

    function buildSpotlightModal() {
        const overlay = el('div', {
            id: 'gostream-spotlight-overlay',
            style: {
                position: 'fixed',
                top: 0,
                left: 0,
                width: '100vw',
                height: '100vh',
                background: 'rgba(0,0,0,0.6)',
                backdropFilter: 'blur(12px)',
                zIndex: 999999,
                display: 'none',
                alignItems: 'center',
                justifyContent: 'center',
                overflowY: 'auto',
                overflowX: 'hidden',
                padding: '24px 20px',
                boxSizing: 'border-box',
                overscrollBehavior: 'contain'
            }
        });

        const modal = el('div', {
            style: {
                width: '650px',
                maxWidth: '92vw',
                maxHeight: 'calc(100vh - 48px)',
                borderRadius: '16px',
                border: '1px solid rgba(255,255,255,0.12)',
                background: 'rgba(2,6,23,0.95)',
                backdropFilter: 'blur(16px)',
                overflow: 'hidden',
                display: 'flex',
                flexDirection: 'column',
                minHeight: 0
            }
        });

        const searchBox = el('div', { style: { padding: '14px 14px 10px 14px' } }, [
            el('input', {
                id: 'gostream-spotlight-input',
                type: 'search',
                placeholder: 'Search movies or TV shows…',
                style: {
                    width: '100%',
                    height: '38px',
                    padding: '0 12px',
                    borderRadius: '10px',
                    border: '1px solid rgba(255,255,255,0.12)',
                    background: 'rgba(255,255,255,0.06)',
                    color: 'white',
                    outline: 'none',
                    fontSize: '13px'
                }
            }),
            el('div', { style: { marginTop: '10px', display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' } }, [
                el('div', { style: { fontSize: '12px', color: 'rgba(148,163,184,1)', marginRight: '6px' } }, ['Mode:']),
                el('label', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '6px', alignItems: 'center' } }, [
                    el('input', { type: 'radio', name: 'gostream-mode', value: 'movie' }),
                    'Movies only'
                ]),
                el('label', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '6px', alignItems: 'center' } }, [
                    el('input', { type: 'radio', name: 'gostream-mode', value: 'tv' }),
                    'Series only'
                ]),
                el('label', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '6px', alignItems: 'center' } }, [
                    el('input', { type: 'radio', name: 'gostream-mode', value: 'both' }),
                    'Both'
                ]),
            ])
        ]);

        const resultsBox = el('div', {
            id: 'gostream-spotlight-results',
            style: {
                flex: '1 1 auto',
                minHeight: 0,
                overflowY: 'auto',
                display: 'none'
            }
        });

        const optionsBox = el('div', {
            id: 'gostream-spotlight-options',
            style: {
                padding: '14px',
                display: 'none',
                overflowY: 'auto',
                boxSizing: 'border-box',
                flex: '1 1 auto',
                minHeight: 0
            }
        });

        const confirmBox = el('div', {
            id: 'gostream-spotlight-confirm',
            style: {
                padding: '14px',
                display: 'none',
                boxSizing: 'border-box',
                flex: '1 1 auto',
                minHeight: 0,
                overflow: 'hidden',
                flexDirection: 'column'
            }
        });

        const progressBox = el('div', {
            id: 'gostream-spotlight-progress',
            style: {
                padding: '14px',
                display: 'none',
                boxSizing: 'border-box',
                flex: '1 1 auto',
                minHeight: 0,
                overflow: 'hidden',
                flexDirection: 'column'
            }
        });

        const selectedInfo = el('div', { style: { display: 'flex', gap: '12px', marginBottom: '12px' } }, [
            el('img', {
                id: 'gostream-selected-poster',
                src: TMDB_IMG_PLACEHOLDER,
                style: { width: '60px', height: '88px', objectFit: 'cover', borderRadius: '8px', background: 'rgba(30,41,59,1)' }
            }),
            el('div', { style: { flex: 1, display: 'flex', flexDirection: 'column', gap: '4px' } }, [
                el('div', { id: 'gostream-selected-title', style: { fontSize: '14px', fontWeight: 700, color: 'white' } }, ['--']),
                el('div', { id: 'gostream-selected-meta', style: { fontSize: '12px', color: 'rgba(148,163,184,1)' } }, ['--'])
            ])
        ]);

        const toggles = el('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '10px', marginBottom: '10px' } }, [
            el('label', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '8px', alignItems: 'center' } }, [
                el('input', { type: 'checkbox', id: 'gostream-req-multiaudio' }),
                'Multi-audio'
            ]),
            el('label', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '8px', alignItems: 'center' } }, [
                el('input', { type: 'checkbox', id: 'gostream-req-multisubs' }),
                'Multi-subtitles'
            ])
        ]);

        const langs = [
            { code: 'en', label: 'English' },
            { code: 'es', label: 'Spanish' },
            { code: 'it', label: 'Italian' },
            { code: 'fr', label: 'French' },
            { code: 'de', label: 'German' },
            { code: 'pt', label: 'Portuguese' },
            { code: 'ja', label: 'Japanese' },
            { code: 'ko', label: 'Korean' },
        ];

        const langCodeDatalist = el('datalist', { id: 'gostream-lang-codes' }, langs.map(l => el('option', { value: l.code, label: l.label }, [])));

        function defaultPairRow(title, id1, id2) {
            const inputStyle = {
                width: '110px',
                height: '28px',
                padding: '0 8px',
                borderRadius: '10px',
                border: '1px solid rgba(255,255,255,0.12)',
                background: 'rgba(255,255,255,0.04)',
                color: 'white',
                outline: 'none',
                fontSize: '12px'
            };

            return el('div', { style: { marginTop: '6px', display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' } }, [
                el('div', { style: { fontSize: '11px', color: 'rgba(148,163,184,1)' } }, [title]),
                el('label', { style: { fontSize: '11px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '6px', alignItems: 'center' } }, [
                    el('span', { style: { opacity: 0.85 } }, ['Default #1']),
                    el('input', { id: id1, type: 'text', placeholder: 'en', list: 'gostream-lang-codes', style: inputStyle })
                ]),
                el('label', { style: { fontSize: '11px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '6px', alignItems: 'center' } }, [
                    el('span', { style: { opacity: 0.85 } }, ['Default #2']),
                    el('input', { id: id2, type: 'text', placeholder: '(optional)', list: 'gostream-lang-codes', style: inputStyle })
                ])
            ]);
        }

        function langGrid(className) {
            const grid = el('div', { style: { display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: '6px', marginTop: '6px' } });
            langs.forEach(l => {
                grid.appendChild(
                    el('div', { style: { fontSize: '12px', color: 'rgba(148,163,184,1)', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '8px', background: 'rgba(15,23,42,0.55)', border: '1px solid rgba(255,255,255,0.08)', borderRadius: '10px', padding: '6px 8px' } }, [
                        el('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', cursor: 'pointer', flex: 1, minWidth: 0 } }, [
                            el('input', { type: 'checkbox', value: l.code, class: className }),
                            el('span', { style: { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } }, [l.label])
                        ])
                    ])
                );
            });
            return grid;
        }

        const audioLangs = el('div', {}, [
            el('div', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', marginTop: '4px' } }, ['Audio languages']),
            defaultPairRow('Audio defaults:', 'gostream-default-audio-1', 'gostream-default-audio-2'),
            langGrid('gostream-audio-lang'),
            el('input', {
                id: 'gostream-audio-other',
                type: 'text',
                placeholder: 'Other codes (comma-separated), e.g. zh, nl',
                style: {
                    width: '100%',
                    marginTop: '8px',
                    height: '34px',
                    padding: '0 10px',
                    borderRadius: '10px',
                    border: '1px solid rgba(255,255,255,0.12)',
                    background: 'rgba(255,255,255,0.04)',
                    color: 'white',
                    outline: 'none',
                    fontSize: '12px'
                }
            })
        ]);

        const subLangs = el('div', {}, [
            el('div', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', marginTop: '10px' } }, ['Subtitle languages']),
            defaultPairRow('Subtitle defaults:', 'gostream-default-subs-1', 'gostream-default-subs-2'),
            langGrid('gostream-sub-lang'),
            el('input', {
                id: 'gostream-subs-other',
                type: 'text',
                placeholder: 'Other codes (comma-separated), e.g. sv, da',
                style: {
                    width: '100%',
                    marginTop: '8px',
                    height: '34px',
                    padding: '0 10px',
                    borderRadius: '10px',
                    border: '1px solid rgba(255,255,255,0.12)',
                    background: 'rgba(255,255,255,0.04)',
                    color: 'white',
                    outline: 'none',
                    fontSize: '12px'
                }
            })
        ]);

        const saveDefaultsRow = el('div', {
            style: { marginTop: '10px' }
        }, [
            el('label', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '8px', alignItems: 'center' } }, [
                el('input', { type: 'checkbox', id: 'gostream-save-defaults' }),
                'Save defaults (server)'
            ]),
            el('div', { style: { marginTop: '6px', fontSize: '11px', color: 'rgba(148,163,184,1)' } }, [
                'If enabled, preferences are saved via /api/preferences before submitting.'
            ])
        ]);

        const collectionCompleteRow = el('div', {
            style: { marginTop: '10px' }
        }, [
            el('label', { style: { fontSize: '12px', color: 'rgba(203,213,225,1)', display: 'flex', gap: '8px', alignItems: 'center' } }, [
                el('input', { type: 'checkbox', id: 'gostream-req-collection-complete' }),
                'Collection complete (expand)'
            ]),
            el('div', { style: { marginTop: '6px', fontSize: '11px', color: 'rgba(148,163,184,1)' } }, [
                'Calls /api/request/expand and lets you confirm candidates.'
            ])
        ]);

        const status = el('div', { id: 'gostream-request-status', style: { marginTop: '10px', fontSize: '12px', color: 'rgba(148,163,184,1)' } }, ['']);

        const continueBtn = el('button', {
            type: 'button',
            style: {
                marginTop: '12px',
                width: '100%',
                background: 'rgba(59,130,246,0.85)',
                border: 'none',
                color: 'white',
                padding: '10px',
                borderRadius: '12px',
                cursor: 'pointer',
                fontWeight: 700
            },
            onclick: goToConfirmStep
        }, ['Continue']);

        optionsBox.appendChild(selectedInfo);
        optionsBox.appendChild(collectionCompleteRow);
        optionsBox.appendChild(toggles);
        optionsBox.appendChild(langCodeDatalist);
        optionsBox.appendChild(audioLangs);
        optionsBox.appendChild(subLangs);
        optionsBox.appendChild(saveDefaultsRow);
        optionsBox.appendChild(continueBtn);
        optionsBox.appendChild(status);

        const confirmHeader = el('div', { style: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '10px', marginBottom: '10px' } }, [
            el('div', { style: { fontSize: '13px', fontWeight: 700, color: 'white' } }, ['Confirm selection']),
            el('div', { id: 'gostream-confirm-count', style: { fontSize: '12px', color: 'rgba(148,163,184,1)' } }, ['0 selected'])
        ]);

        const confirmActions = el('div', { style: { display: 'flex', gap: '8px', marginBottom: '10px', flexWrap: 'wrap' } }, [
            el('button', {
                type: 'button',
                style: { padding: '6px 10px', borderRadius: '10px', border: '1px solid rgba(255,255,255,0.12)', background: 'rgba(255,255,255,0.06)', color: 'white', cursor: 'pointer', fontSize: '12px' },
                onclick: () => selectAllConfirm(true)
            }, ['Select all']),
            el('button', {
                type: 'button',
                style: { padding: '6px 10px', borderRadius: '10px', border: '1px solid rgba(255,255,255,0.12)', background: 'rgba(255,255,255,0.06)', color: 'white', cursor: 'pointer', fontSize: '12px' },
                onclick: () => selectAllConfirm(false)
            }, ['Select none']),
            el('div', { style: { flex: 1 } }, []),
            el('button', {
                type: 'button',
                style: { padding: '6px 10px', borderRadius: '10px', border: '1px solid rgba(255,255,255,0.12)', background: 'transparent', color: 'rgba(203,213,225,1)', cursor: 'pointer', fontSize: '12px' },
                onclick: () => showPanel('options')
            }, ['Back'])
        ]);

        const confirmList = el('div', {
            id: 'gostream-confirm-list',
            style: { flex: '1 1 auto', minHeight: 0, overflowY: 'auto', borderRadius: '12px', border: '1px solid rgba(255,255,255,0.08)', background: 'rgba(15,23,42,0.35)', marginBottom: '12px' }
        });

        const submitBtn = el('button', {
            type: 'button',
            style: {
                width: '100%',
                background: 'rgba(34,197,94,0.9)',
                border: 'none',
                color: 'white',
                padding: '10px',
                borderRadius: '12px',
                cursor: 'pointer',
                fontWeight: 700
            },
            onclick: submitConfirmedRequest
        }, ['Submit request']);

        confirmBox.appendChild(confirmHeader);
        confirmBox.appendChild(confirmActions);
        confirmBox.appendChild(confirmList);
        confirmBox.appendChild(submitBtn);

        progressBox.appendChild(el('div', { style: { fontSize: '13px', fontWeight: 700, color: 'white', marginBottom: '8px' } }, ['Request status']));
        progressBox.appendChild(el('div', { style: { fontSize: '12px', color: 'rgba(148,163,184,1)', marginBottom: '10px' } }, [
            'request_id: ',
            el('span', { id: 'gostream-progress-request-id', style: { fontWeight: 700, color: 'rgba(203,213,225,1)' } }, ['--'])
        ]));
        progressBox.appendChild(el('div', { id: 'gostream-progress-text', style: { fontSize: '12px', color: 'rgba(203,213,225,1)', marginBottom: '10px' } }, ['']));
        progressBox.appendChild(el('pre', {
            id: 'gostream-progress-json',
            style: { flex: '1 1 auto', minHeight: 0, overflow: 'auto', padding: '10px', borderRadius: '12px', border: '1px solid rgba(255,255,255,0.08)', background: 'rgba(15,23,42,0.45)', color: 'rgba(226,232,240,1)', fontSize: '11px', margin: 0 }
        }, ['{}']));

        progressBox.appendChild(el('button', {
            type: 'button',
            style: { marginTop: '12px', width: '100%', padding: '10px', borderRadius: '12px', border: '1px solid rgba(255,255,255,0.12)', background: 'rgba(255,255,255,0.06)', color: 'white', cursor: 'pointer', fontWeight: 700 },
            onclick: () => showPanel('search')
        }, ['Back to search']));

        modal.appendChild(searchBox);
        modal.appendChild(resultsBox);
        modal.appendChild(optionsBox);
        modal.appendChild(confirmBox);
        modal.appendChild(progressBox);
        overlay.appendChild(modal);

        return overlay;
    }

    function buildTriggerButton() {
        return el('button', {
            type: 'button',
            style: {
                marginLeft: '10px',
                padding: '8px 14px',
                borderRadius: '999px',
                border: '1px solid rgba(59,130,246,0.3)',
                background: 'rgba(59,130,246,0.15)',
                color: 'rgba(147,197,253,1)',
                fontSize: '12px',
                cursor: 'pointer',
                display: 'flex',
                alignItems: 'center',
                gap: '6px'
            },
            onclick: openSpotlight
        }, [
            el('span', {}, ['🔍']),
            el('span', {}, ['Request']),
            el('span', { style: { opacity: 0.7 } }, ['Ctrl+K'])
        ]);
    }

    function buildRequestProcessBadge() {
        const badge = el('span', {
            id: 'gostream-request-process',
            style: {
                marginLeft: '8px',
                padding: '7px 10px',
                borderRadius: '999px',
                border: '1px solid rgba(148,163,184,0.35)',
                background: 'rgba(15,23,42,0.7)',
                color: 'rgba(148,163,184,1)',
                fontSize: '11px',
                lineHeight: '12px',
                whiteSpace: 'nowrap',
                display: 'inline-flex',
                alignItems: 'center'
            }
        }, ['Ready']);
        return badge;
    }

    function mount() {
        if (document.getElementById('gostream-spotlight-overlay')) return;
        const anchor = findPlexSearchAnchor();
        if (!anchor || !anchor.parentElement) return;

        const overlay = buildSpotlightModal();
        document.body.appendChild(overlay);

        setModeInUI('both');

        const btn = buildTriggerButton();
        anchor.parentElement.insertBefore(btn, anchor.nextSibling);
        const processBadge = buildRequestProcessBadge();
        anchor.parentElement.insertBefore(processBadge, btn.nextSibling);
        setRequestProcess('idle', 'Ready');

        const input = document.getElementById('gostream-spotlight-input');
        if (input) input.addEventListener('input', doSearch);

        document.querySelectorAll('input[name="gostream-mode"]').forEach(r => {
            r.addEventListener('change', () => {
                currentMode = getModeFromUI();
                doSearch();
            });
        });

        input.addEventListener('keydown', (e) => {
            if (e.key === 'Escape') {
                e.preventDefault();
                closeSpotlight();
                return;
            }

            if (searchResults.length === 0) return;

            if (e.key === 'ArrowDown') {
                e.preventDefault();
                highlightedIndex = (highlightedIndex + 1) % searchResults.length;
                renderHighlightedResults();
            } else if (e.key === 'ArrowUp') {
                e.preventDefault();
                highlightedIndex = (highlightedIndex - 1 + searchResults.length) % searchResults.length;
                renderHighlightedResults();
            } else if (e.key === 'Enter' && highlightedIndex >= 0) {
                e.preventDefault();
                selectResult(searchResults[highlightedIndex]);
            }
        });

        overlay.addEventListener('click', (e) => {
            if (e.target === overlay) closeSpotlight();
        });
    }

    const obs = new MutationObserver(() => mount());
    obs.observe(document.documentElement, { childList: true, subtree: true });
    mount();

    document.addEventListener('keydown', (e) => {
        const overlay = document.getElementById('gostream-spotlight-overlay');
        if (e.key === 'Escape' && overlay && overlay.style.display !== 'none') {
            e.preventDefault();
            closeSpotlight();
            return;
        }

        if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
            e.preventDefault();
            e.stopPropagation();
            openSpotlight();
        }
    }, true);

})();
