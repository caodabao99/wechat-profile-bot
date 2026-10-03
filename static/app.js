/* 微信画像管理前端逻辑：Vue 3（global build），无构建步骤 */
/* global Vue */
const { createApp, ref, reactive, computed, onMounted, onUnmounted } = Vue;

createApp({
  setup() {
    // ---------- 登录态（Token + TOTP 两步登录） ----------
    const TOKEN_KEY = 'wp_api_token'; // 登录成功后这里存的是「网页会话令牌」，不再存 apiToken
    const authed = ref(false);
    const tokenInput = ref('');
    // authStage: token=输Token / setup=首次绑定验证器 / 2fa=输6位动态码
    const authStage = ref('token');
    const pendingToken = ref('');
    const codeInput = ref('');
    const setupSecret = ref('');
    const setupOtpauth = ref('');
    const setupQr = ref('');
    const loginChecking = ref(false);
    const loginError = ref('');

    // ---------- 路由（hash） ----------
    const route = reactive({ view: 'contacts', id: 0 });

    // ---------- 联系人列表（分页 + 服务端搜索） ----------
    const contacts = ref([]);
    const contactsTotal = ref(0);
    const contactsOffset = ref(0);
    const contactsLimit = 30;
    const loadingContacts = ref(false);
    const search = ref('');        // 输入框内容
    const searchApplied = ref(''); // 实际生效的搜索词（点查询/回车后）
    const showMerged = ref(false);

    // ---------- 联系人详情 ----------
    const contact = ref(null);
    const loadingDetail = ref(false);
    const detailTab = ref('profile');
    const messages = ref([]);
    const messagesLoading = ref(false);
    const messagesHasMore = ref(false);
    const history = ref([]);
    const expandedHistory = ref(0);
    const stats = ref(null);
    const busy = ref(false);
    const assistance = reactive({});
    const assist = computed(() => {
      const id = route.id;
      return assistance[id] || (assistance[id] = { draft: '', review: null, reviewBusy: false, message: '', analyzeBusy: false, replies: [], changes: null, changesBusy: false });
    });
    // 候选回复与「换个说法」共用的四种固定风格，需与服务端 replyStyles 保持一致
    const styles = ['稳妥得体', '简洁直接', '亲切热情', '委婉留余地'];
    // 兼容旧版纯字符串数组：[{style,text}] 优先，字符串则按顺序补默认风格
    function normalizeReplies(raw, single) {
      const list = Array.isArray(raw) ? raw : (single ? [single] : []);
      return list.slice(0, 3).map((r, i) => {
        if (typeof r === 'string') return { text: r, style: styles[i] || styles[0], target: '', busy: false };
        const text = (r.text || r.reply || '').trim();
        const style = styles.includes(r.style) ? r.style : (styles[i] || styles[0]);
        return { text, style, target: '', busy: false };
      }).filter(r => r.text);
    }
    async function copyAssist(text) {
      try { await navigator.clipboard.writeText(text); toast('已复制'); }
      catch (e) { toast('复制失败，请选中文字手动复制', 'error'); }
    }
    async function reviewDraft() {
      const id = route.id, state = assist.value, text = state.draft;
      if (state.reviewBusy) return;
      state.reviewBusy = true;
      try { const out = await api('/api/contacts/' + id + '/review-draft', { method: 'POST', body: { text } }); if (state.draft === text) state.review = out; }
      catch (e) { toast(e.message, 'error'); }
      finally { state.reviewBusy = false; }
    }
    async function analyzeReplies() {
      const id = route.id, state = assist.value, message = state.message;
      if (state.analyzeBusy) return;
      state.analyzeBusy = true;
      try {
        const out = await api('/api/contacts/' + id + '/analyze', { method: 'POST', body: { message } });
        if (state.message === message) state.replies = normalizeReplies(out.suggested_replies, out.suggested_reply);
      } catch (e) { toast(e.message, 'error'); }
      finally { state.analyzeBusy = false; }
    }
    async function rewriteReply(reply) {
      const id = route.id, state = assist.value, original = reply.text, style = reply.target;
      if (reply.busy) return;
      if (!style) { toast('请先点选一种目标风格', 'error'); return; }
      reply.busy = true;
      try {
        const out = await api('/api/contacts/' + id + '/rewrite', { method: 'POST', body: { text: original, style } });
        if (state.replies.includes(reply) && reply.text === original) {
          reply.text = out.reply;
          reply.style = style;   // 卡片标题跟随改写后的风格
          reply.target = '';    // 清空单选项
        }
      } catch (e) { toast(e.message, 'error'); }
      finally { reply.busy = false; }
    }
    async function loadChanges() {
      const id = route.id, state = assist.value;
      if (state.changesBusy) return;
      state.changesBusy = true;
      try { state.changes = await api('/api/contacts/' + id + '/profile-changes'); }
      catch (e) { toast(e.message, 'error'); }
      finally { state.changesBusy = false; }
    }
    function closeChanges() { assist.value.changes = null; }

    // ---------- 合并记录 ----------
    const mergeLogs = ref([]);
    const loadingMerges = ref(false);

    // ---------- 备份 ----------
    const backupBusy = ref(false);
    const backupResult = ref('');
    const backupFile = ref(null);
    const backupLogs = ref([]);
    const encPassword = ref(''); // 导出时给密钥文件设的口令，留空=明文
    const impPassword = ref(''); // 导入加密备份时的口令
    const showEncPwd = ref(false); // 明文显示口令，方便核对

    // ---------- 弹层 ----------
    const showRemark = ref(false);
    const remarkInput = ref('');
    const showSupplement = ref(false);
    const supplementNote = ref('');
    const showMerge = ref(false);
    const mergeSourceId = ref(0);
    const mergeUseSourceName = ref(false);
    const mergeRegenerate = ref(true);
    const showDelete = ref(false);
    const profileEditor = ref(null);
    const profileSaving = ref(false);
    const profileEditError = ref('');
    const profileFields = [
      ['summary', '核心摘要'], ['basic_info.occupation', '职业'], ['basic_info.location', '城市/地区'],
      ['basic_info.important_dates', '重要日子', true], ['personality', '性格特征', true],
      ['communication_style.reply_length', '回复长短'], ['communication_style.tone', '语气'],
      ['communication_style.frequent_phrases', '常用表达', true], ['communication_style.emoji_usage', '表情习惯'],
      ['communication_style.initiative', '主动程度'], ['interests', '兴趣爱好', true],
      ['emotional_patterns.stressors', '压力源/雷点', true], ['emotional_patterns.comfort_topics', '安慰话题', true],
      ['emotional_patterns.when_upset', '不高兴时的表现'], ['relationship.closeness', '亲密程度'],
      ['relationship.recent_events', '近期共同事件', true], ['relationship.interaction_pattern', '互动模式'],
      ['important_facts', '重要事实', true],
    ];
    function startProfileEdit() {
      try {
        const base = contact.value.profileJson || '';
        const p = JSON.parse(base || '{}') || {};
        const values = {};
        for (const [path, , list] of profileFields) {
          let v = path.split('.').reduce((obj, key) => obj && obj[key], p);
          if (path === 'basic_info.important_dates' && v && !Array.isArray(v)) v = Object.entries(v).map(([k, val]) => k + ': ' + val);
          values[path] = list ? (v || []).join('\n') : (v || '');
        }
        profileEditor.value = { id: contact.value.id, base, values, intents: Object.entries(p.intent_patterns || {}).map(([name, description]) => ({ name, description })) };
        profileEditError.value = '';
      } catch (e) { toast('画像无法解析：' + e.message, 'error'); }
    }
    async function saveProfileEdit() {
      if (profileSaving.value) return;
      const draft = profileEditor.value;
      const p = {};
      for (const [path, , list] of profileFields) {
        const keys = path.split('.');
        let obj = p;
        for (const key of keys.slice(0, -1)) obj = obj[key] || (obj[key] = {});
        obj[keys[keys.length - 1]] = list ? draft.values[path].split('\n').map(s => s.trim()).filter(Boolean) : draft.values[path].trim();
      }
      p.intent_patterns = Object.create(null);
      for (const row of draft.intents) {
        const name = row.name.trim(), description = row.description.trim();
        if (!name && !description) continue;
        if (!name || Object.hasOwn(p.intent_patterns, name)) { profileEditError.value = '意图名称不能为空或重复'; return; }
        p.intent_patterns[name] = description;
      }
      profileSaving.value = true;
      profileEditError.value = '';
      try {
        await api('/api/contacts/' + draft.id + '/profile', { method: 'PUT', body: { profile: p, baseProfileJson: draft.base } });
        profileEditor.value = null;
        toast('画像已保存');
        if (route.view === 'detail' && route.id === draft.id) await loadDetail();
        loadContacts();
      } catch (e) { profileEditError.value = e.message; }
      finally { profileSaving.value = false; }
    }

    const toasts = ref([]);
    let toastSeq = 0;

    // ---------- 基础设施 ----------
    function toast(msg, type) {
      const id = ++toastSeq;
      toasts.value.push({ id, msg, type: type || '' });
      setTimeout(() => {
        const i = toasts.value.findIndex(x => x.id === id);
        if (i >= 0) toasts.value.splice(i, 1);
      }, 3000);
    }

    // api 统一走 /api/，带 Bearer Token；401 清空登录态跳回登录页
    async function api(path, opts) {
      opts = opts || {};
      opts.headers = Object.assign({}, opts.headers, {
        Authorization: 'Bearer ' + localStorage.getItem(TOKEN_KEY),
      });
      // FormData（文件上传）必须让浏览器自动设置带 boundary 的 multipart 头
      if (opts.body && typeof opts.body === 'object' &&
        !(typeof FormData !== 'undefined' && opts.body instanceof FormData)) {
        opts.headers['Content-Type'] = 'application/json';
        opts.body = JSON.stringify(opts.body);
      }
      const res = await fetch(path, opts);
      if (res.status === 401) {
        localStorage.removeItem(TOKEN_KEY);
        authed.value = false;
        throw new Error('登录已过期，请重新输入 Token');
      }
      let body = {};
      try { body = await res.json(); } catch (e) { /* 非 JSON 响应 */ }
      if (!res.ok) {
        throw new Error(body.error || ('请求失败 (' + res.status + ')'));
      }
      return body;
    }

    // authPost 登录专用：不带任何 Authorization 头（避免残留会话干扰），
    // 也不触发全局 401 跳转
    async function authPost(path, body) {
      const res = await fetch(path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      let data = {};
      try { data = await res.json(); } catch (e) { /* 非 JSON */ }
      if (!res.ok) throw new Error(data.error || ('请求失败 (' + res.status + ')'));
      return data;
    }

    function enterApp(session) {
      localStorage.setItem(TOKEN_KEY, session);
      authed.value = true;
      authStage.value = 'token';
      tokenInput.value = '';
      codeInput.value = '';
      pendingToken.value = '';
      setupSecret.value = '';
      setupOtpauth.value = '';
      setupQr.value = '';
      loginError.value = '';
      parseRoute();
    }

    function backToToken() {
      authStage.value = 'token';
      codeInput.value = '';
      loginError.value = '';
    }

    // 第一步：提交 apiToken，由后端决定下一步是首次绑定还是输动态码
    async function login() {
      const t = tokenInput.value.trim();
      if (!t) { loginError.value = '请输入 API Token'; return; }
      loginChecking.value = true;
      loginError.value = '';
      try {
        const r = await authPost('/api/auth/login', { token: t });
        if (r.stage === 'ok') { enterApp(r.session); return; }
        pendingToken.value = t;
        if (r.stage === 'setup') {
          setupSecret.value = r.secret;
          setupOtpauth.value = r.otpauth;
          setupQr.value = r.qrPng;
          authStage.value = 'setup';
        } else {
          authStage.value = '2fa';
        }
      } catch (e) {
        loginError.value = e.message;
      } finally {
        loginChecking.value = false;
      }
    }

    // 首次绑定：验证器扫码后输入 6 位码确认
    async function enable2FA() {
      const code = codeInput.value.trim();
      if (!/^\d{6}$/.test(code)) { loginError.value = '请输入验证器上的 6 位数字'; return; }
      loginChecking.value = true;
      loginError.value = '';
      try {
        const r = await authPost('/api/auth/2fa/enable', {
          token: pendingToken.value, secret: setupSecret.value, code,
        });
        if (r.stage === 'ok') enterApp(r.session);
      } catch (e) {
        loginError.value = e.message;
      } finally {
        loginChecking.value = false;
      }
    }

    // 日常登录：输入 TOTP 动态码
    async function verify2FA() {
      const code = codeInput.value.trim();
      if (!/^\d{6}$/.test(code)) { loginError.value = '请输入 6 位动态码'; return; }
      loginChecking.value = true;
      loginError.value = '';
      try {
        const r = await authPost('/api/auth/2fa/verify', {
          token: pendingToken.value, code,
        });
        if (r.stage === 'ok') enterApp(r.session);
      } catch (e) {
        loginError.value = e.message;
        codeInput.value = '';
      } finally {
        loginChecking.value = false;
      }
    }

    async function logout() {
      try { await api('/api/auth/logout', { method: 'POST' }); } catch (e) { /* 忽略，本地照样清 */ }
      stopStatusTimer();
      sysStatus.value = null;
      localStorage.removeItem(TOKEN_KEY);
      authed.value = false;
      authStage.value = 'token';
      loginError.value = '';
    }

    // ---------- 路由解析 ----------
    function parseRoute() {
      const h = location.hash;
      const m = h.match(/^#\/contact\/(\d+)$/);
      if (m) {
        const id = Number(m[1]);
        if (route.view !== 'detail' || route.id !== id) {
          resetDetail();
        }
        route.view = 'detail';
        route.id = id;
      } else if (h.startsWith('#/merges')) {
        route.view = 'merges';
        route.id = 0;
      } else if (h.startsWith('#/backup')) {
        route.view = 'backup';
        route.id = 0;
      } else if (h.startsWith('#/help')) {
        route.view = 'help';
        route.id = 0;
      } else if (h.startsWith('#/status')) {
        route.view = 'status';
        route.id = 0;
      } else {
        route.view = 'contacts';
        route.id = 0;
      }
      onRouteEnter();
    }

    function onRouteEnter() {
      stopStatusTimer();
      if (!authed.value) return;
      if (route.view === 'contacts') loadContacts();
      if (route.view === 'detail') loadDetail();
      if (route.view === 'merges') loadMergeLogs();
      if (route.view === 'backup') loadBackupLogs();
      if (route.view === 'status') {
        loadStatus();
        statusTimer = setInterval(loadStatus, 5000);
      }
    }

    function resetDetail() {
      contact.value = null;
      detailTab.value = 'profile';
      messages.value = [];
      messagesHasMore.value = false;
      history.value = [];
      expandedHistory.value = 0;
      stats.value = null;
      showRemark.value = false;
      showSupplement.value = false;
      showMerge.value = false;
      showDelete.value = false;
    }

    function gotoDetail(id) {
      location.hash = '#/contact/' + id;
    }

    // ---------- 联系人列表 ----------
    // 分页加载：不再一次性取全量，避免联系人多了首屏卡顿。
    // 搜索走后端：本地 filter 只能搜已加载的页，会漏掉后面的联系人。
    function buildContactsQuery(offset) {
      const p = new URLSearchParams();
      p.set('paged', '1');
      p.set('offset', String(offset));
      p.set('limit', String(contactsLimit));
      if (showMerged.value) p.set('includeMerged', '1');
      if (searchApplied.value) p.set('q', searchApplied.value);
      return '?' + p.toString();
    }
    async function loadContacts(reset) {
      if (reset !== false) {
        contactsOffset.value = 0;
      }
      loadingContacts.value = true;
      try {
        // 兜底成 []：后端空列表若返回 null，ref 变成 null，
        // 模板里 .length 会抛 TypeError 导致整页白屏
        const out = await api('/api/contacts' + buildContactsQuery(contactsOffset.value));
        const list = (out && out.list) || [];
        if (contactsOffset.value === 0) {
          contacts.value = list;
        } else {
          contacts.value = contacts.value.concat(list);
        }
        contactsTotal.value = (out && out.total) || 0;
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        loadingContacts.value = false;
      }
    }
    function loadMoreContacts() {
      if (loadingContacts.value || contacts.value.length >= contactsTotal.value) return;
      contactsOffset.value = contacts.value.length;
      loadContacts(false);
    }
    function applySearch() {
      searchApplied.value = search.value.trim();
      loadContacts();
    }
    function clearSearch() {
      search.value = '';
      searchApplied.value = '';
      loadContacts();
    }
    const contactsHasMore = computed(() => contacts.value.length < contactsTotal.value);

    // displayName：有备注时显示「备注（昵称）」
    function displayName(c) {
      return c.remark ? c.remark + '（' + c.name + '）' : c.name;
    }

    // ---------- 联系人详情 ----------
    async function loadDetail() {
      loadingDetail.value = true;
      try {
        contact.value = await api('/api/contacts/' + route.id);
        if (detailTab.value === 'messages' && !messages.value.length) loadMessages(false);
        if (detailTab.value === 'history') loadHistory();
        if (detailTab.value === 'stats') loadStats();
      } catch (e) {
        toast(e.message, 'error');
        contact.value = null;
      } finally {
        loadingDetail.value = false;
      }
    }

    function switchTab(tab) {
      detailTab.value = tab;
      if (tab === 'messages' && !messages.value.length) loadMessages(false);
      if (tab === 'history' && !history.value.length) loadHistory();
      if (tab === 'stats' && !stats.value) loadStats();
    }

    // 画像按桌面端 profile.go 固定分节铺开
    const profileSections = computed(() => buildSections(contact.value && contact.value.profileJson));

    // asArr 把画像字段统一成数组。
    // LLM 正常输出数组，但历史快照/旧版本/手动补充合并后这些字段可能是字符串，
    // 直接 .map 会抛 TypeError 导致整个画像（含历史展开）渲染失败，必须兜底
    const asArr = (v) => {
      if (Array.isArray(v)) return v.filter(x => x !== null && x !== '');
      if (v == null || v === '') return [];
      return [String(v)];
    };

    function buildSections(pj) {
      if (!pj || pj === '{}') return [];
      let p;
      try { p = JSON.parse(pj); } catch (e) { return []; }
      const secs = [];
      const push = (title, lines) => {
        if (lines && lines.length) secs.push({ title, lines });
      };
      push('概要', p.summary ? [p.summary] : []);
      const bi = (p.basic_info && typeof p.basic_info === 'object') ? p.basic_info : {};
      const biLines = [];
      if (bi.occupation) biLines.push('职业：' + bi.occupation);
      if (bi.location) biLines.push('所在地：' + bi.location);
      const dates = asArr(bi.important_dates);
      if (dates.length) {
        biLines.push('重要日子：');
        dates.forEach(d => biLines.push('- ' + d));
      }
      push('基本信息', biLines);
      push('性格特征', asArr(p.personality).map(x => '- ' + x));
      const cs = (p.communication_style && typeof p.communication_style === 'object') ? p.communication_style : {};
      const csLines = [];
      if (cs.reply_length) csLines.push('回复长短：' + cs.reply_length);
      if (cs.tone) csLines.push('语气：' + cs.tone);
      const phrases = asArr(cs.frequent_phrases);
      if (phrases.length) {
        csLines.push('口头禅：');
        phrases.forEach(x => csLines.push('- ' + x));
      }
      if (cs.emoji_usage) csLines.push('表情使用：' + cs.emoji_usage);
      if (cs.initiative) csLines.push('主动程度：' + cs.initiative);
      push('沟通风格', csLines);
      push('兴趣爱好', asArr(p.interests).map(x => '- ' + x));
      const ep = (p.emotional_patterns && typeof p.emotional_patterns === 'object') ? p.emotional_patterns : {};
      const epLines = [];
      const stressors = asArr(ep.stressors);
      if (stressors.length) {
        epLines.push('压力源：');
        stressors.forEach(x => epLines.push('- ' + x));
      }
      const comforts = asArr(ep.comfort_topics);
      if (comforts.length) {
        epLines.push('安慰有效话题：');
        comforts.forEach(x => epLines.push('- ' + x));
      }
      if (ep.when_upset) epLines.push('不高兴时的表现：' + ep.when_upset);
      push('情绪模式', epLines);
      const rel = (p.relationship && typeof p.relationship === 'object') ? p.relationship : {};
      const relLines = [];
      if (rel.closeness) relLines.push('亲密程度：' + rel.closeness);
      const events = asArr(rel.recent_events);
      if (events.length) {
        relLines.push('近期共同事件：');
        events.forEach(x => relLines.push('- ' + x));
      }
      if (rel.interaction_pattern) relLines.push('互动模式：' + rel.interaction_pattern);
      push('关系', relLines);
      if (p.intent_patterns && typeof p.intent_patterns === 'object' && !Array.isArray(p.intent_patterns)
        && Object.keys(p.intent_patterns).length) {
        const ipLines = Object.keys(p.intent_patterns).sort()
          .map(k => '- ' + k + '：' + p.intent_patterns[k]);
        push('典型意图', ipLines);
      }
      push('重要事实', asArr(p.important_facts).map(x => '- ' + x));
      return secs;
    }

    // ---------- 消息 ----------
    async function loadMessages(more) {
      messagesLoading.value = true;
      try {
        const offset = more ? messages.value.length : 0;
        const list = (await api('/api/contacts/' + route.id + '/messages?offset=' + offset + '&limit=50')) || [];
        if (more) messages.value = messages.value.concat(list);
        else messages.value = list;
        messagesHasMore.value = list.length === 50;
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        messagesLoading.value = false;
      }
    }

    // ---------- 画像历史 ----------
    async function loadHistory() {
      try {
        history.value = (await api('/api/contacts/' + route.id + '/history?limit=100')) || [];
      } catch (e) {
        toast(e.message, 'error');
      }
    }

    function toggleHistory(h) {
      expandedHistory.value = expandedHistory.value === h.ID ? 0 : h.ID;
    }

    function historySections(h) {
      return buildSections(h.ProfileJSON);
    }

    // 回滚：现有 API 没有专门的回滚端点，通过 supplement 让 LLM 以历史快照为准覆盖当前画像
    async function rollback(h) {
      if (!confirm('确定把画像回滚到 ' + fmtTime(h.CreatedAt) + ' 的版本吗？\n将通过「补充画像」接口让大模型以该历史快照覆盖当前画像。')) return;
      busy.value = true;
      try {
        await api('/api/contacts/' + route.id + '/supplement', {
          method: 'POST',
          body: { note: '请把画像整体恢复为以下历史版本，以该 JSON 内容为准，忽略与之冲突的现有信息：\n' + h.ProfileJSON },
        });
        toast('已回滚到该版本');
        expandedHistory.value = 0;
        await loadDetail();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        busy.value = false;
      }
    }

    // ---------- 统计 ----------
    async function loadStats() {
      try {
        stats.value = await api('/api/contacts/' + route.id + '/stats');
      } catch (e) {
        toast(e.message, 'error');
      }
    }

    // ---------- 操作：备注 / 补充 / 合并 / 删除 ----------
    function startRemark() {
      remarkInput.value = contact.value ? contact.value.remark : '';
      showRemark.value = true;
    }

    async function doSetRemark() {
      busy.value = true;
      try {
        await api('/api/contacts/' + route.id + '/remark', {
          method: 'POST',
          body: { remark: remarkInput.value.trim() },
        });
        toast('备注已更新');
        showRemark.value = false;
        loadDetail();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        busy.value = false;
      }
    }

    async function doSupplement() {
      busy.value = true;
      try {
        await api('/api/contacts/' + route.id + '/supplement', {
          method: 'POST',
          body: { note: supplementNote.value.trim() },
        });
        toast('画像已补充');
        showSupplement.value = false;
        loadDetail();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        busy.value = false;
      }
    }

    async function doRegenerate() {
      if (!confirm('将基于该联系人的全部消息重新生成画像（调用大模型，可能要等一两分钟），继续？')) return;
      busy.value = true;
      try {
        await api('/api/contacts/' + route.id + '/regenerate', { method: 'POST' });
        toast('画像已重新生成');
        loadDetail();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        busy.value = false;
      }
    }

    // 关联昵称：候选为除当前联系人外的未合并联系人
    const mergeCandidates = ref([]);
    async function startMerge() {
      try {
        const list = (await api('/api/contacts')) || [];
        mergeCandidates.value = list.filter(c => c.id !== route.id);
        mergeSourceId.value = 0;
        mergeUseSourceName.value = false;
        mergeRegenerate.value = true;
        showMerge.value = true;
      } catch (e) {
        toast(e.message, 'error');
      }
    }

    async function doMerge() {
      if (!mergeSourceId.value) return;
      const src = mergeCandidates.value.find(c => c.id === mergeSourceId.value);
      if (!confirm('确定把「' + (src ? displayName(src) : '') + '」并入「' + displayName(contact.value) + '」吗？')) return;
      busy.value = true;
      try {
        await api('/api/merge', {
          method: 'POST',
          body: {
            sourceId: mergeSourceId.value,
            targetId: route.id,
            useSourceName: mergeUseSourceName.value,
            regenerate: mergeRegenerate.value,
          },
        });
        toast(mergeRegenerate.value ? '已合并，画像正在后台重新生成' : '已合并');
        showMerge.value = false;
        loadDetail();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        busy.value = false;
      }
    }

    // 删除：先弹层说明，再确认（二次确认）
    function doDelete() {
      showDelete.value = true;
    }

    async function confirmDelete() {
      busy.value = true;
      try {
        await api('/api/contacts/' + route.id, { method: 'DELETE' });
        toast('联系人已删除');
        showDelete.value = false;
        location.hash = '#/';
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        busy.value = false;
      }
    }

    // ---------- 合并记录 ----------
    async function loadMergeLogs() {
      loadingMerges.value = true;
      try {
        mergeLogs.value = (await api('/api/merge/logs?limit=100')) || [];
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        loadingMerges.value = false;
      }
    }

    async function undoMerge(l) {
      if (!confirm('确定撤销「' + l.SourceName + ' → ' + l.TargetName + '」的合并吗？\n消息和别名将退回原联系人。')) return;
      busy.value = true;
      try {
        await api('/api/merge/undo', { method: 'POST', body: { logId: l.ID } });
        toast('已撤销合并');
        loadMergeLogs();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        busy.value = false;
      }
    }

    // ---------- 备份 / 恢复 ----------
    async function loadBackupLogs() {
      try {
        backupLogs.value = (await api('/api/backup/logs')) || [];
      } catch (e) {
        backupLogs.value = [];
      }
    }

    async function exportBackup() {
      const pwd = encPassword.value.trim();
      if (pwd && !confirm('将用密码加密备份中的密钥文件（模型 Key、微信登录凭据、2FA 密钥）。\n\n' +
        '· 密码忘了这些文件就再也解不开，程序不会保存密码\n' +
        '· 聊天数据仍是明文，zip 可正常打开查看\n\n确定继续吗？')) return;
      backupBusy.value = 'export';
      backupResult.value = '';
      try {
        const opt = { headers: { Authorization: 'Bearer ' + localStorage.getItem(TOKEN_KEY) } };
        if (pwd) {
          // 口令走请求体而不是查询串，避免落进浏览器历史和访问日志
          opt.method = 'POST';
          opt.headers['Content-Type'] = 'application/json';
          opt.body = JSON.stringify({ password: pwd });
        }
        const res = await fetch('/api/backup/export', opt);
        if (res.status === 401) {
          localStorage.removeItem(TOKEN_KEY);
          authed.value = false;
          return;
        }
        if (!res.ok) {
          let msg = '导出失败 (' + res.status + ')';
          try { msg = (await res.json()).error || msg; } catch (e) {}
          throw new Error(msg);
        }
        // 文件名优先取 Content-Disposition
        let name = 'wechat-profile-backup.zip';
        const cd = res.headers.get('Content-Disposition') || '';
        const m = cd.match(/filename\*=UTF-8''([^;]+)/i) || cd.match(/filename="?([^";]+)"?/i);
        if (m) { try { name = decodeURIComponent(m[1]); } catch (e) { name = m[1]; } }
        const blob = await res.blob();
        const a = document.createElement('a');
        a.href = URL.createObjectURL(blob);
        a.download = name;
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(a.href), 10000);
        const mb = (blob.size / 1024 / 1024).toFixed(1);
        backupResult.value = '已导出：' + name + '（' + mb + ' MB）' +
          (pwd ? '，密钥文件已用密码加密，导入时需提供同一密码' : '');
        loadBackupLogs();
      } catch (e) {
        backupResult.value = '导出失败：' + e.message;
      } finally {
        backupBusy.value = false;
      }
    }

    function pickImport() {
      backupResult.value = '';
      // 每次重新选择同一个文件也要触发 change
      if (backupFile.value) backupFile.value.value = '';
      backupFile.value && backupFile.value.click();
    }

    async function importBackup(ev) {
      const file = ev.target.files && ev.target.files[0];
      if (!file) return;
      if (!confirm('确定用「' + file.name + '」恢复吗？\n\n' +
        '· 当前所有联系人、消息、画像数据将被整体替换\n' +
        '· 系统会自动在服务器留一份恢复前备份\n' +
        '· 配置/登录凭据恢复后需重启服务生效\n' +
        '· 若这份备份导出时设过密码，请先在下方「备份密码」里填上')) {
        ev.target.value = '';
        return;
      }
      backupBusy.value = 'import';
      backupResult.value = '正在上传并恢复，请稍候（大文件可能需要一两分钟）…';
      try {
        const fd = new FormData();
        fd.append('file', file);
        const pwd = impPassword.value.trim();
        if (pwd) fd.append('password', pwd);
        const r = await api('/api/backup/import', { method: 'POST', body: fd });
        backupResult.value =
          '恢复完成：联系人 ' + r.contacts + '、消息 ' + r.messages +
          '、画像历史 ' + r.histories + '、合并记录 ' + r.mergeLogs +
          (r.files && r.files.length ? '\n已恢复配置文件：' + r.files.join('、') + '，需重启服务生效' : '') +
          (r.safetyBackup ? '\n恢复前自动备份：' + r.safetyBackup : '');
        toast('恢复完成');
        loadContacts();
        loadBackupLogs();
      } catch (e) {
        backupResult.value = '恢复失败：' + e.message;
      } finally {
        backupBusy.value = false;
        ev.target.value = '';
      }
    }

    // ---------- 服务器状态 ----------
    const sysStatus = ref(null);
    const statusLoading = ref(false);
    let statusTimer = null;
    function stopStatusTimer() {
      if (statusTimer) { clearInterval(statusTimer); statusTimer = null; }
    }
    async function loadStatus() {
      statusLoading.value = true;
      try {
        sysStatus.value = await api('/api/status');
      } catch (e) {
        if (route.view === 'status') toast(e.message, 'error');
      } finally {
        statusLoading.value = false;
      }
    }
    // 秒数转「x天 x小时 x分」
    function fmtUptime(sec) {
      if (sec == null || sec < 0) return '—';
      const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
      return (d ? d + ' 天 ' : '') + (d || h ? h + ' 小时 ' : '') + m + ' 分';
    }
    // “N 秒前”；-1 表示从未发生
    function fmtAgo(sec) {
      if (sec == null || sec < 0) return '—';
      if (sec < 5) return '刚刚';
      if (sec < 60) return sec + ' 秒前';
      if (sec < 3600) return Math.floor(sec / 60) + ' 分钟前';
      if (sec < 86400) return Math.floor(sec / 3600) + ' 小时前';
      return Math.floor(sec / 86400) + ' 天前';
    }
    // MB 转可读容量
    function fmtMB(mb) {
      if (mb == null) return '—';
      return mb >= 1024 ? (mb / 1024).toFixed(1) + ' GB' : mb + ' MB';
    }
    // 仪表条配色：<70 绿、<90 黄、否则红
    function pctClass(p) { return p >= 90 ? 'lv-bad' : (p >= 70 ? 'lv-warn' : 'lv-ok'); }

    // ---------- 工具 ----------
    // 时间显示：兼容 RFC3339（2026-10-01T14:45:54+08:00）和纯文本日期
    function fmtTime(s) {
      if (!s) return '';
      let d = new Date(s);
      if (isNaN(d.getTime())) d = new Date(String(s).replace(' ', 'T'));
      if (isNaN(d.getTime())) return s;
      const pad = n => String(n).padStart(2, '0');
      return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
        ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
    }

    onMounted(() => {
      window.addEventListener('hashchange', parseRoute);
      parseRoute();
      // 本地存的是网页会话令牌（7 天有效），启动时向后端验真，过期/被吊销就回登录页
      (async () => {
        if (!localStorage.getItem(TOKEN_KEY)) return;
        try {
          await api('/api/status');
          authed.value = true;
          parseRoute();
        } catch (e) {
          localStorage.removeItem(TOKEN_KEY);
        }
      })();
    });
    onUnmounted(() => { window.removeEventListener('hashchange', parseRoute); stopStatusTimer(); });

    return {
      assist, styles, copyAssist, reviewDraft, analyzeReplies, rewriteReply, loadChanges, closeChanges,
      profileEditor, profileSaving, profileEditError, profileFields, startProfileEdit, saveProfileEdit,
      authed, tokenInput, loginChecking, loginError, login, logout,
      authStage, codeInput, setupSecret, setupOtpauth, setupQr,
      enable2FA, verify2FA, backToToken,
      route, contacts, contactsTotal, contactsHasMore, loadingContacts, search, searchApplied, showMerged,
      loadContacts, loadMoreContacts, applySearch, clearSearch,
      contact, loadingDetail, detailTab, messages, messagesLoading, messagesHasMore,
      history, expandedHistory, stats, busy, profileSections,
      mergeLogs, loadingMerges, mergeCandidates,
      backupBusy, backupResult, backupFile, backupLogs, exportBackup, pickImport, importBackup,
      encPassword, impPassword, showEncPwd,
      showRemark, remarkInput, showSupplement, supplementNote,
      showMerge, mergeSourceId, mergeUseSourceName, mergeRegenerate, showDelete,
      toasts,
      loadContacts, gotoDetail, displayName, loadDetail, switchTab, loadMessages,
      toggleHistory, historySections, rollback,
      startRemark, doSetRemark, doSupplement, doRegenerate,
      startMerge, doMerge, doDelete, confirmDelete,
      loadMergeLogs, undoMerge, fmtTime,
      sysStatus, statusLoading, loadStatus, fmtUptime, fmtAgo, fmtMB, pctClass,
    };
  },
}).mount('#app');
