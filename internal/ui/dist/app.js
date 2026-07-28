(() => {
  "use strict";

  const byId = (id) => document.getElementById(id);
  const loginScreen = byId("loginScreen");
  const mainApp = byId("mainApp");
  const loginForm = byId("loginForm");
  const loginError = byId("loginError");
  const connectionDot = byId("connectionDot");
  const connectionText = byId("connectionText");
  const confirmDialog = byId("confirmDialog");
  const history = { cpuRpm: [], gpuRpm: [], cpuTemp: [], gpuTemp: [] };

  let currentState = null;
  let socket = null;
  let socketRetry = null;
  let socketKeepalive = null;
  let authenticated = false;
  let manualDraft = { cpu: 50, gpu: 50 };
  // Overwritten from /api/setup so the client-side check matches the server.
  let minPasswordLength = 12;

  const effectIgnored = {
    0: ["speed", "direction"],
    1: ["speed"],
    2: ["color", "direction"],
    3: ["color"],
    4: [],
    5: ["direction"],
    6: ["direction"],
    7: ["direction"],
  };

  async function api(path, options = {}) {
    const request = {
      credentials: "same-origin",
      ...options,
      headers: {
        ...(options.body ? { "Content-Type": "application/json" } : {}),
        ...(options.method && options.method !== "GET" ? { "X-Requested-With": "phnctl" } : {}),
        ...(options.headers || {}),
      },
    };
    const response = await fetch(path, request);
    let data = null;
    try {
      data = await response.json();
    } catch {
      data = {};
    }
    if (response.status === 401) {
      showLogin();
      const error = new Error("登录已过期，请重新登录");
      error.code = "unauthorized";
      throw error;
    }
    // The server rejects everything until a password exists; bounce back to the
    // login card so it can swap itself over to the first-run form.
    if (response.status === 403 && data.code === "setup_required" && path !== "/api/setup") {
      showLogin();
      const error = new Error("尚未完成初始设置");
      error.code = "setup_required";
      throw error;
    }
    if (!response.ok) {
      const error = new Error(data.error || `请求失败 (${response.status})`);
      error.code = data.code;
      error.status = response.status;
      throw error;
    }
    return data;
  }

  loginForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    loginError.textContent = "";
    const button = loginForm.querySelector("button");
    button.disabled = true;
    try {
      await api("/api/login", {
        method: "POST",
        body: JSON.stringify({
          username: byId("loginUsername").value.trim(),
          password: byId("loginPassword").value,
        }),
      });
      byId("loginPassword").value = "";
      authenticated = true;
      await loadState();
      showApp();
      connectSocket();
    } catch (error) {
      loginError.textContent = error.message;
    } finally {
      button.disabled = false;
    }
  });

  const setupForm = byId("setupForm");
  const setupError = byId("setupError");

  // Asks the server whether a password exists yet and swaps the login card over
  // to the first-run form when it does not.
  async function refreshSetupState() {
    try {
      const response = await fetch("/api/setup", { headers: { Accept: "application/json" } });
      if (!response.ok) return false;
      const data = await response.json();
      const required = Boolean(data.setupRequired);
      loginForm.hidden = required;
      setupForm.hidden = !required;
      if (required) {
        minPasswordLength = data.minPasswordLength || minPasswordLength;
        setTimeout(() => byId("setupToken").focus(), 0);
      }
      return required;
    } catch {
      return false;
    }
  }

  setupForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    setupError.textContent = "";
    const password = byId("setupPassword").value;
    if (password !== byId("setupConfirm").value) {
      setupError.textContent = "两次输入的密码不一致";
      return;
    }
    if ([...password].length < minPasswordLength) {
      setupError.textContent = `密码至少需要 ${minPasswordLength} 个字符`;
      return;
    }
    const button = setupForm.querySelector("button");
    button.disabled = true;
    try {
      await api("/api/setup", {
        method: "POST",
        body: JSON.stringify({
          token: byId("setupToken").value.trim(),
          username: byId("setupUsername").value.trim(),
          password,
        }),
      });
      byId("setupToken").value = "";
      byId("setupPassword").value = "";
      byId("setupConfirm").value = "";
      setupForm.hidden = true;
      loginForm.hidden = false;
      authenticated = true;
      await loadState();
      showApp();
      connectSocket();
      toast("管理员密码已设置");
    } catch (error) {
      setupError.textContent = error.message;
      // The token is single-use per server start; a conflict means someone else
      // finished setup first, so fall back to the normal login form.
      if (error.code === "already_configured") await refreshSetupState();
    } finally {
      button.disabled = false;
    }
  });

  byId("logoutButton").addEventListener("click", async () => {
    try {
      await api("/api/logout", { method: "POST", body: "{}" });
    } catch {
      // Local UI still ends the session even if the connection disappeared.
    }
    showLogin();
  });

  function showLogin() {
    authenticated = false;
    loginScreen.hidden = false;
    mainApp.hidden = true;
    closeSocket();
    setConnection("offline", "未连接");
    refreshSetupState().then((setupRequired) => {
      if (!setupRequired) setTimeout(() => byId("loginPassword").focus(), 0);
    });
  }

  function showApp() {
    loginScreen.hidden = true;
    mainApp.hidden = false;
    authenticated = true;
  }

  async function loadState() {
    const state = await api("/api/state");
    acceptState(state);
    return state;
  }

  function connectSocket() {
    if (!authenticated) return;
    closeSocket();
    setConnection("", "正在连接");
    const protocol = location.protocol === "https:" ? "wss:" : "ws:";
    socket = new WebSocket(`${protocol}//${location.host}/api/ws`);
    socket.addEventListener("open", () => {
      setConnection("online", "实时连接");
      clearTimeout(socketRetry);
      socketKeepalive = setInterval(() => {
        if (socket && socket.readyState === WebSocket.OPEN) socket.send("keepalive");
      }, 45000);
    });
    socket.addEventListener("message", (event) => {
      try {
        acceptState(JSON.parse(event.data));
      } catch {
        toast("收到无法解析的遥测数据", true);
      }
    });
    socket.addEventListener("close", (event) => {
      clearInterval(socketKeepalive);
      socketKeepalive = null;
      if (event.code === 1008) {
        showLogin();
        return;
      }
      if (authenticated) {
        setConnection("offline", "连接中断");
        clearTimeout(socketRetry);
        socketRetry = setTimeout(connectSocket, 2500);
      }
    });
    socket.addEventListener("error", () => setConnection("offline", "连接异常"));
  }

  function closeSocket() {
    clearTimeout(socketRetry);
    clearInterval(socketKeepalive);
    socketRetry = null;
    socketKeepalive = null;
    if (socket) {
      socket.onclose = null;
      socket.close();
      socket = null;
    }
  }

  function setConnection(kind, text) {
    connectionDot.className = `status-dot ${kind}`;
    connectionText.textContent = text;
  }

  function acceptState(state) {
    currentState = state;
    showApp();
    renderCapabilities(state.capabilities);
    renderPower(state);
    renderProfiles(state);
    renderTelemetry(state);
    renderFans(state);
    renderSettings(state);
    renderKeyboard(state);
    byId("driverVersion").textContent = state.driverVersion || "未知";
    // Last, so it can override what renderCapabilities decided for a sensor
    // that exists but is currently failing to read.
    renderDegraded(state);
  }

  const degradedLabels = {
    thermalProfile: "性能模式",
    profileChoices: "性能模式选项",
    fanControl: "风扇控制",
    fanTelemetry: "风扇转速",
    cpuTemperature: "CPU 温度",
    gpuTemperature: "GPU 温度",
    batteryLimiter: "电池限充",
    batteryCalibration: "电池校准",
    backlightTimeout: "背光超时",
    lcdOverride: "屏幕覆盖",
    usbCharging: "关机 USB 充电",
    bootAnimationSound: "开机动画与声音",
    keyboardPerZone: "键盘分区",
    keyboardEffects: "键盘灯效",
  };

  function renderDegraded(state) {
    const degraded = state.degraded || [];
    const notice = byId("degradedNotice");
    notice.hidden = degraded.length === 0;
    if (degraded.length > 0) {
      const names = degraded.map((key) => degradedLabels[key] || key);
      notice.textContent = `以下硬件读取失败，其余数据仍然有效：${names.join("、")}`;
    }
    if (degraded.includes("gpuTemperature")) {
      byId("gpuTempMetric").classList.add("unavailable-metric");
      byId("gpuTempNote").textContent = "读取失败";
    }
  }

  function renderCapabilities(capabilities) {
    document.querySelectorAll("[data-feature]").forEach((element) => {
      const feature = element.dataset.feature;
      element.hidden = !capabilities[feature];
    });
    byId("gpuTempMetric").classList.toggle("unavailable-metric", !capabilities.gpuTemperature);
    byId("gpuTempNote").textContent = capabilities.gpuTemperature ? "°C" : "传感器不可用";
    byId("keyboardSection").hidden = !capabilities.keyboardPerZone && !capabilities.keyboardEffects;
    if (!capabilities.keyboardPerZone && capabilities.keyboardEffects) selectKeyboardTab("effect");
    if (capabilities.keyboardPerZone && !capabilities.keyboardEffects) selectKeyboardTab("zone");
  }

  function renderPower(state) {
    const mapping = {
      ac: ["交流电源", "已接通外部电源"],
      battery: ["电池供电", "正在使用电池"],
      unknown: ["状态未知", "未找到供电传感器"],
    };
    const value = mapping[state.powerSource] || mapping.unknown;
    byId("powerText").textContent = value[0];
    byId("powerBadge").title = value[1];
  }

  function renderProfiles(state) {
    byId("currentProfile").textContent = state.profile.label || "—";
    const grid = byId("profileGrid");
    grid.replaceChildren();
    state.profile.available.forEach((profile) => {
      const button = document.createElement("button");
      button.type = "button";
      button.className = `profile-button${profile.value === state.profile.value ? " active" : ""}`;
      button.dataset.profile = profile.value;
      button.dataset.description = profile.description;
      const label = document.createElement("span");
      label.textContent = profile.label;
      const value = document.createElement("small");
      value.textContent = profile.value.toUpperCase();
      button.append(label, value);
      button.addEventListener("mouseenter", () => {
        byId("profileDescription").textContent = profile.description;
      });
      button.addEventListener("focus", () => {
        byId("profileDescription").textContent = profile.description;
      });
      button.addEventListener("click", () => setProfile(profile.value));
      grid.append(button);
    });
    const current = state.profile.available.find((item) => item.value === state.profile.value);
    byId("profileDescription").textContent = current?.description ||
      "当前模式不在此供电状态下的可选列表中。";
  }

  async function setProfile(profile) {
    try {
      const state = await api("/api/profile", {
        method: "PUT",
        body: JSON.stringify({ profile }),
      });
      acceptState(state);
      toast(`性能模式已切换为 ${state.profile.label}`);
    } catch (error) {
      toast(error.message, true);
      await reconcile();
    }
  }

  function renderTelemetry(state) {
    const cpuRpm = state.fans.cpuRpm;
    const gpuRpm = state.fans.gpuRpm;
    const cpuTemp = state.temperatures.cpu;
    const gpuTemp = state.temperatures.gpu;
    byId("cpuRpm").textContent = formatMetric(cpuRpm);
    byId("gpuRpm").textContent = formatMetric(gpuRpm);
    byId("cpuTemp").textContent = formatMetric(cpuTemp, 1);
    byId("gpuTemp").textContent = formatMetric(gpuTemp, 1);

    appendHistory(history.cpuRpm, cpuRpm);
    appendHistory(history.gpuRpm, gpuRpm);
    appendHistory(history.cpuTemp, cpuTemp);
    appendHistory(history.gpuTemp, gpuTemp);
    drawChart(byId("fanChart"), [
      { values: history.cpuRpm, color: "#59e7ff" },
      { values: history.gpuRpm, color: "#88f0c0" },
    ]);
    drawChart(byId("tempChart"), [
      { values: history.cpuTemp, color: "#59e7ff" },
      { values: history.gpuTemp, color: "#ffca71" },
    ]);
  }

  function formatMetric(value, decimals = 0) {
    return value == null ? "—" : Number(value).toFixed(decimals);
  }

  function appendHistory(series, value) {
    series.push(value == null ? null : Number(value));
    if (series.length > 60) series.shift();
  }

  function drawChart(canvas, series) {
    const bounds = canvas.getBoundingClientRect();
    if (!bounds.width || !bounds.height) return;
    const ratio = Math.min(window.devicePixelRatio || 1, 2);
    canvas.width = Math.round(bounds.width * ratio);
    canvas.height = Math.round(bounds.height * ratio);
    const context = canvas.getContext("2d");
    context.scale(ratio, ratio);
    const width = bounds.width;
    const height = bounds.height;
    context.clearRect(0, 0, width, height);
    context.strokeStyle = "rgba(148, 184, 193, .09)";
    context.lineWidth = 1;
    for (let y = 12; y < height; y += 24) {
      context.beginPath();
      context.moveTo(0, y + .5);
      context.lineTo(width, y + .5);
      context.stroke();
    }
    const values = series.flatMap((item) => item.values.filter((value) => value != null));
    if (values.length < 2) return;
    let min = Math.min(...values);
    let max = Math.max(...values);
    const padding = Math.max((max - min) * .25, max > 200 ? 100 : 4);
    min = Math.max(0, min - padding);
    max += padding;
    series.forEach((item) => {
      context.beginPath();
      context.strokeStyle = item.color;
      context.lineWidth = 1.7;
      context.shadowColor = item.color;
      context.shadowBlur = 7;
      let drawing = false;
      item.values.forEach((value, index) => {
        if (value == null) {
          drawing = false;
          return;
        }
        const x = item.values.length === 1 ? width : index / (item.values.length - 1) * width;
        const y = height - ((value - min) / Math.max(1, max - min)) * (height - 9) - 4;
        if (!drawing) context.moveTo(x, y);
        else context.lineTo(x, y);
        drawing = true;
      });
      context.stroke();
      context.shadowBlur = 0;
    });
  }

  function renderFans(state) {
    const buttons = byId("fanMode").querySelectorAll("button");
    buttons.forEach((button) => {
      button.classList.toggle("active", button.dataset.mode === state.fans.mode);
      button.disabled = button.dataset.mode === "manual" && !state.fans.manualAllowed;
    });
    if (state.fans.mode === "manual") {
      manualDraft = { cpu: state.fans.cpu, gpu: state.fans.gpu };
    }
    const cpuValue = state.fans.mode === "manual" ? state.fans.cpu : manualDraft.cpu;
    const gpuValue = state.fans.mode === "manual" ? state.fans.gpu : manualDraft.gpu;
    setRange(byId("cpuFanSlider"), cpuValue, byId("cpuFanOutput"), "%");
    setRange(byId("gpuFanSlider"), gpuValue, byId("gpuFanOutput"), "%");
    const disabled = state.fans.mode !== "manual" || !state.fans.manualAllowed;
    byId("cpuFanSlider").disabled = disabled;
    byId("gpuFanSlider").disabled = disabled;
    byId("fanSliders").classList.toggle("disabled", disabled);
    byId("fanInterlock").hidden = state.fans.manualAllowed;
  }

  byId("fanMode").addEventListener("click", async (event) => {
    const button = event.target.closest("button[data-mode]");
    if (!button || button.disabled) return;
    const mode = button.dataset.mode;
    await requestFans(mode, false);
  });

  const sendManualFans = coalesce(450, async (value) => {
    await requestFans("manual", false, value);
  });

  ["cpu", "gpu"].forEach((kind) => {
    const slider = byId(`${kind}FanSlider`);
    slider.addEventListener("input", () => {
      manualDraft[kind] = Number(slider.value);
      setRange(slider, slider.value, byId(`${kind}FanOutput`), "%");
      sendManualFans({ ...manualDraft });
    });
  });

  async function requestFans(mode, confirmed, values = manualDraft) {
    if (mode === "manual" && currentState && !currentState.fans.manualAllowed) {
      toast("Silent 模式下不能启用手动风扇", true);
      return;
    }
    const payload = { mode, cpu: values.cpu, gpu: values.gpu, confirmed };
    try {
      const state = await api("/api/fans", { method: "PUT", body: JSON.stringify(payload) });
      acceptState(state);
      if (mode !== "manual") toast(mode === "auto" ? "风扇已恢复自动控制" : "风扇已设为全速");
    } catch (error) {
      if (error.code === "confirmation_required") {
        const approved = await confirmAction(
          "低风扇转速警告",
          "高性能模式下将任一风扇设为低于 20% 可能造成过热。只有确认持续监控温度时才应继续。",
        );
        if (approved) return requestFans(mode, true, values);
      }
      toast(error.message, true);
      await reconcile();
    }
  }

  const settingBindings = [
    ["batteryLimiter", "battery-limiter"],
    ["backlightTimeout", "backlight-timeout"],
    ["lcdOverride", "lcd-override"],
    ["bootAnimationSound", "boot-animation-sound"],
  ];

  settingBindings.forEach(([id, endpoint]) => {
    byId(id).addEventListener("change", async (event) => {
      try {
        const state = await api(`/api/settings/${endpoint}`, {
          method: "PUT",
          body: JSON.stringify({ enabled: event.target.checked }),
        });
        acceptState(state);
      } catch (error) {
        toast(error.message, true);
        await reconcile();
      }
    });
  });

  byId("usbCharging").addEventListener("change", async (event) => {
    try {
      const state = await api("/api/settings/usb-charging", {
        method: "PUT",
        body: JSON.stringify({ value: Number(event.target.value) }),
      });
      acceptState(state);
    } catch (error) {
      toast(error.message, true);
      await reconcile();
    }
  });

  byId("calibrationButton").addEventListener("click", async () => {
    const enabled = Boolean(currentState?.settings.batteryCalibration);
    let confirmed = false;
    if (!enabled) {
      confirmed = await confirmAction(
        "开始电池校准？",
        "校准会对电池进行一次深度充放电。驱动只报告校准开关，不提供进度或完成原因；请保持交流电源连接并避免中断服务。",
      );
      if (!confirmed) return;
    }
    try {
      const state = await api("/api/settings/battery-calibration", {
        method: "PUT",
        body: JSON.stringify({ enabled: !enabled, confirmed }),
      });
      acceptState(state);
      toast(enabled ? "已请求停止电池校准" : "电池校准已启动");
    } catch (error) {
      toast(error.message, true);
      await reconcile();
    }
  });

  function renderSettings(state) {
    byId("batteryLimiter").checked = state.settings.batteryLimiter;
    byId("backlightTimeout").checked = state.settings.backlightTimeout;
    byId("lcdOverride").checked = state.settings.lcdOverride;
    byId("bootAnimationSound").checked = state.settings.bootAnimationSound;
    byId("usbCharging").value = String(state.settings.usbCharging);
    const running = state.settings.batteryCalibration;
    byId("calibrationStatus").textContent = running ? "驱动报告正在运行" : "未运行";
    byId("calibrationButton").textContent = running ? "停止" : "开始";
    byId("calibrationButton").classList.toggle("running", running);
  }

  byId("zoneTab").addEventListener("click", () => selectKeyboardTab("zone"));
  byId("effectTab").addEventListener("click", () => selectKeyboardTab("effect"));

  function selectKeyboardTab(tab) {
    const zone = tab === "zone";
    byId("zoneTab").classList.toggle("active", zone);
    byId("effectTab").classList.toggle("active", !zone);
    byId("zoneTab").setAttribute("aria-selected", String(zone));
    byId("effectTab").setAttribute("aria-selected", String(!zone));
    byId("zonePanel").hidden = !zone;
    byId("effectPanel").hidden = zone;
  }

  ["zone1", "zone2", "zone3", "zone4"].forEach((id) => {
    const input = byId(id);
    input.addEventListener("input", () => paintZone(input));
    input.addEventListener("change", () => sendZones(readZones()));
  });
  byId("zoneBrightness").addEventListener("input", (event) => {
    setRange(event.target, event.target.value, byId("zoneBrightnessOutput"), "%");
    sendZones(readZones());
  });

  const sendZones = coalesce(400, async (value) => {
    try {
      const state = await api("/api/keyboard/per-zone", {
        method: "PUT", body: JSON.stringify(value),
      });
      acceptState(state);
    } catch (error) {
      toast(error.message, true);
      await reconcile();
    }
  });

  function readZones() {
    return {
      zone1: byId("zone1").value.slice(1),
      zone2: byId("zone2").value.slice(1),
      zone3: byId("zone3").value.slice(1),
      zone4: byId("zone4").value.slice(1),
      brightness: Number(byId("zoneBrightness").value),
    };
  }

  function paintZone(input) {
    input.parentElement.style.setProperty("--zone-color", input.value);
  }

  const sendEffect = coalesce(450, async (value) => {
    try {
      const state = await api("/api/keyboard/effect", {
        method: "PUT", body: JSON.stringify(value),
      });
      acceptState(state);
    } catch (error) {
      toast(error.message, true);
      await reconcile();
    }
  });

  ["effectMode", "effectColor", "effectDirection", "effectSpeed", "effectBrightness"].forEach((id) => {
    const input = byId(id);
    input.addEventListener("input", () => {
      updateEffectControls();
      setRange(byId("effectSpeed"), byId("effectSpeed").value, byId("effectSpeedOutput"), "");
      setRange(byId("effectBrightness"), byId("effectBrightness").value, byId("effectBrightnessOutput"), "%");
      sendEffect(readEffect());
    });
  });

  function readEffect() {
    const [red, green, blue] = hexToRGB(byId("effectColor").value);
    return {
      mode: Number(byId("effectMode").value),
      speed: Number(byId("effectSpeed").value),
      brightness: Number(byId("effectBrightness").value),
      direction: Number(byId("effectDirection").value),
      red, green, blue,
    };
  }

  function renderKeyboard(state) {
    if (state.keyboard.perZone) {
      const zone = state.keyboard.perZone;
      ["zone1", "zone2", "zone3", "zone4"].forEach((id) => {
        const input = byId(id);
        if (document.activeElement !== input) input.value = `#${zone[id]}`;
        paintZone(input);
      });
      setRange(byId("zoneBrightness"), zone.brightness, byId("zoneBrightnessOutput"), "%");
    }
    if (state.keyboard.effect) {
      const effect = state.keyboard.effect;
      setControlValue(byId("effectMode"), effect.mode);
      // The EC reports 0 when direction does not apply to the active mode, and
      // the control only offers the two real directions.
      setControlValue(byId("effectDirection"), effect.direction || 1);
      setControlValue(byId("effectColor"), rgbToHex(effect.red, effect.green, effect.blue));
      setRange(byId("effectSpeed"), effect.speed, byId("effectSpeedOutput"), "");
      setRange(byId("effectBrightness"), effect.brightness, byId("effectBrightnessOutput"), "%");
      updateEffectControls();
    }
  }

  function updateEffectControls() {
    const ignored = effectIgnored[Number(byId("effectMode").value)] || [];
    document.querySelectorAll("[data-effect-param]").forEach((wrapper) => {
      const disabled = ignored.includes(wrapper.dataset.effectParam);
      wrapper.classList.toggle("ignored", disabled);
      wrapper.querySelectorAll("input, select").forEach((input) => { input.disabled = disabled; });
    });
  }

  function setControlValue(control, value) {
    if (document.activeElement !== control) control.value = String(value);
  }

  function setRange(input, value, output, suffix) {
    if (document.activeElement !== input) input.value = String(value);
    output.value = `${value}${suffix}`;
    const minimum = Number(input.min || 0);
    const maximum = Number(input.max || 100);
    const fill = (Number(value) - minimum) / (maximum - minimum) * 100;
    input.style.setProperty("--fill", `${fill}%`);
  }

  function hexToRGB(hex) {
    const value = Number.parseInt(hex.slice(1), 16);
    return [(value >> 16) & 255, (value >> 8) & 255, value & 255];
  }

  function rgbToHex(red, green, blue) {
    return `#${[red, green, blue].map((value) => Number(value).toString(16).padStart(2, "0")).join("")}`;
  }

  function coalesce(delay, worker) {
    let timer = null;
    let pending;
    let running = false;
    const run = async () => {
      if (running || pending === undefined) return;
      const value = pending;
      pending = undefined;
      running = true;
      try {
        await worker(value);
      } finally {
        running = false;
        if (pending !== undefined) {
          clearTimeout(timer);
          timer = setTimeout(run, delay);
        }
      }
    };
    return (value) => {
      pending = value;
      clearTimeout(timer);
      timer = setTimeout(run, delay);
    };
  }

  function confirmAction(title, message) {
    byId("confirmTitle").textContent = title;
    byId("confirmMessage").textContent = message;
    confirmDialog.showModal();
    return new Promise((resolve) => {
      confirmDialog.addEventListener("close", () => resolve(confirmDialog.returnValue === "confirm"), { once: true });
    });
  }

  function toast(message, isError = false) {
    const element = document.createElement("div");
    element.className = `toast${isError ? " error" : ""}`;
    element.textContent = message;
    byId("toastRegion").append(element);
    setTimeout(() => element.remove(), 4200);
  }

  async function reconcile() {
    try {
      await loadState();
    } catch {
      // Authentication or connectivity UI is handled by api().
    }
  }

  window.addEventListener("resize", () => {
    if (currentState) renderTelemetry(currentState);
  });

  async function boot() {
    setRange(byId("cpuFanSlider"), 50, byId("cpuFanOutput"), "%");
    setRange(byId("gpuFanSlider"), 50, byId("gpuFanOutput"), "%");
    setRange(byId("zoneBrightness"), 100, byId("zoneBrightnessOutput"), "%");
    setRange(byId("effectSpeed"), 5, byId("effectSpeedOutput"), "");
    setRange(byId("effectBrightness"), 100, byId("effectBrightnessOutput"), "%");
    ["zone1", "zone2", "zone3", "zone4"].forEach((id) => paintZone(byId(id)));
    updateEffectControls();
    try {
      authenticated = true;
      await loadState();
      connectSocket();
    } catch (error) {
      if (error.code !== "unauthorized") {
        loginError.textContent = "无法连接控制服务";
      }
      showLogin();
    }
  }

  boot();
})();
