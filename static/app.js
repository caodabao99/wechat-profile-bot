/* 微信画像管理前端逻辑：Vue 3（global build），无构建步骤 */
/* global Vue */
const { createApp, ref, reactive, computed, onMounted, onUnmounted, nextTick } = Vue;

// 画像分节定义：查看页（buildSections）与编辑弹窗共用同一份，
// 保证「看到的字段/顺序/叫法」和「编辑时」完全一致，编辑器不许自造另一套字段。
// kind: text=单值（label 为空时整节就是一段文字，如概要）；list=每行一项；map=名称：描述。
const PROFILE_SCHEMA = [
  { title: '概要', fields: [
    { path: 'summary', kind: 'text', hint: '一段话概括' },
  ]},
  { title: '基本信息', fields: [
    { path: 'basic_info.occupation', label: '职业', kind: 'text' },
    { path: 'basic_info.location', label: '所在地', kind: 'text' },
    { path: 'basic_info.important_dates', label: '重要日子', kind: 'list' },
  ]},
  { title: '性格特征', fields: [
    { path: 'personality', kind: 'list', hint: '每行一项' },
  ]},
  { title: '沟通风格', fields: [
    { path: 'communication_style.reply_length', label: '回复长短', kind: 'text' },
    { path: 'communication_style.tone', label: '语气', kind: 'text' },
    { path: 'communication_style.frequent_phrases', label: '口头禅', kind: 'list' },
    { path: 'communication_style.emoji_usage', label: '表情使用', kind: 'text' },
    { path: 'communication_style.initiative', label: '主动程度', kind: 'text' },
  ]},
  { title: '兴趣爱好', fields: [
    { path: 'interests', kind: 'list', hint: '每行一项' },
  ]},
  { title: '情绪模式', fields: [
    { path: 'emotional_patterns.stressors', label: '压力源', kind: 'list' },
    { path: 'emotional_patterns.comfort_topics', label: '安慰有效话题', kind: 'list' },
    { path: 'emotional_patterns.when_upset', label: '不高兴时的表现', kind: 'text' },
  ]},
  { title: '关系', fields: [
    { path: 'relationship.closeness', label: '亲密程度', kind: 'text' },
    { path: 'relationship.recent_events', label: '近期共同事件', kind: 'list' },
    { path: 'relationship.interaction_pattern', label: '互动模式', kind: 'text' },
  ]},
  { title: '典型意图', fields: [
    { path: 'intent_patterns', kind: 'map', hint: '名称 + 典型表现' },
  ]},
  { title: '重要事实', fields: [
    { path: 'important_facts', kind: 'list', hint: '每行一项' },
  ]},
];

const profileGetPath = (obj, path) => path.split('.').reduce((o, key) => (o == null ? o : o[key]), obj);

createApp({
  setup() {
    // ---------- 登录态（Token + TOTP 两步登录） ----------
    const TOKEN_KEY = 'wp_api_token'; // 登录成功后这里存的是「网页会话令牌」，不再存 apiToken
    const TRUSTED_KEY = 'wp_trusted_token'; // 可信客户端长效令牌（90 天免登录）
    const SKIP_TRUSTED_KEY = 'wp_skip_trusted'; // 本标签页登出后不再自动免登录
    const authed = ref(false);
    const tokenInput = ref('');
    const trustDevice = ref(true); // 登录页「信任此设备」勾选框，默认勾选
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
      return list.slice(0, 4).map((r, i) => {
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

    // ---------- 关系助手 ----------
    const asst = ref(null);        // 看板聚合数据
    const asstLoading = ref(false);
    const asstBusy = ref('');      // 'save' | 'test' | 'daily'
    const asstForm = ref({
      enabled: false, remindBirthday: true, remindCooling: true,
      birthdayAdvanceDays: 3, coolingDays: 7, dailyCheckTime: '08:00',
      emotionAlert: true, emotionDailyMax: 10,
      // 增值功能：待跟进提醒 + AI 祝福草稿。自动抽取和祝福草稿都要调模型，默认关闭
      remindFollowup: true, followupEnabled: false,
      followupDailyMax: 8, followupWindowDays: 30, blessingDraft: false,
      // 每周维护计划：默认开，关掉后调度器不再每周一自动生成
      weeklyPlanEnabled: true,
      // 人生模拟器：默认开，纯 SQL 零模型开销，关掉后不再自动重算人生状态/推演
      lifeSimEnabled: true,
      // 高阶洞察四件套（社交网络/自我画像/干预学习/本周简报）：默认开，纯确定性零模型
      advancedInsightsEnabled: true,
      // 日历订阅密钥：后端只回传打码值，保存时原样带回，避免把库里的真密钥冲掉
      calendarKey: '',
      smtp: { host: '', port: 465, ssl: true, user: '', pass: '', from: '', toText: '' },
    });

    async function loadAssistant() {
      if (asstLoading.value) return;
      asstLoading.value = true;
      try {
        const data = await api('/api/assistant/dashboard');
        asst.value = data;
        const st = data.settings || {};
        const f = asstForm.value;
        f.enabled = !!st.enabled;
        f.remindBirthday = st.remindBirthday !== false;
        f.remindCooling = st.remindCooling !== false;
        f.birthdayAdvanceDays = st.birthdayAdvanceDays || 3;
        f.coolingDays = st.coolingDays || 7;
        f.dailyCheckTime = st.dailyCheckTime || '08:00';
        f.emotionAlert = st.emotionAlert !== false;
        f.emotionDailyMax = st.emotionDailyMax || 10;
        f.remindFollowup = st.remindFollowup !== false;
        f.followupEnabled = !!st.followupEnabled;
        f.followupDailyMax = st.followupDailyMax || 8;
        f.followupWindowDays = st.followupWindowDays || 30;
        f.blessingDraft = !!st.blessingDraft;
        f.weeklyPlanEnabled = st.weeklyPlanEnabled !== false; // 默认开（opt-out）
        f.lifeSimEnabled = st.lifeSimEnabled !== false; // 默认开（opt-out）
        f.advancedInsightsEnabled = st.advancedInsightsEnabled !== false; // 默认开（opt-out）
        f.calendarKey = st.calendarKey || '';
        const sm = st.smtp || {};
        f.smtp = {
          host: sm.host || '', port: sm.port || 465, ssl: sm.ssl !== false,
          user: sm.user || '', pass: sm.pass || '', from: sm.from || '',
          toText: (sm.to || []).join(', '),
        };
        // 看板之外的两块增值数据：待跟进列表 + 日历订阅密钥
        loadFollowups();
        loadCalendarKey();
        loadWeeklyPlan();
      } catch (e) { toast(e.message, 'error'); }
      finally { asstLoading.value = false; }
    }

    // ---------- 本周维护计划 ----------
    const weeklyPlan = ref({ items: [], stats: null, generatedAt: '' });
    const weeklyPlanBusy = ref(false);

    const weeklyKinds = { cooling: '互动降温', silence: '久未联系', no_reply: '对方消息待回' };
    function weeklyKindLabel(k) { return weeklyKinds[k] || k; }

    async function loadWeeklyPlan() {
      try {
        const data = await api('/api/assistant/weekly-plan');
        weeklyPlan.value = {
          items: data.items || [],
          stats: data.stats || null,
          generatedAt: data.generatedAt || '',
        };
      } catch (e) { /* 静默：周计划卡片加载失败不打断看板 */ }
    }

    async function regenWeeklyPlan() {
      if (weeklyPlanBusy.value) return;
      weeklyPlanBusy.value = true;
      try {
        const out = await api('/api/assistant/weekly-plan', { method: 'POST' });
        toast(out.msg || '周计划已在后台重新生成');
        // 生成要调模型，几秒后自动刷一次
        setTimeout(loadWeeklyPlan, 8000);
      } catch (e) { toast(e.message, 'error'); }
      finally { setTimeout(() => { weeklyPlanBusy.value = false; }, 3000); }
    }

    // numField 校验 type=number + v-model.number 的输入框。
    // 用户把框里的数字删空时 Vue 交上来的是空串 ''，直接 PUT 会让后端
    // json 反序列化 int 失败，返回一句笼统的「请求体解析失败」，
    // 完全看不出是哪个框出了问题。这里在发请求前拦下并指名道姓。
    // 上下界与后端 assistant.go / archive.go 的校验保持一致。
    function numField(label, v, min, max) {
      const n = (v === '' || v === null || v === undefined) ? NaN : Number(v);
      if (!Number.isInteger(n)) return { ok: false, msg: label + '必须是整数' };
      if (n < min || n > max) return { ok: false, msg: label + '必须在 ' + min + '~' + max + ' 之间' };
      return { ok: true, v: n };
    }

    async function saveAssistantSettings() {
      if (asstBusy.value) return;
      asstBusy.value = 'save';
      try {
        const f = asstForm.value;
        const nums = {};
        const spec = [
          ['birthdayAdvanceDays', '生日提前天数', f.birthdayAdvanceDays, 1, 30],
          ['coolingDays', '久未联系天数', f.coolingDays, 1, 365],
          ['emotionDailyMax', '情绪预警每日人数', f.emotionDailyMax, 1, 50],
          ['followupWindowDays', '待跟进回溯天数', f.followupWindowDays, 1, 180],
          ['followupDailyMax', '待跟进每日人数', f.followupDailyMax, 1, 30],
          ['smtpPort', 'SMTP 端口', f.smtp.port, 1, 65535],
        ];
        for (const [key, label, val, min, max] of spec) {
          const r = numField(label, val, min, max);
          if (!r.ok) { toast(r.msg, 'error'); return; }
          nums[key] = r.v;
        }
        const smtp = {
          host: f.smtp.host, port: nums.smtpPort, ssl: f.smtp.ssl,
          user: f.smtp.user, pass: f.smtp.pass, from: f.smtp.from,
          to: f.smtp.toText.split(/[,，;；\s]+/).map(s => s.trim()).filter(Boolean),
        };
        await api('/api/assistant/settings', {
          method: 'PUT',
          body: {
            enabled: f.enabled, remindBirthday: f.remindBirthday, remindCooling: f.remindCooling,
            birthdayAdvanceDays: nums.birthdayAdvanceDays, coolingDays: nums.coolingDays,
            dailyCheckTime: f.dailyCheckTime,
            emotionAlert: f.emotionAlert, emotionDailyMax: nums.emotionDailyMax,
            remindFollowup: f.remindFollowup, followupEnabled: f.followupEnabled,
            followupDailyMax: nums.followupDailyMax, followupWindowDays: nums.followupWindowDays,
            blessingDraft: f.blessingDraft, weeklyPlanEnabled: f.weeklyPlanEnabled,
            lifeSimEnabled: f.lifeSimEnabled,
            advancedInsightsEnabled: f.advancedInsightsEnabled,
            calendarKey: f.calendarKey,
            smtp,
          },
        });
        toast('设置已保存');
        await loadAssistant(); // 刷新 smtpReady 状态（密码已被后端打码回显）
      } catch (e) { toast(e.message, 'error'); }
      finally { asstBusy.value = ''; }
    }

    // refreshDashboard 只刷新看板聚合数据（含 smtpReady），不回填 asstForm。
    // 发测试邮件后需要看到 smtpReady 的变化，但绝不能顺手把用户正在编辑的
    // 十几个字段冲回服务器上的旧值——那等于「点一下测试，刚才改的全没了」。
    async function refreshDashboard() {
      try {
        asst.value = await api('/api/assistant/dashboard');
      } catch (e) { /* 静默：看板刷新失败不该打断用户正在做的编辑 */ }
    }

    async function testAssistantEmail() {
      if (asstBusy.value) return;
      asstBusy.value = 'test';
      try {
        await api('/api/assistant/test-email', { method: 'POST' });
        toast('测试邮件已发送，请检查收件箱（含垃圾邮件）');
      } catch (e) { toast(e.message, 'error'); }
      finally { asstBusy.value = ''; refreshDashboard(); }
    }

    async function runAssistantNow(kind) {
      if (asstBusy.value) return;
      asstBusy.value = kind;
      try {
        const out = await api('/api/assistant/run-now', { method: 'POST', body: { kind } });
        toast(out.msg || '任务已启动');
        // 后台任务需要点时间（情绪分析走 LLM），稍后自动刷新一次看板
        // 只刷数据不回填表单，免得把用户正在改的设置冲掉
        setTimeout(() => { if (route.view === 'assistant') { refreshDashboard(); loadFollowups(); } }, 5000);
      } catch (e) { toast(e.message, 'error'); }
      finally { asstBusy.value = ''; }
    }

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
    // 编辑器按 PROFILE_SCHEMA 的分节铺开，与画像查看页同一套字段定义。
    const profileSchema = PROFILE_SCHEMA;
    // 意图行的稳定 key。用数组下标做 :key 时，删掉中间一行会让后面所有行
    // 的 key 集体前移，Vue 复用错节点，输入框里的内容和它绑定的数据对不上。
    let intentUid = 0;
    function nextIntentUid() { return ++intentUid; }
    function addIntentRow() {
      if (!profileEditor.value) return;
      profileEditor.value.intents.push({ _uid: nextIntentUid(), name: '', description: '' });
    }

    function startProfileEdit() {
      try {
        const base = contact.value.profileJson || '';
        const p = JSON.parse(base || '{}') || {};
        const values = {};
        for (const sec of profileSchema) {
          for (const f of sec.fields) {
            if (f.kind === 'map') continue; // 典型意图单独用 name/description 行编辑
            const v = profileGetPath(p, f.path);
            if (f.kind === 'list') {
              if (Array.isArray(v)) {
                values[f.path] = v.filter(x => x !== null && x !== '').map(x => String(x)).join('\n');
              } else if (v && typeof v === 'object') {
                // important_dates 旧快照可能是 {生日: '5月1日'} 对象，转成每行「键: 值」
                values[f.path] = Object.entries(v).map(([k, val]) => k + ': ' + val).join('\n');
              } else {
                values[f.path] = v == null || v === '' ? '' : String(v);
              }
            } else {
              values[f.path] = v == null ? '' : String(v);
            }
          }
        }
        const intentObj = p.intent_patterns;
        const intents = (intentObj && typeof intentObj === 'object' && !Array.isArray(intentObj))
          ? Object.keys(intentObj).sort().map(name => ({ _uid: nextIntentUid(), name, description: String(intentObj[name] ?? '') }))
          : [];
        profileEditor.value = { id: contact.value.id, base, values, intents };
        profileEditError.value = '';
      } catch (e) { toast('画像无法解析：' + e.message, 'error'); }
    }
    async function saveProfileEdit() {
      if (profileSaving.value) return;
      const draft = profileEditor.value;
      const p = {};
      for (const sec of profileSchema) {
        for (const f of sec.fields) {
          if (f.kind === 'map') continue;
          const keys = f.path.split('.');
          let obj = p;
          for (const key of keys.slice(0, -1)) obj = obj[key] || (obj[key] = {});
          const raw = draft.values[f.path] || '';
          obj[keys[keys.length - 1]] = f.kind === 'list'
            ? raw.split('\n').map(s => s.trim()).filter(Boolean)
            : raw.trim();
        }
      }
      p.intent_patterns = Object.create(null);
      for (const row of draft.intents) {
        const name = row.name.trim(), description = row.description.trim();
        if (!name && !description) continue;
        // 不用 Object.hasOwn：它要 Chrome 93+/Safari 15.4+/Firefox 92+，
        // 旧浏览器上这里会抛 TypeError，而这行在 try 之外，
        // 表现就是点「保存画像」完全没反应、也没有任何报错。
        // intent_patterns 是 Object.create(null)，自身没有 hasOwnProperty，必须走原型上的。
        if (!name || Object.prototype.hasOwnProperty.call(p.intent_patterns, name)) { profileEditError.value = '意图名称不能为空或重复'; return; }
        p.intent_patterns[name] = description;
      }
      profileSaving.value = true;
      profileEditError.value = '';
      try {
        await api('/api/contacts/' + draft.id + '/profile', { method: 'PUT', body: { profile: p, baseProfileJson: draft.base } });
      } catch (e) { profileEditError.value = e.message; return; }
      finally { profileSaving.value = false; }
      // 走到这里说明保存已经成功，后面的刷新只是收尾。
      // 刷新失败不该再往 profileEditError 里写东西——弹窗这时已经关了，
      // 错误既看不见，又会残留到用户下次打开弹窗时。
      profileEditor.value = null;
      toast('画像已保存');
      if (route.view === 'detail' && route.id === draft.id) loadDetail();
      loadContacts();
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
        throw httpErr('登录已过期，请重新输入 Token', 401);
      }
      let body = {};
      try { body = await res.json(); } catch (e) { /* 非 JSON 响应 */ }
      if (!res.ok) {
        throw httpErr(body.error || ('请求失败 (' + res.status + ')'), res.status);
      }
      return body;
    }

    // httpErr 造一个带 HTTP 状态码的错误对象。
    // 调用方需要区分「令牌确实失效(401/403)」和「服务器挂了/断网(5xx、TypeError)」，
    // 只有前者才该清掉本地凭据，否则一次网络抖动就把用户踢下线。
    function httpErr(msg, status) {
      const e = new Error(msg);
      e.status = status;
      return e;
    }
    // isAuthRejected 判断错误是不是明确的认证/授权失败
    function isAuthRejected(e) {
      return !!e && (e.status === 401 || e.status === 403);
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
      if (!res.ok) throw httpErr(data.error || ('请求失败 (' + res.status + ')'), res.status);
      return data;
    }

    // guessDeviceName 从 UA 猜一个人类可读的设备名（可信设备列表展示用）
    function guessDeviceName() {
      const ua = navigator.userAgent;
      let os = '未知设备';
      if (/iPhone|iPad|iPod/i.test(ua)) os = 'iPhone/iPad';
      else if (/Android/i.test(ua)) os = 'Android 手机';
      else if (/Windows/i.test(ua)) os = 'Windows';
      else if (/Mac OS X/i.test(ua)) os = 'Mac';
      else if (/Linux/i.test(ua)) os = 'Linux';
      let br = '浏览器';
      if (/Edg\//i.test(ua)) br = 'Edge';
      else if (/Firefox\//i.test(ua)) br = 'Firefox';
      else if (/Chrome\//i.test(ua)) br = 'Chrome';
      else if (/Safari\//i.test(ua)) br = 'Safari';
      return os + ' · ' + br;
    }

    function enterApp(session, trustedToken) {
      localStorage.setItem(TOKEN_KEY, session);
      if (trustedToken) localStorage.setItem(TRUSTED_KEY, trustedToken);
      sessionStorage.removeItem(SKIP_TRUSTED_KEY);
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

    // trustFields 登录请求附带的「信任此设备」字段
    function trustFields() {
      return trustDevice.value
        ? { trustDevice: true, deviceName: guessDeviceName() }
        : { trustDevice: false };
    }

    // 第一步：提交 apiToken，由后端决定下一步是首次绑定还是输动态码
    async function login() {
      const t = tokenInput.value.trim();
      if (!t) { loginError.value = '请输入 API Token'; return; }
      loginChecking.value = true;
      loginError.value = '';
      try {
        const r = await authPost('/api/auth/login', Object.assign({ token: t }, trustFields()));
        if (r.stage === 'ok') { enterApp(r.session, r.trustedToken); return; }
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
        const r = await authPost('/api/auth/2fa/enable', Object.assign({
          token: pendingToken.value, secret: setupSecret.value, code,
        }, trustFields()));
        if (r.stage === 'ok') enterApp(r.session, r.trustedToken);
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
        const r = await authPost('/api/auth/2fa/verify', Object.assign({
          token: pendingToken.value, code,
        }, trustFields()));
        if (r.stage === 'ok') enterApp(r.session, r.trustedToken);
      } catch (e) {
        loginError.value = e.message;
        codeInput.value = '';
      } finally {
        loginChecking.value = false;
      }
    }

    // tryTrustedLogin 打开页面时用可信令牌静默换取会话（免输 Token+动态码）。
    // 本标签页刚登出过（SKIP_TRUSTED_KEY）则跳过；令牌失效就清掉不再试。
    async function tryTrustedLogin() {
      const tt = localStorage.getItem(TRUSTED_KEY);
      if (!tt || sessionStorage.getItem(SKIP_TRUSTED_KEY)) return false;
      try {
        const r = await authPost('/api/auth/trusted/login', { trustedToken: tt });
        if (r.stage === 'ok') { enterApp(r.session); return true; }
      } catch (e) {
        // 只有服务端明确说「令牌无效」才清除本地可信令牌。
        // 5xx / 断网 / 服务重启中一律保留，否则一次网络抖动就把免登录设备踢掉，
        // 用户下次还得重新输 Token + 动态码。
        if (isAuthRejected(e)) localStorage.removeItem(TRUSTED_KEY);
      }
      return false;
    }

    async function logout() {
      try { await api('/api/auth/logout', { method: 'POST' }); } catch (e) { /* 忽略，本地照样清 */ }
      stopStatusTimer();
      sysStatus.value = null;
      localStorage.removeItem(TOKEN_KEY);
      // 可信令牌保留（下次打开页面仍免登录），但本标签页登出后不再自动登录
      sessionStorage.setItem(SKIP_TRUSTED_KEY, '1');
      authed.value = false;
      authStage.value = 'token';
      loginError.value = '';
      // 必须整页刷新：内存里还留着上一个账号的联系人、画像、聊天记录、看板数据。
      // 只把 authed 置 false 的话，换个人登录（或多标签页里另一个人登录）会看到
      // 别人的通讯录残留在列表和详情页里，属于数据串号。
      location.hash = '#/';
      location.reload();
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
      } else if (h.startsWith('#/assistant')) {
        route.view = 'assistant';
        route.id = 0;
      } else if (h.startsWith('#/insights')) {
        route.view = 'insights';
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
      if (route.view === 'contacts') { loadContacts(); loadTags(); }
      if (route.view === 'detail') loadDetail();
      if (route.view === 'merges') loadMergeLogs();
      if (route.view === 'backup') { loadBackupLogs(); loadArchive(); loadTrusted(); }
      if (route.view === 'assistant') loadAssistant();
      if (route.view === 'insights') switchInsight(insightTab.value);
      if (route.view === 'status') {
        loadStatus();
        statusTimer = setInterval(loadStatus, 5000);
      }
    }

    // 请求序号：每发起一次加载就自增，回包时比对，过期的直接丢弃。
    // 没有它的话快速点 A→B，A 的慢响应会把 B 的画像/聊天记录覆盖掉，
    // 而保存类操作（备注、标签、事件、合并、删除）用的都是「当前」route.id，
    // 用户看着 A 的资料操作，实际写进了 B —— 属于会静默损坏数据的竞态。
    let detailSeq = 0;
    let contactsSeq = 0;

    function resetDetail() {
      detailSeq++;   // 作废所有在途的详情页请求
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
      timeline.value = [];
      tagEditOpen.value = false;
      // 弹窗与忙碌态也要一起收掉。addEvent/delEvent/saveTagEdit 用的都是「当前」route.id，
      // 弹窗开着时切换联系人，事件会被写到新联系人名下还提示保存成功；
      // busy 卡在 true 会让整个详情页的按钮永久禁用。
      showEventModal.value = false;
      eventForm.title = '';
      eventForm.detail = '';
      eventForm.eventTime = '';
      showFollowupModal.value = false;
      profileEditor.value = null;
      // 预演也要一起清：它整场对话都在内存里，不清的话上一个人的演练
      // 会原样挂在新联系人的「预演」页签下，还能继续往下发
      rh.scene = '';
      rh.first = 'me';
      rh.input = '';
      rh.turns = [];
      rh.ctx = null;
      rh.review = null;
      rh.started = false;
      rh.ctxFailed = false;
      // 驾驶舱同样按联系人隔离：不清就会把上一个人的事实/趋势/推演结果挂到新页面
      resetCockpit();
      busy.value = false;
      timelineBusy.value = false;
      rh.busy = false;
      rh.reviewBusy = false;
      rh.ctxBusy = false;
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
      // 标签筛选：后端多个标签是「且」的关系
      if (filterTagIds.value.length) p.set('tags', filterTagIds.value.join(','));
      return '?' + p.toString();
    }
    async function loadContacts(reset) {
      const my = ++contactsSeq;
      if (reset !== false) {
        contactsOffset.value = 0;
      }
      loadingContacts.value = true;
      try {
        // 兜底成 []：后端空列表若返回 null，ref 变成 null，
        // 模板里 .length 会抛 TypeError 导致整页白屏
        const out = await api('/api/contacts' + buildContactsQuery(contactsOffset.value));
        if (my !== contactsSeq) return;   // 已被更新的搜索/筛选取代，丢弃
        const list = (out && out.list) || [];
        if (contactsOffset.value === 0) {
          contacts.value = list;
          picked.value = []; // 列表换了一批，之前的勾选不再有意义
        } else {
          contacts.value = contacts.value.concat(list);
        }
        contactsTotal.value = (out && out.total) || 0;
      } catch (e) {
        if (my !== contactsSeq) return;
        toast(e.message, 'error');
      } finally {
        if (my === contactsSeq) loadingContacts.value = false;
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
      const my = ++detailSeq;
      loadingDetail.value = true;
      try {
        const c = await api('/api/contacts/' + route.id);
        if (my !== detailSeq) return;   // 已经切到别的联系人了，这份数据作废
        contact.value = c;
        if (detailTab.value === 'messages' && !messages.value.length) loadMessages(false);
        if (detailTab.value === 'history') loadHistory();
        if (detailTab.value === 'stats') loadStats();
        if (detailTab.value === 'timeline' && !timeline.value.length) loadTimeline();
      } catch (e) {
        if (my !== detailSeq) return;
        toast(e.message, 'error');
        contact.value = null;
      } finally {
        if (my === detailSeq) loadingDetail.value = false;
      }
    }

    function switchTab(tab) {
      detailTab.value = tab;
      if (tab === 'messages' && !messages.value.length) loadMessages(false);
      if (tab === 'history' && !history.value.length) loadHistory();
      if (tab === 'stats' && !stats.value) loadStats();
      if (tab === 'timeline' && !timeline.value.length) loadTimeline();
      if (tab === 'rehearsal' && !rh.ctx && !rh.ctxBusy) loadRehearsalContext();
      if (tab === 'cockpit' && !ck.loaded) loadCockpit();
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
      for (const sec of PROFILE_SCHEMA) {
        const lines = [];
        for (const f of sec.fields) {
          const v = profileGetPath(p, f.path);
          if (f.kind === 'map') {
            if (v && typeof v === 'object' && !Array.isArray(v)) {
              Object.keys(v).sort().forEach(k => {
                lines.push('- ' + k + '：' + (v[k] == null ? '' : v[k]));
              });
            }
          } else if (f.kind === 'list') {
            const arr = asArr(v);
            if (!arr.length) continue;
            // 有标签的列表（重要日子/口头禅等）先出一行小标题；性格特征/兴趣爱好这类整节即列表的直接列项
            if (f.label) lines.push(f.label + '：');
            arr.forEach(x => lines.push('- ' + x));
          } else {
            const s = v == null ? '' : String(v).trim();
            if (!s) continue;
            lines.push(f.label ? f.label + '：' + s : s);
          }
        }
        if (lines.length) secs.push({ title: sec.title, lines });
      }
      return secs;
    }

    // ---------- 消息 ----------
    async function loadMessages(more) {
      const my = detailSeq;   // 跟随当前详情页；期间切人则本次回包作废
      messagesLoading.value = true;
      try {
        // keyset 分页：首屏不带 beforeId；“加载更多”用当前已加载最旧一条的 id 作游标，
        // 避免 OFFSET 深翻页全表扫描（接口按 id DESC 返回，列表末尾即最旧）。
        let url = '/api/contacts/' + route.id + '/messages?limit=50';
        if (more && messages.value.length && messages.value[messages.value.length - 1].id) {
          url += '&beforeId=' + messages.value[messages.value.length - 1].id;
        }
        const list = (await api(url)) || [];
        if (my !== detailSeq) return;
        if (more) messages.value = messages.value.concat(list);
        else messages.value = list;
        messagesHasMore.value = list.length === 50;
      } catch (e) {
        if (my !== detailSeq) return;
        toast(e.message, 'error');
      } finally {
        if (my === detailSeq) messagesLoading.value = false;
      }
    }

    // ---------- 画像历史 ----------
    async function loadHistory() {
      const my = detailSeq;
      try {
        const out = (await api('/api/contacts/' + route.id + '/history?limit=100')) || [];
        if (my !== detailSeq) return;
        history.value = out;
      } catch (e) {
        if (my !== detailSeq) return;
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
      const my = detailSeq;
      try {
        const out = await api('/api/contacts/' + route.id + '/stats');
        if (my !== detailSeq) return;
        stats.value = out;
      } catch (e) {
        if (my !== detailSeq) return;
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
        // 必须出声：静默置空会让「读取失败」和「还没有备份记录」长得一模一样，
        // 用户以为备份没跑过，实际是接口挂了
        toast('备份记录读取失败：' + e.message, 'error');
      }
    }

    // ---------- 消息归档 ----------
    const archive = ref(null);   // {stats, byContact}
    const archiveBusy = ref(''); // '' / 'run' / 'restore' / 'save'
    const archiveResult = ref('');
    const archiveError = ref(false);   // 读取失败（区别于「还在读」）
    const archiveForm = reactive({ enabled: false, retentionDays: 730 });

    async function loadArchive() {
      try {
        const d = await api('/api/archive/status');
        archive.value = d;
        archiveError.value = false;
        archiveForm.enabled = !!d.stats.enabled;
        archiveForm.retentionDays = d.stats.retentionDays;
      } catch (e) {
        archive.value = null;
        archiveError.value = true;
        toast('归档状态读取失败：' + e.message, 'error');
      }
    }

    async function saveArchiveSettings() {
      archiveBusy.value = 'save';
      try {
        // 同 saveAssistantSettings：清空输入框会送出 ''，后端解析 int 失败只会回一句
        // 「请求体解析失败」，这里先拦下来并说清是哪个字段、合法范围是多少
        const r = numField('保留天数', archiveForm.retentionDays, 30, 36500);
        if (!r.ok) { toast(r.msg, 'error'); return; }
        await api('/api/archive/settings', { method: 'POST', body: {
          enabled: archiveForm.enabled, retentionDays: r.v,
        } });
        toast('归档设置已保存');
        loadArchive();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        archiveBusy.value = '';
      }
    }

    async function runArchive() {
      // 确认框里显示的是输入框当前的值，那就必须把这个值一起发给后端。
      // 后端 days=0 时用的是「已保存」的配置：用户改了天数但没点「保存设置」
      // 就直接归档的话，弹框说 90 天、实际按 730 天跑，归档范围完全对不上。
      const chk = numField('保留天数', archiveForm.retentionDays, 30, 36500);
      if (!chk.ok) { toast(chk.msg, 'error'); return; }
      if (!confirm('将把超过 ' + chk.v + ' 天的消息移入归档表（可随时恢复）。继续吗？')) return;
      archiveBusy.value = 'run';
      archiveResult.value = '';
      try {
        const r = await api('/api/archive/run', { method: 'POST', body: { days: chk.v } });
        archiveResult.value = '本次归档 ' + r.moved + ' 条消息';
        toast(archiveResult.value);
        loadArchive();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        archiveBusy.value = '';
      }
    }

    async function restoreArchive(contactId) {
      const all = !contactId;
      if (all && !confirm('将把全部归档消息恢复到聊天记录中。继续吗？')) return;
      archiveBusy.value = 'restore';
      archiveResult.value = '';
      try {
        const r = await api('/api/archive/restore', { method: 'POST', body: { contactId: contactId || 0 } });
        let msg = '已恢复 ' + r.result.restored + ' 条消息';
        if (r.result.skipped) msg += '，' + r.result.skipped + ' 条因联系人已删除留在归档';
        archiveResult.value = msg;
        toast(msg);
        loadArchive();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        archiveBusy.value = '';
      }
    }

    // ---------- 可信设备（免登录） ----------
    const trustedList = ref(null);
    const trustedBusy = ref(false);

    async function loadTrusted() {
      try {
        const d = await api('/api/trusted/list');
        trustedList.value = d.clients || [];
      } catch (e) {
        trustedList.value = [];
        // 同上：不报错的话「读取失败」会被当成「暂无可信设备」，
        // 用户以为设备都掉线了，其实是接口挂了
        toast('可信设备列表读取失败：' + e.message, 'error');
      }
    }

    async function revokeTrusted(t) {
      if (!confirm('吊销「' + t.name + '」后，该设备下次打开页面需要重新完整登录。继续吗？')) return;
      trustedBusy.value = true;
      try {
        await api('/api/trusted/revoke', { method: 'POST', body: { token: t.tokenPrefix } });
        // 吊销的正是本机令牌时，把本地缓存也清掉
        const local = localStorage.getItem(TRUSTED_KEY) || '';
        if (local.startsWith(t.tokenPrefix)) localStorage.removeItem(TRUSTED_KEY);
        toast('已吊销');
        loadTrusted();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        trustedBusy.value = false;
      }
    }

    async function revokeAllTrusted() {
      if (!confirm('将吊销全部可信设备，所有设备下次都需要重新完整登录。继续吗？')) return;
      trustedBusy.value = true;
      try {
        await api('/api/trusted/revoke-all', { method: 'POST' });
        localStorage.removeItem(TRUSTED_KEY);
        toast('已吊销全部可信设备');
        loadTrusted();
      } catch (e) {
        toast(e.message, 'error');
      } finally {
        trustedBusy.value = false;
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

    // ---------- 联系人标签 ----------
    const tags = ref([]);           // [{id,name,count}]
    const tagsError = ref('');      // 标签读取失败的原因（空串表示正常）
    const filterTagIds = ref([]);   // 列表页生效的标签筛选（多个为「且」）
    const picked = ref([]);         // 批量勾选的联系人 id
    const showTagMgr = ref(false);
    const showBatchTag = ref(false);
    const tagBusy = ref(false);
    const tagNewName = ref('');
    const tagEditId = ref(0);
    const tagEditName = ref('');
    const batchTagIds = ref([]);
    const batchTagRemove = ref(false);
    const tagEditOpen = ref(false); // 详情页标签编辑展开
    const tagEditIds = ref([]);

    async function loadTags() {
      try {
        const out = await api('/api/tags');
        tags.value = (out && out.list) || [];
        tagsError.value = '';
      } catch (e) {
        // 标签是增值功能，取不到不该打断联系人列表，但也不能一声不吭：
        // tags=[] 会让列表页和详情页的标签筛选条整块消失，用户以为标签功能没了
        tags.value = [];
        tagsError.value = e.message;
      }
    }
    async function createTag() {
      const name = tagNewName.value.trim();
      if (!name) return;
      tagBusy.value = true;
      try {
        await api('/api/tags', { method: 'POST', body: { name } });
        tagNewName.value = '';
        await loadTags();
        toast('标签已创建');
      } catch (e) { toast(e.message, 'error'); }
      finally { tagBusy.value = false; }
    }
    function startTagRename(t) { tagEditId.value = t.id; tagEditName.value = t.name; }
    function cancelTagRename() { tagEditId.value = 0; tagEditName.value = ''; }
    async function commitTagRename() {
      const name = tagEditName.value.trim();
      if (!name || !tagEditId.value) return;
      tagBusy.value = true;
      try {
        await api('/api/tags/' + tagEditId.value, { method: 'PUT', body: { name } });
        tagEditId.value = 0;
        await loadTags();
        loadContacts();
        toast('标签已改名');
      } catch (e) { toast(e.message, 'error'); }
      finally { tagBusy.value = false; }
    }
    async function deleteTag(t) {
      if (!confirm('删除标签「' + t.name + '」？\n该标签会从 ' + (t.count || 0) + ' 位联系人身上移除，联系人本身不受影响。')) return;
      tagBusy.value = true;
      try {
        await api('/api/tags/' + t.id, { method: 'DELETE' });
        filterTagIds.value = filterTagIds.value.filter(id => id !== t.id);
        await loadTags();
        loadContacts();
        toast('标签已删除');
      } catch (e) { toast(e.message, 'error'); }
      finally { tagBusy.value = false; }
    }
    function toggleFilterTag(id) {
      const i = filterTagIds.value.indexOf(id);
      if (i >= 0) filterTagIds.value.splice(i, 1); else filterTagIds.value.push(id);
      loadContacts();
    }
    function clearFilterTags() {
      if (!filterTagIds.value.length) return;
      filterTagIds.value = [];
      loadContacts();
    }
    function togglePick(id) {
      const i = picked.value.indexOf(id);
      if (i >= 0) picked.value.splice(i, 1); else picked.value.push(id);
    }
    function togglePickAll() {
      if (picked.value.length) picked.value = [];
      else picked.value = contacts.value.map(c => c.id);
    }
    function clearPicks() { picked.value = []; }
    function openBatchTag() {
      if (!picked.value.length) return;
      batchTagIds.value = [];
      batchTagRemove.value = false;
      showBatchTag.value = true;
    }
    function toggleBatchTag(id) {
      const i = batchTagIds.value.indexOf(id);
      if (i >= 0) batchTagIds.value.splice(i, 1); else batchTagIds.value.push(id);
    }
    async function applyBatchTag() {
      if (!batchTagIds.value.length || !picked.value.length) return;
      tagBusy.value = true;
      try {
        const out = await api('/api/tags/batch', {
          method: 'POST',
          body: { contactIds: picked.value, tagIds: batchTagIds.value, remove: batchTagRemove.value },
        });
        toast((batchTagRemove.value ? '已移除标签，影响 ' : '已添加标签，影响 ') + (out.affected || 0) + ' 位联系人');
        showBatchTag.value = false;
        clearPicks();
        loadContacts();
        loadTags();
      } catch (e) { toast(e.message, 'error'); }
      finally { tagBusy.value = false; }
    }
    // 详情页标签编辑：直接用 contact.tags 初始化，省一次请求
    function openTagEdit() {
      // 再点一次要能收起：按钮文案写的就是「收起标签」，
      // 原来却无条件重置勾选并强制展开 —— 既收不起来，又把没保存的改动丢了
      if (tagEditOpen.value) { tagEditOpen.value = false; return; }
      const c = contact.value;
      tagEditIds.value = ((c && c.tags) || []).map(t => t.id);
      tagEditOpen.value = true;
      if (!tags.value.length) loadTags();
    }
    function toggleTagEdit(id) {
      const i = tagEditIds.value.indexOf(id);
      if (i >= 0) tagEditIds.value.splice(i, 1); else tagEditIds.value.push(id);
    }
    async function saveTagEdit() {
      const cid = route.id;   // 钉住写入目标，避免请求在途时切人导致标签打到别人身上
      tagBusy.value = true;
      try {
        await api('/api/contacts/' + cid + '/tags', { method: 'PUT', body: { tagIds: tagEditIds.value } });
        tagEditOpen.value = false;
        toast('标签已保存');
        loadTags();
        if (route.id === cid) loadDetail();
      } catch (e) { toast(e.message, 'error'); }
      finally { tagBusy.value = false; }
    }

    // ---------- 洞察页 ----------
    const insightTab = ref('briefing');
    const insightLoaded = reactive({ report: false, social: false, dup: false, period: false, graph: false, life: false, lifeproj: false, lifets: false, briefing: false, network: false, self: false, learning: false });
    function switchInsight(tab) {
      insightTab.value = tab;
      if (tab === 'report' && !insightLoaded.report) loadReport();
      if (tab === 'social' && !insightLoaded.social) loadSocial();
      if (tab === 'dup' && !insightLoaded.dup) loadDuplicates();
      if (tab === 'period' && !insightLoaded.period) loadPeriodReport();
      if (tab === 'graph' && !insightLoaded.graph) { insightLoaded.graph = true; loadConnections(); }
      if (tab === 'life' && !insightLoaded.life) { insightLoaded.life = true; loadLifeState(); }
      if (tab === 'lifeproj' && !insightLoaded.lifeproj) { insightLoaded.lifeproj = true; loadLifeProjection(); }
      if (tab === 'lifets' && !insightLoaded.lifets) { insightLoaded.lifets = true; loadLifeTimeline(); }
      if (tab === 'briefing' && !insightLoaded.briefing) { insightLoaded.briefing = true; loadBriefing(); }
      if (tab === 'network' && !insightLoaded.network) { insightLoaded.network = true; loadNetwork(); }
      if (tab === 'self' && !insightLoaded.self) { insightLoaded.self = true; loadSelfPortrait(); }
      if (tab === 'learning' && !insightLoaded.learning) { insightLoaded.learning = true; loadIntervention(); }
    }

    // ---------- 关系图谱 ----------
    const connections = ref([]);
    const connBusy = ref(false);
    const connTypes = {
      shared_location: '同城/同地区', shared_interest: '共同兴趣',
      shared_occupation: '同类职业', mentioned_name: '画像里提到对方',
    };
    function connTypeLabel(t) { return connTypes[t] || t; }

    async function loadConnections() {
      if (connBusy.value) return;
      connBusy.value = true;
      try {
        const data = await api('/api/relationships/connections');
        connections.value = data.connections || [];
      } catch (e) { toast(e.message, 'error'); }
      finally { connBusy.value = false; }
    }

    async function rebuildConnections() {
      if (connBusy.value) return;
      connBusy.value = true;
      try {
        const out = await api('/api/relationships/connections/rebuild', { method: 'POST' });
        toast('发现 ' + (out.count || 0) + ' 条关联');
        const data = await api('/api/relationships/connections');
        connections.value = data.connections || [];
      } catch (e) { toast(e.message, 'error'); }
      finally { connBusy.value = false; }
    }

    // ---------- 人生模拟器 ----------
    const life = reactive({ enabled: true, generatedAt: '', portfolio: null, assets: [], highRisk: [], time: null, trajectory: [] });
    const lifeproj = reactive({ enabled: true, generatedAt: '', projection: null });
    const lifes = reactive({ enabled: true, narrative: null });
    const lifeBusy = ref(false);
    const lifeClasses = { close: '挚友', friend: '好友', acquaintance: '熟人', weak: '弱关系', transactional: '事务型' };
    function lifeClassLabel(c) { return lifeClasses[c] || c; }

    async function loadLifeState() {
      if (lifeBusy.value) return;
      lifeBusy.value = true;
      try {
        const data = await api('/api/life/state');
        const st = data.state || {};
        life.enabled = data.enabled !== false;
        life.generatedAt = (data.generatedAt || '').slice(0, 16).replace('T', ' ');
        life.portfolio = st.portfolio || null;
        life.assets = st.assets || [];
        life.highRisk = st.highRisk || [];
        life.time = st.time || null;
        life.trajectory = st.trajectory || [];
      } catch (e) { toast(e.message, 'error'); }
      finally { lifeBusy.value = false; }
    }

    async function loadLifeProjection() {
      if (lifeBusy.value) return;
      lifeBusy.value = true;
      try {
        const data = await api('/api/life/projection');
        lifeproj.enabled = data.enabled !== false;
        lifeproj.generatedAt = (data.generatedAt || '').slice(0, 16).replace('T', ' ');
        lifeproj.projection = data.projection || null;
      } catch (e) { toast(e.message, 'error'); }
      finally { lifeBusy.value = false; }
    }

    async function loadLifeTimeline() {
      lifeBusy.value = true;
      try {
        const data = await api('/api/life/timeline');
        lifes.enabled = data.enabled !== false;
        lifes.narrative = data.narrative || null;
      } catch (e) { toast(e.message, 'error'); }
      finally { lifeBusy.value = false; }
    }

    async function recomputeLife() {
      if (lifeBusy.value) return;
      lifeBusy.value = true;
      try {
        await api('/api/life/recompute', { method: 'POST' });
        toast('正在后台重算，稍后自动刷新');
        setTimeout(() => {
          lifeBusy.value = false;
          if (insightTab.value === 'life') loadLifeState();
          if (insightTab.value === 'lifeproj') loadLifeProjection();
        }, 6000);
      } catch (e) {
        toast(e.message, 'error');
        lifeBusy.value = false;
      }
    }

    // ---------- 高阶洞察四件套 ----------
    const advBusy = ref(false);
    const net = reactive({ enabled: true, generatedAt: '', network: null });
    const selfpt = reactive({ enabled: true, generatedAt: '', self: null });
    const learn = reactive({ enabled: true, generatedAt: '', intervention: null });
    const brief = reactive({ enabled: true, generatedAt: '', briefing: null });

    async function loadNetwork() {
      if (advBusy.value) return;
      advBusy.value = true;
      try {
        const data = await api('/api/insight/network');
        net.enabled = data.enabled !== false;
        net.generatedAt = (data.generatedAt || '').slice(0, 16).replace('T', ' ');
        net.network = data.network || null;
      } catch (e) { toast(e.message, 'error'); }
      finally { advBusy.value = false; }
    }

    async function loadSelfPortrait() {
      if (advBusy.value) return;
      advBusy.value = true;
      try {
        const data = await api('/api/insight/self');
        selfpt.enabled = data.enabled !== false;
        selfpt.generatedAt = (data.generatedAt || '').slice(0, 16).replace('T', ' ');
        selfpt.self = data.self || null;
      } catch (e) { toast(e.message, 'error'); }
      finally { advBusy.value = false; }
    }

    async function loadIntervention() {
      if (advBusy.value) return;
      advBusy.value = true;
      try {
        const data = await api('/api/insight/intervention');
        learn.enabled = data.enabled !== false;
        learn.generatedAt = (data.generatedAt || '').slice(0, 16).replace('T', ' ');
        learn.intervention = data.intervention || null;
      } catch (e) { toast(e.message, 'error'); }
      finally { advBusy.value = false; }
    }

    async function loadBriefing() {
      if (advBusy.value) return;
      advBusy.value = true;
      try {
        const data = await api('/api/insight/briefing');
        brief.enabled = data.enabled !== false;
        brief.generatedAt = (data.generatedAt || '').slice(0, 16).replace('T', ' ');
        brief.briefing = data.briefing || null;
      } catch (e) { toast(e.message, 'error'); }
      finally { advBusy.value = false; }
    }

    function refreshCurrentInsightTab() {
      if (insightTab.value === 'briefing') loadBriefing();
      else if (insightTab.value === 'network') loadNetwork();
      else if (insightTab.value === 'self') loadSelfPortrait();
      else if (insightTab.value === 'learning') loadIntervention();
      else if (insightTab.value === 'life') loadLifeState();
      else if (insightTab.value === 'lifeproj') loadLifeProjection();
    }

    async function recomputeInsights() {
      if (advBusy.value) return;
      advBusy.value = true;
      try {
        await api('/api/insight/recompute', { method: 'POST' });
        toast('高阶洞察正在后台重算，稍后自动刷新');
        setTimeout(() => { advBusy.value = false; refreshCurrentInsightTab(); }, 6000);
      } catch (e) {
        toast(e.message, 'error');
        advBusy.value = false;
      }
    }

    // 聊天记录全文搜索
    const srch = reactive({ q: '', contactId: 0, from: '', to: '', archive: false });
    const srchRes = ref(null);
    const srchBusy = ref(false);
    const srchContacts = ref([]);
    function buildSearchQuery(offset) {
      const p = new URLSearchParams();
      p.set('q', srch.q.trim());
      p.set('offset', String(offset));
      if (srch.contactId) p.set('contactId', String(srch.contactId));
      if (srch.from) p.set('from', srch.from);
      if (srch.to) p.set('to', srch.to);
      if (srch.archive) p.set('archive', '1');
      return '?' + p.toString();
    }
    async function doSearch() {
      if (!srch.q.trim()) { toast('请输入搜索关键词', 'error'); return; }
      srchBusy.value = true;
      try {
        if (!srchContacts.value.length) srchContacts.value = (await api('/api/contacts')) || [];
        srchRes.value = await api('/api/search/messages' + buildSearchQuery(0));
      } catch (e) { toast(e.message, 'error'); }
      finally { srchBusy.value = false; }
    }
    async function searchMore() {
      const r = srchRes.value;
      if (!r || srchBusy.value || (r.list || []).length >= (r.total || 0)) return;
      srchBusy.value = true;
      try {
        const out = await api('/api/search/messages' + buildSearchQuery(r.list.length));
        srchRes.value = Object.assign({}, out, { list: r.list.concat(out.list || []) });
      } catch (e) { toast(e.message, 'error'); }
      finally { srchBusy.value = false; }
    }
    const srchHasMore = computed(() => {
      const r = srchRes.value;
      return !!r && (r.list || []).length < (r.total || 0);
    });

    // 疑似重复联系人
    const dup = ref(null);
    const dupBusy = ref(false);
    async function loadDuplicates() {
      dupBusy.value = true;
      try {
        dup.value = await api('/api/insights/duplicates');
        insightLoaded.dup = true;
      } catch (e) { toast(e.message, 'error'); }
      finally { dupBusy.value = false; }
    }
    function dupName(x) {
      if (!x) return '';
      return x.remark ? x.remark + '（' + x.name + '）' : x.name;
    }
    async function mergeDuplicate(pair) {
      const keep = pair.suggestedKeep === pair.b.id ? pair.b : pair.a;
      const drop = keep === pair.a ? pair.b : pair.a;
      if (!confirm('把「' + dupName(drop) + '」并入「' + dupName(keep) + '」？\n聊天记录和画像历史会一起搬过去，事后可在「合并记录」里撤销。')) return;
      dupBusy.value = true;
      try {
        await api('/api/merge', {
          method: 'POST',
          body: { sourceId: drop.id, targetId: keep.id, useSourceName: false, regenerate: true },
        });
        toast('已合并，画像正在后台重新生成');
        loadDuplicates();
      } catch (e) { toast(e.message, 'error'); }
      finally { dupBusy.value = false; }
    }

    // 社交大盘
    const social = ref(null);
    const socialDays = ref(30);
    const socialBusy = ref(false);
    let socialDaysOk = socialDays.value;   // 最近一次成功加载的天数，失败时退回它
    const weekdayNames = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
    async function loadSocial() {
      const d = socialDays.value;
      socialBusy.value = true;
      try {
        social.value = await api('/api/insights/social?days=' + d);
        socialDaysOk = d;
        insightLoaded.social = true;
      } catch (e) {
        toast(e.message, 'error');
        socialDays.value = socialDaysOk;   // 与年度报告同理：高亮和数据的口径要一致
      } finally { socialBusy.value = false; }
    }
    function changeSocialDays(d) { socialDays.value = d; loadSocial(); }
    const socialHourMax = computed(() => {
      const s = social.value;
      if (!s || !s.hourly) return 1;
      let m = 1;
      s.hourly.forEach(h => { m = Math.max(m, h.mine || 0, h.theirs || 0); });
      return m;
    });
    const socialWeekMax = computed(() => {
      const s = social.value;
      if (!s || !s.weekday) return 1;
      return Math.max(1, ...s.weekday);
    });
    // 秒数转可读时长（回复速度用）
    function fmtDur(sec) {
      if (sec == null || sec <= 0) return '—';
      if (sec < 60) return Math.round(sec) + ' 秒';
      if (sec < 3600) return Math.round(sec / 60) + ' 分钟';
      if (sec < 86400) return (sec / 3600).toFixed(1) + ' 小时';
      return (sec / 86400).toFixed(1) + ' 天';
    }
    // 柱状图百分比宽度：模板里不用 Math，避免全局对象解析的坑
    function barPct(v, max) {
      const n = Number(v) || 0;
      const m = Number(max) || 1;
      if (n <= 0) return '0%';
      const p = Math.round((n * 100) / m);
      return Math.min(100, Math.max(1, p)) + '%';
    }

    // 年度关系报告
    const report = ref(null);
    const reportYear = ref(new Date().getFullYear());
    const reportBusy = ref(false);
    // 后端只接受 2000~2100（insights_api.go），连点「上一年」能一路点到负数
    const REPORT_YEAR_MIN = 2000, REPORT_YEAR_MAX = 2100;
    let reportYearOk = reportYear.value;   // 最近一次成功加载的年份，失败时退回它
    async function loadReport() {
      const y = reportYear.value;
      reportBusy.value = true;
      try {
        report.value = await api('/api/insights/report?year=' + y);
        reportYearOk = y;
        insightLoaded.report = true;
      } catch (e) {
        toast(e.message, 'error');
        // 失败就把年份退回上一次成功的值：否则按钮高亮的是新年份、
        // 表格里还是旧年份的数据，用户会以为统计算错了
        reportYear.value = reportYearOk;
      } finally { reportBusy.value = false; }
    }
    function changeReportYear(y) {
      const clamped = Math.min(REPORT_YEAR_MAX, Math.max(REPORT_YEAR_MIN, y));
      if (clamped !== y) toast('仅支持 ' + REPORT_YEAR_MIN + '~' + REPORT_YEAR_MAX + ' 年', 'error');
      reportYear.value = clamped;
      loadReport();
    }
    const reportMonthMax = computed(() => {
      const r = report.value;
      if (!r || !r.months) return 1;
      let m = 1;
      r.months.forEach(x => { m = Math.max(m, x.total || 0); });
      return m;
    });
    // HTML 长页要带 Authorization 头，window.open 直接给 URL 会被 401 拦下；
    // 改成先 fetch 拿文本，再转 Blob URL 打开新标签页
    async function openReportHTML() {
      reportBusy.value = true;
      try {
        const res = await fetch('/api/insights/report?year=' + reportYear.value + '&format=html', {
          headers: { Authorization: 'Bearer ' + localStorage.getItem(TOKEN_KEY) },
        });
        if (res.status === 401) { localStorage.removeItem(TOKEN_KEY); authed.value = false; throw new Error('登录已过期，请重新输入 Token'); }
        if (!res.ok) {
          let msg = '生成失败 (' + res.status + ')';
          try { msg = (await res.json()).error || msg; } catch (e) { /* 非 JSON */ }
          throw new Error(msg);
        }
        const html = await res.text();
        const url = URL.createObjectURL(new Blob([html], { type: 'text/html;charset=utf-8' }));
        const w = window.open(url, '_blank');
        if (!w) toast('浏览器拦截了新窗口，请允许本站弹窗后重试', 'error');
        setTimeout(() => URL.revokeObjectURL(url), 120000);
      } catch (e) { toast(e.message, 'error'); }
      finally { reportBusy.value = false; }
    }

    // ---------- 多周期报告（日/周/月/季/半年/年） ----------
    const PERIODS = [
      { key: 'day', name: '日报' },
      { key: 'week', name: '周报' },
      { key: 'month', name: '月报' },
      { key: 'quarter', name: '季报' },
      { key: 'half', name: '半年报' },
      { key: 'year', name: '年报' },
    ];
    const prPeriod = ref('week');
    const prAnchor = ref('');           // 'YYYY-MM-DD'，空表示后端按今天
    const prReport = ref(null);
    const prBusy = ref(false);
    let prOk = { period: 'week', anchor: '' };   // 最近一次成功加载的周期/锚点，失败时退回
    function prToday() {
      const d = new Date();
      const p = n => String(n).padStart(2, '0');
      return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
    }
    function prPeriodName(k) {
      const it = PERIODS.find(x => x.key === k);
      return it ? it.name : k;
    }
    function prQuery(extra) {
      const p = new URLSearchParams();
      p.set('period', prPeriod.value);
      if (prAnchor.value) p.set('anchor', prAnchor.value);
      if (extra) p.set('format', extra);
      return '?' + p.toString();
    }
    async function loadPeriodReport() {
      prBusy.value = true;
      try {
        prReport.value = await api('/api/insights/period-report' + prQuery());
        if (!prAnchor.value) prAnchor.value = prToday();
        prOk = { period: prPeriod.value, anchor: prAnchor.value };
        insightLoaded.period = true;
      } catch (e) {
        toast(e.message, 'error');
        prPeriod.value = prOk.period;
        prAnchor.value = prOk.anchor;
      } finally { prBusy.value = false; }
    }
    function changePeriod(k) { prPeriod.value = k; loadPeriodReport(); }
    // 上一个/下一个自然区间：按周期把锚点日期平移固定步长
    function prShift(dir) {
      const base = prAnchor.value ? new Date(prAnchor.value + 'T00:00:00') : new Date();
      const p = n => String(n).padStart(2, '0');
      let y = base.getFullYear(), m = base.getMonth(), d = base.getDate();
      switch (prPeriod.value) {
        case 'day': d += dir; break;
        case 'week': d += dir * 7; break;
        case 'month': m += dir; break;
        case 'quarter': m += dir * 3; break;
        case 'half': m += dir * 6; break;
        case 'year': y += dir; break;
      }
      const nd = new Date(y, m, d);
      prAnchor.value = nd.getFullYear() + '-' + p(nd.getMonth() + 1) + '-' + p(nd.getDate());
      loadPeriodReport();
    }
    const prBucketMax = computed(() => {
      const r = prReport.value;
      if (!r || !r.buckets) return 1;
      let m = 1;
      r.buckets.forEach(x => { m = Math.max(m, x.total || 0); });
      return m;
    });
    async function openPeriodReportHTML() {
      prBusy.value = true;
      try {
        const res = await fetch('/api/insights/period-report' + prQuery('html'), {
          headers: { Authorization: 'Bearer ' + localStorage.getItem(TOKEN_KEY) },
        });
        if (res.status === 401) { localStorage.removeItem(TOKEN_KEY); authed.value = false; throw new Error('登录已过期，请重新输入 Token'); }
        if (!res.ok) {
          let msg = '生成失败 (' + res.status + ')';
          try { msg = (await res.json()).error || msg; } catch (e) { /* 非 JSON */ }
          throw new Error(msg);
        }
        const html = await res.text();
        const url = URL.createObjectURL(new Blob([html], { type: 'text/html;charset=utf-8' }));
        const w = window.open(url, '_blank');
        if (!w) toast('浏览器拦截了新窗口，请允许本站弹窗后重试', 'error');
        setTimeout(() => URL.revokeObjectURL(url), 120000);
      } catch (e) { toast(e.message, 'error'); }
      finally { prBusy.value = false; }
    }

    // ---------- 联系人时间线 ----------
    const timeline = ref([]);
    const timelineBusy = ref(false);
    const showEventModal = ref(false);
    const eventForm = reactive({ title: '', detail: '', eventTime: '' });
    const timelineKinds = {
      created: '创建', first_message: '首条消息', last_message: '最近消息',
      profile: '画像更新', merge: '合并进来', merged_into: '被合并',
      renamed: '改名', remark: '改备注', custom: '手动记录',
    };
    function tlKind(k) { return timelineKinds[k] || k || ''; }
    async function loadTimeline() {
      const my = detailSeq;
      timelineBusy.value = true;
      try {
        const out = await api('/api/contacts/' + route.id + '/timeline');
        if (my !== detailSeq) return;
        timeline.value = (out && out.list) || [];
      } catch (e) { if (my === detailSeq) toast(e.message, 'error'); }
      finally { if (my === detailSeq) timelineBusy.value = false; }
    }
    function openEventModal() {
      const d = new Date(), pad = n => String(n).padStart(2, '0');
      eventForm.title = '';
      eventForm.detail = '';
      eventForm.eventTime = d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
        'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
      showEventModal.value = true;
    }
    async function addEvent() {
      if (!eventForm.title.trim()) { toast('请填写事件标题', 'error'); return; }
      // 进门先钉住联系人 id：请求发出后用户可能已经切到别人，
      // 用「当前」route.id 会把事件写到新联系人名下还提示成功
      const cid = route.id;
      // datetime-local 只给到分钟，后端 parseTimeLoose 不认这个格式，补上秒
      let when = eventForm.eventTime;
      if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(when)) when += ':00';
      timelineBusy.value = true;
      try {
        await api('/api/contacts/' + cid + '/timeline', {
          method: 'POST',
          body: { title: eventForm.title.trim(), detail: eventForm.detail.trim(), eventTime: when },
        });
        showEventModal.value = false;
        toast('事件已记录');
        if (route.id === cid) loadTimeline();
      } catch (e) { toast(e.message, 'error'); }
      finally { timelineBusy.value = false; }
    }
    async function delEvent(ev) {
      if (!ev || !ev.id) return;
      if (!confirm('删除手动记录的事件「' + ev.title + '」？')) return;
      const cid = route.id;   // 同上：钉住删除目标，避免误删别人的事件
      timelineBusy.value = true;
      try {
        await api('/api/contacts/' + cid + '/timeline?eventId=' + ev.id, { method: 'DELETE' });
        toast('已删除');
        if (route.id === cid) loadTimeline();
      } catch (e) { toast(e.message, 'error'); }
      finally { timelineBusy.value = false; }
    }

    // ---------- 对话预演 ----------
    // 完全无状态：一场预演只活在这里，切联系人/刷新页面就没了（后端也不落库）。
    // 因此 resetDetail 必须把 rh 整个清干净，否则 A 的演练会挂在 B 的详情页上。
    const rh = reactive({
      scene: '', first: 'me', input: '',
      turns: [], ctx: null, review: null,
      started: false, busy: false, reviewBusy: false, ctxBusy: false, ctxFailed: false,
    });
    const rhMineCount = computed(() => rh.turns.filter(t => t.role === 'me').length);

    async function loadRehearsalContext() {
      const my = detailSeq;
      rh.ctxBusy = true;
      rh.ctxFailed = false;
      try {
        const out = await api('/api/contacts/' + route.id + '/rehearsal/context');
        if (my !== detailSeq) return;
        out.hints = out.hints || [];
        out.sampleLines = out.sampleLines || [];
        rh.ctx = out;
      } catch (e) {
        if (my !== detailSeq) return;
        rh.ctxFailed = true;
        toast(e.message, 'error');
      } finally { if (my === detailSeq) rh.ctxBusy = false; }
    }

    // askOther 让模型以对方身份回一条。turns 由调用方传副本：
    // 请求在途时用户还能继续编辑数组，直接传引用会把半截内容发给后端。
    async function askOther(cid, scene, turns) {
      const out = await api('/api/contacts/' + cid + '/rehearsal/turn', {
        method: 'POST',
        body: { scene: scene, turns: turns },
      });
      const text = ((out && out.text) || '').trim();
      if (!text) throw httpErr('模型没有返回内容，请重试', 500);
      return { role: 'other', text: text, emotion: ((out && out.emotion) || '').trim() };
    }

    async function startRehearsal() {
      const scene = rh.scene.trim();
      if (!scene) { toast('请先描述预演场景', 'error'); return; }
      const cid = route.id;   // 钉住联系人：请求在途时用户可能已经切到别人
      rh.busy = true;
      rh.turns = [];
      rh.review = null;
      rh.started = true;
      try {
        if (rh.first === 'other') {
          const t = await askOther(cid, scene, []);
          if (route.id !== cid) return;
          rh.turns.push(t);
        }
      } catch (e) {
        if (route.id === cid) { rh.started = false; toast(e.message, 'error'); }
      } finally { if (route.id === cid) rh.busy = false; }
    }

    async function sendRehearsal() {
      const text = rh.input.trim();
      if (!text || rh.busy || rh.reviewBusy) return;
      const cid = route.id;
      const scene = rh.scene.trim();
      rh.turns.push({ role: 'me', text: text, emotion: '' });
      rh.input = '';
      const turns = rh.turns.map(t => ({ role: t.role, text: t.text }));
      rh.busy = true;
      try {
        const t = await askOther(cid, scene, turns);
        if (route.id !== cid) return;
        rh.turns.push(t);
      } catch (e) {
        if (route.id === cid) {
          // 失败要回滚刚推入的那句，否则用户看着自己说的话躺在列表里，
          // 会以为对方已读不回，其实是请求根本没成功
          rh.turns.pop();
          rh.input = text;
          toast(e.message, 'error');
        }
      } finally { if (route.id === cid) rh.busy = false; }
    }

    async function reviewRehearsal() {
      if (!rhMineCount.value) { toast('你还没说过话，没有可复盘的内容', 'error'); return; }
      if (rh.busy || rh.reviewBusy) return;
      const cid = route.id;
      const scene = rh.scene.trim();
      const turns = rh.turns.map(t => ({ role: t.role, text: t.text }));
      rh.reviewBusy = true;
      try {
        const out = await api('/api/contacts/' + cid + '/rehearsal/review', {
          method: 'POST', body: { scene: scene, turns: turns },
        });
        if (route.id !== cid) return;
        ['good', 'bad', 'risks', 'suggestions'].forEach(k => {
          if (!Array.isArray(out[k])) out[k] = [];
        });
        rh.review = out;
      } catch (e) { if (route.id === cid) toast(e.message, 'error'); }
      finally { if (route.id === cid) rh.reviewBusy = false; }
    }

    // restartRehearsal 重来一局：保留场景描述，清掉对话和复盘
    function restartRehearsal() {
      rh.turns = [];
      rh.review = null;
      rh.input = '';
      rh.started = false;
      rh.busy = false;
      rh.reviewBusy = false;
    }

    function copyRehearsal() {
      const name = (rh.ctx && rh.ctx.name) || '对方';
      const lines = ['【对话预演】' + name, '场景：' + rh.scene.trim(), ''];
      rh.turns.forEach(t => lines.push((t.role === 'me' ? '我' : name) + '：' + t.text));
      const r = rh.review;
      if (r) {
        lines.push('', '【复盘】达成可能 ' + r.score + ' / 10');
        if (r.summary) lines.push(r.summary);
        const sec = (title, arr) => {
          if (arr && arr.length) {
            lines.push(title + '：');
            arr.forEach(x => lines.push('- ' + x));
          }
        };
        sec('做得好', r.good);
        sec('有问题', r.bad);
        sec('风险', r.risks);
        sec('建议话术', r.suggestions);
      }
      copyText(lines.join('\n'));
    }

    // ---------- 关系驾驶舱（Phase 8：可信画像事实/证据 + 趋势 + 行动建议 + 回复前推演）----------
    const ck = reactive({
      loaded: false,
      facts: [], factsBusy: false, factsFailed: false,
      trend: null, trendBusy: false,
      suggestions: [], sugBusy: false,
      draft: '', simBusy: false, simResult: null, simFailed: false, simError: '',
      askQ: '', askBusy: false, askResult: null, askFailed: false, askError: '',
    });
    const ckTrendMeta = {
      warming: { label: '关系升温', cls: 'st-ok' },
      cooling: { label: '关系降温', cls: 'st-warn' },
      dormant: { label: '趋于沉寂', cls: 'st-gray' },
      stable: { label: '平稳', cls: 'st-blue' },
      new: { label: '新关系', cls: 'st-purple' },
    };
    const ckKindLabel = { cooling: '关系降温', silence: '长期沉默', no_reply: '对方消息未回' };
    // 展开证据的事实 id 集合：用 reactive Set 模拟，命中/收起都触发重渲染
    const ckExpanded = reactive({});
    function ckToggleFact(id) { ckExpanded[id] = !ckExpanded[id]; }
    function ckPct(c) { return Math.round(Math.max(0, Math.min(1, c)) * 100) + '%'; }

    function loadCockpit() {
      ck.loaded = true;
      loadFacts(false);
      loadTrend();
      loadSuggestions();
    }
    async function loadFacts(force) {
      const my = detailSeq;
      const cid = route.id;
      ck.factsBusy = true;
      ck.factsFailed = false;
      try {
        let out;
        if (force) {
          out = await api('/api/contacts/' + cid + '/facts/rebuild', { method: 'POST' });
        } else {
          out = await api('/api/contacts/' + cid + '/facts');
        }
        if (my !== detailSeq || route.id !== cid) return;
        ck.facts = (out && out.facts) || [];
      } catch (e) {
        if (my === detailSeq && route.id === cid) { ck.factsFailed = true; toast(e.message, 'error'); }
      } finally { if (my === detailSeq && route.id === cid) ck.factsBusy = false; }
    }
    async function loadTrend() {
      const my = detailSeq;
      const cid = route.id;
      ck.trendBusy = true;
      try {
        const out = await api('/api/contacts/' + cid + '/trend');
        if (my !== detailSeq || route.id !== cid) return;
        ck.trend = out;
      } catch (e) {
        if (my === detailSeq && route.id === cid) ck.trend = null;
      } finally { if (my === detailSeq && route.id === cid) ck.trendBusy = false; }
    }
    async function loadSuggestions() {
      const my = detailSeq;
      const cid = route.id;
      ck.sugBusy = true;
      try {
        const out = await api('/api/relationships/suggestions?includeHandled=1');
        if (my !== detailSeq || route.id !== cid) return;
        const all = (out && out.suggestions) || [];
        ck.suggestions = all.filter(s => s.contactId === cid);
      } catch (e) {
        if (my === detailSeq && route.id === cid) ck.suggestions = [];
      } finally { if (my === detailSeq && route.id === cid) ck.sugBusy = false; }
    }
    async function genSuggestions() {
      const cid = route.id;
      ck.sugBusy = true;
      try {
        await api('/api/relationships/suggestions/generate', { method: 'POST', body: { contactId: cid } });
        if (route.id === cid) await loadSuggestions();
        else { if (detailSeq === cid) loadSuggestions(); }
      } catch (e) { toast(e.message, 'error'); }
      finally { if (route.id === cid) ck.sugBusy = false; }
    }
    async function setSuggestionStatus(s, st) {
      const cid = route.id;
      try {
        await api('/api/relationships/suggestions/' + s.id + '/status', { method: 'POST', body: { status: st } });
        if (route.id === cid) { s.status = st; }
      } catch (e) { toast(e.message, 'error'); }
    }
    async function runSimulate() {
      const text = ck.draft.trim();
      if (!text || ck.simBusy) return;
      const cid = route.id;
      ck.simBusy = true;
      ck.simFailed = false;
      ck.simError = '';
      try {
        const out = await api('/api/contacts/' + cid + '/rehearsal/simulate', { method: 'POST', body: { draft: text } });
        if (route.id !== cid) return;
        out.replies = out.replies || [];
        ck.simResult = out;
      } catch (e) {
        if (route.id === cid) { ck.simFailed = true; ck.simError = e.message || '推演失败'; }
      } finally { if (route.id === cid) ck.simBusy = false; }
    }
    // 问 TA 的历史：先检索相关原文，再由模型带 [n] 出处作答
    async function askContact() {
      const q = ck.askQ.trim();
      if (!q || ck.askBusy) return;
      const cid = route.id;
      ck.askBusy = true;
      ck.askFailed = false;
      ck.askError = '';
      try {
        const out = await api('/api/contacts/' + cid + '/ask', { method: 'POST', body: { question: q } });
        if (route.id !== cid) return;
        out.sources = out.sources || [];
        ck.askResult = out;
      } catch (e) {
        if (route.id === cid) { ck.askFailed = true; ck.askError = e.message || '问答失败'; }
      } finally { if (route.id === cid) ck.askBusy = false; }
    }
    // 点击出处：切到聊天记录 tab 并尝试高亮定位原文
    async function jumpToAskSource(src) {
      switchTab('messages');
      if (!messages.value.length) loadMessages(false);
      await nextTick();
      const el = document.getElementById('msg-' + src.messageId);
      if (el) {
        el.scrollIntoView({ behavior: 'smooth', block: 'center' });
        el.classList.add('msg-flash');
        setTimeout(() => el.classList.remove('msg-flash'), 1600);
      } else {
        toast('该条原文可能在更早的记录里，可在聊天记录按时间翻查', 'info');
      }
    }
    function resetCockpit() {
      ck.loaded = false;
      ck.facts = []; ck.factsBusy = false; ck.factsFailed = false;
      ck.trend = null; ck.trendBusy = false;
      ck.suggestions = []; ck.sugBusy = false;
      ck.draft = ''; ck.simBusy = false; ck.simResult = null; ck.simFailed = false; ck.simError = '';
      ck.askQ = ''; ck.askBusy = false; ck.askResult = null; ck.askFailed = false; ck.askError = '';
      Object.keys(ckExpanded).forEach(k => { delete ckExpanded[k]; });
    }

    // ---------- 待跟进事项 ----------
    const followups = ref([]);
    const followupFilter = ref('open');
    const followupBusy = ref(false);
    const showFollowupModal = ref(false);
    const followupForm = reactive({ contactId: 0, kind: 'custom', content: '', amount: '' });
    const pickContacts = ref([]);
    const followupKinds = { question: '待回复', promise: '我答应的事', money: '钱款往来', custom: '手动记录' };
    async function loadFollowups() {
      try {
        const out = await api('/api/assistant/followups?status=' + followupFilter.value + '&limit=200');
        followups.value = (out && out.list) || [];
      } catch (e) {
        if (route.view === 'assistant') toast(e.message, 'error');
      }
    }
    function switchFollowup(st) {
      if (followupFilter.value === st) return;
      followupFilter.value = st;
      loadFollowups();
    }
    async function setFollowupStatus(it, st) {
      followupBusy.value = true;
      try {
        await api('/api/assistant/followups/' + it.id, { method: 'PUT', body: { status: st } });
        toast(st === 'done' ? '已标记完成' : '已更新状态');
        loadFollowups();
      } catch (e) { toast(e.message, 'error'); }
      finally { followupBusy.value = false; }
    }
    async function delFollowup(it) {
      if (!confirm('删除这条待跟进？\n' + it.content)) return;
      followupBusy.value = true;
      try {
        await api('/api/assistant/followups/' + it.id, { method: 'DELETE' });
        toast('已删除');
        loadFollowups();
      } catch (e) { toast(e.message, 'error'); }
      finally { followupBusy.value = false; }
    }
    async function scanFollowups() {
      if (followupBusy.value) return;
      followupBusy.value = true;
      try {
        const out = await api('/api/assistant/followups/scan', { method: 'POST' });
        toast('扫描完成：看了 ' + (out.scanned || 0) + ' 位联系人，新增 ' + (out.added || 0) + ' 条');
        loadFollowups();
      } catch (e) { toast(e.message, 'error'); }
      finally { followupBusy.value = false; }
    }
    async function openFollowupModal(cid) {
      try {
        if (!pickContacts.value.length) pickContacts.value = (await api('/api/contacts')) || [];
      } catch (e) { toast(e.message, 'error'); return; }
      followupForm.contactId = cid || (contact.value ? contact.value.id : 0);
      followupForm.kind = 'custom';
      followupForm.content = '';
      followupForm.amount = '';
      showFollowupModal.value = true;
    }
    async function addFollowup() {
      if (!followupForm.contactId) { toast('请选择联系人', 'error'); return; }
      if (!followupForm.content.trim()) { toast('请填写内容', 'error'); return; }
      followupBusy.value = true;
      try {
        await api('/api/assistant/followups', {
          method: 'POST',
          body: {
            contactId: followupForm.contactId,
            kind: followupForm.kind,
            content: followupForm.content.trim(),
            // 只有钱款往来才带金额，切换类型后不要把残留值提交上去
            amount: followupForm.kind === 'money' ? followupForm.amount.trim() : '',
          },
        });
        showFollowupModal.value = false;
        toast('已添加');
        loadFollowups();
      } catch (e) { toast(e.message, 'error'); }
      finally { followupBusy.value = false; }
    }

    // ---------- 日历订阅 ----------
    const cal = ref(null);
    const calLoaded = ref(false);   // 是否成功读到过订阅状态（null 可能是"没开启"也可能是"读失败"）
    const calBusy = ref(false);
    async function loadCalendarKey() {
      try {
        cal.value = await api('/api/assistant/calendar/key');
        calLoaded.value = true;
      } catch (e) {
        cal.value = null;
        calLoaded.value = false;
        toast('日历订阅状态读取失败：' + e.message, 'error');
      }
    }
    async function rotateCalendarKey() {
      // 读不到状态时同样要确认。原来写的是 cal.value && cal.value.enabled，
      // 一旦 loadCalendarKey 失败（cal=null）条件短路，点「重新生成」会
      // 不做任何提示就把正在用的订阅链接废掉。
      if (!calLoaded.value || (cal.value && cal.value.enabled)) {
        if (!confirm('重新生成订阅密钥后，旧的订阅链接会立刻失效，需要在日历客户端里重新添加。确定继续？')) return;
      }
      calBusy.value = true;
      try {
        cal.value = await api('/api/assistant/calendar/key', { method: 'POST' });
        calLoaded.value = true;
        // 表单里存的是打码值，保存设置时后端凭它保留库中真密钥；
        // 不同步的话，密钥从无到有时表单仍是空串，一保存就把新密钥冲掉
        asstForm.value.calendarKey = (cal.value && cal.value.enabled) ? '******' : '';
        toast('订阅已开启，把下面的地址添加到手机/电脑日历');
      } catch (e) { toast(e.message, 'error'); }
      finally { calBusy.value = false; }
    }
    async function clearCalendarKey() {
      if (!confirm('关闭日历订阅？密钥会被清除，已添加的订阅会失效。')) return;
      calBusy.value = true;
      try {
        cal.value = await api('/api/assistant/calendar/key', { method: 'DELETE' });
        calLoaded.value = true;
        asstForm.value.calendarKey = '';
        toast('日历订阅已关闭');
      } catch (e) { toast(e.message, 'error'); }
      finally { calBusy.value = false; }
    }
    async function copyText(t) {
      if (!t) return;
      try { await navigator.clipboard.writeText(t); toast('已复制'); }
      catch (e) { toast('复制失败，请选中文字手动复制', 'error'); }
    }

    // ---------- 重要日子祝福草稿 ----------
    const blessBusy = ref(0); // 正在生成的条目下标，0 表示空闲（数组下标从 1 记）
    async function genBlessing(d, idx) {
      if (blessBusy.value) return;
      blessBusy.value = idx + 1;
      try {
        const out = await api('/api/assistant/blessing', {
          method: 'POST',
          body: {
            contactId: d.contactId, kind: d.kind, raw: d.raw,
            month: d.month, day: d.day, dateStr: d.dateStr, daysUntil: d.daysUntil,
          },
        });
        d.blessings = (out && out.list) || [];
        if (!d.blessings.length) toast('模型没返回内容，稍后再试', 'error');
      } catch (e) { toast(e.message, 'error'); }
      finally { blessBusy.value = 0; }
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
        // 会话已经没了（api() 在 401 时把 authed 置 false）就停掉轮询。
        // 不然停在 #/status 页面时会每 5 秒弹一次「登录已过期」，把屏幕刷满。
        if (!authed.value) stopStatusTimer();
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
      // 本地存的是网页会话令牌（7 天有效），启动时向后端验真，过期/被吊销就回登录页；
      // 无有效会话时若存有可信令牌（且本标签页没刚登出过），静默换取新会话免登录
      (async () => {
        if (localStorage.getItem(TOKEN_KEY)) {
          try {
            await api('/api/status');
            authed.value = true;
            parseRoute();
            return;
          } catch (e) {
            // 同样只在「明确被拒」时清会话令牌：服务没起来或断网时留着它，
            // 等网络恢复后刷新页面还能直接进，不用重走一遍登录
            if (isAuthRejected(e)) localStorage.removeItem(TOKEN_KEY);
          }
        }
        if (await tryTrustedLogin()) parseRoute();
      })();
    });
    onUnmounted(() => { window.removeEventListener('hashchange', parseRoute); stopStatusTimer(); });

    return {
      assist, styles, copyAssist, reviewDraft, analyzeReplies, rewriteReply, loadChanges, closeChanges,
      profileEditor, profileSaving, profileEditError, profileSchema, startProfileEdit, saveProfileEdit, addIntentRow,
      authed, tokenInput, loginChecking, loginError, login, logout,
      authStage, codeInput, setupSecret, setupOtpauth, setupQr,
      enable2FA, verify2FA, backToToken, trustDevice,
      archive, archiveBusy, archiveResult, archiveError, archiveForm,
      loadArchive, saveArchiveSettings, runArchive, restoreArchive,
      trustedList, trustedBusy, loadTrusted, revokeTrusted, revokeAllTrusted,
      route, contacts, contactsTotal, contactsHasMore, loadingContacts, search, searchApplied, showMerged,
      loadContacts, loadMoreContacts, applySearch, clearSearch,
      contact, loadingDetail, detailTab, messages, messagesLoading, messagesHasMore,
      history, expandedHistory, stats, busy, profileSections,
      mergeLogs, loadingMerges, mergeCandidates,
      backupBusy, backupResult, backupFile, backupLogs, exportBackup, pickImport, importBackup,
      encPassword, impPassword, showEncPwd,
      asst, asstLoading, asstBusy, asstForm,
      loadAssistant, saveAssistantSettings, testAssistantEmail, runAssistantNow,
      weeklyPlan, weeklyPlanBusy, weeklyKindLabel, loadWeeklyPlan, regenWeeklyPlan,
      showRemark, remarkInput, showSupplement, supplementNote,
      showMerge, mergeSourceId, mergeUseSourceName, mergeRegenerate, showDelete,
      toasts,
      loadContacts, gotoDetail, displayName, loadDetail, switchTab, loadMessages,
      toggleHistory, historySections, rollback,
      startRemark, doSetRemark, doSupplement, doRegenerate,
      startMerge, doMerge, doDelete, confirmDelete,
      loadMergeLogs, undoMerge, fmtTime,
      sysStatus, statusLoading, loadStatus, fmtUptime, fmtAgo, fmtMB, pctClass,
      // 标签
      tags, tagsError, filterTagIds, picked, showTagMgr, showBatchTag, tagBusy, tagNewName,
      tagEditId, tagEditName, batchTagIds, batchTagRemove, tagEditOpen, tagEditIds,
      loadTags, createTag, startTagRename, cancelTagRename, commitTagRename, deleteTag,
      toggleFilterTag, clearFilterTags, togglePick, togglePickAll, clearPicks,
      openBatchTag, toggleBatchTag, applyBatchTag, openTagEdit, toggleTagEdit, saveTagEdit,
      // 洞察页
      insightTab, switchInsight,
      connections, connBusy, connTypeLabel, loadConnections, rebuildConnections,
      // 人生模拟器
      life, lifeproj, lifes, lifeBusy, lifeClassLabel,
      loadLifeState, loadLifeProjection, loadLifeTimeline, recomputeLife,
      // 高阶洞察四件套
      advBusy, net, selfpt, learn, brief,
      loadNetwork, loadSelfPortrait, loadIntervention, loadBriefing, recomputeInsights,
      srch, srchRes, srchBusy, srchContacts, srchHasMore, doSearch, searchMore,
      dup, dupBusy, loadDuplicates, dupName, mergeDuplicate,
      social, socialDays, socialBusy, weekdayNames, loadSocial, changeSocialDays,
      socialHourMax, socialWeekMax, fmtDur, barPct,
      report, reportYear, reportBusy, loadReport, changeReportYear, reportMonthMax, openReportHTML,
      PERIODS, prPeriod, prAnchor, prReport, prBusy, loadPeriodReport, changePeriod, prShift, prBucketMax, openPeriodReportHTML, prPeriodName,
      // 时间线
      timeline, timelineBusy, showEventModal, eventForm, tlKind,
      loadTimeline, openEventModal, addEvent, delEvent,
      // 对话预演
      rh, rhMineCount, startRehearsal, sendRehearsal, reviewRehearsal,
      restartRehearsal, copyRehearsal,
      // 关系驾驶舱
      ck, ckTrendMeta, ckKindLabel, ckExpanded, ckToggleFact, ckPct,
      loadCockpit, loadFacts, loadTrend, genSuggestions, setSuggestionStatus, runSimulate,
      askContact, jumpToAskSource,
      // 待跟进 / 日历订阅 / 祝福草稿
      followups, followupFilter, followupBusy, showFollowupModal, followupForm,
      pickContacts, followupKinds, loadFollowups, switchFollowup, setFollowupStatus,
      delFollowup, scanFollowups, openFollowupModal, addFollowup,
      cal, calBusy, loadCalendarKey, rotateCalendarKey, clearCalendarKey, copyText,
      blessBusy, genBlessing,
    };
  },
}).mount('#app');
