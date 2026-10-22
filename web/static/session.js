// Session page client. Applies server-rendered UIEvents (see agent.go),
// renders assistant Markdown, and turns form posts into fetches.
(() => {
  const {id} = document.getElementById("session-config").dataset;
  let cursor = document.getElementById("session-config").dataset.cursor;

  const $ = (sel) => document.querySelector(sel);
  const transcript = $("#transcript");
  const status = $("#status");
  const dialogs = $("#dialogs");
  const form = $("#promptform");
  const message = $("#message");

  const frag = (html) => document.createRange().createContextualFragment(html);
  const nearBottom = () =>
    window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 120;
  const toBottom = () => window.scrollTo(0, document.documentElement.scrollHeight);

  // Keep source separate from rendered HTML, both for streaming and copying.
  // Raw HTML is disabled; markdown-it also rejects unsafe link protocols.
  const markdown = window.markdownit({html: false, linkify: true, typographer: false});
  markdown.linkify.set({fuzzyLink: true});
  // Do not load remote images automatically (tracking URLs, large images, etc.).
  markdown.renderer.rules.image = (tokens, idx) => {
    const token = tokens[idx];
    return markdown.utils.escapeHtml(`[image: ${token.content || token.attrGet("src") || ""}]`);
  };
  const sources = new WeakMap();
  const dirty = new Set();
  function renderMarkdown() {
    transcript.querySelectorAll(".assistant .text").forEach(el => {
      if (sources.has(el) && !dirty.has(el)) return;
      if (!sources.has(el)) sources.set(el, el.textContent);
      el.classList.add("markdown");
      el.innerHTML = markdown.render(sources.get(el));
      el.querySelectorAll("pre > code").forEach(code => {
        const pre = code.parentElement;
        const wrapper = document.createElement("div");
        wrapper.className = "code-block";
        const button = document.createElement("button");
        button.type = "button";
        button.className = "copy-code";
        button.textContent = "Copy";
        button.title = "Copy code block";
        button.setAttribute("aria-label", button.title);
        button.setAttribute("aria-live", "polite");
        pre.replaceWith(wrapper);
        wrapper.append(button, pre);
      });
    });
    dirty.clear();
  }
  renderMarkdown();
  // Reloading an active tool should show its newest published output.
  transcript.querySelectorAll(".tool-live pre").forEach(pre => { pre.scrollTop = pre.scrollHeight; });

  // --- event handlers, keyed by UIEvent.t --------------------------------
  const handlers = {
    append(ev) {
      const live = ev.id && document.getElementById(ev.id);
      if (live) {
        if (ev.html) {
          const replacement = frag(ev.html);
          // Keep the user's expanded/collapsed choice when a tool completes.
          if (live.matches("details") && replacement.firstElementChild?.matches("details")) {
            replacement.firstElementChild.open = live.open;
          }
          live.replaceWith(replacement);
        } else {
          live.remove();
        }
      } else if (ev.html) {
        transcript.append(frag(ev.html));
      }
    },
    live_start(ev) {
      $("#live")?.remove();
      transcript.append(frag(ev.html));
    },
    block(ev) {
      $("#live")?.append(frag(ev.html));
    },
    delta(ev) {
      const el = $(`#live [data-idx="${ev.idx}"]`);
      if (el) {
        sources.set(el, (sources.get(el) ?? el.textContent) + ev.s);
        dirty.add(el);
      }
    },
    tool_delta(ev) {
      const pre = document.getElementById(ev.id)?.querySelector("pre");
      if (pre) {
        const follow = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
        pre.append(document.createTextNode(ev.s));
        if (follow) pre.scrollTop = pre.scrollHeight;
      }
    },
    tool_reset(ev) {
      const pre = document.getElementById(ev.id)?.querySelector("pre");
      if (pre) pre.textContent = "";
    },
    status(ev) {
      status.innerHTML = ev.html;
      document.body.classList.toggle("streaming", !!ev.streaming);
    },
    dialog(ev) {
      dialogs.append(frag(ev.html));
      dialogs.querySelector(`[data-dialog="${ev.id}"] input, [data-dialog="${ev.id}"] textarea`)?.focus();
    },
    dialog_close(ev) {
      dialogs.querySelector(`[data-dialog="${ev.id}"]`)?.remove();
    },
    notice(ev) {
      transcript.append(frag(ev.html));
    },
    editor(ev) {
      message.value = ev.s || "";
      autosize();
    },
    name(ev) {
      const nm = $(".sessname .nm");
      nm.textContent = ev.s || nm.dataset.fallback;
      document.title = `${nm.textContent} · pircd`;
      const input = $("#rename-input");
      if (input && document.activeElement !== input) input.value = ev.s || "";
      // Sidebar entry: keep the status dot, replace the text.
      const title = document.querySelector("#project-tree .conv-link.current .conv-title");
      if (title && ev.s) {
        title.lastChild.nodeType === Node.TEXT_NODE ? title.lastChild.textContent = ev.s : title.append(ev.s);
      }
    },
    reload() {
      location.reload();
    },
  };

  function apply(events) {
    const stick = nearBottom();
    for (const ev of events) {
      handlers[ev.t]?.(ev);
    }
    renderMarkdown();
    updateCopyButtons();
    if (stick) toBottom();
  }

  // --- live stream ---------------------------------------------------------
  let es;
  function connect() {
    es?.close();
    es = new EventSource(`/s/${encodeURIComponent(id)}/events?since=${encodeURIComponent(cursor)}`);
    es.onmessage = (e) => {
      if (e.lastEventId) cursor = e.lastEventId;
      apply(JSON.parse(e.data));
    };
  }
  connect();
  // Phones drop connections while asleep; reconnect when we come back.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && es.readyState === EventSource.CLOSED) connect();
  });

  // --- forms -----------------------------------------------------------------
  async function post(url, body) {
    const res = await fetch(url, {method: "POST", body, headers: {"X-Pircd": "1"}});
    const text = await res.text();
    if (!res.ok) throw new Error(text || res.statusText);
    return text;
  }

  function showError(err) {
    handlers.notice({html: `<div class="notice error"></div>`});
    transcript.lastElementChild.textContent = String(err.message || err);
    toBottom();
  }

  // --- attachments -----------------------------------------------------------
  // Files from the "+" picker, drag and drop, or a pasted screenshot wait here
  // and are sent with the next prompt; the server saves them and appends their
  // paths to the message.
  const attachments = $("#attachments");
  const fileInput = $("#file-input");
  const composer = $(".composer");
  let pending = [];

  const fileSize = (n) => n < 1024 ? `${n} B`
    : n < 1024 * 1024 ? `${(n / 1024).toFixed(0)} KB`
    : `${(n / 1024 / 1024).toFixed(1)} MB`;

  function setAttachments(files) {
    pending = files;
    attachments.replaceChildren(...pending.map((f, i) => {
      const chip = document.createElement("div");
      chip.className = "attachment";
      if (f.type.startsWith("image/")) {
        const img = document.createElement("img");
        img.alt = "";
        img.src = URL.createObjectURL(f);
        img.onload = () => URL.revokeObjectURL(img.src);
        chip.append(img);
      }
      const name = document.createElement("span");
      name.className = "name";
      name.textContent = f.name;
      name.title = f.name;
      const size = document.createElement("span");
      size.className = "size";
      size.textContent = fileSize(f.size);
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "remove";
      remove.dataset.i = i;
      remove.textContent = "×";
      remove.title = `Remove ${f.name}`;
      remove.setAttribute("aria-label", remove.title);
      chip.append(name, size, remove);
      return chip;
    }));
    attachments.hidden = !pending.length;
    syncEmpty();
  }
  const addFiles = (files) => { if (files.length) setAttachments([...pending, ...files]); };
  const syncEmpty = () => form.classList.toggle("empty", !message.value.trim() && !pending.length);
  syncEmpty();

  fileInput.addEventListener("change", () => {
    addFiles(fileInput.files);
    fileInput.value = "";
  });
  attachments.addEventListener("click", (e) => {
    const btn = e.target.closest(".remove");
    if (btn) setAttachments(pending.filter((_, i) => i !== Number(btn.dataset.i)));
  });

  // Accept drops anywhere on the page, so a missed drop doesn't navigate away.
  const hasFiles = (e) => e.dataTransfer?.types.includes("Files");
  let dragDepth = 0;
  document.addEventListener("dragenter", (e) => {
    if (!hasFiles(e)) return;
    dragDepth++;
    composer.classList.add("dropping");
  });
  document.addEventListener("dragleave", (e) => {
    if (hasFiles(e) && --dragDepth <= 0) {
      dragDepth = 0;
      composer.classList.remove("dropping");
    }
  });
  document.addEventListener("dragover", (e) => { if (hasFiles(e)) e.preventDefault(); });
  document.addEventListener("drop", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth = 0;
    composer.classList.remove("dropping");
    addFiles(e.dataTransfer.files);
    message.focus();
  });

  // Pasting a screenshot attaches it; pasted text is left alone.
  message.addEventListener("paste", (e) => {
    const data = e.clipboardData;
    if (!data?.files.length || data.types.includes("text/plain")) return;
    e.preventDefault();
    addFiles(data.files);
  });

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const btn = e.submitter;
    const action = btn?.getAttribute("formaction") || form.action;
    const body = new FormData(form, btn);
    body.delete("files");
    const isPrompt = action === form.action;
    const files = isPrompt ? pending : [];
    for (const f of files) body.append("files", f, f.name);
    const sent = message.value;
    if (isPrompt) {
      message.value = "";
      setAttachments([]);
    }
    autosize();
    try {
      const reply = await post(action, body);
      if (isPrompt) toBottom();
      // abort returns the text of any queued messages it dropped
      else if (reply && !message.value) { message.value = reply; autosize(); }
    } catch (err) {
      if (isPrompt && !message.value) message.value = sent;
      if (isPrompt && !pending.length) setAttachments(files);
      autosize();
      showError(err);
    }
  });

  // Desktop: Enter sends, Shift+Enter inserts a newline. Touch devices keep
  // the on-screen keyboard's Enter as a newline and send with the button.
  const touch = window.matchMedia("(pointer: coarse)");
  const setPlaceholder = () => {
    message.placeholder = touch.matches
      ? "Message pi"
      : "Message pi (Enter to send, Shift+Enter for newline)";
  };
  setPlaceholder();
  touch.addEventListener("change", setPlaceholder);

  message.addEventListener("keydown", (e) => {
    if (suggestKey(e)) return;
    if (e.key !== "Enter" || e.isComposing || touch.matches) return;
    if (e.shiftKey || e.altKey) return;
    e.preventDefault();
    form.requestSubmit($("#send"));
  });

  function autosize() {
    message.style.height = "auto";
    message.style.height = Math.min(message.scrollHeight, window.innerHeight * 0.4) + "px";
    syncEmpty();
  }
  message.addEventListener("input", autosize);

  dialogs.addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    const body = new FormData(f, e.submitter);
    f.remove();
    try {
      await post(f.action, body);
    } catch (err) {
      showError(err);
    }
  });

  // --- model / effort picker -------------------------------------------------
  const picker = document.createElement("dialog");
  picker.className = "model-picker";
  document.body.append(picker);
  let pickerBusy = false;
  let modelOpts = [];
  const pickerURL = `/s/${encodeURIComponent(id)}/model`;

  // Scores a whitespace-tokenized fuzzy query against target text; returns null on mismatch.
  function fuzzyScore(query, target) {
    const tokens = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
    if (!tokens.length) return 0;
    const text = target.toLowerCase();
    let total = 0;
    for (const tok of tokens) {
      const subIdx = text.indexOf(tok);
      if (subIdx !== -1) {
        total += 100 - subIdx * 0.5 + (subIdx === 0 || /[\s/\-_.]/.test(text[subIdx - 1]) ? 20 : 0);
        continue;
      }
      let ti = 0, score = 0, prevMatch = -2;
      for (let qi = 0; qi < tok.length; qi++) {
        const ch = tok[qi];
        let found = -1;
        while (ti < text.length) {
          if (text[ti] === ch) { found = ti++; break; }
          ti++;
        }
        if (found === -1) return null;
        score += 10;
        if (found === prevMatch + 1) score += 12;
        if (found === 0 || /[\s/\-_.]/.test(text[found - 1])) score += 10;
        score -= (found - prevMatch - 1) * 0.5;
        prevMatch = found;
      }
      total += score;
    }
    return total;
  }

  function setActiveOpt(opt, scroll = true) {
    picker.querySelectorAll(".model-opt.active").forEach(el => el.classList.remove("active"));
    if (opt) {
      opt.classList.add("active");
      if (scroll) opt.scrollIntoView({block: "nearest"});
    }
  }

  function filterModels(query) {
    const list = picker.querySelector(".model-list");
    const empty = picker.querySelector(".model-empty");
    if (!list || !modelOpts.length) return;
    const q = query.trim();
    if (!q) {
      for (const opt of modelOpts) {
        opt.hidden = false;
        list.insertBefore(opt, empty);
      }
      if (empty) empty.hidden = true;
      setActiveOpt(picker.querySelector(".model-opt.sel") || modelOpts[0], false);
      return;
    }
    const matches = [];
    for (const opt of modelOpts) {
      const score = fuzzyScore(q, opt.dataset.search || "");
      if (score === null) {
        opt.hidden = true;
      } else {
        opt.hidden = false;
        matches.push({opt, score});
      }
    }
    matches.sort((a, b) => b.score - a.score);
    for (const m of matches) list.insertBefore(m.opt, empty);
    if (empty) empty.hidden = matches.length > 0;
    list.scrollTop = 0;
    setActiveOpt(matches[0]?.opt || null, false);
  }

  function selectModel(opt) {
    if (!opt || pickerBusy) return;
    const body = new URLSearchParams();
    body.set("setting", "model");
    body.set("modelId", opt.dataset.modelId);
    body.set("provider", opt.dataset.provider);
    updatePicker(body);
  }

  function colorEffortSlider(control, index, count) {
    const colors = count === 2 ? ["red", "purple"]
      : count === 3 ? ["red", "blue", "purple"]
      : count === 4 ? ["red", "green", "blue", "purple"]
      : ["red", "orange", "green", "blue", "purple"];
    control.dataset.effortColor = colors[Math.min(Math.max(index, 0), colors.length - 1)];
  }

  function syncEffortSlider() {
    const control = picker.querySelector(".effort-control");
    if (!control) return;
    const slider = control.querySelector("[data-effort-slider]");
    const levels = Array.from(control.querySelectorAll("[data-effort-level]"));
    const index = levels.findIndex(el => el.dataset.effortLevel === control.dataset.currentEffort);
    slider.max = Math.max(0, levels.length - 1);
    slider.value = Math.max(0, index);
    colorEffortSlider(control, index, levels.length);
    slider.disabled = levels.length < 2;
    slider.hidden = levels.length < 2;
    control.querySelector(".effort-unavailable").hidden = levels.length > 1;
    // Do not imply a supported setting when the current value is absent.
    slider.setAttribute("aria-valuetext", index >= 0 ? levels[index].dataset.effortLevel : control.dataset.currentEffort);
  }

  async function updatePicker(body) {
    if (pickerBusy) return;
    pickerBusy = true;
    const prevFilter = picker.querySelector("[data-model-filter]")?.value || "";
    const keepFilter = body && body.get("setting") !== "model";
    if (body) body.set("makeDefault", String(!!picker.querySelector("[data-make-default]")?.checked));
    picker.setAttribute("aria-busy", "true");
    picker.querySelectorAll("select, input, button").forEach(el => el.disabled = true);
    try {
      let html;
      if (body) html = await post(pickerURL, body);
      else {
        const res = await fetch(pickerURL);
        html = await res.text();
        if (!res.ok) throw new Error(html);
      }
      picker.innerHTML = html;
      modelOpts = Array.from(picker.querySelectorAll(".model-opt"));
      if (!picker.open) picker.showModal();
      const input = picker.querySelector("[data-model-filter]");
      if (input && keepFilter && prevFilter) {
        input.value = prevFilter;
        filterModels(prevFilter);
      } else {
        const sel = picker.querySelector(".model-opt.sel");
        setActiveOpt(sel || modelOpts[0], true);
      }
      if (!keepFilter) input?.focus();
    } catch (err) {
      // A failed save must not leave the checkbox implying success.
      const makeDefault = picker.querySelector("[data-make-default]");
      if (makeDefault && body?.get("makeDefault") === "true") makeDefault.checked = false;
      showError(err);
    } finally {
      pickerBusy = false;
      picker.removeAttribute("aria-busy");
      picker.querySelectorAll("select, input, button").forEach(el => el.disabled = false);
      syncEffortSlider();
      if (keepFilter) picker.querySelector("[data-effort-slider]")?.focus();
    }
  }
  status.addEventListener("click", e => {
    if (e.target.closest("[data-model-picker]")) updatePicker();
    if (e.target.closest("[data-stop-shell]")) form.requestSubmit($("#abort"));
  });
  picker.addEventListener("click", e => {
    if (e.target === picker || e.target.closest("[data-picker-close]")) {
      picker.close();
      return;
    }
    const opt = e.target.closest(".model-opt");
    if (opt) selectModel(opt);
  });
  picker.addEventListener("input", e => {
    if (e.target.matches("[data-model-filter]")) filterModels(e.target.value);
    if (e.target.matches("[data-effort-slider]")) {
      const control = e.target.closest(".effort-control");
      const levels = control.querySelectorAll("[data-effort-level]");
      const index = Number(e.target.value);
      const level = levels[index]?.dataset.effortLevel;
      colorEffortSlider(control, index, levels.length);
      control.querySelector("[data-effort-value]").textContent = level || "";
      e.target.setAttribute("aria-valuetext", level || "");
    }
  });
  picker.addEventListener("keydown", e => {
    if (!e.target.matches("[data-model-filter]")) return;
    const visible = Array.from(picker.querySelectorAll(".model-opt:not([hidden])"));
    if (!visible.length) return;
    const cur = visible.findIndex(el => el.classList.contains("active"));
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActiveOpt(visible[(cur + 1) % visible.length]);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveOpt(visible[(cur - 1 + visible.length) % visible.length]);
    } else if (e.key === "Enter") {
      e.preventDefault();
      selectModel(visible[cur >= 0 ? cur : 0]);
    }
  });
  picker.addEventListener("change", e => {
    if (e.target.matches("[data-make-default]")) {
      if (e.target.checked) updatePicker(new URLSearchParams({setting: "default"}));
      return;
    }
    const slider = e.target;
    if (!slider.matches("[data-effort-slider]")) return;
    const level = slider.closest(".effort-control").querySelectorAll("[data-effort-level]")[Number(slider.value)]?.dataset.effortLevel;
    if (!level) return;
    const body = new URLSearchParams();
    body.set("setting", "effort");
    body.set("level", level);
    updatePicker(body);
  });

  // A response spans all assistant chunks and tool results between prompts.
  // Only the entire finished response gets a copy action.
  function responseGroups() {
    const groups = [];
    let group = [];
    for (const el of transcript.children) {
      if (el.matches(".msg.user")) {
        if (group.length) groups.push(group);
        group = [];
      } else if (el.matches(".msg")) group.push(el);
    }
    if (group.length) groups.push(group);
    return groups;
  }

  function updateCopyButtons() {
    transcript.querySelectorAll(".copy-message").forEach(el => el.remove());
    const groups = responseGroups();
    groups.forEach((group, i) => {
      if (i === groups.length - 1 && document.body.classList.contains("streaming")) return;
      const assistants = group.filter(el => el.matches(".assistant:not(.live)"));
      if (!assistants.length) return;
      const button = document.createElement("button");
      button.type = "button";
      button.className = "copy-message";
      button.textContent = "Copy";
      button.title = "Copy full response text";
      button.setAttribute("aria-label", button.title);
      button.responseMessages = assistants;
      assistants[assistants.length - 1].append(button);
    });
  }
  updateCopyButtons();

  // Delegation also covers completed responses inserted by the live stream.
  transcript.addEventListener("click", async (e) => {
    const copyBtn = e.target.closest(".copy-message, .copy-code");
    if (!copyBtn) return;
    const text = copyBtn.matches(".copy-code")
      ? copyBtn.parentElement.querySelector("pre > code").textContent
      : copyBtn.responseMessages.flatMap(message =>
        Array.from(message.querySelectorAll(":scope > .text"), el => sources.get(el) ?? el.textContent)
      ).join("\n\n");
    const flash = (label) => {
      clearTimeout(copyBtn.copyTimer);
      copyBtn.textContent = label;
      copyBtn.copyTimer = setTimeout(() => { copyBtn.textContent = "Copy"; }, 1500);
    };
    if (!text) return flash("Nothing yet");
    try {
      // navigator.clipboard needs a secure context (https or localhost)
      await navigator.clipboard.writeText(text);
    } catch {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.append(ta);
      ta.select();
      ta.setSelectionRange(0, text.length);
      let ok = false;
      try {
        ok = document.execCommand("copy");
      } catch {
        // Some browsers reject the fallback as well.
      } finally {
        ta.remove();
        copyBtn.focus({preventScroll: true});
      }
      if (!ok) return flash("Copy failed");
    }
    flash("Copied ✓");
  });


  // --- slash-command suggestions ---------------------------------------------
  // Typing "/" at the start of an empty prompt lists pi's extension commands,
  // prompt templates and skills; picking one inserts "/name ".
  const suggest = $("#suggest");
  let commands = null;
  let suggestions = [];
  let active = 0;
  const slashQuery = () => /^\/(\S*)$/.exec(message.value)?.[1];

  function loadCommands() {
    commands ??= fetch(`/s/${encodeURIComponent(id)}/commands`)
      .then(res => res.ok ? res.json() : Promise.reject())
      .then(list => list || [])
      .catch(() => { commands = null; return []; });
    return commands;
  }

  function hideSuggest() {
    suggest.hidden = true;
    suggestions = [];
  }

  async function updateSuggest() {
    if (slashQuery() === undefined) return hideSuggest();
    const list = await loadCommands();
    const q = slashQuery()?.toLowerCase();
    if (q === undefined) return hideSuggest();
    suggestions = list
      .filter(c => c.name.toLowerCase().includes(q))
      .sort((a, b) => b.name.toLowerCase().startsWith(q) - a.name.toLowerCase().startsWith(q))
      .slice(0, 8);
    if (!suggestions.length) return hideSuggest();
    active = 0;
    renderSuggest();
    suggest.hidden = false;
  }

  function renderSuggest() {
    suggest.replaceChildren(...suggestions.map((c, i) => {
      const li = document.createElement("li");
      li.role = "option";
      li.dataset.i = i;
      li.className = i === active ? "active" : "";
      li.setAttribute("aria-selected", String(i === active));
      const name = document.createElement("span");
      name.className = "cmd";
      name.textContent = "/" + c.name;
      const desc = document.createElement("span");
      desc.className = "desc";
      desc.textContent = c.description || c.source;
      li.append(name, desc);
      return li;
    }));
  }

  function pickSuggestion(i) {
    const c = suggestions[i];
    if (!c) return;
    message.value = `/${c.name} `;
    hideSuggest();
    message.focus();
    autosize();
  }

  // Handles keys while the list is open; returns true when it used the key.
  function suggestKey(e) {
    if (suggest.hidden || e.isComposing) return false;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      active = (active + (e.key === "ArrowDown" ? 1 : -1) + suggestions.length) % suggestions.length;
      renderSuggest();
    } else if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey)) {
      pickSuggestion(active);
    } else if (e.key === "Escape") {
      hideSuggest();
    } else {
      return false;
    }
    e.preventDefault();
    return true;
  }

  message.addEventListener("input", updateSuggest);
  form.addEventListener("submit", hideSuggest);
  // Keep focus in the prompt box while picking.
  suggest.addEventListener("mousedown", (e) => e.preventDefault());
  suggest.addEventListener("click", (e) => {
    const li = e.target.closest("li");
    if (li) pickSuggestion(Number(li.dataset.i));
  });

  // --- session actions -------------------------------------------------------
  const actions = $("#session-actions");
  const stats = $("#session-stats");
  $("#actions-toggle").addEventListener("click", async () => {
    actions.showModal();
    stats.textContent = "Loading…";
    try {
      const res = await fetch(`/s/${encodeURIComponent(id)}/stats`);
      if (!res.ok) throw new Error(await res.text());
      stats.innerHTML = await res.text();
    } catch {
      stats.textContent = "Stats are unavailable while pi isn't running.";
    }
  });
  actions.addEventListener("click", (e) => {
    if (e.target === actions || e.target.closest("[data-actions-close]")) actions.close();
  });
  actions.addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    if (f.hasAttribute("data-release") && document.body.classList.contains("streaming") &&
        !confirm("pi is still working. Release anyway? The current run will be stopped.")) return;
    const buttons = f.querySelectorAll("button");
    buttons.forEach(b => b.disabled = true);
    // Compaction can take minutes; its progress shows in the status line.
    if (f.action.endsWith("/compact")) actions.close();
    try {
      const url = await post(f.action, new FormData(f));
      if (url) { location.href = url; return; }
      actions.close();
      if (f.action.endsWith("/compact")) f.reset();
    } catch (err) {
      actions.close();
      showError(err);
    } finally {
      buttons.forEach(b => b.disabled = false);
    }
  });

  toBottom();
})();
