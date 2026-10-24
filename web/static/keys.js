// Global navigation shortcuts and the "?" help overlay. Loaded on every page;
// each binding only applies (and is only listed) where its target exists.
(() => {
  const mac = typeof navigator !== "undefined" &&
    /mac|iphone|ipad/i.test(navigator.userAgentData?.platform || navigator.platform || "");

  // "Mod" is ⌘ on Apple platforms and Ctrl elsewhere. Modifiers must match exactly,
  // so Mod+K doesn't also fire on Mod+Shift+K.
  function matches(combo, e, isMac = mac) {
    const parts = combo.split("+");
    const key = parts.pop().toLowerCase();
    const want = {ctrl: false, meta: false, alt: false, shift: false};
    for (const p of parts) {
      if (p === "Mod") want[isMac ? "meta" : "ctrl"] = true;
      else want[p.toLowerCase()] = true;
    }
    return e.ctrlKey === want.ctrl && e.metaKey === want.meta &&
      e.altKey === want.alt && e.shiftKey === want.shift &&
      (e.key || "").toLowerCase() === key;
  }

  function label(combo, isMac = mac) {
    const names = isMac
      ? {Mod: "⌘", Alt: "⌥", Shift: "⇧", ArrowUp: "↑", ArrowDown: "↓"}
      : {Mod: "Ctrl", ArrowUp: "↑", ArrowDown: "↓"};
    const keys = combo.split("+").map(k => names[k] || (k.length === 1 ? k.toUpperCase() : k));
    return isMac ? keys.join("") : keys.join("+");
  }

  // The session `step` away from `current`, wrapping around at either end. With no
  // current session, forward starts at the top and backward at the bottom.
  function adjacent(hrefs, current, step) {
    if (!hrefs.length) return null;
    const at = hrefs.indexOf(current);
    const from = at < 0 ? (step > 0 ? -1 : 0) : at;
    return hrefs[(from + step + hrefs.length) % hrefs.length];
  }

  if (typeof module !== "undefined") module.exports = {matches, label, adjacent};
  if (typeof document === "undefined") return;

  const $ = id => document.getElementById(id);
  const visible = el => !!el && el.getClientRects().length > 0;
  // Unsent prompt text isn't saved anywhere, so don't drop it silently.
  const go = href => {
    if ($("message")?.value.trim() && !confirm("Leave this page? Your unsent message will be lost.")) return;
    location.href = href;
  };

  const currentSession = () => location.pathname.startsWith("/s/") ? location.pathname : null;
  // Active (attached or running) sessions in sidebar order, folded or filtered out or
  // not, plus the open one.
  const sessionLinks = () => [...new Set(Array.from(document.querySelectorAll("#project-tree .conv-link"))
    .filter(a => a.querySelector(".dot") || a.getAttribute("href") === currentSession())
    .map(a => a.getAttribute("href")))];
  const step = dir => {
    const href = adjacent(sessionLinks(), currentSession(), dir);
    if (href) go(href);
  };

  // The session page's project, from its "+ New" button. Drafts have none, so from
  // a draft "new session" goes on to the directory picker.
  const projectDir = () =>
    document.querySelector("header.top form[action='/draft'] input[name=dir]")?.value;

  function setSidebar(open) {
    const toggle = $("sidebar-toggle");
    if (visible(toggle) && document.body.classList.contains("sidebar-open") !== open) toggle.click();
  }

  function findSession() {
    const picker = $("proj-filter");
    if (picker) { picker.focus(); picker.select(); return; }
    const filter = $("home-filter");
    setSidebar(true);
    if ($("search-form")?.hidden) $("toggle-filter").click();
    filter.focus();
    filter.select();
  }

  function newSession() {
    const dir = projectDir();
    go(dir ? `/draft?dir=${encodeURIComponent(dir)}` : "/new");
  }

  const bindings = [
    {keys: "Mod+K", desc: "Find a session or project", when: () => $("home-filter") || $("proj-filter"), run: findSession},
    {keys: "Mod+B", desc: "Show or hide the sidebar", when: () => visible($("sidebar-toggle")),
      run: () => setSidebar(!document.body.classList.contains("sidebar-open"))},
    {keys: "Alt+ArrowUp", desc: "Previous active session", when: () => sessionLinks().length > (currentSession() ? 1 : 0), run: () => step(-1)},
    {keys: "Alt+ArrowDown", desc: "Next active session", when: () => sessionLinks().length > (currentSession() ? 1 : 0), run: () => step(1)},
    {keys: "Mod+Shift+O", desc: () => projectDir() ? "New session in this project" : "New session",
      when: () => location.pathname !== "/new", run: newSession},
    {keys: "Mod+Shift+H", desc: "Home", when: () => location.pathname !== "/", run: () => go("/")},
    {keys: "Mod+/", desc: "Show keyboard shortcuts", when: () => true, run: toggleHelp},
  ];

  let help;
  function toggleHelp() {
    if (help?.open) { help.close(); return; }
    help ??= buildHelp();
    const list = help.querySelector("dl");
    list.replaceChildren();
    for (const b of bindings) {
      if (!b.when()) continue;
      const dt = document.createElement("dt");
      const kbd = document.createElement("kbd");
      kbd.textContent = label(b.keys);
      dt.append(kbd);
      const dd = document.createElement("dd");
      dd.textContent = typeof b.desc === "function" ? b.desc() : b.desc;
      list.append(dt, dd);
    }
    help.showModal();
  }

  function buildHelp() {
    const d = document.createElement("dialog");
    d.className = "model-picker keys-help";
    d.setAttribute("aria-label", "Keyboard shortcuts");
    const title = document.createElement("div");
    title.className = "dtitle";
    title.textContent = "Keyboard shortcuts";
    const note = document.createElement("p");
    note.className = "action-note";
    note.textContent = "Press ? (outside a text field) or " + label("Mod+/") + " to open this list.";
    const close = document.createElement("button");
    close.type = "button";
    close.textContent = "Close";
    close.addEventListener("click", () => d.close());
    d.addEventListener("click", e => { if (e.target === d) d.close(); });
    d.append(title, document.createElement("dl"), note, close);
    document.body.append(d);
    return d;
  }

  const typing = el => el instanceof HTMLElement &&
    (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName));

  document.addEventListener("keydown", e => {
    if (e.defaultPrevented || e.isComposing || e.repeat) return;
    // Leave other modals (session actions, model picker) alone; the help overlay
    // can still be toggled closed.
    const modal = document.querySelector("dialog:modal");
    if (modal && modal !== help) return;
    const plainHelp = e.key === "?" && !e.ctrlKey && !e.metaKey && !e.altKey && !typing(e.target);
    const binding = plainHelp ? bindings.at(-1) : bindings.find(b => matches(b.keys, e));
    if (!binding || (modal && binding.run !== toggleHelp) || !binding.when()) return;
    e.preventDefault();
    binding.run();
  });
})();
