(() => {
  // Token-wise subsequence matching: short contiguous runs and word starts
  // outrank scattered matches. Name matches outrank path-only matches.
  function fuzzyScore(query, name, path) {
    const score = (token, text) => {
      let best = -Infinity;
      for (let start = 0; start < text.length; start++) {
        if (text[start] !== token[0]) continue;
        let at = start, previous = start - 1, total = 0, matched = true;
        for (const char of token) {
          at = text.indexOf(char, at);
          if (at < 0) { matched = false; break; }
          total += 10 + (at === previous + 1 ? 24 : 0) + (at === 0 || /[\s/_.-]/.test(text[at - 1]) ? 16 : 0) - (at - previous - 1);
          previous = at++;
        }
        if (matched) best = Math.max(best, total - start * 0.1 - text.length * 0.01);
      }
      return best;
    };
    return query.toLowerCase().trim().split(/\s+/).reduce((total, token) =>
      total + Math.max(score(token, name.toLowerCase()) + 30, score(token, path.toLowerCase())), 0);
  }
  if (typeof module !== "undefined") module.exports = {fuzzyScore};
  if (typeof document === "undefined") return;
  const filter = document.getElementById("proj-filter");
  if (!filter) return;
  const form = document.getElementById("project-picker");
  const list = document.getElementById("project-list");
  const pathList = document.getElementById("path-list");
  const rows = Array.from(list.children);
  const empty = document.getElementById("proj-empty");
  const title = document.getElementById("results-title");
  const note = document.getElementById("results-note");
  const ghost = document.getElementById("path-ghost");
  const pinnedSection = document.getElementById("pinned-section");
  const pinnedList = document.getElementById("pinned-projects");
  const storageKey = "perch.pinned-projects";
  let pins = [];
  try {
    const stored = JSON.parse(localStorage.getItem(storageKey) || "[]");
    if (Array.isArray(stored)) pins = stored.filter(p => p && typeof p.path === "string" && typeof p.name === "string");
  } catch (_) { /* Storage can be unavailable in private browsing. */ }
  let visible = [], selected = -1, completion = "", generation = 0, timer;
  const pathMode = () => /^[~/]/.test(filter.value);
  const button = (label, className, action) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = className;
    b.textContent = label;
    b.addEventListener("click", action);
    return b;
  };
  function start(path) {
    const params = new URLSearchParams({dir: path});
    const name = form.elements.name.value.trim();
    if (name) params.set("name", name);
    window.location.assign("/draft?" + params);
  }
  function renderPins() {
    pinnedList.replaceChildren();
    pinnedSection.hidden = !pins.length || !!filter.value;
    for (const pin of pins) {
      const card = document.createElement("div");
      card.className = "pinned-card";
      const open = button(pin.name, "ghost", () => start(pin.path));
      open.title = pin.path;
      const remove = button("×", "ghost icon-btn", () => togglePin(pin));
      remove.setAttribute("aria-label", "Unpin " + pin.name);
      card.append(open, remove);
      pinnedList.append(card);
    }
    for (const row of rows) {
      const pinned = pins.some(p => p.path === row.dataset.path);
      const b = row.querySelector(".pin-project");
      b.textContent = pinned ? "★" : "☆";
      b.setAttribute("aria-pressed", String(pinned));
      b.setAttribute("aria-label", (pinned ? "Unpin " : "Pin ") + row.dataset.name);
      b.title = pinned ? "Unpin project" : "Pin project";
    }
  }
  function togglePin(pin) {
    pins = pins.some(p => p.path === pin.path) ? pins.filter(p => p.path !== pin.path) : [...pins, pin];
    try { localStorage.setItem(storageKey, JSON.stringify(pins)); } catch (_) {}
    renderPins();
  }
  for (const row of rows) row.querySelector(".pin-project").addEventListener("click", () => togglePin({name: row.dataset.name, path: row.dataset.path}));
  function select(index) {
    selected = index;
    visible.forEach((row, i) => row.classList.toggle("selected", i === index));
  }
  function showGhost() {
    ghost.replaceChildren();
    if (!completion.startsWith(filter.value) || completion === filter.value) return;
    const prefix = document.createElement("span");
    prefix.textContent = filter.value;
    prefix.style.visibility = "hidden";
    ghost.append(prefix, document.createTextNode(completion.slice(filter.value.length)));
    ghost.scrollLeft = filter.scrollLeft;
  }
  function acceptCompletion(value = completion) {
    if (!value) return;
    filter.value = value;
    filter.focus();
    update();
  }
  async function fetchPaths(q, version) {
    try {
      const response = await fetch("/new/paths?" + new URLSearchParams({q}));
      if (!response.ok) throw new Error("Cannot complete this path");
      const data = await response.json();
      if (version !== generation) return;
      completion = data.completion;
      showGhost();
      pathList.replaceChildren();
      visible = data.entries.map(entry => {
        const row = document.createElement("div");
        row.className = "project-row";
        row.dataset.path = entry.Path;
        const open = button("", "project-start", () => start(entry.Path));
        const name = document.createElement("span");
        name.className = "pname";
        name.textContent = entry.Name + "/";
        const path = document.createElement("span");
        path.className = "ppath";
        path.textContent = entry.Path;
        open.append(name, path);
        const complete = button("›", "ghost icon-btn", () => acceptCompletion(entry.Path + "/"));
        complete.setAttribute("aria-label", "Complete " + entry.Name);
        complete.title = "Browse " + entry.Name;
        const pin = button("☆", "ghost icon-btn", () => togglePin({name: entry.Name, path: entry.Path}));
        pin.setAttribute("aria-label", "Pin " + entry.Name);
        row.append(open, pin, complete);
        pathList.append(row);
        return row;
      });
      empty.textContent = data.error || "No subdirectories. Press Enter to start here.";
      empty.hidden = visible.length > 0;
      note.textContent = "Enter starts here · › browses a folder";
    } catch (_) {
      if (version !== generation) return;
      empty.textContent = "Path completion unavailable. Enter still opens this directory.";
      empty.hidden = false;
      note.textContent = "Enter starts here";
    }
  }
  function update() {
    const version = ++generation;
    clearTimeout(timer);
    completion = "";
    ghost.replaceChildren();
    select(-1);
    const q = filter.value.trim();
    renderPins();
    list.hidden = pathMode();
    pathList.hidden = !pathMode();
    title.textContent = pathMode() ? "Directories" : q ? "Matching projects" : "Recent projects";
    if (pathMode()) {
      visible = [];
      pathList.replaceChildren();
      empty.hidden = true;
      note.textContent = "Completing…";
      timer = setTimeout(() => fetchPaths(filter.value, version), 100);
      return;
    }
    note.textContent = "Choose a project to start a draft";
    const ranked = rows.map((row, index) => ({row, index, score: q ? fuzzyScore(q, row.dataset.name, row.dataset.display + " " + row.dataset.path) : 0}))
      .filter(item => Number.isFinite(item.score))
      .sort((a, b) => b.score - a.score || a.index - b.index).slice(0, 8);
    rows.forEach(row => { row.hidden = true; });
    visible = ranked.map(({row}) => { row.hidden = false; list.append(row); return row; });
    empty.textContent = q ? "No matching projects. Type / or ~/ to use a directory." : "No recent projects. Type / or ~/ to choose a directory.";
    empty.hidden = visible.length > 0;
    select(visible.length ? 0 : -1);
  }
  filter.addEventListener("input", update);
  filter.addEventListener("scroll", showGhost);
  filter.addEventListener("keydown", event => {
    if (event.isComposing) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (!visible.length) return;
      select((selected + (event.key === "ArrowDown" ? 1 : -1) + visible.length) % visible.length);
      visible[selected].scrollIntoView({block: "nearest"});
    } else if ((event.key === "Tab" && !event.shiftKey || event.key === "ArrowRight" && filter.selectionStart === filter.value.length) && completion && completion !== filter.value && completion.startsWith(filter.value)) {
      event.preventDefault();
      acceptCompletion();
    } else if (event.key === "Enter") {
      event.preventDefault();
      if (selected >= 0) start(visible[selected].dataset.path);
      else if (pathMode()) start(filter.value);
    } else if (event.key === "Escape") {
      filter.value = "";
      update();
    }
  });
  form.addEventListener("submit", event => {
    if (event.submitter?.name === "dir") return;
    event.preventDefault();
    if (selected >= 0) start(visible[selected].dataset.path);
    else if (pathMode()) start(filter.value);
  });
  update();
})();
